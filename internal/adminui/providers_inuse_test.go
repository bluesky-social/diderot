package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestAProviderInUseIsNotStoppedByOneClick: turning off a provider that
// serves a job or search names what stops and asks; removing it, or its key
// while it is on, is refused until nothing uses it.
func TestAProviderInUseIsNotStoppedByOneClick(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_providers_inuse"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE starts_with(connector, 'provider:') OR connector = 'models'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM source_state WHERE starts_with(source, 'provider:')`)
	require.NoError(t, err)
	keys, err := settings.NewKeyring([][]byte{bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := settings.NewStore(pool, logger).
		WithConnectors(settings.NewRegistry(), settings.Secrets{Keys: keys}).
		WithFamily(modelprovider.Definitions{Standalone: true}.Family())
	require.NoError(t, store.Load(ctx))
	catalog := &pageCatalog{fails: map[string]string{}}
	service := setup.New(pool, store, setup.Registries{}, setup.Facts{}, catalog,
		func(context.Context, settings.Models, ...string) []settings.RoleTest { return nil })
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool, Logger: logger,
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Connectors: &fakeConnectors{view: exampleConnectorView()}, Providers: store, Setup: service,
		Registries: testRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	// The provider openai, on, serving Ask's model and the embedding model.
	actor := settings.Actor{Email: "op@example.com"}
	provider := newTestProvider(t)
	created, err := store.Create(ctx, actor, modelprovider.Row("openai"), map[string]any{
		modelprovider.FieldKind: "openai", modelprovider.FieldBaseURL: provider.URL + "/v1",
	})
	require.NoError(t, err)
	_, err = store.Save(ctx, actor, created.Name, created.Version, nil,
		map[string]settings.SecretInput{modelprovider.FieldKey: {Value: "test-key"}}, true)
	require.NoError(t, err)
	_, err = store.UpdateModels(ctx, store.Models().Version, actor, "test", func(models *settings.Models) error {
		models.Roles = map[string]string{"ask": "openai/model-a"}
		models.Available = []modelregistry.Model{{Provider: "openai", ID: "model-a", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses}}}
		models.Embedding = &settings.Embedding{Provider: "openai", Model: "embed-a", Dimensions: 1536, Collection: "c"}
		return nil
	})
	require.NoError(t, err)

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/settings/providers/openai", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "It can't be removed while these use it: Ask; Search, and Ask, which searches")
	var csrf string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrf = cookie.Value
		}
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		view, err := store.View(ctx, modelprovider.Row("openai"))
		require.NoError(t, err)
		form.Set("csrf_token", csrf)
		form.Set("version", strconv.FormatInt(view.Version, 10))
		return browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, form, csrf)
	}
	enabled := func() bool {
		t.Helper()
		view, err := store.View(ctx, modelprovider.Row("openai"))
		require.NoError(t, err)
		return view.Enabled
	}

	// Turn off: one click names what stops and changes nothing.
	asked := post("/admin/settings/providers/openai/disable", url.Values{})
	assert.Equal(t, http.StatusUnprocessableEntity, asked.Code)
	assert.Contains(t, asked.Body.String(), "Turning openai off stops:")
	assert.Contains(t, asked.Body.String(), "<li>Ask</li>")
	assert.Contains(t, asked.Body.String(), "<li>Search, and Ask, which searches (it serves the embedding model)</li>")
	assert.Contains(t, asked.Body.String(), `name="confirm" value="off" required`)
	assert.True(t, enabled(), "nothing is turned off until it is confirmed")

	// Its key can't be taken away while it is on and in use.
	keyless := post("/admin/settings/providers/openai", url.Values{"secret.key.remove": {"true"}})
	assert.Equal(t, http.StatusUnprocessableEntity, keyless.Code)
	assert.Contains(t, keyless.Body.String(), "use this provider and would stop without its key")
	rows := store.FamilyRows(modelprovider.Prefix)
	require.Len(t, rows, 1)
	assert.Equal(t, "test-key", modelprovider.FromResolved(rows[0].Resolved).Provider.Endpoint.APIKey)

	// Nor can it be removed, confirmed or not.
	removed := post("/admin/settings/providers/openai/delete", url.Values{"confirm": {"remove"}})
	require.Equal(t, http.StatusSeeOther, removed.Code)
	assert.Contains(t, removed.Header().Get("Location"), "error=Nothing+was+removed%3A+Ask")
	_, err = store.View(ctx, modelprovider.Row("openai"))
	require.NoError(t, err, "the provider is still there")

	// Confirmed, it is turned off; its jobs' models stay saved.
	off := post("/admin/settings/providers/openai/disable", url.Values{"confirm": {"off"}})
	require.Equal(t, http.StatusSeeOther, off.Code, off.Body.String())
	assert.False(t, enabled())
	assert.Equal(t, "openai/model-a", store.Models().Roles["ask"])

	// A provider nothing uses is turned off and removed as before.
	_, err = store.UpdateModels(ctx, store.Models().Version, actor, "test", func(models *settings.Models) error {
		models.Roles, models.Embedding = nil, nil
		return nil
	})
	require.NoError(t, err)
	gone := post("/admin/settings/providers/openai/delete", url.Values{"confirm": {"remove"}})
	require.Equal(t, http.StatusSeeOther, gone.Code)
	assert.Contains(t, gone.Header().Get("Location"), "notice=openai+is+removed")
}
