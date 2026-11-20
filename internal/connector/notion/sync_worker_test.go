package notion

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWalkRootsConcurrentlyUsesBoundedParallelism(t *testing.T) {
	roots := make([]Root, 6)
	for i := range roots {
		roots[i] = Root{ObjectType: RootTypePage, ObjectID: string(rune('A' + i))}
	}
	started := make(chan struct{}, len(roots))
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	var seenMu sync.Mutex
	seen := make(map[string]int)
	walk := func(ctx context.Context, ref RootRef) (WalkReport, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		seenMu.Lock()
		seen[ref.ObjectID]++
		seenMu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
			return WalkReport{Processed: 1}, nil
		case <-ctx.Done():
			return WalkReport{}, context.Cause(ctx)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	results := walkRootsConcurrently(context.Background(), roots, 3, logger, walk)

	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("three roots did not start concurrently")
		}
	}
	select {
	case <-started:
		t.Fatal("a fourth root started before a worker slot was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	resultCount := 0
	for result := range results {
		if result.err != nil || result.report.Processed != 1 {
			t.Fatalf("result=%+v", result)
		}
		resultCount++
	}
	if resultCount != len(roots) {
		t.Fatalf("results=%d, want %d", resultCount, len(roots))
	}
	if got := peak.Load(); got != 3 {
		t.Fatalf("peak concurrency=%d, want 3", got)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for _, root := range roots {
		if seen[root.ObjectID] != 1 {
			t.Fatalf("root %s walked %d times, want once", root.ObjectID, seen[root.ObjectID])
		}
	}
}

func TestPartialReconcileErrorClassification(t *testing.T) {
	partial := &partialReconcileError{failed: 1, total: 3}
	if !isPartialReconcileError(partial) {
		t.Fatal("partial reconcile error was not classified as partial")
	}
	if isPartialReconcileError(errors.New("database unavailable")) {
		t.Fatal("fatal error was classified as partial")
	}
	if isPartialReconcileError(context.Canceled) {
		t.Fatal("context cancellation was classified as partial")
	}
	if partial.Error() != "1 of 3 notion roots failed" {
		t.Fatalf("error=%q", partial.Error())
	}
	if !reconcileAttemptCompletesBackfill(nil) {
		t.Fatal("successful reconcile did not complete backfill")
	}
	if !reconcileAttemptCompletesBackfill(partial) {
		t.Fatal("partial reconcile did not complete backfill")
	}
	if reconcileAttemptCompletesBackfill(errors.New("database unavailable")) {
		t.Fatal("fatal reconcile completed backfill")
	}
	if reconcileAttemptCompletesBackfill(context.Canceled) {
		t.Fatal("canceled reconcile completed backfill")
	}
	status, err := reconcileRootFailureResult(1, 3)
	if status != "partial" || !isPartialReconcileError(err) || !reconcileAttemptCompletesBackfill(err) {
		t.Fatalf("partial result: status=%q err=%v", status, err)
	}
	status, err = reconcileRootFailureResult(3, 3)
	if status != "failed" || isPartialReconcileError(err) || reconcileAttemptCompletesBackfill(err) {
		t.Fatalf("total-failure result: status=%q err=%v", status, err)
	}
}

func TestRefreshCredentialIdentityResetsOnlyOnChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	srv := newNotionTestServer(t)
	srv.set("GET /users/me", `{"object":"user","id":"new-integration","type":"bot","name":"Diderot"}`)
	w := NewSyncWorker(pool, newWalkerForTest(srv, &fakeIngester{}), nil, nil)
	watermark := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE notion_sync SET integration_id = 'old-integration', last_discovery_at = $1 WHERE id = 1`, watermark); err != nil {
		t.Fatalf("seed changed credential: %v", err)
	}
	if err := w.refreshCredentialIdentity(ctx); err != nil {
		t.Fatalf("refresh changed credential: %v", err)
	}
	var integrationID string
	var gotWatermark *time.Time
	if err := pool.QueryRow(ctx, `SELECT integration_id, last_discovery_at FROM notion_sync WHERE id = 1`).Scan(&integrationID, &gotWatermark); err != nil {
		t.Fatalf("read changed credential state: %v", err)
	}
	if integrationID != "new-integration" || gotWatermark != nil {
		t.Fatalf("state after change=(%q, %v), want new-integration and nil watermark", integrationID, gotWatermark)
	}

	if _, err := pool.Exec(ctx, `UPDATE notion_sync SET last_discovery_at = $1 WHERE id = 1`, watermark); err != nil {
		t.Fatalf("seed same credential: %v", err)
	}
	if err := w.refreshCredentialIdentity(ctx); err != nil {
		t.Fatalf("refresh same credential: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_discovery_at FROM notion_sync WHERE id = 1`).Scan(&gotWatermark); err != nil {
		t.Fatalf("read same credential state: %v", err)
	}
	if gotWatermark == nil || !gotWatermark.Equal(watermark) {
		t.Fatalf("same credential watermark=%v, want %v", gotWatermark, watermark)
	}
}
