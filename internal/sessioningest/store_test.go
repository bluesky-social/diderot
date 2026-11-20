package sessioningest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "sessioningest"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		TRUNCATE session_ingest_candidates, session_ingest_published, session_ingest_map_checkpoints,
		         session_ingest_cursor, session_owner_privacy, session_privacy_audit,
		         session_owner_privacy_audit, session_ingest_control,
		         session_ingest_bindings, session_push_owner_usage
		 RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO session_ingest_cursor (id) VALUES (true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO session_ingest_control (id, summarization_enabled) VALUES (true, false)`); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), pool
}

func admitAll(SessionChange) bool { return true }

func change(seq int64, session, owner, client string) SessionChange {
	sourceUpdatedAt := time.Now().Add(-time.Hour)
	return SessionChange{
		ChangeSeq: seq, SessionID: session, Owner: owner, OwnerKind: "human",
		Client: client, Role: "primary", OriginApp: "codex",
		Revision: "sha256:rev" + session, RewriteEpoch: 1,
		RecordCount: 20, UserTurnCount: 5, ContentBytes: 40000,
		SourceStartedAt: sourceUpdatedAt.Add(-time.Hour), SourceUpdatedAt: sourceUpdatedAt,
		UpdatedAt: sourceUpdatedAt, Complete: true,
	}
}

func TestCursor_InitialZero(t *testing.T) {
	s, _ := newTestStore(t)
	c, err := s.GetCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.ChangeSeq != 0 || c.PolicyHash != "" {
		t.Fatalf("initial cursor = %+v, want no position and no policy", c)
	}
}

func TestSessionSummarizationStartsUnpausedAndThePauseIsDurable(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	// The row with the column's default, as a new install has it.
	if _, err := pool.Exec(ctx, `DELETE FROM session_ingest_control`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO session_ingest_control (id) VALUES (true)`); err != nil {
		t.Fatal(err)
	}
	enabled, err := store.SessionSummarizationEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("session summarization started paused")
	}
	if err := store.SetSessionSummarizationEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	otherStore := NewStore(pool)
	enabled, err = otherStore.SessionSummarizationEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("the pause did not persist")
	}
}

func TestPendingCorpusTombstonesBoundedAndRetryable(test *testing.T) {
	store, pool := newTestStore(test)
	changes := make([]SessionChange, 101)
	for index := range changes {
		changes[index] = change(int64(index+1), fmt.Sprintf("tombstone-%03d", index), "owner", "codex")
	}
	if _, err := store.UpsertChanges(test.Context(), changes, admitAll); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(test.Context(), `
		UPDATE session_ingest_candidates
		   SET corpus_tombstone_pending = to_timestamp(change_seq)
	`); err != nil {
		test.Fatal(err)
	}
	first, err := store.ListPendingCorpusTombstones(test.Context())
	if err != nil {
		test.Fatal(err)
	}
	if len(first) != 100 || first[0].SessionID != "tombstone-000" || first[99].SessionID != "tombstone-099" {
		test.Fatalf("unexpected oldest-first bounded tombstone batch: %v", first)
	}
	for _, identity := range first[1:] {
		if err := store.ClearCorpusTombstone(test.Context(), identity); err != nil {
			test.Fatal(err)
		}
	}
	remaining, err := store.ListPendingCorpusTombstones(test.Context())
	if err != nil {
		test.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].SessionID != "tombstone-000" || remaining[1].SessionID != "tombstone-100" {
		test.Fatalf("failed and undispatched tombstones must remain pending: %v", remaining)
	}
}

func TestUpsertChanges_CoalescesAndAdvancesCursor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, err := s.UpsertChanges(ctx, []SessionChange{
		change(1, "sess-a", "o1", "codex"),
		change(2, "sess-b", "o1", "codex"),
		change(3, "sess-a", "o1", "codex"), // newer state, same session — coalesces
	}, admitAll)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetCursor(ctx)
	if c.ChangeSeq != 3 {
		t.Fatalf("cursor advanced to %d, want 3", c.ChangeSeq)
	}
	cands, err := s.FindPendingCandidates(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidate sessions (coalesced), got %d", len(cands))
	}
	for _, cnd := range cands {
		if cnd.SessionID == "sess-a" && cnd.ChangeSeq != 3 {
			t.Fatalf("sess-a change_seq = %d, want 3", cnd.ChangeSeq)
		}
	}
}

// TestUpsertChanges_FeedRebindLeavesOnlyTheCurrentOwnersCandidate: a
// session_id is unique to the feed after the UUID cutover, so the feed
// reporting it a second time under a different owner is a rebind
// (bindings.go's bindSessionTx: "the feed stays the authority for the owner
// of a session it writes"), not two independent sessions that happen to
// collide. Only the current owner's candidate survives; the former owner's
// is retired, not kept alongside it.
func TestUpsertChanges_FeedRebindLeavesOnlyTheCurrentOwnersCandidate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, err := s.UpsertChanges(ctx, []SessionChange{
		change(1, "ses_1", "a@x", "codex"),
		change(2, "ses_1", "b@y", "codex"), // same session_id, the feed moves it to a new owner
	}, admitAll)
	if err != nil {
		t.Fatal(err)
	}
	cands, _ := s.FindPendingCandidates(ctx, 100)
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate (the former owner's is retired on rebind), got %d", len(cands))
	}
	if cands[0].Owner != "b@y" {
		t.Fatalf("surviving candidate owner = %q, want %q (the feed's latest report)", cands[0].Owner, "b@y")
	}
}

func TestCountPendingCandidates_MatchesCompleteBacklog(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	readyA := change(1, "ready-a", "o1", "codex")
	readyB := change(2, "ready-b", "o1", "codex")
	incomplete := change(3, "incomplete", "o1", "codex")
	incomplete.Complete = false
	if _, err := s.UpsertChanges(ctx, []SessionChange{readyA, readyB, incomplete}, admitAll); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishRevision(ctx,
		Identity{Owner: readyA.Owner, Client: readyA.Client, SessionID: readyA.SessionID},
		readyA.Revision, readyA.RewriteEpoch, "sha256:body"); err != nil {
		t.Fatal(err)
	}

	counts, err := s.CountPendingCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (PendingCounts{Pending: 1}) {
		t.Fatalf("pending backlog = %+v, want 1 pending", counts)
	}
	batch, err := s.FindPendingCandidates(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != counts.Pending || batch[0].SessionID != readyB.SessionID {
		t.Fatalf("pending batch = %+v, want only %s", batch, readyB.SessionID)
	}
}

func TestPublishRevision_ReplacesPriorCanonically(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _ = s.UpsertChanges(ctx, []SessionChange{change(1, "sess-a", "o1", "codex")}, admitAll)

	id := Identity{Owner: "o1", Client: "codex", SessionID: "sess-a"}
	res, err := s.PublishRevision(ctx, id, "sha256:revsess-a", 1, "sha256:d1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.Stale {
		t.Fatalf("first publish = %+v, want published", res)
	}
	// Replacement with a newer revision: first advance the observed
	// head to rev2 so the CAS (revision matches candidate) passes.
	_, err = s.UpsertChanges(ctx, []SessionChange{
		{ChangeSeq: 9, SessionID: "sess-a", Owner: "o1", Client: "codex",
			OwnerKind: "human", Role: "primary", OriginApp: "codex",
			Revision: "sha256:rev2", RewriteEpoch: 2,
			RecordCount: 30, Complete: true,
			SourceStartedAt: time.Now().Add(-2 * time.Hour), SourceUpdatedAt: time.Now().Add(-time.Hour),
			UpdatedAt: time.Now()},
	}, admitAll)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := s.PublishRevision(ctx, id, "sha256:rev2", 2, "sha256:d2")
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Published || res2.Stale {
		t.Fatalf("replacement publish = %+v, want published", res2)
	}
	var super int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM session_ingest_published
		  WHERE owner='o1' AND client='codex' AND session_id='sess-a' AND superseded_at IS NOT NULL`,
	).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super != 1 {
		t.Fatalf("expected 1 superseded revision, got %d", super)
	}
}

func TestPublishRevision_StaleCASPRejectsOlderFetch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _ = s.UpsertChanges(ctx, []SessionChange{change(1, "sess-a", "o1", "codex")}, admitAll)
	id := Identity{Owner: "o1", Client: "codex", SessionID: "sess-a"}
	_, _ = s.PublishRevision(ctx, id, "sha256:revsess-a", 1, "d1")

	// Advance the observed head past what the fetch would have seen,
	// and make the new revision the active published head.
	_, _ = s.UpsertChanges(ctx, []SessionChange{
		{ChangeSeq: 9, SessionID: "sess-a", Owner: "o1", Client: "codex",
			OwnerKind: "human", Role: "primary", OriginApp: "codex",
			Revision: "sha256:newer", RewriteEpoch: 1,
			RecordCount: 30, Complete: true,
			SourceStartedAt: time.Now().Add(-2 * time.Hour), SourceUpdatedAt: time.Now().Add(-time.Hour),
			UpdatedAt: time.Now()},
	}, admitAll)
	if _, err := s.PublishRevision(ctx, id, "sha256:newer", 1, "d-new"); err != nil {
		t.Fatal(err)
	}

	// Try to publish the STALE older revision — must be rejected.
	res, err := s.PublishRevision(ctx, id, "sha256:revsess-a", 1, "d-old")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stale {
		t.Fatalf("expected stale CAS rejection, got %+v", res)
	}
	// The stale older revision must NOT have replaced the newer head.
	ok, _ := s.LoadPublishedRevision(ctx, id, "sha256:revsess-a")
	if ok {
		t.Error("stale older revision should not be the active published revision")
	}
	okNew, _ := s.LoadPublishedRevision(ctx, id, "sha256:newer")
	if !okNew {
		t.Error("newer revision should remain the active published revision")
	}
}

func TestPublishRevision_RejectsMissingSourceTimes(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	withoutSourceTimes := change(1, "sess-a", "o1", "codex")
	withoutSourceTimes.SourceStartedAt = time.Time{}
	withoutSourceTimes.SourceUpdatedAt = time.Time{}
	if _, err := s.UpsertChanges(ctx, []SessionChange{withoutSourceTimes}, admitAll); err != nil {
		t.Fatal(err)
	}
	id := Identity{Owner: "o1", Client: "codex", SessionID: "sess-a"}
	if _, err := s.PublishRevision(ctx, id, "sha256:revsess-a", 1, "d1"); err == nil {
		t.Fatal("publish must fail closed when canonical source timestamps are unavailable")
	}
	if ok, err := s.LoadPublishedRevision(ctx, id, "sha256:revsess-a"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("failed source-time validation must not publish a revision")
	}
}

func TestPublishRevision_AdvancesWatermarkOnlyOnPublish(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _ = s.UpsertChanges(ctx, []SessionChange{change(1, "sess-a", "o1", "codex")}, admitAll)
	id := Identity{Owner: "o1", Client: "codex", SessionID: "sess-a"}

	c, _ := s.LoadCandidate(ctx, id)
	if c.PublishedRevision != "" {
		t.Fatalf("published_revision should start empty, got %q", c.PublishedRevision)
	}
	// Publish advances it.
	_, _ = s.PublishRevision(ctx, id, "sha256:revsess-a", 1, "d1")
	c2, _ := s.LoadCandidate(ctx, id)
	if c2.PublishedRevision != "sha256:revsess-a" {
		t.Fatalf("published_revision = %q, want sha256:revsess-a", c2.PublishedRevision)
	}
}

func TestExclusion_TombstonesAndReinclusionReenables(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _ = s.UpsertChanges(ctx, []SessionChange{change(1, "s1", "o1", "codex")}, admitAll)
	id := Identity{Owner: "o1", Client: "codex", SessionID: "s1"}
	_, _ = s.PublishRevision(ctx, id, "sha256:revs1", 1, "d1")

	// Policy change excludes this session: admission returns false.
	excl, err := s.UpsertChanges(ctx, []SessionChange{
		{ChangeSeq: 2, SessionID: "s1", Owner: "o1", Client: "codex",
			OwnerKind: "human", Role: "primary", OriginApp: "diderot", // excluded app
			Revision: "sha256:revs1", Complete: true, UpdatedAt: time.Now()},
	}, func(SessionChange) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(excl) != 1 || excl[0].SessionID != "s1" {
		t.Fatalf("expected 1 newly-excluded identity, got %+v", excl)
	}
	c, _ := s.LoadCandidate(ctx, id)
	if c.ExcludedAt == nil {
		t.Fatal("candidate should be excluded")
	}
	// Published ledger tombstoned.
	ok, _ := s.LoadPublishedRevision(ctx, id, "sha256:revs1")
	if ok {
		t.Error("excluded session's published revision should be tombstoned")
	}

	// Re-inclusion: same session re-admitted → excluded_at cleared, eligible again.
	_, err = s.UpsertChanges(ctx, []SessionChange{
		{ChangeSeq: 3, SessionID: "s1", Owner: "o1", Client: "codex",
			OwnerKind: "human", Role: "primary", OriginApp: "codex",
			Revision: "sha256:rev2", Complete: true, UpdatedAt: time.Now()},
	}, admitAll)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := s.LoadCandidate(ctx, id)
	if c2.ExcludedAt != nil {
		t.Fatal("re-included candidate's excluded_at should be cleared")
	}
}

func TestUpsertChangesAt_PersistsExplicitCursorGreaterThanRows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// The feed may legally return an explicit next_cursor that is GREATER
	// than every served change_seq (e.g. it coalesced a newer change into
	// the same page or reserved a later cursor slot). The consumer must
	// accept and persist exactly that cursor — never clamp it to the max
	// served row (which would re-deliver work).
	rows := []SessionChange{
		change(1, "s1", "o1", "codex"),
		change(3, "s2", "o1", "codex"),
	}
	if _, err := s.UpsertChangesAt(ctx, rows, 10, admitAll); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetCursor(ctx)
	if c.ChangeSeq != 10 {
		t.Fatalf("cursor = %d, want explicit next_cursor 10", c.ChangeSeq)
	}
}

func TestUpsertChangesAt_RejectsCursorTrailingRows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	rows := []SessionChange{
		change(7, "s1", "o1", "codex"),
	}
	// A next_cursor behind its own highest served row would re-deliver
	// the row on the next drain — must be rejected, never persisted.
	if _, err := s.UpsertChangesAt(ctx, rows, 5, admitAll); err == nil {
		t.Fatal("next_cursor trailing served rows should fail")
	}
	c, _ := s.GetCursor(ctx)
	if c.ChangeSeq != 0 {
		t.Fatalf("rejected page must not move the cursor, got %d", c.ChangeSeq)
	}
}
