// Package ask is the agentic Q&A layer over the diderot corpus.
// Multi-turn sessions persist to Postgres (ask_sessions, ask_messages,
// ask_slack_sessions). This file owns shared session metadata and
// session-level concurrency control; AgenticMessage persistence lives in
// session_agentic.go.
//
// Data model:
//
//   - One ask_sessions row per logical conversation.
//   - One ask_messages row per Eino AgenticMessage (role: user|assistant).
//     Tool calls and results are typed content blocks within those rows.
//   - ask_slack_sessions maps a Slack (channel_id, thread_ts) to a
//     session UUID so follow-ups in the same thread continue the
//     conversation. Other transports (MCP) get their own mapping.
package ask

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// Session is the stored shape of an ask_sessions row.
type Session struct {
	ID         uuid.UUID
	CreatedAt  time.Time
	LastTurnAt time.Time
	FinishedAt *time.Time // nil = unfinished/continuable; UI activity is time-windowed
}

// Manager owns ask_* table reads + writes and the per-session lock that
// serializes concurrent turns: in process, then across processes through a
// lease in ask_session_leases.
type Manager struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	// Per-session in-memory channel-based lock keyed by session UUID.
	// Channel of size 1: send acquires, recv releases. Wrapped in
	// `*sessionLock` with a refCount so we can drop the entry when
	// no goroutine is holding or waiting, preventing the map from
	// growing unboundedly across sessions seen over the service's
	// lifetime. Outer mu guards the map; the per-session ch is
	// released without holding mu so a slow turn doesn't block other
	// sessions from acquiring their own locks.
	mu    sync.Mutex
	locks map[uuid.UUID]*sessionLock

	// lease times the session leases this process takes.
	lease sessionLeaseTiming
}

type sessionLock struct {
	ch       chan struct{}
	refCount int // bumped on AcquireLock entry, decremented on release
}

// NewManager constructs a Manager. logger may be nil (falls back to
// slog.Default).
func NewManager(pool *pgxpool.Pool, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		pool:   pool,
		logger: logger,
		locks:  make(map[uuid.UUID]*sessionLock),
		lease:  defaultSessionLeaseTiming,
	}
}

// Create inserts a new ask_sessions row and returns it. Used by the
// HTTP /v1/ask path on first call (when no session_id is provided).
// The Slack path uses LoadOrCreateForSlackThread instead.
func (m *Manager) Create(ctx context.Context) (*Session, error) {
	var s Session
	err := m.pool.QueryRow(ctx,
		`INSERT INTO ask_sessions DEFAULT VALUES
		 RETURNING id, created_at, last_turn_at, finished_at`,
	).Scan(&s.ID, &s.CreatedAt, &s.LastTurnAt, &s.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &s, nil
}

// CreateOwned inserts a session owned by a verified caller. Agent-facing
// surfaces use this instead of creating an ownerless row and stamping it in a
// second query, so a failed request can never leave a session with a weakened
// continuation boundary.
func (m *Manager) CreateOwned(ctx context.Context, ownerEmail string) (*Session, error) {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	if ownerEmail == "" {
		return nil, errors.New("create owned session: owner email is required")
	}
	var s Session
	err := m.pool.QueryRow(ctx,
		`INSERT INTO ask_sessions (owner_email) VALUES ($1)
		 RETURNING id, created_at, last_turn_at, finished_at`,
		ownerEmail,
	).Scan(&s.ID, &s.CreatedAt, &s.LastTurnAt, &s.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("create owned session: %w", err)
	}
	return &s, nil
}

// OwnsSession reports whether ownerEmail may continue sessionID and read its
// artifacts: the rule HTTP /v1/ask applies (PersonalSessionOwnedBy).
func (m *Manager) OwnsSession(ctx context.Context, sessionID uuid.UUID, ownerEmail string) (bool, error) {
	return PersonalSessionOwnedBy(ctx, m.pool, sessionID, strings.ToLower(strings.TrimSpace(ownerEmail)))
}

// PersonalSessionOwnedBy reports whether email owns sessionID as a personal
// session, one a single caller started over HTTP or MCP. It is false when the
// session doesn't exist, has no owner, belongs to a shared audience (a Slack
// thread, a scheduled report), or is owned by a different email, and for an
// empty email: every case is the same "not yours" answer, so continuing a
// session is not an ownership oracle. The runner replays a session's earlier
// tool results into the model's context, so continuing one without this
// check would show a caller content retrieved under another's scopes.
func PersonalSessionOwnedBy(ctx context.Context, pool *pgxpool.Pool, sessionID uuid.UUID, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	var owner *string
	err := pool.QueryRow(ctx,
		`SELECT owner_email FROM ask_sessions s
		 WHERE id = $1 AND (surface IS NULL OR surface IN ('http', 'unknown'))
		   AND NOT EXISTS (SELECT 1 FROM ask_slack_sessions sl WHERE sl.session_id = s.id)
		   AND NOT EXISTS (SELECT 1 FROM scheduled_report_runs sr WHERE sr.ask_session_id = s.id)`, sessionID,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("owner lookup: %w", err)
	}
	return owner != nil && strings.EqualFold(*owner, email), nil
}

// RecordSessionProvenance stores the runtime configuration used by the first
// turn in a session. Surface is always present on a valid TurnRequest and acts
// as the atomic write-once marker; this preserves intentional NULL model or
// embedded-prompt values across continuations after a deploy. Historical rows
// are migrated to a non-NULL known/unknown surface before this path ships. A
// zero release ID is retained only for tests that use fixed in-memory agents.
//
// A session continues only on the surface that started it. A chat thread's
// session is mapped in ask_slack_sessions, whose keys are Slack's or
// Discord's IDs, which never collide. One exception: a Slack turn may
// continue a PagerDuty investigation, whose alert thread the PagerDuty
// adapter attaches to the session so follow-ups work
// (docs/configuration.md § PagerDuty investigations). That is allowed only when the Slack
// turn's scopes contain every scope the investigation's first turn ran with,
// so the follow-up's audience could already read everything in the session's
// history. Sessions recorded before initial_scopes existed never qualify.
func (m *Manager) RecordSessionProvenance(ctx context.Context, sessionID uuid.UUID, surface AskRunSurface, modelID string, promptReleaseID int64, scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("record session provenance: allowed scopes are required")
	}
	var releaseArg any
	if promptReleaseID > 0 {
		releaseArg = promptReleaseID
	}
	var modelArg any
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		modelArg = modelID
	}
	tag, err := m.pool.Exec(ctx, `
		UPDATE ask_sessions s
		   SET surface = COALESCE(surface, $2),
		       initial_model_id = CASE WHEN surface IS NULL THEN $3 ELSE initial_model_id END,
		       prompt_release_id = CASE WHEN surface IS NULL THEN $4 ELSE prompt_release_id END,
		       initial_scopes = CASE WHEN surface IS NULL THEN $5::text[] ELSE initial_scopes END
		 WHERE id = $1
		   AND (surface IS NULL OR surface IN ($2, 'unknown')
		        OR ($2 = 'slack' AND surface = 'pagerduty'
		            AND initial_scopes IS NOT NULL AND initial_scopes <@ $5::text[]))
		   AND ($2 IN ('slack', 'discord') OR NOT EXISTS (SELECT 1 FROM ask_slack_sessions sl WHERE sl.session_id = s.id))
		   AND ($2 = 'scheduled' OR NOT EXISTS (SELECT 1 FROM scheduled_report_runs sr WHERE sr.ask_session_id = s.id))`, sessionID, string(surface), modelArg, releaseArg, scopes)
	if err != nil {
		return fmt.Errorf("record session provenance: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("session is not available to this audience surface")
	}
	return nil
}

// SessionPromptReleaseID returns the immutable release pinned by the first
// turn. A new session has no release until RecordSessionProvenance succeeds.
func (m *Manager) SessionPromptReleaseID(ctx context.Context, sessionID uuid.UUID) (int64, bool, error) {
	var id *int64
	if err := m.pool.QueryRow(ctx, `SELECT prompt_release_id FROM ask_sessions WHERE id = $1`, sessionID).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("load session prompt release: %w", err)
	}
	if id == nil {
		return 0, false, nil
	}
	return *id, true, nil
}

// PinOrGetSessionPromptRelease serializes first-pin contenders in Postgres and
// returns the winning release. This is separate from first-turn provenance so
// sessions created before prompt releases existed can adopt one release on
// their first post-migration turn. The row lock makes this safe across pods.
func (m *Manager) PinOrGetSessionPromptRelease(ctx context.Context, sessionID uuid.UUID, releaseID int64) (int64, error) {
	if releaseID <= 0 {
		return 0, errors.New("pin session prompt release: release ID must be positive")
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pin session prompt release: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pinned *int64
	if err := tx.QueryRow(ctx, `
		SELECT prompt_release_id FROM ask_sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&pinned); err != nil {
		return 0, fmt.Errorf("pin session prompt release: lock session: %w", err)
	}
	if pinned == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE ask_sessions SET prompt_release_id = $2 WHERE id = $1`, sessionID, releaseID); err != nil {
			return 0, fmt.Errorf("pin session prompt release: update: %w", err)
		}
		pinned = &releaseID
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pin session prompt release: commit: %w", err)
	}
	return *pinned, nil
}

// LoadOrCreateForSlackThread resolves a Slack thread to an ask
// session UUID, creating both the session and the mapping atomically
// if neither exists. Returns the session UUID only — history loading
// happens later inside WithSessionLock so the read-path sanitizer
// (which DELETEs orphan rows) can't race with an active turn writing
// to the same session.
//
// On create, stamps ask_sessions.owner_email with the (lowercased,
// trimmed) askerEmail of the @-mention that created the session — the
// original asker, not whoever's asking on any given follow-up turn.
// Empty/unresolved askerEmail stores SQL NULL. The fast path (existing
// mapping) never stamps, so only the creating call can set owner_email;
// this is what makes "original asker" the semantics rather than "most
// recent asker".
//
// Race-safe path:
//  1. Read-first SELECT — common case for follow-up messages in an
//     existing thread.
//  2. On miss, open a tx and INSERT into ask_sessions and
//     ask_slack_sessions. If the mapping insert hits the unique
//     constraint (a concurrent racer beat us), rollback so neither
//     row is committed (no orphan ask_sessions). Then re-SELECT to
//     pick up the racer's session.
func (m *Manager) LoadOrCreateForSlackThread(ctx context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error) {
	// Fast path: existing mapping.
	var id uuid.UUID
	err := m.pool.QueryRow(ctx,
		`SELECT session_id FROM ask_slack_sessions
		  WHERE channel_id = $1 AND thread_ts = $2`,
		channelID, threadTS,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("lookup slack session: %w", err)
	}

	// Slow path: create both rows in a tx, rollback on mapping conflict.
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if Commit succeeded

	var ownerArg any
	if e := strings.ToLower(strings.TrimSpace(askerEmail)); e != "" {
		ownerArg = e
	}

	var newID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO ask_sessions (owner_email) VALUES ($1) RETURNING id`,
		ownerArg,
	).Scan(&newID); err != nil {
		return uuid.Nil, fmt.Errorf("insert session: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (channel_id, thread_ts) DO NOTHING`,
		channelID, threadTS, newID,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert mapping: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// Concurrent racer beat us. Rollback discards our orphan
		// ask_sessions row, then we re-SELECT to pick up theirs.
		if err := tx.Rollback(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("rollback after race: %w", err)
		}
		err := m.pool.QueryRow(ctx,
			`SELECT session_id FROM ask_slack_sessions
			  WHERE channel_id = $1 AND thread_ts = $2`,
			channelID, threadTS,
		).Scan(&id)
		if err != nil {
			return uuid.Nil, fmt.Errorf("lookup after race: %w", err)
		}
		return id, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return newID, nil
}

// SlackSessionState reports whether ask_slack_sessions has a row for
// the supplied (channel, thread) and returns the context_through
// high-water mark (zero time when NULL or when no session exists).
//
// Read-only; no sanitize, no session lock — safe to call from the
// slack consumer's event loop to decide whether an incoming message
// should route to the ask runner and what thread messages have
// already been delivered into the session.
func (m *Manager) SlackSessionState(ctx context.Context, channelID, threadTS string) (known bool, contextThrough time.Time, err error) {
	// NULL context_through scans as zero time.Time (pgx zero-value for
	// timestamptz pointers not needed here — we use a direct scan and
	// treat zero as "never set").
	var ct *time.Time
	scanErr := m.pool.QueryRow(ctx,
		`SELECT context_through FROM ask_slack_sessions
		  WHERE channel_id = $1 AND thread_ts = $2`,
		channelID, threadTS,
	).Scan(&ct)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("slack session state: %w", scanErr)
	}
	if ct != nil {
		contextThrough = *ct
	}
	return true, contextThrough, nil
}

// SessionConversations returns the corpus conversations a session is itself
// part of: the Slack thread it was started in, for a session that has one.
// Every Slack entry point (a mention, a direct message, a follow-up in the
// thread) keys its session to a thread, and a session continued over the API
// keeps that key, so the session ID is enough to find it. A session with no
// Slack thread has no records of its own in the corpus and returns nothing:
// Diderot's own posts are never ingested, and an API or MCP conversation is
// not a corpus source.
func (m *Manager) SessionConversations(ctx context.Context, sessionID uuid.UUID) ([]retrieve.Conversation, error) {
	rows, err := m.pool.Query(ctx,
		`SELECT channel_id, thread_ts FROM ask_slack_sessions WHERE session_id = $1 ORDER BY channel_id, thread_ts`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("session conversations: %w", err)
	}
	defer rows.Close()
	var conversations []retrieve.Conversation
	for rows.Next() {
		var channelID, threadTS string
		if err := rows.Scan(&channelID, &threadTS); err != nil {
			return nil, fmt.Errorf("session conversations: %w", err)
		}
		// The Slack connector's thread key: see threadKey in its parser.
		conversations = append(conversations, retrieve.Conversation{Source: "slack", ThreadID: channelID + ":" + threadTS})
	}
	return conversations, rows.Err()
}

// AdvanceSlackContextThrough advances the context_through high-water
// mark on an ask_slack_sessions row to `through`, using GREATEST so
// the mark never moves backward. `through` should be the occurred_at
// of the @-mention that triggered the turn being committed.
//
// Must be called after the turn's messages are persisted. If the turn
// failed before persist, do NOT call this — the mark must stay at its
// previous value so missed messages get re-injected on the next attempt.
func (m *Manager) AdvanceSlackContextThrough(ctx context.Context, sessionID uuid.UUID, through time.Time) error {
	_, err := m.pool.Exec(ctx,
		`UPDATE ask_slack_sessions
		    SET context_through = GREATEST(COALESCE(context_through, '-infinity'::timestamptz), $2)
		  WHERE session_id = $1`,
		sessionID, through,
	)
	if err != nil {
		return fmt.Errorf("advance context_through: %w", err)
	}
	return nil
}

// InspectWindow reads a session and its validated AgenticMessage history,
// optionally within an inclusive turn range, without sanitization, tail
// repair, or any DELETE side effects: it observes exactly what is on disk.
// It reuses the same central row decoder as LoadAgenticSession, so
// schema-version, role, and token-drift validation are identical; a
// malformed persisted row fails loudly here too. It returns the total
// persisted message count independently of the selected window so the
// browser inspector can page very large transcripts without loading and
// decoding every message. Nil bounds read the complete history.
func (m *Manager) InspectWindow(ctx context.Context, sessionID uuid.UUID, fromTurn, toTurn *int) (*Session, []StoredMessage, int, error) {
	var s Session
	var total int
	err := m.pool.QueryRow(ctx,
		`SELECT id, created_at, last_turn_at, finished_at,
		        (SELECT count(*) FROM ask_messages WHERE session_id = $1)
		   FROM ask_sessions WHERE id = $1`,
		sessionID,
	).Scan(&s.ID, &s.CreatedAt, &s.LastTurnAt, &s.FinishedAt, &total)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, 0, ErrSessionNotFound
		}
		return nil, nil, 0, fmt.Errorf("session lookup: %w", err)
	}
	rows, err := m.pool.Query(ctx,
		`SELECT turn, role, message_schema_version, message, created_at,
		        input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		        COALESCE(incomplete_reason, '')
		   FROM ask_messages
		  WHERE session_id = $1
		    AND ($2::int IS NULL OR turn >= $2)
		    AND ($3::int IS NULL OR turn <= $3)
		  ORDER BY turn`,
		sessionID, fromTurn, toTurn,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("messages query: %w", err)
	}
	scans, err := scanAgenticMessageRows(rows)
	if err != nil {
		rows.Close()
		return nil, nil, 0, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("messages iter: %w", err)
	}
	msgs := make([]StoredMessage, 0, len(scans))
	for _, sc := range scans {
		msg, err := decodeAgenticMessageRow(messageRow{
			Role:                        sc.Role,
			AgenticMessageSchemaVersion: sc.SchemaVersion,
			JSON:                        sc.Message,
			InputTokens:                 sc.InputTokens,
			OutputTokens:                sc.OutputTokens,
			CacheReadTokens:             sc.CacheReadTokens,
			CacheWriteTokens:            sc.CacheWriteTokens,
		})
		if err != nil {
			return nil, nil, 0, fmt.Errorf("ask message turn=%d: %w", sc.Turn, err)
		}
		msgs = append(msgs, StoredMessage{
			Turn: sc.Turn, Message: msg, CreatedAt: sc.CreatedAt,
			IncompleteReason: sc.IncompleteReason,
		})
	}
	return &s, msgs, total, nil
}

// ErrSessionNotFound is returned when no session matches the lookup.
var ErrSessionNotFound = errors.New("ask: session not found")

// WithSessionLock runs fn while no other turn of sessionID runs, in this
// process or any other replica: it waits for an in-memory lock keyed on
// sessionID, then for the session's lease (withSessionLease). A waiter
// gives up when ctx ends. Lock entries are reference-counted and removed
// from the map on last release so the map doesn't grow unboundedly.
//
// Waiters in one process queue on the in-memory lock and are handed it the
// moment it frees, so only one turn per process at a time polls for the
// lease.
func (m *Manager) WithSessionLock(ctx context.Context, sessionID uuid.UUID, fn func(context.Context) error) error {
	lock := m.acquireLockEntry(sessionID)

	// Acquire — block until the channel slot is free or ctx fires.
	select {
	case lock.ch <- struct{}{}:
		// Got the lock. Continue.
	case <-ctx.Done():
		m.releaseLockEntry(sessionID, lock, false)
		return ctx.Err()
	}

	defer func() {
		// Release: drain the channel and decrement refcount.
		<-lock.ch
		m.releaseLockEntry(sessionID, lock, true)
	}()

	return m.withSessionLease(ctx, sessionID, fn)
}

// acquireLockEntry bumps the refcount for sessionID and returns the
// shared lock object. Creates one if it doesn't exist yet.
func (m *Manager) acquireLockEntry(sessionID uuid.UUID) *sessionLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.locks[sessionID]
	if !ok {
		lock = &sessionLock{ch: make(chan struct{}, 1)}
		m.locks[sessionID] = lock
	}
	lock.refCount++
	return lock
}

// releaseLockEntry decrements the refcount and deletes the map entry
// when nobody else is holding or waiting on it. Pass acquired=true
// only after a successful channel send (i.e. you actually held the
// lock); when ctx fired before send, pass false.
func (m *Manager) releaseLockEntry(sessionID uuid.UUID, lock *sessionLock, acquired bool) {
	_ = acquired // kept for future symmetry; same path either way
	m.mu.Lock()
	defer m.mu.Unlock()
	lock.refCount--
	if lock.refCount == 0 {
		delete(m.locks, sessionID)
	}
}
