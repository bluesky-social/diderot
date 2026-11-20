package files

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestClaimOnlyRegisteredSourcesAndLiveParents(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	slackID := seedEventForWorker(t, pool, "slack-file")
	seedImage(t, pool, slackID, "slack-file")
	zendeskID := seedEventForWorker(t, pool, "zendesk-file")
	zendeskImage := seedImage(t, pool, zendeskID, "zendesk-file")
	_, err := pool.Exec(ctx, `UPDATE events SET source = 'zendesk' WHERE id = $1`, zendeskID)
	require.NoError(t, err)
	worker := &Worker{Pool: pool}
	row, err := worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.Nil(t, row)
	worker.Fetchers = map[string]SourceFetcher{"zendesk": &fakeFetcher{}}
	row, err = worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, zendeskImage, row.imageID)
	row, err = worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.Nil(t, row)
	// A fetcher that can't fetch right now (a stopped connector's) claims
	// nothing, so its rows keep their failure budget.
	slack := &switchableFetcher{}
	worker.Fetchers["slack"] = slack
	row, err = worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.Nil(t, row)
	slack.available = true
	row, err = worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, row)
	_, err = pool.Exec(ctx, `UPDATE event_images SET claimed_at = NULL WHERE event_id = $1`, slackID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, slackID)
	require.NoError(t, err)
	row, err = worker.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.Nil(t, row)
}

type switchableFetcher struct {
	fakeFetcher
	available bool
}

func (f *switchableFetcher) Available() bool { return f.available }

// getWorkerPool returns a pool with the events table truncated
// (cascades to event_images via FK). Tests seed event_images rows
// directly via helpers below.
func getWorkerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.URL(t, "slack_files"))
	if err != nil {
		t.Fatal(err)
	}
	// Concurrency tests need >= goroutines+1 conns. Five is enough
	// for the 4-worker case + a controller goroutine.
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
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

func seedEventForWorker(t *testing.T, pool *pgxpool.Pool, externalID string) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO events
			(source, external_id, occurred_at, actor, body, raw)
		VALUES ('slack', $1, $2, 'tester', 'body', '{}'::jsonb)
		RETURNING id
	`, externalID, time.Now().UTC()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func seedImage(t *testing.T, pool *pgxpool.Pool, eventID int64, sourceImageID string) int64 {
	t.Helper()
	ctx := context.Background()
	var imageID int64
	// occurred_at derives from the parent event, same as the
	// production ReconcileImages path — keeps seeded rows consistent
	// with the NOT NULL denormalization added in
	// 20260722000001_event_images_occurred_at.sql.
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at)
		SELECT $1, $2, 'image/png', e.occurred_at FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID, sourceImageID).Scan(&imageID); err != nil {
		t.Fatal(err)
	}
	return imageID
}

// seedEventForWorkerAt is seedEventForWorker with an explicit
// occurred_at, so tests can construct events whose recency differs
// from their insertion order. Used to build the id-vs-occurred_at
// ordering conflict that
// TestClaimUncaptionedPrefersRecentOverInsertionOrder exercises.
func seedEventForWorkerAt(t *testing.T, pool *pgxpool.Pool, externalID string, occurredAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO events
			(source, external_id, occurred_at, actor, body, raw)
		VALUES ('slack', $1, $2, 'tester', 'body', '{}'::jsonb)
		RETURNING id
	`, externalID, occurredAt).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestClaimUncaptionedReturnsRow(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("expected a row, got nil")
	}
	if row.imageID != imageID {
		t.Errorf("imageID: got %d, want %d", row.imageID, imageID)
	}
	if row.source != "slack" {
		t.Errorf("source: got %q, want slack", row.source)
	}

	// Subsequent claim returns nil — the previous one is still
	// claimed (claimed_at within staleClaimAge).
	row2, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row2 != nil {
		t.Errorf("expected nil on second claim while first still in-flight, got id=%d",
			row2.imageID)
	}
}

// TestClaimUncaptionedPrefersRecentOverInsertionOrder is the
// regression test for the head-of-line-blocking bug: a live
// @-mention screenshot occurring NOW must not get buried behind an
// older backfill image just because the backfill row was inserted
// (and therefore assigned its bigserial id) first.
//
// Seeds two events out of chronological insertion order: the OLD
// (backfill) event is created first, so it gets the smaller id; the
// NEW (recent) event is created second, so it gets the larger id.
// Both events' images are uncaptioned. An
// `ORDER BY ei.id` claim would return the OLD row (smaller id,
// inserted first); `ORDER BY ei.occurred_at DESC, ei.id`
// returns the NEW row instead.
func TestClaimUncaptionedPrefersRecentOverInsertionOrder(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()

	oldEventID := seedEventForWorkerAt(t, pool, "C1:1.0", time.Now().Add(-24*time.Hour))
	oldImageID := seedImage(t, pool, oldEventID, "F_OLD") // smaller id: inserted first

	newEventID := seedEventForWorkerAt(t, pool, "C1:2.0", time.Now())
	newImageID := seedImage(t, pool, newEventID, "F_NEW") // larger id: inserted second

	if oldImageID >= newImageID {
		t.Fatalf("test setup invariant broken: want oldImageID < newImageID, got %d, %d",
			oldImageID, newImageID)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("expected a row, got nil")
	}
	if row.imageID != newImageID {
		t.Errorf("claimUncaptioned claimed imageID=%d (source_image_id=%s); want the RECENT row imageID=%d (F_NEW) — "+
			"the OLDER backfill row (smaller id) must not jump the queue ahead of a live image",
			row.imageID, row.sourceImageID, newImageID)
	}
}

func TestClaimUncaptionedReturnsNilOnEmpty(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("expected nil on empty queue, got id=%d", row.imageID)
	}
}

func TestClaimUncaptionedConcurrentNoDoubleClaim(t *testing.T) {
	// Critical correctness property: with N goroutines all calling
	// claimUncaptioned in parallel against M rows, each row is
	// returned by exactly one goroutine. The FOR UPDATE SKIP LOCKED
	// inside the UPDATE is what guarantees this.
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	const numImages = 20
	for i := 0; i < numImages; i++ {
		seedImage(t, pool, eventID, "F"+string(rune('A'+i)))
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	const numWorkers = 4
	claimed := make(chan int64, numImages*2)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				row, err := w.claimUncaptioned(ctx)
				if err != nil {
					return
				}
				if row == nil {
					return
				}
				claimed <- row.imageID
				// Don't process; just claim so subsequent attempts
				// see this row as in-flight.
			}
		}()
	}
	wg.Wait()
	close(claimed)

	seen := map[int64]int{}
	for id := range claimed {
		seen[id]++
	}
	if len(seen) != numImages {
		t.Errorf("expected %d distinct rows claimed, got %d", numImages, len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("image %d claimed %d times (must be exactly 1)", id, count)
		}
	}
}

func TestClaimUncaptionedSkipsCaptioned(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET captioned_at = now() WHERE id = $1`,
		imageID); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("captioned_at-set row should not be claimable, got id=%d", row.imageID)
	}
}

func TestClaimUncaptionedSkipsPoisoned(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET failure_count = $2 WHERE id = $1`,
		imageID, events.MaxFailures); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("poison row (failure_count >= %d) should not be claimable, got id=%d",
			events.MaxFailures, row.imageID)
	}
}

func TestClaimUncaptionedRecoversStaleClaim(t *testing.T) {
	// A worker that died mid-LLM leaves claimed_at set + captioned_at
	// NULL. After staleClaimAge passes, another worker must be able
	// to re-claim the row. Without this, any pod crash would leave
	// rows permanently stuck.
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	stale := time.Now().Add(-(staleClaimAge + time.Minute))
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET claimed_at = $2 WHERE id = $1`,
		imageID, stale); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("stale claim should be re-claimable")
	}
	if row.imageID != imageID {
		t.Errorf("imageID: got %d, want %d", row.imageID, imageID)
	}
}

func TestClaimUncaptionedSkipsFreshClaim(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	// claimed_at well within staleClaimAge — another worker is
	// actively processing.
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET claimed_at = now() WHERE id = $1`,
		imageID); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("fresh-claim row should not be re-claimable, got id=%d", row.imageID)
	}
}

func TestRefreshClaimUpdatesClaimedAt(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	// Set claimed_at to an old value and build a row that reflects it, as
	// claimUncaptioned would have.
	old := time.Now().Add(-2 * time.Minute)
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET claimed_at = $2 WHERE id = $1`,
		imageID, old); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row := &claimRow{imageID: imageID, eventID: eventID, source: "slack", sourceImageID: "F1", claimedAt: old}
	if err := w.refreshClaim(ctx, row); err != nil {
		t.Fatal(err)
	}

	var newAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT claimed_at FROM event_images WHERE id = $1`,
		imageID).Scan(&newAt); err != nil {
		t.Fatal(err)
	}
	if !newAt.After(old.Add(time.Minute)) {
		t.Errorf("refreshClaim should advance claimed_at; got %v, was %v",
			newAt, old)
	}
	// row.claimedAt must track the refresh, so a later refresh or commit
	// fences against the current value, not the stale one it started with.
	if !row.claimedAt.Equal(newAt) {
		t.Errorf("refreshClaim should update row.claimedAt to the new value; got %v, want %v",
			row.claimedAt, newAt)
	}
}

// TestRefreshClaimRejectsAlreadySupersededClaim is the regression test for
// the variant alongside the main claim-fencing bug: refreshClaim must not
// blindly stamp claimed_at = now() over a claim another worker already
// stole (via stale-claim recovery). Without this guard, a worker retrying
// on a transient error after its claim went stale would resurrect a dead
// claim and make a later, superseded commit look current again — reopening
// the same lost-update window commitCaption's claimed_at check closes.
//
// Trigger: row.claimedAt (what this worker believes it holds) no longer
// matches the database's claimed_at (because a second worker's
// claimUncaptioned already re-claimed the row). Expected: refreshClaim
// reports errClaimSuperseded instead of refreshing, and leaves the row's
// claimed_at (the new owner's) untouched.
func TestRefreshClaimRejectsAlreadySupersededClaim(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	staleOriginal := time.Now().Add(-(staleClaimAge + time.Minute))
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET claimed_at = $2 WHERE id = $1`,
		imageID, staleOriginal); err != nil {
		t.Fatal(err)
	}

	// A second worker's normal stale-claim recovery reclaims the row,
	// stamping a fresh claimed_at that the first worker doesn't know about.
	other := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	otherRow, err := other.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if otherRow == nil || otherRow.imageID != imageID {
		t.Fatalf("expected the second worker to reclaim the stale row, got %+v", otherRow)
	}

	// The first worker, still believing it holds staleOriginal, tries to
	// refresh its (already-stolen) claim.
	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row := &claimRow{imageID: imageID, eventID: eventID, source: "slack", sourceImageID: "F1", claimedAt: staleOriginal}
	if err := w.refreshClaim(ctx, row); !errors.Is(err, errClaimSuperseded) {
		t.Fatalf("refreshClaim on a superseded claim = %v, want errClaimSuperseded", err)
	}

	var current time.Time
	if err := pool.QueryRow(ctx,
		`SELECT claimed_at FROM event_images WHERE id = $1`, imageID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if !current.Equal(otherRow.claimedAt) {
		t.Errorf("the superseded worker's refresh must not touch claimed_at; got %v, want the other worker's claim %v",
			current, otherRow.claimedAt)
	}
}

func TestBumpFailureRecordsCause(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	if err := w.bumpFailure(ctx, imageID, &fakeError{msg: "vertex empty (finishReason=SAFETY)"}); err != nil {
		t.Fatal(err)
	}

	var count int
	var msg *string
	if err := pool.QueryRow(ctx,
		`SELECT failure_count, last_failure FROM event_images WHERE id = $1`,
		imageID).Scan(&count, &msg); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("failure_count: got %d, want 1", count)
	}
	if msg == nil || *msg == "" {
		t.Error("last_failure should be set")
	}
}

// Every statement that records a failed attempt also records when, by the
// database's clock: the Failures page dates a failure by it.
func TestRecordedFailuresAreTimed(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")
	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}

	failedJustNow := func(step string) {
		t.Helper()
		var timed, justNow bool
		if err := pool.QueryRow(ctx, `
			SELECT last_failed_at IS NOT NULL,
			       COALESCE(last_failed_at BETWEEN now() - interval '1 minute' AND now(), false)
			  FROM event_images WHERE id = $1`, imageID).Scan(&timed, &justNow); err != nil {
			t.Fatal(err)
		}
		if !timed || !justNow {
			t.Errorf("%s: last_failed_at set=%v, within the last minute=%v", step, timed, justNow)
		}
	}
	var before *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_failed_at FROM event_images WHERE id = $1`, imageID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		t.Fatalf("an image that has not failed has no failure time, got %v", before)
	}

	if err := w.bumpFailure(ctx, imageID, &fakeError{msg: "vertex empty (finishReason=SAFETY)"}); err != nil {
		t.Fatal(err)
	}
	failedJustNow("a failed attempt")

	// An older failure's time is replaced by the newer one's.
	if _, err := pool.Exec(ctx, `UPDATE event_images SET last_failed_at = now() - interval '3 days' WHERE id = $1`, imageID); err != nil {
		t.Fatal(err)
	}
	if err := w.recordExhaustion(ctx, imageID, &TransientError{Underlying: &fakeError{msg: "status 503"}}); err != nil {
		t.Fatal(err)
	}
	failedJustNow("retries used up on a passing fault")

	if _, err := pool.Exec(ctx, `UPDATE event_images SET last_failed_at = now() - interval '3 days' WHERE id = $1`, imageID); err != nil {
		t.Fatal(err)
	}
	if err := w.bumpFailure(ctx, imageID, &PermanentError{Underlying: &fakeError{msg: "gone for good"}}); err != nil {
		t.Fatal(err)
	}
	failedJustNow("a failure that ends the attempts")
}

// usage of atomic just to keep concurrent test references stable
var _ = atomic.Int64{}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }
