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
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestAProviderWithoutAKeyAndWithExtraHeaders: a local server is added with
// no key and no made-up one, and a provider's extra headers are saved, sent,
// and, when secret, never shown again.
func TestAProviderWithoutAKeyAndWithExtraHeaders(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_providers_headers"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE starts_with(connector, 'provider:')`)
	require.NoError(t, err)
	keys, err := settings.NewKeyring([][]byte{bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := settings.NewStore(pool, logger).
		WithConnectors(settings.NewRegistry(), settings.Secrets{Keys: keys}).
		WithFamily(modelprovider.Definitions{Standalone: true}.Family())
	require.NoError(t, store.Load(ctx))
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool, Logger: logger,
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Connectors: &fakeConnectors{view: exampleConnectorView()}, Providers: store,
		Registries: testRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	// A local server that takes no key and wants a header.
	var mu sync.Mutex
	var got http.Header
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-embed"}}})
	}))
	t.Cleanup(local.Close)
	seen := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return got
	}

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/new", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), `name="no_key" value="true"`)
	assert.Contains(t, page.Body.String(), `name="secret.secret_headers"`)
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

	// Without a key and without the mark it is added but stays off.
	keyless := post("/admin/settings/providers", url.Values{"kind": {"openai"}, "name": {"nokey"}, "base_url": {local.URL + "/v1"}})
	require.Equal(t, http.StatusSeeOther, keyless.Code)
	assert.Contains(t, keyless.Header().Get("Location"), "still+off")
	assert.Nil(t, seen(), "nothing is sent without a key by accident")

	created := post("/admin/settings/providers", url.Values{
		"kind": {"openai"}, "name": {"local"}, "base_url": {local.URL + "/v1"}, "no_key": {"true"},
		"headers": {"X-Team: search"}, "secret.secret_headers": {"X-Api-Token: s3cret\r\nX-Other: also"},
	})
	require.Equal(t, http.StatusSeeOther, created.Code, created.Body.String())
	assert.Contains(t, created.Header().Get("Location"), "local+is+on.")
	assert.Empty(t, seen().Get("Authorization"), "no key, and no made-up one")
	assert.Equal(t, "search", seen().Get("X-Team"))
	assert.Equal(t, "s3cret", seen().Get("X-Api-Token"))
	assert.Equal(t, "also", seen().Get("X-Other"))

	body := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/local", nil, "").Body.String()
	assert.Contains(t, body, `name="field.no_key" value="true" checked`)
	assert.Contains(t, body, ">X-Team: search</textarea>")
	assert.NotContains(t, body, "s3cret", "a secret header is never shown again")
	assert.Contains(t, body, "Saved. Enter all of them again to replace them.")

	version := func() string {
		view, err := store.View(ctx, modelprovider.Row("local"))
		require.NoError(t, err)
		return strconv.FormatInt(view.Version, 10)
	}
	// A header Diderot sets itself is refused; the secret ones are kept
	// through a save that does not mention them, and removed when asked.
	refused := post("/admin/settings/providers/local", url.Values{
		"version": {version()}, "field.models": {""}, "field.no_key": {"true"}, "field.headers": {"Authorization: Bearer x"},
	})
	assert.Equal(t, http.StatusBadRequest, refused.Code)
	assert.Contains(t, refused.Body.String(), "Authorization is set by Diderot itself")
	saved := post("/admin/settings/providers/local", url.Values{
		"version": {version()}, "field.models": {""}, "field.no_key": {"true"}, "field.headers": {"X-Team: ops"},
	})
	require.Equal(t, http.StatusSeeOther, saved.Code, saved.Body.String())
	assert.Equal(t, "ops", seen().Get("X-Team"))
	assert.Equal(t, "s3cret", seen().Get("X-Api-Token"))
	removed := post("/admin/settings/providers/local", url.Values{
		"version": {version()}, "field.models": {""}, "field.no_key": {"true"}, "field.headers": {""}, "secret.secret_headers.remove": {"true"},
	})
	require.Equal(t, http.StatusSeeOther, removed.Code, removed.Body.String())
	assert.Empty(t, seen().Get("X-Api-Token"))
	assert.Empty(t, seen().Get("X-Team"))

	// Secret headers that are not headers are refused before anything is
	// saved, without their text: a provider is never left with headers
	// that can't be read.
	broken := post("/admin/settings/providers/local", url.Values{
		"version": {version()}, "field.models": {""}, "field.no_key": {"true"}, "field.headers": {""}, "secret.secret_headers": {"t0p-s3cret-without-a-name"},
	})
	assert.Equal(t, http.StatusBadRequest, broken.Code)
	assert.Contains(t, broken.Body.String(), "Enter each header as Name: value: line 1 is not a header as Name: value.")
	assert.NotContains(t, broken.Body.String(), "t0p-s3cret")
	rows := store.FamilyRows(modelprovider.Prefix)
	for _, row := range rows {
		assert.NoError(t, modelprovider.FromResolved(row.Resolved).HeadersError)
	}
	added := post("/admin/settings/providers", url.Values{
		"kind": {"openai"}, "name": {"other"}, "base_url": {local.URL + "/v1"}, "no_key": {"true"}, "secret.secret_headers": {"nope"},
	})
	assert.Equal(t, http.StatusBadRequest, added.Code)
	assert.Contains(t, added.Body.String(), "line 1 is not a header")

	// Anthropic's API always takes a key.
	anthropic := post("/admin/settings/providers", url.Values{"kind": {"anthropic"}, "name": {"claude"}, "base_url": {"https://api.anthropic.com"}, "no_key": {"true"}})
	assert.Equal(t, http.StatusBadRequest, anthropic.Code)
	assert.Contains(t, anthropic.Body.String(), "Anthropic always takes a key.")
}
