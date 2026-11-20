package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/ratelimit"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// linearTestDBURL skips the test if no DB is available.
func linearTestDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "linear")
}

// buildTestSyncWorker builds a SyncWorker pointed at a test httptest.Server.
func buildTestSyncWorker(t *testing.T, srv *httptest.Server, pool *pgxpool.Pool) *SyncWorker {
	t.Helper()
	provider := NewStaticTokenProvider(syntheticToken)
	client := NewClient(provider)
	client.httpClient = srv.Client()
	client.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	reg := prometheus.NewRegistry()
	sink := events.NewDBSink(pool, reg)
	return NewSyncWorker(pool, client, sink, nil, reg)
}

// emptyIssuesResponse is a GraphQL response returning no issues.
func emptyIssuesResponse() []byte {
	return mustMarshal(map[string]any{
		"data": map[string]any{
			"issues": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			},
		},
	})
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestWatermarkAdvancedOnSuccess verifies that last_sync_at advances after
// a successful tick and does NOT advance if the tick fails.
func TestWatermarkAdvancedOnSuccess(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset watermark.
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	// Serve empty issues — successful tick with nothing to ingest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(emptyIssuesResponse())
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	w.WatermarkOverlap = time.Minute

	tickStart := time.Now()
	if err := w.runSync(ctx); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	// Watermark should be set to approximately tickStart - 1m overlap.
	wm, err := w.readSyncWatermark(ctx)
	if err != nil {
		t.Fatalf("readSyncWatermark: %v", err)
	}
	if wm.IsZero() {
		t.Fatal("watermark not set after successful tick")
	}
	// Watermark should be before tickStart (due to overlap).
	if !wm.Before(tickStart) {
		t.Errorf("watermark=%v should be before tick_start=%v", wm, tickStart)
	}
}

// TestWatermarkNotAdvancedOnAPIError verifies the watermark is not advanced
// when the API call fails.
func TestWatermarkNotAdvancedOnAPIError(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Stamp a known watermark.
	knownTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = $1 WHERE id = 1`, knownTime); err != nil {
		t.Fatalf("set watermark: %v", err)
	}

	// Serve 500 — API error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	if err := w.runSync(ctx); err == nil {
		t.Fatal("expected error from runSync with 500 API, got nil")
	}

	// Watermark should still be the known value.
	wm, err := w.readSyncWatermark(ctx)
	if err != nil {
		t.Fatalf("readSyncWatermark: %v", err)
	}
	if !wm.Equal(knownTime) {
		t.Errorf("watermark=%v, want %v (unchanged after failure)", wm, knownTime)
	}
}

// TestSyncWatermarkNotAdvancedPastFailedWrite is the deterministic
// regression test for: a per-issue write failure mid-tick must not let the
// incremental sync watermark advance past that issue.
//
// Trigger: one incremental sync page returns two issues. The first
// ("wm-a") is ordinary and writes successfully. The second ("wm-b")'s
// write fails: its updatedAt is reported as the zero time, which
// events.Event.validate() rejects ("events: OccurredAt is required") — a
// deterministic stand-in for any write failure unrelated to the fetch
// itself, such as a transient Postgres error.
//
// Contract (sync.go's SyncWorker doc comment: "Watermark advances ... on
// full success"; the sync cursor never advances past data that failed
// to write): when a write fails, the watermark
// must stay where it was so the next tick's `updatedAt gte since` filter
// still covers the failed issue.
//
// runIssuePage (backfill.go) counts every upsertIssue error but
// context.Canceled into batch.TotalErrors and returns nil for the page,
// so runSync must consult that count: advancing the watermark to
// wall-clock tick_start - WatermarkOverlap regardless would move it
// past wm-b.
func TestSyncWatermarkNotAdvancedPastFailedWrite(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	issueA := buildTestIssue("wm-a", "ENG-1", "Fine issue", false, "ENG")
	issueA["updatedAt"] = "2026-01-01T00:00:00Z"
	issueB := buildTestIssue("wm-b", "ENG-2", "Issue whose write fails", false, "ENG")
	issueB["updatedAt"] = "0001-01-01T00:00:00Z" // corrupted: fails events.Event.validate()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{issueA, issueB},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	w.WatermarkOverlap = time.Minute

	// Run the tick. Its error is not asserted here: the bug under test is
	// about the watermark, not about whether the tick reports itself as
	// failed.
	_ = w.runSync(ctx)

	var storedA int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'wm-a' AND NOT deleted`).Scan(&storedA); err != nil {
		t.Fatalf("count wm-a: %v", err)
	}
	if storedA != 1 {
		t.Fatalf("wm-a (not poisoned) not stored: count=%d, want 1", storedA)
	}
	var storedB int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'wm-b' AND NOT deleted`).Scan(&storedB); err != nil {
		t.Fatalf("count wm-b: %v", err)
	}
	if storedB != 0 {
		t.Fatalf("wm-b (failed write) unexpectedly stored: count=%d, want 0", storedB)
	}

	// Expected: the watermark is left exactly as it was before the tick
	// (unset / zero), not advanced to ~tick_start-1m, so the very next
	// tick's filter still includes wm-b.
	wm, err := w.readSyncWatermark(ctx)
	if err != nil {
		t.Fatalf("readSyncWatermark: %v", err)
	}
	if !wm.IsZero() {
		t.Fatalf("watermark=%v, want zero (unset): it must not advance past wm-b's failed write, or the next tick's `updatedAt gte since` filter will never see wm-b again", wm)
	}
}

// TestBackfillCursorNotAdvancedPastFailedWrite is the regression test for
// runBackfillBatch's own hold-back: an operator backfill page containing an
// issue whose write fails must not advance backfill_cursor, or clear
// backfill_requested_at, past that issue -- the same contract runSync
// enforces for the sync watermark, but exercised through the durable
// backfill-request path instead of walkUpdatedSince directly.
func TestBackfillCursorNotAdvancedPastFailedWrite(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}
	requestedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		UPDATE linear_sync
		   SET backfill_requested_at = $1, backfill_cursor = NULL,
		       backfill_project = NULL, backfill_project_id = NULL
		 WHERE id = 1
	`, requestedAt); err != nil {
		t.Fatalf("seed backfill request: %v", err)
	}

	issueA := buildTestIssue("bf-wm-a", "ENG-1", "Fine issue", false, "ENG")
	issueB := buildTestIssue("bf-wm-b", "ENG-2", "Issue whose write fails", false, "ENG")
	issueB["updatedAt"] = "0001-01-01T00:00:00Z" // corrupted: fails events.Event.validate()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{issueA, issueB},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "page-1-end"},
				},
			},
		}))
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)

	// The batch's write failure is expected to surface as an error here;
	// that's exactly how runBackfillSafe learns to log and report it.
	if err := w.runBackfillBatch(ctx); err == nil {
		t.Fatal("runBackfillBatch: want error for a page with a failed write, got nil")
	}

	var storedA, storedB int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'bf-wm-a' AND NOT deleted`).Scan(&storedA); err != nil {
		t.Fatalf("count bf-wm-a: %v", err)
	}
	if storedA != 1 {
		t.Fatalf("bf-wm-a (not poisoned) not stored: count=%d, want 1", storedA)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'bf-wm-b' AND NOT deleted`).Scan(&storedB); err != nil {
		t.Fatalf("count bf-wm-b: %v", err)
	}
	if storedB != 0 {
		t.Fatalf("bf-wm-b (failed write) unexpectedly stored: count=%d, want 0", storedB)
	}

	var gotRequestedAt *time.Time
	var gotCursor *string
	if err := pool.QueryRow(ctx, `SELECT backfill_requested_at, backfill_cursor FROM linear_sync WHERE id = 1`).Scan(&gotRequestedAt, &gotCursor); err != nil {
		t.Fatalf("read linear_sync: %v", err)
	}
	// Expected: the request is left exactly as it was before this batch
	// (not cleared, cursor not advanced to the failed page's endCursor),
	// so the next backfill tick refetches and retries this same page
	// instead of silently skipping bf-wm-b and moving on.
	if gotRequestedAt == nil || !gotRequestedAt.Equal(requestedAt) {
		t.Errorf("backfill_requested_at=%v, want unchanged %v: it must not be cleared past a page with a failed write", gotRequestedAt, requestedAt)
	}
	if gotCursor != nil {
		t.Errorf("backfill_cursor=%v, want nil (unchanged): it must not advance past bf-wm-b's failed write, or the next batch will start from the wrong page", *gotCursor)
	}
}

// TestGuardRefusePrivateTeam verifies private-team issues are never upserted.
func TestGuardRefusePrivateTeam(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset events.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	// Issue in a private team.
	privateIssue := buildTestIssue("priv-001", "PRIV-1", "Secret task", true, "PRIV")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{privateIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	_, _, err = w.walkUpdatedSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("walkUpdatedSince: %v", err)
	}

	// No event should be stored.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'priv-001'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("private team issue was stored (count=%d), want 0", count)
	}
}

// TestGuardRefuseExcludedTeam verifies excluded-team issues are never upserted.
func TestGuardRefuseExcludedTeam(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset events.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	excludedIssue := buildTestIssue("excl-001", "SUP-1", "Support ticket", false, "SUP")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{excludedIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	w.ExcludeTeams = []string{"SUP"}
	_, _, err = w.walkUpdatedSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("walkUpdatedSince: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'excl-001'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("excluded team issue was stored (count=%d), want 0", count)
	}
}

// TestGuardAllowsPublicNonExcluded verifies public, non-excluded issues are stored.
func TestGuardAllowsPublicNonExcluded(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset events.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	publicIssue := buildTestIssue("pub-001", "ENG-1", "Public task", false, "ENG")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{publicIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	count, _, err := w.walkUpdatedSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("walkUpdatedSince: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 upserted, got %d", count)
	}

	var stored int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'pub-001'`).Scan(&stored); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != 1 {
		t.Errorf("public issue not stored (count=%d), want 1", stored)
	}
}

// TestIsExcluded_CaseInsensitive verifies exclusion matching is case-insensitive.
func TestIsExcluded_CaseInsensitive(t *testing.T) {
	w := &SyncWorker{ExcludeTeams: []string{"SUP", "INTAKE"}}

	cases := []struct {
		key      string
		excluded bool
	}{
		{"SUP", true},
		{"sup", true},
		{"Sup", true},
		{"INTAKE", true},
		{"intake", true},
		{"ENG", false},
		{"", false},
	}
	for _, c := range cases {
		issue := &Issue{Team: &Team{Key: c.key}}
		got := w.isExcluded(issue)
		if got != c.excluded {
			t.Errorf("isExcluded(%q)=%v, want %v", c.key, got, c.excluded)
		}
	}
}

// TestGuardSelfClean verifies that when a guard refusal fires, any previously-
// ingested event for that issue is deleted by the incremental loop rather than
// waiting for the hourly visibility sweep.
// TestGuardRunsDespiteCommentOverflowFailure is the regression test for:
// when a previously-ingested issue with more than 50 comments moves to a
// private team (or an excluded team) in the same tick its comment-overflow
// follow-up request fails, the privacy guard must still run and delete the
// stored row within that tick.
//
// Before the fix, runIssuePage fetched overflow comments for every issue
// (refused or not) before ever calling upsertIssue, and a failure there
// made it `continue` before upsertIssue -- and the guard check and its
// delete live inside upsertIssue. So a now-private issue whose overflow
// fetch failed stayed stored and searchable until a later tick's overflow
// fetch happened to succeed, or the hourly visibility sweep ran.
func TestGuardRunsDespiteCommentOverflowFailure(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	const issueID = "flip-overflow-1"

	// The issue now appears on a private team, still reporting more than
	// 50 inline comments with more available (hasNextPage=true). If the
	// comment-overflow follow-up is attempted at all, it fails with a
	// non-retried error -- the same class of failure as the reported
	// trigger, and proof that the guard doesn't depend on that call
	// succeeding (or even being attempted).
	privateIssue := buildTestIssue(issueID, "ENG-99", "Will go private", true, "ENG")
	privateIssue["comments"] = map[string]any{
		"nodes":    makeTestComments(1, 50),
		"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c50"},
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// The incremental sync page: the issue, now on a private team.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustMarshal(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes":    []any{privateIssue},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}))
			return
		}
		// The overflow comments fetch, if attempted: a non-retried failure.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)

	// Seed a previously-ingested, public row directly: this test is about
	// the guard's behavior on the tick that sees the team flip to private,
	// not about overflow fetch mechanics for the original store.
	insertLinearEvent(ctx, t, w, issueID)

	if _, _, err := w.walkUpdatedSince(ctx, time.Time{}); err != nil {
		t.Fatalf("walkUpdatedSince: %v", err)
	}

	var deletedFlag bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted FROM events WHERE source = 'linear' AND external_id = $1`, issueID,
	).Scan(&deletedFlag); err != nil {
		t.Fatalf("query after tick: %v", err)
	}
	if !deletedFlag {
		t.Errorf("event.deleted=false after team moved private with a failing comment-overflow fetch; want tombstoned (the privacy guard must run regardless of the overflow result); overflow requests seen=%d", calls.Load()-1)
	}
}

func TestGuardSelfClean(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Clean events.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	const issueID = "flip-001"
	publicIssue := buildTestIssue(issueID, "ENG-99", "Will go private", false, "ENG")

	// Step 1: ingest the issue as public.
	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{publicIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer publicSrv.Close()

	w := buildTestSyncWorker(t, publicSrv, pool)
	if _, _, err := w.walkUpdatedSince(ctx, time.Time{}); err != nil {
		t.Fatalf("first walk (public): %v", err)
	}

	// Verify the event is stored.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = $1 AND NOT deleted`, issueID).Scan(&count); err != nil {
		t.Fatalf("count after ingest: %v", err)
	}
	if count != 1 {
		t.Fatalf("event not stored after public ingest (count=%d)", count)
	}

	// Step 2: same issue now appears as private (team flip).
	privateIssue := buildTestIssue(issueID, "ENG-99", "Will go private", true, "ENG")
	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{privateIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer privateSrv.Close()

	// Rebuild worker pointed at the "now private" server.
	w2 := buildTestSyncWorker(t, privateSrv, pool)
	if _, _, err := w2.walkUpdatedSince(ctx, time.Time{}); err != nil {
		t.Fatalf("second walk (private): %v", err)
	}

	// The event should have been tombstoned (deleted=true) by the guard.
	var deletedFlag bool
	err = pool.QueryRow(ctx,
		`SELECT deleted FROM events WHERE source = 'linear' AND external_id = $1`, issueID,
	).Scan(&deletedFlag)
	if err != nil {
		t.Fatalf("query after guard self-clean: %v", err)
	}
	if !deletedFlag {
		t.Errorf("event.deleted=false after guard refusal; want tombstoned")
	}
}

// buildTestIssue builds a minimal issue map for JSON responses.
func buildTestIssue(id, identifier, title string, private bool, teamKey string) map[string]any {
	return map[string]any{
		"id":          id,
		"identifier":  identifier,
		"title":       title,
		"description": "",
		"url":         "https://linear.app/test/issue/" + identifier,
		"createdAt":   "2026-07-11T00:00:00Z",
		"updatedAt":   "2026-07-11T00:00:00Z",
		"priority":    0,
		"creator": map[string]any{
			"id":    "user-1",
			"name":  "Test User",
			"email": "test@example.com",
		},
		"team": map[string]any{
			"id":      "team-" + teamKey,
			"name":    teamKey + " Team",
			"key":     teamKey,
			"private": private,
		},
		"state": map[string]any{
			"name": "Backlog",
			"type": "backlog",
		},
		"labels":   map[string]any{"nodes": []any{}},
		"comments": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
	}
}

// insertLinearEvent inserts a minimal stored linear event directly via
// the sink, for tests that seed a "previously ingested" row without
// walking the full incremental sync path.
func insertLinearEvent(ctx context.Context, t *testing.T, w *SyncWorker, externalID string) {
	t.Helper()
	_, err := w.sink.Upsert(ctx, events.Event{
		Source:     "linear",
		ExternalID: externalID,
		OccurredAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Title:      "seed: " + externalID,
		Body:       "seed body for " + externalID,
		URL:        "https://linear.app/test/issue/" + externalID,
		Scope:      events.ScopeInternal,
	})
	if err != nil {
		t.Fatalf("seed insertLinearEvent(%s): %v", externalID, err)
	}
}

// visibilityRecheckHandler builds an httptest handler that serves an
// empty visibility-walk response for VisibilitySweep queries (so nothing
// is ingestible from the walk's perspective) and a single-issue or error
// response for VisibilityRecheck queries, keyed by fn. fn receives the
// request body's raw query variables and returns the JSON body to write
// plus the HTTP status.
func visibilityRecheckHandler(t *testing.T, recheckResp func(vars map[string]any) ([]byte, int)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "VisibilitySweep"):
			_, _ = w.Write(emptyIssuesResponse())
		case strings.Contains(req.Query, "VisibilityRecheck"):
			respBody, status := recheckResp(req.Variables)
			w.WriteHeader(status)
			_, _ = w.Write(respBody)
		default:
			t.Fatalf("unexpected query in visibility recheck test: %s", req.Query)
		}
	})
}

// TestVisibilitySweep_SparesFalsePositive verifies that a delete candidate
// whose point-query recheck says "still public, non-excluded, not
// archived" is NOT deleted, and the false-positive counter increments.
func TestVisibilitySweep_SparesFalsePositive(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	const candidateID = "fp-001"

	srv := httptest.NewServer(visibilityRecheckHandler(t, func(vars map[string]any) ([]byte, int) {
		if vars["id"] != candidateID {
			t.Fatalf("recheck queried unexpected id %v", vars["id"])
		}
		return mustMarshal(map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":         candidateID,
					"archivedAt": nil,
					"trashed":    nil,
					"team": map[string]any{
						"id":      "team-ENG",
						"key":     "ENG",
						"private": false,
					},
				},
			},
		}), http.StatusOK
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	insertLinearEvent(ctx, t, w, candidateID)

	if err := w.runVisibility(ctx); err != nil {
		t.Fatalf("runVisibility: %v", err)
	}

	var deleted bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted FROM events WHERE source = 'linear' AND external_id = $1`, candidateID,
	).Scan(&deleted); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if deleted {
		t.Errorf("candidate %s was deleted; want spared (false positive)", candidateID)
	}

	if got := testutil.ToFloat64(w.metrics.visibilityFalsePositives); got != 1 {
		t.Errorf("visibilityFalsePositives=%v, want 1", got)
	}
}

// TestVisibilitySweep_DeletesOnEntityNotFound verifies that a delete
// candidate whose point-query recheck 404s with an entity-not-found-shaped
// GraphQL error IS deleted.
func TestVisibilitySweep_DeletesOnEntityNotFound(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	const candidateID = "gone-001"

	srv := httptest.NewServer(visibilityRecheckHandler(t, func(vars map[string]any) ([]byte, int) {
		if vars["id"] != candidateID {
			t.Fatalf("recheck queried unexpected id %v", vars["id"])
		}
		return mustMarshal(map[string]any{
			"errors": []map[string]any{
				{"message": "Entity not found: Issue"},
			},
		}), http.StatusOK
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	insertLinearEvent(ctx, t, w, candidateID)

	if err := w.runVisibility(ctx); err != nil {
		t.Fatalf("runVisibility: %v", err)
	}

	var deleted bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted FROM events WHERE source = 'linear' AND external_id = $1`, candidateID,
	).Scan(&deleted); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if !deleted {
		t.Errorf("candidate %s not deleted after entity-not-found recheck; want deleted", candidateID)
	}
	if got := testutil.ToFloat64(w.metrics.visibilityFalsePositives); got != 0 {
		t.Errorf("visibilityFalsePositives=%v, want 0 (this is a confirmed delete, not a false positive)", got)
	}
}

// TestVisibilitySweep_DeletesOnPrivateTeam verifies that a delete
// candidate whose point-query recheck reports a private team IS deleted.
func TestVisibilitySweep_DeletesOnPrivateTeam(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}

	const candidateID = "priv-flip-001"

	srv := httptest.NewServer(visibilityRecheckHandler(t, func(vars map[string]any) ([]byte, int) {
		if vars["id"] != candidateID {
			t.Fatalf("recheck queried unexpected id %v", vars["id"])
		}
		return mustMarshal(map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":         candidateID,
					"archivedAt": nil,
					"trashed":    nil,
					"team": map[string]any{
						"id":      "team-PRIV",
						"key":     "PRIV",
						"private": true,
					},
				},
			},
		}), http.StatusOK
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	insertLinearEvent(ctx, t, w, candidateID)

	if err := w.runVisibility(ctx); err != nil {
		t.Fatalf("runVisibility: %v", err)
	}

	var deleted bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted FROM events WHERE source = 'linear' AND external_id = $1`, candidateID,
	).Scan(&deleted); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if !deleted {
		t.Errorf("candidate %s not deleted after private-team recheck; want deleted", candidateID)
	}
	if got := testutil.ToFloat64(w.metrics.visibilityFalsePositives); got != 0 {
		t.Errorf("visibilityFalsePositives=%v, want 0 (this is a confirmed delete, not a false positive)", got)
	}
}

// handlerBusyTracker instruments the three runXSafe handlers so a test can
// assert Run's select loop never lets two of them execute concurrently.
// Each Enter call atomically flips a "busy" flag; if it was already set,
// the overlap is recorded (via overlapDetected) rather than failing
// immediately from a non-test goroutine — Run drives handlers from within
// its own call stack, so t.Fatal there would not fail the test cleanly.
type handlerBusyTracker struct {
	busy    atomic.Bool
	overlap atomic.Bool
	calls   atomic.Int64
}

func (h *handlerBusyTracker) enter() func() {
	h.calls.Add(1)
	if !h.busy.CompareAndSwap(false, true) {
		h.overlap.Store(true)
		// Still "enter" for the duration even though we detected overlap,
		// so exit's CompareAndSwap below has a consistent flag to clear.
	}
	return func() {
		h.busy.Store(false)
	}
}

// TestRun_NeverOverlapsHandlers is a negative-control concurrency test: it
// drives the real Run select loop with very short ticker intervals against
// a slow fake server, and asserts that sync, visibility, and reconcile never
// execute concurrently with each other. Backfill uses the same serialized
// loop but has no API call unless durable work is pending.
//
// Mechanism: each handler's underlying API call goes through a shared
// httptest server that increments a busy tracker on entry and decrements
// on exit, sleeping briefly in between to widen the window in which an
// overlap (if Run launched its handlers on goroutines) would be
// observed. It is a genuine negative control: adding `go` keywords to
// Run's ticker cases reliably flips tracker.overlap.Load() to true and
// fails this test.
func TestRun_NeverOverlapsHandlers(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}
	// Clean backfill + watermark state so all handlers actually run.
	if _, err := pool.Exec(ctx, `
		UPDATE linear_sync
		   SET backfill_requested_at = NULL,
		       backfill_cursor = NULL,
		       last_sync_at = NULL
		 WHERE id = 1`); err != nil {
		t.Fatalf("reset linear_sync: %v", err)
	}

	tracker := &handlerBusyTracker{}

	// Every query type gets an empty-issues (or empty-issue) response, but
	// only after sleeping — long enough that, under the old goroutine-based
	// Run, a fast ticker firing during the sleep would start a second
	// handler concurrently.
	const handlerSleep = 15 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exit := tracker.enter()
		defer exit()
		time.Sleep(handlerSleep)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var req struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "IssueComments") {
			_, _ = w.Write(mustMarshal(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"comments": map[string]any{
							"nodes":    []any{},
							"pageInfo": map[string]any{"hasNextPage": false},
						},
					},
				},
			}))
			return
		}
		_, _ = w.Write(emptyIssuesResponse())
	}))
	defer srv.Close()

	wk := buildTestSyncWorker(t, srv, pool)
	// The client's production rate limiter (0.4 req/s, burst 3) would
	// starve this test after the first few ticks — override it so every
	// tick actually reaches the fake server and exercises the busy
	// tracker, keeping the negative-control window meaningful.
	wk.client.limiter = ratelimit.New(rate.Inf, 0)
	// Tiny intervals so many ticks fire inside the 20s test window, well
	// within the handlerSleep windows of earlier ticks.
	wk.SyncInterval = 10 * time.Millisecond
	wk.VisibilityInterval = 12 * time.Millisecond
	wk.ReconcileInterval = 14 * time.Millisecond

	runCtx, runCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer runCancel()

	err = wk.Run(runCtx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if tracker.calls.Load() < 3 {
		t.Fatalf("test did not exercise enough handler calls to be meaningful (calls=%d)", tracker.calls.Load())
	}
	if tracker.overlap.Load() {
		t.Errorf("Run allowed two handlers to execute concurrently — serialization regressed")
	}
}
