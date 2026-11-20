package sessioningest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// newPublishTestStore boots the sessioningest store AND the events
// sink corpus against the same test DB, so we can verify the real
// publish + tombstone path end-to-end.
func newPublishTestStore(t *testing.T) (*Store, *events.DBSink, *pgxpool.Pool) {
	store, pool := newTestStore(t)
	// Reset the events corpus too (both packages share the clone's DB
	// but tests truncate independently).
	if _, err := pool.Exec(context.Background(), `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	return store, sink, pool
}

func docFor(id Identity, revision string) *sessionDocument {
	body := validRevisionBodyFor(id, revision)
	d, err := parseSessionDocument(body, id, revision, 0)
	if err != nil {
		panic(err)
	}
	return d
}

// validRevisionBodyFor builds a Bsky Session v1 document whose
// envelope carries the given canonical identity (owner/client/session)
// so identity-pinned validation passes for the requested candidate.
func validRevisionBodyFor(id Identity, revision string) []byte {
	record := `{"index":0,"id":"rec0","role":"user","content":"hello","timestamp":"2026-07-24T18:00:00Z"}`
	envelope := fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":%q,"revision":%q,"record_count":1,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":%q,"owner_kind":"human","source":%q,"session_role":"primary","origin_app":"codex","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}`,
		id.SessionID, revision, len(record)+1, id.Owner, id.Client)
	return []byte(envelope + "\n" + record + "\n")
}

func TestEventPublisher_PublishAndTombstone(t *testing.T) {
	store, sink, pool := newPublishTestStore(t)
	ep := NewEventPublisher(sink, store)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	ext := externalID(id)

	rev := "sha256:v1"
	var legacyEventID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, session_id, record_start, record_end, scope)
		VALUES ('agent_session', $1, now(), 'legacy duplicate', '{"legacy":true}', $2, 0, 1, 'internal')
		RETURNING id`, id.Owner+"/"+id.Client+"/"+id.SessionID+":session", id.SessionID).Scan(&legacyEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO event_chunks (event_id, chunk_idx, kind, content, context_prefix, occurred_at)
		VALUES ($1, 0, 'text', 'legacy duplicate', '', now())`, legacyEventID); err != nil {
		t.Fatal(err)
	}
	// Seed the candidate so the CAS (which requires the current head to
	// match) passes.
	if _, err := store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: rev, RewriteEpoch: 1, RecordCount: 1, Complete: true,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour),
	}}, admitAll); err != nil {
		t.Fatal(err)
	}
	res, err := ep.PublishSession(ctx, docFor(id, rev), id, "", rev, 1, digest([]byte(validRevisionBodyFor(id, rev))), events.ScopeInternal, SummaryResult{
		Narrative: "hello", Title: "Useful session title", TitleModel: "title-model",
		ModelID: "map-model", InputDigest: "sha256:summary-input",
		Chunks: []SummaryRecord{{Identity: id, RecordStart: 0, RecordEnd: 1, Summary: "chunk", ModelID: "map-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.Stale {
		t.Fatalf("atomic publish = %+v, want published", res)
	}
	// Corpus row exists, active (not deleted).
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`,
		ext).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 active corpus event, got %d", n)
	}
	var eventTitle string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`,
		ext).Scan(&eventTitle); err != nil {
		t.Fatal(err)
	}
	if eventTitle != "Useful session title" {
		t.Fatalf("event title = %q", eventTitle)
	}
	var legacyDeleted bool
	var legacyBody string
	var legacyChunks int
	if err := pool.QueryRow(ctx, `SELECT deleted, body FROM events WHERE id=$1`, legacyEventID).Scan(&legacyDeleted, &legacyBody); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE event_id=$1`, legacyEventID).Scan(&legacyChunks); err != nil {
		t.Fatal(err)
	}
	if !legacyDeleted || legacyChunks != 0 || legacyBody != "legacy duplicate" {
		t.Fatalf("legacy duplicate not safely superseded: deleted=%v chunks=%d body=%q", legacyDeleted, legacyChunks, legacyBody)
	}
	var archivedEventBody, archivedChunkContent string
	if err := pool.QueryRow(ctx,
		`SELECT event_snapshot->>'body' FROM legacy_session_event_supersessions WHERE event_id=$1`,
		legacyEventID).Scan(&archivedEventBody); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT chunk_snapshot->>'content' FROM legacy_session_event_chunk_archive WHERE event_id=$1`,
		legacyEventID).Scan(&archivedChunkContent); err != nil {
		t.Fatal(err)
	}
	if archivedEventBody != "legacy duplicate" || archivedChunkContent != "legacy duplicate" {
		t.Fatalf("legacy archive event=%q chunk=%q", archivedEventBody, archivedChunkContent)
	}
	var summaries, chunks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_summaries WHERE owner=$1 AND client=$2 AND session_id=$3 AND revision=$4`, id.Owner, id.Client, id.SessionID, rev).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_chunk_summaries WHERE owner=$1 AND client=$2 AND session_id=$3`, id.Owner, id.Client, id.SessionID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || chunks != 1 {
		t.Fatalf("atomic summary state: summaries=%d chunks=%d", summaries, chunks)
	}
	var inputDigest, title, titleModel, titleDigest string
	if err := pool.QueryRow(ctx, `SELECT input_digest, title, title_model, title_input_digest FROM session_ingest_summaries WHERE owner=$1 AND client=$2 AND session_id=$3`, id.Owner, id.Client, id.SessionID).Scan(&inputDigest, &title, &titleModel, &titleDigest); err != nil {
		t.Fatal(err)
	}
	if inputDigest != "sha256:summary-input" || title != "Useful session title" ||
		titleModel != "title-model" || titleDigest != inputDigest {
		t.Fatalf("persisted title input=%q title=%q model=%q digest=%q",
			inputDigest, title, titleModel, titleDigest)
	}

	// Tombstone removes it.
	if err := ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	var dead int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=true`,
		ext).Scan(&dead); err != nil {
		t.Fatal(err)
	}
	if dead != 1 {
		t.Fatalf("expected 1 tombstoned corpus event, got %d", dead)
	}
}

func TestSessionURLUsesSessionIDAsCanonicalIdentity(t *testing.T) {
	got := sessionURL("https://sessions.example/", Identity{
		Owner: "a+b@example.com", Client: "claude-code", SessionID: "ses/one",
	})
	want := "https://sessions.example/sessions/ses%2Fone"
	if got != want {
		t.Fatalf("sessionURL = %q, want %q", got, want)
	}
	ranged := sessionRangeURL("https://sessions.example/", Identity{
		Owner: "a+b@example.com", Client: "claude-code", SessionID: "ses/one",
	}, 4, 9)
	if ranged != want+"?start=4&end=9" {
		t.Fatalf("sessionRangeURL = %q", ranged)
	}
}

func TestEventPublisher_StaleCASDoesNotWriteCorpus(t *testing.T) {
	store, sink, pool := newPublishTestStore(t)
	ep := NewEventPublisher(sink, store)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	ext := externalID(id)

	// Seed the candidate with observed head v2 (never fetched).
	_, err := store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v2", RewriteEpoch: 2, RecordCount: 20, Complete: true,
		UpdatedAt: time.Now().Add(-time.Hour),
	}}, admitAll)
	if err != nil {
		t.Fatal(err)
	}

	// Publish a document fetched for the OLD v1 head. The CAS must
	// reject; critically, the events corpus must remain untouched (no
	// partial/vexing orphan written), because the write + CAS are in
	// one transaction.
	res, err := ep.PublishSession(ctx, docFor(id, "sha256:v1"), id, "",
		"sha256:v1", 1, "d1", events.ScopeInternal, SummaryResult{Narrative: "stale summary"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stale {
		t.Fatalf("expected stale CAS, got %+v", res)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`,
		ext).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stale publish wrote %d corpus rows; want 0 (corpus + ledger must be atomic)", n)
	}
	var summaries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_summaries WHERE owner=$1 AND client=$2 AND session_id=$3`, id.Owner, id.Client, id.SessionID).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 {
		t.Fatalf("stale publish wrote %d summary rows; want 0", summaries)
	}
}

func TestEventPublisher_SourceTimeMismatchDoesNotWriteCorpus(t *testing.T) {
	store, sink, pool := newPublishTestStore(t)
	ep := NewEventPublisher(sink, store)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_time_mismatch"}
	rev := "sha256:v1"
	_, err := store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", Revision: rev, RewriteEpoch: 1,
		RecordCount: 1, Complete: true, UpdatedAt: time.Now(),
		SourceStartedAt: time.Date(2026, 7, 24, 17, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
	}}, admitAll)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ep.PublishSession(ctx, docFor(id, rev), id, "", rev, 1,
		digest(validRevisionBodyFor(id, rev)), events.ScopeInternal, SummaryResult{Narrative: "must roll back"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stale {
		t.Fatalf("source-time mismatch result=%+v, want stale", res)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE external_id=$1`, externalID(id)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("source-time mismatch wrote %d corpus rows", count)
	}
}

func TestEventPublisher_MissingCandidateSourceTimesDoesNotWriteCorpus(t *testing.T) {
	store, sink, pool := newPublishTestStore(t)
	ep := NewEventPublisher(sink, store)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_time_missing"}
	rev := "sha256:v1"
	_, err := store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", Revision: rev, RewriteEpoch: 1,
		RecordCount: 1, Complete: true, UpdatedAt: time.Now(),
	}}, admitAll)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ep.PublishSession(ctx, docFor(id, rev), id, "", rev, 1,
		digest(validRevisionBodyFor(id, rev)), events.ScopeInternal, SummaryResult{Narrative: "must roll back"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stale {
		t.Fatalf("missing candidate source-time result=%+v, want stale", res)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE external_id=$1`, externalID(id)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("missing candidate source times wrote %d corpus rows", count)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_published
		WHERE owner=$1 AND client=$2 AND session_id=$3`, id.Owner, id.Client, id.SessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("missing candidate source times wrote %d ledger rows", count)
	}
}

func TestEventPublisher_TombstoneSurvivesRestart(t *testing.T) {
	store, _, pool := newPublishTestStore(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	// A head was excluded + marked pending before a "crash" (the corpus
	// Tombstone call never ran). On restart the pending marker must
	// surface so the worker re-tombstones.
	_, err := store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "service", Role: "rider", OriginApp: "diderot", // not admitted
		Deleted: true, UpdatedAt: time.Now(),
	}}, func(SessionChange) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingCorpusTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != id {
		t.Fatalf("expected 1 pending tombstone, got %+v", pending)
	}
	// Clear it as the worker would after a successful tombstone.
	if err := store.ClearCorpusTombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_ingest_candidates WHERE owner=$1 AND client=$2 AND session_id=$3 AND corpus_tombstone_pending IS NOT NULL`,
		id.Owner, id.Client, id.SessionID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("pending tombstone not cleared: %d rows", remaining)
	}
}

func TestOwnerOnlyPolicyRestampsAllHistoricalNativeSessionEvents(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	selected := Identity{Owner: "selected@example.com", Client: "codex", SessionID: "ses_selected"}
	other := Identity{Owner: "other@example.com", Client: "codex", SessionID: "ses_other"}

	for _, id := range []Identity{selected, other} {
		h.seedCandidate(t, id, "sha256:v1", 1, 1)
		h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
			Identity: id, RecordStart: 0, RecordEnd: 1,
			ContentHash: "h1", Summary: "range", ModelID: "map-model",
		}})
	}
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var slackEventID int64
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('slack', $1, now(), 'unrelated source', $2, 'public')
		RETURNING id
	`, externalID(selected), mustSummaryJSON(map[string]any{"owner": selected.Owner})).Scan(&slackEventID); err != nil {
		t.Fatal(err)
	}

	candidate, err := h.store.LoadCandidate(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	change := SessionChange{
		ChangeSeq: 2, SessionID: selected.SessionID, Owner: selected.Owner, Client: selected.Client,
		OwnerKind: candidate.OwnerKind, Role: candidate.Role, OriginApp: candidate.OriginApp,
		Revision: candidate.Revision, RewriteEpoch: candidate.RewriteEpoch,
		RecordCount: candidate.RecordCount, UserTurnCount: candidate.UserTurnCount,
		ContentBytes: candidate.ContentBytes, SourceStartedAt: candidate.SourceStartedAt,
		SourceUpdatedAt: candidate.SourceUpdatedAt, UpdatedAt: candidate.UpdatedAt,
		Complete: candidate.Complete,
	}
	policy := DefaultPolicy()
	policy.OwnerOnly.Principals = []string{selected.Owner}
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{change}, 2, func(ch SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: policy.VisibilityScope(ch.Owner)}
	}); err != nil {
		t.Fatal(err)
	}

	wantScope := events.ScopeOwner(selected.Owner)
	var selectedTotal, selectedOwnerScoped int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE scope=$2)
		  FROM events
		 WHERE source='agent_session' AND deleted=false AND raw->>'owner'=$1
	`, selected.Owner, wantScope).Scan(&selectedTotal, &selectedOwnerScoped); err != nil {
		t.Fatal(err)
	}
	if selectedTotal != 2 || selectedOwnerScoped != selectedTotal {
		t.Fatalf("selected native events total=%d owner_scoped=%d, want 2/2", selectedTotal, selectedOwnerScoped)
	}
	var otherWrongScope int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND deleted=false AND raw->>'owner'=$1 AND scope<>$2
	`, other.Owner, events.ScopeInternal).Scan(&otherWrongScope); err != nil {
		t.Fatal(err)
	}
	if otherWrongScope != 0 {
		t.Fatalf("restamp changed %d events for an unselected owner", otherWrongScope)
	}
	var slackScope string
	if err := h.pool.QueryRow(ctx, `SELECT scope FROM events WHERE id=$1`, slackEventID).Scan(&slackScope); err != nil {
		t.Fatal(err)
	}
	if slackScope != events.ScopePublic {
		t.Fatalf("session policy changed unrelated Slack event scope to %q", slackScope)
	}
	got, err := h.store.LoadCandidate(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if got.VisibilityScope != wantScope {
		t.Fatalf("candidate visibility scope = %q, want %q", got.VisibilityScope, wantScope)
	}
}

func TestOwnerOnlyCandidateAcceptsFeedOwnerNormalizationDifferences(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	change := SessionChange{
		ChangeSeq: 1, SessionID: "ses_normalized", Owner: " Selected@Example.com\t", Client: "codex",
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v1", RewriteEpoch: 1, RecordCount: 1,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
	}
	policy := DefaultPolicy()
	policy.OwnerOnly.Principals = []string{"selected@example.com"}
	wantScope := events.ScopeOwner("selected@example.com")
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{change}, 1, func(ch SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: policy.VisibilityScope(ch.Owner)}
	}); err != nil {
		t.Fatal(err)
	}
	var gotScope string
	if err := h.pool.QueryRow(ctx, `
		SELECT visibility_scope
		  FROM session_ingest_candidates
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, change.Owner, change.Client, change.SessionID).Scan(&gotScope); err != nil {
		t.Fatal(err)
	}
	if gotScope != wantScope {
		t.Fatalf("visibility scope = %q, want %q", gotScope, wantScope)
	}
}

func TestOwnerOnlyPolicyFlipRejectsInflightInternalPublish(t *testing.T) {
	store, sink, pool := newPublishTestStore(t)
	ep := NewEventPublisher(sink, store)
	ctx := context.Background()
	id := Identity{Owner: "selected@example.com", Client: "codex", SessionID: "ses_race"}
	rev := "sha256:v1"
	change := SessionChange{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: rev, RewriteEpoch: 1, RecordCount: 1, Complete: true,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour),
	}
	if _, err := store.UpsertChanges(ctx, []SessionChange{change}, admitAll); err != nil {
		t.Fatal(err)
	}
	change.ChangeSeq = 2
	wantScope := events.ScopeOwner(id.Owner)
	if _, err := store.upsertScopedAt(ctx, []SessionChange{change}, 2, func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
	}); err != nil {
		t.Fatal(err)
	}

	result, err := ep.PublishSession(ctx, docFor(id, rev), id, "", rev, 1,
		digest(validRevisionBodyFor(id, rev)), events.ScopeInternal,
		SummaryResult{Narrative: "must not publish broadly"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stale {
		t.Fatalf("internal publish after owner-only flip = %+v, want stale", result)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`, externalID(id)).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("stale broad publish left %d active events", active)
	}

	result, err = ep.PublishSession(ctx, docFor(id, rev), id, "", rev, 1,
		digest(validRevisionBodyFor(id, rev)), wantScope,
		SummaryResult{Narrative: "owner-only summary"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.Stale {
		t.Fatalf("owner-only publish = %+v", result)
	}
	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`, externalID(id)).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != wantScope {
		t.Fatalf("published scope = %q, want %q", scope, wantScope)
	}
}
