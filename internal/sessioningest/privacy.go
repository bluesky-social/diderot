package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/events"
)

const (
	defaultOwnerSessionPageSize = 50
	maxOwnerSessionPageSize     = 200
)

var (
	ErrInvalidSessionSelection = errors.New("invalid session selection")
	ErrSessionNotFound         = errors.New("selected session not found")
)

// OwnerSession is the content-free descriptor exposed by the self-service
// session inventory. Transcripts and summaries stay in their owning systems.
type OwnerSession struct {
	Client          string     `json:"client"`
	SessionID       string     `json:"session_id"`
	Title           string     `json:"title,omitempty"`
	Role            string     `json:"role"`
	OriginApp       string     `json:"origin_app"`
	ChangeSeq       int64      `json:"change_seq"`
	RecordCount     int        `json:"record_count"`
	UserTurnCount   int        `json:"user_turn_count"`
	SourceStartedAt *time.Time `json:"source_started_at,omitempty"`
	SourceUpdatedAt *time.Time `json:"source_updated_at,omitempty"`
	Complete        bool       `json:"complete"`
	Deleted         bool       `json:"deleted"`
	Excluded        bool       `json:"excluded"`
	Indexed         bool       `json:"indexed"`
	// SummaryFailed reports that the session's current revision could not
	// be summarized and is no longer being tried.
	SummaryFailed   bool `json:"summary_failed"`
	OwnerOnly       bool `json:"owner_only"`
	PrivacyExplicit bool `json:"privacy_explicit"`
}

type OwnerSessionPage struct {
	Sessions   []OwnerSession `json:"sessions"`
	NextBefore int64          `json:"next_before,omitempty"`
}

type SessionRef struct {
	Client    string `json:"client"`
	SessionID string `json:"session_id"`
}

type SessionPrivacyUpdate struct {
	OwnerOnly        bool  `json:"owner_only"`
	SessionUpdates   int   `json:"session_updates"`
	CandidateUpdates int64 `json:"candidate_updates"`
	EventUpdates     int64 `json:"event_updates"`
}

type PrivacyService struct {
	store *Store
}

func NewPrivacyService(store *Store, basePolicy Policy) (*PrivacyService, error) {
	if store == nil || store.pool == nil {
		return nil, fmt.Errorf("sessioningest: privacy store is required")
	}
	if err := basePolicy.Validate(); err != nil {
		return nil, err
	}
	return &PrivacyService{store: store}, nil
}

// CanonicalOwner applies the same email normalization as owner-scope
// resolution. Callers must still source the value from a verified identity.
func CanonicalOwner(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}

func (s *PrivacyService) SetSessions(ctx context.Context, owner string, sessions []SessionRef, ownerOnly bool) (SessionPrivacyUpdate, error) {
	owner = CanonicalOwner(owner)
	if owner == "" {
		return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: owner identity is required")
	}
	if len(sessions) == 0 || len(sessions) > maxOwnerSessionPageSize {
		return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: %w: select between 1 and %d sessions", ErrInvalidSessionSelection, maxOwnerSessionPageSize)
	}
	seen := make(map[SessionRef]struct{}, len(sessions))
	for _, session := range sessions {
		if strings.TrimSpace(session.Client) == "" || strings.TrimSpace(session.SessionID) == "" {
			return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: %w: client and session_id are required", ErrInvalidSessionSelection)
		}
		if _, ok := seen[session]; ok {
			return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: %w: duplicate session selection", ErrInvalidSessionSelection)
		}
		seen[session] = struct{}{}
	}
	return s.store.setSessionPrivacy(ctx, owner, sessions, ownerOnly)
}

func (s *PrivacyService) ListSessions(ctx context.Context, owner string, limit int, before int64) (OwnerSessionPage, error) {
	owner = CanonicalOwner(owner)
	if owner == "" {
		return OwnerSessionPage{}, fmt.Errorf("sessioningest: owner identity is required")
	}
	if limit <= 0 {
		limit = defaultOwnerSessionPageSize
	}
	if limit > maxOwnerSessionPageSize {
		limit = maxOwnerSessionPageSize
	}
	return s.store.listOwnerSessions(ctx, owner, limit, before)
}

func (s *Store) setSessionPrivacy(ctx context.Context, owner string, sessions []SessionRef, ownerOnly bool) (SessionPrivacyUpdate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: begin session privacy update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOwnerPrivacyTx(ctx, tx, owner); err != nil {
		return SessionPrivacyUpdate{}, err
	}
	result := SessionPrivacyUpdate{OwnerOnly: ownerOnly}
	for _, session := range sessions {
		if err := applySessionPrivacyTx(ctx, tx, owner, session, ownerOnly, &result); err != nil {
			return SessionPrivacyUpdate{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionPrivacyUpdate{}, fmt.Errorf("sessioningest: commit session privacy update: %w", err)
	}
	return result, nil
}

// applySessionPrivacyTx restamps one owned session's candidate and indexed
// events and audits the change. The caller holds the owner's privacy lock.
func applySessionPrivacyTx(ctx context.Context, tx pgx.Tx, owner string, session SessionRef, ownerOnly bool, result *SessionPrivacyUpdate) error {
	scope := events.ScopeInternal
	if ownerOnly {
		scope = events.ScopeOwner(owner)
	}
	// Defense in depth: act through a candidate only when
	// session_ingest_bindings -- the sole durable authority for a
	// session's current owner, where one exists -- does not disagree
	// that it belongs to owner. A candidate with no binding row at all
	// is a legacy one (the feed wrote it before the bindings table
	// existed, and it was never backfilled) and there is
	// nothing to disagree with, so it still passes. Without this guard,
	// a candidate row that disagrees with an existing binding (a stale
	// row a rebind missed, a restore, a future write-path bug) would
	// still satisfy the plain owner_canonical/client/session_id match
	// and let a former owner restamp the current owner's session.
	tag, err := tx.Exec(ctx, `
		UPDATE session_ingest_candidates c
		   SET privacy_owner_only = $4, visibility_scope = $5, updated_in_db = now()
		 WHERE c.owner_canonical = $1 AND c.client = $2 AND c.session_id = $3
		   AND NOT EXISTS (
		       SELECT 1 FROM session_ingest_bindings b
		        WHERE b.session_id = c.session_id AND b.owner_canonical <> $1
		   )
	`, owner, session.Client, session.SessionID, ownerOnly, scope)
	if err != nil {
		return fmt.Errorf("sessioningest: restamp session candidate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("sessioningest: %w: %s/%s", ErrSessionNotFound, session.Client, session.SessionID)
	}
	result.CandidateUpdates++
	eventTag, err := tx.Exec(ctx, `
			UPDATE events
			   SET scope = $2
			 WHERE source = 'agent_session' AND deleted = false
			   AND scope IS DISTINCT FROM $2
			   AND (
			       (external_id = $1 || ':session'
			        AND raw->>'event_kind' = 'native_session')
			       OR
			       (starts_with(external_id, $1 || ':records:')
			        AND raw->>'event_kind' = 'native_session_chunk')
			   )
	`, session.SessionID, scope)
	if err != nil {
		return fmt.Errorf("sessioningest: restamp session events: %w", err)
	}
	result.EventUpdates += eventTag.RowsAffected()
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_privacy_audit
		       (owner, client, session_id, owner_only, actor, event_updates)
		VALUES ($1, $2, $3, $4, $1, $5)
	`, owner, session.Client, session.SessionID, ownerOnly, eventTag.RowsAffected()); err != nil {
		return fmt.Errorf("sessioningest: audit session privacy: %w", err)
	}
	result.SessionUpdates++
	return nil
}

func (s *Store) listOwnerSessions(ctx context.Context, owner string, limit int, before int64) (OwnerSessionPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT candidate.client, candidate.session_id, COALESCE(summary.title, ''),
		       candidate.role, candidate.origin_app, candidate.change_seq,
		       candidate.record_count, candidate.user_turn_count,
		       candidate.source_started_at, candidate.source_updated_at,
		       candidate.complete, candidate.deleted, candidate.excluded_at IS NOT NULL,
		       candidate.published_revision <> '',
		       candidate.published_revision <> candidate.revision
		         AND candidate.index_failure_revision = candidate.revision
		         AND candidate.index_quarantined_at IS NOT NULL,
		       candidate.visibility_scope = 'owner:' || candidate.owner_canonical,
		       candidate.privacy_owner_only IS NOT NULL
		  FROM session_ingest_candidates candidate
		  LEFT JOIN session_ingest_summaries summary
		    ON summary.owner = candidate.owner
		   AND summary.client = candidate.client
		   AND summary.session_id = candidate.session_id
		   AND summary.title_input_digest = summary.input_digest
		 WHERE candidate.owner_canonical = $1
		   AND ($2::bigint = 0 OR candidate.change_seq < $2)
		 ORDER BY candidate.change_seq DESC
		 LIMIT $3
	`, owner, before, limit+1)
	if err != nil {
		return OwnerSessionPage{}, fmt.Errorf("sessioningest: list owner sessions: %w", err)
	}
	defer rows.Close()
	var page OwnerSessionPage
	for rows.Next() {
		var item OwnerSession
		if err := rows.Scan(&item.Client, &item.SessionID, &item.Title, &item.Role, &item.OriginApp,
			&item.ChangeSeq, &item.RecordCount, &item.UserTurnCount,
			&item.SourceStartedAt, &item.SourceUpdatedAt, &item.Complete,
			&item.Deleted, &item.Excluded, &item.Indexed, &item.SummaryFailed,
			&item.OwnerOnly, &item.PrivacyExplicit); err != nil {
			return OwnerSessionPage{}, fmt.Errorf("sessioningest: scan owner session: %w", err)
		}
		page.Sessions = append(page.Sessions, item)
	}
	if err := rows.Err(); err != nil {
		return OwnerSessionPage{}, fmt.Errorf("sessioningest: iterate owner sessions: %w", err)
	}
	if len(page.Sessions) > limit {
		page.Sessions = page.Sessions[:limit]
		page.NextBefore = page.Sessions[len(page.Sessions)-1].ChangeSeq
	}
	return page, nil
}

func lockOwnerPrivacyTx(ctx context.Context, tx pgx.Tx, owner string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('diderot:session-owner-privacy:' || $1, 0))`, owner); err != nil {
		return fmt.Errorf("sessioningest: lock owner privacy: %w", err)
	}
	return nil
}
