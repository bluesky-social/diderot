package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

func newTestThreadWorker(t *testing.T, pool *pgxpool.Pool, api backfillAPI) *ThreadWorker {
	t.Helper()
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")
	return NewThreadWorker(pool, bf, nil, prometheus.NewRegistry()).
		WithConfig(ThreadWorkerConfig{
			StaleAfter: 250 * time.Millisecond,
			IdleSleep:  10 * time.Millisecond,
		})
}

// newSyncWorkerForEnqueue builds a SyncWorker just for accessing
// enqueueThreadRoot in upsert-shape tests. The worker doesn't run;
// we just call the helper.
func newSyncWorkerForEnqueue(t *testing.T, pool *pgxpool.Pool) *SyncWorker {
	t.Helper()
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, nil, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")
	return NewSyncWorker(pool, bf, nil, prometheus.NewRegistry())
}

func TestThreadEnqueue_InsertsNewRow(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", "1700000050.000200"); err != nil {
		t.Fatal(err)
	}

	var (
		enqueuedAt  time.Time
		doneAt      *time.Time
		inProgress  *time.Time
		token       *uuid.UUID
		cursor      *string
		latestReply *string
		walked      *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT enqueued_at, done_at, in_progress_at, worker_token, cursor,
		       latest_reply, walked_latest_reply
		  FROM slack_thread_sync
		 WHERE channel_id = 'C1' AND thread_ts = '1700000000.000100'
	`).Scan(&enqueuedAt, &doneAt, &inProgress, &token, &cursor,
		&latestReply, &walked); err != nil {
		t.Fatal(err)
	}
	if time.Since(enqueuedAt) > 5*time.Second {
		t.Errorf("enqueued_at should be ~now; got %v ago", time.Since(enqueuedAt))
	}
	if doneAt != nil || inProgress != nil || token != nil || cursor != nil {
		t.Errorf("fresh row should have all in-flight fields NULL; got done_at=%v in_progress_at=%v token=%v cursor=%v",
			doneAt, inProgress, token, cursor)
	}
	// Insert path stamps the pending latest_reply; walked is still NULL
	// (no walk has confirmed coverage yet).
	if latestReply == nil || *latestReply != "1700000050.000200" {
		t.Errorf("insert should stamp latest_reply; got %v", latestReply)
	}
	if walked != nil {
		t.Errorf("fresh row should have walked_latest_reply NULL; got %v", walked)
	}
}

func TestThreadEnqueue_ReopensCompletedRow(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	// Seed a completed thread row.
	oldDoneAt := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at, done_at)
		VALUES ($1, $2, $3, $4)
	`, "C1", "1700000000.000100",
		oldDoneAt.Add(-30*time.Minute), oldDoneAt); err != nil {
		t.Fatal(err)
	}

	// Re-discovery: should reset done_at to NULL + bump enqueued_at.
	// walked_latest_reply is NULL on the seeded row, so any populated
	// latest_reply clears the gate (NULL counts as "older than
	// anything").
	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", "1700000050.000200"); err != nil {
		t.Fatal(err)
	}

	var (
		enqueuedAt time.Time
		doneAt     *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT enqueued_at, done_at
		  FROM slack_thread_sync
		 WHERE channel_id = 'C1' AND thread_ts = '1700000000.000100'
	`).Scan(&enqueuedAt, &doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt != nil {
		t.Errorf("re-open should NULL done_at; got %v", doneAt)
	}
	if time.Since(enqueuedAt) > 5*time.Second {
		t.Errorf("enqueued_at should be ~now after reopen; got %v ago", time.Since(enqueuedAt))
	}
}

// TestThreadEnqueue_LeavesInflightWalksAlone verifies the
// livelock-prevention behavior: a sweep that re-discovers a
// thread root mid-walk MUST be a no-op on the in-flight row.
// Otherwise the 30-min history overlap → constant re-discovery
// → constant cursor reset → walker never finishes a thread that
// takes longer than the sweep cadence.
//
// New replies posted during the walk are caught one cycle later:
// walker completes (sets done_at), next sweep re-discovers the
// root (done_at IS NOT NULL → re-opens), worker walks again.
func TestThreadEnqueue_LeavesInflightWalksAlone(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	// Seed a row that LOOKS like an in-flight walk: done_at NULL,
	// in_progress_at recent, worker_token set, cursor mid-pagination.
	walkerToken := uuid.New()
	originalEnqueued := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at,
		     in_progress_at, worker_token, cursor)
		VALUES ($1, $2, $3, now(), $4, 'mid-cursor')
	`, "C1", "1700000000.000100", originalEnqueued, walkerToken); err != nil {
		t.Fatal(err)
	}

	// Sweep re-discovers the thread root mid-walk, with a newer
	// latest_reply. Must still be a no-op (done_at IS NULL → the
	// livelock-prevention WHERE fails regardless of the gate).
	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", "1700000099.000900"); err != nil {
		t.Fatal(err)
	}

	// All in-flight state should be UNTOUCHED — that's the
	// livelock-prevention guarantee.
	var (
		token      *uuid.UUID
		cursor     *string
		enqueuedAt time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT worker_token, cursor, enqueued_at
		  FROM slack_thread_sync
		 WHERE channel_id = 'C1' AND thread_ts = '1700000000.000100'
	`).Scan(&token, &cursor, &enqueuedAt); err != nil {
		t.Fatal(err)
	}
	if token == nil || *token != walkerToken {
		t.Errorf("worker_token must be preserved on in-flight row; got %v want %v",
			token, walkerToken)
	}
	if cursor == nil || *cursor != "mid-cursor" {
		t.Errorf("cursor must be preserved on in-flight row; got %v want mid-cursor",
			cursor)
	}
	if !enqueuedAt.Equal(originalEnqueued) {
		t.Errorf("enqueued_at must be preserved on in-flight row; got %v want %v",
			enqueuedAt, originalEnqueued)
	}

	// The walker's MarkDone should still succeed (its token guard
	// is intact), and the row transitions to done_at IS NOT NULL.
	tw := newTestThreadWorker(t, pool, nil)
	walker := &ThreadClaim{
		ChannelID:   "C1",
		ThreadTS:    "1700000000.000100",
		WorkerToken: walkerToken,
	}
	if err := tw.MarkDone(ctx, walker); err != nil {
		t.Fatalf("walker MarkDone after no-op re-discovery: %v", err)
	}
	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt == nil {
		t.Error("walker MarkDone should have stamped done_at on the in-flight row")
	}
}

func TestThreadWorker_ClaimNext_MostRecentActivityFirst(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	// Two claimable threads with deliberately-provenanced recency keys.
	//
	// Thread A has OLD source activity (root ts 1700000001, no replies
	// so activity_ts = root ts) but was enqueued NOW (recent enqueue).
	// Thread B has RECENT source activity (root 1700000100 with a
	// latest_reply of 1700000100.000900, so activity_ts is that) but
	// was enqueued AN HOUR AGO.
	//
	// The claim must follow SOURCE activity recency (activity_ts DESC):
	// B wins even though A was enqueued more recently. Both an
	// insertion-time ordering (enqueued_at ASC — the pre-change FIFO)
	// and a naive "recent enqueue wins" (enqueued_at DESC) would
	// wrongly pick A, so this is a negative control against using a
	// processing timestamp as the recency key.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at)
		VALUES ('C1', '1700000001.000100', now())
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, latest_reply)
		VALUES ('C1', '1700000100.000100', now() - '1 hour'::interval,
		        '1700000100.000900')
	`); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.ThreadTS != "1700000100.000100" {
		t.Errorf("most-recent-activity first: got %s, want 1700000100.000100 (recent source activity, but older enqueue)",
			claim.ThreadTS)
	}
}

func TestThreadWorker_ClaimNext_SkipsDoneRows(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	// Done row is older but should be skipped.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at, done_at)
		VALUES ('C1', '1700000001.000100', now() - '1 hour'::interval, now())
	`); err != nil {
		t.Fatal(err)
	}
	// Pending row newer.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at)
		VALUES ('C1', '1700000002.000100', now() - '5 minutes'::interval)
	`); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.ThreadTS != "1700000002.000100" {
		t.Errorf("done rows should be skipped; got %s", claim.ThreadTS)
	}
}

func TestThreadWorker_MarkDone_HonorsWorkerToken(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at)
		VALUES ('C1', '1700000001.000100', now() - '1 hour'::interval)
	`); err != nil {
		t.Fatal(err)
	}
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	// Stolen claim
	stolen := *claim
	stolen.WorkerToken = uuid.New()
	if err := w.MarkDone(ctx, &stolen); !errors.Is(err, errClaimLost) {
		t.Errorf("stolen MarkDone: got %v, want errClaimLost", err)
	}

	// Verify done_at not set
	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = '1700000001.000100'`,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt != nil {
		t.Error("stolen MarkDone shouldn't have stamped done_at")
	}

	// Real claim should succeed
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = '1700000001.000100'`,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt == nil {
		t.Error("real MarkDone should have stamped done_at")
	}
}

func TestThreadWorker_MarkError_BacksOff(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at)
		VALUES ('C1', '1700000001.000100', now() - '1 hour'::interval)
	`); err != nil {
		t.Fatal(err)
	}
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}

	if err := w.MarkError(ctx, claim, errors.New("boom")); err != nil {
		t.Fatal(err)
	}

	// Same row should NOT be reclaimable (backed off).
	next, err := w.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Errorf("MarkError backoff: expected nil, got %+v (would infinite-loop)", next)
	}

	// Verify state.
	var (
		inProgress *time.Time
		lastErr    string
		doneAt     *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT in_progress_at, COALESCE(last_error, ''), done_at
		  FROM slack_thread_sync WHERE thread_ts = '1700000001.000100'
	`).Scan(&inProgress, &lastErr, &doneAt); err != nil {
		t.Fatal(err)
	}
	if inProgress == nil {
		t.Error("MarkError should park in_progress_at = now()")
	}
	if lastErr != "boom" {
		t.Errorf("last_error: got %q, want boom", lastErr)
	}
	if doneAt != nil {
		t.Error("MarkError should NOT set done_at; row stays claimable after backoff")
	}
}

// TestThreadWorker_WalkOne_HappyPath drives one thread end-to-end:
// claim, walk, ingest replies, mark done.
func TestThreadWorker_WalkOne_HappyPath(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	rootTS := "1700000000.000100"
	reply1 := "1700000050.000200"
	reply2 := "1700000100.000300"

	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
		},
		// Slack returns root + replies, oldest-first.
		replies: map[string][]slack.Message{
			rootTS: {
				buildMsg("U1", rootTS, rootTS, "is production down?", 2),
				buildMsg("U1", reply1, rootTS, "looking into it", 0),
				buildMsg("U1", reply2, rootTS, "back now", 0),
			},
		},
	}
	w := newTestThreadWorker(t, pool, api)

	// Pre-seed the root in events (simulates pass-1 having ingested it).
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, body, occurred_at, raw, actor)
		VALUES ('slack', 'C1:'||$1, 'is production down?',
		        to_timestamp(1700000000), '{}'::jsonb, 'U1')
	`, rootTS); err != nil {
		t.Fatal(err)
	}
	// Enqueue the thread.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at)
		VALUES ('C1', $1, now())
	`, rootTS); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := w.WalkOne(ctx, claim); err != nil {
		t.Fatalf("WalkOne: %v", err)
	}
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	// 3 events: pre-seeded root + 2 replies.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'slack'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("events count: got %d, want 3", n)
	}

	// Thread row marked done.
	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = $1`, rootTS,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt == nil {
		t.Error("done_at should be set after WalkOne + MarkDone")
	}
}

// TestThreadWorker_RewalkAfterCompletion — the actual flow that
// captures new replies posted during a walk:
//
//  1. Sweep enqueues thread root.
//  2. Walker claims, paginates (may take a while).
//  3. Mid-walk, sweep re-discovers — but the WHERE-gated upsert
//     leaves the in-flight row alone (livelock prevention).
//  4. Walker completes, stamps done_at.
//  5. NEXT sweep re-discovers, sees done_at IS NOT NULL, re-opens.
//  6. New ClaimNext picks up the re-opened row, walker walks again.
//
// New replies arriving during step (2) get caught in step (6).
// One sweep cycle of staleness, no livelock.
func TestThreadWorker_RewalkAfterCompletion(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	tw := newTestThreadWorker(t, pool, nil)
	sw := newSyncWorkerForEnqueue(t, pool)

	rootTS := "1700000000.000100"
	replyA := "1700000050.000200" // latest reply at first walk
	replyB := "1700000099.000900" // a new reply arrived during the walk

	// Step 1: initial enqueue with the latest_reply at discovery.
	if err := sw.enqueueThreadRoot(ctx, "C1", rootTS, replyA); err != nil {
		t.Fatal(err)
	}

	// Step 2: walker claims; carries the pending latest_reply.
	claim, err := tw.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("first ClaimNext: %v", err)
	}
	if claim.LatestReply != replyA {
		t.Errorf("claim should carry pending latest_reply; got %q want %q",
			claim.LatestReply, replyA)
	}

	// Step 3: mid-walk re-discovery is a no-op (livelock prevention
	// path; tested separately). A new reply (replyB) has arrived, but
	// done_at IS NULL so the row is untouched.
	if err := sw.enqueueThreadRoot(ctx, "C1", rootTS, replyB); err != nil {
		t.Fatal(err)
	}

	// Step 4: walker completes; promotes walked_latest_reply = replyA
	// (the enqueue-time pending value the claim carried).
	if err := tw.MarkDone(ctx, claim); err != nil {
		t.Fatalf("walker MarkDone: %v", err)
	}
	var walked *string
	if err := pool.QueryRow(ctx,
		`SELECT walked_latest_reply FROM slack_thread_sync WHERE thread_ts = $1`, rootTS,
	).Scan(&walked); err != nil {
		t.Fatal(err)
	}
	if walked == nil || *walked != replyA {
		t.Errorf("MarkDone should promote walked_latest_reply=%q; got %v", replyA, walked)
	}

	// Step 5: next sweep re-discovers with replyB > walked (replyA).
	// done_at IS NOT NULL AND the gate clears → re-open.
	if err := sw.enqueueThreadRoot(ctx, "C1", rootTS, replyB); err != nil {
		t.Fatal(err)
	}

	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = $1`, rootTS,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt != nil {
		t.Error("post-completion re-discovery should reset done_at to NULL")
	}

	// Step 6: next claim picks up the re-opened row with a fresh
	// token.
	next, err := tw.ClaimNext(ctx)
	if err != nil || next == nil {
		t.Fatalf("post-rewalk ClaimNext: %v claim=%v", err, next)
	}
	if next.WorkerToken == claim.WorkerToken {
		t.Error("re-opened claim should mint a fresh worker_token")
	}
}

// TestThreadWorker_MarkError_PreservesCursor verifies that
// transient errors don't throw away pagination progress. Critical
// for long threads — without preservation, every transient error
// restarts the walk from page 1.
func TestThreadWorker_MarkError_PreservesCursor(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, cursor)
		VALUES ('C1', '1700000001.000100', now() - '1 hour'::interval, 'page-7')
	`); err != nil {
		t.Fatal(err)
	}
	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}
	if claim.Cursor != "page-7" {
		t.Errorf("ClaimNext should return persisted cursor; got %q", claim.Cursor)
	}

	if err := w.MarkError(ctx, claim, errors.New("transient")); err != nil {
		t.Fatal(err)
	}

	// Cursor must survive MarkError. Otherwise the next claim
	// resumes from page 1.
	var cursor *string
	if err := pool.QueryRow(ctx,
		`SELECT cursor FROM slack_thread_sync WHERE thread_ts = '1700000001.000100'`,
	).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor == nil || *cursor != "page-7" {
		t.Errorf("MarkError must preserve cursor; got %v want page-7", cursor)
	}
}

// TestSyncWorker_SweepOne_EnqueuesThreadRoot exercises the
// SyncWorker → enqueueThreadRoot integration: history sweep that
// returns a thread root must populate slack_thread_sync.
func TestSyncWorker_SweepOne_EnqueuesThreadRoot(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	rootTS := "1700000000.000100"
	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
		},
		history: []slack.Message{
			// Thread root with replies; SweepOne should enqueue it.
			buildMsg("U1", rootTS, rootTS, "is prod down?", 2),
		},
	}
	w := newTestSyncWorker(t, pool, api)
	seedSyncRow(t, pool, "C1", 0, true) // never-synced

	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_thread_sync WHERE channel_id = 'C1'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 thread_sync row from sweep; got %d", n)
	}
}

// TestSyncWorker_SweepOne_EnqueuesLatestReply verifies the full
// production wiring: a history-pass thread root carries latest_reply
// through to slack_thread_sync.latest_reply (so the gate has a value
// to compare on the next sweep).
func TestSyncWorker_SweepOne_EnqueuesLatestReply(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()

	rootTS := "1700000000.000100"
	latestReply := "1700000050.000200"
	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "general", false, false)},
		},
		history: []slack.Message{
			buildThreadRoot("U1", rootTS, "is prod down?", 2, latestReply),
		},
	}
	w := newTestSyncWorker(t, pool, api)
	seedSyncRow(t, pool, "C1", 0, true) // never-synced

	claim, _ := w.ClaimNext(ctx)
	if claim == nil {
		t.Fatal("ClaimNext returned nil")
	}
	if err := w.SweepOne(ctx, claim); err != nil {
		t.Fatal(err)
	}

	var got *string
	if err := pool.QueryRow(ctx,
		`SELECT latest_reply FROM slack_thread_sync WHERE channel_id = 'C1' AND thread_ts = $1`,
		rootTS,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != latestReply {
		t.Errorf("sweep should carry latest_reply into slack_thread_sync; got %v want %q", got, latestReply)
	}
}

// TestThreadEnqueue_GateUnchangedLatestReply_NoReopen verifies the
// core waste-reduction property: a completed thread whose
// latest_reply has NOT advanced past walked_latest_reply is left
// done (no re-walk).
func TestThreadEnqueue_GateUnchangedLatestReply_NoReopen(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	// Completed row: walked up to replyA, latest_reply also replyA.
	replyA := "1700000050.000200"
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, done_at,
		     latest_reply, walked_latest_reply)
		VALUES ('C1', '1700000000.000100', now() - '1 hour'::interval, now(),
		        $1, $1)
	`, replyA); err != nil {
		t.Fatal(err)
	}

	// Sweep re-discovers with the SAME latest_reply — gate must not
	// re-open.
	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", replyA); err != nil {
		t.Fatal(err)
	}

	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt == nil {
		t.Error("unchanged latest_reply must NOT re-open a completed row")
	}
}

// TestThreadEnqueue_GateAdvancedLatestReply_Reopens verifies the
// data-loss fix: a completed thread whose latest_reply advanced past
// walked_latest_reply IS re-opened so the new reply gets walked.
func TestThreadEnqueue_GateAdvancedLatestReply_Reopens(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	replyA := "1700000050.000200"
	replyB := "1700000099.000900" // newer
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, done_at,
		     latest_reply, walked_latest_reply)
		VALUES ('C1', '1700000000.000100', now() - '1 hour'::interval, now(),
		        $1, $1)
	`, replyA); err != nil {
		t.Fatal(err)
	}

	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", replyB); err != nil {
		t.Fatal(err)
	}

	var (
		doneAt      *time.Time
		latestReply *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT done_at, latest_reply FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&doneAt, &latestReply); err != nil {
		t.Fatal(err)
	}
	if doneAt != nil {
		t.Error("advanced latest_reply must re-open the completed row (done_at NULL)")
	}
	if latestReply == nil || *latestReply != replyB {
		t.Errorf("re-open should stamp the new pending latest_reply; got %v want %q", latestReply, replyB)
	}
}

// TestThreadEnqueue_GateNullWalked_Reopens verifies a completed row
// that never recorded walked_latest_reply (NULL) is treated as
// "older than anything" and re-opened by any populated latest_reply.
func TestThreadEnqueue_GateNullWalked_Reopens(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, done_at)
		VALUES ('C1', '1700000000.000100', now() - '1 hour'::interval, now())
	`); err != nil {
		t.Fatal(err)
	}

	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", "1700000050.000200"); err != nil {
		t.Fatal(err)
	}

	var doneAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT done_at FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt != nil {
		t.Error("NULL walked_latest_reply must re-open on any populated latest_reply")
	}
}

// TestThreadEnqueue_MissingRow_AlwaysInserts verifies a never-seen
// thread root is always enqueued (the gate only applies to the
// ON CONFLICT re-open branch, never to a fresh INSERT).
func TestThreadEnqueue_MissingRow_AlwaysInserts(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newSyncWorkerForEnqueue(t, pool)

	// Even with an empty latest_reply, a missing row must insert.
	if err := w.enqueueThreadRoot(ctx, "C1", "1700000000.000100", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("missing row should always insert; got %d rows", n)
	}
}

// TestThreadWorker_MarkDone_RecordsWalkedLatestReply verifies the
// promotion: the pending latest_reply on the claimed row becomes
// walked_latest_reply at markDone.
func TestThreadWorker_MarkDone_RecordsWalkedLatestReply(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	replyA := "1700000050.000200"
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, latest_reply)
		VALUES ('C1', '1700000000.000100', now() - '1 hour'::interval, $1)
	`, replyA); err != nil {
		t.Fatal(err)
	}
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.LatestReply != replyA {
		t.Errorf("claim should carry pending latest_reply; got %q want %q",
			claim.LatestReply, replyA)
	}
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var walked *string
	if err := pool.QueryRow(ctx,
		`SELECT walked_latest_reply FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&walked); err != nil {
		t.Fatal(err)
	}
	if walked == nil || *walked != replyA {
		t.Errorf("MarkDone should record walked_latest_reply=%q; got %v", replyA, walked)
	}
}

// TestThreadWorker_MarkDone_EmptyPendingPreservesWalked verifies that
// an empty pending latest_reply (Slack omitted it) does NOT regress a
// previously-recorded walked_latest_reply.
func TestThreadWorker_MarkDone_EmptyPendingPreservesWalked(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)

	prior := "1700000050.000200"
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at,
		     latest_reply, walked_latest_reply)
		VALUES ('C1', '1700000000.000100', now() - '1 hour'::interval, NULL, $1)
	`, prior); err != nil {
		t.Fatal(err)
	}
	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claim.LatestReply != "" {
		t.Errorf("claim should carry empty pending latest_reply; got %q", claim.LatestReply)
	}
	if err := w.MarkDone(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var walked *string
	if err := pool.QueryRow(ctx,
		`SELECT walked_latest_reply FROM slack_thread_sync WHERE thread_ts = '1700000000.000100'`,
	).Scan(&walked); err != nil {
		t.Fatal(err)
	}
	if walked == nil || *walked != prior {
		t.Errorf("empty pending must preserve prior walked_latest_reply=%q; got %v", prior, walked)
	}
}
