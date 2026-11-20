package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminjob"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// botMembership is the Slack membership calls the pages make.
type botMembership struct {
	invited, kicked []string
}

func (*botMembership) ResolveChannel(_ context.Context, nameOrID string) (string, string, error) {
	return nameOrID, "", nil
}

func (m *botMembership) Invite(_ context.Context, channelID string) error {
	m.invited = append(m.invited, channelID)
	return nil
}

func (*botMembership) IsBotInChannel(context.Context, string) (bool, error) { return true, nil }

func (m *botMembership) Kick(_ context.Context, channelID string) error {
	m.kicked = append(m.kicked, channelID)
	return nil
}

// TestPagesActOnAReplicaWithoutLeaderWork: the Slack channel page and the
// re-embed control act through an admin API server that runs no Slack source
// and no leader term, which is every replica but the leader. Every control is
// enabled and works; with Slack off the page says so.
func TestPagesActOnAReplicaWithoutLeaderWork(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_replica"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		TRUNCATE slack_ingest_blacklist, slack_channel_config, slack_channel_sync,
		         slack_channel_info, admin_audit_log, admin_jobs
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
		VALUES ('CPLATFORM', 'platform', false, false, now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_sync (channel_id, last_synced_at, cursor, ingest_enabled)
		VALUES ('CPLATFORM', now(), 'mid-walk', true)`)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	membership := &botMembership{}
	slackOn, userToken := true, true
	api := adminapi.NewServer(nil, logger)
	api.SetStatusPool(pool)
	api.SetSlackControls(func() (adminapi.SlackControls, bool) {
		if !slackOn {
			return adminapi.SlackControls{}, false
		}
		controls := adminapi.SlackControls{
			SyncWorker:     slackcx.NewChannelSync(pool),
			ChannelConfigs: slackcx.NewChannelConfigStore(pool, nil, nil, logger),
			Blacklist:      slackcx.NewIngestBlacklist(pool, logger),
		}
		if userToken {
			controls.Inviter = membership
		}
		return controls, true
	})
	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: logger,
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		SlackSourceControl: api, ReembedLauncher: api, ReembedSources: []string{"slack"},
		Registries: pageRegistries(), SourceDeps: source.Deps{Pool: pool},
		Connectors: &everyConnector{fakeConnectors{list: []settings.View{{Name: "slack", Enabled: true}}}},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	const channelPage = "https://diderot.test/admin/connectors/slack/channels/CPLATFORM"
	page := browserRequest(t, mux, http.MethodGet, channelPage, nil, "")
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	assert.Zero(t, strings.Count(page.Body.String(), " disabled"), "every control that applies works")
	assert.Contains(t, page.Body.String(), "Available while Index documents is on.",
		"reading the documents again waits for document indexing")
	assert.NotContains(t, page.Body.String(), "Slack is off")
	var csrfToken string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.True(t, validCSRFToken(csrfToken))
	post := func(path string, values url.Values) string {
		t.Helper()
		values.Set("csrf_token", csrfToken)
		result := browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, values, csrfToken)
		require.Equal(t, http.StatusSeeOther, result.Code, result.Body.String())
		message, _ := redirectMessage(t, result.Header().Get("Location"))
		return message
	}

	const channel = "/admin/connectors/slack/channels/CPLATFORM"
	assert.Equal(t, "Ask is on in this channel: Diderot answers when someone mentions it here.",
		post(channel+"/config", url.Values{"field": {"ask"}, "enabled": {"true"}}))
	assert.Equal(t, "Documents posted in this channel are indexed now, starting with those already posted.",
		post(channel+"/config", url.Values{"field": {"documents"}, "enabled": {"true"}}))
	assert.Equal(t, "People outside your organization may now ask in this channel.",
		post(channel+"/config", url.Values{"field": {"external"}, "enabled": {"true"}, "confirm": {"external"}}))
	var ask, documents, external bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT ask_enabled, doc_ingest_enabled, allow_external_ask
		FROM slack_channel_config WHERE channel_id = 'CPLATFORM'`).Scan(&ask, &documents, &external))
	assert.True(t, ask && documents && external)

	assert.Equal(t, "Reading this channel's history again. Messages appear in search as Diderot reads them.",
		post(channel+"/backfill", url.Values{}))
	var cursor *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT cursor FROM slack_channel_sync WHERE channel_id = 'CPLATFORM'`).Scan(&cursor))
	assert.Nil(t, cursor, "the walk the leader's worker drains is reset")
	assert.Equal(t, "Reading this channel's documents again.", post(channel+"/backfill", url.Values{"kind": {"documents"}}))

	assert.Equal(t, "The bot is in this channel now. Diderot starts reading it within a few minutes.",
		post(channel+"/membership", url.Values{"joined": {"true"}}))
	assert.Equal(t, "The bot left this channel. Diderot stops reading it; what it indexed stays searchable.",
		post(channel+"/membership", url.Values{"joined": {"false"}}))
	assert.Equal(t, []string{"CPLATFORM"}, membership.invited)
	assert.Equal(t, []string{"CPLATFORM"}, membership.kicked)

	assert.Equal(t, "This channel is blocked. Diderot reads nothing new in it.",
		post(channel+"/blacklist", url.Values{"enabled": {"true"}, "reason": {"privileged"}}))
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT reason FROM slack_ingest_blacklist WHERE channel_id = 'CPLATFORM'`).Scan(&reason))
	assert.Equal(t, "privileged", reason)
	assert.Equal(t, "This channel is unblocked. If the bot is in it, Diderot starts reading it again within a few minutes.",
		post(channel+"/blacklist", url.Values{"enabled": {"false"}}))

	// The re-embed is recorded for the leader, and the page shows it.
	operations := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, csrfToken)
	require.Equal(t, http.StatusOK, operations.Code)
	assert.Contains(t, operations.Body.String(), ">Start re-embed</button>")
	assert.Equal(t, "Re-embed requested. It starts within a few seconds.", post("/admin/operations/reembed", url.Values{"confirm": {"reembed"}, "source": {"slack"}}))
	job, err := adminjob.NewStore(pool).Latest(ctx, reembed.JobKind)
	require.NoError(t, err)
	assert.Equal(t, "queued", job.Status)
	assert.Equal(t, "operator@example.com", job.RequestedBy)
	operations = browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, csrfToken)
	require.Equal(t, http.StatusOK, operations.Code)
	assert.Contains(t, operations.Body.String(), "data-jobs-active")
	assert.NotContains(t, operations.Body.String(), "Start re-embed", "one re-embed at a time")
	assert.Contains(t, operations.Body.String(), `<span class="badge badge-attention">waiting to start</span>`)
	assert.Equal(t, "A re-embed is already waiting or running.", post("/admin/operations/reembed", url.Values{"confirm": {"reembed"}, "source": {"slack"}}))

	// Without a user token only inviting and removing the bot are off.
	userToken = false
	page = browserRequest(t, mux, http.MethodGet, channelPage, nil, csrfToken)
	require.Equal(t, http.StatusOK, page.Code)
	assert.NotContains(t, page.Body.String(), "Remove the bot")
	assert.NotContains(t, page.Body.String(), "Invite the bot")
	assert.Contains(t, page.Body.String(), "Inviting and removing the bot need a Slack user token.")
	assert.Contains(t, page.Body.String(), ">Turn off</button>")
	assert.Contains(t, post(channel+"/membership", url.Values{"joined": {"true"}}), "Slack has no user token")

	// Slack off: the page says so, and an action is refused in the same words.
	slackOn = false
	page = browserRequest(t, mux, http.MethodGet, channelPage, nil, csrfToken)
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "Slack is off. Turn it on to change channels.")
	assert.NotContains(t, page.Body.String(), "Block this channel</button>")
	assert.NotContains(t, page.Body.String(), "Turn off</button>")
	assert.Contains(t, page.Body.String(), ">Rebuild descriptions</button>", "stored messages' images can still be rebuilt")
	assert.Equal(t, "That didn't work: Slack is off.", post(channel+"/config", url.Values{"field": {"ask"}, "enabled": {"false"}}))
	for _, body := range []string{page.Body.String(), operations.Body.String()} {
		assert.NotContains(t, body, "replica")
		assert.NotContains(t, body, "leader")
	}
}
