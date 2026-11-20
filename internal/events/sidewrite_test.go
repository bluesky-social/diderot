package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// These tests cover UpsertWithSideWrite — the in-transaction side
// write seam added so slack_message_details (and any future
// source-specific side row) lands atomically with the event rather
// than in a separate post-commit Exec. With a separate Exec, a
// transient DB failure between the event commit and the side-table
// write strands an event with no side row, invisible to
// the channel-history pager. All of these need a real DB and so skip
// without DIDEROT_TEST_DB_URL.
//
// New file (not appended to events_test.go) to avoid colliding with
// the wave-2 test-infrastructure refactor happening in that file.

// TestUpsertWithSideWriteRunsInTx asserts the side write fires on the
// INSERT path and shares the event's transaction (the side row exists
// after commit, keyed on the event id the callback received).
func TestUpsertWithSideWriteRunsInTx(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	var sawEventID int64
	side := func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		sawEventID = eventID
		// Write a probe row into slack_message_details using the same
		// tx — the table exists in the test schema (migrations applied
		// by `just up`).
		_, err := tx.Exec(ctx, `
			INSERT INTO slack_message_details
				(event_id, channel_id, occurred_at)
			VALUES ($1, $2, $3)
		`, eventID, "C1", time.Unix(1700000000, 0))
		return err
	}

	res, err := sink.UpsertWithSideWrite(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "hello",
		Raw:        json.RawMessage(`{"type":"message"}`),
	}, side)
	if err != nil {
		t.Fatalf("UpsertWithSideWrite: %v", err)
	}
	if sawEventID != res.EventID {
		t.Fatalf("side write saw event id %d, upsert returned %d", sawEventID, res.EventID)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_message_details WHERE event_id = $1`,
		res.EventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("side row count = %d, want 1", n)
	}
}

// TestUpsertWithSideWriteRollsBackEvent is the strand-fix proof: when
// the side write fails, the WHOLE transaction rolls back, so the event
// row never lands. Pre-refactor the event committed first and a later
// failed side Exec left it orphaned; now they're atomic.
func TestUpsertWithSideWriteRollsBackEvent(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	boom := errors.New("side write boom")
	side := func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		return boom
	}

	_, err := sink.UpsertWithSideWrite(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "hello",
		Raw:        json.RawMessage(`{"type":"message"}`),
	}, side)
	if err == nil {
		t.Fatal("expected error from failed side write, got nil")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error chain lost the side-write cause: %v", err)
	}

	// The event must NOT exist — the tx rolled back.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source = 'slack' AND external_id = 'C1:1.1'`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("event row count = %d, want 0 (side-write failure should roll back the event)", n)
	}
}

// TestUpsertWithSideWriteRunsOnNoChange asserts the side write fires
// even on the no-change path. This is the repair case: a re-ingest of
// a byte-identical event whose original side row failed to write must
// still get its side row created, even though the event body is
// unchanged. The callback is an upsert, so re-running it on the
// change path then the no-change path is benign.
func TestUpsertWithSideWriteRunsOnNoChange(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	e := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "hello",
		Raw:        json.RawMessage(`{"type":"message"}`),
	}

	// First upsert with a NO-OP side write: event lands, side row is
	// deliberately NOT written (simulates the original side-write
	// failure that the pre-refactor code would have swallowed).
	noop := func(ctx context.Context, tx pgx.Tx, eventID int64) error { return nil }
	first, err := sink.UpsertWithSideWrite(ctx, e, noop)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_message_details WHERE event_id = $1`,
		first.EventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("precondition: side row count = %d, want 0", n)
	}

	// Second upsert with byte-identical content -> NoChange. The side
	// write must STILL run and repair the missing row.
	var ranOnNoChange bool
	repair := func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		ranOnNoChange = true
		_, err := tx.Exec(ctx, `
			INSERT INTO slack_message_details (event_id, channel_id, occurred_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO UPDATE SET channel_id = EXCLUDED.channel_id
		`, eventID, "C1", time.Unix(1700000000, 0))
		return err
	}
	second, err := sink.UpsertWithSideWrite(ctx, e, repair)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !second.NoChange {
		t.Fatal("second upsert of identical content should be NoChange")
	}
	if !ranOnNoChange {
		t.Fatal("side write did not run on the NoChange path (strand repair would be impossible)")
	}

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_message_details WHERE event_id = $1`,
		second.EventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("side row count after no-change repair = %d, want 1", n)
	}
}
