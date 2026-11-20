package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestSessionFeedConnectorName: a new install's settings row for the session
// feed is session_feed, its token comes from DIDEROT_SESSION_FEED_TOKEN, and
// the browser serves it under that name.
func TestSessionFeedConnectorName(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_session_feed_name"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		AuthMode: config.AuthModeNone, NoneEmail: "admin@example.com",
		AdminGroups: []string{"admins"}, SelfServiceGroups: []string{"users"},
		PublicURL: "http://127.0.0.1:9091",
	}
	sa, err := buildServerAuth(cfg, []string{"127.0.0.1:9091"}, nil, discardLogger())
	require.NoError(t, err)
	store := settings.NewStore(pool, discardLogger())
	registries := setup.Registries{
		Sources: sourceRegistry(pool, nil, nil, nil, nil, nil, false), Tools: toolRegistry(), Automations: automationRegistry(cfg, store),
	}
	registry := connectorRegistry(registries.Sources, registries.Tools, registries.Automations)
	env := map[string]string{"DIDEROT_SESSION_FEED_TOKEN": "feed-fixture"}
	store.WithConnectors(registry, settings.Secrets{Env: settings.ReadEnv(registry, func(name string) string { return env[name] })})
	require.NoError(t, store.Load(ctx))

	rows, err := pool.Query(ctx, `SELECT connector FROM connector_settings WHERE connector = 'session_feed'`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"session_feed"}, names)

	view, err := store.View(ctx, sessionfeed.ConnectorName)
	require.NoError(t, err)
	assert.Equal(t, map[string]settings.SecretView{sessionfeed.FieldToken: {Env: "DIDEROT_SESSION_FEED_TOKEN"}}, view.Secrets)

	admin := adminapi.NewServer(sa.api, discardLogger())
	mux := http.NewServeMux()
	require.NoError(t, admin.Mount(mux))
	uiCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	connectors := connectorSet{store: store, registries: registries, sessionFeed: &liveSessionFeed{store: store}}
	require.NoError(t, mountBrowserUI(uiCtx, mux, cfg, sa, connectors,
		operatorServices{sourceDeps: source.Deps{Pool: pool, Logger: discardLogger()}}, admin, pool, nil, discardLogger()))

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	response := get("/admin/connectors/session_feed")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Session feed")
}
