package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

// SlackClientMembersAdapter wraps a *slack.Client to satisfy the
// slackAPIMembers interface. Production code passes
// (&SlackClientMembersAdapter{Client: api}) into NewChannelConfigStore.
type SlackClientMembersAdapter struct {
	Client *slack.Client
}

// GetUsersInConversationContext re-shapes the slack-go signature to
// match our local membersParams type. Keeps the slack-go
// dependency out of channel_config_test.go.
func (a *SlackClientMembersAdapter) GetUsersInConversationContext(ctx context.Context, p *membersParams) ([]string, string, error) {
	return a.Client.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
		ChannelID: p.ChannelID,
		Cursor:    p.Cursor,
		Limit:     p.Limit,
	})
}

// externalMembersTTL controls how long a cached has_external_members
// scan stays trusted before the next gate check forces a fresh
// conversations.members + per-user IsExternal sweep. 6h matches the
// rough cadence at which a vendor or guest gets added to a channel
// in our workspace; tighter would re-burn the API budget for little
// gain, looser would let a newly-added external user ask for a
// while before getting blocked.
const externalMembersTTL = 6 * time.Hour

// externalMembersConfirmAge is the much tighter staleness bound used
// when a cached POSITIVE (has_external_members=true) is about to
// block a mention. Membership-change events are at-most-once under
// socket mode, so the cache-bust on member_left can be missed — the
// confirm rescan is the authoritative backstop that makes "kick the
// guest, re-ask" work immediately instead of after the 6h TTL. 30s
// (rather than 0) keeps a burst of mentions in a genuinely-blocked
// channel from re-scanning on every single one.
const externalMembersConfirmAge = 30 * time.Second

// ChannelConfig is the read-shape of the slack_channel_config row
// for one channel. Returned by GetChannelConfig + EnsureExternalMembersFresh.
//
// All fields are about *operator intent* (allow_external_ask) or
// *cached state* (has_external_members + checked_at). The struct is
// built defensively: a row that doesn't exist yet returns the zero
// value, which means {AllowExternalAsk: false, HasExternalMembers:
// false (cached), CheckedAt: zero}. The TTL helper interprets a
// zero CheckedAt as "never scanned" and forces a refresh.
type ChannelConfig struct {
	ChannelID  string
	AskEnabled bool
	// AllowExternalAsk is the operator opt-in for channels that contain
	// (or are expected to contain) external members. Setting this flag
	// has two effects: (a) the channel is permitted to use ask at all
	// despite having external members, and (b) those external members
	// are themselves permitted to ask — they pass through the
	// external-asker gate instead of receiving silence. The retrieval
	// scope is audience-based (answers are visible to everyone in the
	// channel), so permitting the channel to use ask already accepts
	// that externals will read the answers; blocking them from typing
	// the mention adds no security, only friction.
	AllowExternalAsk bool
	// DocIngestEnabled is the document extraction opt-in.
	DocIngestEnabled bool
	// Non-nil while automatic history enqueue is pending.
	DocBackfillCursor  *int64
	HasExternalMembers bool
	// ExternalMembers lists the slack user ids the last scan found
	// to be external. Render-time callers (gate ephemeral, operator
	// DM) resolve them to names. Empty when the channel is clean OR
	// never scanned — ExternalMembersAt disambiguates.
	ExternalMembers   []string
	ExternalMembersAt time.Time // zero value = never scanned
	UpdatedBy         string
	UpdatedAt         time.Time
}

// IsExternalCheckStale reports whether the cached
// has_external_members result needs a fresh scan. True if never
// scanned (zero time) or if the last scan is older than the TTL.
func (c ChannelConfig) IsExternalCheckStale() bool {
	if c.ExternalMembersAt.IsZero() {
		return true
	}
	return time.Since(c.ExternalMembersAt) > externalMembersTTL
}

// ChannelConfigStore is the read/write surface for the
// slack_channel_config table. Stateless except for the *pgxpool
// handle + a single-flight group; safe for concurrent use.
//
// The single-flight group coalesces concurrent
// EnsureExternalMembersFresh calls for the same channel, so a
// burst of messages right after a TTL expiry doesn't fire
// duplicate conversations.members + per-user lookups. Critical
// because conversations.members is Tier 4 (~100 req/min); a
// stampede on a 1k-member channel could blow it without this.
type ChannelConfigStore struct {
	pool    *pgxpool.Pool
	dir     *Directory
	api     slackAPIMembers
	logger  *slog.Logger
	sf      singleflight
	limiter *Limiter // optional; gates conversations.members scan
}

// WithLimiter wires the budget governor for the conversations.members
// scan. Without it, the scan uses slack-go's per-request behavior
// (no proactive pacing). Optional; nil keeps prior behavior.
func (s *ChannelConfigStore) WithLimiter(l *Limiter) *ChannelConfigStore {
	s.limiter = l
	return s
}

// slackAPIMembers is the sub-surface needed for the
// conversations.members scan. Defined separately from slackAPI so
// tests can inject a minimal fake without re-stubbing the directory
// methods.
type slackAPIMembers interface {
	GetUsersInConversationContext(ctx context.Context, params *getUsersInConversationParams) ([]string, string, error)
}

// getUsersInConversationParams mirrors slack-go's
// GetUsersInConversationParameters. Local alias keeps slack-go out
// of the ChannelConfigStore tests.
type getUsersInConversationParams = membersParams

type membersParams struct {
	ChannelID string
	Cursor    string
	Limit     int
}

// NewChannelConfigStore wires the store. dir is needed for the
// per-member IsExternal lookup; api is the conversations.members
// surface (production: a thin adapter around *slack.Client that
// shaped-translates the slack-go params).
func NewChannelConfigStore(pool *pgxpool.Pool, dir *Directory, api slackAPIMembers, logger *slog.Logger) *ChannelConfigStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChannelConfigStore{pool: pool, dir: dir, api: api, logger: logger, sf: newSingleflight()}
}

// Get returns the current row for channelID, or a zero-valued
// ChannelConfig with the channel id set if no row exists yet. The
// caller's gate logic treats both the same way (default-deny).
func (s *ChannelConfigStore) Get(ctx context.Context, channelID string) (ChannelConfig, error) {
	cfg := ChannelConfig{ChannelID: channelID}
	var (
		hasExternal *bool
		checkedAt   *time.Time
		updatedBy   *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT ask_enabled, allow_external_ask, doc_ingest_enabled,
		       doc_backfill_cursor,
		       has_external_members,
		       external_members, external_members_checked_at,
		       updated_by, updated_at
		FROM slack_channel_config
		WHERE channel_id = $1
	`, channelID).Scan(
		&cfg.AskEnabled, &cfg.AllowExternalAsk, &cfg.DocIngestEnabled,
		&cfg.DocBackfillCursor,
		&hasExternal,
		&cfg.ExternalMembers, &checkedAt,
		&updatedBy, &cfg.UpdatedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return cfg, nil
	case err != nil:
		return cfg, fmt.Errorf("channel config lookup: %w", err)
	}
	if hasExternal != nil {
		cfg.HasExternalMembers = *hasExternal
	}
	if checkedAt != nil {
		cfg.ExternalMembersAt = *checkedAt
	}
	if updatedBy != nil {
		cfg.UpdatedBy = *updatedBy
	}
	return cfg, nil
}

// SetAllowExternalAsk writes the operator opt-in flag. Setting true
// both permits the channel to use ask despite external members AND
// permits those external members to ask (they pass the external-asker
// gate). updatedBy is stamped for audit (operator email, "cli",
// etc.). Upserts the row; other columns remain whatever they were.
func (s *ChannelConfigStore) SetAllowExternalAsk(ctx context.Context, channelID string, allow bool, updatedBy string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, allow_external_ask, updated_by, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			allow_external_ask = EXCLUDED.allow_external_ask,
			updated_by         = EXCLUDED.updated_by,
			updated_at         = now()
	`, channelID, allow, nullIfEmpty(updatedBy))
	if err != nil {
		return fmt.Errorf("set allow_external_ask: %w", err)
	}
	return nil
}

// SetDocIngestEnabled also starts or cancels automatic history enqueue.
func (s *ChannelConfigStore) SetDocIngestEnabled(
	ctx context.Context, channelID string, enabled bool, updatedBy string,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO slack_channel_config
			(channel_id, doc_ingest_enabled, doc_backfill_cursor, updated_by, updated_at)
		VALUES ($1, $2, CASE WHEN $2 THEN 0 ELSE NULL END, $3, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			doc_ingest_enabled  = EXCLUDED.doc_ingest_enabled,
			doc_backfill_cursor = EXCLUDED.doc_backfill_cursor,
			updated_by          = EXCLUDED.updated_by,
			updated_at          = now()
	`, channelID, enabled, nullIfEmpty(updatedBy))
	if err != nil {
		return fmt.Errorf("set doc_ingest_enabled: %w", err)
	}
	return nil
}

// DocIngestEnabled reports whether channelID is opted in to document
// ingest. Satisfies the extraction worker's GateChecker.
//
// Reads the row directly rather than going through Get so the worker's
// per-document re-check is a single narrow query. A missing row is
// false (default-deny), and — importantly — a query ERROR is returned
// rather than swallowed to false: the caller treats an error as
// transient and retries, so a DB blip can't silently look like an
// opt-out and settle a row that should have been extracted.
func (s *ChannelConfigStore) DocIngestEnabled(ctx context.Context, channelID string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `
		SELECT doc_ingest_enabled FROM slack_channel_config WHERE channel_id = $1
	`, channelID).Scan(&enabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("doc_ingest_enabled lookup: %w", err)
	}
	return enabled, nil
}

// SetAskEnabled writes the per-channel ask gate. Default for new
// channels is false (default-deny). Operator turns it on per
// channel on its admin page; the gate in maybeRouteAsk silently drops
// @-mentions when this is false.
func (s *ChannelConfigStore) SetAskEnabled(ctx context.Context, channelID string, enabled bool, updatedBy string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, ask_enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			ask_enabled = EXCLUDED.ask_enabled,
			updated_by  = EXCLUDED.updated_by,
			updated_at  = now()
	`, channelID, enabled, nullIfEmpty(updatedBy))
	if err != nil {
		return fmt.Errorf("set ask_enabled: %w", err)
	}
	return nil
}

// DefaultAskEnabled flips ask_enabled=true for a channel UNLESS the
// config has ever been set — the updated_by IS NULL guard tests
// "nobody (human or auto) ever configured this row". The automatic
// external-members scan path deliberately never sets updated_by;
// SetAskEnabled / SetAllowExternalAsk (operator + self-serve) and
// this function always do. So an operator's explicit
// ask_enabled=false is never overridden, and a re-fired discovery
// hook is a no-op (the prior auto-enable's own marker blocks it —
// harmless, the row is already enabled). The "auto (joined
// channel)" marker is deliberately written rather than NULL so the
// channels listing shows how ask got turned on.
//
// Called by the channel-discovery hook when diderot joins a new
// channel: the policy is answer-by-default on join.
// Returns whether the channel ended up enabled by this call.
func (s *ChannelConfigStore) DefaultAskEnabled(ctx context.Context, channelID, updatedBy string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, ask_enabled, updated_by, updated_at)
		VALUES ($1, true, $2, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			ask_enabled = true,
			updated_by  = EXCLUDED.updated_by,
			updated_at  = now()
		WHERE slack_channel_config.updated_by IS NULL
	`, channelID, nullIfEmpty(updatedBy))
	if err != nil {
		return false, fmt.Errorf("default ask_enabled: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EnsureExternalMembersFresh returns the current ChannelConfig with
// the has_external_members field guaranteed to be no older than the
// TTL. If the cached result is fresh, returns it as-is; otherwise
// scans conversations.members, updates the cache, and returns the
// updated row.
//
// Concurrent calls for the same channel are coalesced through a
// per-channel single-flight group, so a burst of messages right
// after a TTL expiry only fires one Slack API roundtrip — not N.
//
// On scan failure (Slack API error, ctx cancellation), returns the
// stale row + error — callers can decide whether to fail-closed
// (default-deny) or fail-open. The slack consumer's gate honors
// AllowExternalAsk first when it sees an error, so an opted-in
// channel keeps routing even if the scan is temporarily broken.
func (s *ChannelConfigStore) EnsureExternalMembersFresh(ctx context.Context, channelID string) (ChannelConfig, error) {
	return s.ensureFreshWithin(ctx, channelID, externalMembersTTL)
}

// ConfirmExternalMembers is the tight-bound variant the gate calls
// when a cached positive is about to BLOCK a mention: it re-scans
// unless the cached result is younger than
// externalMembersConfirmAge. This is what makes "remove the guest,
// re-ask" work immediately — membership events are at-most-once
// under socket mode, so the event-driven cache bust can be missed
// and the 6h TTL alone would keep blocking for hours after the
// channel went clean. The asymmetry is deliberate: a stale
// negative (external joined, cache says clean) is bounded by the
// TTL + the member-join cache bust; a stale positive costs a user
// their answer right now, so it gets confirmed before being acted
// on.
func (s *ChannelConfigStore) ConfirmExternalMembers(ctx context.Context, channelID string) (ChannelConfig, error) {
	return s.ensureFreshWithin(ctx, channelID, externalMembersConfirmAge)
}

func (s *ChannelConfigStore) ensureFreshWithin(ctx context.Context, channelID string, maxAge time.Duration) (ChannelConfig, error) {
	// The singleflight key is strictly channel-bound — NOT keyed by
	// maxAge — so an Ensure (6h bound) and a Confirm (30s bound)
	// racing on the same channel coalesce into ONE scan instead of
	// two concurrent conversations.members paginations. The price
	// of sharing a flight is that a tight-bound caller can coalesce
	// into a flight that satisfied only the looser bound (joined
	// late, got a cached-but-too-old result): detect that and loop.
	// The loop converges because a flight that actually scans
	// persists checked_at=now(), which satisfies any bound; bounded
	// retries guard the pathological case anyway.
	for attempt := 0; ; attempt++ {
		result, err, _ := s.sf.Do("ensure:"+channelID, func() (any, error) {
			return s.ensureFreshUncoalesced(ctx, channelID, maxAge)
		})
		if err != nil {
			// On error, the result may still hold a (stale) cfg —
			// try to surface it so the caller's gate logic can see
			// the AllowExternalAsk bit.
			if cfg, ok := result.(ChannelConfig); ok {
				return cfg, err
			}
			return ChannelConfig{ChannelID: channelID}, err
		}
		cfg := result.(ChannelConfig)
		if attempt < 3 && (cfg.ExternalMembersAt.IsZero() || time.Since(cfg.ExternalMembersAt) > maxAge) {
			continue // coalesced result too stale for this caller's bound
		}
		return cfg, nil
	}
}

func (s *ChannelConfigStore) ensureFreshUncoalesced(ctx context.Context, channelID string, maxAge time.Duration) (ChannelConfig, error) {
	cfg, err := s.Get(ctx, channelID)
	if err != nil {
		return cfg, err
	}
	if !cfg.ExternalMembersAt.IsZero() && time.Since(cfg.ExternalMembersAt) <= maxAge {
		return cfg, nil
	}
	externals, scanErr := s.scanExternalMembers(ctx, channelID)
	if scanErr != nil {
		// Return stale-or-empty cfg + error. Caller decides.
		return cfg, scanErr
	}
	if externals == nil {
		// pgx encodes a nil slice as SQL NULL; the column is NOT
		// NULL with an array default, so a clean scan persists [].
		externals = []string{}
	}
	// Persist the fresh result. Don't overwrite allow_external_ask
	// or updated_by/updated_at — those are operator intent (the
	// scan path is automatic background activity, NOT an operator
	// change). external_members_checked_at is the scan-side
	// timestamp; updated_at stays whatever the operator last set.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members, external_members_checked_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			has_external_members         = EXCLUDED.has_external_members,
			external_members             = EXCLUDED.external_members,
			external_members_checked_at  = EXCLUDED.external_members_checked_at
	`, channelID, len(externals) > 0, externals); err != nil {
		return cfg, fmt.Errorf("persist external scan: %w", err)
	}
	// Re-read instead of patching the pre-scan snapshot: the scan
	// can take seconds on large channels, and ask_enabled /
	// allow_external_ask may have been written concurrently in that
	// window (the self-serve opt-in click is exactly such a writer).
	// Returning the stale snapshot would make the gate block a
	// mention the operator/user just allowed.
	return s.Get(ctx, channelID)
}

// ExpireExternalCheck zeroes the scan timestamp for a channel so
// the next gate check re-scans regardless of TTL. Called from the
// live member_joined / member_left handlers: a membership change is
// a strong signal the cached scan is wrong in one direction or the
// other. Best-effort — socket-mode delivery is at-most-once, so
// this is an accelerator, not the correctness mechanism (that's
// the TTL + the confirm rescan).
func (s *ChannelConfigStore) ExpireExternalCheck(ctx context.Context, channelID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE slack_channel_config
		   SET external_members_checked_at = NULL
		 WHERE channel_id = $1
	`, channelID)
	if err != nil {
		return fmt.Errorf("expire external check: %w", err)
	}
	return nil
}

// scanExternalMembers paginates conversations.members for the
// channel and returns the slack user ids of every external member.
// No short-circuit: the full list is what lets the gate ephemeral
// NAME the externals (so the asker knows exactly who to remove)
// instead of just asserting some exist. The marginal cost over the
// old first-hit short-circuit is per-user directory lookups, which
// are cache-warm for any channel that's been scanned before.
//
// Per-user lookup failures FAIL THE SCAN: if we can't tell whether
// a user is external, returning "no external members" would
// silently let an external user past the gate for the full TTL.
// Better to surface the error and let the caller's gate logic
// fail-closed. The caller's allow_external_ask short-circuit
// (see maybeRouteAsk) keeps the flow working for opted-in
// channels even if a transient users.info hiccup blocks the scan.
func (s *ChannelConfigStore) scanExternalMembers(ctx context.Context, channelID string) ([]string, error) {
	cursor := ""
	// 1000 is Slack's max per page; minimizes round-trips on huge
	// channels. The endpoint is Tier 4 (~100 req/min sustained), so
	// a single channel scan is cheap regardless of size.
	const pageLimit = 1000
	var externals []string
	for {
		var (
			members []string
			next    string
		)
		err := WithSlackRetry(ctx, s.limiter, methodConvMembers, func() error {
			var apiErr error
			members, next, apiErr = s.api.GetUsersInConversationContext(ctx, &membersParams{
				ChannelID: channelID,
				Cursor:    cursor,
				Limit:     pageLimit,
			})
			return apiErr
		})
		if err != nil {
			return nil, fmt.Errorf("conversations.members: %w", err)
		}
		for _, uid := range members {
			ui, err := s.dir.LookupUserInfo(ctx, uid)
			if err != nil {
				// Distinguish definitive not-found (user disabled,
				// deactivated, never existed — Slack has surfaced
				// a stable answer) from transient errors. A
				// definitively-gone user is, by definition, not
				// an external member of the channel — skip and
				// continue so a deactivated guest doesn't taint
				// every future scan. Transient errors fail the
				// scan so the gate can fail-closed (or rely on
				// AllowExternalAsk).
				if isDefinitiveNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("user lookup during scan (uid=%s): %w", uid, err)
			}
			if ui.IsExternal() {
				externals = append(externals, uid)
			}
		}
		if next == "" {
			return externals, nil
		}
		cursor = next
	}
}
