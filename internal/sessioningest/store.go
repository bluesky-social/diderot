// Package sessioningest owns diderot's side of the unified inference/
// session audit layer's "session feed → diderot discovery and summarization"
// contract. The session feed
// holds the canonical session/change clock and serves sanitized Bsky
// Session v1 revisions; diderot owns its replay cursor + the candidate
// / eligibility / published-revision state, and never touches raw audit
// records or creates a raw audit surface.
//
// Summary of the durable state (all Diderot-owned; the feed keeps no
// consumer table, lease, or cursor):
//
//   - session_ingest_cursor: the single replay cursor over the feed's
//     change clock, plus the policy_hash of the eligibility policy every
//     candidate has been checked against. A different policy is applied
//     to the candidates already here (ReapplyPolicy); the cursor never
//     moves back.
//   - session_ingest_candidates: per-session OBSERVED head state,
//     keyed on canonical session_id after the UUID cutover. The
//     mirrored descriptor is what the feed currently reports. The
//     published_* columns are the watermark of what has actually been
//     rendered into the searchable corpus — running with summarization
//     disabled NEVER advances them.
//   - session_ingest_published: an immutable ledger, one row per
//     (session_id, revision) after the UUID cutover, with superseded_at /
//     tombstoned_at so append/rewind/rewrite replace and
//     delete/policy-exclusion tombstone corpus state.
//
// The volume of work and the several scan/upsert helpers keep this
// file long by design (one transaction per durable unit).
package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

// ErrNoCandidates is returned when a selection query yields nothing.
var ErrNoCandidates = errors.New("sessioningest: no candidates")

// Store is the DB layer for the Diderot-owned session-ingest state.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SessionSummarizationEnabled reads the operator's pause: false is paused. A
// new install starts unpaused.
func (s *Store) SessionSummarizationEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT summarization_enabled FROM session_ingest_control WHERE id = true`).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("sessioningest: load summarization control: %w", err)
	}
	return enabled, nil
}

func (s *Store) SetSessionSummarizationEnabled(ctx context.Context, enabled bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE session_ingest_control SET summarization_enabled = $1, updated_at = now() WHERE id = true`, enabled)
	if err != nil {
		return fmt.Errorf("sessioningest: set summarization control: %w", err)
	}
	return nil
}

// Cursor is the single durable replay position.
type Cursor struct {
	ChangeSeq  int64
	PolicyHash string
}

// GetCursor loads the durable cursor. The row always exists (migration
// seeds id=true); a fresh install returns change_seq=0.
func (s *Store) GetCursor(ctx context.Context) (Cursor, error) {
	var c Cursor
	err := s.pool.QueryRow(ctx,
		`SELECT change_seq, policy_hash FROM session_ingest_cursor WHERE id = true`,
	).Scan(&c.ChangeSeq, &c.PolicyHash)
	if err != nil {
		return Cursor{}, fmt.Errorf("sessioningest: load cursor: %w", err)
	}
	return c, nil
}

// ErrCursorMoved is returned when a feed page was read from a position the
// stored cursor no longer holds: another writer moved it, such as an
// operator correcting it by hand. Nothing of the page is saved, and the
// next discovery pass resumes from the stored position.
var ErrCursorMoved = errors.New("sessioningest: the feed cursor moved while a page was being saved")

// RecordAppliedPolicy records that every candidate has been checked against
// the policy with this hash. It leaves the feed position alone.
func (s *Store) RecordAppliedPolicy(ctx context.Context, policyHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session_ingest_cursor
		   SET policy_hash = $1, updated_at = now()
		 WHERE id = true`,
		policyHash)
	if err != nil {
		return fmt.Errorf("sessioningest: record applied policy: %w", err)
	}
	return nil
}

// Identity addresses one canonical feed session. SessionID is the
// sole durable key; Owner and Client are provenance/authorization
// attributes echoed from the session envelope, never part of the
// session's identity. Legacy owner/client/native-ID rows were re-keyed
// to canonical session UUIDs before the durable primary keys collapsed
// to session_id alone.
type Identity struct {
	Owner     string
	Client    string
	SessionID string
}

// Candidate is one session_ingest_candidates row used by the schedule
// pass. It carries BOTH the observed head (Revision/RewriteEpoch/
// counters/UpdatedAt/Complete/Deleted) and the published watermark
// (PublishedRevision/PublishedRewrite/Published*Counters/PublishedAt).
type Candidate struct {
	Identity
	OwnerKind string
	Role      string
	OriginApp string

	Revision        string
	RewriteEpoch    int64
	ChangeSeq       int64
	RecordCount     int
	UserTurnCount   int
	ContentBytes    int64
	SourceStartedAt time.Time
	SourceUpdatedAt time.Time
	UpdatedAt       time.Time
	Complete        bool
	Deleted         bool

	PublishedRevision        string
	PublishedRewrite         int64
	PublishedRecordCount     int
	PublishedUserTurns       int
	PublishedContentBytes    int64
	PublishedAt              time.Time
	PublishedSourceStartedAt time.Time
	PublishedSourceUpdatedAt time.Time
	FirstPendingAt           *time.Time
	ExcludedAt               *time.Time
	VisibilityScope          string
}

// candidateColumns is the shared SELECT column list / scan order for
// candidate rows. Keep in sync with the scan below and LoadCandidate.
var candidateColumns = `owner, client, session_id, owner_kind, role, origin_app,
       revision, rewrite_epoch, change_seq,
       record_count, user_turn_count, content_bytes,
	       source_started_at, source_updated_at, updated_at, complete, deleted,
       published_revision, published_rewrite_epoch,
       published_record_count, published_user_turn_count, published_content_bytes,
	       published_at, published_source_started_at, published_source_updated_at,
	       first_pending_at, excluded_at, visibility_scope`

func scanCandidate(row pgx.Row) (Candidate, error) {
	var c Candidate
	var owned, observedStarted, observedUpdated, sourceStarted, sourceUpdated *time.Time
	err := row.Scan(
		&c.Owner, &c.Client, &c.SessionID, &c.OwnerKind, &c.Role, &c.OriginApp,
		&c.Revision, &c.RewriteEpoch, &c.ChangeSeq,
		&c.RecordCount, &c.UserTurnCount, &c.ContentBytes,
		&observedStarted, &observedUpdated, &c.UpdatedAt, &c.Complete, &c.Deleted,
		&c.PublishedRevision, &c.PublishedRewrite,
		&c.PublishedRecordCount, &c.PublishedUserTurns, &c.PublishedContentBytes,
		&owned, &sourceStarted, &sourceUpdated,
		&c.FirstPendingAt, &c.ExcludedAt, &c.VisibilityScope,
	)
	if owned != nil {
		c.PublishedAt = *owned
	}
	if observedStarted != nil {
		c.SourceStartedAt = *observedStarted
	}
	if observedUpdated != nil {
		c.SourceUpdatedAt = *observedUpdated
	}
	if sourceStarted != nil {
		c.PublishedSourceStartedAt = *sourceStarted
	}
	if sourceUpdated != nil {
		c.PublishedSourceUpdatedAt = *sourceUpdated
	}
	return c, err
}

// retryableIndexPredicate keeps revisions that failed indexing out of the
// bounded oldest-first batch until their backoff expires; a quarantined
// revision never re-enters. Failure state describes index_failure_revision
// only, so a new head is selectable immediately. Without this, a batch's
// worth of always-failing revisions would pin the front of the queue.
const retryableIndexPredicate = `(c.index_failure_revision <> c.revision
	        OR (c.index_quarantined_at IS NULL
	            AND (c.index_retry_at IS NULL OR c.index_retry_at <= now())))`

// indexRetryBase is the wait after a revision's first failed attempt; each
// consecutive failure doubles it. A package var so tests can shorten it.
var indexRetryBase = 10 * time.Minute

// MaxIndexFailures is how many attempts in a row one revision gets before it
// is parked: the budget the index, caption and document queues give an
// item. With the default wait that is four retries over two and a half
// hours. Parked revisions are listed on the admin Failures page, where an
// administrator can put them back.
const MaxIndexFailures = events.MaxFailures

// These conditions are true of a session_ingest_candidates row, named
// without an alias. Every count and list of sessions waiting, failing or
// given up reads them, so the pages and the worker cannot disagree.
const (
	// CandidatePendingSQL: the session's current revision is owed a summary.
	CandidatePendingSQL = `(excluded_at IS NULL AND NOT deleted AND complete AND revision <> published_revision)`
	// CandidateFailingSQL: the last attempt at the current revision failed.
	CandidateFailingSQL = `(index_failure_revision = revision AND index_last_error IS NOT NULL)`
	// CandidateParkedSQL: the current revision is parked. Nothing tries it
	// again until the session changes or an administrator retries it.
	CandidateParkedSQL = `(index_failure_revision = revision AND index_quarantined_at IS NOT NULL)`
)

// FindPendingCandidates returns genuinely-pending, admitted,
// non-excluded, complete candidate rows whose observed revision is not
// yet published and is not backing off or quarantined after an index
// failure, ordered FAIRLY (oldest pending first) within a bounded
// batch. Only rows with revision != ” AND published_revision != revision
// are selected, so a never-published revision is genuinely pending.
// Ordering by COALESCE(first_pending_at, updated_at) ASC means an old
// never-published revision sits at the front of the batch and cannot be
// starved by a steady stream of fresh heads. No cross-batch pagination is
// needed: the oldest rows are always included in the bounded window and
// published rows drop out, so every pending revision eventually reaches
// the front across ticks.
func (s *Store) FindPendingCandidates(ctx context.Context, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+candidateColumns+`
		  FROM session_ingest_candidates c
		 WHERE c.excluded_at IS NULL
		   AND c.deleted = false
		   AND c.complete = true
		   AND c.revision IS NOT NULL AND c.revision <> ''
		   AND c.published_revision <> c.revision
		   AND `+retryableIndexPredicate+`
		 ORDER BY COALESCE(c.first_pending_at, c.updated_at) ASC NULLS FIRST,
		          c.owner ASC, c.client ASC, c.session_id ASC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoCandidates
	}
	return out, nil
}

// PendingCounts summarizes the durable backlog for the pending gauges.
// Pending excludes quarantined revisions (they are parked until an
// operator or a new revision releases them) but includes revisions
// backing off after a transient failure, which are still owed a publish.
type PendingCounts struct {
	Pending     int
	BackingOff  int
	Quarantined int
}

// CountPendingCandidates returns the complete durable backlog, not merely the
// bounded batch selected by FindPendingCandidates. It intentionally uses the
// same base predicate as that selector so the gauge remains meaningful while a
// low-concurrency batch takes hours or days to drain.
func (s *Store) CountPendingCandidates(ctx context.Context) (PendingCounts, error) {
	var counts PendingCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE NOT failed OR index_quarantined_at IS NULL),
		       count(*) FILTER (WHERE failed AND index_quarantined_at IS NULL AND index_retry_at > now()),
		       count(*) FILTER (WHERE failed AND index_quarantined_at IS NOT NULL)
		  FROM (
		    SELECT index_failure_revision = revision AS failed,
		           index_quarantined_at, index_retry_at
		      FROM session_ingest_candidates
		     WHERE excluded_at IS NULL
		       AND deleted = false
		       AND complete = true
		       AND revision IS NOT NULL AND revision <> ''
		       AND published_revision <> revision
		  ) pending
	`).Scan(&counts.Pending, &counts.BackingOff, &counts.Quarantined)
	if err != nil {
		return PendingCounts{}, fmt.Errorf("sessioningest: count pending candidates: %w", err)
	}
	return counts, nil
}

// IndexFailure is the durable failure state after RecordIndexFailure.
// Recorded is false when the revision was no longer the unpublished head
// (it moved or published concurrently), in which case nothing was written.
type IndexFailure struct {
	Recorded    bool
	Count       int
	RetryAt     time.Time
	Quarantined bool
}

// RecordIndexFailure records a failed index attempt for one observed
// revision. Consecutive failures of the same revision back off
// exponentially (indexRetryBase doubling) and the MaxIndexFailures-th parks
// it, so no revision is retried without end; a failure against a different
// revision than the recorded one restarts the count. permanent parks the
// revision at once: it is for failures an immutable revision can never
// recover from. A revision parked by an outage that outlasted its retries is
// put back from the admin Failures page. The write is guarded on the
// revision still being the unpublished head, so it cannot attach stale
// failure state to newer content.
func (s *Store) RecordIndexFailure(ctx context.Context, id Identity, revision string, cause error, permanent bool) (IndexFailure, error) {
	out := IndexFailure{Recorded: true}
	var retryAt *time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE session_ingest_candidates
		   SET index_failure_count = CASE WHEN index_failure_revision = $4
		                                  THEN index_failure_count + 1 ELSE 1 END,
		       index_failure_revision = $4,
		       index_last_error = $5,
		       index_last_failed_at = now(),
		       index_quarantined_at = CASE WHEN $6 OR (CASE WHEN index_failure_revision = $4
		                                                    THEN index_failure_count + 1 ELSE 1 END) >= $8
		                                   THEN now() END,
		       index_retry_at = CASE WHEN NOT ($6 OR (CASE WHEN index_failure_revision = $4
		                                                   THEN index_failure_count + 1 ELSE 1 END) >= $8)
		                             THEN now() + make_interval(secs =>
		           $7::float8 * power(2, CASE WHEN index_failure_revision = $4
		                                      THEN index_failure_count ELSE 0 END)) END,
		       updated_in_db = now()
		 WHERE owner = $1 AND client = $2 AND session_id = $3
		   AND revision = $4 AND published_revision <> $4
		RETURNING index_failure_count, index_retry_at, index_quarantined_at IS NOT NULL
	`, id.Owner, id.Client, id.SessionID, revision, boundedIndexError(cause), permanent,
		indexRetryBase.Seconds(), MaxIndexFailures,
	).Scan(&out.Count, &retryAt, &out.Quarantined)
	if errors.Is(err, pgx.ErrNoRows) {
		return IndexFailure{}, nil
	}
	if err != nil {
		return IndexFailure{}, fmt.Errorf("sessioningest: record index failure %s: %w", id.SessionID, err)
	}
	if retryAt != nil {
		out.RetryAt = *retryAt
	}
	return out, nil
}

// boundedIndexError keeps index_last_error a short, valid-UTF-8 diagnostic.
// NUL/invalid-UTF-8 sanitizing reuses events.SanitizeText, the same rule
// the sink applies to every text column it writes, rather than a second,
// separately-maintained copy of it.
func boundedIndexError(err error) string {
	const limit = 1000
	msg := events.SanitizeText(err.Error())
	if len(msg) <= limit {
		return msg
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

// LoadCandidate loads one candidate by canonical identity.
func (s *Store) LoadCandidate(ctx context.Context, id Identity) (Candidate, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+candidateColumns+`
		   FROM session_ingest_candidates
		  WHERE owner = $1 AND client = $2 AND session_id = $3`,
		id.Owner, id.Client, id.SessionID)
	return scanCandidate(row)
}

// ReconcileCanonicalEventScopes repairs privacy-widening scope drift on the
// independently searchable session and range entries. Policy replay normally
// changes the candidate and event in one transaction, but this level-triggered
// pass makes every owner-only candidate authoritative even after manual
// corruption or a future write-path bug. The query is driven by the partial
// owner-only candidate index into events' unique identity index, so an
// every-discovery-pass check does not scan the general session corpus. Privacy
// repair is independent of whether a range has record-time evidence.
//
// Retrieval's SQL dereference gate enforces the repaired scope immediately;
// the same statement queues the existing payload worker without re-embedding.
//
// A candidate is excluded from this only when session_ingest_bindings --
// the sole durable authority for a session's current owner, where one
// exists -- disagrees that it belongs to the candidate's owner_canonical: a
// candidate row left over from a session rebind, or any other mismatch,
// must not restamp another owner's session. A candidate with no binding row
// at all is legacy (written before the bindings table existed, with no
// backfill) and still drives this, since there is nothing to disagree with.
func (s *Store) ReconcileCanonicalEventScopes(ctx context.Context) (int64, error) {
	var repaired int64
	err := s.pool.QueryRow(ctx, `
		WITH changed AS (
		UPDATE events e
		   SET scope = c.visibility_scope
		  FROM session_ingest_candidates c
		 CROSS JOIN LATERAL (
			SELECT c.session_id || ':session' AS external_id, 'native_session' AS event_kind
			UNION ALL
			SELECT c.session_id || ':records:' || s.record_start || ':' || s.record_end,
			       'native_session_chunk'
			  FROM session_ingest_chunk_summaries s
			 WHERE s.owner = c.owner AND s.client = c.client AND s.session_id = c.session_id
			   AND s.summary <> ''
		 ) desired
		 WHERE e.source = 'agent_session'
		   AND e.deleted = false
		   AND c.deleted = false
		   AND c.excluded_at IS NULL
		   AND c.published_revision <> ''
		   AND c.visibility_scope <> 'internal'
		   AND e.external_id = desired.external_id
		   AND e.raw->>'event_kind' = desired.event_kind
		   AND e.scope IS DISTINCT FROM c.visibility_scope
		   AND NOT EXISTS (SELECT 1 FROM session_ingest_bindings b
		                     WHERE b.session_id = c.session_id AND b.owner_canonical <> c.owner_canonical)
		RETURNING e.id
		), queued AS (
			UPDATE event_chunks c
			   SET scope_sync_needed = true,
			       scope_sync_attempts = 0,
			       scope_sync_next_attempt_at = now(),
			       scope_sync_last_error = NULL
			  FROM changed
			 WHERE c.event_id = changed.id
			 RETURNING c.id
		)
		SELECT count(*) FROM changed
	`).Scan(&repaired)
	if err != nil {
		return 0, fmt.Errorf("sessioningest: reconcile canonical event scopes: %w", err)
	}
	return repaired, nil
}

// ChangePolicy is the complete session-ingest policy decision persisted for a
// change-feed head. VisibilityScope is meaningful only when Admitted is true.
type ChangePolicy struct {
	Admitted        bool
	VisibilityScope string
}

const restampSessionScopeSQL = `
	UPDATE events
	   SET scope = $1
	 WHERE source = 'agent_session'
	   AND deleted = false
	   AND scope IS DISTINCT FROM $1
	   AND (
	       external_id = $2
	       OR (external_id ^@ $3 AND raw->>'event_kind' = $4)
	   )
`

// SkippedChange is one feed-page change UpsertChangesScopedAt left out
// because of a deterministic, permanent per-change refusal: its own data
// fails validateSessionChange, or its session's binding belongs to the
// other writer or another format, or its decided visibility scope is
// invalid for its owner. skippablePerChangeError recognizes every case.
// The page's other changes still commit and the cursor still advances past
// it; the caller logs it and counts it so an operator can see it.
type SkippedChange struct {
	Identity
	Err error
}

// UpsertChangesScopedAt is the durable, retry-safe write of one feed page
// read from position after. It upserts every candidate by canonical
// identity, reconciles admission (tombstone the newly excluded and the
// soft-deleted, re-enable the re-included, re-stamp the visibility of every
// active native session event), and advances the cursor to the feed's
// explicit nextCursor, all in one transaction, so a restart can't lose a
// delete or an exclusion. It returns the identities that are now excluded
// or soft-deleted; the events corpus is tombstoned for them after commit.
//
// A change is validated in Go (validateSessionChange) before the
// transaction touches it at all; a change that fails is skipped and
// reported as ErrMalformedFeedChange without ever reaching Postgres. A
// change that passes validation but still fails with a deterministic,
// permanent per-change refusal (skippablePerChangeError: a writer/format
// binding conflict, or an invalid decided visibility scope) is skipped the
// same way. Both depend only on the change itself and its already-durable
// binding, so replaying the identical page would refuse them identically
// forever, and the page's other, unrelated changes must still land. Each
// skip is reported back, never silently dropped.
//
// Any other error -- a connection failure, a serialization or deadlock
// failure, a database error that is not one of the above -- fails the
// whole page: the caller treats it as an all-or-nothing failure and the
// drain retries the whole page.
//
// The cursor advances only from after. When it holds another position the
// page is discarded with ErrCursorMoved, so a drain paging from memory can
// never overwrite a position written underneath it.
//
// A re-stamp here changes events.scope only. It does not queue a Qdrant
// payload patch, so a point's payload keeps its earlier scope until the
// session publishes again or a `diderot qdrant-audit` run marks the
// mismatch. Retrieval re-checks every hit against events.scope in Postgres,
// and that filter is what enforces the new scope from this commit on.
func (s *Store) UpsertChangesScopedAt(ctx context.Context, changes []SessionChange, after, nextCursor int64, decide func(SessionChange) ChangePolicy) ([]Identity, []SkippedChange, error) {
	if len(changes) == 0 {
		return nil, nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sessioningest: begin changes: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Taken first, so the row lock also keeps two drains from interleaving
	// their candidate writes.
	tag, err := tx.Exec(ctx, `
		UPDATE session_ingest_cursor
		   SET change_seq = $2, updated_at = now()
		 WHERE id = true AND change_seq = $1
	`, after, nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("sessioningest: persist cursor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil, fmt.Errorf("%w (page read after %d)", ErrCursorMoved, after)
	}

	var excluded []Identity
	var skipped []SkippedChange
	lockedOwners := make(map[string]struct{})
	for _, ch := range changes {
		if ch.ChangeSeq > nextCursor {
			return nil, nil, fmt.Errorf("sessioningest: next cursor %d trails change %d", nextCursor, ch.ChangeSeq)
		}
		id := Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID}
		if err := validateSessionChange(ch); err != nil {
			skipped = append(skipped, SkippedChange{Identity: id, Err: err})
			continue
		}
		wasExcluded, err := upsertCandidateTx(ctx, tx, ch, decide(ch), WriterFeed, sessiondecode.FormatBsky, lockedOwners)
		if err != nil {
			if !skippablePerChangeError(err) {
				return nil, nil, err
			}
			skipped = append(skipped, SkippedChange{Identity: id, Err: err})
			continue
		}
		if wasExcluded {
			excluded = append(excluded, id)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("sessioningest: commit changes: %w", err)
	}
	return excluded, skipped, nil
}

// PolicyBatch is the outcome of one ReapplyPolicy batch.
type PolicyBatch struct {
	// Next is the key to continue after.
	Next Identity
	// Checked and Changed count the candidates read and the ones the policy
	// decided differently from what was stored.
	Checked, Changed int
	// Done reports that the batch reached the last candidate.
	Done bool
}

// ReapplyPolicy checks one batch of the feed's candidates, in key order
// after the given identity, against the current policy, and reconciles in
// one transaction those it decides differently from what is stored: a
// session it now excludes is tombstoned, one it now admits is re-enabled,
// and one whose visibility it changes is re-stamped. Each candidate mirrors
// the descriptor the feed last reported for its session, so this reaches
// what re-reading the feed from the start would, without the feed. An
// unchanged decision writes nothing, which makes a batch safe to repeat.
//
// Deleted heads are skipped, since no policy admits them, and so are pushed
// sessions, which the feed's policy does not govern. A candidate is also
// skipped when session_ingest_bindings -- the sole durable authority for a
// session's current owner, where one exists -- disagrees that it belongs to
// the candidate's owner_canonical: a candidate left over from a session
// rebind, or any other mismatch, must not have the policy act on it, since
// the owner it names no longer holds the session. A candidate with no
// binding row at all is legacy (written before the bindings table existed,
// with no backfill) and the policy still reaches it, since there is nothing
// to disagree with.
func (s *Store) ReapplyPolicy(ctx context.Context, after Identity, limit int, decide func(SessionChange) ChangePolicy) (PolicyBatch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.owner, c.client, c.session_id, c.owner_kind, c.role, c.origin_app,
		       c.excluded_at IS NULL, c.visibility_scope, c.privacy_owner_only IS NOT NULL
		  FROM session_ingest_candidates c
		 WHERE (c.owner, c.client, c.session_id) > ($1, $2, $3)
		   AND NOT c.deleted
		   AND NOT EXISTS (SELECT 1 FROM session_ingest_bindings b
		                     WHERE b.session_id = c.session_id
		                       AND (b.writer = $5 OR b.owner_canonical <> c.owner_canonical))
		 ORDER BY c.owner, c.client, c.session_id
		 LIMIT $4
	`, after.Owner, after.Client, after.SessionID, limit, WriterPush)
	if err != nil {
		return PolicyBatch{}, fmt.Errorf("sessioningest: list candidates for policy: %w", err)
	}
	defer rows.Close()
	type decided struct {
		id       Identity
		admitted bool
		scope    string
	}
	batch := PolicyBatch{Next: after}
	var changed []decided
	for rows.Next() {
		var (
			ch             SessionChange
			active, pinned bool
			scope          string
		)
		if err := rows.Scan(&ch.Owner, &ch.Client, &ch.SessionID, &ch.OwnerKind, &ch.Role, &ch.OriginApp,
			&active, &scope, &pinned); err != nil {
			return PolicyBatch{}, fmt.Errorf("sessioningest: scan candidate for policy: %w", err)
		}
		batch.Checked++
		batch.Next = Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID}
		decision := decide(ch)
		// The owner's own choice pins a session's visibility over the policy's.
		if decision.Admitted != active || (decision.Admitted && !pinned && decision.VisibilityScope != scope) {
			changed = append(changed, decided{batch.Next, decision.Admitted, decision.VisibilityScope})
		}
	}
	if err := rows.Err(); err != nil {
		return PolicyBatch{}, fmt.Errorf("sessioningest: iterate candidates for policy: %w", err)
	}
	batch.Done = batch.Checked < limit
	if len(changed) == 0 {
		return batch, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PolicyBatch{}, fmt.Errorf("sessioningest: begin policy batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockedOwners := make(map[string]struct{})
	for _, d := range changed {
		canonicalOwner := CanonicalOwner(d.id.Owner)
		if _, ok := lockedOwners[canonicalOwner]; !ok {
			if err := lockOwnerPrivacyTx(ctx, tx, canonicalOwner); err != nil {
				return PolicyBatch{}, err
			}
			lockedOwners[canonicalOwner] = struct{}{}
		}
		scope := d.scope
		if !d.admitted {
			scope = events.ScopeInternal
		} else if scope != events.ScopeInternal && scope != events.ScopeOwner(canonicalOwner) {
			return PolicyBatch{}, fmt.Errorf("sessioningest: invalid visibility scope %q for owner %q", scope, d.id.Owner)
		}
		// Read under the owner's lock, so a choice the owner made since the
		// batch was listed still wins. The binding check is defense in
		// depth: the listing above already excludes a candidate that
		// disagrees with it, but the write never trusts (owner, client,
		// session_id) alone either. A candidate with no binding row at all
		// (legacy) still has nothing to disagree with, so it still writes.
		var effectiveScope string
		if err := tx.QueryRow(ctx, `
			UPDATE session_ingest_candidates c
			   SET visibility_scope = CASE c.privacy_owner_only
			           WHEN true THEN 'owner:' || c.owner_canonical
			           WHEN false THEN 'internal'
			           ELSE $4
			       END,
			       updated_in_db = now()
			 WHERE c.owner = $1 AND c.client = $2 AND c.session_id = $3
			   AND NOT EXISTS (
			       SELECT 1 FROM session_ingest_bindings b
			        WHERE b.session_id = c.session_id AND b.owner_canonical <> c.owner_canonical
			   )
			RETURNING c.visibility_scope
		`, d.id.Owner, d.id.Client, d.id.SessionID, scope).Scan(&effectiveScope); err != nil {
			return PolicyBatch{}, fmt.Errorf("sessioningest: apply policy to %s: %w", d.id.SessionID, err)
		}
		if err := reconcileAdmissionTx(ctx, tx, d.id, d.admitted, effectiveScope); err != nil {
			return PolicyBatch{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PolicyBatch{}, fmt.Errorf("sessioningest: commit policy batch: %w", err)
	}
	batch.Changed = len(changed)
	return batch, nil
}

// upsertCandidateTx is the one candidate write every writer shares: the
// feed for each change it drains, push for each accepted revision. It binds
// the session id to its writer, owner, and format first (bindSessionTx),
// then mirrors the head and reconciles admission in the caller's
// transaction. It reports whether the head is now excluded or deleted, so
// the caller can tombstone the corpus after commit.
func upsertCandidateTx(ctx context.Context, tx pgx.Tx, ch SessionChange, decision ChangePolicy, writer, format string, lockedOwners map[string]struct{}) (bool, error) {
	admitted := !ch.Deleted && decision.Admitted
	visibilityScope := decision.VisibilityScope
	canonicalOwner := CanonicalOwner(ch.Owner)
	if _, ok := lockedOwners[canonicalOwner]; !ok {
		if err := lockOwnerPrivacyTx(ctx, tx, canonicalOwner); err != nil {
			return false, err
		}
		lockedOwners[canonicalOwner] = struct{}{}
	}
	if _, err := bindSessionTx(ctx, tx, ch.SessionID, canonicalOwner, writer, format); err != nil {
		return false, err
	}
	// Retire every other candidate row for this session_id that disagrees
	// with the binding we just confirmed or moved. This has to run on
	// every feed write, not only a reported rebind (bindSessionTx's
	// Inserted branch), because a session_id can carry a stale row from
	// before that write even when THIS write only inserts or confirms the
	// binding: a session the feed wrote before the bindings table existed
	// (it was never backfilled) and later reassigned, where
	// the reassignment's own bindSessionTx call already saw Inserted (no
	// prior binding to report as moved) rather than a rebind. Gating this
	// on bindSessionTx's Rebound flag alone misses exactly that case, the
	// likeliest target of a real owner correction, so this instead always
	// looks for a disagreeing row and locks every owner it finds before
	// retiring it. Push can never create or discover a disagreeing row
	// (ErrSessionOwnerMismatch), so this only runs for the feed.
	if writer == WriterFeed {
		staleOwners, err := staleCandidateOwnersTx(ctx, tx, ch.SessionID, canonicalOwner)
		if err != nil {
			return false, err
		}
		for _, owner := range staleOwners {
			if _, ok := lockedOwners[owner]; !ok {
				if err := lockOwnerPrivacyTx(ctx, tx, owner); err != nil {
					return false, err
				}
				lockedOwners[owner] = struct{}{}
			}
		}
		if len(staleOwners) > 0 {
			if err := retireRebindStaleCandidatesTx(ctx, tx, ch.SessionID, canonicalOwner); err != nil {
				return false, err
			}
		}
	}
	if !admitted {
		visibilityScope = events.ScopeInternal
	} else if visibilityScope != events.ScopeInternal && visibilityScope != events.ScopeOwner(canonicalOwner) {
		return false, fmt.Errorf("%w: scope %q for owner %q", ErrInvalidVisibilityScope, visibilityScope, ch.Owner)
	}
	var effectiveVisibilityScope string
	if err := tx.QueryRow(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, owner_kind, role, origin_app,
		    revision, rewrite_epoch, change_seq,
		    record_count, user_turn_count, content_bytes,
		    source_started_at, source_updated_at,
		    updated_at, complete, deleted, visibility_scope, updated_in_db,
		    owner_canonical
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18, now(),$19)
		ON CONFLICT ON CONSTRAINT session_ingest_candidates_pkey DO UPDATE SET
		    owner_kind = EXCLUDED.owner_kind,
		    role = EXCLUDED.role,
		    origin_app = EXCLUDED.origin_app,
		    revision = EXCLUDED.revision,
		    rewrite_epoch = EXCLUDED.rewrite_epoch,
		    change_seq = EXCLUDED.change_seq,
		    record_count = EXCLUDED.record_count,
		    user_turn_count = EXCLUDED.user_turn_count,
		    content_bytes = EXCLUDED.content_bytes,
		    source_started_at = EXCLUDED.source_started_at,
		    source_updated_at = EXCLUDED.source_updated_at,
		    updated_at = EXCLUDED.updated_at,
		    complete = EXCLUDED.complete,
		    deleted = EXCLUDED.deleted,
		    visibility_scope = CASE session_ingest_candidates.privacy_owner_only
		        WHEN true THEN 'owner:' || EXCLUDED.owner_canonical
		        WHEN false THEN 'internal'
		        ELSE EXCLUDED.visibility_scope
		    END,
		    owner_canonical = EXCLUDED.owner_canonical,
		    updated_in_db = now()
		RETURNING visibility_scope
	`, ch.Owner, ch.Client, ch.SessionID, ch.OwnerKind, ch.Role, ch.OriginApp,
		ch.Revision, ch.RewriteEpoch, ch.ChangeSeq,
		ch.RecordCount, ch.UserTurnCount, ch.ContentBytes,
		nullableTime(ch.SourceStartedAt), nullableTime(ch.SourceUpdatedAt),
		ch.UpdatedAt, ch.Complete, ch.Deleted, visibilityScope, canonicalOwner).Scan(&effectiveVisibilityScope); err != nil {
		return false, fmt.Errorf("sessioningest: upsert change %s: %w", ch.SessionID, err)
	}
	id := Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID}
	if err := reconcileAdmissionTx(ctx, tx, id, admitted, effectiveVisibilityScope); err != nil {
		return false, err
	}
	return !admitted, nil
}

// staleCandidateOwnersTx returns the distinct owner_canonical of every
// session_ingest_candidates row for sessionID that disagrees with
// newOwnerCanonical, so the caller can lock each one before
// retireRebindStaleCandidatesTx deletes it. Backed by the plain session_id
// index (session_ingest_candidates_session_id): cheap even when, as on
// every feed write, it usually finds nothing.
func staleCandidateOwnersTx(ctx context.Context, tx pgx.Tx, sessionID, newOwnerCanonical string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT owner_canonical FROM session_ingest_candidates
		 WHERE session_id = $1 AND owner_canonical <> $2
	`, sessionID, newOwnerCanonical)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list stale candidate owners %s: %w", sessionID, err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("sessioningest: scan stale candidate owner %s: %w", sessionID, err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate stale candidate owners %s: %w", sessionID, err)
	}
	return owners, nil
}

// retireRebindStaleCandidatesTx runs whenever upsertCandidateTx's feed write
// finds a session_ingest_candidates row for sessionID under an owner other
// than newOwnerCanonical. Candidates are keyed on the composite (owner,
// client, session_id); a rebind changes owner, so upsertCandidateTx's own
// INSERT ... ON CONFLICT targets that key and creates a fresh row for the
// new owner instead of updating the old one. Left alone, that former-owner
// row would still satisfy applySessionPrivacyTx's ownership check
// (owner_canonical/client/session_id) and ReconcileCanonicalEventScopes'/
// ReapplyPolicy's candidate scan, letting the former owner keep acting on a
// session they no longer own. This can happen even on a write whose own
// bindSessionTx call only inserted or confirmed the binding (no rebind
// reported this time): a session the feed wrote before the bindings table
// existed, with no backfill, where an EARLIER write already moved the
// candidate's owner with no binding row yet in place to record it as a
// rebind. Checking for a disagreeing row on every feed write, rather than
// trusting bindSessionTx's own report, is what catches that case.
//
// This deletes every candidate row for sessionID whose owner_canonical
// disagrees with newOwnerCanonical -- normally just one, but also any
// earlier duplicate the same gap left behind -- and its per-identity
// derived state (summaries, chunk summaries, map checkpoints), and marks
// its published ledger rows superseded so nothing re-derives from them. It
// never touches the new owner's row, so no explicit privacy choice carries
// across the rebind: the new owner's candidate is the fresh INSERT from
// upsertCandidateTx, which leaves privacy_owner_only unset.
func retireRebindStaleCandidatesTx(ctx context.Context, tx pgx.Tx, sessionID, newOwnerCanonical string) error {
	_, err := tx.Exec(ctx, `
		WITH stale AS (
			DELETE FROM session_ingest_candidates
			 WHERE session_id = $1 AND owner_canonical <> $2
			RETURNING owner, client
		), del_summaries AS (
			DELETE FROM session_ingest_summaries s
			 USING stale
			 WHERE s.session_id = $1 AND s.owner = stale.owner AND s.client = stale.client
			RETURNING 1
		), del_chunk_summaries AS (
			DELETE FROM session_ingest_chunk_summaries s
			 USING stale
			 WHERE s.session_id = $1 AND s.owner = stale.owner AND s.client = stale.client
			RETURNING 1
		), del_map_checkpoints AS (
			DELETE FROM session_ingest_map_checkpoints m
			 USING stale
			 WHERE m.session_id = $1 AND m.owner = stale.owner AND m.client = stale.client
			RETURNING 1
		), superseded AS (
			UPDATE session_ingest_published p
			   SET superseded_at = now()
			  FROM stale
			 WHERE p.session_id = $1 AND p.owner = stale.owner AND p.client = stale.client
			   AND p.superseded_at IS NULL AND p.tombstoned_at IS NULL
			RETURNING 1
		)
		SELECT count(*) FROM stale
	`, sessionID, newOwnerCanonical)
	if err != nil {
		return fmt.Errorf("sessioningest: retire rebind-stale candidates %s: %w", sessionID, err)
	}
	return nil
}

// reconcileAdmissionTx makes a candidate's ledger and events agree with its
// admission: an excluded or deleted head is tombstoned, an admitted one is
// re-enabled and its events carry effectiveScope, the candidate's stored
// visibility. The feed drain runs it for every head it mirrors, and
// ReapplyPolicy for every head a changed policy decides differently.
func reconcileAdmissionTx(ctx context.Context, tx pgx.Tx, id Identity, admitted bool, effectiveScope string) error {
	if !admitted {
		if _, err := tx.Exec(ctx, `DELETE FROM session_ingest_map_checkpoints
			WHERE session_id=$1 AND owner=$2 AND client=$3`, id.SessionID, id.Owner, id.Client); err != nil {
			return fmt.Errorf("sessioningest: clear excluded map checkpoints: %w", err)
		}
		// Newly excluded (policy change) or soft-deleted head:
		// atomically tombstone the candidate AND its published
		// ledger state. If it was already excluded this is a
		// no-op; re-inclusion clears excluded_at in the else arm.
		// The caller then tombstones the events
		// corpus too (separate table, after commit).
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_candidates
			   SET excluded_at = COALESCE(excluded_at, now()),
			       first_pending_at = NULL
			 WHERE owner = $1 AND client = $2 AND session_id = $3
		`, id.Owner, id.Client, id.SessionID); err != nil {
			return fmt.Errorf("sessioningest: exclude change %s: %w", id.SessionID, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_published
			   SET tombstoned_at = COALESCE(tombstoned_at, now())
			 WHERE owner = $1 AND client = $2 AND session_id = $3
			   AND tombstoned_at IS NULL
		`, id.Owner, id.Client, id.SessionID); err != nil {
			return fmt.Errorf("sessioningest: tombstone excluded indexes %s: %w", id.SessionID, err)
		}
		// Mark corpus-tombstone work outstanding IN THIS TX so a
		// crash after commit re-tombstones on restart — the events
		// corpus (a separate table outside the ledger tx) is driven
		// after commit via ListPendingCorpusTombstones, so cursor
		// advancement can never lose a delete/exclusion.
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_candidates
			   SET corpus_tombstone_pending = COALESCE(corpus_tombstone_pending, now())
			 WHERE owner = $1 AND client = $2 AND session_id = $3
		`, id.Owner, id.Client, id.SessionID); err != nil {
			return fmt.Errorf("sessioningest: mark corpus tombstone pending %s: %w", id.SessionID, err)
		}
	} else {
		// Restamp every searchable event belonging to this canonical
		// native session. The exact final id plus the event-kind guard on
		// the range prefix prevent touching legacy or colliding rows.
		if _, err := tx.Exec(ctx, restampSessionScopeSQL, effectiveScope,
			externalID(id), rangeExternalIDPrefix(id),
			chunkEventKind); err != nil {
			return fmt.Errorf("sessioningest: restamp session scope %s: %w", id.SessionID, err)
		}
		// Admitted: make sure a previously-excluded session is
		// re-enabled, and clear any stale tombstone flag only if
		// the head came back alive (re-inclusion). Since we flip
		// admission per the latest descriptor, once a session is
		// admitted again it may schedule a fresh publish.
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_candidates
			   SET excluded_at = NULL,
			       corpus_tombstone_pending = NULL,
			       published_revision = CASE WHEN excluded_at IS NOT NULL THEN '' ELSE published_revision END,
			       first_pending_at = CASE WHEN excluded_at IS NOT NULL THEN now() ELSE first_pending_at END
			 WHERE owner = $1 AND client = $2 AND session_id = $3
		`, id.Owner, id.Client, id.SessionID); err != nil {
			return fmt.Errorf("sessioningest: re-enable change %s: %w", id.SessionID, err)
		}
	}
	return nil
}

// MarkPending stamps first_pending_at when it is not already set, but
// ONLY for a genuinely-new unsummarized (unpublished) revision. A
// session whose observed revision is already published never gets its
// pending age started, so a quiet no-op can't consume maxPendingFor.
// Returns whether this call newly started the pending clock.
func (s *Store) MarkPending(ctx context.Context, id Identity, observedRevision string) (bool, error) {
	var started bool
	err := s.pool.QueryRow(ctx, `
		UPDATE session_ingest_candidates
		   SET first_pending_at = COALESCE(first_pending_at, now())
		 WHERE owner = $1 AND client = $2 AND session_id = $3
		   AND excluded_at IS NULL AND deleted = false
		   AND revision = $4 AND published_revision <> $4
		   AND first_pending_at IS NULL
		 RETURNING (first_pending_at IS NOT NULL)
	`, id.Owner, id.Client, id.SessionID, observedRevision).Scan(&started)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("sessioningest: mark pending: %w", err)
	}
	return started, nil
}

// errCASStale is returned by PublishRevisionTx when the fetched revision
// is no longer the session's current candidate. Callers (the atomic
// corpus publisher) treat it as an abort signal: the enclosing events
// tx rolls back, so a stale run can never change the visible corpus.
var errCASStale = errors.New("sessioningest: publish CAS rejected (stale fetch)")

// PublishRevision runs PublishRevisionTx in its own transaction
// (standalone path used by tests and reconciliation).
func (s *Store) PublishRevision(ctx context.Context, id Identity, revision string, rewriteEpoch int64, contentDigest string) (PublishResult, error) {
	candidate, err := s.LoadCandidate(ctx, id)
	if err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: publish load source time: %w", err)
	}
	if candidate.SourceStartedAt.IsZero() || candidate.SourceUpdatedAt.IsZero() ||
		candidate.SourceUpdatedAt.Before(candidate.SourceStartedAt) {
		return PublishResult{}, fmt.Errorf("sessioningest: publish candidate requires ordered source timestamps")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: publish begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := s.PublishRevisionTx(ctx, tx, id, revision, rewriteEpoch, contentDigest,
		candidate.SourceStartedAt, candidate.SourceUpdatedAt, candidate.VisibilityScope)
	if errors.Is(err, errCASStale) {
		// Roll back the (empty) CAS tx; nothing was written.
		return PublishResult{Stale: true}, nil
	}
	if err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: publish commit: %w", err)
	}
	return res, nil
}

// PublishRevisionTx is the repository-native, in-transaction CAS that
// atomically:
//
//  1. Reloads the candidate inside the tx and compare-and-swaps that
//     the observed (revision, rewrite_epoch, deleted, excluded) state
//     still matches what the worker fetched. An older fetch can never
//     replace a newer head: if the advertised revision advanced past
//     what we fetched, it returns errCASStale and the enclosing
//     transaction (including the corpus event write) rolls back.
//  2. Supersedes the session's prior published revision.
//  3. Inserts the newly-published revision ledger row.
//  4. Advances the candidate's published_* watermark to this revision.
//
// The caller passes an already-open tx (typically the events sink's
// UpsertWithSideWrite transaction) so the corpus event and the ledger
// land atomically: a stale CAS aborts the corpus write too. Stale runs
// may populate reusable chunk cache but can never replace the visible
// current corpus.
func (s *Store) PublishRevisionTx(ctx context.Context, tx pgx.Tx, id Identity, revision string, rewriteEpoch int64, contentDigest string, sourceStartedAt, sourceUpdatedAt time.Time, visibilityScope string) (PublishResult, error) {
	if sourceStartedAt.IsZero() || sourceUpdatedAt.IsZero() || sourceUpdatedAt.Before(sourceStartedAt) {
		return PublishResult{}, fmt.Errorf("sessioningest: publish requires ordered source timestamps")
	}
	var (
		curRevision                        string
		curEpoch                           int64
		curDeleted                         bool
		curExcluded                        *time.Time
		curSourceStarted, curSourceUpdated *time.Time
		curVisibilityScope                 string
	)
	err := tx.QueryRow(ctx, `
		SELECT revision, rewrite_epoch, deleted, excluded_at,
		       source_started_at, source_updated_at, visibility_scope
		  FROM session_ingest_candidates
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 FOR UPDATE
	`, id.Owner, id.Client, id.SessionID).Scan(
		&curRevision, &curEpoch, &curDeleted, &curExcluded,
		&curSourceStarted, &curSourceUpdated, &curVisibilityScope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PublishResult{}, fmt.Errorf("sessioningest: publish: candidate not found for %+v", id)
		}
		return PublishResult{}, fmt.Errorf("sessioningest: publish load: %w", err)
	}
	if curDeleted || curExcluded != nil {
		// Already tombstoned; don't publish stale content.
		return PublishResult{Stale: true}, errCASStale
	}
	if curRevision != revision || curEpoch != rewriteEpoch {
		// An older fetch can never replace a newer head.
		return PublishResult{Stale: true}, errCASStale
	}
	if curSourceStarted == nil || curSourceUpdated == nil ||
		!curSourceStarted.Equal(sourceStartedAt) ||
		!curSourceUpdated.Equal(sourceUpdatedAt) ||
		curVisibilityScope != visibilityScope {
		return PublishResult{Stale: true}, errCASStale
	}

	if _, err := tx.Exec(ctx, `
		UPDATE session_ingest_published
		   SET superseded_at = now()
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		   AND tombstoned_at IS NULL AND superseded_at IS NULL AND revision <> $4
	`, id.Owner, id.Client, id.SessionID, revision); err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: supersede prior: %w", err)
	}

	var fresh bool
	err = tx.QueryRow(ctx, `
		INSERT INTO session_ingest_published
		    (owner, client, session_id, revision, rewrite_epoch, content_digest,
		     source_started_at, source_updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT ON CONSTRAINT session_ingest_published_pkey DO UPDATE SET
		    content_digest = EXCLUDED.content_digest,
		    source_started_at = EXCLUDED.source_started_at,
		    source_updated_at = EXCLUDED.source_updated_at,
		    published_at = now(),
		    superseded_at = NULL,
		    tombstoned_at = NULL
		RETURNING (xmax = 0)
	`, id.Owner, id.Client, id.SessionID, revision, rewriteEpoch, contentDigest,
		sourceStartedAt, sourceUpdatedAt).Scan(&fresh)
	if err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: insert published: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE session_ingest_candidates
		   SET published_revision = $4,
		       published_rewrite_epoch = $5,
		       published_record_count = record_count,
		       published_user_turn_count = user_turn_count,
		       published_content_bytes = content_bytes,
		       published_source_started_at = $6,
		       published_source_updated_at = $7,
		       published_at = now(),
		       first_pending_at = NULL,
		       excluded_at = NULL,
		       corpus_tombstone_pending = NULL
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, id.Owner, id.Client, id.SessionID, revision, rewriteEpoch,
		sourceStartedAt, sourceUpdatedAt); err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: advance published watermark: %w", err)
	}

	return PublishResult{Published: fresh}, nil
}

// ListPendingCorpusTombstones returns the oldest 100 canonical identities whose
// corpus tombstone work is still outstanding (excluded/soft-deleted but
// the events-corpus removal has not yet completed). The marker is set in
// the SAME transaction as the cursor advance, so a crash after commit
// re-tombstones on restart — cursor advancement can never lose a
// delete/exclusion.
func (s *Store) ListPendingCorpusTombstones(ctx context.Context) ([]Identity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT owner, client, session_id
		  FROM session_ingest_candidates
		 WHERE corpus_tombstone_pending IS NOT NULL
		 ORDER BY corpus_tombstone_pending, session_id
		 LIMIT 100
	`)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list pending tombstones: %w", err)
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Owner, &id.Client, &id.SessionID); err != nil {
			return nil, fmt.Errorf("sessioningest: scan pending tombstone: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate pending tombstones: %w", err)
	}
	return out, nil
}

// ClearCorpusTombstone marks a session's corpus tombstone as completed
// (called only after the events-corpus removal succeeds).
func (s *Store) ClearCorpusTombstone(ctx context.Context, id Identity) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE session_ingest_candidates
		   SET corpus_tombstone_pending = NULL
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, id.Owner, id.Client, id.SessionID)
	if err != nil {
		return fmt.Errorf("sessioningest: clear corpus tombstone %s: %w", id.SessionID, err)
	}
	return nil
}

// LoadPublishedRevision reports whether a (identity, revision) is the
// currently-active published revision (used by tests and reconciliation).
func (s *Store) LoadPublishedRevision(ctx context.Context, id Identity, revision string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM session_ingest_published
		 WHERE owner=$1 AND client=$2 AND session_id=$3 AND revision=$4
		   AND tombstoned_at IS NULL AND superseded_at IS NULL
	`, id.Owner, id.Client, id.SessionID, revision).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- repository-native summarized state (canonical identity) ------------

// SummaryRecord is one persisted chunk-summary row (Bsky record range).
type SummaryRecord struct {
	Identity
	RecordStart int // inclusive
	RecordEnd   int // exclusive
	ContentHash string
	MapDigest   string
	Evidence    []string
	Summary     string
	ModelID     string
	OccurredAt  time.Time
}

// UpsertSummaryRecord persists one canonical-identity chunk summary for
// a (session, record range). Idempotent on the range.
func (s *Store) UpsertSummaryRecord(ctx context.Context, r SummaryRecord) error {
	if r.Evidence == nil {
		r.Evidence = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_ingest_chunk_summaries
		    (owner, client, session_id, record_start, record_end,
		     content_hash, map_input_digest, evidence, summary, model_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT ON CONSTRAINT session_ingest_chunk_summaries_pkey DO UPDATE SET
		    record_end    = EXCLUDED.record_end,
		    content_hash  = EXCLUDED.content_hash,
		    map_input_digest = EXCLUDED.map_input_digest,
		    evidence      = EXCLUDED.evidence,
		    summary       = EXCLUDED.summary,
		    model_id      = EXCLUDED.model_id,
		    created_at    = now()
	`, r.Owner, r.Client, r.SessionID, r.RecordStart, r.RecordEnd,
		r.ContentHash, r.MapDigest, r.Evidence, r.Summary, r.ModelID)
	if err != nil {
		return fmt.Errorf("sessioningest: upsert summary record: %w", err)
	}
	return nil
}

// ListSummaryRecords returns the persisted chunk summaries for one
// canonical session, ordered by record_start.
func (s *Store) ListSummaryRecords(ctx context.Context, id Identity) ([]SummaryRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT owner, client, session_id, record_start, record_end,
		       content_hash, map_input_digest, evidence, summary, model_id
		  FROM session_ingest_chunk_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 ORDER BY record_start ASC
	`, id.Owner, id.Client, id.SessionID)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list summary records: %w", err)
	}
	defer rows.Close()
	var out []SummaryRecord
	for rows.Next() {
		var r SummaryRecord
		if err := rows.Scan(&r.Owner, &r.Client, &r.SessionID, &r.RecordStart,
			&r.RecordEnd, &r.ContentHash, &r.MapDigest, &r.Evidence, &r.Summary, &r.ModelID); err != nil {
			return nil, fmt.Errorf("sessioningest: scan summary record: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate summary records: %w", err)
	}
	return out, nil
}

func (s *Store) CheckpointMapChunk(ctx context.Context, record SummaryRecord, revision string, rewriteEpoch int64) error {
	if record.Evidence == nil {
		record.Evidence = []string{}
	}
	result, err := s.pool.Exec(ctx, `
		INSERT INTO session_ingest_map_checkpoints
		    (session_id, record_start, owner, client, record_end, content_hash,
		     map_input_digest, evidence, summary, model_id)
		SELECT $3, $4, $1, $2, $5, $6, $7, $8, $9, $10
		  FROM session_ingest_candidates
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		   AND revision=$11 AND rewrite_epoch=$12
		   AND published_revision<>$11
		   AND deleted=false AND excluded_at IS NULL AND complete=true
		 FOR UPDATE
		ON CONFLICT (session_id, record_start) DO UPDATE SET
		    record_end=EXCLUDED.record_end, content_hash=EXCLUDED.content_hash,
		    map_input_digest=EXCLUDED.map_input_digest, evidence=EXCLUDED.evidence,
		    summary=EXCLUDED.summary, model_id=EXCLUDED.model_id
		WHERE session_ingest_map_checkpoints.owner=EXCLUDED.owner
		  AND session_ingest_map_checkpoints.client=EXCLUDED.client
	`, record.Owner, record.Client, record.SessionID, record.RecordStart, record.RecordEnd,
		record.ContentHash, record.MapDigest, record.Evidence, record.Summary, record.ModelID,
		revision, rewriteEpoch)
	if err != nil {
		return fmt.Errorf("sessioningest: checkpoint map chunk: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errCASStale
	}
	return nil
}

// LoadReusableMapChunks returns independently reusable published chunks and
// unfinished map checkpoints. Reduce-only changes do not invalidate them.
func (s *Store) LoadReusableMapChunks(ctx context.Context, id Identity, mapDigest string, recordCount int, expected []sessionsummary.Chunk, mapModel string) (map[int]SummaryRecord, error) {
	if mapDigest == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH cached AS (
		    SELECT owner, client, session_id, record_start, record_end,
		           content_hash, map_input_digest, evidence, summary, model_id, 0 AS priority
		      FROM session_ingest_chunk_summaries
		    UNION ALL
		    SELECT owner, client, session_id, record_start, record_end,
		           content_hash, map_input_digest, evidence, summary, model_id, 1 AS priority
		      FROM session_ingest_map_checkpoints
		)
		SELECT owner, client, session_id, record_start, record_end,
		       content_hash, map_input_digest, evidence, summary, model_id
		  FROM cached
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		   AND map_input_digest=$4 AND model_id=$5
		 ORDER BY priority
	`, id.Owner, id.Client, id.SessionID, mapDigest, mapModel)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list reusable map chunks: %w", err)
	}
	defer rows.Close()
	indices := make(map[int]int, len(expected))
	for index, chunk := range expected {
		indices[chunk.RecordStart] = index
	}
	chunks := make(map[int]SummaryRecord)
	for rows.Next() {
		var chunk SummaryRecord
		if err := rows.Scan(&chunk.Owner, &chunk.Client, &chunk.SessionID,
			&chunk.RecordStart, &chunk.RecordEnd, &chunk.ContentHash, &chunk.MapDigest,
			&chunk.Evidence, &chunk.Summary, &chunk.ModelID); err != nil {
			return nil, fmt.Errorf("sessioningest: scan reusable map chunk: %w", err)
		}
		index, wanted := indices[chunk.RecordStart]
		if !wanted || chunk.RecordStart < 0 || chunk.RecordEnd > recordCount ||
			chunk.RecordEnd != expected[index].RecordEnd || chunk.ContentHash != shaHex(expected[index].Bytes) ||
			strings.TrimSpace(chunk.Summary) == "" {
			continue
		}
		if _, exists := chunks[index]; !exists {
			chunks[index] = chunk
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate reusable map chunks: %w", err)
	}
	return chunks, nil
}

// LoadReusableSummary returns the current persisted map/reduce result only
// when it was produced from the exact same summary-input identity. Legacy
// rows have an empty digest and are intentionally never adopted: their prompt
// and model inputs cannot be proven after the fact.
func (s *Store) LoadReusableSummary(ctx context.Context, id Identity, inputDigest string, recordCount int, expected []sessionsummary.Chunk, mapModel string) (SummaryResult, bool, error) {
	if inputDigest == "" {
		return SummaryResult{}, false, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: begin reusable summary read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var result SummaryResult
	var storedRecordCount int
	err = tx.QueryRow(ctx, `
		SELECT narrative, COALESCE(embedding_text,''), COALESCE(context_hint,''),
		       tags, model_id, input_tokens, output_tokens, record_count,
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title END, ''),
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title_model END, '')
		  FROM session_ingest_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		   AND input_digest=$4 AND record_count=$5
	`, id.Owner, id.Client, id.SessionID, inputDigest, recordCount).Scan(
		&result.Narrative, &result.EmbeddingText, &result.ContextHint,
		&result.Tags, &result.ModelID, &result.InputTokens, &result.OutputTokens,
		&storedRecordCount, &result.Title, &result.TitleModel,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SummaryResult{}, false, nil
	}
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: load reusable summary: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT owner, client, session_id, record_start, record_end,
		       content_hash, map_input_digest, evidence, summary, model_id
		  FROM session_ingest_chunk_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 ORDER BY record_start ASC
	`, id.Owner, id.Client, id.SessionID)
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: list reusable summary records: %w", err)
	}
	defer rows.Close()
	var chunks []SummaryRecord
	for rows.Next() {
		var chunk SummaryRecord
		if err := rows.Scan(&chunk.Owner, &chunk.Client, &chunk.SessionID,
			&chunk.RecordStart, &chunk.RecordEnd, &chunk.ContentHash, &chunk.MapDigest,
			&chunk.Evidence, &chunk.Summary, &chunk.ModelID); err != nil {
			return SummaryResult{}, false, fmt.Errorf("sessioningest: scan reusable summary record: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: iterate reusable summary records: %w", err)
	}
	rows.Close()
	if strings.TrimSpace(result.Narrative) == "" || !validReusableSummaryChunks(chunks, storedRecordCount) ||
		len(chunks) != len(expected) || result.ModelID != mapModel {
		return SummaryResult{}, false, nil
	}
	for i, chunk := range chunks {
		want := expected[i]
		if chunk.RecordStart != want.RecordStart || chunk.RecordEnd != want.RecordEnd ||
			chunk.ContentHash != shaHex(want.Bytes) || chunk.ModelID != mapModel {
			return SummaryResult{}, false, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: commit reusable summary read: %w", err)
	}
	result.InputDigest = inputDigest
	result.Chunks = chunks
	return result, true, nil
}

// EarlierSummary is a session's published summary with the chunk set it
// was made from. The summarizer folds it into the next summary instead of
// reading those chunks' summaries again.
type EarlierSummary struct {
	Narrative     string
	EmbeddingText string
	ContextHint   string
	Tags          []string
	InputDigest   string
	ModelID       string
	RecordCount   int
	Chunks        []SummaryRecord
}

// LoadEarlierSummary reads the published summary and its chunks in one
// snapshot (both are replaced in the same publish transaction). ok is false
// when the session has no published summary.
func (s *Store) LoadEarlierSummary(ctx context.Context, id Identity) (EarlierSummary, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return EarlierSummary{}, false, fmt.Errorf("sessioningest: begin earlier summary read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var earlier EarlierSummary
	err = tx.QueryRow(ctx, `
		SELECT narrative, COALESCE(embedding_text,''), COALESCE(context_hint,''), tags,
		       input_digest, model_id, record_count
		  FROM session_ingest_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, id.Owner, id.Client, id.SessionID).Scan(&earlier.Narrative, &earlier.EmbeddingText,
		&earlier.ContextHint, &earlier.Tags, &earlier.InputDigest, &earlier.ModelID, &earlier.RecordCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return EarlierSummary{}, false, nil
	}
	if err != nil {
		return EarlierSummary{}, false, fmt.Errorf("sessioningest: load earlier summary: %w", err)
	}
	if earlier.Chunks, err = publishedChunks(ctx, tx, id); err != nil {
		return EarlierSummary{}, false, err
	}
	return earlier, true, nil
}

// publishedChunks reads a session's published chunk summaries, in order.
func publishedChunks(ctx context.Context, q pgxQuerier, id Identity) ([]SummaryRecord, error) {
	rows, err := q.Query(ctx, `
		SELECT owner, client, session_id, record_start, record_end,
		       content_hash, map_input_digest, evidence, summary, model_id
		  FROM session_ingest_chunk_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 ORDER BY record_start ASC
	`, id.Owner, id.Client, id.SessionID)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list published summary chunks: %w", err)
	}
	defer rows.Close()
	var chunks []SummaryRecord
	for rows.Next() {
		var chunk SummaryRecord
		if err := rows.Scan(&chunk.Owner, &chunk.Client, &chunk.SessionID,
			&chunk.RecordStart, &chunk.RecordEnd, &chunk.ContentHash, &chunk.MapDigest,
			&chunk.Evidence, &chunk.Summary, &chunk.ModelID); err != nil {
			return nil, fmt.Errorf("sessioningest: scan published summary chunk: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate published summary chunks: %w", err)
	}
	return chunks, nil
}

// AdoptLegacySummary stamps exact pre-digest cache rows with the current
// map/reduce input identities without changing their content or historical
// timestamps. The caller must first prove that the active prompts and models
// are the known legacy inputs. This transaction independently proves the
// remaining provenance: canonical identity, model, complete record coverage,
// exact recomputed ranges, and exact sanitized-content hashes.
//
// Locking the summary row first serializes concurrent adopters and the atomic
// summary-replacement path, which uses the same summary-then-chunks order.
func (s *Store) AdoptLegacySummary(ctx context.Context, id Identity, inputDigest, mapDigest string, recordCount int, expected []sessionsummary.Chunk, mapModel string) (SummaryResult, bool, error) {
	if inputDigest == "" || mapDigest == "" || recordCount <= 0 || len(expected) == 0 || mapModel == "" {
		return SummaryResult{}, false, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: begin legacy summary adoption: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		result            SummaryResult
		storedRecordCount int
		storedDigest      string
	)
	err = tx.QueryRow(ctx, `
		SELECT narrative, COALESCE(embedding_text,''), COALESCE(context_hint,''),
		       tags, model_id, input_tokens, output_tokens, record_count,
		       input_digest,
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title END, ''),
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title_model END, '')
		  FROM session_ingest_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 FOR UPDATE
	`, id.Owner, id.Client, id.SessionID).Scan(
		&result.Narrative, &result.EmbeddingText, &result.ContextHint,
		&result.Tags, &result.ModelID, &result.InputTokens, &result.OutputTokens,
		&storedRecordCount, &storedDigest, &result.Title, &result.TitleModel,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SummaryResult{}, false, nil
	}
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: lock legacy summary: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT owner, client, session_id, record_start, record_end,
		       content_hash, map_input_digest, evidence, summary, model_id
		  FROM session_ingest_chunk_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 ORDER BY record_start ASC
		 FOR UPDATE
	`, id.Owner, id.Client, id.SessionID)
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: lock legacy summary chunks: %w", err)
	}
	var chunks []SummaryRecord
	for rows.Next() {
		var chunk SummaryRecord
		if err := rows.Scan(&chunk.Owner, &chunk.Client, &chunk.SessionID,
			&chunk.RecordStart, &chunk.RecordEnd, &chunk.ContentHash, &chunk.MapDigest,
			&chunk.Evidence, &chunk.Summary, &chunk.ModelID); err != nil {
			rows.Close()
			return SummaryResult{}, false, fmt.Errorf("sessioningest: scan legacy summary chunk: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SummaryResult{}, false, fmt.Errorf("sessioningest: iterate legacy summary chunks: %w", err)
	}
	rows.Close()

	if strings.TrimSpace(result.Narrative) == "" || storedRecordCount != recordCount ||
		result.ModelID != mapModel || !validReusableSummaryChunks(chunks, recordCount) ||
		len(chunks) != len(expected) {
		return SummaryResult{}, false, nil
	}
	alreadyAdopted := storedDigest == inputDigest
	if storedDigest != "" && !alreadyAdopted {
		return SummaryResult{}, false, nil
	}
	for i, chunk := range chunks {
		want := expected[i]
		if chunk.RecordStart != want.RecordStart || chunk.RecordEnd != want.RecordEnd ||
			chunk.ContentHash != shaHex(want.Bytes) || chunk.ModelID != mapModel {
			return SummaryResult{}, false, nil
		}
		if alreadyAdopted {
			if chunk.MapDigest != mapDigest {
				return SummaryResult{}, false, nil
			}
		} else if chunk.MapDigest != "" {
			return SummaryResult{}, false, nil
		}
	}

	if !alreadyAdopted {
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_summaries
			   SET input_digest=$4
			 WHERE owner=$1 AND client=$2 AND session_id=$3 AND input_digest=''
		`, id.Owner, id.Client, id.SessionID, inputDigest); err != nil {
			return SummaryResult{}, false, fmt.Errorf("sessioningest: stamp legacy summary digest: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE session_ingest_chunk_summaries
			   SET map_input_digest=$4
			 WHERE owner=$1 AND client=$2 AND session_id=$3 AND map_input_digest=''
		`, id.Owner, id.Client, id.SessionID, mapDigest); err != nil {
			return SummaryResult{}, false, fmt.Errorf("sessioningest: stamp legacy map digests: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: commit legacy summary adoption: %w", err)
	}
	for i := range chunks {
		chunks[i].MapDigest = mapDigest
	}
	result.InputDigest = inputDigest
	result.Chunks = chunks
	return result, true, nil
}

func validReusableSummaryChunks(chunks []SummaryRecord, recordCount int) bool {
	if len(chunks) == 0 || recordCount <= 0 {
		return false
	}
	next := 0
	for _, chunk := range chunks {
		if chunk.RecordStart != next || chunk.RecordEnd <= chunk.RecordStart ||
			chunk.RecordEnd > recordCount || chunk.ContentHash == "" || strings.TrimSpace(chunk.Summary) == "" {
			return false
		}
		next = chunk.RecordEnd
	}
	return next == recordCount
}

// ChunkEventItem is one persisted native map-summary chunk selected
// for corpus publication, stamped with the candidate's currently
// PUBLISHED revision + rewrite epoch (paired — never the observed
// epoch of a newer head). The events content must reflect exactly the
// published state: session_ingest_chunk_summaries rows are written in
// the same transaction that advances the published watermark, so
// they always describe the published state, and stamping an
// observed-but-unpublished head would make the corpus claim content
// that was never canonically published.
type ChunkEventItem struct {
	Identity
	Revision        string
	RewriteEpoch    int64
	Chunk           SummaryRecord
	SourceUpdatedAt time.Time
	VisibilityScope string
}

// errChunkCASStale is returned by AssertChunkCurrentTx when the
// candidate / chunk-summary state the reconciler rendered no longer
// matches the current durable state (a newer publish or an exclusion
// landed between selection and the events write). The enclosing events
// transaction rolls back, so a superseded revision can never publish
// (or resurrect) chunk content.
var errChunkCASStale = errors.New("sessioningest: chunk publish CAS rejected (stale state)")

// AssertChunkCurrentTx is the in-transaction CAS for chunk-event
// publication. Inside the caller's events transaction it verifies,
// under FOR UPDATE, that:
//
//  1. The session's candidate still advertises the published revision
//     AND published rewrite epoch being rendered, and is not excluded
//     or soft-deleted.
//  2. The chunk summary row for (identity, record_start) still covers
//     the same record_end with the same content hash, model id, and
//     summary body.
//
// Either mismatch returns errChunkCASStale and the enclosing events
// transaction aborts, so the corpus can never drift ahead of (or lag
// behind) the durable candidate/summary state.
func (s *Store) AssertChunkCurrentTx(ctx context.Context, tx pgx.Tx, id Identity, revision string, rewriteEpoch int64, sourceUpdatedAt time.Time, visibilityScope string, chunk SummaryRecord) error {
	var (
		curRevision        string
		curPublishEpoch    int64
		curDeleted         bool
		curExcluded        *time.Time
		curSourceUpdatedAt *time.Time
		curVisibilityScope string
	)
	err := tx.QueryRow(ctx, `
		SELECT published_revision, published_rewrite_epoch, deleted, excluded_at,
		       published_source_updated_at, visibility_scope
		  FROM session_ingest_candidates
		 WHERE owner=$1 AND client=$2 AND session_id=$3
		 FOR UPDATE
	`, id.Owner, id.Client, id.SessionID).Scan(&curRevision, &curPublishEpoch,
		&curDeleted, &curExcluded, &curSourceUpdatedAt, &curVisibilityScope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errChunkCASStale
		}
		return fmt.Errorf("sessioningest: chunk CAS load candidate: %w", err)
	}
	if curDeleted || curExcluded != nil || curRevision != revision || curPublishEpoch != rewriteEpoch ||
		curSourceUpdatedAt == nil || !curSourceUpdatedAt.Equal(sourceUpdatedAt) ||
		curVisibilityScope != visibilityScope {
		return errChunkCASStale
	}

	var (
		curHash  string
		curEnd   int
		curModel string
		curBody  string
		curTime  *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT content_hash, record_end, model_id, summary, occurred_at
		  FROM session_ingest_chunk_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3 AND record_start=$4
	`, id.Owner, id.Client, id.SessionID, chunk.RecordStart).Scan(&curHash, &curEnd, &curModel, &curBody, &curTime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errChunkCASStale
		}
		return fmt.Errorf("sessioningest: chunk CAS load summary row: %w", err)
	}
	if curHash != chunk.ContentHash || curEnd != chunk.RecordEnd ||
		curModel != chunk.ModelID || curBody != chunk.Summary || curTime == nil || !curTime.Equal(chunk.OccurredAt) {
		return errChunkCASStale
	}
	return nil
}

// SelectChunkEventsToPublish returns up to limit desired native
// map-summary chunk events whose corpus row is missing, previously
// tombstoned, or stale relative to the current candidate/summary
// state. A row is stale when the revision/rewrite epoch, the range
// coordinates, the summary body, or the exact raw content_hash /
// model_id metadata drifted from the current chunk summary. Purely
// level-triggered: recomputed from durable state every tick, so a
// crash or CAS rejection mid-pass re-selects the remainder on the next
// tick without any queue or cursor.
//
// A chunk is desired when its session is published (published_revision
// set), not excluded or soft-deleted, and the summary text is
// non-empty (an empty map summary is not searchable content).
func (s *Store) SelectChunkEventsToPublish(ctx context.Context, limit int) ([]ChunkEventItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH desired AS (
		    SELECT c.owner, c.client, c.session_id,
		           c.published_revision, c.published_rewrite_epoch,
		           s.record_start, s.record_end, s.content_hash,
		           s.summary, s.model_id, s.occurred_at, c.published_source_updated_at,
		           c.visibility_scope
		      FROM session_ingest_candidates c
		      JOIN session_ingest_chunk_summaries s
		        ON s.owner = c.owner AND s.client = c.client
		       AND s.session_id = c.session_id
		     WHERE c.published_revision <> ''
		       AND c.published_source_updated_at IS NOT NULL
		       AND c.deleted = false AND c.excluded_at IS NULL
		       AND s.summary <> ''
		       AND s.occurred_at IS NOT NULL
		)
		SELECT d.owner, d.client, d.session_id,
		       d.published_revision, d.published_rewrite_epoch,
		       d.record_start, d.record_end, d.content_hash,
		       d.summary, d.model_id, d.occurred_at, d.published_source_updated_at,
		       d.visibility_scope
		  FROM desired d
		  LEFT JOIN events e
		    ON e.source = 'agent_session'
		   AND e.external_id = d.session_id
		                       || ':records:' || d.record_start || ':' || d.record_end
		 WHERE e.id IS NULL
		    OR e.deleted
		    OR e.raw->>'event_kind' IS DISTINCT FROM 'native_session_chunk'
		    OR e.raw->>'revision'   IS DISTINCT FROM d.published_revision
		    OR coalesce((e.raw->>'rewrite_epoch')::bigint, -1) <> d.published_rewrite_epoch
		    OR e.raw->>'content_hash' IS DISTINCT FROM d.content_hash
		    OR e.raw->>'model_id'     IS DISTINCT FROM d.model_id
		    OR coalesce(e.record_start, -1) <> d.record_start
		    OR coalesce(e.record_end, -1)   <> d.record_end
		    OR e.body IS DISTINCT FROM d.summary
		    OR e.scope IS DISTINCT FROM d.visibility_scope
		    OR e.occurred_at IS DISTINCT FROM d.occurred_at
		 -- Scope drift is an active audience-policy violation, while the
		 -- other predicates are content freshness work. Repair every
		 -- privacy mismatch ahead of ordinary churn so a bounded batch
		 -- cannot indefinitely starve a range event at the wider scope.
		 ORDER BY (e.scope IS DISTINCT FROM d.visibility_scope) DESC,
		          d.owner, d.client, d.session_id, d.record_start
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: select chunk events to publish: %w", err)
	}
	defer rows.Close()
	var out []ChunkEventItem
	for rows.Next() {
		var it ChunkEventItem
		if err := rows.Scan(&it.Owner, &it.Client, &it.SessionID,
			&it.Revision, &it.RewriteEpoch,
			&it.Chunk.RecordStart, &it.Chunk.RecordEnd, &it.Chunk.ContentHash,
			&it.Chunk.Summary, &it.Chunk.ModelID, &it.Chunk.OccurredAt, &it.SourceUpdatedAt,
			&it.VisibilityScope); err != nil {
			return nil, fmt.Errorf("sessioningest: scan chunk event item: %w", err)
		}
		it.Chunk.Identity = it.Identity
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate chunk event items: %w", err)
	}
	return out, nil
}

// SelectObsoleteRangeEvents returns up to limit external ids of
// reconciler-managed native range events (raw.event_kind =
// native_session_chunk) whose range no longer corresponds to any
// desired chunk of its session — a chunk whose boundaries changed, a
// chunk summary that was removed, or a session that stopped being
// publishable (excluded / soft-deleted / never published). The
// canonical final :session event and every legacy event are excluded
// by the event_kind discriminator, never by string matching.
func (s *Store) SelectObsoleteRangeEvents(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH desired AS (
		    SELECT c.session_id, s.record_start, s.record_end
		      FROM session_ingest_candidates c
		      JOIN session_ingest_chunk_summaries s
		        ON s.owner = c.owner AND s.client = c.client
		       AND s.session_id = c.session_id
		     WHERE c.published_revision <> ''
		       AND c.deleted = false AND c.excluded_at IS NULL
		       AND s.summary <> ''
		)
		SELECT e.external_id
		  FROM events e
		 WHERE e.source = 'agent_session'
		   AND e.deleted = false
		   AND e.raw->>'event_kind' = 'native_session_chunk'
		   AND e.external_id NOT IN (
		        SELECT d.session_id || ':records:' || d.record_start || ':' || d.record_end
		          FROM desired d
		   )
		 ORDER BY e.id
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: select obsolete range events: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			return nil, fmt.Errorf("sessioningest: scan obsolete range event: %w", err)
		}
		out = append(out, ext)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: iterate obsolete range events: %w", err)
	}
	return out, nil
}

// SessionSummaryDoc is the persisted reduce-stage summary for one
// canonical session revision.
type SessionSummaryDoc struct {
	Identity
	Revision      string
	RewriteEpoch  int64
	InputDigest   string
	Narrative     string
	EmbeddingText string
	Title         string
	TitleModel    string
	ContextHint   string
	Tags          []string
	ModelID       string
	InputTokens   int32
	OutputTokens  int32
	RecordCount   int
}

// UpsertSessionSummary persists the canonical summary for a session
// (replaced on each publish).
func (s *Store) UpsertSessionSummary(ctx context.Context, d SessionSummaryDoc) error {
	if d.Tags == nil {
		d.Tags = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_ingest_summaries
		    (owner, client, session_id, revision, rewrite_epoch, input_digest, narrative,
		     embedding_text, context_hint, tags, model_id, input_tokens,
		     output_tokens, record_count, title, title_model, title_input_digest,
		     title_generated_at, summarized_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,
		        NULLIF($15,''), NULLIF($16,''), CASE WHEN $15='' THEN NULL ELSE $6 END,
		        CASE WHEN $15='' THEN NULL ELSE now() END, now())
		ON CONFLICT ON CONSTRAINT session_ingest_summaries_pkey DO UPDATE SET
		    revision       = EXCLUDED.revision,
		    rewrite_epoch  = EXCLUDED.rewrite_epoch,
		    input_digest   = EXCLUDED.input_digest,
		    narrative      = EXCLUDED.narrative,
		    embedding_text = EXCLUDED.embedding_text,
		    context_hint   = EXCLUDED.context_hint,
		    tags           = EXCLUDED.tags,
		    model_id       = EXCLUDED.model_id,
		    input_tokens   = EXCLUDED.input_tokens,
		    output_tokens  = EXCLUDED.output_tokens,
		    record_count   = EXCLUDED.record_count,
		    title = COALESCE(EXCLUDED.title, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title
		    END),
		    title_model = COALESCE(EXCLUDED.title_model, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_model
		    END),
		    title_input_digest = COALESCE(EXCLUDED.title_input_digest, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_input_digest
		    END),
		    title_generated_at = COALESCE(EXCLUDED.title_generated_at, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_generated_at
		    END),
		    title_attempted_at = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN NULL
		        ELSE session_ingest_summaries.title_attempted_at
		    END,
		    title_failure_count = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN 0
		        ELSE session_ingest_summaries.title_failure_count
		    END,
		    title_last_error = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN NULL
		        ELSE session_ingest_summaries.title_last_error
		    END,
		    summarized_at  = now()
	`, d.Owner, d.Client, d.SessionID, d.Revision, d.RewriteEpoch, d.InputDigest,
		d.Narrative, nullableString(d.EmbeddingText), nullableString(d.ContextHint),
		d.Tags, d.ModelID, d.InputTokens, d.OutputTokens, d.RecordCount,
		d.Title, d.TitleModel)
	if err != nil {
		return fmt.Errorf("sessioningest: upsert session summary: %w", err)
	}
	return nil
}

// ReplaceSummaryTx replaces the visible summary metadata and its complete
// half-open record-range chunk set in the caller's publication transaction.
// The caller must run the candidate CAS in the same transaction first.
func (s *Store) ReplaceSummaryTx(ctx context.Context, tx pgx.Tx, d SessionSummaryDoc, chunks []SummaryRecord) error {
	if d.Tags == nil {
		d.Tags = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_ingest_summaries
		    (owner, client, session_id, revision, rewrite_epoch, input_digest, narrative,
		     embedding_text, context_hint, tags, model_id, input_tokens,
		     output_tokens, record_count, title, title_model, title_input_digest,
		     title_generated_at, summarized_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,
		        NULLIF($15,''), NULLIF($16,''), CASE WHEN $15='' THEN NULL ELSE $6 END,
		        CASE WHEN $15='' THEN NULL ELSE now() END, now())
		ON CONFLICT ON CONSTRAINT session_ingest_summaries_pkey DO UPDATE SET
		    revision = EXCLUDED.revision, rewrite_epoch = EXCLUDED.rewrite_epoch,
		    input_digest = EXCLUDED.input_digest,
		    narrative = EXCLUDED.narrative, embedding_text = EXCLUDED.embedding_text,
		    context_hint = EXCLUDED.context_hint, tags = EXCLUDED.tags,
		    model_id = EXCLUDED.model_id, input_tokens = EXCLUDED.input_tokens,
		    output_tokens = EXCLUDED.output_tokens, record_count = EXCLUDED.record_count,
		    title = COALESCE(EXCLUDED.title, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title
		    END),
		    title_model = COALESCE(EXCLUDED.title_model, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_model
		    END),
		    title_input_digest = COALESCE(EXCLUDED.title_input_digest, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_input_digest
		    END),
		    title_generated_at = COALESCE(EXCLUDED.title_generated_at, CASE
		        WHEN session_ingest_summaries.input_digest = EXCLUDED.input_digest THEN session_ingest_summaries.title_generated_at
		    END),
		    title_attempted_at = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN NULL
		        ELSE session_ingest_summaries.title_attempted_at
		    END,
		    title_failure_count = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN 0
		        ELSE session_ingest_summaries.title_failure_count
		    END,
		    title_last_error = CASE
		        WHEN session_ingest_summaries.input_digest IS DISTINCT FROM EXCLUDED.input_digest THEN NULL
		        ELSE session_ingest_summaries.title_last_error
		    END,
		    summarized_at = now()
	`, d.Owner, d.Client, d.SessionID, d.Revision, d.RewriteEpoch, d.InputDigest,
		d.Narrative, nullableString(d.EmbeddingText), nullableString(d.ContextHint),
		d.Tags, d.ModelID, d.InputTokens, d.OutputTokens, d.RecordCount,
		d.Title, d.TitleModel); err != nil {
		return fmt.Errorf("sessioningest: replace session summary: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_ingest_chunk_summaries
		WHERE owner=$1 AND client=$2 AND session_id=$3`, d.Owner, d.Client, d.SessionID); err != nil {
		return fmt.Errorf("sessioningest: clear prior summary chunks: %w", err)
	}
	for _, chunk := range chunks {
		if chunk.RecordStart < 0 || chunk.RecordEnd <= chunk.RecordStart || chunk.RecordEnd > d.RecordCount {
			return fmt.Errorf("sessioningest: invalid summary range [%d,%d) for %d records", chunk.RecordStart, chunk.RecordEnd, d.RecordCount)
		}
		if chunk.Evidence == nil {
			chunk.Evidence = []string{}
		}
		if chunk.OccurredAt.IsZero() {
			return fmt.Errorf("sessioningest: summary range [%d,%d) missing record time", chunk.RecordStart, chunk.RecordEnd)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_ingest_chunk_summaries
			    (owner, client, session_id, record_start, record_end,
			     content_hash, map_input_digest, evidence, summary, model_id, occurred_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now())
		`, d.Owner, d.Client, d.SessionID, chunk.RecordStart, chunk.RecordEnd,
			chunk.ContentHash, chunk.MapDigest, chunk.Evidence, chunk.Summary, chunk.ModelID, chunk.OccurredAt); err != nil {
			return fmt.Errorf("sessioningest: insert summary range [%d,%d): %w", chunk.RecordStart, chunk.RecordEnd, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_ingest_map_checkpoints
		WHERE session_id=$1 AND owner=$2 AND client=$3`, d.SessionID, d.Owner, d.Client); err != nil {
		return fmt.Errorf("sessioningest: clear published map checkpoints: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
