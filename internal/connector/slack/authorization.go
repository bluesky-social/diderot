package slack

import (
	"context"
	"log/slog"

	"github.com/bluesky-social/diderot/internal/authorization"
)

type ChannelAuthorizer struct {
	Policy       *authorization.Authorizer
	Directory    *Directory
	Logger       *slog.Logger
	PublicScopes *PublicScopeResolver
}

func (resolver *ChannelAuthorizer) ResolveChannel(ctx context.Context, channelID string) authorization.ChannelAccess {
	if resolver == nil || resolver.Policy == nil {
		return authorization.ChannelAccess{}
	}
	access := resolver.Policy.RestrictChannelScopes(channelID, nil)
	if access.CorpusOnly {
		if resolver.Directory == nil {
			return access
		}
		eligible, err := resolver.Directory.eligibleProtectedChannel(ctx, channelID, resolver.Policy.AllowsPublicChannel(channelID))
		if err != nil || !eligible {
			return access
		}
	}
	scopes := scopesForChannel(channelID)
	scopes = append(scopes, resolver.PublicScopes.Scopes(ctx)...)
	decision := resolver.AuthorizeChannel(ctx, channelID)
	scopes = append(scopes, decision.CorpusScopes()...)
	return resolver.Policy.RestrictChannelScopes(channelID, scopes)
}

func (resolver *ChannelAuthorizer) AuthorizeChannel(ctx context.Context, channelID string) authorization.Decision {
	if resolver == nil || resolver.Directory == nil {
		return authorization.Decision{}
	}
	decision, err := resolver.Policy.Resolve([]authorization.Principal{authorization.ChannelPrincipal(channelID)})
	if err == nil && len(decision.Permissions()) > 0 {
		var eligible bool
		eligible, err = resolver.Directory.eligibleProtectedChannel(ctx, channelID, resolver.Policy.AllowsPublicChannel(channelID))
		if err == nil && eligible {
			return decision
		}
	}
	if err != nil && resolver.Logger != nil {
		resolver.Logger.WarnContext(ctx, "protected channel authorization unavailable", "channel", channelID, "err", err)
	}
	return authorization.Decision{}
}

func (directory *Directory) eligibleProtectedChannel(ctx context.Context, channelID string, allowPublic bool) (bool, error) {
	if _, err := directory.LookupChannel(ctx, channelID); err != nil {
		return false, err
	}
	var eligible bool
	err := directory.pool.QueryRow(ctx, `
		SELECT COALESCE(name <> '' AND (is_private OR $2) AND NOT is_ext_shared AND NOT is_pending_ext_shared, false)
		  FROM slack_channel_info WHERE channel_id=$1`, channelID, allowPublic).Scan(&eligible)
	return eligible, err
}
