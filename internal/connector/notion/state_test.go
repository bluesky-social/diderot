package notion

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

type recordingHider struct {
	mu  sync.Mutex
	ids []string
}

func (h *recordingHider) HideNotionTx(_ context.Context, _ pgx.Tx, externalID string) (func(context.Context), error) {
	return func(context.Context) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.ids = append(h.ids, externalID)
	}, nil
}

func (h *recordingHider) hidden(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, got := range h.ids {
		if got == id {
			return true
		}
	}
	return false
}

func TestPageStateStoreHidesPagesMissingFromCompletedRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	root := RootRef{Type: RootTypePage, ObjectID: "root-1"}
	if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'),
		       ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id)
		VALUES ('stale-page', now(), 'old-run')
	`); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('stale-page', $1, $2, 'old-run')
	`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		root: {{PageID: "current-page", LastEditedAt: time.Now(), HasContent: true}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if !hider.hidden("stale-page") {
		t.Fatal("stale page was not hidden")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT access_state FROM notion_page_state WHERE page_id = 'stale-page'`).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "removed" {
		t.Fatalf("access_state=%q, want removed", state)
	}
}

func TestPageStateStoreRetainsPageReachableFromAnotherRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	rootA := RootRef{Type: RootTypePage, ObjectID: "root-a"}
	rootB := RootRef{Type: RootTypePage, ObjectID: "root-b"}
	if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2), ($3, $4)`, rootA.Type, rootA.ObjectID, rootB.Type, rootB.ObjectID); err != nil {
		t.Fatalf("seed roots: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'), ('new-run', 'reconcile', now(), 'running');
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id)
		VALUES ('shared-page', now(), 'old-run')
	`); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('shared-page', $1, $2, 'old-run'), ('shared-page', $3, $4, 'old-run')
	`, rootA.Type, rootA.ObjectID, rootB.Type, rootB.ObjectID); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	if err := store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{rootA: {}}); err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if hider.hidden("shared-page") {
		t.Fatal("page reachable from root B was hidden")
	}
}

func TestPageStateStorePreservesTransientlyInaccessiblePage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	root := RootRef{Type: RootTypePage, ObjectID: "root-1"}
	if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'),
		       ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state
		       (page_id, last_seen_at, last_seen_run_id, access_state, last_error)
		VALUES ('live-page', now(), 'old-run', 'active', '')
	`); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('live-page', $1, $2, 'old-run')
	`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		root: {{
			PageID: "live-page", TransientlyInaccessible: true,
			LastError: "page markdown: forbidden",
		}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if hider.hidden("live-page") {
		t.Fatal("transiently inaccessible page was hidden")
	}
	var accessState, lastError, membershipRun string
	if err := pool.QueryRow(ctx, `
		SELECT state.access_state, state.last_error, membership.last_seen_run_id
		  FROM notion_page_state state
		  JOIN notion_page_roots membership USING (page_id)
		 WHERE state.page_id = 'live-page'
	`).Scan(&accessState, &lastError, &membershipRun); err != nil {
		t.Fatalf("read preserved page: %v", err)
	}
	if accessState != "active" || lastError == "" || membershipRun != "new-run" {
		t.Fatalf("access=%q error=%q membership_run=%q", accessState, lastError, membershipRun)
	}
}

func TestPageStateStoreTombstonesInaccessiblePage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	root := RootRef{Type: RootTypePage, ObjectID: "root-1"}
	if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'), ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ('private-page', now(), 'old-run', 'active')
	`); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('private-page', $1, $2, 'old-run')
	`, root.Type, root.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		root: {{PageID: "private-page", Inaccessible: true, LastError: "notion returned 404"}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if !hider.hidden("private-page") {
		t.Fatal("inaccessible page was not tombstoned")
	}
	var accessState, lastError, membershipRun string
	if err := pool.QueryRow(ctx, `
		SELECT state.access_state, state.last_error, membership.last_seen_run_id
		  FROM notion_page_state state
		  JOIN notion_page_roots membership USING (page_id)
		 WHERE state.page_id = 'private-page'
	`).Scan(&accessState, &lastError, &membershipRun); err != nil {
		t.Fatalf("read inaccessible page: %v", err)
	}
	if accessState != "inaccessible" || lastError == "" || membershipRun != "new-run" {
		t.Fatalf("access=%q error=%q membership_run=%q", accessState, lastError, membershipRun)
	}
}

func TestPageStateStoreReadableRootWinsOverInaccessibleRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	readableRoot := RootRef{Type: RootTypePage, ObjectID: "readable-root"}
	inaccessibleRoot := RootRef{Type: RootTypePage, ObjectID: "inaccessible-root"}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_roots (object_type, object_id)
		VALUES ($1, $2), ($3, $4)
	`, readableRoot.Type, readableRoot.ObjectID, inaccessibleRoot.Type, inaccessibleRoot.ObjectID); err != nil {
		t.Fatalf("seed roots: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		readableRoot: {{PageID: "shared-page", HasContent: true}},
		inaccessibleRoot: {{
			PageID: "shared-page", Inaccessible: true, LastError: "notion returned 404",
		}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if hider.hidden("shared-page") {
		t.Fatal("page readable through another root was tombstoned")
	}
	var accessState, lastError string
	var memberships int
	if err := pool.QueryRow(ctx, `
		SELECT state.access_state, state.last_error, count(membership.page_id)
		  FROM notion_page_state state
		  JOIN notion_page_roots membership USING (page_id)
		 WHERE state.page_id = 'shared-page'
		 GROUP BY state.access_state, state.last_error
	`).Scan(&accessState, &lastError, &memberships); err != nil {
		t.Fatalf("read shared page: %v", err)
	}
	if accessState != "active" || lastError != "" || memberships != 2 {
		t.Fatalf("access=%q error=%q memberships=%d", accessState, lastError, memberships)
	}
}

// TestPageStateStoreHonorsSurvivingRootClaimWhenOwningRootRemovedConcurrently
// is a deterministic regression test for a bug in ApplySuccessfulRoots.
//
// Trigger: a root (rootA) that was the only thing linking a page to the
// corpus gets removed (an admin's RootsStore.Remove, modeled here by
// deleting its notion_roots row, which cascades the FK and deletes rootA's
// notion_page_roots link) while rootA's own in-flight walk report and a
// second, still-configured root's (rootB) fresh report of the SAME page
// both land in one ApplySuccessfulRoots call — exactly how runReconcile
// batches every root's report from one tick.
//
// Contract (PageStateStore doc comment, state.go): a page readable through
// a currently-configured root must not be hidden, and the function must not
// commit self-contradictory state (a page cannot be both "active and
// linked" and "deleted from the corpus" in the same transaction).
//
// Bug: ApplySuccessfulRoots used to decide whether a removed root's pages
// were still reachable by checking notion_page_roots BEFORE writing this
// run's fresh links for surviving roots (rootB's link doesn't exist yet at
// that point), so it hid the page and then, moments later in the same
// transaction, recorded it as active and linked to rootB — a corpus event
// silently deleted underneath state that says the page is live.
func TestPageStateStoreHonorsSurvivingRootClaimWhenOwningRootRemovedConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	rootA := RootRef{Type: RootTypePage, ObjectID: "root-a"} // removed concurrently
	rootB := RootRef{Type: RootTypePage, ObjectID: "root-b"} // stays configured, newly reaches the page this run
	for _, r := range []RootRef{rootA, rootB} {
		if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, r.Type, r.ObjectID); err != nil {
			t.Fatalf("seed root %v: %v", r, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'),
		       ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ('moved-page', now(), 'old-run', 'active')
	`); err != nil {
		t.Fatalf("seed page state: %v", err)
	}
	// Historically, moved-page was reachable only through rootA.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('moved-page', $1, $2, 'old-run')
	`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Concurrent admin action: removing rootA (its own, already-committed
	// transaction) cascades away rootA's notion_page_roots link. This
	// models RootsStore.Remove's FK cascade directly so the test exercises
	// only ApplySuccessfulRoots.
	if _, err := pool.Exec(ctx, `DELETE FROM notion_roots WHERE object_type = $1 AND object_id = $2`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("simulate concurrent root removal: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)

	// Both walks land in the same ApplySuccessfulRoots call: rootA's report
	// is whatever it discovered before being torn down; rootB's report is a
	// fresh, successful, content-bearing render of the SAME page.
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		rootA: {{PageID: "moved-page", LastEditedAt: time.Now(), Inaccessible: true, LastError: "403 forbidden"}},
		rootB: {{PageID: "moved-page", LastEditedAt: time.Now(), HasContent: true}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}

	if hider.hidden("moved-page") {
		t.Fatal("moved-page's event was hidden even though rootB's fresh report, processed in the same transaction, proves it alive and content-bearing right now")
	}
	var accessState string
	if err := pool.QueryRow(ctx, `SELECT access_state FROM notion_page_state WHERE page_id = 'moved-page'`).Scan(&accessState); err != nil {
		t.Fatalf("read access_state: %v", err)
	}
	if accessState != "active" {
		t.Fatalf("access_state=%q, want active", accessState)
	}
	var linkedToB bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM notion_page_roots WHERE page_id = 'moved-page' AND root_object_type = $1 AND root_object_id = $2)
	`, rootB.Type, rootB.ObjectID).Scan(&linkedToB); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if !linkedToB {
		t.Fatal("moved-page is not linked to rootB")
	}
}

// TestPageStateStoreRequestsReplayWhenPageWasHiddenByConcurrentRootRemoval is
// a deterministic regression test for the same root cause as the test
// above, but reached through RootsStore.Remove's own orphan check
// (roots.go) instead of ApplySuccessfulRoots's removed-root branch: Remove
// commits in its own, independent transaction and can tombstone a page
// whose only persisted link is the root being removed, even though a
// second root's walk — already in flight, running concurrently for
// minutes or hours — is about to prove the same page reachable. Reordering
// ApplySuccessfulRoots (the fix for the test above) cannot fix this one:
// the page is already tombstoned by the time ApplySuccessfulRoots runs.
//
// Trigger: RootsStore.Remove(rootA) runs to completion — tombstoning
// moved-page and marking it access_state='removed' because, at that
// moment, no other root claims it — followed by an ApplySuccessfulRoots
// call reporting the same page as freshly readable under rootB.
//
// Expected: since ApplySuccessfulRoots cannot recreate the tombstoned
// event's content itself, it must request an immediate replay
// (notion_sync.backfill_requested_at) instead of silently leaving
// access_state="active" with no corresponding event until the next
// scheduled (default 24h) reconcile.
func TestPageStateStoreRequestsReplayWhenPageWasHiddenByConcurrentRootRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	const externalID = "moved-page-replay"
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'notion' AND external_id = $1`, externalID); err != nil {
		t.Fatalf("clean events: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notion_sync SET backfill_requested_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset backfill_requested_at: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE notion_sync SET backfill_requested_at = NULL WHERE id = 1`)
	})

	// RootsStore.Remove parses its input through ParseRootReference, which
	// requires a UUID shape, so these roots (unlike the DELETE-cascade test
	// above) need real UUIDs rather than readable labels.
	rootA := RootRef{Type: RootTypePage, ObjectID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"} // removed concurrently
	rootB := RootRef{Type: RootTypePage, ObjectID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"} // stays configured, newly reaches the page this run
	for _, r := range []RootRef{rootA, rootB} {
		if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, r.Type, r.ObjectID); err != nil {
			t.Fatalf("seed root %v: %v", r, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete')
	`); err != nil {
		t.Fatalf("seed old run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ($1, now(), 'old-run', 'active')
	`, externalID); err != nil {
		t.Fatalf("seed page state: %v", err)
	}
	// Historically, moved-page was reachable only through rootA.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ($1, $2, $3, 'old-run')
	`, externalID, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// A real, previously-ingested event: this is what RootsStore.Remove's
	// hider actually tombstones. A fake recording hider can't exercise the
	// new check, which reads the events table's own "deleted" flag rather
	// than the page's prior access_state.
	adapter := NewEventSinkAdapter(events.NewDBSink(pool, prometheus.NewRegistry()), pool)
	if _, err := adapter.IngestNotion(ctx, IngestEvent{
		ExternalID: externalID, OccurredAt: time.Now(), Title: "Moved page", Body: "moved page body",
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	roots := NewRootsStore(pool)
	roots.SetHider(adapter)
	removed, err := roots.Remove(ctx, "page:"+rootA.ObjectID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove reported no row deleted")
	}
	var accessState string
	if err := pool.QueryRow(ctx, `SELECT access_state FROM notion_page_state WHERE page_id = $1`, externalID).Scan(&accessState); err != nil {
		t.Fatalf("read access_state after Remove: %v", err)
	}
	if accessState != "removed" {
		t.Fatalf("access_state after Remove=%q, want removed", accessState)
	}
	var deletedAfterRemove bool
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE source = 'notion' AND external_id = $1`, externalID).Scan(&deletedAfterRemove); err != nil {
		t.Fatalf("read deleted after Remove: %v", err)
	}
	if !deletedAfterRemove {
		t.Fatal("Remove should have tombstoned moved-page's event: it was, at that moment, orphaned")
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	store := NewPageStateStore(pool, adapter)
	// rootB's walk, already in flight before Remove ran, completes and
	// reports moved-page as freshly readable. Unlike the sequential re-add
	// case below, rootB's walk has NOT re-ingested the page by the time
	// Apply runs — the whole point of this race is that Remove's tombstone
	// lands in between the walk finishing discovery and the ingest that
	// would normally restore it — so the event is still tombstoned here.
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		rootB: {{PageID: externalID, LastEditedAt: time.Now(), HasContent: true}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}

	var backfillRequestedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT backfill_requested_at FROM notion_sync WHERE id = 1`).Scan(&backfillRequestedAt); err != nil {
		t.Fatalf("read backfill_requested_at: %v", err)
	}
	if backfillRequestedAt == nil {
		t.Fatal("BUG: ApplySuccessfulRoots revived moved-page (access_state=active, linked to rootB) without requesting a replay, even though its event is still tombstoned by the concurrent Remove and nothing else will re-ingest its content before the next scheduled reconcile")
	}
}

// TestPageStateStoreNoReplayRequestedOnSequentialReAdd is the negative
// counterpart: a root is removed (tombstoning its pages and marking them
// access_state='removed'), and later a different root is added that covers
// the same pages and walks them BEFORE this run's ApplySuccessfulRoots call
// — the ordinary, non-race sequence, exactly like
// TestPageStateStoreRequestsReplayWhenPageWasHiddenByConcurrentRootRemoval
// except the walk's ingest (ingestOne upserts unconditionally, restoring
// the event) completes first. ApplySuccessfulRoots must not request a
// replay here: the event is already live, and the prior check — which
// keyed off access_state=='removed' rather than the event's own tombstone
// flag — fired on this transition for nothing, scheduling a needless full
// re-walk of every configured root on every ordinary remove-then-re-cover.
func TestPageStateStoreNoReplayRequestedOnSequentialReAdd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	const externalID = "moved-page-sequential"
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'notion' AND external_id = $1`, externalID); err != nil {
		t.Fatalf("clean events: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notion_sync SET backfill_requested_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset backfill_requested_at: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE notion_sync SET backfill_requested_at = NULL WHERE id = 1`)
	})

	rootA := RootRef{Type: RootTypePage, ObjectID: "cccccccc-cccc-cccc-cccc-cccccccccccc"}
	rootB := RootRef{Type: RootTypePage, ObjectID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	for _, r := range []RootRef{rootA, rootB} {
		if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, r.Type, r.ObjectID); err != nil {
			t.Fatalf("seed root %v: %v", r, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete')
	`); err != nil {
		t.Fatalf("seed old run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ($1, now(), 'old-run', 'active')
	`, externalID); err != nil {
		t.Fatalf("seed page state: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ($1, $2, $3, 'old-run')
	`, externalID, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	adapter := NewEventSinkAdapter(events.NewDBSink(pool, prometheus.NewRegistry()), pool)
	if _, err := adapter.IngestNotion(ctx, IngestEvent{
		ExternalID: externalID, OccurredAt: time.Now(), Title: "Moved page", Body: "moved page body",
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	roots := NewRootsStore(pool)
	roots.SetHider(adapter)
	if _, err := roots.Remove(ctx, "page:"+rootA.ObjectID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var accessState string
	if err := pool.QueryRow(ctx, `SELECT access_state FROM notion_page_state WHERE page_id = $1`, externalID).Scan(&accessState); err != nil {
		t.Fatalf("read access_state after Remove: %v", err)
	}
	if accessState != "removed" {
		t.Fatalf("access_state after Remove=%q, want removed", accessState)
	}

	// rootB's walk reaches the page and ingests it — exactly like the real
	// walker's ingestOne, which upserts unconditionally — BEFORE this run's
	// ApplySuccessfulRoots call, restoring the event.
	if _, err := adapter.IngestNotion(ctx, IngestEvent{
		ExternalID: externalID, OccurredAt: time.Now(), Title: "Moved page", Body: "moved page body, re-read under rootB",
	}); err != nil {
		t.Fatalf("re-ingest event: %v", err)
	}
	var deletedAfterReingest bool
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE source = 'notion' AND external_id = $1`, externalID).Scan(&deletedAfterReingest); err != nil {
		t.Fatalf("read deleted after re-ingest: %v", err)
	}
	if deletedAfterReingest {
		t.Fatal("re-ingest should have un-tombstoned the event")
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	store := NewPageStateStore(pool, adapter)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		rootB: {{PageID: externalID, LastEditedAt: time.Now(), HasContent: true}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}

	var backfillRequestedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT backfill_requested_at FROM notion_sync WHERE id = 1`).Scan(&backfillRequestedAt); err != nil {
		t.Fatalf("read backfill_requested_at: %v", err)
	}
	if backfillRequestedAt != nil {
		t.Fatal("BUG: ApplySuccessfulRoots requested a replay for a page whose event rootB's walk had already restored — this schedules a needless full re-walk of every root on every ordinary remove-then-re-cover transition")
	}
}

// TestPageStateStorePreservesTransientlyInaccessiblePageWhenOwningRootRemovedConcurrently
// is a sibling of TestPageStateStoreHonorsSurvivingRootClaimWhenOwningRootRemovedConcurrently
// for the same root cause (ApplySuccessfulRoots used to judge a removed
// root's pages before this run's surviving-root links were written), but
// with the surviving root reporting the page TransientlyInaccessible
// instead of fully readable.
//
// Contract: the walker's "preserve last-known-good event" rule (state.go,
// the TransientlyInaccessible branch) must hold even when the page's only
// other claim came from a root removed this same tick: rootB's structural
// re-discovery of the page is still a live, current-tick membership link,
// and a transient render failure on rootB's side must not let the
// removed-root branch read that link as absent and hide the page.
func TestPageStateStorePreservesTransientlyInaccessiblePageWhenOwningRootRemovedConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	rootA := RootRef{Type: RootTypePage, ObjectID: "root-a"} // removed concurrently
	rootB := RootRef{Type: RootTypePage, ObjectID: "root-b"} // stays configured, newly reaches the page this run
	for _, r := range []RootRef{rootA, rootB} {
		if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, r.Type, r.ObjectID); err != nil {
			t.Fatalf("seed root %v: %v", r, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'),
		       ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ('rate-limited-page', now(), 'old-run', 'active')
	`); err != nil {
		t.Fatalf("seed page state: %v", err)
	}
	// Historically, rate-limited-page was reachable only through rootA.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('rate-limited-page', $1, $2, 'old-run')
	`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notion_roots WHERE object_type = $1 AND object_id = $2`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("simulate concurrent root removal: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		rootA: {{PageID: "rate-limited-page", LastEditedAt: time.Now(), Inaccessible: true, LastError: "403 forbidden"}},
		rootB: {{
			PageID: "rate-limited-page", TransientlyInaccessible: true,
			LastError: "page markdown: 429 rate limited",
		}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}

	if hider.hidden("rate-limited-page") {
		t.Fatal("BUG: rate-limited-page was hidden even though rootB structurally re-proved it this run; a transient render failure must preserve the last-known-good event, not delete it")
	}
	var linkedToB bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM notion_page_roots WHERE page_id = 'rate-limited-page' AND root_object_type = $1 AND root_object_id = $2)
	`, rootB.Type, rootB.ObjectID).Scan(&linkedToB); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if !linkedToB {
		t.Fatal("rate-limited-page is not linked to rootB")
	}
}

// TestPageStateStoreMarksTrulyOrphanedPageRemovedAfterRootRemoval confirms
// that a page genuinely left with no claim after a root removal is still
// hidden and, because the removed-root branch updates
// notion_page_state as well as calling HideNotionTx, ends up recorded
// access_state='removed', consistent with every other hiding path in this
// function (the sweep loop at the bottom of ApplySuccessfulRoots).
func TestPageStateStoreMarksTrulyOrphanedPageRemovedAfterRootRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	rootA := RootRef{Type: RootTypePage, ObjectID: "root-a"} // removed concurrently; no other root claims its page
	if _, err := pool.Exec(ctx, `INSERT INTO notion_roots (object_type, object_id) VALUES ($1, $2)`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('old-run', 'reconcile', now(), 'complete'),
		       ('new-run', 'reconcile', now(), 'running')
	`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id, access_state)
		VALUES ('orphaned-page', now(), 'old-run', 'active')
	`); err != nil {
		t.Fatalf("seed page state: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('orphaned-page', $1, $2, 'old-run')
	`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notion_roots WHERE object_type = $1 AND object_id = $2`, rootA.Type, rootA.ObjectID); err != nil {
		t.Fatalf("simulate concurrent root removal: %v", err)
	}

	hider := &recordingHider{}
	store := NewPageStateStore(pool, hider)
	// rootA is the only root in this tick's walked map, and no surviving
	// root claims orphaned-page, so it must still be hidden.
	err = store.ApplySuccessfulRoots(ctx, "new-run", map[RootRef][]PageSnapshot{
		rootA: {{PageID: "orphaned-page", LastEditedAt: time.Now(), Inaccessible: true, LastError: "403 forbidden"}},
	})
	if err != nil {
		t.Fatalf("ApplySuccessfulRoots: %v", err)
	}
	if !hider.hidden("orphaned-page") {
		t.Fatal("orphaned-page should have been hidden: no root claims it after rootA's removal")
	}
	var accessState string
	if err := pool.QueryRow(ctx, `SELECT access_state FROM notion_page_state WHERE page_id = 'orphaned-page'`).Scan(&accessState); err != nil {
		t.Fatalf("read access_state: %v", err)
	}
	if accessState != "removed" {
		t.Fatalf("access_state=%q, want removed", accessState)
	}
}
