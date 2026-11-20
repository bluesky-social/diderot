package slack

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bluesky-social/diderot/internal/events"
)

func isDirectMessage(channelID string) bool {
	return strings.HasPrefix(channelID, "D")
}

// isVerifiedInternalHuman is the authorization boundary for one-to-one DMs.
// Keep routing and ingestion on the same predicate: a message that cannot
// start an Ask turn must not be persisted under a retrievable owner scope.
func isVerifiedInternalHuman(ui UserInfo) bool {
	return !ui.IsNegativeCache &&
		!ui.IsBot &&
		!ui.IsExternal() &&
		strings.TrimSpace(ui.Email) != ""
}

// scopesForChannel is the Level-0 resolver: the base visibility set before
// corpus grants and channel ceilings. Audience-based — the answer is read
// by the channel, so the set depends only on which channel diderot
// was mentioned in, never on who asked (multi-turn replay makes
// asker-scoping unsound in shared contexts).
//
//   - public:    anonymously internet-readable content
//   - internal: company-wide non-Slack knowledge
//   - slack-channel:<id>: this channel's own scope. Public-channel
//     scopes from the shared cache are added by ChannelAuthorizer.
//     Slack itself enforces
//     revocation: removed members can't invoke diderot here.
//
// Note ScopeUnstamped is never granted: events that missed their
// stamp stay invisible everywhere until the scope sweep heals them.
func scopesForChannel(channelID string) []string {
	return []string{
		events.ScopePublic,
		events.ScopeInternal,
		events.ScopeChannel(channelID),
	}
}

// scopesForDM computes the single-reader audience boundary for a direct
// message. The existing resolver contributes public, internal, and currently
// shared private-channel scopes. The verified Slack profile email adds the
// caller's owner scope so they can ask about their own coding sessions.
//
// Copy the resolver result before appending: UserScopeResolver caches slices,
// and mutating one would leak an owner grant into later calls using that cache.
func (c *Consumer) scopesForDM(ctx context.Context, email string) []string {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	scopes := baselineScopes()
	if c.dmScopes != nil && normalizedEmail != "" {
		if resolved := c.dmScopes.ScopesForEmail(ctx, normalizedEmail); len(resolved) > 0 {
			scopes = append([]string(nil), resolved...)
		}
	}
	if normalizedEmail != "" {
		scopes = append(scopes, events.ScopeOwner(normalizedEmail))
	}
	return scopes
}

// stampScope sets ev.Scope from the conversation audience. One-to-one DMs
// scope to the verified author's canonical email, and known public and
// private channels to "slack-channel:<id>".
//
// Privacy-unknown (lookup error, negative-cache channel row) stamps
// events.ScopeUnstamped: the event still ingests but no resolver
// grants the sentinel, so it stays invisible until the hourly scope
// reconcile re-stamps it from a fresher slack_channel_info row.
// Deny-by-default — a private message must never degrade to internal
// because conversations.info was flaky at ingest time.
//
// Shared by all three slack ingest paths (live consumer, backfill,
// rebuilder). ParseMessage stays scope-agnostic on purpose: privacy
// is channel state, not message content.
func stampScope(ctx context.Context, dir *Directory, logger *slog.Logger, channelID string, ev *events.Event) {
	if dir == nil || channelID == "" {
		ev.Scope = events.ScopeUnstamped
		return
	}
	if isDirectMessage(channelID) {
		// Slack's is_private field is not the access-control signal for IMs;
		// treating false as public would leak DM bodies into every Ask surface.
		// Re-resolve through the authorization-grade profile cache so the owner
		// scope never rests on identity data older than five minutes.
		ui, err := dir.LookupUserInfoStrict(ctx, ev.ActorID)
		email := ""
		if err == nil {
			email = strings.ToLower(strings.TrimSpace(ui.Email))
		}
		if err != nil || !isVerifiedInternalHuman(ui) {
			if logger != nil {
				logger.Warn("slack: DM owner is not a verified internal human; stamping unscoped",
					"channel_id", channelID, "actor_id", ev.ActorID, "err", err)
			}
			ev.Scope = events.ScopeUnstamped
			return
		}
		ev.ActorCanonical = email
		ev.Scope = events.ScopeOwner(email)
		return
	}
	_, err := dir.ChannelIsPrivate(ctx, channelID)
	if err != nil {
		if logger != nil {
			logger.Warn("slack: channel privacy unknown; stamping unscoped (invisible until reconciled)",
				"channel_id", channelID, "err", err)
		}
		ev.Scope = events.ScopeUnstamped
		return
	}
	ev.Scope = events.ScopeChannel(channelID)
}
