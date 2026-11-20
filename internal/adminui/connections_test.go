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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// newTestMCPServer serves a read-only and a writing tool over MCP.
func newTestMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	for name, readOnly := range map[string]bool{"list_deployments": true, "rollback": false} {
		srv.AddTool(&mcp.Tool{
			Name: name, Description: "Tool " + name + ".", InputSchema: map[string]any{"type": "object"},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	}
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(server.Close)
	return server
}

// TestToolConnectionIsAddedTestedSavedAndRemoved: an administrator adds a
// connection, tests it, which lists the server's tools to choose from,
// saves the chosen ones and turns it on, sees its state on the Connectors
// page and on the Overview when its latest check fails, and removes it. A
// pasted credential is never shown again.
func TestToolConnectionIsAddedTestedSavedAndRemoved(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_connections"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE starts_with(connector, 'connection:')`)
	require.NoError(t, err)
	keys, err := settings.NewKeyring([][]byte{bytes.Repeat([]byte{9}, 32)})
	require.NoError(t, err)
	store := settings.NewStore(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithConnectors(settings.NewRegistry(), settings.Secrets{Keys: keys}).
		WithFamily(askconn.Definitions{Standalone: true}.Family()) // the fake server is on loopback
	require.NoError(t, store.Load(ctx))
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Connectors: &fakeConnectors{view: exampleConnectorView()}, Connections: store,
		Registries: testRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connections/new", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	var csrf string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrf = cookie.Value
		}
	}
	require.NotEmpty(t, csrf)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf_token", csrf)
		return browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, form, csrf)
	}
	version := func() string {
		t.Helper()
		view, err := store.View(ctx, askconn.Row("deployments"))
		require.NoError(t, err)
		return strconv.FormatInt(view.Version, 10)
	}

	refused := post("/admin/connections", url.Values{"kind": {"generic"}, "name": {"Deploys!"}})
	assert.Equal(t, http.StatusBadRequest, refused.Code)
	assert.Contains(t, refused.Body.String(), "Not added: for the name, use 1 to 40 lowercase letters")

	created := post("/admin/connections", url.Values{"kind": {"generic"}, "name": {"deployments"}})
	require.Equal(t, http.StatusSeeOther, created.Code)
	assert.Equal(t, "/admin/connections/deployments?notice=Connection+added.+Fill+in+its+settings%2C+then+save+and+turn+it+on.", created.Header().Get("Location"))
	again := post("/admin/connections", url.Values{"kind": {"generic"}, "name": {"deployments"}})
	assert.Contains(t, again.Body.String(), "a connection named deployments already exists")

	mcpServer := newTestMCPServer(t)
	settingsForm := func(extra url.Values) url.Values {
		form := url.Values{
			"version": {version()}, "field.url": {mcpServer.URL + "/mcp"}, "field.agents": {"both"},
			"field.auth_scheme": {""}, "field.actor_header": {""}, "field.timeout": {"10s"},
			"secret.token": {"pasted-credential-123"}, "tools": {"shown"},
		}
		for key, values := range extra {
			form[key] = values
		}
		return form
	}
	tested := post("/admin/connections/deployments/test", settingsForm(nil))
	require.Equal(t, http.StatusOK, tested.Code)
	body := tested.Body.String()
	assert.Contains(t, body, "The test failed. Fix what it reports and test again.", "no tool is chosen yet")
	assert.Contains(t, body, `name="tool" value="list_deployments"`)
	assert.Contains(t, body, "Marked read-only by the server")
	assert.Contains(t, body, `name="tool" value="rollback"`)
	assert.Contains(t, body, "Not marked read-only")
	assert.Contains(t, body, "Paste it again to save it.")
	assert.NotContains(t, body, "pasted-credential-123", "a pasted credential is never shown again")

	saved := post("/admin/connections/deployments/enable", settingsForm(url.Values{"tool": {"list_deployments"}}))
	require.Equal(t, http.StatusSeeOther, saved.Code, saved.Body.String())
	assert.Contains(t, saved.Header().Get("Location"), "deployments+is+on.")
	view, err := store.View(ctx, askconn.Row("deployments"))
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	assert.Equal(t, []string{"list_deployments"}, view.Settings[askconn.FieldTools])
	assert.True(t, view.Secrets[askconn.FieldToken].Saved)
	connection := askconn.FromResolved(store.FamilyRows(askconn.Prefix)[0].Resolved)
	assert.Equal(t, "pasted-credential-123", connection.Token)
	assert.Equal(t, askconn.AgentsBoth, connection.Agents)

	list := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors", nil, "")
	require.Equal(t, http.StatusOK, list.Code)
	assert.Contains(t, list.Body.String(), `<a class="row-link" href="/admin/connections/deployments">deployments</a><span class="sub">MCP server · 127.0.0.1:`)

	// The leader's latest check of what is saved failing makes it need
	// attention, here and on the Overview.
	check, err := json.Marshal(ConnectionCheck{Version: view.Version, Test: settings.TestRecord{TestReport: settings.TestReport{
		Checks: []settings.Check{{Name: "Connect", Detail: "Diderot could not list the server's tools: connect: refused."}},
	}}})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO source_state (source, key, value) VALUES ($1, $2, $3)`, view.Name, ConnectionCheckKey, check)
	require.NoError(t, err)
	page = browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connections/deployments", nil, "")
	assert.Contains(t, page.Body.String(), "Its latest check failed: diderot could not list the server&#39;s tools: connect: refused.")
	items, err := server.connectionAttention(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "/admin/connections/deployments", items[0].Href)

	kept := post("/admin/connections/deployments/delete", url.Values{"version": {version()}})
	assert.Contains(t, kept.Header().Get("Location"), "Nothing+was+removed")
	removed := post("/admin/connections/deployments/delete", url.Values{"version": {version()}, "confirm": {"remove"}})
	require.Equal(t, http.StatusSeeOther, removed.Code)
	_, err = store.View(ctx, askconn.Row("deployments"))
	require.ErrorIs(t, err, settings.ErrUnknownConnector)
	gone := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connections/deployments", nil, "")
	assert.Equal(t, http.StatusNotFound, gone.Code)
}
