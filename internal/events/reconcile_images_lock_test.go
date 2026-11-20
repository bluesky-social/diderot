package events

// Tests for the event-row lock in reconcileImages (reconcile_images.go).
// Every writer of event_images takes
// `SELECT ... FROM events WHERE id = $1 FOR UPDATE` before touching
// event_images rows (ReconcileDocuments, ReconcileDocumentImagesTx, the
// docs worker, files.Worker.commitCaption, and reconcileImages). Without
// it, reconcileImages would take the event row only at its last statement
// (the image_count UPDATE), and two concurrent callers against the same
// event could take row locks in data-dependent orders and deadlock
// (Postgres SQLSTATE 40P01) instead of one committing cleanly and the
// other short-circuiting or superseding it, which is what
// reconcile_images.go's own doc comment ("Single transaction.
// Idempotent.") promises.
//
// Each test below is a plain, fixed-input reduction of one way concurrent
// callers would deadlock without the lock; none of them need the rapid-driven model-based
// property suite to be understood.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isDeadlockDetected reports whether err is Postgres SQLSTATE 40P01
// (deadlock_detected).
func isDeadlockDetected(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// TestReconcileImagesConcurrentDivergentRefOrderDoesNotDeadlock: two
// concurrent reconciles of the same event, with the same two brand-new
// attachments listed in opposite order, must not raise 40P01.
//
// Trigger: event has no image rows. Goroutine A reconciles refs [F1, F2]
// (INSERTs F1 then F2); goroutine B concurrently reconciles the same event
// with refs [F2, F1] (INSERTs F2 then F1) — the same attachment set built
// in a different order, exactly as two connectors, or a live event racing
// a backfill replay of the same message, could hand two concurrent
// ReconcileImages calls. Step 3 of reconcileImages issues one INSERT ...
// ON CONFLICT DO NOTHING per ref in refs' own order, with no canonical
// row-lock ordering across callers, so A can sit on F1 waiting for B's
// uncommitted F2 while B sits on F2 waiting for A's uncommitted F1.
//
// Contract: reconcile_images.go's doc comment ("Single transaction.
// Idempotent.") promises a clean commit or a clean no-op for reconciling
// one event's current attachment list, not a database error for two
// legitimate concurrent callers.
func TestReconcileImagesConcurrentDivergentRefOrderDoesNotDeadlock(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()

	// Without the lock, the deadlock reproduces on effectively every
	// attempt once both sides are doing fresh inserts in opposite order,
	// so a handful of attempts is enough to make a regression certain to
	// fail while keeping the test fast.
	const attempts = 10
	const roundsPerAttempt = 8
	deadlockSeen := false
	for attempt := 0; attempt < attempts && !deadlockSeen; attempt++ {
		eventID := seedReconcileEvent(t, pool, fmt.Sprintf("C1:divergent-order-%d", attempt))
		refsA := []ImageRef{{SourceImageID: "F1", Mimetype: "image/png"}, {SourceImageID: "F2", Mimetype: "image/png"}}
		refsB := []ImageRef{{SourceImageID: "F2", Mimetype: "image/png"}, {SourceImageID: "F1", Mimetype: "image/png"}}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var otherErr error
		run := func(refs []ImageRef) {
			defer wg.Done()
			for i := 0; i < roundsPerAttempt; i++ {
				// Clear the row set first so the next round's INSERTs are
				// always fresh (no pre-existing row for ON CONFLICT DO
				// NOTHING to find without touching the index).
				if err := ReconcileImages(ctx, pool, eventID, nil); err != nil && !isDeadlockDetected(err) {
					mu.Lock()
					otherErr = fmt.Errorf("clearing round: %w", err)
					mu.Unlock()
					return
				}
				if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
					if isDeadlockDetected(err) {
						mu.Lock()
						deadlockSeen = true
						mu.Unlock()
						continue
					}
					mu.Lock()
					otherErr = err
					mu.Unlock()
					return
				}
			}
		}
		wg.Add(2)
		go run(refsA)
		go run(refsB)
		wg.Wait()
		if otherErr != nil {
			t.Fatalf("attempt %d: unexpected (non-deadlock) error: %v", attempt, otherErr)
		}
	}
	if deadlockSeen {
		t.Fatalf("ReconcileImages raised SQLSTATE 40P01 (deadlock detected): two concurrent calls " +
			"reconciling the same event's attachments, listed in opposite order, made Postgres kill " +
			"one side outright instead of either completing cleanly")
	}
}

// TestReconcileImagesConcurrentShrinkingVersionsDoNotDeadlock: a
// Slack-shaped edit race — files are only ever removed from a message, and
// their relative order never changes (unlike the divergent-order case
// above) — must not raise 40P01 either.
//
// Trigger: event already has event_images rows for F1 and F2 (a committed
// version of the message). Goroutine A reconciles with refs=nil (every
// attachment removed — the newest edit); goroutine B concurrently
// reconciles with refs=[F1] (only F2 removed — an older, in-flight version
// of the same edit, or a redelivery of it). A's single DELETE statement
// removes both F1 and F2 and, having already locked F1's row by the time
// it is blocked trying to lock F2's row, holds F1 while waiting on B. B's
// DELETE removes F2 only, then its Step 3 INSERT of F1 (ON CONFLICT DO
// NOTHING, since F1 already exists) must inspect F1's row to resolve the
// conflict and blocks on A's held F1 lock. A waits on F2 (B holds it); B
// waits on F1 (A holds it): a two-transaction cycle.
//
// Contract: same as above — a concurrent, legitimate reconcile of a
// shrinking attachment list must not fail the caller outright.
func TestReconcileImagesConcurrentShrinkingVersionsDoNotDeadlock(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:shrinking-versions")

	// Seed the committed version: F1 and F2 both present.
	seedRefs := []ImageRef{
		{SourceImageID: "F1", Mimetype: "image/png"},
		{SourceImageID: "F2", Mimetype: "image/png"},
	}

	// Without the lock about a third of attempts deadlock when both sides start from the exact same {F1,F2}
	// state at the same moment, so a bounded number of attempts, each
	// re-seeding that state and starting both sides from a barrier, makes
	// the absence of any deadlock over the whole run a reliable signal
	// rather than luck.
	const attempts = 60
	deadlockSeen := false
	for attempt := 0; attempt < attempts && !deadlockSeen; attempt++ {
		if err := ReconcileImages(ctx, pool, eventID, seedRefs); err != nil {
			t.Fatalf("attempt %d: seeding {F1,F2}: %v", attempt, err)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var otherErr error
		start := make(chan struct{})
		race := func(refs []ImageRef) {
			defer wg.Done()
			<-start
			if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
				mu.Lock()
				defer mu.Unlock()
				if isDeadlockDetected(err) {
					deadlockSeen = true
				} else {
					otherErr = err
				}
			}
		}
		wg.Add(2)
		go race(nil)
		go race([]ImageRef{{SourceImageID: "F1", Mimetype: "image/png"}})
		close(start)
		wg.Wait()
		if otherErr != nil {
			t.Fatalf("attempt %d: unexpected (non-deadlock) error: %v", attempt, otherErr)
		}
	}
	if deadlockSeen {
		t.Fatalf("ReconcileImages raised SQLSTATE 40P01 (deadlock detected): racing a full " +
			"removal (nil refs) against a shrinking-but-nonempty edit ([F1]) of the same " +
			"committed {F1,F2} event made Postgres kill one side outright")
	}
}

// TestReconcileImagesCaptionCommitRacingRemovalDoesNotDeadlockOrOrphanCaption:
// a caption commit (files.Worker.commitCaption) racing a reconcile that
// removes the image being captioned.
//
// Trigger: event has one image, F1, uncaptioned. A caption-commit
// transaction (below, mirroring commitCaption's own lock order: event row
// first, then the image row) is raced against ReconcileImages(eventID,
// nil), which drops F1. Without an event-row lock
// of its own, ReconcileImages's Step 2 DELETE locks F1's event_images row
// immediately and only reaches the events row at Step 4 (the image_count
// UPDATE) — by which point the caption-commit side already holds the
// events row and is waiting to lock F1's row at its own Step 3. Each side
// holds what the other wants: a deadlock. This test forces exactly that
// interleaving (rather than hoping goroutine scheduling finds it) by
// waiting, via pg_locks, for ReconcileImages to be blocked before letting
// the caption-commit side take its own second lock.
//
// Contract: reconcile_images.go's own doc comment calls this exact
// scenario out as critical — "a poster who deletes an image must not have
// its caption linger in retrieval" — so beyond not deadlocking, the image
// and its caption chunk must both be gone once the race resolves: the
// reconcile is expected to win (last-committer-wins is documented,
// acceptable behavior), and letting the caption's chunk outlive the image
// it describes would mean a deleted image's caption stays searchable.
func TestReconcileImagesCaptionCommitRacingRemovalDoesNotDeadlockOrOrphanCaption(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:caption-vs-removal")

	if err := ReconcileImages(ctx, pool, eventID, []ImageRef{
		{SourceImageID: "F1", Mimetype: "image/png"},
	}); err != nil {
		t.Fatal(err)
	}
	var imageID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM event_images WHERE event_id = $1 AND source_image_id = 'F1'`,
		eventID).Scan(&imageID); err != nil {
		t.Fatal(err)
	}

	// Hold the event row FOR UPDATE ourselves first, exactly as the
	// caption commit's own Step 1 would, so we control when the second
	// (image-row) lock request on the caption side is issued.
	holder, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT id FROM events WHERE id = $1 FOR UPDATE`, eventID); err != nil {
		t.Fatal(err)
	}

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- ReconcileImages(ctx, pool, eventID, nil)
	}()

	// Wait for ReconcileImages to block on a lock somewhere (on unfixed
	// code: the events row at its Step 4, after it has already deleted
	// F1's row; on fixed code: the events row immediately, before
	// touching F1's row at all). Either way, by the time this returns,
	// ReconcileImages cannot be holding anything it hasn't already
	// acquired, so it is now safe to take the caption side's second lock
	// without racing the order in which ReconcileImages issues its own
	// statements.
	waitForBlockedBackend(t, pool, ctx, 5*time.Second)

	captionDone := make(chan error, 1)
	go func() {
		var chunkID int64
		err := holder.QueryRow(ctx, `
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			SELECT $1, 'image_caption', COALESCE(MAX(chunk_idx), -1) + 1, 'a caption', now()
			  FROM event_chunks WHERE event_id = $1 AND kind = 'image_caption'
			RETURNING id
		`, eventID).Scan(&chunkID)
		if err != nil {
			captionDone <- fmt.Errorf("insert chunk: %w", err)
			return
		}
		tag, err := holder.Exec(ctx, `
			UPDATE event_images i
			   SET captioned_at = now(), chunk_id = $2
			  FROM (SELECT id FROM event_images WHERE id = $1 FOR UPDATE) previous
			 WHERE i.id = previous.id
		`, imageID, chunkID)
		if err != nil {
			captionDone <- fmt.Errorf("update event_images: %w", err)
			return
		}
		if tag.RowsAffected() == 0 {
			captionDone <- errors.New("event_images row vanished mid-caption")
			return
		}
		captionDone <- holder.Commit(ctx)
	}()

	var captionErr, reconcileErr error
	select {
	case captionErr = <-captionDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the caption-commit side")
	}
	select {
	case reconcileErr = <-reconcileDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ReconcileImages")
	}

	if isDeadlockDetected(captionErr) || isDeadlockDetected(reconcileErr) {
		t.Fatalf("caption commit vs. ReconcileImages(nil) deadlocked (SQLSTATE 40P01): "+
			"captionErr=%v reconcileErr=%v", captionErr, reconcileErr)
	}
	if captionErr != nil {
		t.Fatalf("unexpected caption-commit error: %v", captionErr)
	}
	if reconcileErr != nil {
		t.Fatalf("unexpected ReconcileImages error: %v", reconcileErr)
	}

	var imageCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_images WHERE event_id = $1 AND source_image_id = 'F1'`,
		eventID).Scan(&imageCount); err != nil {
		t.Fatal(err)
	}
	if imageCount != 0 {
		t.Errorf("event_images rows for F1 after the race: got %d, want 0 (ReconcileImages(nil) removed it)", imageCount)
	}

	var orphanCaptions int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM event_chunks
		 WHERE event_id = $1 AND kind = 'image_caption'
	`, eventID).Scan(&orphanCaptions); err != nil {
		t.Fatal(err)
	}
	if orphanCaptions != 0 {
		t.Errorf("image_caption chunks left for event %d after F1 was removed: got %d, want 0 "+
			"(a deleted image's caption must not linger in retrieval)", eventID, orphanCaptions)
	}
}

// waitForBlockedBackend polls pg_locks until some other backend is
// waiting (not granted) on a lock, or fails the test after timeout. Used
// to force a specific interleaving between two transactions deterministically
// instead of hoping goroutine scheduling finds it.
func waitForBlockedBackend(t *testing.T, pool *pgxpool.Pool, ctx context.Context, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted AND a.pid <> pg_backend_pid()
		`).Scan(&n); err != nil {
			t.Fatalf("polling pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a backend to block on a lock")
}
