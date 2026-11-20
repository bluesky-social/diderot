package adminui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestDiscordChannelsSection: the Discord page lists every channel the bot
// can see by server, says which are indexed and who may read them, and
// chooses, re-reads and removes channels through audited actions. A channel
// private in Discord is refused until its visibility is confirmed.
func TestDiscordChannelsSection(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_discord"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, source_state RESTART IDENTITY`)
	require.NoError(t, err)
	state := source.NewState(pool, discordcx.ConnectorName)
	now := time.Now().UTC()
	require.NoError(t, state.Set(ctx, "directory", discordcx.Directory{ReadAt: now, Guilds: []discordcx.GuildEntry{{
		ID: "100000000000000001", Name: "Example Server",
		Channels: []discordcx.ChannelEntry{
			{ID: "200000000000000001", Name: "general", Type: 0, Category: "Projects", Readable: true},
			{ID: "200000000000000002", Name: "help", Type: 15, Readable: true},
			{ID: "200000000000000003", Name: "staff", Type: 0, Private: true, Readable: true},
			{ID: "200000000000000004", Name: "locked", Type: 0, Private: true},
		},
	}}}))
	require.NoError(t, state.Set(ctx, "progress", map[string]discordcx.Progress{
		"200000000000000001": {Scope: "internal", Messages: 1234, FullReadAt: &now, RecentReadAt: &now},
	}))
	channels := discordcx.Channels{State: state}
	_, err = channels.Add(ctx, "200000000000000001", discordcx.VisibilityEveryone, "", false, "operator@example.com")
	require.NoError(t, err)

	b := newAdminBrowser(t, Config{
		Pool: pool, Connectors: &everyConnector{},
		Registries: setup.Registries{Sources: source.NewRegistry(discordcx.Connector{})}, SourceDeps: source.Deps{Pool: pool},
	})
	page := b.get("/admin/connectors/discord")
	assert.Contains(t, page, "Example Server")
	assert.Contains(t, page, "#general")
	assert.Contains(t, page, "1.2K")
	assert.Contains(t, page, "Everyone who can use Diderot")
	assert.Contains(t, page, "private in Discord")
	assert.Contains(t, page, "bot can&#39;t read")
	assert.Equal(t, 1, strings.Count(page, "Read its history again"), "only the chosen channel can be read again")
	assert.Equal(t, 2, strings.Count(page, "<summary>Index</summary>"), "the readable channels not chosen can be indexed")
	assert.Contains(t, page, `name="confirm_private"`)

	message, refused := b.post("/admin/connectors/discord/channels", url.Values{"channel": {"200000000000000003"}, "visibility": {"everyone"}})
	assert.True(t, refused)
	assert.Contains(t, message, "private in Discord")
	message, refused = b.post("/admin/connectors/discord/channels", url.Values{
		"channel": {"200000000000000003"}, "visibility": {"policy"}, "policy": {"Not A Name"}, "confirm_private": {"yes"},
	})
	assert.True(t, refused)
	assert.Contains(t, message, "lower-case letters")
	message, refused = b.post("/admin/connectors/discord/channels", url.Values{
		"channel": {"200000000000000003"}, "visibility": {"policy"}, "policy": {"staff"}, "confirm_private": {"yes"},
	})
	assert.False(t, refused, message)
	message, refused = b.post("/admin/connectors/discord/channels/reread", url.Values{"channel": {"200000000000000001"}})
	assert.False(t, refused, message)
	_, refused = b.post("/admin/connectors/discord/channels/remove", url.Values{"channel": {"200000000000000001"}})
	assert.True(t, refused, "removing needs the box ticked")
	message, refused = b.post("/admin/connectors/discord/channels/remove", url.Values{"channel": {"200000000000000001"}, "confirm": {"remove"}})
	assert.False(t, refused, message)

	o, err := channels.Overview(ctx)
	require.NoError(t, err)
	assert.Equal(t, "corpus:staff", o.Selections["200000000000000003"].Scope)
	assert.True(t, o.Selections["200000000000000001"].Removing)
	assert.NotNil(t, o.Selections["200000000000000001"].RereadAt)
	assert.Equal(t, []string{
		"sources.discord.channel.choose 200000000000000003 failed",
		"sources.discord.channel.choose 200000000000000003 failed",
		"sources.discord.channel.choose 200000000000000003 succeeded",
		"sources.discord.channel.reread 200000000000000001 succeeded",
		"sources.discord.channel.remove 200000000000000001 succeeded",
	}, audited(t, pool))

	page = b.get("/admin/connectors/discord")
	assert.Contains(t, page, "Groups granted corpus:staff")
	assert.Contains(t, page, "removing")

	// A server is untrusted until an administrator trusts it, and the page
	// says what trusting means.
	assert.Contains(t, page, "Trust Example Server")
	assert.Contains(t, page, "anyone who is a member of the server could ask about it")
	message, refused = b.post("/admin/connectors/discord/ask/trusted", url.Values{"guild": {"100000000000000001"}, "trusted": {"yes"}})
	assert.False(t, refused, message)
	_, refused = b.post("/admin/connectors/discord/ask/trusted", url.Values{"guild": {"100000000000000009"}, "trusted": {"yes"}})
	assert.True(t, refused, "a server the bot is not in")
	settings, err := channels.AskSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"100000000000000001": true}, settings.Trusted)
	page = b.get("/admin/connectors/discord")
	assert.Contains(t, page, "Stop trusting Example Server")
	message, refused = b.post("/admin/connectors/discord/ask/trusted", url.Values{"guild": {"100000000000000001"}, "trusted": {"no"}})
	assert.False(t, refused, message)
	settings, err = channels.AskSettings(ctx)
	require.NoError(t, err)
	assert.Empty(t, settings.Trusted)
	assert.Equal(t, []string{
		"sources.discord.ask.trusted 100000000000000001 succeeded",
		"sources.discord.ask.trusted 100000000000000009 failed",
		"sources.discord.ask.trusted 100000000000000001 succeeded",
	}, audited(t, pool)[5:])

	// A trusted server the bot has left can still be untrusted, so trust
	// can't come back if it rejoins.
	require.NoError(t, state.Update(ctx, "ask", &settings, func(bool) error {
		settings.Trusted = map[string]bool{"100000000000000009": true}
		return nil
	}))
	page = b.get("/admin/connectors/discord")
	assert.Contains(t, page, "Servers the bot has left")
	assert.Contains(t, page, "Stop trusting it")
	message, refused = b.post("/admin/connectors/discord/ask/trusted", url.Values{"guild": {"100000000000000009"}, "trusted": {"no"}})
	assert.False(t, refused, message)
	settings, err = channels.AskSettings(ctx)
	require.NoError(t, err)
	assert.Empty(t, settings.Trusted)
}
