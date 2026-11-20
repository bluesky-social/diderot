package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// positionFeed serves nothing new and records every position it is asked
// for. onCall, when set, runs before each request is answered and may
// return a page to serve.
type positionFeed struct {
	afters []int64
	onCall func(call int, after int64) *ChangesPage
}

func (f *positionFeed) ListSessionChanges(_ context.Context, after int64, _ int) (*ChangesPage, error) {
	f.afters = append(f.afters, after)
	if f.onCall != nil {
		if page := f.onCall(len(f.afters), after); page != nil {
			return page, nil
		}
	}
	return &ChangesPage{NextCursor: after}, nil
}

func (f *positionFeed) GetSessionRevisionBytes(context.Context, string, string, int64) ([]byte, error) {
	return nil, errors.New("no revision")
}

func (f *positionFeed) requireNeverBelow(t *testing.T, position int64) {
	t.Helper()
	if len(f.afters) == 0 {
		t.Fatal("the feed was never read")
	}
	for _, after := range f.afters {
		if after < position {
			t.Fatalf("the feed was read from %d, below the stored position %d: requests %v", after, position, f.afters)
		}
	}
}

func setStoredPolicyHash(t *testing.T, pool *pgxpool.Pool, hash string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE session_ingest_cursor SET policy_hash = $1`, hash); err != nil {
		t.Fatal(err)
	}
}

func policyWorker(store *Store, feed Feed, policy Policy) *Worker {
	return &Worker{
		Cfg:   WorkerConfig{Policy: policy},
		Store: store, Feed: feed, Metrics: NewMetrics(nil),
	}
}

func decideWith(policy Policy) func(SessionChange) ChangePolicy {
	return (&Worker{Cfg: WorkerConfig{Policy: policy}}).changePolicy
}

func seedUnder(t *testing.T, store *Store, policy Policy, changes ...SessionChange) {
	t.Helper()
	var next int64
	for _, ch := range changes {
		next = max(next, ch.ChangeSeq)
	}
	if _, err := store.upsertScopedAt(context.Background(), changes, next, decideWith(policy)); err != nil {
		t.Fatal(err)
	}
}

func mustCandidate(t *testing.T, store *Store, ch SessionChange) Candidate {
	t.Helper()
	c, err := store.LoadCandidate(context.Background(), Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A release that changes what the policy hash covers leaves every install
// with a stored hash its policy no longer produces. The decisions are the
// same, so nothing may be read again and nothing written.
func TestTick_AHashThatChangedWithoutThePolicyReadsAndWritesNothing(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	policy := DefaultPolicy()
	admitted := change(40, "s-admitted", "a@x", "codex")
	excluded := change(41, "s-excluded", "b@x", "codex")
	excluded.OriginApp = "diderot"
	deleted := change(42, "s-deleted", "c@x", "codex")
	deleted.Deleted = true
	seedUnder(t, store, policy, admitted, excluded, deleted)
	// Settle the removals the seed queued, as the worker would have.
	if _, err := pool.Exec(ctx, `UPDATE session_ingest_candidates SET corpus_tombstone_pending = NULL`); err != nil {
		t.Fatal(err)
	}
	setStoredPolicyHash(t, pool, "sha256:the-same-policy-hashed-by-an-older-release")
	var before string
	snapshot := `SELECT md5(string_agg(c::text, '|' ORDER BY session_id)) FROM session_ingest_candidates c`
	if err := pool.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}

	feed := &positionFeed{}
	if err := policyWorker(store, feed, policy).Tick(ctx); err != nil {
		t.Fatal(err)
	}

	feed.requireNeverBelow(t, 42)
	cursor, _ := store.GetCursor(ctx)
	if cursor.ChangeSeq != 42 {
		t.Fatalf("cursor = %d, want it left at 42", cursor.ChangeSeq)
	}
	if cursor.PolicyHash != policy.Hash() {
		t.Fatalf("stored hash = %q, want the current policy's", cursor.PolicyHash)
	}
	var after string
	if err := pool.QueryRow(ctx, snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("candidates were rewritten although no decision changed")
	}

	// The hash now matches, so the next tick does not check again.
	feed.afters = nil
	setStoredPolicyHash(t, pool, policy.Hash())
	if _, err := pool.Exec(ctx, `UPDATE session_ingest_candidates SET excluded_at = now() WHERE session_id = 's-admitted'`); err != nil {
		t.Fatal(err)
	}
	if err := policyWorker(store, feed, policy).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if mustCandidate(t, store, admitted).ExcludedAt == nil {
		t.Fatal("a matching hash still re-checked the candidates")
	}
}

// A real policy change is applied to the sessions already seen: the cursor
// stays where it is and the feed is never asked for anything older.
func TestTick_PolicyChangeAppliesToSessionsAlreadySeenWithoutRereadingTheFeed(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	old := DefaultPolicy()
	old.Exclude.Clients = []string{"pi"}

	stays := change(1, "s-stays", "a@x", "codex")
	dropped := change(2, "s-dropped", "a@x", "codex")
	dropped.OriginApp = "scratch-app"
	returns := change(3, "s-returns", "b@x", "pi")
	private := change(4, "s-private", "c@x", "codex")
	deleted := change(5, "s-deleted", "d@x", "pi")
	deleted.Deleted = true
	seedUnder(t, store, old, stays, dropped, returns, private, deleted)
	if _, err := store.PublishRevision(ctx, dropped.identity(), dropped.Revision, dropped.RewriteEpoch, "sha256:d"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('agent_session', $1, now(), 'summary', '{"event_kind":"native_session"}', 'internal')`,
		externalID(private.identity())); err != nil {
		t.Fatal(err)
	}
	setStoredPolicyHash(t, pool, old.Hash())

	changed := DefaultPolicy()
	changed.Exclude.Apps = append(changed.Exclude.Apps, "scratch-app")
	changed.OwnerOnly.Principals = []string{"c@x"}

	feed := &positionFeed{}
	if err := policyWorker(store, feed, changed).Tick(ctx); err != nil {
		t.Fatal(err)
	}

	feed.requireNeverBelow(t, 5)
	cursor, _ := store.GetCursor(ctx)
	if cursor.ChangeSeq != 5 || cursor.PolicyHash != changed.Hash() {
		t.Fatalf("cursor = %+v, want position 5 and the new policy's hash", cursor)
	}
	if c := mustCandidate(t, store, stays); c.ExcludedAt != nil || c.VisibilityScope != events.ScopeInternal {
		t.Fatalf("unchanged session = %+v", c)
	}
	if c := mustCandidate(t, store, dropped); c.ExcludedAt == nil {
		t.Fatal("newly excluded session is still active")
	}
	var tombstoned, removalQueued bool
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT bool_and(tombstoned_at IS NOT NULL) FROM session_ingest_published WHERE session_id = 's-dropped'),
		       (SELECT corpus_tombstone_pending IS NOT NULL FROM session_ingest_candidates WHERE session_id = 's-dropped')`,
	).Scan(&tombstoned, &removalQueued); err != nil {
		t.Fatal(err)
	}
	if !tombstoned || !removalQueued {
		t.Fatalf("newly excluded session: ledger tombstoned=%v, corpus removal queued=%v", tombstoned, removalQueued)
	}
	if c := mustCandidate(t, store, returns); c.ExcludedAt != nil || c.FirstPendingAt == nil {
		t.Fatalf("re-admitted session = %+v, want active and waiting for a summary", c)
	}
	if c := mustCandidate(t, store, private); c.VisibilityScope != events.ScopeOwner("c@x") {
		t.Fatalf("owner-only session scope = %q", c.VisibilityScope)
	}
	var eventScope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE external_id = $1`,
		externalID(private.identity())).Scan(&eventScope); err != nil {
		t.Fatal(err)
	}
	if eventScope != events.ScopeOwner("c@x") {
		t.Fatalf("owner-only session's event scope = %q", eventScope)
	}
	if c := mustCandidate(t, store, deleted); c.ExcludedAt == nil || !c.Deleted {
		t.Fatalf("deleted session = %+v, want it left deleted", c)
	}
}

func (ch SessionChange) identity() Identity {
	return Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID}
}

func TestReapplyPolicy_PagesInKeyOrderAndIsSafeToRepeat(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	var seeded []SessionChange
	for i := range 5 {
		seeded = append(seeded, change(int64(i+1), fmt.Sprintf("s%d", i), fmt.Sprintf("o%d@x", i), "codex"))
	}
	seedUnder(t, store, DefaultPolicy(), seeded...)
	excludeOdd := func(ch SessionChange) ChangePolicy {
		return ChangePolicy{
			Admitted:        ch.SessionID != "s1" && ch.SessionID != "s3",
			VisibilityScope: events.ScopeInternal,
		}
	}

	run := func() (checked, changed, batches int) {
		var after Identity
		for {
			batch, err := store.ReapplyPolicy(ctx, after, 2, excludeOdd)
			if err != nil {
				t.Fatal(err)
			}
			batches++
			checked += batch.Checked
			changed += batch.Changed
			if batch.Done {
				return checked, changed, batches
			}
			after = batch.Next
		}
	}
	if checked, changed, batches := run(); checked != 5 || changed != 2 || batches != 3 {
		t.Fatalf("first pass checked %d, changed %d in %d batches; want 5, 2 in 3", checked, changed, batches)
	}
	for i, ch := range seeded {
		if excluded := mustCandidate(t, store, ch).ExcludedAt != nil; excluded != (i%2 == 1) {
			t.Fatalf("%s excluded = %v", ch.SessionID, excluded)
		}
	}
	// An interrupted pass starts over; what it already did is left alone.
	if checked, changed, _ := run(); checked != 5 || changed != 0 {
		t.Fatalf("repeated pass checked %d, changed %d; want 5, 0", checked, changed)
	}
}

func TestReapplyPolicy_LeavesAnOwnersChoiceAndPushedSessionsAlone(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	chosen := change(1, "s-chosen", "a@x", "codex")
	pushed := change(2, "s-pushed", "b@x", "codex")
	seedUnder(t, store, DefaultPolicy(), chosen, pushed)
	if _, err := pool.Exec(ctx, `
		UPDATE session_ingest_candidates SET privacy_owner_only = false WHERE session_id = 's-chosen';
		UPDATE session_ingest_bindings SET writer = 'push' WHERE session_id = 's-pushed'`); err != nil {
		t.Fatal(err)
	}
	ownerOnlyOrExcluded := func(ch SessionChange) ChangePolicy {
		if ch.SessionID == "s-pushed" {
			return ChangePolicy{}
		}
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeOwner(CanonicalOwner(ch.Owner))}
	}

	batch, err := store.ReapplyPolicy(ctx, Identity{}, 100, ownerOnlyOrExcluded)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Checked != 1 || batch.Changed != 0 || !batch.Done {
		t.Fatalf("batch = %+v, want the one feed session checked and nothing changed", batch)
	}
	if c := mustCandidate(t, store, chosen); c.VisibilityScope != events.ScopeInternal {
		t.Fatalf("the owner chose internal; scope = %q", c.VisibilityScope)
	}
	if c := mustCandidate(t, store, pushed); c.ExcludedAt != nil {
		t.Fatal("a pushed session was excluded by the feed's policy")
	}
}

// Applying a policy to many sessions must not hold back new ones: the feed
// is read again between batches.
func TestTick_NewSessionsArriveWhileAPolicyIsApplied(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, client, session_id, owner_kind, role, origin_app, change_seq, owner_canonical)
		SELECT 'o@x', 'codex', 'seen-' || lpad(n::text, 5, '0'), 'human', 'primary', 'codex', n, 'o@x'
		  FROM generate_series(1, $1) n;
		`, policyBatchSize+1); err != nil {
		t.Fatal(err)
	}
	position := int64(policyBatchSize + 1)
	if _, err := pool.Exec(ctx, `UPDATE session_ingest_cursor SET change_seq = $1, policy_hash = 'sha256:old'`, position); err != nil {
		t.Fatal(err)
	}
	arrival := change(position+1, "s-new", "n@x", "codex")
	var checkedWhenItArrived int
	feed := &positionFeed{onCall: func(call int, after int64) *ChangesPage {
		if call != 2 {
			return nil
		}
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_candidates WHERE excluded_at IS NOT NULL`).Scan(&checkedWhenItArrived)
		return &ChangesPage{Changes: []SessionChange{arrival}, NextCursor: arrival.ChangeSeq}
	}}
	policy := DefaultPolicy()
	policy.Exclude.Principals = []string{"o@x"}

	if err := policyWorker(store, feed, policy).Tick(ctx); err != nil {
		t.Fatal(err)
	}

	feed.requireNeverBelow(t, position)
	if checkedWhenItArrived != policyBatchSize {
		t.Fatalf("the feed was read again after %d sessions were re-decided, want after the first batch of %d",
			checkedWhenItArrived, policyBatchSize)
	}
	if c := mustCandidate(t, store, arrival); c.ExcludedAt != nil {
		t.Fatalf("new session = %+v, want admitted", c)
	}
	var excluded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_candidates WHERE excluded_at IS NOT NULL`).Scan(&excluded); err != nil {
		t.Fatal(err)
	}
	cursor, _ := store.GetCursor(ctx)
	if excluded != policyBatchSize+1 || cursor.ChangeSeq != arrival.ChangeSeq || cursor.PolicyHash != policy.Hash() {
		t.Fatalf("excluded %d of %d, cursor %+v", excluded, policyBatchSize+1, cursor)
	}
}

func TestUpsertChangesScopedAt_DiscardsAPageReadFromAnotherPosition(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE session_ingest_cursor SET change_seq = 900`); err != nil {
		t.Fatal(err)
	}
	page := []SessionChange{change(8, "s-stale", "a@x", "codex")}

	_, _, err := store.UpsertChangesScopedAt(ctx, page, 7, 8, decideWith(DefaultPolicy()))

	if !errors.Is(err, ErrCursorMoved) {
		t.Fatalf("err = %v, want ErrCursorMoved", err)
	}
	cursor, _ := store.GetCursor(ctx)
	var candidates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_candidates`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if cursor.ChangeSeq != 900 || candidates != 0 {
		t.Fatalf("cursor = %d with %d candidates, want 900 and none saved", cursor.ChangeSeq, candidates)
	}
}

// An operator corrects the position while a drain is paging from memory.
// The drain stops there and the next tick resumes from the correction.
func TestTick_ACursorCorrectedDuringADrainIsKept(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	policy := DefaultPolicy()
	setStoredPolicyHash(t, pool, policy.Hash())
	feed := &positionFeed{onCall: func(call int, after int64) *ChangesPage {
		switch call {
		case 1:
			return &ChangesPage{Changes: []SessionChange{change(1, "s1", "a@x", "codex")}, NextCursor: 1}
		case 2:
			if _, err := pool.Exec(ctx, `UPDATE session_ingest_cursor SET change_seq = 5000`); err != nil {
				t.Error(err)
			}
			return &ChangesPage{Changes: []SessionChange{change(2, "s2", "a@x", "codex")}, NextCursor: 2}
		}
		return nil
	}}
	w := policyWorker(store, feed, policy)

	if err := w.Tick(ctx); !errors.Is(err, ErrCursorMoved) {
		t.Fatalf("tick err = %v, want ErrCursorMoved", err)
	}
	if cursor, _ := store.GetCursor(ctx); cursor.ChangeSeq != 5000 {
		t.Fatalf("cursor = %d, want the corrected 5000", cursor.ChangeSeq)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if last := feed.afters[len(feed.afters)-1]; last != 5000 {
		t.Fatalf("the next tick read from %d, want 5000", last)
	}
}

func TestRecordAppliedPolicy_LeavesThePositionAlone(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE session_ingest_cursor SET change_seq = 42, updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAppliedPolicy(ctx, "sha256:pol"); err != nil {
		t.Fatal(err)
	}
	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ChangeSeq != 42 || cursor.PolicyHash != "sha256:pol" {
		t.Fatalf("cursor = %+v", cursor)
	}
	var age time.Duration
	if err := pool.QueryRow(ctx, `SELECT now() - updated_at FROM session_ingest_cursor`).Scan(&age); err != nil {
		t.Fatal(err)
	}
	if age > time.Minute {
		t.Fatalf("updated_at is %s old", age)
	}
}
