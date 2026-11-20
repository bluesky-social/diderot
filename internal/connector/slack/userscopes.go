package slack

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
)

// userScopeTTL is the in-process cache lifetime for one email's
// resolved scope set. 5 minutes is the revocation window: after an
// operator is removed from a private channel in Slack, they keep
// seeing that channel's content over the API and MCP for at most this long.
// Deliberately short (vs. the ask-gate's 6h external-members TTL)
// because THIS is the revocation path: a user removed from a private
// channel loses its scope within one TTL.
const userScopeTTL = 5 * time.Minute

// userConvAPI is the slack-go subset the resolver needs: enumerate
// the channels one user is a member of. Mirrors sync_worker.go's
// reconcileAPI shape so the production *slack.Client satisfies it and
// tests can inject a fake. users.conversations returns only channels
// the BOT can also see, so a private channel the bot isn't in never
// appears — which is exactly the "membership diderot can vouch for"
// set (the bot indexed nothing it can't see).
type userConvAPI interface {
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
}

// UserScopeResolver maps an OIDC email to the visibility-scope set the
// caller's Slack identity is entitled to on the HTTP and MCP surfaces:
// always {public, internal}, plus slack-channel:<id> for every private
// channel the user shares with the bot: the DM scope resolver applied
// to the API instead of DMs.
//
// Fail-closed by construction: any lookup failure (no email match,
// multiple matches, Slack API error mid-pagination) degrades to the
// baseline {public, internal}. It NEVER widens to unrestricted and
// NEVER serves a stale private grant past its TTL — an expired entry
// forces a refresh, and a failed refresh returns baseline rather than
// re-serving the old set, so a revoked member loses private access
// within one TTL even during a Slack outage.
//
// Safe for concurrent use: ScopesForEmail is called once per inbound
// HTTP request. The cache + warn-dedup maps are mutex-guarded; the
// Slack pagination runs outside the lock so a slow refresh for one
// email can't block cache reads for another.
type UserScopeResolver struct {
	api          userConvAPI
	limiter      *Limiter
	pool         *pgxpool.Pool
	logger       *slog.Logger
	publicScopes *PublicScopeResolver

	ttl time.Duration
	// now is time.Now in production; tests override it to drive the
	// cache clock deterministically.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]scopeCacheEntry
	// warnedAt rate-limits the "no unique slack_users row" warning to
	// once per TTL per email so a caller whose OIDC email doesn't
	// match a Slack profile doesn't spam the log on every request.
	warnedAt map[string]time.Time
}

type scopeCacheEntry struct {
	scopes    []string
	expiresAt time.Time
}

// NewUserScopeResolver constructs a resolver. api + pool are required
// in production; limiter may be nil (WithSlackRetry no-ops the pacing
// then, useful in tests). logger nil falls back to slog.Default.
func NewUserScopeResolver(api userConvAPI, limiter *Limiter, pool *pgxpool.Pool, logger *slog.Logger) *UserScopeResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &UserScopeResolver{
		api:      api,
		limiter:  limiter,
		pool:     pool,
		logger:   logger,
		ttl:      userScopeTTL,
		now:      time.Now,
		cache:    make(map[string]scopeCacheEntry),
		warnedAt: make(map[string]time.Time),
	}
}

// baselineScopes is the always-granted set. Returned as a fresh slice
// on every call so a caller can't mutate a shared backing array.
func baselineScopes() []string {
	return []string{events.ScopePublic, events.ScopeInternal}
}

// ScopesForEmail returns the visibility set for the given OIDC email.
// Always contains {public, internal}; adds cached workspace-public scopes
// and slack-channel:<id> for each private channel shared with the bot.
// Failed resolution never widens to unrestricted access.
func (r *UserScopeResolver) ScopesForEmail(ctx context.Context, email string) []string {
	scopes := baselineScopes()
	if r.api != nil {
		scopes = r.privateScopesForEmail(ctx, email)
	}
	return append(slices.Clone(scopes), r.publicScopes.Scopes(ctx)...)
}

func (r *UserScopeResolver) WithPublicScopes(resolver *PublicScopeResolver) *UserScopeResolver {
	r.publicScopes = resolver
	return r
}

func (r *UserScopeResolver) privateScopesForEmail(ctx context.Context, email string) []string {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return baselineScopes()
	}

	// Cache hit within TTL: serve it. Expired entries are ignored —
	// no stale-serving, since this is the revocation path.
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && r.now().Before(e.expiresAt) {
		scopes := e.scopes
		r.mu.Unlock()
		return scopes
	}
	r.mu.Unlock()

	scopes, ok := r.resolve(ctx, key)
	if !ok {
		// Refresh failed (Slack error / partial pagination). Return
		// baseline and DO NOT cache — caching a partial set would pin
		// an under-grant, and caching baseline here would suppress the
		// retry that recovers the private grants on the next request.
		return baselineScopes()
	}

	r.mu.Lock()
	r.cache[key] = scopeCacheEntry{scopes: scopes, expiresAt: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return scopes
}

// UserIDForEmail resolves an email to exactly one non-bot Slack user. It is
// exposed for delivery systems that must prove an audience identity before
// applying that email's private-channel grants (scheduled DM reports). A false
// result is fail-closed: callers must not deliver owner-scoped content to an
// unverified target.
func (r *UserScopeResolver) UserIDForEmail(ctx context.Context, email string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return "", false
	}
	return r.lookupUserID(ctx, key)
}

// AskerForEmail enriches a verified HTTP identity with the same cached Slack
// profile used by direct Slack asks. Email is the only cross-surface join key;
// zero matches degrade to the caller's own claims, while ambiguity is
// reported rather than guessing between identities.
func (r *UserScopeResolver) AskerForEmail(ctx context.Context, email string) (*ask.AskerInfo, error) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return nil, nil
	}
	matches, err := r.lookupUserProfiles(ctx, key)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return askerInfoFromUserInfo(matches[0]), nil
	default:
		return nil, fmt.Errorf("email matched %d Slack profiles", len(matches))
	}
}

// lookupUserProfiles owns the email-to-Slack join for both HTTP asker
// enrichment and audience-scope resolution. Keeping uniqueness and field
// selection here prevents those two decisions from drifting apart.
func (r *UserScopeResolver) lookupUserProfiles(ctx context.Context, key string) ([]UserInfo, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT user_id, coalesce(name, ''), coalesce(display_name, ''),
		       coalesce(real_name, ''), coalesce(email, ''),
		       coalesce(tz, ''), coalesce(tz_label, '')
		  FROM slack_users
		 WHERE lower(email) = $1 AND NOT is_bot`, key)
	if err != nil {
		return nil, fmt.Errorf("query slack profile: %w", err)
	}
	defer rows.Close()

	var matches []UserInfo
	for rows.Next() {
		var ui UserInfo
		if err := rows.Scan(&ui.UserID, &ui.Name, &ui.DisplayName,
			&ui.RealName, &ui.Email, &ui.TZ, &ui.TZLabel); err != nil {
			return nil, fmt.Errorf("scan slack profile: %w", err)
		}
		matches = append(matches, ui)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate slack profiles: %w", err)
	}
	return matches, nil
}

// resolve does the DB + Slack work for one email. The bool is false
// only when the Slack pagination failed (caller must return baseline
// WITHOUT caching); a missing/ambiguous email is a *successful*
// resolution to baseline (cacheable — the user genuinely has no
// private grants we can vouch for).
func (r *UserScopeResolver) resolve(ctx context.Context, key string) ([]string, bool) {
	userID, ok := r.lookupUserID(ctx, key)
	if !ok {
		return baselineScopes(), true
	}

	scopes := baselineScopes()
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, false
		}
		var (
			channels []slack.Channel
			next     string
		)
		err := WithSlackRetry(ctx, r.limiter, methodConvUserList, func() error {
			var apiErr error
			channels, next, apiErr = r.api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
				UserID:          userID,
				Types:           []string{"private_channel"},
				ExcludeArchived: true,
				Limit:           200,
				Cursor:          cursor,
			})
			return apiErr
		})
		if err != nil {
			// Partial walk: discard everything gathered so far. An
			// incomplete set under-grants (safe) but caching it would
			// pin that under-grant for a full TTL, so we drop it and
			// return baseline uncached — the next request retries.
			r.logger.Warn("userscopes: users.conversations failed; degrading to baseline",
				"user_id", userID, "err", err)
			return nil, false
		}
		for _, c := range channels {
			scopes = append(scopes, events.ScopeChannel(c.ID))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return scopes, true
}

// lookupUserID resolves an email to exactly one non-bot Slack user id.
// Zero or multiple matches return ok=false (caller degrades to
// baseline), with a TTL-rate-limited Warn so a persistent mismatch
// (operator's sign-in email != their Slack profile email) is
// visible without flooding the log on every request.
func (r *UserScopeResolver) lookupUserID(ctx context.Context, key string) (string, bool) {
	// lower(email): the ingest paths (directory.go, warm.go) store the
	// Slack profile email verbatim (mixed case possible), while key is
	// already lowercased. A byte-exact compare would silently drop
	// private scopes for any uppercase-bearing address — fail-closed but
	// wrong. No index on slack_users.email today (the table holds one
	// row per workspace user, so a seq scan is trivial), so the function
	// call costs nothing measurable.
	matches, err := r.lookupUserProfiles(ctx, key)
	if err != nil {
		r.logger.Warn("userscopes: slack_users lookup failed; degrading to baseline",
			"email", key, "err", err)
		return "", false
	}
	if len(matches) != 1 {
		r.warnUserMatch(key, len(matches))
		return "", false
	}
	return matches[0].UserID, true
}

// warnUserMatch logs a no-unique-match warning at most once per TTL
// per email. Zero matches = email never mapped to Slack; >1 = a data
// bug (email is not unique in slack_users). Either way the caller
// degrades to baseline; the log just makes the degrade diagnosable.
func (r *UserScopeResolver) warnUserMatch(key string, matches int) {
	r.mu.Lock()
	last, seen := r.warnedAt[key]
	now := r.now()
	if seen && now.Sub(last) < r.ttl {
		r.mu.Unlock()
		return
	}
	r.warnedAt[key] = now
	r.mu.Unlock()
	r.logger.Warn("userscopes: no unique slack_users row for email; caller sees public+internal only",
		"email", key, "matches", matches)
}
