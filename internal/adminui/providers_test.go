package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// newTestProvider is an OpenAI-compatible provider that lists two models,
// one of them retiring, to the key test-key alone.
func newTestProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		require.Equal(t, "/v1/models", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "chat-model", "name": "Chat Model", "context_length": 200000, "pricing": map[string]string{"prompt": "0.000002", "completion": "0.00001"}},
			{"id": "old-model", "shutdown_date": "2027-01-15"},
		}})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestModelProviderIsAddedListedChangedAndRemoved: an administrator adds a
// provider with its key, which lists its models and turns it on; its page
// shows the listing and never the key; a wrong key fails a save; Models
// offered narrows the menu; turning it off and removing it work.
func TestModelProviderIsAddedListedChangedAndRemoved(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_providers"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE starts_with(connector, 'provider:')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM source_state WHERE starts_with(source, 'provider:')`)
	require.NoError(t, err)
	keys, err := settings.NewKeyring([][]byte{bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)
	store := settings.NewStore(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithConnectors(settings.NewRegistry(), settings.Secrets{Keys: keys}).
		WithFamily(modelprovider.Definitions{Standalone: true}.Family())
	require.NoError(t, store.Load(ctx))
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Connectors: &fakeConnectors{view: exampleConnectorView()}, Providers: store,
		Registries: testRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/new", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), `name="secret.key"`)
	var csrf string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrf = cookie.Value
		}
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf_token", csrf)
		return browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, form, csrf)
	}
	version := func() string {
		t.Helper()
		view, err := store.View(ctx, modelprovider.Row("lab"))
		require.NoError(t, err)
		return strconv.FormatInt(view.Version, 10)
	}
	provider := newTestProvider(t)

	refused := post("/admin/settings/providers", url.Values{"kind": {"openai"}, "name": {"Lab!"}, "base_url": {provider.URL + "/v1"}})
	assert.Equal(t, http.StatusBadRequest, refused.Code)
	assert.Contains(t, refused.Body.String(), "Not added: for the name")
	badURL := post("/admin/settings/providers", url.Values{"kind": {"openai"}, "name": {"lab"}, "base_url": {"ftp://x"}})
	assert.Equal(t, http.StatusBadRequest, badURL.Code)
	assert.Contains(t, badURL.Body.String(), "enter an http or https address")

	created := post("/admin/settings/providers", url.Values{
		"kind": {"openai"}, "name": {"lab"}, "base_url": {provider.URL + "/v1/"}, "secret.key": {"test-key"},
	})
	require.Equal(t, http.StatusSeeOther, created.Code, created.Body.String())
	assert.Equal(t, "/admin/settings/models?notice=lab+is+on.+Choose+the+model+for+each+job+below.", created.Header().Get("Location"))
	view, err := store.View(ctx, modelprovider.Row("lab"))
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	assert.Equal(t, provider.URL+"/v1", view.Settings[modelprovider.FieldBaseURL], "a trailing slash is trimmed")
	rows := store.FamilyRows(modelprovider.Prefix)
	require.Len(t, rows, 1)
	assert.Equal(t, "test-key", modelprovider.FromResolved(rows[0].Resolved).Provider.Endpoint.APIKey)

	body := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/lab", nil, "").Body.String()
	assert.Contains(t, body, `<span class="mono">chat-model</span> <span class="sub">Chat Model</span>`)
	assert.Contains(t, body, "200.0K tokens")
	assert.Contains(t, body, "$2 in, $10 out per million tokens")
	assert.Contains(t, body, "retires 2027-01-15")
	assert.NotContains(t, body, "test-key", "a key is never shown again")
	assert.Contains(t, body, "No job uses its models, and it does not serve the embedding model.")

	// Kind and base URL are fixed: a save never changes them.
	saved := post("/admin/settings/providers/lab", url.Values{
		"version": {version()}, "field.models": {"chat-model\nunlisted-model"}, "field.base_url": {"https://elsewhere.example/v1"},
	})
	require.Equal(t, http.StatusSeeOther, saved.Code, saved.Body.String())
	view, err = store.View(ctx, modelprovider.Row("lab"))
	require.NoError(t, err)
	assert.Equal(t, provider.URL+"/v1", view.Settings[modelprovider.FieldBaseURL])
	menus, err := modelprovider.ReadMenus(ctx, store, pool)
	require.NoError(t, err)
	require.Len(t, menus, 1)
	var ids []string
	for _, model := range menus[0].Models {
		ids = append(ids, model.ID)
	}
	assert.Equal(t, []string{"chat-model", "unlisted-model"}, ids, "Models offered is the menu")

	wrongKey := post("/admin/settings/providers/lab", url.Values{"version": {version()}, "secret.key": {"wrong-key"}})
	require.Equal(t, http.StatusOK, wrongKey.Code)
	assert.Contains(t, wrongKey.Body.String(), "the provider refused the key (HTTP 401)")
	assert.NotContains(t, wrongKey.Body.String(), "wrong-key")

	// The leader's latest listing, when it failed, needs attention.
	failed, err := json.Marshal(modelprovider.Check{Version: mustAtoi(t, version()), Test: settings.TestRecord{
		Passed: false, By: "diderot", At: time.Now().Add(time.Minute), TestReport: settings.TestReport{Checks: []settings.Check{
			{Name: "List models", Detail: "Diderot could not list the provider's models: the provider answered HTTP 500 Internal Server Error."},
		}},
	}})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO source_state (source, key, value, updated_at) VALUES ($1, 'check', $2, now())`,
		modelprovider.Row("lab"), failed)
	require.NoError(t, err)
	summaries, err := server.providerSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.True(t, summaries[0].State.Attention())
	assert.Equal(t, "Diderot could not list the provider's models: the provider answered HTTP 500 Internal Server Error.",
		summaries[0].State.Reason)
	attention, err := server.providerAttention(ctx)
	require.NoError(t, err)
	require.Len(t, attention, 1, "the Overview lists it")

	off := post("/admin/settings/providers/lab/disable", url.Values{"version": {version()}})
	require.Equal(t, http.StatusSeeOther, off.Code)
	assert.Contains(t, off.Header().Get("Location"), "lab+is+off.")
	kept := post("/admin/settings/providers/lab/delete", url.Values{"version": {version()}})
	assert.Contains(t, kept.Header().Get("Location"), "Nothing+was+removed")
	removed := post("/admin/settings/providers/lab/delete", url.Values{"version": {version()}, "confirm": {"remove"}})
	require.Equal(t, http.StatusSeeOther, removed.Code)
	assert.Equal(t, http.StatusNotFound, browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/lab", nil, "").Code)

	var actions []string
	rowsAudit, err := pool.Query(ctx, `SELECT action FROM admin_audit_log WHERE starts_with(action, 'connectors.provider:lab.') ORDER BY id`)
	require.NoError(t, err)
	for rowsAudit.Next() {
		var action string
		require.NoError(t, rowsAudit.Scan(&action))
		actions = append(actions, action)
	}
	assert.Contains(t, actions, "connectors.provider:lab.create", "every change is audited")
	assert.Contains(t, actions, "connectors.provider:lab.delete")
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return n
}
