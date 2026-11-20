package sessioningest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/events"
)

// EventPublisher renders a validated Bsky Session v1 revision into the
// repository's canonical searchable corpus through the events sink,
// keyed on canonical identity. The corpus write and the ledger
// compare-and-swap are fused into ONE events transaction, so a stale
// fetch can never replace the visible current summary.
type EventPublisher struct {
	Sink  *events.DBSink
	Store *Store
	// ViewerBaseURL is the public host used to build session deep-link
	// URLs on published chunk events (the same link the final :session
	// event carries, since every range belongs to the same session).
	// Empty = URL omitted. Set by the worker wiring; the worker's own
	// per-publish viewerBaseURL parameter covers the atomic
	// PublishSession path.
	ViewerBaseURL string
}

// NewEventPublisher wires the corpus publisher to the events sink and
// the sessioningest store (the latter provides the in-transaction
// ledger CAS). store must be non-nil for PublishSession (atomic CAS).
func NewEventPublisher(sink *events.DBSink, store *Store) *EventPublisher {
	return &EventPublisher{Sink: sink, Store: store}
}

// externalID is the stable events.external_id for a session's corpus
// event. The canonical feed session UUID alone addresses the corpus
// row; owner and harness are provenance bytes on the event, never part
// of the addressing key.
func externalID(id Identity) string {
	return id.SessionID + ":session"
}

// chunkEventKind is the raw.event_kind tag on every native
// map-summary range event published by the reconciler. It is the
// deterministic discriminator the reconciler uses to identify its own
// events (for stale selection and obsolete tombstoning) without
// touching the canonical :session event or any legacy event.
const chunkEventKind = "native_session_chunk"

// rangeExternalID is the stable events.external_id for one native
// map-summary chunk event. The revision is deliberately NOT part of
// the id: a range belongs to the session, not to a revision, so a new
// revision replaces the same range row in place (and an unchanged
// range re-emit is a NoChange that preserves chunk ids / embedded_at).
func rangeExternalID(id Identity, start, end int) string {
	return fmt.Sprintf("%s:records:%d:%d", id.SessionID, start, end)
}

// rangeExternalIDPrefix is the external_id prefix that matches every
// native range event of one identity (used by the whole-session
// tombstone to cover the final :session event AND all range events).
func rangeExternalIDPrefix(id Identity) string {
	return id.SessionID + ":records:"
}

// sessionURL builds the viewer deep link for a session.
func sessionURL(viewerBaseURL string, id Identity) string {
	if viewerBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/sessions/%s", strings.TrimRight(viewerBaseURL, "/"), url.PathEscape(id.SessionID))
}

func sessionRangeURL(viewerBaseURL string, id Identity, start, end int) string {
	base := sessionURL(viewerBaseURL, id)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s?start=%d&end=%d", base, start, end)
}

// PublishResult reports the atomic publish outcome.
type PublishResult struct {
	Published     bool // true if a genuinely-new revision was canonically published
	Stale         bool // true if the fetched revision was not the session's current candidate (CAS rejected)
	SummaryReused bool // true when publication skipped inference via an exact input digest
	ReduceEntries int  // summaries the reduce call read; zero when the summary was reused
}

// PublishSession renders one validated Bsky Session v1 document into
// the corpus and advances the session's published ledger in a SINGLE
// events transaction. The events sink upsert writes the session event
// (canonical replacement on (source, external_id)); its in-transaction
// side write runs PublishRevisionTx, which compares-and-swaps that the
// revision the document was fetched for is still the session's current
// candidate. If the CAS rejects (a newer head won), the side write
// returns an error and the whole events transaction rolls back — so a
// stale fetch can never change the visible corpus, even transiently.
//
// body is the repository-native summary content (the actual summarizer
// never publishes a raw transcript as if it were the summary); a
// transformed embedding string may also be supplied.
func (p *EventPublisher) PublishSession(ctx context.Context, doc *sessionDocument, id Identity, viewerBaseURL, revision string, rewriteEpoch int64, contentDigest, visibilityScope string, summary SummaryResult) (PublishResult, error) {
	if p.Sink == nil {
		return PublishResult{}, fmt.Errorf("sessioningest: sink not configured")
	}
	if p.Store == nil {
		return PublishResult{}, fmt.Errorf("sessioningest: store not configured for atomic publish")
	}
	sourceStartedAt, sourceUpdatedAt := doc.StartedAt, doc.UpdatedAt
	if sourceStartedAt.IsZero() {
		return PublishResult{}, fmt.Errorf("sessioningest: missing source started_at")
	}
	if sourceUpdatedAt.IsZero() || sourceUpdatedAt.Before(sourceStartedAt) {
		return PublishResult{}, fmt.Errorf("sessioningest: invalid source updated_at")
	}
	chunks := append([]SummaryRecord(nil), summary.Chunks...)
	for chunkIndex := range chunks {
		chunk := &chunks[chunkIndex]
		if chunk.RecordStart < 0 || chunk.RecordEnd <= chunk.RecordStart || chunk.RecordEnd > len(doc.RecordTimes) {
			return PublishResult{}, fmt.Errorf("sessioningest: invalid summary range [%d,%d) for %d record timestamps", chunk.RecordStart, chunk.RecordEnd, len(doc.RecordTimes))
		}
		chunk.OccurredAt = time.Time{}
		for _, recordTime := range doc.RecordTimes[chunk.RecordStart:chunk.RecordEnd] {
			if recordTime.After(chunk.OccurredAt) {
				chunk.OccurredAt = recordTime
			}
		}
	}
	actor := doc.Owner
	if actor == "" {
		actor = "unknown"
	}
	eventTitle := strings.TrimSpace(summary.Title)
	if eventTitle == "" {
		eventTitle = "session " + id.SessionID
	}
	raw := map[string]any{
		"event_kind":        "native_session",
		"owner":             id.Owner,
		"client":            id.Client,
		"revision":          revision,
		"rewrite_epoch":     rewriteEpoch,
		"model_id":          summary.ModelID,
		"tags":              summary.Tags,
		"record_count":      doc.RecordCount,
		"chunk_count":       len(summary.Chunks),
		"source_started_at": sourceStartedAt.UTC().Format(time.RFC3339Nano),
		"source_updated_at": sourceUpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if doc.DecoderVersion != "" {
		// A pushed session's citations name the materialization they were
		// built from. Feed events keep their historical raw bytes.
		raw["decoder_version"] = doc.DecoderVersion
	}
	ev := events.Event{
		Source:         "agent_session",
		ExternalID:     externalID(id),
		OccurredAt:     sourceUpdatedAt,
		Actor:          actor,
		ActorID:        actor,
		ActorCanonical: actor,
		Title:          eventTitle,
		Body:           summary.Narrative,
		EmbeddingText:  summary.EmbeddingText,
		URL:            sessionURL(viewerBaseURL, id),
		ContextPrefix:  summary.ContextHint,
		SessionID:      id.SessionID,
		RecordStart:    0,
		RecordEnd:      doc.RecordCount,
		Scope:          visibilityScope,
		Raw:            mustSummaryJSON(raw),
	}
	var res PublishResult
	_, err := p.Sink.UpsertWithSideWrite(ctx, ev, func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		r, err := p.Store.PublishRevisionTx(ctx, tx, id, revision, rewriteEpoch,
			contentDigest, sourceStartedAt, sourceUpdatedAt, visibilityScope)
		if errors.Is(err, errCASStale) {
			res.Stale = true
			return errCASStale
		}
		if err != nil {
			return err
		}
		if err := p.Store.ReplaceSummaryTx(ctx, tx, SessionSummaryDoc{
			Identity: id, Revision: revision, RewriteEpoch: rewriteEpoch,
			InputDigest: summary.InputDigest,
			Narrative:   summary.Narrative, EmbeddingText: summary.EmbeddingText,
			Title: summary.Title, TitleModel: summary.TitleModel,
			ContextHint: summary.ContextHint, Tags: summary.Tags, ModelID: summary.ModelID,
			InputTokens: summary.InputTokens, OutputTokens: summary.OutputTokens,
			RecordCount: doc.RecordCount,
		}, chunks); err != nil {
			return err
		}
		if err := tombstoneLegacySessionTx(ctx, tx, id, revision); err != nil {
			return err
		}
		res.Published = r.Published
		return nil
	})
	if err != nil {
		if errors.Is(err, errCASStale) {
			return PublishResult{Stale: true}, nil
		}
		return PublishResult{}, err
	}
	return res, nil
}

func mustSummaryJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Tombstone removes a session's corpus events from the searchable
// surface: the canonical final :session event AND every native range
// event the reconciler published for it. Idempotent (missing events =
// no-op). Called when a head is soft-deleted or newly excluded by a
// policy change.
//
// The removals are separate sink transactions. The sequence is safe because the
// candidate's corpus_tombstone_pending marker (set in the same tx as
// the exclusion) makes the whole-session tombstone durably retried on
// every discovery pass until the native removals have run. Untagged
// (legacy) events under a session's external_id prefix are deliberately
// left alone: only the event_kind-tagged native rows are removed.
func (p *EventPublisher) Tombstone(ctx context.Context, id Identity) error {
	if p.Sink == nil {
		return nil
	}
	if _, err := p.Sink.Delete(ctx, "agent_session", externalID(id)); err != nil {
		return err
	}
	_, err := p.Sink.DeletePattern(ctx, "agent_session", rangeExternalIDPrefix(id), chunkEventKind)
	return err
}

func tombstoneLegacySessionTx(ctx context.Context, tx pgx.Tx, id Identity, revision string) error {
	// This is corpus supersession, not a user/privacy deletion. Preserve the
	// exact old event and every derived chunk in immutable archive tables before
	// removing the chunks from the active embed queue. If the opaque session ID
	// ever maps to more than one canonical owner/client, retain the legacy row
	// rather than guessing which identity owns it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO legacy_session_event_supersessions
		       (event_id, owner, client, session_id, revision, event_snapshot)
		SELECT e.id, $1, $2, $3, $4, to_jsonb(e)
		  FROM events e
		 WHERE e.source='agent_session'
		   AND e.session_id=$3
		   AND e.deleted=false
		   AND COALESCE(e.raw->>'event_kind', '') NOT IN ('native_session', 'native_session_chunk')
		   AND NOT (e.raw ? 'owner' AND e.raw ? 'client' AND e.raw ? 'revision')
		   AND 1 = (
		       SELECT count(DISTINCT (p.owner, p.client))
		         FROM session_ingest_published p
		        WHERE p.session_id=$3
		   )
		 ON CONFLICT (event_id) DO NOTHING`, id.Owner, id.Client, id.SessionID, revision); err != nil {
		return fmt.Errorf("archive legacy session events: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO legacy_session_event_chunk_archive (chunk_id, event_id, chunk_snapshot)
		SELECT c.id, c.event_id, to_jsonb(c)
		  FROM event_chunks c
		  JOIN legacy_session_event_supersessions a ON a.event_id=c.event_id
		 WHERE a.owner=$1 AND a.client=$2 AND a.session_id=$3 AND a.revision=$4
		 ON CONFLICT (chunk_id) DO NOTHING`, id.Owner, id.Client, id.SessionID, revision); err != nil {
		return fmt.Errorf("archive legacy session chunks: %w", err)
	}

	rows, err := tx.Query(ctx, `
		UPDATE events e
		   SET deleted=true, updated_at=now()
		 WHERE e.id IN (
		       SELECT a.event_id
		         FROM legacy_session_event_supersessions a
		        WHERE a.owner=$1 AND a.client=$2 AND a.session_id=$3 AND a.revision=$4
		   )
		   AND e.deleted=false
		 RETURNING e.id`, id.Owner, id.Client, id.SessionID, revision)
	if err != nil {
		return fmt.Errorf("tombstone legacy session events: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan legacy session event: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan legacy session events: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `DELETE FROM event_chunks WHERE event_id=$1`, id); err != nil {
			return fmt.Errorf("delete legacy session chunks: %w", err)
		}
	}
	return nil
}

// ChunkReconcileResult reports one bounded reconciler pass.
type ChunkReconcileResult struct {
	Upserted   int // chunk events inserted or updated in place
	NoChange   int // desired chunk events the sink already had current
	Stale      int // chunk events whose CAS rejected (concurrent state change)
	Tombstoned int // obsolete native range events removed
}

// PublishChunkEvent renders one persisted native map-summary chunk
// (a session_ingest_chunk_summaries row) into the corpus as an
// independently searchable agent_session range event. The external
// id is stable per record range (revision never in the id), raw is
// deterministic (canonical owner/client/revision + event_kind +
// content identity), and the events row carries the range expansion
// coordinates (session_id / record_start / record_end) so the retrieve
// expand path proxies the exact record range to the session feed.
//
// The events sink's own dirty-check makes a replay of identical state
// a NoChange (chunk ids + embedded_at preserved, no re-embedding).
// The in-transaction side write runs AssertChunkCurrentTx, which
// compares-and-swaps against the CURRENT candidate + chunk state: if
// the session was excluded, its published revision advanced, or the
// chunk row was replaced between selection and write, the CAS rejects
// and the whole events transaction rolls back — a superseded revision
// can never publish (or resurrect) chunk content.
func (p *EventPublisher) PublishChunkEvent(ctx context.Context, item ChunkEventItem) (events.UpsertResult, error) {
	if p.Sink == nil {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: sink not configured")
	}
	if p.Store == nil {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: store not configured for chunk publication")
	}
	if item.Revision == "" {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: chunk publication requires a published revision")
	}
	c := item.Chunk
	if c.Summary == "" {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: chunk publication requires a non-empty summary")
	}
	if c.RecordEnd <= c.RecordStart {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: invalid chunk range [%d,%d)", c.RecordStart, c.RecordEnd)
	}
	if item.SourceUpdatedAt.IsZero() {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: chunk publication requires source updated_at")
	}
	if c.OccurredAt.IsZero() {
		return events.UpsertResult{}, fmt.Errorf("sessioningest: chunk publication requires record time")
	}
	ev := events.Event{
		Source:         "agent_session",
		ExternalID:     rangeExternalID(item.Identity, c.RecordStart, c.RecordEnd),
		OccurredAt:     c.OccurredAt,
		Actor:          item.Owner,
		ActorID:        item.Owner,
		ActorCanonical: item.Owner,
		Title:          fmt.Sprintf("session %s records [%d,%d)", item.SessionID, c.RecordStart, c.RecordEnd),
		Body:           c.Summary,
		EmbeddingText:  c.Summary,
		URL:            sessionRangeURL(p.ViewerBaseURL, item.Identity, c.RecordStart, c.RecordEnd),
		ContextPrefix:  fmt.Sprintf("session %s records [%d,%d)", item.SessionID, c.RecordStart, c.RecordEnd),
		SessionID:      item.SessionID,
		RecordStart:    c.RecordStart,
		RecordEnd:      c.RecordEnd,
		Scope:          item.VisibilityScope,
		Raw: mustSummaryJSON(map[string]any{
			"owner":             item.Owner,
			"client":            item.Client,
			"revision":          item.Revision,
			"rewrite_epoch":     item.RewriteEpoch,
			"event_kind":        chunkEventKind,
			"content_hash":      c.ContentHash,
			"model_id":          c.ModelID,
			"source_updated_at": item.SourceUpdatedAt.UTC().Format(time.RFC3339Nano),
		}),
	}
	return p.Sink.UpsertWithSideWrite(ctx, ev, func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		return p.Store.AssertChunkCurrentTx(ctx, tx, item.Identity, item.Revision,
			item.RewriteEpoch, item.SourceUpdatedAt, item.VisibilityScope, c)
	})
}

// ReconcileChunkEvents is the bounded, level-triggered publication
// pass for native map-summary chunk events. Each tick it:
//
//  1. Selects up to limit desired chunk events whose events row is
//     missing, previously tombstoned, or stale (event_kind, revision,
//     content hash, range coordinates, or summary body drifted from
//     the current candidate/summary state).
//  2. Upserts each in place via the CAS'd PublishChunkEvent (the
//     sink's dirty-check turns already-current rows into NoChange).
//  3. Then tombstones up to the remaining budget of native range
//     events that no longer correspond to any desired chunk of their
//     session (changed boundaries, removed chunks, or a session that
//     stopped being publishable).
//
// Selection is derived purely from durable state every tick (level
// triggered — no queue, no cursor), so a crash or stale CAS mid-pass
// leaves the remainder selected again on the next tick and the pass
// converges. The canonical final :session event is never touched, and
// legacy events are never selected.
func (p *EventPublisher) ReconcileChunkEvents(ctx context.Context, limit int) (ChunkReconcileResult, error) {
	if p.Sink == nil {
		return ChunkReconcileResult{}, fmt.Errorf("sessioningest: sink not configured")
	}
	if p.Store == nil {
		return ChunkReconcileResult{}, fmt.Errorf("sessioningest: store not configured for chunk reconciliation")
	}
	if limit <= 0 {
		limit = 100
	}
	var res ChunkReconcileResult

	items, err := p.Store.SelectChunkEventsToPublish(ctx, limit)
	if err != nil {
		return res, err
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		ur, err := p.PublishChunkEvent(ctx, item)
		switch {
		case errors.Is(err, errChunkCASStale):
			res.Stale++
		case err != nil:
			// Abort the tick; next tick re-selects the remainder
			// (nothing here depends on a cursor or queue).
			return res, err
		case ur.NoChange:
			res.NoChange++
		default:
			res.Upserted++
		}
	}

	// Upserts must land before obsolete range events are tombstoned:
	// a range never tombstoned while its current content still needs
	// to be visible.
	remaining := limit - len(items)
	if remaining <= 0 {
		return res, nil
	}
	obsolete, err := p.Store.SelectObsoleteRangeEvents(ctx, remaining)
	if err != nil {
		return res, err
	}
	for _, ext := range obsolete {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if _, err := p.Sink.Delete(ctx, "agent_session", ext); err != nil {
			return res, err
		}
		res.Tombstoned++
	}
	return res, nil
}
