package sessioningest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// chunkTestHarness boots the publish store + events corpus and seeds a
// published session with the given native map-summary chunks (through
// the real atomic PublishSession path so the ledger + summary state
// match production).
type chunkTestHarness struct {
	store *Store
	sink  *events.DBSink
	pool  *pgxpool.Pool
	ep    *EventPublisher
}

func newChunkHarness(t *testing.T) *chunkTestHarness {
	t.Helper()
	store, sink, pool := newPublishTestStore(t)
	return &chunkTestHarness{store: store, sink: sink, pool: pool, ep: NewEventPublisher(sink, store)}
}

func (h *chunkTestHarness) seedCandidate(t *testing.T, id Identity, revision string, rewriteEpoch int64, recordCount int) {
	t.Helper()
	h.seedCandidateAt(t, id, revision, rewriteEpoch, recordCount,
		time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC))
}

func (h *chunkTestHarness) seedCandidateAt(t *testing.T, id Identity, revision string, rewriteEpoch int64, recordCount int, sourceStartedAt, sourceUpdatedAt time.Time) {
	t.Helper()
	if _, err := h.store.UpsertChanges(context.Background(), []SessionChange{{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: revision, RewriteEpoch: rewriteEpoch, RecordCount: recordCount,
		SourceStartedAt: sourceStartedAt, SourceUpdatedAt: sourceUpdatedAt,
		Complete: true, UpdatedAt: time.Now().Add(-time.Hour),
	}}, admitAll); err != nil {
		t.Fatal(err)
	}
}

func (h *chunkTestHarness) publish(t *testing.T, id Identity, revision string, rewriteEpoch int64, chunks []SummaryRecord) {
	t.Helper()
	h.publishAt(t, id, revision, rewriteEpoch,
		time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC), chunks)
}

func (h *chunkTestHarness) publishAt(t *testing.T, id Identity, revision string, rewriteEpoch int64, sourceStartedAt, sourceUpdatedAt time.Time, chunks []SummaryRecord) {
	h.publishRecordsAt(t, id, revision, rewriteEpoch, sourceStartedAt, sourceUpdatedAt, chunks, nil)
}

func (h *chunkTestHarness) publishRecordsAt(t *testing.T, id Identity, revision string, rewriteEpoch int64, sourceStartedAt, sourceUpdatedAt time.Time, chunks []SummaryRecord, recordTimes []time.Time) {
	t.Helper()
	recordCount := 0
	for _, c := range chunks {
		if c.RecordEnd > recordCount {
			recordCount = c.RecordEnd
		}
	}
	if recordCount < 1 {
		recordCount = 1
	}
	var records []string
	for i := 0; i < recordCount; i++ {
		recordTime := sourceStartedAt
		if recordTimes != nil {
			recordTime = recordTimes[i]
		}
		records = append(records, fmt.Sprintf(
			`{"index":%d,"id":"rec%d","role":"user","content":"hello","timestamp":%q}`,
			i, i, recordTime.UTC().Format(time.RFC3339Nano)))
	}
	contentBytes := 0
	for _, r := range records {
		contentBytes += len(r) + 1
	}
	body := fmt.Sprintf(
		"%s\n%s\n",
		fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":%q,"revision":%q,"record_count":%d,"user_turn_count":%d,"content_bytes":%d,"complete":true,"owner":%q,"owner_kind":"human","source":%q,"session_role":"primary","origin_app":"codex","started_at":%q,"updated_at":%q}`,
			id.SessionID, revision, recordCount, recordCount, contentBytes, id.Owner, id.Client,
			sourceStartedAt.UTC().Format(time.RFC3339Nano), sourceUpdatedAt.UTC().Format(time.RFC3339Nano)),
		strings.Join(records, "\n"),
	)
	doc, err := parseSessionDocument([]byte(body), id, revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := h.store.LoadCandidate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.ep.PublishSession(context.Background(), doc, id, "https://viewer", revision, rewriteEpoch,
		digest([]byte(body)), candidate.VisibilityScope, SummaryResult{
			Narrative:     "narrative " + revision,
			EmbeddingText: "embedding " + revision,
			ModelID:       "map-model",
			Chunks:        chunks,
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stale {
		t.Fatalf("publish = %+v, want current revision", res)
	}
}

func TestOwnerOnlyCandidatePublishesSessionAndRangeEventsWithOwnerScope(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "selected@example.com", Client: "codex", SessionID: "ses_owner_only"}
	change := SessionChange{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v1", RewriteEpoch: 1, RecordCount: 1,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
	}
	wantScope := events.ScopeOwner(id.Owner)
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{change}, 1, func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
	}); err != nil {
		t.Fatal(err)
	}
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
		Identity: id, RecordStart: 0, RecordEnd: 1,
		ContentHash: "h1", Summary: "owner range", ModelID: "map-model",
	}})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var total, scoped int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE scope=$2)
		  FROM events
		 WHERE source='agent_session' AND deleted=false AND raw->>'owner'=$1
	`, id.Owner, wantScope).Scan(&total, &scoped); err != nil {
		t.Fatal(err)
	}
	if total != 2 || scoped != total {
		t.Fatalf("owner-only native events total=%d scoped=%d, want 2/2", total, scoped)
	}
}

func TestChunkReconcilerRepairsOwnerScopeDrift(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			testChunkReconcilerRepairsOwnerScopeDrift(t, legacy)
		})
	}
}

func testChunkReconcilerRepairsOwnerScopeDrift(t *testing.T, legacy bool) {
	t.Helper()
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "selected@example.com", Client: "codex", SessionID: "ses_scope_drift"}
	change := SessionChange{
		ChangeSeq: 1, SessionID: id.SessionID, Owner: id.Owner, Client: id.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v1", RewriteEpoch: 1, RecordCount: 1,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
	}
	wantScope := events.ScopeOwner(id.Owner)
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{change}, 1, func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
	}); err != nil {
		t.Fatal(err)
	}
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
		Identity: id, RecordStart: 0, RecordEnd: 1,
		ContentHash: "h1", Summary: "owner range", ModelID: "map-model",
	}})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	rangeID := rangeExternalID(id, 0, 1)
	embeddedAt := change.SourceUpdatedAt.Add(time.Hour)
	if _, err := h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_at=$1, embedder_id='existing-model'`, embeddedAt); err != nil {
		t.Fatal(err)
	}
	var originalID int64
	var originalTime time.Time
	if err := h.pool.QueryRow(ctx, `SELECT c.id,c.occurred_at FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id=$1`, rangeID).Scan(&originalID, &originalTime); err != nil {
		t.Fatal(err)
	}
	if legacy {
		if _, err := h.pool.Exec(ctx, `UPDATE session_ingest_chunk_summaries SET occurred_at=NULL`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE events SET scope='internal'
		 WHERE source='agent_session' AND external_id = ANY($1)
	`, []string{externalID(id), rangeID}); err != nil {
		t.Fatal(err)
	}
	repaired, err := h.store.ReconcileCanonicalEventScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 2 {
		t.Errorf("canonical scope reconcile repaired %d events, want 2", repaired)
	}
	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 0 {
		t.Errorf("scope-drift reconcile = %+v, want no content republishing", res)
	}
	var wrongScope int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND external_id = ANY($1) AND scope<>$2
	`, []string{externalID(id), rangeID}, wantScope).Scan(&wrongScope); err != nil {
		t.Fatal(err)
	}
	if wrongScope != 0 {
		t.Errorf("%d canonical/range events retained a widened scope", wrongScope)
	}
	var chunkID int64
	var eventTime, chunkTime time.Time
	var embedded *time.Time
	var model string
	if err := h.pool.QueryRow(ctx, `SELECT c.id,e.occurred_at,c.occurred_at,c.embedded_at,coalesce(c.embedder_id,'') FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id=$1`, rangeID).Scan(&chunkID, &eventTime, &chunkTime, &embedded, &model); err != nil {
		t.Fatal(err)
	}
	if chunkID != originalID || embedded == nil || !embedded.Equal(embeddedAt) || model != "existing-model" {
		t.Errorf("scope repair replaced embedding: id %d -> %d, embedded=%v model=%q", originalID, chunkID, embedded, model)
	}
	if !eventTime.Equal(originalTime) || !chunkTime.Equal(originalTime) {
		t.Errorf("scope repair changed event/chunk time=%s/%s, want %s", eventTime, chunkTime, originalTime)
	}
	var queued int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id=ANY($1) AND c.scope_sync_needed`, []string{externalID(id), rangeID}).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 2 {
		t.Errorf("scope repair queued %d payload corrections, want 2", queued)
	}
}

func TestChunkSelectionPrioritizesScopeRepairAheadOfContentChurn(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	staleContent := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_content"}
	staleScope := Identity{Owner: "z@x", Client: "codex", SessionID: "ses_scope"}

	h.seedCandidate(t, staleContent, "sha256:v1", 1, 1)
	ownerChange := SessionChange{
		ChangeSeq: 2, SessionID: staleScope.SessionID, Owner: staleScope.Owner, Client: staleScope.Client,
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v1", RewriteEpoch: 1, RecordCount: 1,
		SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
		UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
	}
	wantScope := events.ScopeOwner(staleScope.Owner)
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{ownerChange}, 2, func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []Identity{staleContent, staleScope} {
		h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
			Identity: id, RecordStart: 0, RecordEnd: 1,
			ContentHash: "h1", Summary: "range", ModelID: "map-model",
		}})
	}
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE events
		   SET body = CASE external_id
		       WHEN $1 THEN 'stale content'
		       WHEN $2 THEN body
		       END,
		       scope = CASE external_id
		       WHEN $1 THEN scope
		       WHEN $2 THEN 'internal'
		       END
		 WHERE source='agent_session' AND external_id = ANY($3)
	`, rangeExternalID(staleContent, 0, 1), rangeExternalID(staleScope, 0, 1),
		[]string{rangeExternalID(staleContent, 0, 1), rangeExternalID(staleScope, 0, 1)}); err != nil {
		t.Fatal(err)
	}

	items, err := h.store.SelectChunkEventsToPublish(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Identity != staleScope {
		t.Fatalf("first bounded repair = %+v, want scope-drifted session %+v", items, staleScope)
	}
}

func TestReconcileChunkEventsUsesRangeTimeAcrossBackfillRewriteAndRepublish(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_time"}
	started := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	firstUpdated := started.Add(2 * time.Hour)
	appendUpdated := started.Add(26 * time.Hour)
	rewriteUpdated := started.Add(50 * time.Hour)
	chunks := []SummaryRecord{{
		Identity: id, RecordStart: 0, RecordEnd: 1,
		ContentHash: "h0", Summary: "historical range", ModelID: "map-model",
	}}

	h.seedCandidateAt(t, id, "sha256:v1", 1, 1, started, firstUpdated)
	h.publishAt(t, id, "sha256:v1", 1, started, firstUpdated, chunks)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	t.Run("initial_backfill", func(t *testing.T) {
		assertSessionAndRangeOccurredAt(t, h.pool, id, firstUpdated, started)
	})

	appendChunks := append([]SummaryRecord(nil), chunks...)
	appendChunks = append(appendChunks, SummaryRecord{
		Identity: id, RecordStart: 1, RecordEnd: 2,
		ContentHash: "h1", Summary: "appended historical range", ModelID: "map-model",
	})
	h.seedCandidateAt(t, id, "sha256:v2", 1, 2, started, appendUpdated)
	h.publishAt(t, id, "sha256:v2", 1, started, appendUpdated, appendChunks)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	t.Run("append", func(t *testing.T) {
		assertSessionAndRangeOccurredAt(t, h.pool, id, appendUpdated, started)
	})

	appendChunks[0].ContentHash = "h0-rewrite"
	appendChunks[0].Summary = "rewritten historical range"
	h.seedCandidateAt(t, id, "sha256:v3", 2, 2, started, rewriteUpdated)
	h.publishAt(t, id, "sha256:v3", 2, started, rewriteUpdated, appendChunks)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	t.Run("rewrite", func(t *testing.T) {
		assertSessionAndRangeOccurredAt(t, h.pool, id, rewriteUpdated, started)
	})

	if err := h.ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	h.publishAt(t, id, "sha256:v3", 2, started, rewriteUpdated, appendChunks)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	t.Run("republish", func(t *testing.T) {
		assertSessionAndRangeOccurredAt(t, h.pool, id, rewriteUpdated, started)
	})
}

func assertSessionAndRangeOccurredAt(t *testing.T, pool *pgxpool.Pool, id Identity, sessionTime, rangeTime time.Time) {
	t.Helper()
	for externalID, want := range map[string]time.Time{externalID(id): sessionTime, rangeExternalID(id, 0, 1): rangeTime} {
		var got time.Time
		if err := pool.QueryRow(context.Background(), `
			SELECT occurred_at FROM events
			 WHERE source='agent_session' AND external_id=$1 AND deleted=false`, externalID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Fatalf("event %s occurred_at=%s, want source time %s", externalID, got, want)
		}
	}
}

func TestReconcileChunkEventsGrowthPreservesRangeTimeAndEmbeddings(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_append_recency"}
	started := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	firstUpdated := started.Add(2 * time.Hour)
	appendUpdated := started.Add(26 * time.Hour)
	recordTimes := []time.Time{started.Add(45*time.Minute + 123456*time.Microsecond), started.Add(15 * time.Minute), started.Add(90 * time.Minute)}
	chunks := []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "unchanged first range", ModelID: "map-model"},
		{Identity: id, RecordStart: 2, RecordEnd: 3, ContentHash: "h1", Summary: "unchanged second range", ModelID: "map-model"},
	}
	h.seedCandidateAt(t, id, "sha256:v1", 1, 3, started, firstUpdated)
	h.publishRecordsAt(t, id, "sha256:v1", 1, started, firstUpdated, chunks, recordTimes)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	embeddedAt := started.Add(30 * time.Hour)
	if _, err := h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_at=$1, embedder_id='existing-model'`, embeddedAt); err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	for _, chunk := range chunks {
		external := rangeExternalID(id, chunk.RecordStart, chunk.RecordEnd)
		var chunkID int64
		if err := h.pool.QueryRow(ctx, `SELECT c.id FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id=$1`, external).Scan(&chunkID); err != nil {
			t.Fatal(err)
		}
		before[external] = chunkID
	}
	chunks = append(chunks, SummaryRecord{Identity: id, RecordStart: 3, RecordEnd: 4, ContentHash: "h2", Summary: "new range", ModelID: "map-model"})
	recordTimes = append(recordTimes, appendUpdated.Add(-time.Minute))
	h.seedCandidateAt(t, id, "sha256:v2", 1, 4, started, appendUpdated)
	h.publishRecordsAt(t, id, "sha256:v2", 1, started, appendUpdated, chunks, recordTimes)
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		external := rangeExternalID(id, chunk.RecordStart, chunk.RecordEnd)
		wantTime := recordTimes[chunk.RecordStart]
		for _, recordTime := range recordTimes[chunk.RecordStart:chunk.RecordEnd] {
			if recordTime.After(wantTime) {
				wantTime = recordTime
			}
		}
		var chunkID int64
		var eventTime, chunkTime time.Time
		var embedded *time.Time
		var model, revision string
		if err := h.pool.QueryRow(ctx, `SELECT c.id,e.occurred_at,c.occurred_at,c.embedded_at,coalesce(c.embedder_id,''),e.raw->>'revision'
			FROM events e JOIN event_chunks c ON c.event_id=e.id WHERE e.external_id=$1`, external).Scan(&chunkID, &eventTime, &chunkTime, &embedded, &model, &revision); err != nil {
			t.Fatal(err)
		}
		if !eventTime.Equal(wantTime) || !chunkTime.Equal(wantTime) {
			t.Errorf("range %s event/chunk time=%s/%s, want own-record time %s", external, eventTime, chunkTime, wantTime)
		}
		if revision != "sha256:v2" {
			t.Errorf("range %s revision=%s, want current metadata", external, revision)
		}
		if oldID, existed := before[external]; existed {
			if chunkID != oldID || embedded == nil || !embedded.Equal(embeddedAt) || model != "existing-model" {
				t.Errorf("unchanged range %s lost embedding: id %d -> %d, embedded=%v model=%q", external, oldID, chunkID, embedded, model)
			}
			var deleted int
			if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM qdrant_delete_queue WHERE point_id=$1`, oldID).Scan(&deleted); err != nil {
				t.Fatal(err)
			}
			if deleted != 0 {
				t.Errorf("unchanged range %s queued existing vector for deletion", external)
			}
		} else if embedded != nil {
			t.Errorf("new range %s should need its first embedding", external)
		}
	}
	var sessionTime time.Time
	if err := h.pool.QueryRow(ctx, `SELECT occurred_at FROM events WHERE external_id=$1`, externalID(id)).Scan(&sessionTime); err != nil {
		t.Fatal(err)
	}
	if !sessionTime.Equal(appendUpdated) {
		t.Errorf("whole-session time=%s, want latest activity %s", sessionTime, appendUpdated)
	}
}

func (h *chunkTestHarness) countActiveRangeEvents(ctx context.Context, id Identity) (int, error) {
	var n int
	err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND deleted=false
		   AND external_id LIKE $1 || '%' ESCAPE '\'
	`, escapeLikeForTest(rangeExternalIDPrefix(id))).Scan(&n)
	return n, err
}

func escapeLikeForTest(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func TestReconcileChunkEvents_InitialRangePublication(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 2 || res.NoChange != 0 || res.Stale != 0 || res.Tombstoned != 0 {
		t.Fatalf("reconcile result = %+v, want 2 upserts", res)
	}

	n, err := h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("active range events = %d, want 2", n)
	}
	// The canonical final :session event is preserved as the single
	// session event (unaffected, untouched, still active).
	var sessions int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source='agent_session' AND external_id=$1 AND deleted=false`,
		externalID(id)).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("active :session events = %d, want 1", sessions)
	}

	// Range events carry the stable id + deterministic raw + expansion
	// coordinates.
	var (
		gotExt, gotRaw, gotSession string
		gotStart, gotEnd           int
	)
	if err := h.pool.QueryRow(ctx, `
		SELECT external_id, raw::text, session_id, record_start, record_end
		  FROM events
		 WHERE source='agent_session' AND external_id=$1
	`, rangeExternalID(id, 0, 1)).Scan(&gotExt, &gotRaw, &gotSession, &gotStart, &gotEnd); err != nil {
		t.Fatal(err)
	}
	if gotExt != "ses_1:records:0:1" {
		t.Fatalf("external id = %q", gotExt)
	}
	if gotSession != "ses_1" || gotStart != 0 || gotEnd != 1 {
		t.Fatalf("range coordinates = (%q,[%d,%d)), want (ses_1,[0,1))", gotSession, gotStart, gotEnd)
	}
	var meta struct {
		Owner        string `json:"owner"`
		Client       string `json:"client"`
		Revision     string `json:"revision"`
		RewriteEpoch int64  `json:"rewrite_epoch"`
		EventKind    string `json:"event_kind"`
		ContentHash  string `json:"content_hash"`
	}
	if err := json.Unmarshal([]byte(gotRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Owner != "a@x" || meta.Client != "codex" || meta.Revision != "sha256:v1" ||
		meta.RewriteEpoch != 1 || meta.EventKind != "native_session_chunk" || meta.ContentHash != "h0" {
		t.Fatalf("raw metadata = %+v", meta)
	}

	// The sink's expand path resolves the canonical identity + range
	// from the chunk event row.
	r, err := h.sink.AgentSessionRange(ctx, eventIDForExternal(t, h.pool, rangeExternalID(id, 1, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "a@x" || r.Client != "codex" || r.Revision != "sha256:v1" ||
		r.Start != 1 || r.End != 2 || r.SessionID != "ses_1" {
		t.Fatalf("expanded range = %+v", r)
	}
}

func TestReconcileChunkEvents_NoOpReplayPreservesChunkIDsAndEmbeddedAt(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Simulate the embedder having finished: stamp embedded_at + capture
	// chunk ids before the replay.
	embeddedAt := time.Now()
	if _, err := h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_at=$1 WHERE embedded_at IS NULL`, embeddedAt); err != nil {
		t.Fatal(err)
	}
	type chunkRow struct {
		id       int64
		embedded *time.Time
	}
	before := map[string]chunkRow{}
	for _, ext := range []string{rangeExternalID(id, 0, 1), rangeExternalID(id, 1, 2)} {
		var eventID int64
		if err := h.pool.QueryRow(ctx, `SELECT id FROM events WHERE external_id=$1`, ext).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		var c chunkRow
		if err := h.pool.QueryRow(ctx, `SELECT id, embedded_at FROM event_chunks WHERE event_id=$1`, eventID).Scan(&c.id, &c.embedded); err != nil {
			t.Fatal(err)
		}
		before[ext] = c
	}

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Selection is state-derived: an already-current corpus is no work.
	if res.Upserted != 0 || res.NoChange != 0 || res.Stale != 0 || res.Tombstoned != 0 {
		t.Fatalf("replay reconcile result = %+v, want all-zero no-op", res)
	}
	for _, ext := range []string{rangeExternalID(id, 0, 1), rangeExternalID(id, 1, 2)} {
		var eventID int64
		if err := h.pool.QueryRow(ctx, `SELECT id FROM events WHERE external_id=$1`, ext).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		var c chunkRow
		if err := h.pool.QueryRow(ctx, `SELECT id, embedded_at FROM event_chunks WHERE event_id=$1`, eventID).Scan(&c.id, &c.embedded); err != nil {
			t.Fatal(err)
		}
		if c.id != before[ext].id || !c.embedded.Equal(*before[ext].embedded) {
			t.Fatalf("replay mutated chunk for %s: id %d->%d embedded_at %v->%v",
				ext, before[ext].id, c.id, *before[ext].embedded, *c.embedded)
		}
	}
}

func TestReconcileChunkEvents_RevisionReplacementInPlace(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	for _, ext := range []string{rangeExternalID(id, 0, 1), rangeExternalID(id, 1, 2)} {
		before[ext] = eventIDForExternal(t, h.pool, ext)
	}

	// A new revision replaces the summary chunks in place (same ranges,
	// new content + rewrite epoch).
	h.seedCandidate(t, id, "sha256:v2", 2, 2)
	h.publish(t, id, "sha256:v2", 2, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0b", Summary: "range zero v2", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1b", Summary: "range one v2", ModelID: "map-model"},
	})

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 2 {
		t.Fatalf("replacement reconcile = %+v, want 2 upserts", res)
	}
	n, err := h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("active range events after replacement = %d, want 2 (in place, no duplicates)", n)
	}
	for _, ext := range []string{rangeExternalID(id, 0, 1), rangeExternalID(id, 1, 2)} {
		// Same event row, same stable id (revision never in the id).
		if got := eventIDForExternal(t, h.pool, ext); got != before[ext] {
			t.Fatalf("event %s was replaced with a new row id %d (was %d); want in-place update", ext, got, before[ext])
		}
		var revision, body, kind string
		if err := h.pool.QueryRow(ctx,
			`SELECT raw->>'revision', body, raw->>'event_kind' FROM events WHERE external_id=$1`, ext).
			Scan(&revision, &body, &kind); err != nil {
			t.Fatal(err)
		}
		if revision != "sha256:v2" || kind != "native_session_chunk" {
			t.Fatalf("row for %s not replaced in place: revision=%q kind=%q", ext, revision, kind)
		}
		if body != "range zero v2" && body != "range one v2" {
			t.Fatalf("row for %s has stale body %q", ext, body)
		}
	}
}

func TestReconcileChunkEvents_ChangedRangeTombstone(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// A re-chunk collapses the two half-open ranges into one full range:
	// the old [0,1) and [1,2) events must be tombstoned, the new [0,2)
	// event upserted — the upsert ordering guarantee means the desired
	// event lands before the obsolete ones are removed.
	h.seedCandidate(t, id, "sha256:v2", 2, 2)
	h.publish(t, id, "sha256:v2", 2, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "hfull", Summary: "full range", ModelID: "map-model"},
	})

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 || res.Tombstoned != 2 {
		t.Fatalf("changed-range reconcile = %+v, want 1 upsert + 2 tombstones", res)
	}
	var active, tombstoned int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE deleted=false), count(*) FILTER (WHERE deleted=true)
		  FROM events
		 WHERE source='agent_session' AND external_id LIKE $1 || '%' ESCAPE '\'
	`, escapeLikeForTest(rangeExternalIDPrefix(id))).Scan(&active, &tombstoned); err != nil {
		t.Fatal(err)
	}
	if active != 1 || tombstoned != 2 {
		t.Fatalf("range events after changed-range: active=%d tombstoned=%d, want 1/2", active, tombstoned)
	}
	var remaining string
	if err := h.pool.QueryRow(ctx,
		`SELECT external_id FROM events WHERE source='agent_session' AND external_id LIKE $1 || '%' ESCAPE '\' AND deleted=false`,
		escapeLikeForTest(rangeExternalIDPrefix(id))).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != rangeExternalID(id, 0, 2) {
		t.Fatalf("surviving range event = %q, want %q", remaining, rangeExternalID(id, 0, 2))
	}
}

func TestExternalIDIsCanonicalSessionOnly(t *testing.T) {
	// The canonical feed session UUID alone addresses the corpus row.
	// Owner and client are provenance bytes, never part of the key, so two
	// identities carrying the same canonical UUID address the same row.
	a := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	b := Identity{Owner: "b@x", Client: "claude-code", SessionID: "ses_1"}
	if got := externalID(a); got != "ses_1:session" {
		t.Fatalf("externalID = %q, want ses_1:session", got)
	}
	if got := rangeExternalID(a, 0, 2); got != "ses_1:records:0:2" {
		t.Fatalf("rangeExternalID = %q, want ses_1:records:0:2", got)
	}
	if got := rangeExternalIDPrefix(a); got != "ses_1:records:" {
		t.Fatalf("rangeExternalIDPrefix = %q, want ses_1:records:", got)
	}
	if externalID(a) != externalID(b) || rangeExternalID(a, 0, 2) != rangeExternalID(b, 0, 2) {
		t.Fatal("canonical session UUID alone must address the corpus row regardless of owner/client")
	}
}

func TestReconcileChunkEvents_StaleCASRejects(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
	})

	// A newer revision was canonically published: the session's
	// published watermark and chunk summaries now describe v2.
	h.seedCandidate(t, id, "sha256:v2", 2, 2)
	h.publish(t, id, "sha256:v2", 2, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0v2", Summary: "range zero v2", ModelID: "map-model"},
	})

	// Publishing the superseded v1 chunk (e.g. a straggler from a
	// crashed tick) must be rejected: the corpus can never claim a
	// revision that is no longer the session's published state.
	stale := ChunkEventItem{
		Identity:        id,
		Revision:        "sha256:v1",
		RewriteEpoch:    1,
		VisibilityScope: events.ScopeInternal,
		Chunk:           SummaryRecord{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "range zero", ModelID: "map-model", OccurredAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)},
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
	}
	if _, err := h.ep.PublishChunkEvent(ctx, stale); !errors.Is(err, errChunkCASStale) {
		t.Fatalf("stale candidate publish err = %v, want errChunkCASStale", err)
	}
	n, err := h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stale CAS left %d active range events, want 0 (rolled back)", n)
	}

	// Chunk-state CAS: current revision, but the chunk row was
	// replaced (content hash + summary drifted) between selection and
	// write.
	if _, err := h.pool.Exec(ctx, `
		UPDATE session_ingest_chunk_summaries
		   SET content_hash='h0v3', summary='range zero v3'
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, id.Owner, id.Client, id.SessionID); err != nil {
		t.Fatal(err)
	}
	drifted := ChunkEventItem{
		Identity:        id,
		Revision:        "sha256:v2",
		RewriteEpoch:    2,
		VisibilityScope: events.ScopeInternal,
		Chunk:           SummaryRecord{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0v2", Summary: "range zero v2", ModelID: "map-model", OccurredAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)},
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
	}
	if _, err := h.ep.PublishChunkEvent(ctx, drifted); !errors.Is(err, errChunkCASStale) {
		t.Fatalf("drifted chunk publish err = %v, want errChunkCASStale", err)
	}
	n, err = h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("chunk-state CAS left %d active range events, want 0", n)
	}
}

func TestChunkSelection_MissingStaleObsolete(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Fully reconciled -> the level-triggered selection finds no work.
	items, err := h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("selection after full reconcile = %d items, want 0", len(items))
	}
	obs, err := h.store.SelectObsoleteRangeEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Fatalf("obsolete selection after full reconcile = %d, want 0", len(obs))
	}

	// Missing: one range event is gone (crash lost the commit side).
	if _, err := h.sink.Delete(ctx, "agent_session", rangeExternalID(id, 0, 1)); err != nil {
		t.Fatal(err)
	}
	items, err = h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Chunk.RecordStart != 0 {
		t.Fatalf("missing selection = %+v, want the [0,1) chunk", items)
	}

	// Stale: revision drifted on a live row (revision replacement).
	h.seedCandidate(t, id, "sha256:v2", 2, 2)
	if _, err := h.pool.Exec(ctx, `UPDATE events SET raw = jsonb_set(raw, '{revision}', '"sha256:v2"') WHERE external_id=$1`, rangeExternalID(id, 0, 1)); err != nil {
		t.Fatal(err)
	}
	h.publish(t, id, "sha256:v2", 2, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0v2", Summary: "range zero v2", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1v2", Summary: "range one v2", ModelID: "map-model"},
	})
	items, err = h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("stale selection = %d items, want 2 (both ranges drifted)", len(items))
	}

	// Obsolete: an extra native range event that no longer matches any
	// desired chunk (simulating a chunk that was dropped by a re-chunk).
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, actor_canonical, scope, deleted)
		VALUES ('agent_session', $1, now(), 'orphan', '{"event_kind":"native_session_chunk"}', '', 'internal', false)
	`, rangeExternalID(id, 7, 9)); err != nil {
		t.Fatal(err)
	}
	obs, err = h.store.SelectObsoleteRangeEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0] != rangeExternalID(id, 7, 9) {
		t.Fatalf("obsolete selection = %+v, want the orphan range event", obs)
	}

	// Legacy events (no event_kind) are never selected as obsolete, even
	// when their id smells like a range id.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, actor_canonical, scope, deleted)
		VALUES ('agent_session', $1, now(), 'legacy body', '{"unrelated":true}', '', 'internal', false)
	`, rangeExternalID(id, 3, 4)); err != nil {
		t.Fatal(err)
	}
	obs, err = h.store.SelectObsoleteRangeEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range obs {
		if ext == rangeExternalID(id, 3, 4) {
			t.Fatal("legacy event selected as obsolete; event_kind discriminator failed")
		}
	}
}

func TestSelectObsoleteRangeEvents_GenericPlanPreservesSelection(test *testing.T) {
	cases := []struct {
		name     string
		mutation string
		obsolete bool
	}{
		{name: "current"},
		{name: "missing_event_metadata", mutation: `UPDATE events SET session_id=NULL, record_start=NULL, record_end=NULL WHERE external_id=$1 || ':records:0:1'`},
		{name: "excluded", mutation: `UPDATE session_ingest_candidates SET excluded_at=now() WHERE session_id=$1`, obsolete: true},
		{name: "deleted", mutation: `UPDATE session_ingest_candidates SET deleted=true WHERE session_id=$1`, obsolete: true},
		{name: "unpublished", mutation: `UPDATE session_ingest_candidates SET published_revision='' WHERE session_id=$1`, obsolete: true},
		{name: "empty_summary", mutation: `UPDATE session_ingest_chunk_summaries SET summary='' WHERE session_id=$1`, obsolete: true},
		{name: "removed_summary", mutation: `DELETE FROM session_ingest_chunk_summaries WHERE session_id=$1`, obsolete: true},
		{name: "missing_candidate", mutation: `DELETE FROM session_ingest_candidates WHERE session_id=$1`, obsolete: true},
	}
	for _, testcase := range cases {
		test.Run(testcase.name, func(test *testing.T) {
			harness := newChunkHarness(test)
			ctx := context.Background()
			identity := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
			harness.seedCandidate(test, identity, "sha256:v1", 1, 1)
			harness.publish(test, identity, "sha256:v1", 1, []SummaryRecord{{
				Identity: identity, RecordStart: 0, RecordEnd: 1,
				ContentHash: "h0", Summary: "current range", ModelID: "map-model",
			}})
			if _, err := harness.ep.ReconcileChunkEvents(ctx, 100); err != nil {
				test.Fatal(err)
			}
			if testcase.mutation != "" {
				if _, err := harness.pool.Exec(ctx, testcase.mutation, identity.SessionID); err != nil {
					test.Fatal(err)
				}
			}
			orphanID := rangeExternalID(identity, 7, 9)
			legacyID := rangeExternalID(identity, 3, 4)
			if _, err := harness.pool.Exec(ctx, `
				INSERT INTO events (source, external_id, occurred_at, body, raw, actor_canonical, scope, deleted)
				VALUES ('agent_session', $1, now(), 'orphan', '{"event_kind":"native_session_chunk"}', '', 'internal', false),
				       ('agent_session', $2, now(), 'legacy', '{"legacy":true}', '', 'internal', false)
			`, orphanID, legacyID); err != nil {
				test.Fatal(err)
			}
			config := harness.pool.Config()
			config.MaxConns = 1
			config.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_generic_plan"
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(pool.Close)
			store := NewStore(pool)
			expected := []string{orphanID}
			if testcase.obsolete {
				expected = append([]string{rangeExternalID(identity, 0, 1)}, expected...)
			}
			selected, err := store.SelectObsoleteRangeEvents(ctx, 100)
			if err != nil {
				test.Fatal(err)
			}
			if len(selected) != len(expected) {
				test.Fatalf("obsolete selection = %v, want %v", selected, expected)
			}
			for index, externalID := range expected {
				if selected[index] != externalID {
					test.Fatalf("obsolete selection = %v, want %v", selected, expected)
				}
			}
			limited, err := store.SelectObsoleteRangeEvents(ctx, 1)
			if err != nil {
				test.Fatal(err)
			}
			if len(limited) != 1 || limited[0] != expected[0] {
				test.Fatalf("limited selection = %v, want %v", limited, expected[:1])
			}
			var statementName string
			if err := pool.QueryRow(ctx, `SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`, "%WITH desired AS%ORDER BY e.id%").Scan(&statementName); err != nil {
				test.Fatal(err)
			}
			planRows, err := pool.Query(ctx, "EXPLAIN EXECUTE "+pgx.Identifier{statementName}.Sanitize()+"(100)")
			if err != nil {
				test.Fatal(err)
			}
			defer planRows.Close()
			var plan strings.Builder
			for planRows.Next() {
				var line string
				if err := planRows.Scan(&line); err != nil {
					test.Fatal(err)
				}
				plan.WriteString(line + "\n")
			}
			if err := planRows.Err(); err != nil {
				test.Fatal(err)
			}
			if !strings.Contains(plan.String(), "hashed SubPlan") {
				test.Fatalf("generic cleanup plan does not use hashed membership:\n%s", plan.String())
			}
		})
	}
}

func TestReconcileChunkEvents_CrashRecoveryNextTick(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
		{Identity: id, RecordStart: 2, RecordEnd: 3, ContentHash: "h2", Summary: "range two", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Crash window: the previous tick committed only SOME range events
	// before dying. The level-triggered reconciler must rebuild exactly
	// the missing one on the next tick, leaving the rest untouched.
	if _, err := h.sink.Delete(ctx, "agent_session", rangeExternalID(id, 1, 2)); err != nil {
		t.Fatal(err)
	}
	var keptEventID int64
	if err := h.pool.QueryRow(ctx, `SELECT id FROM events WHERE external_id=$1`, rangeExternalID(id, 0, 1)).Scan(&keptEventID); err != nil {
		t.Fatal(err)
	}

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("recovery reconcile = %+v, want 1 upsert", res)
	}
	n, err := h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("active range events after recovery = %d, want 3", n)
	}
	if eventIDForExternal(t, h.pool, rangeExternalID(id, 0, 1)) != keptEventID {
		t.Fatal("recovery touched an already-published range event")
	}
}

func TestTombstone_CoversSessionAndRangeEvents(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "range one", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	if err := h.ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	var sessionActive, sessionTotal int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE deleted=false), count(*) FROM events WHERE external_id=$1`,
		externalID(id)).Scan(&sessionActive, &sessionTotal); err != nil {
		t.Fatal(err)
	}
	if sessionActive != 0 || sessionTotal != 1 {
		t.Fatalf(":session event after tombstone: active=%d total=%d, want 0/1", sessionActive, sessionTotal)
	}
	var active, tombstoned int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE deleted=false), count(*) FILTER (WHERE deleted=true)
		  FROM events
		 WHERE source='agent_session' AND external_id LIKE $1 || '%' ESCAPE '\'
	`, escapeLikeForTest(rangeExternalIDPrefix(id))).Scan(&active, &tombstoned); err != nil {
		t.Fatal(err)
	}
	if active != 0 || tombstoned != 2 {
		t.Fatalf("range events after tombstone: active=%d tombstoned=%d, want 0/2", active, tombstoned)
	}

	// Tombstone is idempotent.
	if err := h.ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func eventIDForExternal(t *testing.T, pool *pgxpool.Pool, ext string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM events WHERE source='agent_session' AND external_id=$1`, ext).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReconcileChunkEvents_BoundedPerTick(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	chunks := []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 1, ContentHash: "h0", Summary: "chunk zero", ModelID: "map-model"},
		{Identity: id, RecordStart: 1, RecordEnd: 2, ContentHash: "h1", Summary: "chunk one", ModelID: "map-model"},
		{Identity: id, RecordStart: 2, RecordEnd: 3, ContentHash: "h2", Summary: "chunk two", ModelID: "map-model"},
	}
	h.seedCandidate(t, id, "sha256:v1", 1, 3)
	h.publish(t, id, "sha256:v1", 1, chunks)

	for tick, wantTotal := range []int{1, 2, 3} {
		res, err := h.ep.ReconcileChunkEvents(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Upserted != 1 {
			t.Fatalf("tick %d (limit 1) = %+v, want exactly 1 upsert", tick, res)
		}
		n, err := h.countActiveRangeEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if n != wantTotal {
			t.Fatalf("after tick %d active range events = %d, want %d", tick, n, wantTotal)
		}
	}
	// A wider-limit pass over an already-current corpus is a no-op.
	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 0 || res.Tombstoned != 0 {
		t.Fatalf("wide no-op pass = %+v, want zero work", res)
	}
}

func TestWorker_TickReconcilesNativeChunkEvents(t *testing.T) {
	store, pool := newWorkerTestStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		TRUNCATE events, session_ingest_summaries, session_ingest_chunk_summaries RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ep := NewEventPublisher(sink, store)

	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{
				{ChangeSeq: 1, SessionID: "s1",
					Owner: "alex@example.com", OwnerKind: "human",
					Client: "codex", Role: "primary", OriginApp: "codex",
					Revision: "sha256:v1", RewriteEpoch: 1,
					RecordCount: 20, UserTurnCount: 5, ContentBytes: 50000,
					SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
					SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
					UpdatedAt:       time.Now().Add(-30 * time.Minute), Complete: true},
			}},
		},
		revision: map[string][]byte{"s1|sha256:v1": validRevisionBody("s1", "sha256:v1")},
	}
	w := &Worker{
		Cfg:       WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store:     store,
		Feed:      feed,
		Summarize: staticSummarizer{},
		Publish:   ep,
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	id := Identity{Owner: "alex@example.com", Client: "codex", SessionID: "s1"}
	var sessionActive, rangeActive int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE external_id=$1 AND deleted=false),
		       count(*) FILTER (WHERE raw->>'event_kind'='native_session_chunk' AND deleted=false)
		  FROM events
		 WHERE source='agent_session' AND session_id='s1'
	`, externalID(id)).Scan(&sessionActive, &rangeActive); err != nil {
		t.Fatal(err)
	}
	if sessionActive != 1 || rangeActive != 1 {
		t.Fatalf("worker tick corpus: session events=%d range events=%d, want 1/1", sessionActive, rangeActive)
	}
	// The reconciler ran inside the same tick, level-triggered off the
	// summary state the publish just wrote.
	var ext string
	if err := pool.QueryRow(ctx, `
		SELECT external_id FROM events
		 WHERE source='agent_session' AND raw->>'event_kind'='native_session_chunk'
	`).Scan(&ext); err != nil {
		t.Fatal(err)
	}
	if ext != rangeExternalID(id, 0, 1) {
		t.Fatalf("published range event = %q, want %q", ext, rangeExternalID(id, 0, 1))
	}
}

func TestTombstone_LeavesLegacyEventsUntouched(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "full", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// A pre-existing legacy event for the same session (no event_kind)
	// must survive the whole-session tombstone untouched.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, actor_canonical, scope, deleted)
		VALUES ('agent_session', $1, now(), 'legacy body', '{"legacy":true}', '', 'internal', false)
	`, externalID(id)+".legacy"); err != nil {
		t.Fatal(err)
	}

	if err := h.ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	var legacyActive bool
	if err := h.pool.QueryRow(ctx, `SELECT deleted FROM events WHERE external_id=$1`, externalID(id)+".legacy").Scan(&legacyActive); err != nil {
		t.Fatal(err)
	}
	if legacyActive {
		t.Fatal("whole-session tombstone touched a legacy event")
	}
}

func TestChunkSelection_StaleRawMetadataRepaired(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "range zero", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	ext := rangeExternalID(id, 0, 2)
	eventID := eventIDForExternal(t, h.pool, ext)

	// Raw metadata drifts (content_hash + model_id) while the summary
	// body, revision, rewrite epoch, and range coordinates stay
	// identical — exactly the drift a body-only comparison misses.
	if _, err := h.pool.Exec(ctx, `
		UPDATE events
		   SET raw = jsonb_set(jsonb_set(raw, '{content_hash}', '"h0-drift"'), '{model_id}', '"stale-model"')
		 WHERE external_id = $1
	`, ext); err != nil {
		t.Fatal(err)
	}

	items, err := h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Chunk.ContentHash != "h0" || items[0].Chunk.ModelID != "map-model" {
		t.Fatalf("stale raw metadata not selected for repair: %+v", items)
	}

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("repair reconcile = %+v, want 1 upsert", res)
	}
	// Same row (in place), repaired metadata, body untouched.
	if got := eventIDForExternal(t, h.pool, ext); got != eventID {
		t.Fatalf("repair replaced row id %d -> %d; want in-place update", eventID, got)
	}
	var hash, model, body string
	if err := h.pool.QueryRow(ctx,
		`SELECT raw->>'content_hash', raw->>'model_id', body FROM events WHERE external_id=$1`, ext).
		Scan(&hash, &model, &body); err != nil {
		t.Fatal(err)
	}
	if hash != "h0" || model != "map-model" || body != "range zero" {
		t.Fatalf("repaired row: hash=%q model=%q body=%q, want h0/map-model/range zero", hash, model, body)
	}
	remaining, err := h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("selection after repair = %d items, want 0", len(remaining))
	}
}

func TestTombstone_PreservesLegacyRangeShapedEvent(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "full", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// A legacy, untagged event whose external_id EXACTLY matches the
	// native range id shape (raw has no event_kind). The whole-session
	// tombstone's pattern removal is scoped to event_kind =
	// native_session_chunk, so this row must survive untouched.
	legacyExt := rangeExternalID(id, 7, 9)
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, actor_canonical, scope, deleted)
		VALUES ('agent_session', $1, now(), 'legacy body', '{"legacy":true}', '', 'internal', false)
	`, legacyExt); err != nil {
		t.Fatal(err)
	}

	if err := h.ep.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	var legacyDeleted bool
	if err := h.pool.QueryRow(ctx, `SELECT deleted FROM events WHERE external_id=$1`, legacyExt).Scan(&legacyDeleted); err != nil {
		t.Fatal(err)
	}
	if legacyDeleted {
		t.Fatal("whole-session tombstone deleted an untagged legacy event with a range-shaped id")
	}
	var nativeActive bool
	if err := h.pool.QueryRow(ctx, `SELECT deleted FROM events WHERE external_id=$1`, rangeExternalID(id, 0, 2)).Scan(&nativeActive); err != nil {
		t.Fatal(err)
	}
	if !nativeActive {
		t.Fatal("native range event should have been tombstoned")
	}
}

func TestReconcileChunkEvents_PendingObservedEpochIgnored(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "full", ModelID: "map-model"},
	})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	ext := rangeExternalID(id, 0, 2)
	eventID := eventIDForExternal(t, h.pool, ext)
	var epochBefore string
	if err := h.pool.QueryRow(ctx, `SELECT raw->>'rewrite_epoch' FROM events WHERE external_id=$1`, ext).Scan(&epochBefore); err != nil {
		t.Fatal(err)
	}

	// A newer, observed-but-unpublished head advances the candidate's
	// rewrite epoch. Published chunk events must keep pairing the
	// published revision with the PUBLISHED epoch — the pending epoch
	// must not leak into old range events.
	h.seedCandidate(t, id, "sha256:v2", 2, 2)

	res, err := h.ep.ReconcileChunkEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 0 || res.NoChange != 0 || res.Tombstoned != 0 {
		t.Fatalf("reconcile with pending observed epoch = %+v, want zero work", res)
	}
	if got := eventIDForExternal(t, h.pool, ext); got != eventID {
		t.Fatalf("pending epoch pass replaced row id %d -> %d; want untouched", eventID, got)
	}
	var epochAfter, revisionAfter string
	if err := h.pool.QueryRow(ctx, `SELECT raw->>'rewrite_epoch', raw->>'revision' FROM events WHERE external_id=$1`, ext).
		Scan(&epochAfter, &revisionAfter); err != nil {
		t.Fatal(err)
	}
	if epochAfter != "1" || revisionAfter != "sha256:v1" {
		t.Fatalf("published event contaminated by pending head: epoch=%q revision=%q", epochAfter, revisionAfter)
	}
	items, err := h.store.SelectChunkEventsToPublish(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("selection with pending observed epoch = %d items, want 0", len(items))
	}
}

func TestReconcileChunkEvents_StaleCASEpochAndModelRejected(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	id := Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}
	h.seedCandidate(t, id, "sha256:v1", 1, 2)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{
		{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "full", ModelID: "map-model"},
	})
	base := ChunkEventItem{
		Identity:        id,
		Revision:        "sha256:v1",
		RewriteEpoch:    1,
		VisibilityScope: events.ScopeInternal,
		Chunk:           SummaryRecord{Identity: id, RecordStart: 0, RecordEnd: 2, ContentHash: "h0", Summary: "full", ModelID: "map-model", OccurredAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)},
		SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
	}

	// Candidate's published watermark is (v1, epoch 1): an item stamped
	// with the same revision but a different rewrite epoch must fail the
	// CAS.
	epochItem := base
	epochItem.RewriteEpoch = 2
	if _, err := h.ep.PublishChunkEvent(ctx, epochItem); !errors.Is(err, errChunkCASStale) {
		t.Fatalf("epoch-drifted publish err = %v, want errChunkCASStale", err)
	}
	// Selected model must match the persisted chunk row's model_id.
	modelItem := base
	modelItem.Chunk.ModelID = "other-model"
	if _, err := h.ep.PublishChunkEvent(ctx, modelItem); !errors.Is(err, errChunkCASStale) {
		t.Fatalf("model-drifted publish err = %v, want errChunkCASStale", err)
	}
	// Both rejections rolled back: no range event may have landed.
	n, err := h.countActiveRangeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stale CAS left %d active range events, want 0", n)
	}
}
