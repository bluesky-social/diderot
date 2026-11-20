package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminjob"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/settings"
)

// slackCalls is a Slack workspace that records each Web API call with the
// token that made it. The bot is UBOT; ops@example.com is UOPS.
type slackCalls struct {
	mu    sync.Mutex
	calls []string
}

func (s *slackCalls) made() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	made := s.calls
	s.calls = nil
	return made
}

func (s *slackCalls) serve(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		token := r.Form.Get("token")
		if token == "" {
			token = r.Header.Get("Authorization")[len("Bearer "):]
		}
		s.mu.Lock()
		s.calls = append(s.calls, r.URL.Path+" "+token)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT"}`))
		case "/conversations.invite":
			assert.Equal(t, "UBOT", r.Form.Get("users"))
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"CPLATFORM"}}`))
		case "/users.lookupByEmail":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UOPS"}}`))
		case "/conversations.open":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DOPS"}}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"DOPS","ts":"1792006300.000001"}`))
		default:
			t.Errorf("unexpected Slack request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + "/"
}

// TestSlackAdminFollowsTheSavedSettings: with no Slack source and no leader
// term running, a replica's Slack controls and operator notifier work from
// the saved Slack settings, and follow each saved change: a replaced token,
// a user token added, Slack turned off and on.
func TestSlackAdminFollowsTheSavedSettings(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_slack_admin")
	_, err := pool.Exec(ctx, `TRUNCATE slack_channel_sync, slack_ingest_blacklist, slack_users`)
	require.NoError(t, err)
	env := map[string]string{
		settings.EnvName(slackcx.ConnectorName, slackcx.FieldBotToken): "bot-one",
		settings.EnvName(slackcx.ConnectorName, slackcx.FieldAppToken): "app-one",
	}
	load := func() *settings.Store {
		t.Helper()
		store := settings.NewStore(pool, discardLogger()).
			WithConnectors(settings.NewRegistry(slackcx.Connector{}), settings.Secrets{Env: env})
		require.NoError(t, store.Load(ctx))
		return store
	}
	store := load()
	save := func(enabled bool) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE connector_settings SET enabled = $1, version = version + 1,
			settings = '{"notify_emails": ["ops@example.com"], "notify_questions": true}'
			WHERE connector = $2`, enabled, slackcx.ConnectorName)
		require.NoError(t, err)
		require.NoError(t, store.Load(ctx))
	}
	workspace := &slackCalls{}
	live := &liveSlackAdmin{store: store, pool: pool, logger: discardLogger(), apiURL: workspace.serve(t)}

	// Slack off: no controls, and the notifier says nothing.
	_, on := live.Controls()
	require.False(t, on)
	require.NoError(t, live.NotifyAskInvocation(ctx, "asker@example.com", "", "Who owns billing?"))
	require.Empty(t, workspace.made())

	// Slack on: the stores work, and without a user token there is no inviter.
	save(true)
	controls, on := live.Controls()
	require.True(t, on)
	require.Nil(t, controls.Inviter)
	again, _ := live.Controls()
	require.Same(t, controls.ChannelConfigs, again.ChannelConfigs, "the controls are reused until the settings change")
	_, err = controls.SyncWorker.RequestBackfill(ctx, "CPLATFORM")
	require.NoError(t, err)
	var queued int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM slack_channel_sync WHERE channel_id = 'CPLATFORM'`).Scan(&queued))
	require.Equal(t, 1, queued)
	_, err = controls.Blacklist.Add(ctx, "CLEGAL", "legal", "privileged", "operator@example.com")
	require.NoError(t, err)
	require.Empty(t, workspace.made(), "the stores only touch the database")

	require.NoError(t, live.NotifyAskInvocation(ctx, "asker@example.com", "", "Who owns billing?"))
	assert.Equal(t, []string{"/users.lookupByEmail bot-one", "/conversations.open bot-one", "/chat.postMessage bot-one"}, workspace.made())

	// A user token is saved and the bot token replaced: the next use has both.
	env[settings.EnvName(slackcx.ConnectorName, slackcx.FieldBotToken)] = "bot-two"
	env[settings.EnvName(slackcx.ConnectorName, slackcx.FieldUserToken)] = "user-one"
	store = load()
	live.store = store
	save(true)
	controls, on = live.Controls()
	require.True(t, on)
	require.NotNil(t, controls.Inviter)
	require.NoError(t, controls.Inviter.Invite(ctx, "CPLATFORM"))
	require.NoError(t, controls.Inviter.Invite(ctx, "CPLATFORM"))
	assert.Equal(t, []string{
		"/auth.test bot-two", "/conversations.invite user-one", "/conversations.invite user-one",
	}, workspace.made(), "the bot's id is asked for once")

	// Slack turned off, then on again.
	save(false)
	_, on = live.Controls()
	require.False(t, on)
	require.NoError(t, live.NotifyMCPInvocation(ctx, "asker@example.com", "ask", "", "Who owns billing?"))
	require.Empty(t, workspace.made())
	save(true)
	_, on = live.Controls()
	require.True(t, on)
}

// TestReembedJobsWaitForTheSlackRun: the leader's re-embed worker leaves a
// requested job queued while Slack is on but its run has not started, as at
// the start of a term, and runs it once the run is up or when Slack is off.
func TestReembedJobsWaitForTheSlackRun(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_reembed_jobs")
	_, err := pool.Exec(ctx, `TRUNCATE admin_jobs`)
	require.NoError(t, err)
	store := settings.NewStore(pool, discardLogger()).WithConnectors(settings.NewRegistry(slackcx.Connector{}), settings.Secrets{
		Env: map[string]string{
			settings.EnvName(slackcx.ConnectorName, slackcx.FieldBotToken): "bot-one",
			settings.EnvName(slackcx.ConnectorName, slackcx.FieldAppToken): "app-one",
		},
	})
	setSlack := func(enabled bool) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE connector_settings SET enabled = $1, version = version + 1 WHERE connector = $2`,
			enabled, slackcx.ConnectorName)
		require.NoError(t, err)
		require.NoError(t, store.Load(ctx))
	}
	require.NoError(t, store.Load(ctx))
	setSlack(true)
	live := &settings.Live[slackcx.Delivery]{}
	jobs := adminjob.NewStore(pool)
	tick := reembedJobs(pool, events.NewDBSink(pool, prometheus.NewRegistry()), live, store, discardLogger()).Run
	status := func() string {
		t.Helper()
		job, err := jobs.Latest(ctx, reembed.JobKind)
		require.NoError(t, err)
		return job.Status
	}

	_, err = reembed.Request(ctx, jobs, "operator@example.com", nil)
	require.NoError(t, err)
	require.NoError(t, tick(ctx))
	assert.Equal(t, "queued", status(), "the Slack run has not started")

	run, stop := context.WithCancel(ctx)
	live.Set(slackcx.Delivery{Run: run, Rebuilder: slackcx.NewRebuilder(nil, nil, "")})
	require.NoError(t, tick(ctx))
	assert.Equal(t, "succeeded", status())
	stop()
	live.Clear()

	_, err = reembed.Request(ctx, jobs, "operator@example.com", nil)
	require.NoError(t, err)
	require.NoError(t, tick(ctx))
	assert.Equal(t, "queued", status(), "the Slack run has ended")
	setSlack(false)
	require.NoError(t, tick(ctx))
	assert.Equal(t, "succeeded", status(), "with Slack off there is no run to wait for")
}
