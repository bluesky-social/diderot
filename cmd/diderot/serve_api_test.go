package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestSessionsPageWithoutPrivacyServiceAnswers503: with an invalid session
// ingest policy, privacy self-service is off, and /sessions says so rather
// than calling into a service that was never built.
func TestSessionsPageWithoutPrivacyServiceAnswers503(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_serve_api"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	policy := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(policy, []byte("not: [a policy"), 0o600))
	cfg := &config.Config{
		AuthMode: config.AuthModeNone, NoneEmail: "admin@example.com",
		AdminGroups: []string{"admins"}, SelfServiceGroups: []string{"users"},
		PublicURL: "http://127.0.0.1:9091", SessionIngestPolicyPath: policy,
	}
	sa, err := buildServerAuth(cfg, []string{"127.0.0.1:9091"}, nil, discardLogger())
	require.NoError(t, err)
	store := settings.NewStore(pool, discardLogger())
	connectors := connectorSet{
		store:       store,
		registries:  setup.Registries{Sources: sourceRegistry(pool, nil, nil, nil, nil, nil, false), Tools: toolRegistry(), Automations: automationRegistry(cfg, store)},
		sessionFeed: &liveSessionFeed{store: store},
	}
	ops := operatorServices{sessionPrivacy: newSessionPrivacy(cfg, pool, discardLogger())}
	require.Nil(t, ops.sessionPrivacy, "an invalid policy turns privacy self-service off")

	mux := http.NewServeMux()
	uiCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	require.NoError(t, mountBrowserUI(uiCtx, mux, cfg, sa, connectors, ops, adminapi.NewServer(sa.api, discardLogger()), pool, nil, discardLogger()))

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}
