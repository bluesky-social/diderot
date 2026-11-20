// Package events is the write path every diderot connector flows
// through. It takes a logical Event (one Slack message, one Linear
// issue, one agent session summary), normalizes it into the Postgres `events`
// + `event_chunks` tables. A database trigger durably queues Qdrant point
// deletes whenever chunk rows are removed.
//
// The design lives in docs/design.md § Data model and § Qdrant sync.
// Key invariants:
//
//   - Upsert is the only write entry point. Conflict on
//     UNIQUE(source, external_id) means "this is an edit": UPDATE the
//     row, throw away its existing chunk rows, re-chunk the new body,
//     and insert fresh chunk rows with embedded_at NULL so the
//     embedder picks them up. All of that in one Postgres tx.
//
//   - Delete tombstones by setting deleted=true, body="", title=NULL and
//     removing chunk rows in the same transaction. The delete trigger queues
//     every old point id atomically, including rows removed by FK cascades.
//
//   - Upsert is otherwise last-writer-wins: it trusts its caller's Event as
//     the latest truth and overwrites accordingly. A caller whose Event was
//     built from an earlier read of the row (not freshly fetched upstream)
//     must set Event.CheckUnchangedSince/UnchangedSince to that read's
//     updated_at, or a commit that landed after the read — an edit, a
//     delete, a scope change — gets silently overwritten with stale
//     content (reembed.go).
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/chunk"
)

// Event is the connector-facing shape. Every field from the `events`
// row shape except bookkeeping (id, deleted, updated_at) shows up
// here; those are sink-owned.
type Event struct {
	// Source identifies which connector produced the event. Matches
	// events.source: "slack", "linear", "notion". Must be non-empty.
	Source string
	// ExternalID is the upstream-native id: slack channel+ts, a
	// Linear issue id, a Notion page id. Must be stable across edits of
	// the same logical event — that's what lets Upsert find the
	// existing row. UNIQUE(source, external_id) enforces non-null.
	ExternalID string
	// OccurredAt is when the event happened in the source system.
	// NOT NULL in the schema.
	OccurredAt time.Time
	// Actor is the human-readable speaker: a Slack handle, a Linear
	// display name, a session owner's email. Optional.
	Actor string
	// ActorCanonical is the cross-source unified identity key for
	// the speaker. Email when known (any source); for slack rows
	// where the user has no email it's "slack:<user_id>". Same
	// canonical key matches the same person regardless of which
	// source the row came from. Used by the retrieve filter
	// widening (matches actor_canonical OR actor) so the model
	// can issue actors=[asker.email] and pick up cross-source rows
	// without knowing each source's per-source convention.
	//
	// Empty string is allowed; the events table column is NOT NULL
	// DEFAULT '' so a missing canonical degrades to "match by
	// display name only" — same retrieve behavior as today.
	ActorCanonical string
	// ActorID is the opaque upstream id of the speaker: a Slack or
	// Discord user id, a Linear user id. Optional.
	ActorID string
	// ThreadID groups related events: slack thread_ts, a Discord
	// thread id. Optional.
	ThreadID string
	// Title is a short summary for display and citation: a Slack
	// channel name, a Linear issue's "KEY-1: title", a page title.
	// Optional (nullable in schema).
	Title string
	// Body is the full searchable text — what gets chunked + embedded.
	// NOT NULL in the schema. For events with no text (rare; maybe a
	// reaction-only event) use " " or synthesize something meaningful.
	Body string
	// URL is a canonical link back to the source (a Slack permalink,
	// an issue or page URL). Optional.
	URL string
	// Raw is the source-native payload as JSON, stored as jsonb for
	// debugging and edit-history reconstruction. Callers are
	// responsible for pre-marshalling; passing nil means "store
	// null" which violates the schema NOT NULL, so the sink stores
	// `{}` if Raw is nil.
	Raw json.RawMessage
	// ForceUpdate forces the sink's UPDATE branch even when the
	// events-row content is byte-identical. Used by the reembed
	// path where the caller wants chunks + their embeddings
	// regenerated (e.g. because the chunker, prefix builder, or
	// embedder model changed) without waiting for a content edit
	// to trigger it naturally. The NoChange short-circuit stays
	// the default — operator-triggered bulk regen opts in.
	ForceUpdate bool
	// CheckUnchangedSince, together with UnchangedSince, requests an
	// optimistic-concurrency precondition: Upsert becomes a no-op
	// (UpsertResult.Stale=true, nothing written) unless the stored
	// row's updated_at still matches UnchangedSince at write time. Set
	// by a caller whose Event was derived from an earlier read of the
	// row (reembed's batch SELECT), so a write built from that
	// snapshot can never clobber a commit — an edit, a delete, a
	// scope change — that landed after the read. Every write that
	// touches a row's tracked columns bumps updated_at, so this one
	// check catches edits and deletes alike. Checked and applied
	// atomically with the same FOR UPDATE the rest of Upsert takes, so
	// no separate lock is needed.
	CheckUnchangedSince bool
	// UnchangedSince is the row's updated_at as the caller read it.
	// events.updated_at is nullable (NULL until a row's first UPDATE),
	// so nil is itself a meaningful reading — "this row had never been
	// updated" — not "no precondition"; CheckUnchangedSince alone
	// toggles whether the precondition applies.
	UnchangedSince *time.Time
	// ContextPrefix is a short string (typically <200 chars) the
	// embedder prepends to each chunk's content before sending to
	// the embedding model. Intended for contextual retrieval: channel name, thread
	// subject, actor handle — whatever structural context makes
	// the chunk identifiable out of context.
	//
	// Empty string = no prefix (content embedded as-is). Stored
	// per-chunk in event_chunks.context_prefix; every chunk emitted
	// for this event receives the same prefix. Not re-chunked if
	// the prefix changes; operator-level re-embed required.
	//
	// Raw content in events.body and event_chunks.content is NOT
	// modified — retrieval matches via the contextual embedding,
	// citation and display use raw text.
	ContextPrefix string
	// EmbeddingText is the text the chunker should slice for
	// embedding when it differs from the human-readable Body.
	// Empty string = use Body verbatim (what most sources do);
	// non-empty = chunk this instead, leaving
	// Body untouched for retrieval display.
	//
	// Today only agent_session events set this — session ingest sends
	// a keyword-dense form alongside the prose
	// narrative because the embedder benefits from a different
	// shape than the operator should read in retrieve responses.
	EmbeddingText string
	// SessionID, RecordStart, RecordEnd are the agent_session range-
	// as-handle fields. Lifted to dedicated columns so
	// /v1/expand-chunk can index on session_id and proxy back to
	// the session feed without parsing the composite ExternalID. Stored as
	// nullable in Postgres; an empty SessionID means "this isn't
	// an agent_session event" and the columns stay NULL.
	SessionID   string
	RecordStart int
	RecordEnd   int
	// Scope is the retrieval-visibility scope for this event: "public",
	// "internal", "slack-channel:<id>", or "owner:<email>". Required —
	// validate() rejects an empty Scope so no ingest path can
	// forget to stamp it. A connector that can't determine the
	// right scope (e.g. channel-info lookup failed) should stamp
	// ScopeUnstamped explicitly: the event still ingests but no
	// resolver ever grants the sentinel, so it stays invisible
	// until the reconcile sweep re-stamps it. Deny-by-default.
	Scope string
}

// EventIdentity is the source-native actor metadata needed when a model
// expands an event, and the event's scope, which decides who may. It
// intentionally mirrors the flat events columns; the model-facing identity
// projection belongs at the ask boundary.
type EventIdentity struct {
	Source         string
	Scope          string
	Actor          string
	ActorCanonical string
	ActorID        string
}

// Scope constants. Channel scopes are "slack-channel:<channel_id>" and
// owner scopes "owner:<email>" — built with fmt/concat at the
// stamp site, matched verbatim at query time.
const (
	// ScopePublic: anonymously internet-readable content.
	ScopePublic = "public"
	// ScopeInternal: company-wide knowledge from shared agent sessions,
	// Notion, and Linear. Slack uses individual channel scopes.
	ScopeInternal = "internal"
	ScopeZendesk  = "corpus:zendesk"
	// ScopeUnstamped is the deny-by-default sentinel: matches no
	// resolver, ever. The schema default; also stamped explicitly
	// when a connector can't determine the real scope.
	ScopeUnstamped = "unstamped"
)

// ScopeChannel builds the scope string for a public or private Slack channel.
// Stamp side and resolve side both call this so the spelling can't
// drift.
func ScopeChannel(channelID string) string {
	return "slack-channel:" + channelID
}

// ScopeOwner builds the scope string for owner-restricted events. Slack DMs
// use it today; agent-session opt-out can reuse the same audience contract.
func ScopeOwner(email string) string {
	return "owner:" + email
}

// validate rejects events missing load-bearing fields before we hit
// the DB. Letting these through turns into a cryptic Postgres
// constraint violation instead of a clear client-side error.
func (e *Event) validate() error {
	if e.Source == "" {
		return errors.New("events: Source is required")
	}
	if e.ExternalID == "" {
		return errors.New("events: ExternalID is required")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("events: OccurredAt is required")
	}
	if e.Body == "" {
		// body is NOT NULL and is the text the embedder slices. If
		// a source genuinely has nothing to say (a pure reaction event,
		// say), the connector needs to synthesize text first. Failing
		// loudly is correct.
		return errors.New("events: Body is required")
	}
	if e.Scope == "" {
		// Forcing function: every ingest path must decide a scope.
		// "Don't know" is spelled ScopeUnstamped, not "".
		return errors.New("events: Scope is required (use ScopeUnstamped if undeterminable)")
	}
	return nil
}

// DBSink writes events into Postgres. Safe for concurrent use by many
// goroutines because pgxpool is already pooled.
type DBSink struct {
	pool    *pgxpool.Pool
	metrics *sinkMetrics
	// observer, when set, is told of each event the sink inserts or
	// changes.
	observer WriteObserver
}

// WriteObserver is told of each event an observed sink inserts or changes:
// a connector's metrics (settings.Metrics) are one.
type WriteObserver interface {
	Wrote()
}

// Observed returns a sink that writes as s does and also tells observer of
// each event it inserts or changes.
func (s *DBSink) Observed(observer WriteObserver) *DBSink {
	observed := *s
	observed.observer = observer
	return &observed
}

// NewDBSink wires up the sink. reg may be nil in tests.
func NewDBSink(pool *pgxpool.Pool, reg prometheus.Registerer) *DBSink {
	if reg == nil {
		reg = prometheus.NewRegistry() // swallow
	}
	return &DBSink{
		pool:    pool,
		metrics: newSinkMetrics(reg),
	}
}

// UpsertResult reports what changed on the Postgres side. Used by
// connectors to emit per-source metrics ("new events/min",
// "edits/min") and by tests.
type UpsertResult struct {
	EventID   int64
	WasUpdate bool // true if we updated an existing row rather than inserting a new one
	// NoChange is true when (source, external_id) already existed
	// AND the incoming Event's stored fields were byte-identical to the
	// stored row. WasUpdate is false in this case (no UPDATE ran),
	// the chunk rows are untouched, and no re-embedding was
	// triggered. Backfill relies on this: re-running against a
	// fully-ingested channel becomes a no-op at the Postgres level.
	NoChange bool
	Chunks   int // how many chunk rows we inserted (0 on NoChange or a raw-only update)
	// Stale is true when Event.CheckUnchangedSince was set and the
	// stored row's updated_at no longer matched UnchangedSince: nothing
	// was written or touched. The caller's snapshot is out of date — a
	// concurrent write landed after it was taken — and must not be
	// applied over that newer commit.
	Stale bool
}

// SideWrite is an optional callback run inside the same Postgres
// transaction as the event upsert, after the event row (and its
// chunk rows) are written but before COMMIT. It receives the open
// tx and the resolved event id so a connector can fold a
// source-specific side-table write into the same atomic unit as the
// event.
//
// Why this exists: the events package is source-agnostic — it knows
// nothing about slack_message_details, notion page metadata, etc. A
// connector that needs a side row written transactionally with the
// event passes a SideWrite; the events package runs it as an opaque
// step inside the tx without learning what it does. If the callback
// returns an error, the whole transaction rolls back (event row and
// side row both), so a connector can never strand an event with a
// missing side row the way a separate post-commit Exec could.
//
// The callback runs on ALL four upsert outcomes — insert, material
// update, raw-only update, AND no-change — because a no-change event
// still needs its side row to exist (e.g. a re-ingest of a message
// whose original side-table write failed must repair the missing row
// even though the event body is byte-identical). The callback must
// therefore be an upsert (ON CONFLICT DO UPDATE), not a bare INSERT.
type SideWrite func(ctx context.Context, tx pgx.Tx, eventID int64) error

// Upsert writes or updates a single event. The flow:
//
//  1. BEGIN tx
//  2. INSERT INTO events ... ON CONFLICT (source, external_id) DO UPDATE
//     ... RETURNING id, was_update
//  3. if embedding-relevant fields changed: DELETE FROM event_chunks WHERE event_id = $1
//     RETURNING id — these are the chunk ids whose qdrant points
//     need removing.
//  4. chunk.Chunk(body); INSERT INTO event_chunks ...
//  5. COMMIT
//
// The event_chunks delete trigger adds old point IDs to the durable queue in
// the same transaction. Any error leaves both the row change and queue clean.
func (s *DBSink) Upsert(ctx context.Context, e Event) (UpsertResult, error) {
	return s.UpsertWithSideWrite(ctx, e, nil)
}

// UpsertWithSideWrite is Upsert with an optional in-transaction side
// write (see SideWrite). Pass nil for sideWrite to get the plain
// Upsert behavior. The side write runs inside the same tx as the
// event row, so the two land atomically: either both commit or
// neither does.
func (s *DBSink) UpsertWithSideWrite(ctx context.Context, e Event, sideWrite SideWrite) (UpsertResult, error) {
	// Sanitize before validate(), not after: a Body made of nothing but a
	// NUL byte (or other content that sanitizes to empty) must surface as
	// the ordinary, connector-recognizable "Body is required" error here,
	// not reach upsertTx and fail with "chunker produced zero chunks" —
	// a second, undocumented way to spell the same permanent failure.
	// Connectors that treat an event with nothing to index as a skip
	// (Discord's render, Notion's ingestOne) must check
	// SanitizeText(body) themselves before ever calling Upsert, so that
	// case never reaches validate() at all; this ordering only decides
	// which error a connector that doesn't see it coming gets.
	e = sanitizeEvent(e)
	if err := e.validate(); err != nil {
		s.metrics.upsert.WithLabelValues(e.Source, "invalid").Inc()
		return UpsertResult{}, err
	}
	timer := s.metrics.startUpsertTimer(e.Source)

	res, _, err := s.upsertTx(ctx, e, sideWrite)
	if err != nil {
		timer.done("err")
		return UpsertResult{}, err
	}
	switch {
	case res.Stale:
		timer.done("stale")
	case res.NoChange:
		timer.done("noop")
	default:
		timer.done("ok")
		if s.observer != nil {
			s.observer.Wrote()
		}
	}

	// Observe ingest lag only on a genuine INSERT: the first time this
	// (source, external_id) pair lands. Updates, no-ops and stale
	// precondition misses are excluded because ingested_at is immutable
	// after the initial write — the lag from a re-ingest reflects
	// connector/backfill timing, not true first-sighting latency, and a
	// stale miss touches an existing row too. Clamped to >=0 to guard
	// against future-dated occurred_at values from upstream clock skew.
	if !res.WasUpdate && !res.NoChange && !res.Stale {
		lag := time.Since(e.OccurredAt).Seconds()
		if lag < 0 {
			lag = 0
		}
		s.metrics.ingestLag.WithLabelValues(e.Source).Observe(lag)
	}

	return res, nil
}

// upsertTx does steps 1–5 of Upsert. Returning oldChunkIDs supplies metrics;
// the database trigger owns durable Qdrant cleanup.
//
// Four-way outcome: insert (new row), material update (existing row
// with embedding/payload changes -> re-chunk + re-embed), raw-only
// update (persist the latest source envelope without touching chunks),
// and no-change (all stored fields byte-identical). The no-change and
// raw-only paths keep replay/backfill traffic from churning Qdrant.
func (s *DBSink) upsertTx(ctx context.Context, e Event, sideWrite SideWrite) (UpsertResult, []uint64, error) {
	// e arrives here already sanitized (sanitizeEvent, called by
	// UpsertWithSideWrite before validate()); upsertTx only needs to
	// supply the default for an absent Raw.
	raw := e.Raw
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	// EmbeddingText is what the chunker (and therefore the embedder)
	// sees when it's set; Body is what callers display in retrieve
	// responses. Most sources leave EmbeddingText empty and embed
	// Body.
	chunkInput := e.Body
	if e.EmbeddingText != "" {
		chunkInput = e.EmbeddingText
	}
	chunks := chunk.Chunk(chunkInput)
	if len(chunks) == 0 {
		// Shouldn't happen after validate() rejected empty Body, but
		// belt-and-suspenders: an empty body would leave the row with
		// no chunk rows, which the embedder can't act on.
		return UpsertResult{}, nil, errors.New("events: chunker produced zero chunks")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return UpsertResult{}, nil, fmt.Errorf("events: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Four-branch upsert in one round-trip. The CTE:
	//   ins:      INSERT ... ON CONFLICT DO NOTHING → RETURNING id
	//   existing: lock and classify the existing row when ins fired nothing
	//   upd:      UPDATE when any stored field differs, returning whether
	//             chunks must be rebuilt
	//
	// Raw is deliberately excluded from chunks_dirty. It is preserved for
	// connector bookkeeping and debugging, but it is not embedded; Body and
	// EmbeddingText are the authoritative searchable inputs. This prevents
	// equivalent Slack socket and conversations.history envelopes from
	// deleting and recreating the same vector. Every other pre-existing dirty
	// condition remains material.
	//
	// The WHERE on upd only compares columns on the events row.
	// ContextPrefix lives on event_chunks and isn't compared here,
	// so a Slack channel rename (which would change the prefix) on
	// an otherwise-unchanged message is a NoChange — the stored
	// embedding stays slightly stale. Acceptable given renames are
	// rare and mention-resolution in body is the bigger retrieval
	// signal anyway. Bulk re-embed is an operator tool if we ever
	// need to invalidate prefixes en masse.
	// When ForceUpdate is set, the UPDATE WHERE condition collapses
	// to TRUE so chunks always get rewritten + re-embedded. The
	// reembed admin endpoint uses this to regenerate the corpus
	// after a chunker / prefix / embedder-model change.
	// Note on actor_canonical (@actor_canonical): it's NOT NULL
	// DEFAULT '' in the schema, so we pass a plain string (empty when
	// the source doesn't have a canonical key). IS DISTINCT FROM works
	// on plain text too — for two non-null strings it's the same as <>
	// — so an existing row picks up actor_canonical changes the same
	// way it does for the rest of the fields.
	//
	// Note on occurred_at (@occurred_at): mutable on UPDATE. A whole-session
	// row's occurred_at tracks latest activity. Without occurred_at in the
	// row dirty-check + SET list, activity-only updates would
	// be a no-op (body byte-identical, the original dirty-check would
	// say "no change") and the `list` tool would order sessions by
	// birth-time instead of last activity. Including occurred_at here
	// IS the activity tracker for in-place-updated source rows.
	//
	// Args are passed via pgx.NamedArgs (@name placeholders) so adding
	// a column is a one-line edit at the build site below rather than a
	// renumbering of every $N — the latter bit us when scope became
	// the 16th positional param.
	chunksDirtyCondition := `(
		     body              <> @body
		  OR title             IS DISTINCT FROM @title
		  OR actor             IS DISTINCT FROM @actor
		  OR actor_id          IS DISTINCT FROM @actor_id
		  OR thread_id         IS DISTINCT FROM @thread_id
		  OR url               IS DISTINCT FROM @url
		  OR embedding_text    IS DISTINCT FROM @embedding_text
		  OR session_id        IS DISTINCT FROM @session_id
		  OR record_start         IS DISTINCT FROM @record_start
		  OR record_end           IS DISTINCT FROM @record_end
		  OR actor_canonical   IS DISTINCT FROM @actor_canonical
		  OR scope             IS DISTINCT FROM @scope
		  OR deleted
		   )`
	rowDirtyCondition := `(` + chunksDirtyCondition + ` OR occurred_at IS DISTINCT FROM @occurred_at OR raw::text <> @raw::text)`
	if e.ForceUpdate {
		chunksDirtyCondition = "TRUE"
		rowDirtyCondition = "TRUE"
	}
	// staleCondition backs Event.CheckUnchangedSince: when requested, the
	// existing row's updated_at must still match (NULL-safely, via IS
	// DISTINCT FROM) the value the caller read it at. Evaluated in the
	// same MATERIALIZED CTE, under the same FOR UPDATE, as
	// row_dirty/chunks_dirty, so the check-then-write has no gap of its
	// own. Every write that changes a tracked column bumps updated_at
	// (the UPDATE below, Delete, DeleteInTx, DeletePattern, Rescope), so
	// one comparison catches a concurrent edit, delete or scope change
	// alike.
	staleCondition := "FALSE"
	if e.CheckUnchangedSince {
		staleCondition = "updated_at IS DISTINCT FROM @expected_updated_at"
	}
	upsertSQL := `
		WITH ins AS (
			INSERT INTO events
				(source, external_id, occurred_at, actor, actor_id, thread_id,
				 title, body, url, raw, embedding_text, session_id,
				 record_start, record_end, actor_canonical, scope, deleted)
			VALUES
				(@source, @external_id, @occurred_at, @actor, @actor_id, @thread_id,
				 @title, @body, @url, @raw, @embedding_text, @session_id,
				 @record_start, @record_end, @actor_canonical, @scope, false)
			ON CONFLICT (source, external_id) DO NOTHING
			RETURNING id
		),
		existing AS MATERIALIZED (
			SELECT id,
			       ` + chunksDirtyCondition + ` AS chunks_dirty,
			       ` + rowDirtyCondition + ` AS row_dirty,
			       ` + staleCondition + ` AS stale
			  FROM events
			 WHERE source = @source AND external_id = @external_id
			   AND NOT EXISTS (SELECT 1 FROM ins)
			 FOR UPDATE
		),
		upd AS (
			UPDATE events e SET
				body            = @body,
				occurred_at     = @occurred_at,
				title           = @title,
				actor           = @actor,
				actor_id        = @actor_id,
				thread_id       = @thread_id,
				url             = @url,
				raw             = @raw,
				embedding_text  = @embedding_text,
				session_id      = @session_id,
				record_start       = @record_start,
				record_end         = @record_end,
				actor_canonical = @actor_canonical,
				scope           = @scope,
				updated_at      = now(),
				deleted         = false
			  FROM existing
			 WHERE e.id = existing.id
			   AND existing.row_dirty
			   AND NOT existing.stale
			RETURNING e.id, existing.chunks_dirty
		)
		SELECT
			(SELECT id FROM ins)      AS inserted_id,
			(SELECT id FROM existing) AS existing_id,
			EXISTS (SELECT 1 FROM upd) AS row_updated,
			COALESCE((SELECT chunks_dirty FROM upd), false) AS chunks_dirty,
			COALESCE((SELECT stale FROM existing), false) AS stale
	`
	// expectedUpdatedAt stays an untyped nil (-> SQL NULL) when the
	// caller read the row before its first update, or when no
	// precondition was requested at all (staleCondition is "FALSE" in
	// that case, so the value is never compared).
	var expectedUpdatedAt any
	if e.CheckUnchangedSince && e.UnchangedSince != nil {
		expectedUpdatedAt = *e.UnchangedSince
	}
	args := pgx.NamedArgs{
		"source":              e.Source,
		"external_id":         e.ExternalID,
		"occurred_at":         e.OccurredAt,
		"actor":               nullable(e.Actor),
		"actor_id":            nullable(e.ActorID),
		"thread_id":           nullable(e.ThreadID),
		"title":               nullable(e.Title),
		"body":                e.Body,
		"url":                 nullable(e.URL),
		"raw":                 raw,
		"embedding_text":      nullable(e.EmbeddingText),
		"session_id":          nullable(e.SessionID),
		"record_start":        nullableInt(e.RecordStart, e.SessionID != ""),
		"record_end":          nullableInt(e.RecordEnd, e.SessionID != ""),
		"actor_canonical":     e.ActorCanonical,
		"scope":               e.Scope,
		"expected_updated_at": expectedUpdatedAt,
	}
	var (
		insertedID  *int64
		existingID  *int64
		rowUpdated  bool
		chunksDirty bool
		stale       bool
	)
	// Concurrent-first-insert race: two transactions upserting the same
	// brand-new (source, external_id) for the first time both take their
	// snapshot before either commits. The loser's `ins` CTE blocks on the
	// unique index; once the winner commits, DO NOTHING correctly returns
	// no row, but `existing` is a plain SELECT bound to the *same*
	// snapshot — taken before the winner's row existed — so it can't see
	// it either (FOR UPDATE only re-checks rows the snapshot already
	// found). Both ids come back NULL even though the row now genuinely
	// exists.
	//
	// A brand-new statement takes a brand-new snapshot, so re-running
	// this exact query (still the first statement of the transaction, so
	// nothing to roll back) lets a loser see the winner's now-committed
	// row on the next attempt, lock it with FOR UPDATE, and fall through
	// to the existing/update/no-change branch like any other edit. Bound
	// the loop so a pathological case (the row vanishing again between
	// attempts) still errors instead of spinning forever.
	const maxUpsertAttempts = 3
	for attempt := 1; attempt <= maxUpsertAttempts; attempt++ {
		err = tx.QueryRow(ctx, upsertSQL, args).Scan(&insertedID, &existingID, &rowUpdated, &chunksDirty, &stale)
		if err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: upsert row: %w", err)
		}
		if insertedID != nil || existingID != nil {
			break
		}
	}

	var (
		eventID  int64
		inserted = insertedID != nil
	)
	switch {
	case inserted:
		eventID = *insertedID
	case existingID != nil:
		eventID = *existingID
	default:
		return UpsertResult{}, nil, fmt.Errorf("events: upsert produced neither inserted nor existing id after %d attempts", maxUpsertAttempts)
	}

	// Stale precondition: the caller's Event was derived from a read
	// taken before a since-committed write (edit, delete, scope
	// change — anything that bumps updated_at). Touch nothing; the
	// row already reflects a newer truth than this Event does, and
	// writing it would clobber that commit. Commit the otherwise-empty
	// tx to release the FOR UPDATE lock.
	if stale {
		if err := tx.Commit(ctx); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: commit: %w", err)
		}
		return UpsertResult{EventID: eventID, Stale: true}, nil, nil
	}

	// No-change path: existing row, content unchanged. Don't touch
	// chunks, don't fire a qdrant delete, don't bump metrics that
	// imply work was done. Commit the (empty) tx so the BEGIN's
	// lock is released.
	//
	// The side write STILL runs here: a no-change event can have a
	// missing side row (e.g. a re-ingest repairing a side-table write
	// that failed the first time around), and the whole point of the
	// in-tx seam is that the side row can't be stranded. The callback
	// must be an upsert, so re-running it on an already-present row is
	// a benign no-op.
	if !inserted && !rowUpdated {
		if sideWrite != nil {
			if err := sideWrite(ctx, tx, eventID); err != nil {
				return UpsertResult{}, nil, fmt.Errorf("events: side write: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: commit: %w", err)
		}
		return UpsertResult{EventID: eventID, NoChange: true}, nil, nil
	}

	// Metadata-only changes update the event and run the connector side write
	// without replacing chunk rows. Recency changes queue a payload patch;
	// source-envelope-only changes leave Qdrant untouched. Neither re-embeds.
	if rowUpdated && !chunksDirty {
		if _, err := tx.Exec(ctx, `
			UPDATE event_chunks
			   SET occurred_at = $2,
			       scope_sync_needed = true,
			       scope_sync_attempts = 0,
			       scope_sync_next_attempt_at = now(),
			       scope_sync_last_error = NULL
			 WHERE event_id = $1 AND occurred_at IS DISTINCT FROM $2
		`, eventID, e.OccurredAt); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: refresh chunk recency: %w", err)
		}
		if sideWrite != nil {
			if err := sideWrite(ctx, tx, eventID); err != nil {
				return UpsertResult{}, nil, fmt.Errorf("events: side write: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: commit: %w", err)
		}
		return UpsertResult{EventID: eventID, WasUpdate: true}, nil, nil
	}

	// Insert + Update both need fresh chunk rows. Update also needs
	// to drop the old BODY chunks first (and collect their ids so the
	// post-commit qdrant delete hits them).
	//
	// Scope the delete to kind='text': re-chunking on re-ingest only
	// regenerates body chunks. image_caption chunks belong to the
	// out-of-band caption worker and are NOT reproduced here, so a
	// blanket delete would orphan them — the image would be left
	// captioned_at-set with chunk_id NULL, and the worker's
	// `captioned_at IS NULL` claim query never re-picks it, silently
	// dropping the caption from retrieval on every edit / unfurl
	// re-crawl / tail-sweep re-touch. Removed-image caption cleanup is
	// handled separately and correctly by ReconcileImages (which keys
	// off the current attachment list); the tombstone/delete path
	// still drops ALL kinds, which is right for a real deletion.
	var oldChunkIDs []uint64
	if chunksDirty {
		// Body chunks are deleted and recreated below, but worker-owned
		// chunks (image_caption, document_text, and future non-text kinds)
		// survive an event update. Keep their denormalized source recency in
		// lockstep with events.occurred_at; that column is mutable for sources
		// such as agent session rows. Without this update, a preserved
		// unembedded chunk would retain stale queue priority after its parent
		// event's activity timestamp moves.
		if _, err := tx.Exec(ctx, `
			UPDATE event_chunks
			   SET occurred_at = $2,
			       scope_sync_needed = true,
			       scope_sync_attempts = 0,
			       scope_sync_next_attempt_at = now(),
			       scope_sync_last_error = NULL
			 WHERE event_id = $1
			   AND kind <> 'text'
			   AND occurred_at IS DISTINCT FROM $2
		`, eventID, e.OccurredAt); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: refresh non-text chunk recency: %w", err)
		}

		// ForceUpdate also drives corpus-wide embedding-model refreshes
		// (reembed.go, "the embedder model version bumped"). Since we no
		// longer delete+rebuild the non-text chunks here, they'd keep
		// their old embedded_at/embedder_id and never be re-embedded on
		// a model bump — leaving image_caption vectors on the stale
		// model, a silently mixed-model corpus. Invalidate their
		// embeddings (NOT the rows — the image FK and the caption text
		// stay put) so the kind-agnostic embedder claim query
		// (embedded_at IS NULL) re-embeds the preserved text with the
		// new model. This re-embeds; it does NOT re-run the VLM — caption
		// content regen happens by resetting event_images.captioned_at
		// so the caption worker re-runs it, not reembed's job.
		if e.ForceUpdate {
			if _, err := tx.Exec(ctx, `
				UPDATE event_chunks
				   SET embedded_at = NULL, queued_at = now()
				 WHERE event_id = $1 AND kind <> 'text'
			`, eventID); err != nil {
				return UpsertResult{}, nil, fmt.Errorf("events: invalidate non-text embeddings: %w", err)
			}
		}

		rows, err := tx.Query(ctx,
			`DELETE FROM event_chunks WHERE event_id = $1 AND kind = 'text' RETURNING id`,
			eventID,
		)
		if err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: delete old chunks: %w", err)
		}
		ids, err := scanIDs(rows)
		if err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: scan old chunk ids: %w", err)
		}
		oldChunkIDs = ids
	}

	if err := insertChunks(ctx, tx, eventID, chunks, e.ContextPrefix, e.OccurredAt); err != nil {
		return UpsertResult{}, nil, err
	}

	// In-tx side write (e.g. slack_message_details) on the insert /
	// update paths. Runs after the chunk rows so the whole event —
	// row, chunks, and side row — commits atomically or not at all.
	if sideWrite != nil {
		if err := sideWrite(ctx, tx, eventID); err != nil {
			return UpsertResult{}, nil, fmt.Errorf("events: side write: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return UpsertResult{}, nil, fmt.Errorf("events: commit: %w", err)
	}

	s.metrics.chunksInserted.WithLabelValues(e.Source).Add(float64(len(chunks)))
	if chunksDirty {
		s.metrics.chunksDeleted.WithLabelValues(e.Source, "edit").Add(float64(len(oldChunkIDs)))
	}

	return UpsertResult{
		EventID:   eventID,
		WasUpdate: rowUpdated,
		Chunks:    len(chunks),
	}, oldChunkIDs, nil
}

// DeleteResult reports what the tombstone operation touched. Chunks
// reports the number of chunk rows removed.
type DeleteResult struct {
	EventID int64 // 0 if no matching event existed
	Chunks  int
	Existed bool // false when (source, external_id) was never ingested
	// Events is the count of event rows tombstoned. Equal to 1 for
	// exact-id Delete; >1 only for prefix DeletePattern.
	Events int
}

// Delete tombstones an event. Sets deleted=true, body=”, title=NULL,
// deletes all chunk rows, atomically queueing their point IDs. If no event
// exists for (source, externalID) this
// is a no-op — returns (DeleteResult{Existed: false}, nil) rather
// than an error.
func (s *DBSink) Delete(ctx context.Context, source, externalID string) (DeleteResult, error) {
	if source == "" || externalID == "" {
		s.metrics.delete.WithLabelValues(source, "invalid").Inc()
		return DeleteResult{}, errors.New("events: Delete requires source and externalID")
	}
	timer := s.metrics.startDeleteTimer(source)

	res, _, err := s.deleteTx(ctx, source, externalID)
	if err != nil {
		timer.done("err")
		return DeleteResult{}, err
	}
	timer.done("ok")

	return res, nil
}

// DeleteInTx tombstones an event inside a caller-owned transaction. The
// returned callback must be invoked only after that transaction commits so
// deletion metrics describe committed work. The chunk trigger queues Qdrant
// cleanup inside the caller's transaction. On rollback, discard the callback.
func (s *DBSink) DeleteInTx(ctx context.Context, tx pgx.Tx, source, externalID string) (DeleteResult, func(context.Context), error) {
	if tx == nil || source == "" || externalID == "" {
		return DeleteResult{}, nil, errors.New("events: DeleteInTx requires tx, source, and externalID")
	}
	var eventID int64
	err := tx.QueryRow(ctx, `
		UPDATE events
		   SET deleted    = true,
		       body       = '',
		       title      = NULL,
		       updated_at = now()
		 WHERE source = $1 AND external_id = $2
		RETURNING id
	`, source, externalID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteResult{Existed: false}, func(context.Context) {}, nil
	}
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: tombstone row in caller tx: %w", err)
	}
	rows, err := tx.Query(ctx, `DELETE FROM event_chunks WHERE event_id = $1 RETURNING id`, eventID)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: delete chunks in caller tx: %w", err)
	}
	chunkIDs, err := scanIDs(rows)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: scan chunk ids in caller tx: %w", err)
	}
	result := DeleteResult{EventID: eventID, Chunks: len(chunkIDs), Existed: true, Events: 1}
	postCommit := func(context.Context) {
		s.metrics.chunksDeleted.WithLabelValues(source, "tombstone").Add(float64(len(chunkIDs)))
	}
	return result, postCommit, nil
}

func (s *DBSink) deleteTx(ctx context.Context, source, externalID string) (DeleteResult, []uint64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var eventID int64
	err = tx.QueryRow(ctx, `
		UPDATE events
		   SET deleted    = true,
		       body       = '',
		       title      = NULL,
		       updated_at = now()
		 WHERE source = $1 AND external_id = $2
		RETURNING id
	`, source, externalID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing to tombstone; this is an idempotent no-op. Commit
		// the (empty) tx to release the begin.
		if err := tx.Commit(ctx); err != nil {
			return DeleteResult{}, nil, fmt.Errorf("events: commit: %w", err)
		}
		return DeleteResult{Existed: false}, nil, nil
	}
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: tombstone row: %w", err)
	}

	rows, err := tx.Query(ctx,
		`DELETE FROM event_chunks WHERE event_id = $1 RETURNING id`,
		eventID,
	)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: delete chunks: %w", err)
	}
	chunkIDs, err := scanIDs(rows)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: scan chunk ids: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: commit: %w", err)
	}

	s.metrics.chunksDeleted.WithLabelValues(source, "tombstone").Add(float64(len(chunkIDs)))

	return DeleteResult{
		EventID: eventID,
		Chunks:  len(chunkIDs),
		Existed: true,
		Events:  1,
	}, chunkIDs, nil
}

// DeletePattern tombstones every event in source whose external_id
// starts with the given prefix AND, when eventKind is non-empty,
// whose raw.event_kind equals eventKind. Same per-row semantics as
// Delete (deleted=true, body=”, title=NULL, chunk rows removed,
// durable Qdrant delete queue entry for every affected chunk id),
// applied in ONE transaction so a whole logical unit that spans a set
// of range-keyed events (e.g. agent_session native chunk events
// keyed owner/client/session:records:<start>:<end>) is removed
// atomically.
//
// externalIDPrefix is matched as a literal prefix: LIKE metacharacters
// in the value are escaped, so it can never act as a wildcard. The
// eventKind filter lets a caller scope the removal to its own tagged
// events while leaving untagged (legacy) rows under the same prefix
// untouched; pass "" to match the prefix alone. When nothing matches
// it is an idempotent no-op (Existed=false).
func (s *DBSink) DeletePattern(ctx context.Context, source, externalIDPrefix, eventKind string) (DeleteResult, error) {
	if source == "" || externalIDPrefix == "" {
		s.metrics.delete.WithLabelValues(source, "invalid").Inc()
		return DeleteResult{}, errors.New("events: DeletePattern requires source and externalIDPrefix")
	}
	timer := s.metrics.startDeleteTimer(source)

	res, _, err := s.deletePatternTx(ctx, source, externalIDPrefix, eventKind)
	if err != nil {
		timer.done("err")
		return DeleteResult{}, err
	}
	timer.done("ok")

	return res, nil
}

func (s *DBSink) deletePatternTx(ctx context.Context, source, externalIDPrefix, eventKind string) (DeleteResult, []uint64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// LIKE with an escaped literal prefix + ESCAPE '\'. The pattern
	// (<prefix> || '%') is safe because escapeLike quotemines
	// backslash, %, and _ in the caller-supplied prefix.
	rows, err := tx.Query(ctx, `
		UPDATE events
		   SET deleted    = true,
		       body       = '',
		       title      = NULL,
		       updated_at = now()
		 WHERE source = $1 AND external_id LIKE $2 || '%' ESCAPE '\'
		   AND ($3 = '' OR raw->>'event_kind' = $3)
		   AND deleted = false
		RETURNING id
	`, source, escapeLike(externalIDPrefix), eventKind)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: pattern tombstone rows: %w", err)
	}
	eventIDs, err := scanEventIDs(rows)
	if err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: scan pattern tombstone ids: %w", err)
	}
	if len(eventIDs) == 0 {
		// Nothing to tombstone; idempotent no-op. Commit the (empty)
		// tx to release the begin.
		if err := tx.Commit(ctx); err != nil {
			return DeleteResult{}, nil, fmt.Errorf("events: commit: %w", err)
		}
		return DeleteResult{Existed: false}, nil, nil
	}

	var chunkIDs []uint64
	for _, eventID := range eventIDs {
		crows, err := tx.Query(ctx,
			`DELETE FROM event_chunks WHERE event_id = $1 RETURNING id`,
			eventID,
		)
		if err != nil {
			return DeleteResult{}, nil, fmt.Errorf("events: delete chunks: %w", err)
		}
		ids, err := scanIDs(crows)
		if err != nil {
			return DeleteResult{}, nil, fmt.Errorf("events: scan chunk ids: %w", err)
		}
		chunkIDs = append(chunkIDs, ids...)
	}

	if err := tx.Commit(ctx); err != nil {
		return DeleteResult{}, nil, fmt.Errorf("events: commit: %w", err)
	}

	s.metrics.chunksDeleted.WithLabelValues(source, "tombstone").Add(float64(len(chunkIDs)))

	return DeleteResult{
		Chunks:  len(chunkIDs),
		Existed: true,
		Events:  len(eventIDs),
	}, chunkIDs, nil
}

// Rescope gives every live event of source whose external ID starts with
// externalIDPrefix the scope scope, in place: retrieval reads the new scope
// from Postgres at once, and the chunks are queued for the scope payload
// worker to repair Qdrant, without re-embedding. It returns how many events
// changed.
func (s *DBSink) Rescope(ctx context.Context, source, externalIDPrefix, scope string) (int64, error) {
	if source == "" || externalIDPrefix == "" || scope == "" {
		return 0, errors.New("events: Rescope requires source, externalIDPrefix and scope")
	}
	var changed int64
	err := s.pool.QueryRow(ctx, `
		WITH changed AS (
			UPDATE events
			   SET scope = $3, updated_at = now()
			 WHERE source = $1 AND starts_with(external_id, $2)
			   AND deleted = false AND scope <> $3
			RETURNING id
		), queued AS (
			-- A data-modifying CTE runs whether or not it is read.
			UPDATE event_chunks c
			   SET scope_sync_needed = true,
			       scope_sync_attempts = 0,
			       scope_sync_next_attempt_at = now(),
			       scope_sync_last_error = NULL
			  FROM changed
			 WHERE c.event_id = changed.id
		)
		SELECT count(*) FROM changed
	`, source, externalIDPrefix, scope).Scan(&changed)
	if err != nil {
		return 0, fmt.Errorf("events: rescope: %w", err)
	}
	return changed, nil
}

// ExternalIDs returns the external IDs of source's events that are not
// deleted, for a source that removes the ones it no longer finds.
func (s *DBSink) ExternalIDs(ctx context.Context, source string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT external_id FROM events WHERE source = $1 AND NOT deleted`, source)
	if err != nil {
		return nil, fmt.Errorf("events: external ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("events: external ids: %w", err)
	}
	return ids, nil
}

// escapeLike escapes LIKE metacharacters so a caller-supplied literal
// can be prefix-matched via LIKE ... ESCAPE '\' without acting as a
// wildcard. Backslash itself is escaped first so the remaining
// replacements stay unambiguous.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// insertChunks writes the new chunk rows for an event. Called inside
// an open tx. Uses a single parameterized insert with per-chunk rows;
// arg count stays well within pgx's 65k limit for any realistic event.
// contextPrefix is the same for every chunk in the event ("" = NULL
// stored). occurredAt is the parent event's SOURCE recency
// (events.occurred_at) — the insert stamps the denormalized
// event_chunks.occurred_at from it so the embed claim queue can order
// by recency without a JOIN, and so re-ingest/re-chunk always rebuilds
// from source recency rather than a processing timestamp. Both the
// events row and these chunks are written in the same tx, so they
// can't drift.
func insertChunks(ctx context.Context, tx pgx.Tx, eventID int64, chunks []string, contextPrefix string, occurredAt time.Time) error {
	// Build "INSERT INTO event_chunks (event_id, chunk_idx, content,
	// context_prefix, occurred_at) VALUES ($1,$2,$3,$4,$5),($6,$7,$8,$9,$10),..."
	// in one round-trip.
	const cols = 5
	args := make([]any, 0, len(chunks)*cols)
	var placeholders []byte
	var prefixArg any
	if contextPrefix != "" {
		prefixArg = contextPrefix
	}
	for i, c := range chunks {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		base := i*cols + 1
		placeholders = append(placeholders, '(')
		placeholders = append(placeholders, fmt.Sprintf("$%d,$%d,$%d,$%d,$%d", base, base+1, base+2, base+3, base+4)...)
		placeholders = append(placeholders, ')')
		args = append(args, eventID, i, c, prefixArg, occurredAt)
	}
	sql := "INSERT INTO event_chunks (event_id, chunk_idx, content, context_prefix, occurred_at) VALUES " + string(placeholders)
	_, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("events: insert chunks: %w", err)
	}
	return nil
}

// scanIDs pulls bigint ids out of a pgx.Rows and converts them to
// uint64 (qdrant's point-id type). Any negative id is a schema bug
// (IDENTITY columns start at 1); we fail loudly rather than underflow.
func scanIDs(rows pgx.Rows) ([]uint64, error) {
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id < 0 {
			return nil, fmt.Errorf("events: negative chunk id %d", id)
		}
		ids = append(ids, uint64(id))
	}
	return ids, rows.Err()
}

// scanEventIDs pulls event ids out of a pgx.Rows.
func scanEventIDs(rows pgx.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SanitizeText makes a string safe for a Postgres text column: it removes
// literal NUL bytes (Postgres text rejects them outright, SQLSTATE 22021)
// and replaces any invalid UTF-8 byte sequence with U+FFFD. NUL removal
// happens first because strings.ToValidUTF8 treats 0x00 as a perfectly
// valid (if unusual) UTF-8 byte and would otherwise leave it in place.
//
// Exported so every write path that fills a Postgres text column — not
// just the sink itself — can sanitize with the same rule instead of
// growing its own near-duplicate (internal/sessioningest's
// boundedIndexError reuses this).
func SanitizeText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
}

// sanitizeEvent returns e with every Postgres text column and the raw JSON
// document made storable (SanitizeText / SanitizeJSON), run once before
// validate() so a dirty byte is always resolved at the same point relative
// to validation, whichever field carries it.
func sanitizeEvent(e Event) Event {
	e.Title = SanitizeText(e.Title)
	e.Body = SanitizeText(e.Body)
	e.Actor = SanitizeText(e.Actor)
	e.ActorCanonical = SanitizeText(e.ActorCanonical)
	e.ActorID = SanitizeText(e.ActorID)
	e.ThreadID = SanitizeText(e.ThreadID)
	e.URL = SanitizeText(e.URL)
	e.ContextPrefix = SanitizeText(e.ContextPrefix)
	e.EmbeddingText = SanitizeText(e.EmbeddingText)
	if len(e.Raw) > 0 {
		e.Raw = SanitizeJSON(e.Raw)
	}
	return e
}

// SanitizeJSON makes a JSON document safe for a jsonb column. Postgres's
// jsonb input parser refuses a \u0000 escape (SQLSTATE 22P05) because it
// decodes to a NUL character jsonb cannot represent, and the connectors
// that build Raw via json.Marshal produce exactly that escape whenever the
// underlying Go string held a literal NUL byte.
//
// This has to be parse-aware, not a blind byte/substring replace: the
// 6-byte ASCII text `\u0000` appears in well-formed JSON both as the
// escape for an actual NUL character (one Go rune, must be stripped) and,
// with an extra leading backslash, as the escaped-backslash-followed-by-
// literal-text form `\\u0000` (an actual backslash followed by the six
// characters "u0000", not a NUL, must be left alone). Decoding with
// encoding/json resolves that distinction for free — \u0000 decodes to a
// single 0x00 byte, \\u0000 decodes to the seven-byte string `\u0000`
// (backslash included) — so sanitizing after decode and re-encoding never
// confuses the two.
//
// json.Number (via UseNumber) avoids losing precision on large integers
// (Discord/Slack snowflake-style IDs can exceed float64's exact integer
// range) by round-tripping the original digit text instead of a decoded
// float64.
//
// Exported so every write path that fills a jsonb column — not just the
// sink itself — can sanitize with the same parse-aware rule instead of
// growing its own near-duplicate (internal/source's State.Set and
// State.Update, which marshal arbitrary connector-supplied values into
// source_state.value, reuse this).
func SanitizeJSON(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	// dec.Decode reads only the first JSON value and silently ignores
	// anything after it, so a second Decode must confirm there's nothing
	// left. dec.More() alone isn't enough: it reports whether another
	// JSON value *starts* next, so trailing bytes that are themselves
	// invalid as a value — a stray closing `}` or `]` left over from, say,
	// a caller that double-wrapped a document — report More()==false and
	// would have slipped through as if the input were a single clean
	// document. Decoding again and requiring io.EOF catches both: a
	// second well-formed value (err == nil) and leftover bytes that don't
	// parse as one (any other error) are both trailing data.
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		// Trailing data of some kind (a second value, or bytes that don't
		// parse as one at all). Every caller builds Raw with a single
		// json.Marshal, so this shouldn't happen; fall back to the
		// original bytes rather than fail the whole write over a Raw we
		// can't parse — Postgres will reject it if it's genuinely broken,
		// which is no worse than before this function existed.
		return raw
	}
	cleaned, err := json.Marshal(sanitizeJSONValue(v))
	if err != nil {
		return raw
	}
	return cleaned
}

// sanitizeJSONValue recursively applies SanitizeText to every string
// (object keys included) in a decoded JSON value, leaving numbers,
// booleans, and null untouched.
//
// Object keys are visited in sorted order so that two keys which
// collide after sanitizing (e.g. "a" and "a\x00", both -> "a") resolve
// deterministically — the lexicographically-last original key always
// wins — instead of depending on Go's randomized map iteration order.
// Without this, re-ingesting the same unchanged item could flip which
// value survived the collision from one Upsert to the next, which the
// raw::text dirty-check would see as a change and churn a raw-only
// UPDATE (and the connector's "wrote" observer) on every sync.
func sanitizeJSONValue(v any) any {
	switch val := v.(type) {
	case string:
		return SanitizeText(val)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cleaned := make(map[string]any, len(val))
		for _, k := range keys {
			cleaned[SanitizeText(k)] = sanitizeJSONValue(val[k])
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(val))
		for i, vv := range val {
			cleaned[i] = sanitizeJSONValue(vv)
		}
		return cleaned
	default:
		// json.Number, bool, nil.
		return val
	}
}

// nullable turns "" into a typed nil so pgx encodes it as SQL NULL.
// The events table accepts NULL on actor/actor_id/thread_id/title/url.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt turns an int into either itself or typed nil based on
// the present flag. Used for record_start / record_end which are nullable
// only when the event isn't an agent_session range row — sending 0
// for non-range events would be misleading (could be confused with
// "the very first message"). Caller passes (e.SessionID != "") as
// the present flag so the columns stay coupled.
func nullableInt(n int, present bool) any {
	if !present {
		return nil
	}
	return n
}

// ErrNotAgentSession is returned by AgentSessionRange when the event
// row exists but isn't from the agent_session source. Callers
// (the /v1/expand-chunk handler) translate this into a 400.
var ErrNotAgentSession = errors.New("event is not from agent_session source")

// ErrEventNotFound is returned by AgentSessionRange when no event with
// the given id exists. Callers translate this into a 404.
var ErrEventNotFound = errors.New("event not found")

// ErrReferenceAmbiguous is returned when a human-facing corpus identifier
// matches more than one event. Exact references must never silently choose a
// semantic or most-recent match.
var ErrReferenceAmbiguous = errors.New("reference matches multiple events")

// AgentSessionRange resolves an event_id to its (session_id, record_start,
// record_end) triple, asserting the event is an agent_session row.
// Used by /v1/expand-chunk to find the range to proxy to the session feed.
//
// We could lift this onto a separate Lookup type, but it's a
// single-query read against the same pool the sink uses, so
// hanging it off DBSink keeps wiring simple.
type SessionRange struct {
	Owner     string
	Client    string
	SessionID string
	Revision  string
	// DecoderVersion names the materialization a pushed session's event was
	// built from; "" for feed sessions and range events.
	DecoderVersion string
	Start          int
	End            int
	// Whole says the event is the whole-session summary, not one stretch.
	Whole bool
}

// AgentSessionRange resolves both legacy session ranges and the canonical
// Bsky Session v1 identity carried by native session-ingest event metadata.
func (s *DBSink) AgentSessionRange(ctx context.Context, eventID int64) (SessionRange, error) {
	var (
		source     string
		externalID string
		sessionID  *string
		msgStart   *int
		msgEnd     *int
		raw        []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT source, external_id, session_id, record_start, record_end, raw
		   FROM events
		  WHERE id = $1`, eventID,
	).Scan(&source, &externalID, &sessionID, &msgStart, &msgEnd, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRange{}, ErrEventNotFound
		}
		return SessionRange{}, fmt.Errorf("events: agent session range lookup: %w", err)
	}
	if source != "agent_session" {
		return SessionRange{}, ErrNotAgentSession
	}
	if sessionID == nil || msgStart == nil || msgEnd == nil {
		// agent_session row with NULL range fields shouldn't
		// happen — the ingest endpoint enforces all three. But if
		// it does, surfacing it as not-found is the safest fallback
		// (the operator gets a clear "go check the row" error
		// rather than a confusing slice-empty range).
		return SessionRange{}, fmt.Errorf("events: agent_session row %d missing range fields", eventID)
	}
	result := SessionRange{
		SessionID: *sessionID, Start: *msgStart, End: *msgEnd,
		Whole: strings.HasSuffix(externalID, ":session"),
	}
	var meta struct {
		Owner          string `json:"owner"`
		Client         string `json:"client"`
		Revision       string `json:"revision"`
		DecoderVersion string `json:"decoder_version"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &meta) == nil {
		result.Owner, result.Client, result.Revision = meta.Owner, meta.Client, meta.Revision
		result.DecoderVersion = meta.DecoderVersion
	}
	return result, nil
}

// SessionStretch is one stretch of an agent session as the corpus holds it:
// the event to expand for its records, and the summary of what happened in
// it.
type SessionStretch struct {
	EventID    int64
	Start, End int
	OccurredAt time.Time
	Summary    string
}

// SessionStretches returns the stretches of a session that scopes may read,
// in session order.
func (s *DBSink) SessionStretches(ctx context.Context, sessionID string, scopes []string) ([]SessionStretch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, record_start, record_end, occurred_at, body
		  FROM events
		 WHERE source = 'agent_session'
		   AND session_id = $1
		   AND deleted = false
		   AND raw->>'event_kind' = 'native_session_chunk'
		   AND scope = ANY($2)
		 ORDER BY record_start, id`, sessionID, ReadableScopes(scopes))
	if err != nil {
		return nil, fmt.Errorf("events: session stretches: %w", err)
	}
	stretches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SessionStretch, error) {
		var stretch SessionStretch
		err := row.Scan(&stretch.EventID, &stretch.Start, &stretch.End, &stretch.OccurredAt, &stretch.Summary)
		return stretch, err
	})
	if err != nil {
		return nil, fmt.Errorf("events: session stretches: %w", err)
	}
	return stretches, nil
}

// SourceForEvent resolves an event_id to its source string. Used by
// the model-facing expand tool to populate the "source" field on a
// "not expandable" tool result for non-agent_session rows —
// AgentSessionRange already enforces source==agent_session, but it
// throws the source string away on the wrong-source error path.
// Returns ErrEventNotFound if no row with that id exists.
func (s *DBSink) SourceForEvent(ctx context.Context, eventID int64) (string, error) {
	var source string
	err := s.pool.QueryRow(ctx,
		`SELECT source FROM events WHERE id = $1`, eventID,
	).Scan(&source)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrEventNotFound
		}
		return "", fmt.Errorf("events: source lookup: %w", err)
	}
	return source, nil
}

// IdentityForEvent resolves the actor metadata and scope for a model-facing
// expand envelope. Deleted rows are excluded so an expansion cannot
// reintroduce a tombstoned actor after the source lookup races with
// deletion.
func (s *DBSink) IdentityForEvent(ctx context.Context, eventID int64) (EventIdentity, error) {
	var out EventIdentity
	err := s.pool.QueryRow(ctx, `
		SELECT source, scope, COALESCE(actor, ''), COALESCE(actor_canonical, ''), COALESCE(actor_id, '')
		  FROM events
		 WHERE id = $1 AND deleted = false
	`, eventID).Scan(&out.Source, &out.Scope, &out.Actor, &out.ActorCanonical, &out.ActorID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventIdentity{}, ErrEventNotFound
		}
		return EventIdentity{}, fmt.Errorf("events: identity lookup: %w", err)
	}
	return out, nil
}

// SlackEventID resolves one Slack message permalink's canonical
// (channel_id, message_ts) pair to an event id. The channel is part of the
// unique Slack external id, so this also works for legacy events that do not
// have a side-table row. Deleted events are not corpus references.
func (s *DBSink) SlackEventID(ctx context.Context, channelID, messageTS string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT id
		  FROM events
		 WHERE source = 'slack'
		   AND external_id = $1
		   AND deleted = false
	`, channelID+":"+messageTS).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrEventNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("events: slack reference lookup: %w", err)
	}
	return id, nil
}

// SlackEventContent is the exact single-message reader used when a legacy
// Slack event has no side-table metadata for the conversation expander. It is
// deliberately limited to the canonical event row; it does not infer nearby
// messages or bypass the caller's scope check in the reference adapter.
func (s *DBSink) SlackEventContent(ctx context.Context, eventID int64) (source, url, body string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT source, COALESCE(url, ''), body
		  FROM events
		 WHERE id = $1 AND source = 'slack' AND deleted = false
	`, eventID).Scan(&source, &url, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrEventNotFound
	}
	if err != nil {
		return "", "", "", fmt.Errorf("events: slack reference content lookup: %w", err)
	}
	return source, url, body, nil
}

// LinearEventID resolves a Linear issue identifier such as ENG-770 against
// the identifier-prefixed title written by the Linear connector. The query
// requires exactly one live event; a duplicate is an operational data error,
// not permission to pick whichever row happens to be first.
func (s *DBSink) LinearEventID(ctx context.Context, identifier string) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		  FROM events
		 WHERE source = 'linear'
		   AND deleted = false
		   AND upper(split_part(title, ':', 1)) = upper($1)
	`, identifier)
	if err != nil {
		return 0, fmt.Errorf("events: linear reference lookup: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("events: linear reference scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("events: linear reference rows: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, ErrEventNotFound
	case 1:
		return ids[0], nil
	default:
		return 0, ErrReferenceAmbiguous
	}
}

// LinearTeamKeys returns the distinct team keys (ENG in ENG-770) of live Linear
// issues, parsed from the same identifier-prefixed title LinearEventID matches.
// Only these teams can resolve, so they are the keys a bare issue identifier
// must carry to count as an exact reference. The source predicate is served by
// the source-leading indexes, so only the Linear slice of events is read.
func (s *DBSink) LinearTeamKeys(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT team_key
		  FROM (
			SELECT substring(upper(split_part(title, ':', 1)) FROM '^([A-Z][A-Z0-9]{1,15})-[0-9]+$') AS team_key
			  FROM events
			 WHERE source = 'linear'
			   AND deleted = false
		  ) identifiers
		 WHERE team_key IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("events: linear team keys: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("events: linear team key scan: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: linear team key rows: %w", err)
	}
	return keys, nil
}

// SessionEventID resolves a canonical session UUID to the canonical
// session-level summary event. Range chunks share session_id, so choosing the
// explicit :session row makes the UUID reference deterministic and lets the
// existing expander fetch the complete session range.
func (s *DBSink) SessionEventID(ctx context.Context, sessionID string) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		  FROM events
		 WHERE source = 'agent_session'
		   AND session_id = $1
		   AND right(external_id, 8) = ':session'
		   AND deleted = false
	`, sessionID)
	if err != nil {
		return 0, fmt.Errorf("events: session reference lookup: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("events: session reference scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("events: session reference rows: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, ErrEventNotFound
	case 1:
		return ids[0], nil
	default:
		// A canonical session UUID is globally unique; more than one
		// matching row means the historical owner/client/native-ID row
		// set has not yet been re-keyed. Refuse rather than guess.
		return 0, ErrReferenceAmbiguous
	}
}

// EventIDReference validates a direct event id against a live corpus row.
func (s *DBSink) EventIDReference(ctx context.Context, eventID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM events WHERE id = $1 AND deleted = false`, eventID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrEventNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("events: event reference lookup: %w", err)
	}
	return id, nil
}

// ChunkEventID validates a direct event_chunks id and returns its owning live
// event. The chunk itself, rather than a parent event guess, is the exact
// handle; deleted parents are intentionally unavailable.
func (s *DBSink) ChunkEventID(ctx context.Context, chunkID int64) (int64, error) {
	var eventID int64
	err := s.pool.QueryRow(ctx, `
		SELECT c.event_id
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE c.id = $1 AND e.deleted = false
	`, chunkID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrEventNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("events: chunk reference lookup: %w", err)
	}
	return eventID, nil
}

// StoredBody is an event's own title and body, for a source whose
// fuller context is the body it stored at ingest.
type StoredBody struct {
	Source string
	// Title is empty when the event has none.
	Title string
	Body  string
}

// StoredBody reads an event's source, title, and body. It returns
// ErrEventNotFound when there is no such event; the caller checks the
// source, which a re-ingest may have changed since the caller looked.
func (s *DBSink) StoredBody(ctx context.Context, eventID int64) (StoredBody, error) {
	var (
		stored StoredBody
		title  *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT source, title, body FROM events WHERE id = $1`, eventID,
	).Scan(&stored.Source, &title, &stored.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredBody{}, ErrEventNotFound
	}
	if err != nil {
		return StoredBody{}, fmt.Errorf("events: body lookup: %w", err)
	}
	if title != nil {
		stored.Title = *title
	}
	return stored, nil
}

// EventScope resolves an event_id to its visibility scope string.
// Used by the expand tool's authorization gate: expand must apply
// the same scope check as retrieval, or it becomes a bypass (any
// event_id -> raw content). Returns ErrEventNotFound if no row
// with that id exists.
func (s *DBSink) EventScope(ctx context.Context, eventID int64) (string, error) {
	var scope string
	err := s.pool.QueryRow(ctx,
		`SELECT scope FROM events WHERE id = $1`, eventID,
	).Scan(&scope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrEventNotFound
		}
		return "", fmt.Errorf("events: scope lookup: %w", err)
	}
	return scope, nil
}
