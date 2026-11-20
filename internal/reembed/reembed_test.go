package reembed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// stubRebuilder returns whatever Event the test seeds it with,
// optionally rewriting body/actor/prefix to prove those fields
// flow through the sink's UPDATE path.
type stubRebuilder struct {
	source string
	// transform is applied to each row's Event before return.
	// Default: identity (same body).
	transform func(row StoredRow, ev *events.Event)
}

func (s *stubRebuilder) Source() string { return s.source }

func (s *stubRebuilder) Rebuild(_ context.Context, row StoredRow) (*events.Event, error) {
	ev := &events.Event{
		Source:     s.source,
		Scope:      events.ScopePublic,
		ExternalID: row.ExternalID,
		OccurredAt: row.OccurredAt,
		ActorID:    row.ActorID,
		ThreadID:   row.ThreadID,
		URL:        row.URL,
		Body:       "original body",
		Raw:        row.Raw,
	}
	if s.transform != nil {
		s.transform(row, ev)
	}
	return ev, nil
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "reembed"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedRows(t *testing.T, pool *pgxpool.Pool, source string, n int) {
	t.Helper()
	ctx := context.Background()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	for i := 0; i < n; i++ {
		_, err := sink.Upsert(ctx, events.Event{
			Scope:      events.ScopePublic,
			Source:     source,
			ExternalID: fmt.Sprintf("ext-%d", i),
			OccurredAt: time.Unix(1700000000+int64(i), 0),
			Body:       "original body",
			Raw:        json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func TestReembedRewritesAllRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedRows(t, pool, "slack", 5)

	// Rebuilder transforms body so the sink's UPDATE path triggers
	// naturally on content change (ForceUpdate also bypasses the
	// NoChange comparison, but we want to prove body got rewritten).
	rb := &stubRebuilder{
		source: "slack",
		transform: func(row StoredRow, ev *events.Event) {
			ev.Body = "rewritten body for " + row.ExternalID
		},
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	d := NewDriver(pool, sink, nil, rb)

	prog := &Progress{}
	if err := d.Run(ctx, nil, prog); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !prog.Finished.Load() {
		t.Error("progress.Finished should be true")
	}
	if prog.Total.Load() != 5 {
		t.Errorf("total=%d, want 5", prog.Total.Load())
	}
	if prog.Processed.Load() != 5 {
		t.Errorf("processed=%d, want 5", prog.Processed.Load())
	}
	if prog.Rewritten.Load() != 5 {
		t.Errorf("rewritten=%d, want 5", prog.Rewritten.Load())
	}
	if prog.Errors.Load() != 0 {
		t.Errorf("errors=%d, want 0 (last: %v)", prog.Errors.Load(), prog.LastErr.Load())
	}

	// Verify bodies changed in the DB.
	var body string
	if err := pool.QueryRow(ctx,
		`SELECT body FROM events WHERE external_id = 'ext-2'`,
	).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "rewritten body for ext-2" {
		t.Errorf("body = %q, want rewritten", body)
	}
}

func TestReembedForceUpdateBypassesNoChange(t *testing.T) {
	// With an identity-body rebuilder (no change to events row
	// content) we still want chunks rewritten — that's what the
	// ForceUpdate=true injection is for. Assert chunk ids change.
	pool := testPool(t)
	ctx := context.Background()
	seedRows(t, pool, "slack", 3)

	// Capture original chunk ids.
	origIDs := map[string]int64{}
	rows, _ := pool.Query(ctx, `
		SELECT e.external_id, MIN(c.id)
		  FROM events e JOIN event_chunks c ON c.event_id = e.id
		 GROUP BY e.external_id`)
	for rows.Next() {
		var ext string
		var id int64
		_ = rows.Scan(&ext, &id)
		origIDs[ext] = id
	}
	rows.Close()

	rb := &stubRebuilder{source: "slack"} // identity, no transform
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	d := NewDriver(pool, sink, nil, rb)
	prog := &Progress{}
	if err := d.Run(ctx, nil, prog); err != nil {
		t.Fatal(err)
	}
	if prog.Rewritten.Load() != 3 {
		t.Errorf("rewritten=%d, want 3 (ForceUpdate should bypass NoChange)",
			prog.Rewritten.Load())
	}
	if prog.NoChange.Load() != 0 {
		t.Errorf("no_change=%d, want 0", prog.NoChange.Load())
	}
	// New chunk ids.
	for ext, oldID := range origIDs {
		var newID int64
		_ = pool.QueryRow(ctx, `
			SELECT MIN(c.id) FROM event_chunks c
			  JOIN events e ON e.id = c.event_id
			 WHERE e.external_id = $1`, ext).Scan(&newID)
		if newID == oldID {
			t.Errorf("%s: chunk id unchanged (%d) — ForceUpdate didn't rewrite", ext, oldID)
		}
	}
}

func TestReembedSkipsNilEvent(t *testing.T) {
	// Rebuilder returning (nil, nil) means "skip this row" —
	// e.g. stored raw is a subtype we no longer ingest.
	pool := testPool(t)
	ctx := context.Background()
	seedRows(t, pool, "slack", 2)

	rb := &skipAllRebuilder{}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	d := NewDriver(pool, sink, nil, rb)
	prog := &Progress{}
	_ = d.Run(ctx, nil, prog)
	if prog.Skipped.Load() != 2 {
		t.Errorf("skipped=%d, want 2", prog.Skipped.Load())
	}
	if prog.Rewritten.Load() != 0 {
		t.Error("rewritten should stay 0 when rebuilder skips every row")
	}
}

type skipAllRebuilder struct{}

func (*skipAllRebuilder) Source() string { return "slack" }
func (*skipAllRebuilder) Rebuild(_ context.Context, _ StoredRow) (*events.Event, error) {
	return nil, nil
}

func TestReembedUnknownSourceSkippedNotError(t *testing.T) {
	// Passing a source not registered with the driver is operator
	// error, but we shouldn't fail the entire run — log + move on.
	pool := testPool(t)
	ctx := context.Background()
	seedRows(t, pool, "slack", 1)

	rb := &stubRebuilder{source: "slack"}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	d := NewDriver(pool, sink, nil, rb)
	prog := &Progress{}
	if err := d.Run(ctx, []string{"ssh"}, prog); err != nil {
		t.Fatalf("unknown source should not hard-error: %v", err)
	}
	if prog.Processed.Load() != 0 {
		t.Errorf("processed=%d, want 0 (ssh not registered)", prog.Processed.Load())
	}
}

func TestReembedContextCancelReturnsError(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	seedRows(t, pool, "slack", 10)

	rb := &stubRebuilder{
		source: "slack",
		transform: func(row StoredRow, ev *events.Event) {
			// Sleep a tick so we can cancel mid-run.
			time.Sleep(50 * time.Millisecond)
		},
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	d := NewDriver(pool, sink, nil, rb)
	prog := &Progress{}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, nil, prog) }()

	time.Sleep(120 * time.Millisecond)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// Some rows should have been processed but not all.
	if prog.Processed.Load() == 0 {
		t.Error("expected some rows processed before cancel")
	}
	if prog.Processed.Load() == 10 {
		t.Error("expected cancellation to stop before all rows")
	}
}

// offAfterRebuilder rebuilds its first rows, then reports its source off.
type offAfterRebuilder struct {
	stubRebuilder
	rows, calls int
}

func (r *offAfterRebuilder) Rebuild(ctx context.Context, row StoredRow) (*events.Event, error) {
	r.calls++
	if r.calls > r.rows {
		return nil, ErrSourceOff
	}
	return r.stubRebuilder.Rebuild(ctx, row)
}

// TestReembedStopsASourceThatTurnsOff: a source whose connector stops mid-run
// ends that source's pass with one error, not one per remaining row, and
// the run goes on to the next source.
func TestReembedStopsASourceThatTurnsOff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedRows(t, pool, "slack", 5)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	off := &offAfterRebuilder{stubRebuilder: stubRebuilder{source: "slack"}, rows: 2}
	d := NewDriver(pool, sink, nil, off)
	prog := &Progress{}
	if err := d.Run(ctx, nil, prog); err != nil {
		t.Fatalf("a source turning off fails that source, not the run: %v", err)
	}
	if off.calls != 3 || prog.Processed.Load() != 2 || prog.Errors.Load() != 1 {
		t.Fatalf("calls=%d processed=%d errors=%d, want 3, 2, 1", off.calls, prog.Processed.Load(), prog.Errors.Load())
	}
	if last, _ := prog.LastErr.Load().(string); last != "slack: the source's connector is off" {
		t.Fatalf("last error = %q", last)
	}
}

// raceRebuilder simulates the real window between the driver's batch SELECT
// (which snapshots a StoredRow) and its later call to Rebuild + Upsert for
// that row: onRebuild runs a concurrent write through the real sink right
// before Rebuild returns the Event built from the (by then stale) snapshot
// — exactly as a live connector's Upsert or Delete can land on a row the
// reembed driver already has queued in memory from an earlier SELECT.
type raceRebuilder struct {
	source    string
	onRebuild func()
}

func (r *raceRebuilder) Source() string { return r.source }

func (r *raceRebuilder) Rebuild(_ context.Context, row StoredRow) (*events.Event, error) {
	if r.onRebuild != nil {
		r.onRebuild()
	}
	// Mirrors the real internal/connector/slack.Rebuilder: builds the
	// Event purely from the StoredRow fields captured at SELECT time.
	return &events.Event{
		Source:     r.source,
		Scope:      events.ScopePublic,
		ExternalID: row.ExternalID,
		OccurredAt: row.OccurredAt,
		Body:       "original body",
		Raw:        row.Raw,
	}, nil
}

// TestReembedSkipsRowDeletedDuringBatch:
//
// Trigger: the driver's batch SELECT reads a row (deleted=false at that
// instant); before the driver gets to that row's Rebuild+Upsert call, a
// legitimate connector delete tombstones it.
//
// Contract: a reembed run must never resurrect content a concurrent delete
// removed.
func TestReembedSkipsRowDeletedDuringBatch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if _, err := sink.Upsert(ctx, events.Event{
		Source: "slack", ExternalID: "ext-1", OccurredAt: at,
		Body: "original body", Scope: events.ScopePublic,
		Raw: json.RawMessage(`{"seq":1}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rb := &raceRebuilder{
		source: "slack",
		onRebuild: func() {
			// Lands after the driver's batch SELECT already captured
			// this row, but before processRow's Rebuild+Upsert call
			// for it runs.
			if _, err := sink.Delete(ctx, "slack", "ext-1"); err != nil {
				t.Fatalf("concurrent delete: %v", err)
			}
		},
	}
	d := NewDriver(pool, sink, nil, rb)
	if err := d.Run(ctx, []string{"slack"}, &Progress{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var (
		deleted bool
		body    string
	)
	if err := pool.QueryRow(ctx,
		`SELECT deleted, body FROM events WHERE source = 'slack' AND external_id = 'ext-1'`,
	).Scan(&deleted, &body); err != nil {
		t.Fatalf("read back event: %v", err)
	}
	var chunkCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_chunks c JOIN events e ON e.id = c.event_id WHERE e.external_id = 'ext-1'`,
	).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}

	if !deleted || body != "" || chunkCount != 0 {
		t.Fatalf("reembed resurrected a deleted event: deleted=%v body=%q chunks=%d; want deleted=true body=\"\" chunks=0 "+
			"(a delete that lands between the driver's batch SELECT and its Upsert call for that row must survive "+
			"the reembed job, not be overwritten by the stale pre-delete snapshot)", deleted, body, chunkCount)
	}
}

// TestReembedKeepsEditThatLandsDuringBatch:
//
// Trigger: same batch window as above, but a legitimate content edit
// commits on the row (new body, later occurred_at) instead of a delete,
// after the driver's batch SELECT already read the pre-edit snapshot.
//
// Contract: the reembed must not discard a real, already-committed edit by
// writing the stale pre-edit snapshot back over it.
func TestReembedKeepsEditThatLandsDuringBatch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if _, err := sink.Upsert(ctx, events.Event{
		Source: "slack", ExternalID: "ext-1", OccurredAt: at,
		Body: "original body", Scope: events.ScopePublic,
		Raw: json.RawMessage(`{"seq":1}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const editedBody = "edited body, the user's real last word"
	rb := &raceRebuilder{
		source: "slack",
		onRebuild: func() {
			if _, err := sink.Upsert(ctx, events.Event{
				Source: "slack", ExternalID: "ext-1", OccurredAt: at.Add(time.Minute),
				Body: editedBody, Scope: events.ScopePublic,
				Raw: json.RawMessage(`{"seq":2}`),
			}); err != nil {
				t.Fatalf("concurrent edit: %v", err)
			}
		},
	}
	d := NewDriver(pool, sink, nil, rb)
	if err := d.Run(ctx, []string{"slack"}, &Progress{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var body string
	if err := pool.QueryRow(ctx,
		`SELECT body FROM events WHERE source = 'slack' AND external_id = 'ext-1'`,
	).Scan(&body); err != nil {
		t.Fatalf("read back event: %v", err)
	}
	if body != editedBody {
		t.Fatalf("reembed reverted a concurrent edit: body=%q, want %q (the edit); "+
			"the edit committed after the driver's batch SELECT and must not be overwritten by the "+
			"reembed's stale pre-edit rebuild", body, editedBody)
	}

	var chunkContent string
	if err := pool.QueryRow(ctx, `
		SELECT c.content FROM event_chunks c JOIN events e ON e.id = c.event_id
		 WHERE e.external_id = 'ext-1' AND c.kind = 'text'`,
	).Scan(&chunkContent); err != nil {
		t.Fatalf("read back chunk: %v", err)
	}
	if chunkContent != editedBody {
		t.Fatalf("chunk content = %q, want %q (chunks re-derive from the live body, not the stale snapshot)",
			chunkContent, editedBody)
	}
}
