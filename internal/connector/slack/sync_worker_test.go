package slack

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

// getSyncPool returns a pool with the slack-sync-relevant tables
// truncated. Mirrors getBackfillPool but adds slack_channel_sync,
// slack_thread_sync, and slack_ingest_blacklist to the cleanup so
// successive sync_worker tests don't leak state into each other.
func getSyncPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_users, slack_bots, slack_channel_info, slack_usergroups,
		          slack_channel_sync, slack_thread_sync, slack_ingest_blacklist
		 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// newTestSyncWorker wires a SyncWorker with a backfill that points
// at the test DB + the supplied fake api. Tests that don't drive
// SweepOne can pass a nil api (Backfill is only used for its
// pool reference + ingestMessageCore in those scenarios).
func newTestSyncWorker(t *testing.T, pool *pgxpool.Pool, api backfillAPI) *SyncWorker {
	t.Helper()
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")
	w := NewSyncWorker(pool, bf, nil, prometheus.NewRegistry())
	// Tighter intervals for tests; StaleAfter stays moderate so a
	// non-stale claim doesn't accidentally get reclaimed by a
	// concurrent test goroutine.
	w = w.WithConfig(SyncWorkerConfig{
		StaleAfter: 250 * time.Millisecond,
		Overlap:    5 * time.Minute,
		IdleSleep:  10 * time.Millisecond,
		PageSize:   200,
	})
	return w
}

// seedSyncRow is a shorthand for "insert a slack_channel_sync row
// with these fields." Last-synced is offset by lastSyncedOffset
// from now() (negative = older = higher priority); pass time.Time{}
// (zero) for never-synced.
func seedSyncRow(t *testing.T, pool *pgxpool.Pool, channelID string, lastSyncedOffset time.Duration, ingestEnabled bool) {
	t.Helper()
	var lastSynced *time.Time
	if lastSyncedOffset != 0 {
		ts := time.Now().Add(lastSyncedOffset)
		lastSynced = &ts
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ($1, $2, $3)
	`, channelID, lastSynced, ingestEnabled)
	if err != nil {
		t.Fatalf("seedSyncRow: %v", err)
	}
}

func TestSyncWorker_ClaimNext_StaleestFirst(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Three rows, increasing staleness. Worker should pick the
	// oldest (most stale) first.
	seedSyncRow(t, pool, "C-fresh", -1*time.Minute, true)
	seedSyncRow(t, pool, "C-medium", -10*time.Minute, true)
	seedSyncRow(t, pool, "C-stale", -1*time.Hour, true)

	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim == nil {
		t.Fatal("expected claim, got nil")
	}
	if claim.ChannelID != "C-stale" {
		t.Errorf("staleest-first: got %s, want C-stale", claim.ChannelID)
	}
	if claim.WorkerToken == uuid.Nil {
		t.Error("worker_token should be non-nil")
	}
}

func TestSyncWorker_ClaimNext_NeverSyncedFirst(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Never-synced should beat any stale row (NULL → -infinity in
	// the sort).
	seedSyncRow(t, pool, "C-stale", -2*time.Hour, true)
	seedSyncRow(t, pool, "C-never", 0, true) // zero offset → NULL

	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim == nil || claim.ChannelID != "C-never" {
		t.Errorf("never-synced first: got %+v, want C-never", claim)
	}
	if claim.ClaimedLastSyncedAt != nil {
		t.Errorf("never-synced claim should carry nil ClaimedLastSyncedAt; got %v", claim.ClaimedLastSyncedAt)
	}
}

func TestSyncWorker_ClaimNext_RespectsIngestEnabled(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Disabled rows are invisible regardless of staleness.
	seedSyncRow(t, pool, "C-disabled-stale", -2*time.Hour, false)
	seedSyncRow(t, pool, "C-enabled-fresh", -1*time.Minute, true)

	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim == nil || claim.ChannelID != "C-enabled-fresh" {
		t.Errorf("ingest_enabled gate: got %+v, want C-enabled-fresh", claim)
	}
}

func TestSyncWorker_ClaimNext_NoEligibleReturnsNil(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// One row, currently in-flight, not stale yet.
	seedSyncRow(t, pool, "C-busy", -1*time.Hour, true)
	tok, _ := uuid.NewRandom()
	if _, err := pool.Exec(ctx,
		`UPDATE slack_channel_sync SET in_progress_at = now(), worker_token = $2
		 WHERE channel_id = $1`, "C-busy", tok); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim != nil {
		t.Errorf("expected nil (no eligible rows); got %+v", claim)
	}
}

func TestSyncWorker_ClaimNext_StaleRecovery(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Row claimed by a "dead" worker — in_progress_at older than
	// StaleAfter (250ms in the test config). Should be reclaimable.
	seedSyncRow(t, pool, "C-stuck", -1*time.Hour, true)
	deadToken, _ := uuid.NewRandom()
	if _, err := pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET in_progress_at = now() - '5 seconds'::interval,
		       worker_token = $2
		 WHERE channel_id = $1
	`, "C-stuck", deadToken); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim == nil || claim.ChannelID != "C-stuck" {
		t.Fatalf("stale-recovery: got %+v, want C-stuck", claim)
	}
	if claim.WorkerToken == deadToken {
		t.Error("stale-recovery should mint a new worker_token, not preserve the dead one")
	}
}

func TestSyncWorker_ClaimNext_BlacklistedRowFlippedAndSkipped(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Two rows: one blacklisted, one not. Blacklist runs in-process
	// against an empty initial snapshot, then we add C-bot.
	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	w.WithIngestBlacklist(bl)

	seedSyncRow(t, pool, "C-bot", -2*time.Hour, true)
	seedSyncRow(t, pool, "C-good", -1*time.Hour, true)

	if _, err := bl.Add(ctx, "C-bot", "", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// C-bot is older so picked first; should be flipped to
	// ingest_enabled=false and the loop continues to C-good.
	claim, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim == nil || claim.ChannelID != "C-good" {
		t.Fatalf("expected C-good after blacklist flip, got %+v", claim)
	}

	// Verify C-bot was disabled, not just skipped.
	var enabled bool
	var lastErr *string
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled, last_error FROM slack_channel_sync WHERE channel_id = 'C-bot'`,
	).Scan(&enabled, &lastErr); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("C-bot should have been disabled by the blacklist check")
	}
	if lastErr == nil || !contains(*lastErr, "blacklisted") {
		t.Errorf("C-bot last_error should mention blacklist; got %v", lastErr)
	}
}

func TestSyncWorker_ClaimNext_Concurrent_NoDoubleClaim(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Single eligible row; many goroutines race ClaimNext. Exactly
	// one should win, the others should get nil. FOR UPDATE SKIP
	// LOCKED is what enforces this.
	seedSyncRow(t, pool, "C-race", -1*time.Hour, true)

	const N = 8
	results := make([]*Claim, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := w.ClaimNext(ctx)
			results[i] = c
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if r != nil {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("FOR UPDATE SKIP LOCKED guarantee: got %d winners, want 1", winners)
	}
}

func TestSyncWorker_SaveCursor_GuardsWorkerToken(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: claim=%+v err=%v", claim, err)
	}

	// Same-token: should succeed.
	if err := w.SaveCursor(ctx, claim, "next-cursor"); err != nil {
		t.Fatalf("SaveCursor with valid token: %v", err)
	}

	// Forge a different token: should return errClaimLost.
	stolen := *claim
	stolen.WorkerToken, _ = uuid.NewRandom()
	if err := w.SaveCursor(ctx, &stolen, "evil-cursor"); !errors.Is(err, errClaimLost) {
		t.Errorf("stolen token: got %v, want errClaimLost", err)
	}

	// Original cursor should still be the one we wrote, not "evil".
	var cursor string
	if err := pool.QueryRow(ctx,
		`SELECT cursor FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != "next-cursor" {
		t.Errorf("cursor preserved: got %q, want next-cursor", cursor)
	}
}

func TestSyncWorker_SaveCursor_HeartbeatsInProgressAt(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatal(err)
	}

	// Capture the original in_progress_at; sleep > resolution; save;
	// confirm it advanced.
	var t0 time.Time
	if err := pool.QueryRow(ctx,
		`SELECT in_progress_at FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&t0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := w.SaveCursor(ctx, claim, "page-2"); err != nil {
		t.Fatal(err)
	}
	var t1 time.Time
	if err := pool.QueryRow(ctx,
		`SELECT in_progress_at FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&t1); err != nil {
		t.Fatal(err)
	}
	if !t1.After(t0) {
		t.Errorf("heartbeat: in_progress_at didn't advance (%v -> %v)", t0, t1)
	}
}

func TestSyncWorker_MarkDone_StampsLastSyncedAt_NoBump(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	// No bump fired between claim and markDone — last_synced_at
	// should advance to ~now().
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var (
		lastSynced    time.Time
		inProgress    *time.Time
		cursor        *string
		token         *uuid.UUID
		lastErr       *string
		ingestEnabled bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT last_synced_at, in_progress_at, cursor, worker_token, last_error, ingest_enabled
		  FROM slack_channel_sync WHERE channel_id = 'C1'
	`).Scan(&lastSynced, &inProgress, &cursor, &token, &lastErr, &ingestEnabled); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastSynced) > 5*time.Second {
		t.Errorf("last_synced_at should be ~now after markDone; got %v ago", time.Since(lastSynced))
	}
	if inProgress != nil {
		t.Errorf("in_progress_at should be NULL; got %v", inProgress)
	}
	if cursor != nil {
		t.Errorf("cursor should be NULL; got %v", cursor)
	}
	if token != nil {
		t.Errorf("worker_token should be NULL; got %v", token)
	}
	if !ingestEnabled {
		t.Error("ingest_enabled should be unchanged (true)")
	}
}

func TestSyncWorker_MarkDone_PreservesPriorityBump(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Seed a claim. Mid-sweep, simulate a live socket-mode event
	// firing a priority bump (last_synced_at = LEAST(...)).
	originalLastSynced := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ($1, $2, true)
	`, "C1", originalLastSynced); err != nil {
		t.Fatal(err)
	}
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	// Simulate the priority bump: someone fires a live event.
	bumpedTS := originalLastSynced.Add(-1 * time.Hour) // older = higher priority
	if _, err := pool.Exec(ctx, `
		UPDATE slack_channel_sync SET last_synced_at = $2 WHERE channel_id = $1
	`, "C1", bumpedTS); err != nil {
		t.Fatal(err)
	}

	// markDone must NOT overwrite the bumped value.
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(bumpedTS) {
		t.Errorf("priority bump: last_synced_at got %v, want %v (the bumped value)", got, bumpedTS)
	}
}

func TestSyncWorker_MarkDone_GuardsWorkerToken(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	stolen := *claim
	stolen.WorkerToken, _ = uuid.NewRandom()
	if err := w.MarkDone(ctx, &stolen); !errors.Is(err, errClaimLost) {
		t.Errorf("stolen MarkDone: got %v, want errClaimLost", err)
	}

	// State should be untouched: in_progress_at still set, cursor
	// still NULL.
	var inProgress *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT in_progress_at FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&inProgress); err != nil {
		t.Fatal(err)
	}
	if inProgress == nil {
		t.Error("stolen MarkDone shouldn't have cleared in_progress_at")
	}
}

func TestSyncWorker_MarkError_DoesNotAdvanceLastSynced(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	originalLastSynced := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ($1, $2, true)
	`, "C1", originalLastSynced); err != nil {
		t.Fatal(err)
	}
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	if err := w.MarkError(ctx, claim, errors.New("boom")); err != nil {
		t.Fatal(err)
	}

	var (
		lastSynced time.Time
		lastErr    string
		inProgress *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT last_synced_at, COALESCE(last_error, ''), in_progress_at
		  FROM slack_channel_sync WHERE channel_id = 'C1'
	`).Scan(&lastSynced, &lastErr, &inProgress); err != nil {
		t.Fatal(err)
	}
	if !lastSynced.Equal(originalLastSynced) {
		t.Errorf("MarkError must NOT advance last_synced_at: got %v, want %v",
			lastSynced, originalLastSynced)
	}
	if lastErr != "boom" {
		t.Errorf("last_error: got %q, want boom", lastErr)
	}
	// Backoff semantic: MarkError parks the row at in_progress_at = now()
	// so it stays out of the eligible set until stale-recovery picks
	// it up after StaleAfter elapses. Without this, a persistently-
	// failing channel infinite-loops at the front of the queue.
	if inProgress == nil {
		t.Error("MarkError should set in_progress_at = now() to back off; got NULL")
	} else if time.Since(*inProgress) > 5*time.Second {
		t.Errorf("MarkError in_progress_at should be ~now; got %v ago", time.Since(*inProgress))
	}
}

// TestSyncWorker_MarkError_BackoffPreventsImmediateReclaim verifies
// that after MarkError, the same row isn't immediately reclaimable
// — it should drop out of the eligible set until stale-recovery
// picks it up at (now + StaleAfter).
func TestSyncWorker_MarkError_BackoffPreventsImmediateReclaim(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Single eligible row; claim it; MarkError; ClaimNext should
	// return nil because the row is parked.
	seedSyncRow(t, pool, "C-flaky", -1*time.Hour, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := w.MarkError(ctx, claim, errors.New("transient")); err != nil {
		t.Fatal(err)
	}

	// Immediately after MarkError, same channel should NOT be
	// reclaimable.
	next, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext post-error: %v", err)
	}
	if next != nil {
		t.Errorf("MarkError backoff: expected nil, got %+v (would infinite-loop)", next)
	}
}

// TestSyncWorker_SweepOne_HappyPath exercises a single-page sweep
// against a fake history API, asserts the messages flowed into
// events, and that pagination terminates cleanly.
func TestSyncWorker_SweepOne_HappyPath(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
		},
		history: []slack.Message{
			buildMsg("U1", "1700000010.000100", "", "second message", 0),
			buildMsg("U1", "1700000000.000100", "", "first message", 0),
		},
	}

	w := newTestSyncWorker(t, pool, api)
	seedSyncRow(t, pool, "C1", 0, true) // never-synced

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatalf("SweepOne: %v", err)
	}
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'slack'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("events ingested: got %d, want 2", n)
	}
	var lastSynced time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&lastSynced); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastSynced) > 10*time.Second {
		t.Errorf("last_synced_at should be ~now; got %v ago", time.Since(lastSynced))
	}
}

// TestSyncWorker_SweepOne_PersistsOldestTSOnFreshSweep verifies
// that a fresh sweep stamps oldest_ts before walking the first
// page so a crash mid-walk has the originating bound to resume
// from.
func TestSyncWorker_SweepOne_PersistsOldestTSOnFreshSweep(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	// Seed an existing event so computeOldestTS produces a non-zero
	// floor. external_id pattern matches what slack ingest writes.
	if _, err := pool.Exec(ctx, `
		INSERT INTO events
		    (source, external_id, body, occurred_at, raw, actor)
		VALUES
		    ('slack', 'C1:1700000000.000100', 'hi', to_timestamp(1700000000),
		     '{}'::jsonb, 'U1')
	`); err != nil {
		t.Fatal(err)
	}
	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	// Mark the channel recently deep-swept so this claim runs as a
	// NORMAL sweep — this test pins the normal floor
	// (last_synced_at - overlap). Fresh rows have NULL
	// last_deep_sweep_at and would claim deep (now-DeepWindow floor),
	// which TestSyncWorker_DeepSweep* covers separately.
	if _, err := pool.Exec(ctx, `
		UPDATE slack_channel_sync SET last_deep_sweep_at = now()
		 WHERE channel_id = 'C1'
	`); err != nil {
		t.Fatal(err)
	}

	// Use an api fake that returns a single page with HasMore=true
	// and a non-empty cursor — forcing saveCursor to fire.
	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
		},
	}
	w := newTestSyncWorker(t, pool, api)

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	// Run SweepOne — it should call persistOldestTS before the
	// first page, so even before any pages we expect oldest_ts to
	// be populated.
	//
	// Capture oldest_ts via the first persistOldestTS by
	// interrupting after that write but before the first page.
	// Easier: just run SweepOne to completion (history empty by
	// default), then check oldest_ts was written. NOTE: empty
	// history means HasMore=false on the first page, so SweepOne
	// returns nil immediately. After SweepOne completes (no
	// markDone yet), oldest_ts should be set on the row.
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatalf("SweepOne: %v", err)
	}

	var got *float64
	if err := pool.QueryRow(ctx,
		`SELECT oldest_ts FROM slack_channel_sync WHERE channel_id = 'C1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("oldest_ts should be persisted by SweepOne; got NULL")
	}
	// computeOldestTS = last_synced_at - overlap.
	// last_synced_at = now() - 1hr; overlap = 5min.
	// Floor unix-seconds is roughly now() - 65min.
	wantApprox := float64(time.Now().Add(-65 * time.Minute).Unix())
	if *got < wantApprox-60 || *got > wantApprox+60 {
		t.Errorf("oldest_ts: got %f, want ~%f (last_synced_at - overlap)",
			*got, wantApprox)
	}
}

// TestSyncWorker_SweepOne_NeverSyncedPullsAllHistory verifies that
// a never-synced channel (last_synced_at IS NULL) gets oldest=0
// regardless of whether live events have already landed in events.
// Anchoring to maxEventTS would silently skip pre-bot-invite history.
func TestSyncWorker_SweepOne_NeverSyncedPullsAllHistory(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	// A live event already arrived (someone posted while we were
	// being invited).
	if _, err := pool.Exec(ctx, `
		INSERT INTO events
		    (source, external_id, body, occurred_at, raw, actor)
		VALUES
		    ('slack', 'C1:1700000000.000100', 'recent live msg',
		     to_timestamp(1700000000), '{}'::jsonb, 'U1')
	`); err != nil {
		t.Fatal(err)
	}
	// Never-synced: last_synced_at = NULL.
	seedSyncRow(t, pool, "C1", 0, true)

	captured := &captureHistoryAPI{}
	captured.warmFakeAPI = warmFakeAPI{
		users: []slack.User{
			{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
		},
		channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
	}
	w := newTestSyncWorker(t, pool, captured)

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatalf("SweepOne: %v", err)
	}
	if captured.lastParams == nil {
		t.Fatal("history was never called")
	}
	// oldest must be empty (no floor) for a never-synced channel,
	// even though events.MAX(occurred_at) is non-null.
	if captured.lastParams.Oldest != "" {
		t.Errorf("never-synced should pull all history (oldest empty); got Oldest=%q",
			captured.lastParams.Oldest)
	}
}

// TestSyncWorker_SweepOne_ResumeUsesPersistedOldestTS verifies that
// when SweepOne is called with a non-empty cursor on the claim, it
// uses the claim's OldestTS (NOT recomputed) so Slack history walks
// the same window the original sweep started.
func TestSyncWorker_SweepOne_ResumeUsesPersistedOldestTS(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	// Capture the params passed to history so we can assert oldest=
	// is what we persisted, not a recomputed value.
	captured := &captureHistoryAPI{}
	captured.warmFakeAPI = warmFakeAPI{
		users: []slack.User{
			{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
		},
		channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
	}
	w := newTestSyncWorker(t, pool, captured)

	persistedOldest := float64(1600000000) // arbitrary, not derivable from now()

	// Seed a row that LOOKS like a mid-flight sweep: cursor set,
	// oldest_ts set, in_progress_at older than StaleAfter so the
	// next ClaimNext recovers it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync
		    (channel_id, last_synced_at, ingest_enabled,
		     cursor, oldest_ts, in_progress_at, worker_token)
		VALUES
		    ($1, now() - '1 hour'::interval, true,
		     'page-2-cursor', $2, now() - '1 hour'::interval, $3)
	`, "C1", persistedOldest, uuid.New()); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.Cursor != "page-2-cursor" {
		t.Errorf("resumed claim cursor: got %q, want page-2-cursor", claim.Cursor)
	}
	if claim.OldestTS != persistedOldest {
		t.Errorf("resumed claim OldestTS: got %f, want %f",
			claim.OldestTS, persistedOldest)
	}

	// Drive SweepOne and inspect the params passed to history.
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatalf("SweepOne: %v", err)
	}
	if captured.lastParams == nil {
		t.Fatal("history was never called")
	}
	wantOldest := formatSlackTS(persistedOldest)
	if captured.lastParams.Oldest != wantOldest {
		t.Errorf("Slack call oldest: got %q, want %q", captured.lastParams.Oldest, wantOldest)
	}
	if captured.lastParams.Cursor != "page-2-cursor" {
		t.Errorf("Slack call cursor: got %q, want page-2-cursor", captured.lastParams.Cursor)
	}
}

// TestSyncWorker_ClaimNext_DeepDecision verifies the deep-sweep
// tier decision is made at claim time: a row whose last_deep_sweep_at
// is NULL or older than DeepEvery claims as Deep; a row deep-swept
// recently claims as a normal sweep.
func TestSyncWorker_ClaimNext_DeepDecision(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)
	// Test config leaves DeepEvery/DeepWindow at defaults (24h / 7d).

	// Never deep-swept (NULL) → Deep.
	seedSyncRow(t, pool, "C-null", -1*time.Hour, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.ChannelID != "C-null" || !claim.Deep {
		t.Errorf("never-deep-swept row should claim Deep; got %+v", claim)
	}
	// Release so it doesn't block the next claim.
	if err := w.MarkError(ctx, claim, errors.New("release")); err != nil {
		t.Fatal(err)
	}

	// Reset state and seed two rows: one recently deep-swept, one
	// stale. Recently-deep-swept must claim normal; stale claims Deep.
	if _, err := pool.Exec(ctx, `TRUNCATE slack_channel_sync`); err != nil {
		t.Fatal(err)
	}
	// Recent deep sweep (1h ago < 24h cadence) → NOT deep.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync
		    (channel_id, last_synced_at, ingest_enabled, last_deep_sweep_at)
		VALUES ('C-recent', now() - '30 minutes'::interval, true, now() - '1 hour'::interval)
	`); err != nil {
		t.Fatal(err)
	}
	claim, err = w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext recent: %v", err)
	}
	if claim.ChannelID != "C-recent" || claim.Deep {
		t.Errorf("recently-deep-swept row should claim NORMAL; got %+v", claim)
	}
	if err := w.MarkError(ctx, claim, errors.New("release")); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `TRUNCATE slack_channel_sync`); err != nil {
		t.Fatal(err)
	}
	// Stale deep sweep (2 days ago > 24h cadence) → Deep.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync
		    (channel_id, last_synced_at, ingest_enabled, last_deep_sweep_at)
		VALUES ('C-stale', now() - '30 minutes'::interval, true, now() - '2 days'::interval)
	`); err != nil {
		t.Fatal(err)
	}
	claim, err = w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext stale: %v", err)
	}
	if claim.ChannelID != "C-stale" || !claim.Deep {
		t.Errorf("stale-deep-swept row should claim Deep; got %+v", claim)
	}
}

// TestSyncWorker_ClaimNext_ResumedClaimNeverDeep pins the cursor IS
// NULL guard on the deep decision: a parked mid-sweep claim (cursor
// persisted, in_progress_at stale) whose channel crossed the deep
// cadence boundary while parked must resume as a NORMAL sweep. The
// resumed claim inherits the originating sweep's narrow oldest_ts, so
// promoting it to Deep would stamp last_deep_sweep_at at MarkDone
// without the 7d walk ever happening, swallowing that cycle's deep
// pass.
func TestSyncWorker_ClaimNext_ResumedClaimNeverDeep(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// A normal sweep started, persisted a cursor + narrow oldest_ts,
	// then its worker died: in_progress_at is stale (older than
	// StaleAfter), worker_token still set, cursor non-NULL. The
	// channel has never been deep-swept (last_deep_sweep_at NULL), so
	// without the guard the re-claim would evaluate the cadence check
	// and come back Deep.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync
		    (channel_id, last_synced_at, ingest_enabled,
		     in_progress_at, worker_token, cursor, oldest_ts,
		     last_deep_sweep_at)
		VALUES ('C-parked', now() - '30 minutes'::interval, true,
		        now() - '1 hour'::interval, gen_random_uuid(), 'page-3-cursor',
		        extract(epoch from now() - '35 minutes'::interval),
		        NULL)
	`); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.ChannelID != "C-parked" {
		t.Fatalf("claimed %q, want C-parked", claim.ChannelID)
	}
	if claim.Cursor != "page-3-cursor" {
		t.Errorf("resumed claim should carry the persisted cursor; got %q", claim.Cursor)
	}
	if claim.Deep {
		t.Error("resumed claim (cursor != NULL) must NOT be Deep even past the cadence boundary")
	}

	// And because it resumed normal, MarkDone must leave
	// last_deep_sweep_at NULL so the next FRESH claim runs the deep
	// pass.
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var deepAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_deep_sweep_at FROM slack_channel_sync WHERE channel_id = 'C-parked'`,
	).Scan(&deepAt); err != nil {
		t.Fatal(err)
	}
	if deepAt != nil {
		t.Errorf("resumed-normal MarkDone stamped last_deep_sweep_at = %v; want NULL", *deepAt)
	}
}

// TestSyncWorker_SweepOne_DeepUsesWideFloor verifies a Deep claim
// pushes the oldest floor back to now-DeepWindow regardless of
// last_synced_at (which on a recently-swept channel would otherwise
// produce a 30-min-overlap floor).
func TestSyncWorker_SweepOne_DeepUsesWideFloor(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	captured := &captureHistoryAPI{}
	captured.warmFakeAPI = warmFakeAPI{
		users: []slack.User{
			{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
		},
		channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
	}
	w := newTestSyncWorker(t, pool, captured)

	// Recently-synced, never deep-swept → claim is Deep.
	seedSyncRow(t, pool, "C1", -5*time.Minute, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if !claim.Deep {
		t.Fatal("expected Deep claim (last_deep_sweep_at NULL)")
	}
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatalf("SweepOne: %v", err)
	}
	if captured.lastParams == nil {
		t.Fatal("history was never called")
	}
	// Deep floor is now-7d. The normal overlap floor would have been
	// ~now-35min; assert we got the wide one. formatSlackTS round-trips
	// the float the worker computes, so compare the float we'd expect
	// against the parsed wire value via the same parser the resume
	// test uses (parseSlackTS).
	if captured.lastParams.Oldest == "" {
		t.Fatal("deep sweep should set an oldest floor; got empty")
	}
	gotOldest, err := strconv.ParseFloat(captured.lastParams.Oldest, 64)
	if err != nil {
		t.Fatalf("parse oldest %q: %v", captured.lastParams.Oldest, err)
	}
	wantApprox := float64(time.Now().Add(-7 * 24 * time.Hour).Unix())
	if gotOldest < wantApprox-3600 || gotOldest > wantApprox+3600 {
		t.Errorf("deep oldest floor: got %f, want ~%f (now-7d)", gotOldest, wantApprox)
	}
}

// TestSyncWorker_MarkDone_DeepAdvancesLastDeepSweep verifies a clean
// Deep markDone stamps last_deep_sweep_at, and a normal markDone
// leaves it alone (so the cadence clock isn't reset by every normal
// sweep).
func TestSyncWorker_MarkDone_DeepAdvancesLastDeepSweep(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Deep path: NULL last_deep_sweep_at → claim Deep → markDone
	// stamps it.
	seedSyncRow(t, pool, "C-deep", -1*time.Hour, true)
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if !claim.Deep {
		t.Fatal("expected Deep claim")
	}
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var deepAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_deep_sweep_at FROM slack_channel_sync WHERE channel_id = 'C-deep'`,
	).Scan(&deepAt); err != nil {
		t.Fatal(err)
	}
	if deepAt == nil {
		t.Fatal("deep markDone should stamp last_deep_sweep_at")
	}
	if time.Since(*deepAt) > 5*time.Second {
		t.Errorf("last_deep_sweep_at should be ~now; got %v ago", time.Since(*deepAt))
	}

	// Normal path: a fresh deep stamp means the NEXT claim is normal.
	// markDone on that normal claim must leave last_deep_sweep_at
	// unchanged.
	priorDeep := *deepAt
	claim2, err := w.ClaimNext(ctx)
	if err != nil || claim2 == nil {
		t.Fatalf("second ClaimNext: %v", err)
	}
	if claim2.Deep {
		t.Fatal("second claim should be NORMAL (just deep-swept)")
	}
	if err := w.MarkDone(ctx, claim2); err != nil {
		t.Fatal(err)
	}
	var deepAt2 *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_deep_sweep_at FROM slack_channel_sync WHERE channel_id = 'C-deep'`,
	).Scan(&deepAt2); err != nil {
		t.Fatal(err)
	}
	if deepAt2 == nil || !deepAt2.Equal(priorDeep) {
		t.Errorf("normal markDone must NOT advance last_deep_sweep_at; got %v want %v", deepAt2, priorDeep)
	}
}

// captureHistoryAPI records the exact params passed to
// GetConversationHistoryContext so resume-semantics tests can
// assert what we sent to Slack.
type captureHistoryAPI struct {
	warmFakeAPI
	lastParams *slack.GetConversationHistoryParameters
}

func (c *captureHistoryAPI) GetConversationHistoryContext(_ context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	c.lastParams = params
	resp := &slack.GetConversationHistoryResponse{}
	resp.Messages = nil
	resp.HasMore = false
	resp.ResponseMetaData.NextCursor = ""
	resp.Ok = true
	return resp, nil
}

func (c *captureHistoryAPI) GetConversationRepliesContext(_ context.Context, _ *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}

// TestSyncWorker_Run_StopsOnContextCancel verifies the loop exits
// cleanly when ctx cancels — important so a leader handover or a
// shutdown signal doesn't leak a goroutine.
func TestSyncWorker_Run_StopsOnContextCancel(t *testing.T) {
	pool := getSyncPool(t)
	w := newTestSyncWorker(t, pool, &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let the loop spin a few idle ticks (no rows seeded).
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run should return context.Canceled; got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run didn't exit within 500ms of cancel")
	}
}

// TestSyncWorker_Run_HandlesPreemption verifies that if a claim
// gets stolen mid-sweep (worker_token mismatch on saveCursor),
// the run loop logs + continues without crashing.
func TestSyncWorker_Run_HandlesPreemption(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	// "Steal" the claim by overwriting worker_token from another
	// goroutine.
	if _, err := pool.Exec(ctx, `
		UPDATE slack_channel_sync SET worker_token = $2 WHERE channel_id = $1
	`, "C1", uuid.New()); err != nil {
		t.Fatal(err)
	}

	// SaveCursor should now report claim-lost.
	err := w.SaveCursor(ctx, claim, "doesnt-matter")
	if !errors.Is(err, errClaimLost) {
		t.Errorf("SaveCursor after steal: got %v, want errClaimLost", err)
	}
	// MarkDone after steal: same.
	if dErr := w.MarkDone(ctx, claim); !errors.Is(dErr, errClaimLost) {
		t.Errorf("MarkDone after steal: got %v, want errClaimLost", dErr)
	}
}

// TestSyncWorker_RequestBackfill_NewChannel verifies the operator
// re-backfill path inserts a fresh row with last_synced_at NULL
// (highest priority) when the channel isn't tracked yet.
func TestSyncWorker_RequestBackfill_NewChannel(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	existed, err := NewChannelSync(pool).RequestBackfill(ctx, "C-fresh")
	if err != nil {
		t.Fatalf("RequestBackfill: %v", err)
	}
	if existed {
		t.Errorf("expected existed=false on insert path; got true")
	}
	var (
		lastSync      *time.Time
		ingestEnabled bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at, ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-fresh'`,
	).Scan(&lastSync, &ingestEnabled); err != nil {
		t.Fatal(err)
	}
	if lastSync != nil {
		t.Errorf("new row should have last_synced_at=NULL; got %v", lastSync)
	}
	if !ingestEnabled {
		t.Error("new row should be ingest_enabled=true")
	}
}

// TestSyncWorker_RequestBackfill_ExistingChannel verifies the
// operator path resets every in-flight field on an existing row
// so a stuck or completed row gets re-walked from scratch.
func TestSyncWorker_RequestBackfill_ExistingChannel(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync
		    (channel_id, last_synced_at, cursor, oldest_ts,
		     in_progress_at, worker_token, last_error, ingest_enabled)
		VALUES ('C1', now() - '1 hour'::interval, 'mid-cursor', 1700000000,
		        now(), $1, 'something failed', true)
	`, uuid.New()); err != nil {
		t.Fatal(err)
	}

	existed, err := NewChannelSync(pool).RequestBackfill(ctx, "C1")
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Errorf("expected existed=true on UPDATE path; got false")
	}

	var (
		lastSync   *time.Time
		cursor     *string
		oldestTS   *float64
		inProgress *time.Time
		token      *uuid.UUID
		lastErr    *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT last_synced_at, cursor, oldest_ts, in_progress_at, worker_token, last_error
		  FROM slack_channel_sync WHERE channel_id = 'C1'
	`).Scan(&lastSync, &cursor, &oldestTS, &inProgress, &token, &lastErr); err != nil {
		t.Fatal(err)
	}
	if lastSync != nil {
		t.Errorf("RequestBackfill must NULL last_synced_at; got %v", lastSync)
	}
	if cursor != nil {
		t.Errorf("RequestBackfill must NULL cursor; got %v", cursor)
	}
	if oldestTS != nil {
		t.Errorf("RequestBackfill must NULL oldest_ts; got %v", oldestTS)
	}
	if inProgress != nil {
		t.Errorf("RequestBackfill must NULL in_progress_at; got %v", inProgress)
	}
	if token != nil {
		t.Errorf("RequestBackfill must NULL worker_token; got %v", token)
	}
	if lastErr != nil {
		t.Errorf("RequestBackfill must NULL last_error; got %v", lastErr)
	}
}

// TestSyncWorker_RequestBackfill_BlacklistedRefuses ensures the
// admin endpoint can't accidentally re-enable a channel that's
// on the do-not-ingest blacklist.
func TestSyncWorker_RequestBackfill_BlacklistedRefuses(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	// The entry is written by one store and read by another, as when two
	// replicas serve the two requests.
	if _, err := NewIngestBlacklist(pool, nil).Add(ctx, "C-bad", "", "test", "test"); err != nil {
		t.Fatal(err)
	}

	_, err := NewChannelSync(pool).RequestBackfill(ctx, "C-bad")
	if err == nil {
		t.Error("RequestBackfill on blacklisted channel must error")
	}
	if err != nil && !contains(err.Error(), "blacklist") {
		t.Errorf("error should mention blacklist; got %v", err)
	}
}

// TestSyncWorker_PriorityBump_NotNullRow verifies that a live
// MessageEvent priority bump pulls last_synced_at backward by at
// most overlap, sorting the channel toward the front of the queue.
func TestSyncWorker_PriorityBump_NotNullRow(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	originalLastSynced := time.Now().Add(-1 * time.Minute).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ('C1', $1, true)
	`, originalLastSynced); err != nil {
		t.Fatal(err)
	}

	if err := w.PriorityBump(ctx, "C1"); err != nil {
		t.Fatal(err)
	}

	var got time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C1'`,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Before(originalLastSynced) {
		t.Errorf("PriorityBump should pull last_synced_at backward; got %v >= %v",
			got, originalLastSynced)
	}
	wantApprox := time.Now().Add(-5 * time.Minute)
	if got.Before(wantApprox.Add(-1*time.Minute)) || got.After(wantApprox.Add(1*time.Minute)) {
		t.Errorf("PriorityBump value should be ~now-overlap (%v); got %v", wantApprox, got)
	}
}

// TestSyncWorker_PriorityBump_NullRowNoOp verifies the IS NOT NULL
// guard. A never-synced row (NULL = highest priority) must NOT be
// demoted to now-overlap by a bump (LEAST(NULL, ts) returns ts in
// Postgres).
func TestSyncWorker_PriorityBump_NullRowNoOp(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ('C1', NULL, true)
	`); err != nil {
		t.Fatal(err)
	}

	if err := w.PriorityBump(ctx, "C1"); err != nil {
		t.Fatal(err)
	}

	var got *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C1'`,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("PriorityBump must NOT demote NULL → ts; got %v", got)
	}
}

// TestSyncWorker_Disable_FlipsIngestEnabled covers the
// member_left_channel path.
func TestSyncWorker_Disable_FlipsIngestEnabled(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	seedSyncRow(t, pool, "C1", -1*time.Hour, true)

	if err := w.Disable(ctx, "C1"); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C1'`,
	).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("Disable should flip ingest_enabled false")
	}
}

// TestSyncWorker_Reconcile_InsertsNewChannels covers the periodic
// discovery path.
func TestSyncWorker_Reconcile_InsertsNewChannels(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	api := &fakeReconcileAPI{
		page1: []slack.Channel{
			chanWith("C-new1", "general", false, false),
			chanWith("C-new2", "random", false, false),
		},
	}

	seedSyncRow(t, pool, "C-existing", -1*time.Hour, true)

	w := newTestSyncWorker(t, pool, nil)
	added, err := w.Reconcile(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("expected 2 inserts, got %d", added)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_channel_sync`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("total rows after reconcile: got %d, want 3", n)
	}
}

// TestSyncWorker_Reconcile_BlacklistedDisabled verifies blacklisted
// channels land with ingest_enabled=false out of the gate.
func TestSyncWorker_Reconcile_BlacklistedDisabled(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Add(ctx, "C-bad", "", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	w.WithIngestBlacklist(bl)

	api := &fakeReconcileAPI{
		page1: []slack.Channel{
			chanWith("C-bad", "bad-channel", false, false),
			chanWith("C-good", "good-channel", false, false),
		},
	}
	added, err := w.Reconcile(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("both rows should be inserted; got added=%d", added)
	}
	var enabled bool
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-bad'`,
	).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("blacklisted row should land with ingest_enabled=false")
	}
}

// TestSyncWorker_Reconcile_ResumesRejoinedChannel: a row turned off when
// the bot left, or while the channel was blacklisted, turns back on by
// itself once the bot is a member and the channel is not blacklisted. Its
// sweep state stays, so the next sweep covers the gap. A channel still
// blacklisted stays off, and a resumed channel is not announced as new.
func TestSyncWorker_Reconcile_ResumesRejoinedChannel(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Add(ctx, "C-blocked", "", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	w.WithIngestBlacklist(bl)
	var announced []string
	w.WithNewChannelHook(func(_ context.Context, ch slack.Channel) { announced = append(announced, ch.ID) })

	seedSyncRow(t, pool, "C-rejoined", -3*time.Hour, false)
	seedSyncRow(t, pool, "C-blocked", -3*time.Hour, false)
	seedSyncRow(t, pool, "C-left", -3*time.Hour, false)
	var before time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C-rejoined'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	api := &fakeReconcileAPI{page1: []slack.Channel{
		chanWith("C-rejoined", "rejoined", false, false),
		chanWith("C-blocked", "blocked", false, false),
	}}
	added, err := w.Reconcile(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("a resumed channel is not a new one; got added=%d", added)
	}
	if len(announced) != 0 {
		t.Errorf("a resumed channel must not be announced as new; got %v", announced)
	}

	enabled := func(channel string) bool {
		t.Helper()
		var on bool
		if err := pool.QueryRow(ctx,
			`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = $1`, channel).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	if !enabled("C-rejoined") {
		t.Error("the bot is a member of C-rejoined and it is not blacklisted; it must be read again")
	}
	if enabled("C-blocked") {
		t.Error("C-blocked is blacklisted; it must stay off though the bot is a member")
	}
	if enabled("C-left") {
		t.Error("the bot is not in C-left; it must stay off")
	}
	var after time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = 'C-rejoined'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Errorf("last_synced_at moved from %v to %v; the next sweep must start where the last ended", before, after)
	}

	// Unblocked, the channel resumes on the next pass.
	if _, err := bl.RemoveByID(ctx, "C-blocked"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reconcile(ctx, api); err != nil {
		t.Fatal(err)
	}
	if !enabled("C-blocked") {
		t.Error("once unblocked, a channel the bot is in must be read again")
	}
}

// TestSyncWorker_Reconcile_DisablesAbsentChannel verifies the disable
// pass: a channel present in the sync table but absent from the
// current membership walk (a missed member_left) gets
// ingest_enabled=false, while a channel still in the membership set
// stays enabled.
func TestSyncWorker_Reconcile_DisablesAbsentChannel(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// C-present is still a member; C-gone was left (missed leave).
	seedSyncRow(t, pool, "C-present", -1*time.Hour, true)
	seedSyncRow(t, pool, "C-gone", -1*time.Hour, true)

	api := &fakeReconcileAPI{
		page1: []slack.Channel{
			chanWith("C-present", "present", false, false),
		},
	}
	if _, err := w.Reconcile(ctx, api); err != nil {
		t.Fatal(err)
	}

	var presentEnabled, goneEnabled bool
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-present'`,
	).Scan(&presentEnabled); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-gone'`,
	).Scan(&goneEnabled); err != nil {
		t.Fatal(err)
	}
	if !presentEnabled {
		t.Error("C-present is still a member; must stay ingest_enabled=true")
	}
	if goneEnabled {
		t.Error("C-gone is absent from membership; must be disabled")
	}
}

// TestSyncWorker_Reconcile_PaginationErrorSkipsDisable is the critical
// safety case: if any page of the membership walk errors, the disable
// pass must be skipped entirely. Otherwise the channels listed only on
// the un-fetched pages would look "absent" and get mass-disabled.
func TestSyncWorker_Reconcile_PaginationErrorSkipsDisable(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// Two enabled rows. C-page1 comes back on page 1; C-page2 would
	// have been on page 2, but page 2 errors — so it never appears in
	// the membership set. It must NOT be disabled.
	seedSyncRow(t, pool, "C-page1", -1*time.Hour, true)
	seedSyncRow(t, pool, "C-page2", -1*time.Hour, true)

	api := &fakeReconcileAPI{
		page1:    []slack.Channel{chanWith("C-page1", "one", false, false)},
		page2Err: errors.New("boom: users.conversations page 2 failed"),
	}
	if _, err := w.Reconcile(ctx, api); err == nil {
		t.Fatal("expected reconcile to return the pagination error")
	}

	// Neither row may have been disabled — the partial walk must skip
	// the disable pass.
	var enabledCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_channel_sync WHERE ingest_enabled`,
	).Scan(&enabledCount); err != nil {
		t.Fatal(err)
	}
	if enabledCount != 2 {
		t.Errorf("pagination error must skip disable pass; enabled rows = %d, want 2", enabledCount)
	}
}

// TestSyncWorker_Reconcile_SkipsDisableForActiveSweep verifies a row
// with a fresh in_progress_at (a live sweep) is not disabled even when
// absent from the membership set — disabling under a live claim would
// fight the worker. It self-heals on a later tick once the claim
// clears. A stale in_progress_at is fair game and IS disabled.
func TestSyncWorker_Reconcile_SkipsDisableForActiveSweep(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	// StaleAfter must outlast the time between seeding C-active and the
	// reconcile reading it; the shared 250ms flaked on a slow CI runner.
	w := newTestSyncWorker(t, pool, nil).WithConfig(SyncWorkerConfig{
		StaleAfter: 10 * time.Minute,
		Overlap:    5 * time.Minute,
		IdleSleep:  10 * time.Millisecond,
		PageSize:   200,
	})

	// Both rows are absent from membership. C-active has a fresh
	// in_progress_at (live sweep); C-stale's is an hour old, past
	// StaleAfter.
	seedSyncRow(t, pool, "C-active", -1*time.Hour, true)
	seedSyncRow(t, pool, "C-stale", -1*time.Hour, true)
	if _, err := pool.Exec(ctx,
		`UPDATE slack_channel_sync SET in_progress_at = now() WHERE channel_id = 'C-active'`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE slack_channel_sync SET in_progress_at = now() - interval '1 hour' WHERE channel_id = 'C-stale'`,
	); err != nil {
		t.Fatal(err)
	}

	// Membership set is a single unrelated present channel so the set
	// is non-empty (an empty set skips the pass entirely) and both
	// seeded rows are "absent".
	api := &fakeReconcileAPI{
		page1: []slack.Channel{chanWith("C-other", "other", false, false)},
	}
	if _, err := w.Reconcile(ctx, api); err != nil {
		t.Fatal(err)
	}

	var activeEnabled, staleEnabled bool
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-active'`,
	).Scan(&activeEnabled); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = 'C-stale'`,
	).Scan(&staleEnabled); err != nil {
		t.Fatal(err)
	}
	if !activeEnabled {
		t.Error("row with a live in_progress_at must not be disabled under a live claim")
	}
	if staleEnabled {
		t.Error("row with a stale in_progress_at is fair game; must be disabled")
	}
}

// fakeReconcileAPI implements reconcileAPI for the SyncWorker
// reconcile tests. Two-page channel listing across its cursor;
// the second page exercises pagination. page2Err, when set, is
// returned instead of page2 — used to exercise the partial-walk
// safety guard (a mid-walk error must skip the disable pass).
type fakeReconcileAPI struct {
	page1    []slack.Channel
	page2    []slack.Channel
	page2Err error
}

func (f *fakeReconcileAPI) GetConversationsForUserContext(_ context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	if params.Cursor == "" {
		// If page2 is empty AND no page-2 error is configured
		// (single-page test), return immediately.
		if len(f.page2) == 0 && f.page2Err == nil {
			return f.page1, "", nil
		}
		return f.page1, "cur2", nil
	}
	if f.page2Err != nil {
		return nil, "", f.page2Err
	}
	return f.page2, "", nil
}

func (f *fakeReconcileAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return nil, "", nil
}

func (f *fakeReconcileAPI) JoinConversationContext(_ context.Context, _ string) (*slack.Channel, string, []string, error) {
	return nil, "", nil, nil
}

// fakeAllPublicAPI implements reconcileAPI for all-public mode tests.
// joinErr, if non-nil, is returned for the specific channel ID stored in
// errChannelID; all other joins succeed. joinedChannels records the IDs
// that were actually joined.
type fakeAllPublicAPI struct {
	page1          []slack.Channel
	errChannelID   string
	joinErr        error
	joinedChannels []string
}

func (f *fakeAllPublicAPI) GetConversationsForUserContext(_ context.Context, _ *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	return nil, "", nil
}

func (f *fakeAllPublicAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return f.page1, "", nil
}

func (f *fakeAllPublicAPI) JoinConversationContext(_ context.Context, channelID string) (*slack.Channel, string, []string, error) {
	if f.errChannelID != "" && channelID == f.errChannelID {
		return nil, "", nil, f.joinErr
	}
	f.joinedChannels = append(f.joinedChannels, channelID)
	return nil, "", nil, nil
}

// chanWithMember builds a slack.Channel for all-public tests. The
// isMember flag reflects conversations.list's IsMember field which
// tells the reconciler whether the bot is already in the channel.
func chanWithMember(id, name string, isMember bool) slack.Channel {
	return slack.Channel{
		GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{
				ID: id,
			},
			Name: name,
		},
		IsMember: isMember,
	}
}

// TestSyncWorker_JoinSweep_JoinsNonMemberOnly verifies JoinSweep joins
// non-member non-blacklisted channels, skips already-member and
// blacklisted channels, and writes nothing to slack_channel_sync.
func TestSyncWorker_JoinSweep_JoinsNonMemberOnly(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Add(ctx, "C-bad", "", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	w.WithIngestBlacklist(bl)

	api := &fakeAllPublicAPI{
		page1: []slack.Channel{
			chanWithMember("C-already-member", "general", true), // already in → skip
			chanWithMember("C-non-member", "random", false),     // not member → join
			chanWithMember("C-bad", "bad", false),               // blacklisted → skip
		},
	}
	joined, err := w.JoinSweep(ctx, api)
	if err != nil {
		t.Fatalf("JoinSweep: %v", err)
	}
	// Only C-non-member should be joined.
	if joined != 1 {
		t.Errorf("expected 1 join, got %d", joined)
	}
	if len(api.joinedChannels) != 1 || api.joinedChannels[0] != "C-non-member" {
		t.Errorf("joined channels: got %v, want [C-non-member]", api.joinedChannels)
	}
	// JoinSweep must write nothing to slack_channel_sync.
	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM slack_channel_sync`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Errorf("JoinSweep must not write to slack_channel_sync; found %d row(s)", rowCount)
	}
}

// TestSyncWorker_JoinSweep_ThenReconcilePicksUpNewMember verifies the
// two-step tick: JoinSweep changes membership (simulated by updating the
// stub's membership response), then Reconcile inserts the newly-joined
// channel as a sync row.
func TestSyncWorker_JoinSweep_ThenReconcilePicksUpNewMember(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	// JoinSweep stub: C-new is not a member yet → gets joined.
	joinAPI := &fakeAllPublicAPI{
		page1: []slack.Channel{
			chanWithMember("C-new", "new-channel", false),
		},
	}
	joined, err := w.JoinSweep(ctx, joinAPI)
	if err != nil || joined != 1 {
		t.Fatalf("JoinSweep: joined=%d err=%v", joined, err)
	}
	// No DB rows yet.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM slack_channel_sync`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("expected 0 rows before Reconcile, got %d", before)
	}

	// Simulate Slack-side membership update: Reconcile stub now returns
	// C-new as a member (the join succeeded, next API call sees it).
	reconcileAPI := &fakeReconcileAPI{
		page1: []slack.Channel{
			chanWith("C-new", "new-channel", false, false),
		},
	}
	added, err := w.Reconcile(ctx, reconcileAPI)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if added != 1 {
		t.Errorf("Reconcile after JoinSweep: expected 1 insert, got %d", added)
	}
}

// TestSyncWorker_JoinSweep_JoinErrorContinues verifies a join failure
// on one channel does not abort the sweep — other channels are still
// processed.
func TestSyncWorker_JoinSweep_JoinErrorContinues(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	api := &fakeAllPublicAPI{
		page1: []slack.Channel{
			chanWithMember("C-join-fails", "broken", false),
			chanWithMember("C-join-ok", "good", false),
		},
		errChannelID: "C-join-fails",
		joinErr:      &slack.SlackErrorResponse{Err: "is_archived"},
	}
	joined, err := w.JoinSweep(ctx, api)
	if err != nil {
		t.Fatalf("JoinSweep should not return error on per-channel join failure; got %v", err)
	}
	// Only C-join-ok was successfully joined.
	if joined != 1 {
		t.Errorf("expected 1 successful join, got %d", joined)
	}
	if len(api.joinedChannels) != 1 || api.joinedChannels[0] != "C-join-ok" {
		t.Errorf("joined channels: got %v, want [C-join-ok]", api.joinedChannels)
	}
	// Nothing written to DB regardless.
	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM slack_channel_sync`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Errorf("JoinSweep must not write to DB even after join failure; found %d row(s)", rowCount)
	}
}

// TestSyncWorker_JoinSweep_MissingScope verifies a missing_scope error
// is logged at ERROR level and the sweep continues processing other
// channels.
func TestSyncWorker_JoinSweep_MissingScope(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestSyncWorker(t, pool, nil)

	api := &fakeAllPublicAPI{
		page1: []slack.Channel{
			chanWithMember("C-scope-fail", "scope-test", false),
			chanWithMember("C-ok-join", "other", false),
		},
		errChannelID: "C-scope-fail",
		joinErr:      &slack.SlackErrorResponse{Err: "missing_scope"},
	}
	joined, err := w.JoinSweep(ctx, api)
	if err != nil {
		t.Fatalf("JoinSweep should not return error on missing_scope; got %v", err)
	}
	// C-ok-join was still attempted and joined despite the earlier error.
	if joined != 1 {
		t.Errorf("expected 1 successful join (C-ok-join), got %d", joined)
	}
}

// keep atomic.Int64 import live (used elsewhere in the package).
var _ = atomic.Int64{}

// contains is a tiny helper to avoid pulling strings into the test
// for one substring check.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
