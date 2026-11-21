package adminui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// bootstrapOrProxy identifies the bootstrap admin by a test header and
// everyone else through the trusted proxy.
type bootstrapOrProxy struct{ proxy auth.Authenticator }

func (b bootstrapOrProxy) Authenticate(r *http.Request) (*auth.Principal, error) {
	if r.Header.Get("X-Test-Bootstrap") != "" {
		return auth.BootstrapPrincipal(), nil
	}
	return b.proxy.Authenticate(r)
}

type setupHarness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *settings.Store
	mux     *http.ServeMux
	catalog *pageCatalog
}

// pageCatalog is the server's catalog for these pages: Ask is offered two
// models of provider openai, image captions are off, and two embedding
// models are offered. A role uses its saved model once the model's entry
// passed the role's test; a test of a ref in fails fails with its reason.
type pageCatalog struct {
	// published is what is published about a model, by ref.
	published map[string]modelregistry.Published
	fails     map[string]string
	// tests counts the role tests saves ran.
	tests int
}

func (c *pageCatalog) Roles(_ context.Context, saved settings.Models) ([]setup.RoleView, error) {
	ask := setup.RoleView{ModelRole: setup.ModelRole{Key: "ask", Title: "Ask", Description: "Answers questions.", Options: []setup.ModelOption{
		{Ref: "openai/model-a", ID: "model-a", Provider: "openai", ReasoningEfforts: []string{"low", "high"}, DefaultEffort: "low", InputPrice: ptr(2.0), OutputPrice: ptr(10.0)},
		{Ref: "openai/model-b", ID: "model-b", Provider: "openai"},
	}}, Saved: saved.Roles["ask"]}
	if ask.Saved != "" {
		if i := slices.IndexFunc(saved.Available, func(m modelregistry.Model) bool { return m.Ref() == ask.Saved }); i >= 0 {
			ask.Model = ask.Saved
		} else {
			ask.Warning = "It has not passed this job's test. Save it for this job again to test it."
		}
	}
	captions := setup.RoleView{ModelRole: setup.ModelRole{Key: "image_captions", Title: "Image captions", Description: "Describes images.",
		Unavailable: "This needs file storage. Set DIDEROT_IMAGES_BUCKET in the deployment."}}
	return []setup.RoleView{ask, captions}, nil
}

func (c *pageCatalog) TestRole(_ context.Context, _ settings.Models, role, ref, effort string) (modelregistry.Model, error) {
	c.tests++
	if reason, fails := c.fails[ref]; fails {
		return modelregistry.Model{}, &modelregistry.TestError{Reason: reason}
	}
	provider, id, _ := modelregistry.ParseRef(ref)
	entry := modelregistry.Model{Provider: provider, ID: id, Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses},
		TestedFor: []modelregistry.Role{modelregistry.Role(role)}}
	if effort != "" {
		entry.ReasoningEfforts = []string{effort}
	}
	return entry, nil
}

func (c *pageCatalog) Embedding(context.Context, settings.Models) ([]setup.ModelOption, string, string, error) {
	return []setup.ModelOption{{Ref: "openai/embed-a", ID: "embed-a", Provider: "openai"}, {Ref: "openai/embed-b", ID: "embed-b", Provider: "openai"}}, "", "", nil
}

func (c *pageCatalog) EmbeddingDimensions(context.Context, settings.Models, string) (int, error) {
	return 1536, nil
}

// FactsStatus, Published and Detail: the pages' catalog knows what its facts field says is
// published, and nothing else.
func (c *pageCatalog) FactsStatus(_ context.Context, saved settings.Models) (setup.FactsStatus, error) {
	return setup.FactsStatus{Off: saved.PublishedFactsOff}, nil
}

func (c *pageCatalog) Published(_ context.Context, _ settings.Models, ref string, pin *string) (modelregistry.Published, error) {
	if pin != nil && *pin == "nowhere/none" {
		return modelregistry.Published{}, fmt.Errorf("%w: the published catalog has no entry %s", settings.ErrInvalid, *pin)
	}
	return c.published[ref], nil
}

func (c *pageCatalog) Detail(_ context.Context, saved settings.Models, ref string) (setup.ModelDetail, error) {
	detail := setup.ModelDetail{Off: saved.PublishedFactsOff}
	for _, entry := range saved.Available {
		if entry.Ref() != ref {
			continue
		}
		detail.Match.Pin = entry.Catalog
		detail.Match.Suggestions = []setup.MatchSuggestion{{Ref: "maker/model-a-1", Name: "Model A at Maker"}}
		if entry.Catalog != "" && entry.Catalog != modelregistry.CatalogNone {
			detail.Match.Ref, detail.Match.Pinned, detail.Match.How = entry.Catalog, true, "Set by an administrator."
		}
		published := entry.WithPublished(c.published[ref], true)
		detail.Differs = !entry.SameRequests(published)
		row := setup.FactRow{Key: modelregistry.FactTemperature, Label: "Chat requests carry a temperature", Value: "yes", Source: entry.SourceOf(modelregistry.FactTemperature).Title()}
		if entry.ChatOmitTemperature {
			row.Value = "no"
		}
		if published.ChatOmitTemperature != entry.ChatOmitTemperature {
			row.Published, row.PublishedBy = "no", "from the published catalog"
		}
		detail.Facts = []setup.FactRow{row}
		detail.Info = []setup.InfoRow{{Label: "Price", Value: "$2 in, $10 out per million tokens", Source: "the published catalog"}}
	}
	return detail, nil
}

// CanServe refuses an image job an entry that takes no images.
func (c *pageCatalog) CanServe(_ settings.Models, role string, entry modelregistry.Model) error {
	if modelregistry.NeedsImages(modelregistry.Role(role)) && !entry.Images {
		return errors.New("the job sends images, which the model would no longer accept")
	}
	return nil
}

func (c *pageCatalog) PrepareCollection(context.Context, settings.Embedding, bool) error { return nil }

func ptr[T any](v T) *T { return &v }

func newSetupHarness(t *testing.T) *setupHarness {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_setup"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE connector = 'models'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE connector_settings
		   SET settings = '{"ORG_NAME":"","ORG_DESCRIPTION":"","state":"setup","bootstrap_admin_enabled":true}',
		       version = 1, updated_by = 'diderot:migration'
		 WHERE connector = 'org'`)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := settings.NewStore(pool, logger)
	require.NoError(t, store.Load(ctx))
	h := &setupHarness{t: t, pool: pool, store: store, mux: http.NewServeMux(), catalog: &pageCatalog{fails: map[string]string{}}}
	tester := func(_ context.Context, saved settings.Models, roles ...string) []settings.RoleTest {
		require.Empty(t, roles, "Test models tests every role")
		return []settings.RoleTest{{Role: "Ask", Model: saved.Roles["ask"], Passed: true}}
	}
	s := &Server{
		pool: pool, authenticator: bootstrapOrProxy{testBrowserAuth()}, logger: logger, version: "test",
		adminGroups: map[string]struct{}{"admins": {}}, publicOrigin: "https://diderot.test",
		setup: setup.New(pool, store, setup.Registries{}, setup.Facts{
			Access: setup.Access{AuthMode: "static", AdminGroups: []string{"admins"}},
		}, h.catalog, tester),
	}
	require.NoError(t, s.Mount(h.mux))
	return h
}

func (h *setupHarness) get(path string, bootstrap bool) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	h.identify(r, bootstrap)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

func (h *setupHarness) identify(r *http.Request, bootstrap bool) {
	if bootstrap {
		r.Header.Set("X-Test-Bootstrap", "1")
		return
	}
	r.Header.Set(proxyEmailHeader, "op@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
}

func (h *setupHarness) post(path string, form url.Values, bootstrap bool) *httptest.ResponseRecorder {
	h.t.Helper()
	form.Set("csrf_token", strings.Repeat("A", 43))
	if form.Get("version") == "" {
		form.Set("version", "1")
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://diderot.test")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: strings.Repeat("A", 43)})
	h.identify(r, bootstrap)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

func TestSetupPageAndBanner(t *testing.T) {
	h := newSetupHarness(t)
	w := h.get("/admin/settings", true)
	require.Equal(t, http.StatusOK, w.Code, "the bootstrap admin is an administrator")
	body := w.Body.String()
	assert.NotContains(t, body, "app-banner", "the checklist says what the banner would")
	assert.Contains(t, body, "Until you go live, only administrators can use Ask.")
	assert.Contains(t, body, `<a class="button button-primary" href="/admin/settings/organization">Name your organization</a>`,
		"the first unfinished step is the one to do next")
	assert.Contains(t, body, `<li class="step step-current" id="organization">`)
	assert.Contains(t, body, `<span class="account-name" title="admin@bootstrap.invalid">admin (bootstrap)</span>`)
	assert.Contains(t, body, "First, sign in another way.")
	assert.NotContains(t, body, "Turn off the bootstrap admin</button>", "the bootstrap admin cannot turn itself off")
	assert.NotContains(t, body, "Go live</button>", "Go live is offered once every step is done")
	assert.Contains(t, body, `<a href="/admin/connectors#connections">Tool connections</a>`)

	body = h.get("/admin/settings", false).Body.String()
	assert.Contains(t, body, "Turn off the bootstrap admin</button>")
	assert.NotContains(t, body, "First, sign in another way.")
	for _, path := range []string{"/admin/settings/organization", "/admin/settings/models", "/admin/settings/access"} {
		w := h.get(path, false)
		require.Equal(t, http.StatusOK, w.Code, path)
		assert.Equal(t, 1, strings.Count(w.Body.String(), `class="app-banner"`), path)
		assert.Contains(t, w.Body.String(), `Diderot is still being set up: only administrators can use Ask. <a href="/admin/settings">Continue setup</a>`, path)
		assert.Contains(t, w.Body.String(), `href="/admin/settings">Back to the checklist</a>`, path)
	}
	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, "This step of setup is not done yet.")
	assert.Contains(t, body, `href="/admin/settings/organization">Next: Organization</a>`)

	_, err := h.store.UpdateOrg(context.Background(), 1, settings.Actor{Email: "op@example.com"}, "test", func(org *settings.Org) error {
		org.State = settings.StateLive
		return nil
	})
	require.NoError(t, err)
	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `class="app-banner app-banner-bad" role="alert"`, "a live install with the bootstrap admin on says so")
	assert.Contains(t, body, `<a href="/admin/settings#bootstrap-admin">Turn it off</a>`)
	assert.NotContains(t, body, "Back to the checklist", "a live install has no setup to go back to")

	_, err = h.store.UpdateOrg(context.Background(), 2, settings.Actor{Email: "op@example.com"}, "test", func(org *settings.Org) error {
		org.BootstrapAdminEnabled = false
		return nil
	})
	require.NoError(t, err)
	assert.NotContains(t, h.get("/admin/settings/models", false).Body.String(), "app-banner")
	body = h.get("/admin/settings", false).Body.String()
	assert.NotContains(t, body, "app-banner")
	assert.Contains(t, body, "Diderot is live: everyone who can sign in can use Ask.")
	assert.Contains(t, body, "Go back to setup</button>")
}

// TestNavSections: the nav lists each section once, in order; the Settings
// pages highlight Settings and their own tab.
func TestNavSections(t *testing.T) {
	h := newSetupHarness(t)
	for path, tab := range map[string]string{
		"/admin/settings": "Setup checklist", "/admin/settings/organization": "Organization",
		"/admin/settings/models": "Models", "/admin/settings/access": "Access",
	} {
		body := h.get(path, false).Body.String()
		start := strings.Index(body, `<nav class="app-nav"`)
		nav := body[start : start+strings.Index(body[start:], "</nav>")]
		var sections []string
		for _, match := range regexp.MustCompile(`>([^<]+)</a>`).FindAllStringSubmatch(nav, -1) {
			sections = append(sections, match[1])
		}
		assert.Equal(t, []string{
			"Overview", "Ask", "Connectors", "Prompts", "Reports", "Operations", "Settings",
		}, sections, path)
		assert.Contains(t, nav, `<a href="/admin/settings" aria-current="page">Settings</a>`, path)
		assert.Equal(t, 1, strings.Count(nav, "aria-current"), path)

		start = strings.Index(body, `<nav class="tabs" aria-label="Settings">`)
		require.Positive(t, start, path)
		tabs := body[start : start+strings.Index(body[start:], "</nav>")]
		assert.Contains(t, tabs, `<a href="`+path+`" aria-current="page">`+tab+`</a>`, path)
		assert.Equal(t, 1, strings.Count(tabs, "aria-current"), path)
		for _, label := range []string{"Setup checklist", "Organization", "Models", "Access"} {
			assert.Contains(t, tabs, ">"+label+"</a>", path)
		}
	}
}

// TestFirstRunLandsOnSetup: during setup /admin/ is the checklist; once the
// install is live it is the Overview.
func TestFirstRunLandsOnSetup(t *testing.T) {
	h := newSetupHarness(t)
	w := h.get("/admin/", true)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/settings", w.Header().Get("Location"))
	w = h.get("/admin/?range=7d", false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/settings", w.Header().Get("Location"))

	_, err := h.store.UpdateOrg(context.Background(), 1, settings.Actor{Email: "op@example.com"}, "test", func(org *settings.Org) error {
		org.State = settings.StateLive
		return nil
	})
	require.NoError(t, err)
	w = h.get("/admin/", false)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<title>Overview · Diderot</title>")
}

func TestSetupActionsFromTheBrowser(t *testing.T) {
	h := newSetupHarness(t)
	w := h.post("/admin/settings/actions/disable-bootstrap-admin", url.Values{}, true)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "error=Sign+in+another+way", "a refused action comes back as an error")
	assert.True(t, h.store.Org().BootstrapAdminEnabled)

	w = h.post("/admin/settings/actions/disable-bootstrap-admin", url.Values{}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/settings?notice="+url.QueryEscape("The bootstrap admin is off."), w.Header().Get("Location"))
	assert.False(t, h.store.Org().BootstrapAdminEnabled)

	w = h.post("/admin/settings/actions/acknowledge-access", url.Values{}, false)
	assert.Contains(t, w.Header().Get("Location"), "error=Someone+changed+this", "the form's version is stale")
	w = h.post("/admin/settings/actions/acknowledge-access", url.Values{"version": {"2"}}, false)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), "/admin/settings/access?notice=Access+marked+as+reviewed"))

	w = h.post("/admin/settings/actions/go-live", url.Values{"version": {"3"}}, false)
	assert.Contains(t, w.Header().Get("Location"), "Finish+every+required+step")
	assert.False(t, h.store.Org().Live())

	assert.Equal(t, http.StatusNotFound, h.post("/admin/settings/actions/reboot", url.Values{}, false).Code)
	r := httptest.NewRequest(http.MethodPost, "/admin/settings/actions/go-live", strings.NewReader("version=3"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.identify(r, false)
	w = httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code, "actions need the CSRF token")

	var actor string
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT actor_email FROM admin_audit_log WHERE action = 'setup.disable_bootstrap_admin'`).Scan(&actor))
	assert.Equal(t, "op@example.com", actor)
}

// TestModelsPage: a role that is on offers every provider's models with none
// chosen at first, a role that is off says why with no choice, and Ask
// carries its limits. A save tests the new choice and stores the form; a
// rejected one keeps what was chosen.
func TestModelsPage(t *testing.T) {
	h := newSetupHarness(t)
	w := h.get("/admin/settings/models", false)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<h3 id="role-ask-title">Ask</h3>`)
	assert.Contains(t, body, `<select id="role-ask" name="role.ask">`)
	assert.Contains(t, body, `<option value="openai/model-a" data-reasoning-efforts="low high " data-default-effort="low">model-a</option>`)
	assert.Contains(t, body, `<option value="" selected>Off: no model</option>`, "nothing is chosen for an administrator")
	assert.Contains(t, body, `<optgroup label="openai"><option value="openai/model-a" data-reasoning-efforts="low high " data-default-effort="low">model-a</option><option value="openai/model-b">model-b</option></optgroup>`)
	assert.Contains(t, body, "<dt>Image captions</dt><dd>This needs file storage. Set DIDEROT_IMAGES_BUCKET in the deployment.</dd>",
		"a job that is off is listed once, with what would turn it on")
	assert.NotContains(t, body, `name="role.image_captions"`, "a role that is off has no choice")
	assert.Contains(t, body, `name="ask_max_tool_turns" type="number" min="1" max="50" value="8"`)
	assert.Contains(t, body, `<h3>No provider yet</h3>`)
	assert.Contains(t, body, `<option value="" selected>Choose a model</option>`, "no embedding model is chosen either")
	assert.Contains(t, body, `name="version" value="1"`)

	w = h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-b"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"many"}}, false)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "Maximum tool turns must be a number from 1 to 50")
	assert.Contains(t, w.Body.String(), `<option value="openai/model-b" selected>model-b</option>`, "the form keeps what was chosen")
	assert.Zero(t, h.catalog.tests, "a refused form calls no model")

	w = h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-a"}, "ask_reasoning_effort": {"high"}, "ask_max_tool_turns": {"12"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/settings/models?notice=Models+saved.", w.Header().Get("Location"))
	assert.Equal(t, 1, h.catalog.tests)
	saved := h.store.Models()
	assert.Equal(t, map[string]string{"ask": "openai/model-a"}, saved.Roles)
	assert.Equal(t, "high", saved.AskReasoningEffort)
	assert.Equal(t, 12, saved.AskMaxToolTurns)
	require.Len(t, saved.Available, 1)
	assert.Equal(t, "op@example.com", saved.UpdatedBy)

	w = h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-b"}}, false)
	assert.Contains(t, w.Header().Get("Location"), "error=Someone+changed+this", "the form's version is stale")

	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `<option value="openai/model-a" selected data-reasoning-efforts="low high " data-default-effort="low">model-a</option>`)
	assert.Contains(t, body, "$2 in, $10 out per million tokens", "the model in use shows its listed price")
	assert.Contains(t, body, `<option value="high" selected>high</option>`)
	assert.Contains(t, body, `<a class="mono" href="/admin/settings/models/model?ref=openai%2fmodel-a">model-a</a>`)
}

// TestModelsPageRefusesAModelThatFailsItsTest: a save whose new model fails
// the job's test saves nothing, and says why on the form with the choices
// kept; once it passes, the save goes through.
func TestModelsPageRefusesAModelThatFailsItsTest(t *testing.T) {
	h := newSetupHarness(t)
	h.catalog.fails["openai/model-b"] = "HTTP 404 Not Found"
	w := h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-b"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"9"}}, false)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "model-b didn&#39;t pass the test for Ask: HTTP 404 Not Found. Nothing was saved.")
	assert.Contains(t, body, `<option value="openai/model-b" selected>model-b</option>`, "the form keeps what was chosen")
	assert.Contains(t, body, `value="9"`)
	assert.Equal(t, int64(1), h.store.Models().Version, "nothing is saved")

	delete(h.catalog.fails, "openai/model-b")
	w = h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-b"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"9"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "openai/model-b", h.store.Models().Roles["ask"])
}

// TestModelsPageWarnsOfASavedModelNotInUse: a saved model whose entry is
// gone is not used, and the page and its attention list say so.
func TestModelsPageWarnsOfASavedModelNotInUse(t *testing.T) {
	h := newSetupHarness(t)
	_, err := h.store.UpdateModels(context.Background(), 1, settings.Actor{Email: "op@example.com"}, "test", func(models *settings.Models) error {
		models.Roles = map[string]string{"ask": "openai/model-b"}
		return nil
	})
	require.NoError(t, err)
	body := h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `<option value="openai/model-b" selected>model-b</option>`)
	assert.Contains(t, body, "It has not passed this job&#39;s test. Save it for this job again to test it.")
	assert.Contains(t, body, "<li>Ask: It has not passed this job&#39;s test.")
}

// TestModelsPageChoosesTheEmbeddingModel: the first choice saves at once;
// the same choice again changes nothing; a change asks to confirm the
// re-embed, then fills the new model's collection while search keeps the
// first.
func TestModelsPageChoosesTheEmbeddingModel(t *testing.T) {
	h := newSetupHarness(t)
	w := h.post("/admin/settings/models/embedding", url.Values{"embedding": {"openai/embed-a"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "notice=Embedding+model+saved.")
	assert.Equal(t, &settings.Embedding{Provider: "openai", Model: "embed-a", Dimensions: 1536, Collection: "diderot_chunks_embed_a_1536"},
		h.store.Models().Embedding)

	w = h.post("/admin/settings/models/embedding", url.Values{"version": {"2"}, "embedding": {"openai/embed-a"}, "confirm": {"reembed"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "notice=That+is+already+the+embedding+model.")
	assert.Equal(t, int64(2), h.store.Models().Version, "nothing is saved")
	assert.Nil(t, h.store.Models().Filling)

	w = h.post("/admin/settings/models/embedding", url.Values{"version": {"2"}, "embedding": {"openai/embed-b"}}, false)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `name="confirm" value="reembed" required`)
	assert.Contains(t, body, `<option value="openai/embed-b" selected>embed-b</option>`, "the form keeps what was chosen")
	assert.Equal(t, "embed-a", h.store.Models().Embedding.Model, "nothing is saved")

	w = h.post("/admin/settings/models/embedding", url.Values{"version": {"2"}, "embedding": {"openai/embed-b"}, "confirm": {"reembed"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "Search+keeps+using+what+it+has")
	assert.Equal(t, "embed-a", h.store.Models().Embedding.Model, "search keeps the model whose collection is full")
	require.NotNil(t, h.store.Models().Filling)
	assert.Equal(t, "diderot_chunks_embed_b_1536", h.store.Models().Filling.Collection)
	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, "Changing to <code>embed-b</code> from openai.")
	assert.Contains(t, body, "Search keeps using <code>embed-a</code>")
}

// TestModelEntryPage: an entry's page shows what its tests found and saves
// an administrator's correction; an unknown entry is not found.
func TestModelEntryPage(t *testing.T) {
	h := newSetupHarness(t)
	assert.Equal(t, http.StatusNotFound, h.get("/admin/settings/models/model?ref=openai%2Fmodel-a", false).Code)
	w := h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-a"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"8"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)

	body := h.get("/admin/settings/models/model?ref=openai%2Fmodel-a", false).Body.String()
	assert.Contains(t, body, "<th scope=\"row\">Chat requests carry a temperature</th>")
	assert.Contains(t, body, "<td>set when the model was added</td>", "each fact says where it came from")
	assert.Contains(t, body, "what an administrator set on this page, the provider's own list, the published catalog, and last what a job's test found",
		"the page states which source wins")
	assert.Contains(t, body, "<dt>Price</dt><dd>$2 in, $10 out per million tokens <span class=\"sub\">the published catalog</span></dd>")
	assert.Contains(t, body, "<dt>Used by</dt><dd>Ask</dd>")
	assert.NotContains(t, body, "remove-heading", "an entry a job uses can't be removed")

	w = h.post("/admin/settings/models/model", url.Values{"version": {"2"}, "ref": {"openai/model-a"}, "extra_body": {"[1]"}}, false)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Enter a JSON object")

	// Ask uses the model: the page says what the change does to Ask, and
	// saves nothing until that is confirmed.
	correction := url.Values{
		"version": {"2"}, "ref": {"openai/model-a"}, "effort": {"low", "high"}, "omit_temperature": {"true"},
		"allowance": {"25000"}, "extra_body": {`{"verbosity": "low"}`},
	}
	w = h.post("/admin/settings/models/model", correction, false)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	asked := w.Body.String()
	assert.Contains(t, asked, "This changes what Ask sends, from the next request:")
	assert.Contains(t, asked, "<li>Sends a temperature: yes becomes no</li>")
	assert.Contains(t, asked, `name="confirm" value="change" required`)
	assert.Contains(t, asked, `name="omit_temperature" value="true" checked`, "the form keeps what was entered")
	assert.False(t, h.store.Models().Available[0].ChatOmitTemperature, "nothing is saved")
	correction.Set("confirm", "change")
	w = h.post("/admin/settings/models/model", correction, false)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	entry := h.store.Models().Available[0]
	assert.Equal(t, modelregistry.SourceAdmin, entry.SourceOf(modelregistry.FactTemperature))
	assert.Equal(t, []string{"low", "high"}, entry.ReasoningEfforts)
	assert.True(t, entry.ChatOmitTemperature)
	assert.Equal(t, 25000, entry.ChatReasoningTokenAllowance)
	assert.Equal(t, map[string]any{"verbosity": "low"}, entry.ChatExtraBody)
	var action string
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT action FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.entry.save", action)
}

func TestOrganizationPageSaves(t *testing.T) {
	h := newSetupHarness(t)
	assert.NotContains(t, h.get("/admin/settings/organization", false).Body.String(), "Last changed by",
		"a row only a migration has written was changed by nobody")
	w := h.post("/admin/settings/organization", url.Values{"name": {"Example {{ORG_NAME}}"}}, false)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "The name must not contain")
	assert.Contains(t, w.Body.String(), `value="Example {{ORG_NAME}}"`, "the form keeps what was typed")
	assert.Equal(t, "", h.store.OrgName(), "nothing is saved")
	w = h.post("/admin/settings/organization", url.Values{"name": {" Example Corp "}, "description": {"Builds examples"}}, true)
	assert.Equal(t, "/admin/settings/organization?notice=Organization+saved.", w.Header().Get("Location"))
	assert.Equal(t, "Example Corp", h.store.OrgName())

	body := h.get("/admin/settings/organization", false).Body.String()
	assert.Contains(t, body, `value="Example Corp"`)
	assert.Contains(t, body, `name="version" value="2"`)
	assert.Contains(t, body, "Last changed by admin (bootstrap), ", "the change is attributed to who made it")
	assert.NotContains(t, body, "Metrics dashboard", "no dashboard is linked until one is saved")

	w = h.post("/admin/settings/organization", url.Values{
		"version": {"2"}, "name": {"Example Corp"}, "description": {"Builds examples"}, "dashboard": {"https://user@grafana.example.com/d/x"},
	}, true)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "the dashboard address must be an http or https address without a user name or password")
	w = h.post("/admin/settings/organization", url.Values{
		"version": {"2"}, "name": {"Example Corp"}, "description": {"Builds examples"}, "dashboard": {" https://grafana.example.com/d/diderot "},
	}, true)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/settings/organization?notice=Organization+saved.", w.Header().Get("Location"))
	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `<a href="https://grafana.example.com/d/diderot" target="_blank" rel="noreferrer">Metrics dashboard</a>`,
		"every admin page links the saved dashboard")
}

func TestSetupProgress(t *testing.T) {
	checklist := setup.Checklist{Steps: []setup.Step{
		{ID: setup.StepOrganization, Title: "Organization", Required: true, Done: true, Link: organizationPath},
		{ID: setup.StepModels, Title: "Models", Required: true, Link: modelsPath},
		{ID: setup.StepAccess, Title: "Access", Required: true, Link: accessPath},
		{ID: "web-search", Title: "Web search", Link: "/admin/connectors/tavily"},
	}}
	assert.Equal(t, &setupProgress{StepDone: true, Done: 1, Total: 3, NextTitle: "Models", NextHref: modelsPath},
		newSetupProgress(checklist, setup.StepOrganization))
	assert.Equal(t, &setupProgress{Done: 1, Total: 3, NextTitle: "Access", NextHref: accessPath},
		newSetupProgress(checklist, setup.StepModels), "the next step is never this one, and never an optional one")
	checklist.Steps[1].Done, checklist.Steps[2].Done = true, true
	assert.Equal(t, &setupProgress{StepDone: true, Done: 3, Total: 3}, newSetupProgress(checklist, setup.StepAccess),
		"with nothing left, the checklist's Go live is next")
}

// TestAccessPageCountsAskChannels: the page counts the Slack channels where
// Ask is on and links to them instead of listing each.
func TestAccessPageCountsAskChannels(t *testing.T) {
	data := accessData{Summary: setup.AccessSummary{Channels: []setup.AskEnabledChannel{
		{ID: "C1", Name: "platform"}, {ID: "C2", Name: "leadership", Private: true},
		{ID: "C3", Name: "partners", ExternalUsers: true}, {ID: "C4", Name: "private-partners", Private: true, ExternalUsers: true},
	}}}
	assert.Equal(t, 2, data.PrivateChannels())
	assert.Equal(t, 2, data.ExternalChannels())
	var out strings.Builder
	require.NoError(t, accessPage.ExecuteTemplate(&out, "layout", data))
	body := out.String()
	assert.Contains(t, body, `<span class="stat-label">Channels where Ask answers</span><span class="stat-value">4</span>`)
	assert.Contains(t, body, `<a href="/admin/connectors/slack/channels?filter=ask">View these channels</a>`)
	assert.NotContains(t, body, "leadership", "the channels are not listed")
	assert.Contains(t, body, "Mark access as reviewed</button>")

	data.Summary.Acknowledged, data.Summary.AcknowledgedBy = true, "admin@bootstrap.invalid"
	out.Reset()
	require.NoError(t, accessPage.ExecuteTemplate(&out, "layout", data))
	assert.Contains(t, out.String(), "admin (bootstrap) confirmed that these settings are as intended.")
	assert.NotContains(t, out.String(), "Mark access as reviewed", "a review that is done offers no button")
}

// TestModelsPageListsUnavailableJobsOnce: jobs one missing setting turns off
// share a row that says what turns them on.
func TestModelsPageListsUnavailableJobsOnce(t *testing.T) {
	reason := "This needs file storage. Set DIDEROT_IMAGES_BUCKET in the deployment."
	data := modelsData{Models: setup.ModelsView{Roles: []setup.RoleView{
		{ModelRole: setup.ModelRole{Key: "ask", Title: "Ask", Options: []setup.ModelOption{{Ref: "p/m", ID: "m", Provider: "p"}}}, Model: "p/m"},
		{ModelRole: setup.ModelRole{Key: "image_captions", Title: "Image captions", Unavailable: reason}},
		{ModelRole: setup.ModelRole{Key: "document_extraction", Title: "Document extraction", Unavailable: reason}},
		{ModelRole: setup.ModelRole{Key: "titles", Title: "Titles", Unavailable: "No configured provider can serve this."}},
	}}}
	assert.Len(t, data.Available(), 1)
	assert.Equal(t, []unavailableJobs{
		{Titles: []string{"Image captions", "Document extraction"}, Reason: reason},
		{Titles: []string{"Titles"}, Reason: "No configured provider can serve this."},
	}, data.Unavailable())
	var out strings.Builder
	require.NoError(t, modelsPage.ExecuteTemplate(&out, "layout", data))
	assert.Equal(t, 1, strings.Count(out.String(), reason))
	assert.Contains(t, out.String(), "<dt>Image captions<br>Document extraction</dt>")
}
