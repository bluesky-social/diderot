package main

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

// TestSourceScopeGrantsReachTheRegisteredDiscordSource: a reader's scopes
// gain the scope of each chosen Discord channel whose visibility names one
// of them, through the registry, once each.
func TestSourceScopeGrantsReachTheRegisteredDiscordSource(t *testing.T) {
	pool := ingestTestPool(t, "cmd_scope_grants")
	_, err := pool.Exec(t.Context(), `DELETE FROM source_state WHERE source = $1`, discordcx.ConnectorName)
	require.NoError(t, err)
	state := source.NewState(pool, discordcx.ConnectorName)
	require.NoError(t, state.Set(t.Context(), "channels", map[string]discordcx.Selection{
		"1": {GuildID: "9", Scope: events.ScopeInternal},
		"2": {GuildID: "9", Scope: "corpus:mods"},
		"3": {GuildID: "9", Scope: events.ScopeInternal, Removing: true},
	}))
	grants := sourceScopeGrants{
		sources: sourceRegistry(pool, nil, nil, nil, nil, nil, false),
		deps:    source.Deps{Pool: pool}, logger: slog.New(slog.DiscardHandler),
	}
	assert.Equal(t, []string{events.ScopePublic, events.ScopeInternal, discordcx.ChannelScope("1")},
		grants.GrantScopes(t.Context(), []string{events.ScopePublic, events.ScopeInternal}))
	assert.Equal(t, []string{"corpus:mods", discordcx.ChannelScope("2")}, grants.GrantScopes(t.Context(), []string{"corpus:mods"}))
	assert.Equal(t, []string{events.ScopePublic}, grants.GrantScopes(t.Context(), []string{events.ScopePublic}))
	granted := grants.GrantScopes(t.Context(), []string{events.ScopeInternal, discordcx.ChannelScope("1")})
	assert.Equal(t, []string{events.ScopeInternal, discordcx.ChannelScope("1")}, granted, "a scope is held once")
}
