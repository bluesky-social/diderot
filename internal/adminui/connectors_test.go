package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// fakeConnectors records what the pages send the settings store. Save
// answers with the view it would leave: saved holds record as its last test,
// and enable turns it on when record passed.
type fakeConnectors struct {
	view settings.View
	// list is what List answers; nil lists view alone.
	list     []settings.View
	err      error
	record   settings.TestRecord
	saved    map[string]any
	secrets  map[string]settings.SecretInput
	draft    *settings.Draft
	enable   bool
	disabled bool
	version  int64
}

func (f *fakeConnectors) List(context.Context) ([]settings.View, error) {
	if f.list != nil {
		return f.list, nil
	}
	return []settings.View{f.view}, nil
}

func (f *fakeConnectors) View(_ context.Context, name string) (settings.View, error) {
	if name != f.view.Name {
		return settings.View{}, settings.ErrUnknownConnector
	}
	return f.view, nil
}

func (f *fakeConnectors) Save(_ context.Context, _ settings.Actor, _ string, version int64, values map[string]any, secrets map[string]settings.SecretInput, enable bool) (settings.View, error) {
	f.saved, f.secrets, f.version, f.enable = values, secrets, version, enable
	if f.err != nil {
		return settings.View{}, f.err
	}
	saved := f.view
	saved.Version++
	saved.LastTest = &f.record
	saved.Enabled = saved.Enabled || (enable && f.record.Passed)
	return saved, nil
}

func (f *fakeConnectors) Test(_ context.Context, _ settings.Actor, _ string, draft *settings.Draft) (settings.TestRecord, error) {
	f.draft = draft
	return f.record, f.err
}

func (f *fakeConnectors) Disable(_ context.Context, _ settings.Actor, _ string, version int64) (settings.View, error) {
	f.disabled, f.version = true, version
	return f.view, f.err
}

// namedSource is a registered source with only a name and facts.
type namedSource struct {
	source.Source
	name string
	info source.Info
}

func (s namedSource) Name() string      { return s.name }
func (s namedSource) Info() source.Info { return s.info }

// testSource is a source named name, whose pages call it title.
func testSource(name, title string) namedSource {
	return namedSource{name: name, info: source.Info{
		EventSource: name + "_events", Title: title, ResultNoun: title, Summary: title + " records.", ToolDescription: title + " records.",
		ActorRole: "author", IdentityNamespace: func(string) string { return name + "_user" },
	}}
}

// reportingSource is a source whose page shows status.
type reportingSource struct {
	namedSource
	status settings.Status
}

func (s reportingSource) Status(context.Context, source.Deps) (settings.Status, error) {
	return s.status, nil
}

// namedTool is a registered tool with only a name and a title.
type namedTool struct {
	asktool.Tool
	name, title string
}

func (t namedTool) Name() string { return t.name }
func (t namedTool) Info() asktool.Info {
	return asktool.Info{Title: t.title, Step: t.name, Summary: "Gives Ask " + t.title + "."}
}

// namedAutomation is a registered automation with only a name, a title, and
// what it waits for.
type namedAutomation struct {
	automation.Automation
	name, title, waiting string
}

func (a namedAutomation) Name() string { return a.name }
func (a namedAutomation) Info() automation.Info {
	return automation.Info{Title: a.title, Step: a.name, Summary: "Answers " + a.title + "."}
}
func (a namedAutomation) Status(context.Context) (settings.Status, error) {
	return settings.Status{Waiting: a.waiting}, nil
}

// testRegistries are Slack and the session feed, which reports status, as
// sources, web and code search as tools, and PagerDuty as an automation
// waiting for Slack.
func testRegistries() setup.Registries {
	lastRead := time.Now().Add(-90 * time.Minute)
	feed := reportingSource{namedSource: testSource("session_feed", "Session feed"), status: settings.Status{
		Waiting: "Waiting for the feed.",
		Rows: []settings.StatusRow{
			{Label: "Last read", Value: &lastRead, Detail: "page 3"},
			{Label: "Last summary", Value: (*time.Time)(nil)},
			{Label: "Sessions seen", Value: int64(1234)},
			{Label: "Feed", Value: "example"},
		},
		Note: "Sessions follow the policy file.",
	}}
	return setup.Registries{
		Sources: source.NewRegistry(testSource("slack", "Slack"), feed),
		Tools:   asktool.NewRegistry(namedTool{name: "tavily", title: "Web search"}, namedTool{name: "sourcebot", title: "Code search"}),
		Automations: automation.NewRegistry(namedAutomation{name: "pagerduty", title: "PagerDuty",
			waiting: "Waiting for Slack to be enabled. Alerts wait in the queue until then."}),
	}
}

func exampleConnectorView() settings.View {
	return settings.View{
		Name: "example", Version: 4, UpdatedBy: "operator@example.com",
		Fields: []settings.FieldView{
			{Key: "token", Label: "Token", Kind: settings.KindSecret, Required: true, Env: "DIDEROT_EXAMPLE_TOKEN"},
			{Key: "extra", Label: "Extra", Kind: settings.KindSecret, Env: "DIDEROT_EXAMPLE_EXTRA"},
			{Key: "mode", Label: "Mode", Kind: settings.KindEnum, Options: []string{"one", "two"}},
			{Key: "ids", Label: "IDs", Kind: settings.KindList},
			{Key: "deep", Label: "Deep", Kind: settings.KindBool},
			{Key: "every", Label: "Every", Kind: settings.KindDuration, Required: true},
			{Key: "site", Label: "Site", Kind: settings.KindText, Discovered: true, PromptVar: "EXAMPLE_SITE", Help: "Found by Test."},
		},
		Settings:        settings.Values{"mode": "two", "ids": []string{"A1", "B2"}, "deep": true, "every": "1h30m0s", "site": "old.example"},
		Secrets:         map[string]settings.SecretView{"token": {Saved: true}, "extra": {Saved: true}},
		CanStoreSecrets: true,
	}
}

// connectorTestServer serves the connector pages over store. A connector's
// own page never queries the database; the list does, through pool, which
// may be nil for tests that don't open it.
func connectorTestServer(t *testing.T, store *fakeConnectors, pool ...*pgxpool.Pool) (*http.ServeMux, string) {
	t.Helper()
	db := new(pgxpool.Pool)
	if len(pool) > 0 {
		db = pool[0]
	}
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: db,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Connectors: store,
		Registries: testRegistries(),
		SourceDeps: source.Deps{Pool: db},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			return mux, cookie.Value
		}
	}
	t.Fatal("no CSRF cookie")
	return nil, ""
}

// TestConnectorPageIsGeneratedFromFields: the form has a control per field
// with its saved value, a duration in words, and no discovered field; an
// off connector offers Turn on and no test result before its first test.
func TestConnectorPageIsGeneratedFromFields(t *testing.T) {
	store := &fakeConnectors{view: exampleConnectorView()}
	mux, _ := connectorTestServer(t, store)

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	assert.Contains(t, body, `<input id="secret-token" type="password" name="secret.token" autocomplete="off" placeholder="Saved. Paste a new one to replace it.">`)
	assert.Contains(t, body, `<option value="two" selected>two</option>`)
	assert.Contains(t, body, ">A1\nB2</textarea>")
	assert.Contains(t, body, `name="field.deep" value="true" checked`)
	assert.Contains(t, body, `name="field.every" value="1 hour 30 minutes"`)
	assert.NotContains(t, body, `name="field.site"`, "discovered fields are facts, not settings")
	assert.Contains(t, body, `<span class="badge badge-neutral">off</span>`)
	assert.Contains(t, body, `<button class="button button-primary" type="submit" formaction="/admin/connectors/example/enable">Save and turn on</button>`,
		"never set up, its one primary action saves it and turns it on")
	assert.Contains(t, body, `<button class="button" type="submit">Save</button>`, "and Save alone is secondary")
	assert.NotContains(t, body, ">Turn on</button>")
	assert.NotContains(t, body, "Turn off")
	assert.NotContains(t, body, "Last test", "nothing has been saved and tested yet")
	assert.NotContains(t, body, "status-heading", "an off connector shows no status")

	missing := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/nope", nil, "")
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

// TestConnectorsListSourcesToolsAndAutomations: the list groups sources,
// then tools, then automations, each in setup's order, under their page
// names; a connector of no registered kind lists with the tools. Each row
// says the connector's state and why it needs attention, a source's newest
// item, and invites setting up one never set up.
func TestConnectorsListSourcesToolsAndAutomations(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_connectors"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	externalID := "connectors-list-" + time.Now().Format(time.RFC3339Nano)
	// The page says how long ago the item is by this process's clock, so the
	// item is dated by it too, and to the middle of the hour the test reads:
	// dated by the database's clock at exactly two hours, a database a moment
	// ahead of this process made it "1 hour ago".
	_, err = pool.Exec(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('session_feed_events', $1, $2, 'a session', '{}', 'public')`, externalID, time.Now().Add(-150*time.Minute))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE external_id = $1`, externalID)
	})

	failed := &settings.TestRecord{}
	store := &fakeConnectors{view: exampleConnectorView(), list: []settings.View{
		{Name: "tavily"}, {Name: "example", LastTest: failed}, {Name: "session_feed", Enabled: true, LastTest: failed},
		{Name: "slack", LastTest: failed}, {Name: "pagerduty"},
	}}
	mux, _ := connectorTestServer(t, store, pool)
	response := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors", nil, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	body := response.Body.String()
	var order []string
	for _, match := range regexp.MustCompile(`<h2 id="(\w+)-heading">|href="/admin/connectors/\w+">([^<]+)</a>`).FindAllStringSubmatch(body, -1) {
		order = append(order, match[1]+match[2])
	}
	assert.Equal(t, []string{"sources", "Slack", "Session feed", "tools", "Web search", "Example", "automations", "PagerDuty"}, order)
	assert.Contains(t, body, `<span class="badge badge-attention">needs attention</span><span class="sub">Waiting for the feed.</span>`,
		"what the source reports comes before its failed test")
	assert.Contains(t, body, `>2 hours ago</time>`, "the session feed's newest item")
	assert.Contains(t, body, `<span class="sub">Gives Ask Web search.</span>`)
	assert.Equal(t, 2, strings.Count(body, ">Not set up yet<"), "Web search and PagerDuty were never set up")
	assert.NotContains(t, body, "/admin/sources")
}

// TestAutomationPageSaysWhyItIsIdle: an enabled automation's page says what
// it waits for; a disabled one's doesn't, and neither does a tool's.
func TestAutomationPageSaysWhyItIsIdle(t *testing.T) {
	store := &fakeConnectors{view: exampleConnectorView()}
	mux, _ := connectorTestServer(t, store)
	const idle = "<p>Waiting for Slack to be enabled. Alerts wait in the queue until then.</p>"
	page := func(view settings.View) string {
		t.Helper()
		store.view = view
		response := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/"+view.Name, nil, "")
		require.Equal(t, http.StatusOK, response.Code)
		return response.Body.String()
	}
	body := page(settings.View{Name: "pagerduty", Enabled: true})
	assert.Contains(t, body, idle)
	assert.Contains(t, body, `<span class="badge badge-attention">needs attention</span>`)
	assert.NotContains(t, body, "status-heading", "an automation with nothing but a wait shows no status rows")
	assert.NotContains(t, page(settings.View{Name: "pagerduty", Enabled: true, UpdatedBy: "diderot:registry"}), "Last changed by",
		"a row only the registry has written was changed by nobody")
	assert.Contains(t, page(settings.View{Name: "pagerduty", Enabled: true, UpdatedBy: "operator@example.com"}), "Last changed by operator@example.com,")
	body = page(settings.View{Name: "pagerduty"})
	assert.NotContains(t, body, "Waiting for")
	assert.Contains(t, body, "<p>It is not running.</p>")
	body = page(settings.View{Name: "tavily", Enabled: true})
	assert.NotContains(t, body, "Waiting for")
	assert.Contains(t, body, "<p>Ask can use it.</p>")
}

func TestWebSearchProviderFields(t *testing.T) {
	view := settings.View{
		Name: "tavily", CanStoreSecrets: true,
		Settings: settings.Values{"provider": "tavily", "base_url": "https://proxy.example.com"},
		Fields: []settings.FieldView{
			{Key: "provider", Kind: settings.KindEnum, Options: []string{"tavily", "kagi"}},
			{Key: "kagi_api_key", Kind: settings.KindSecret},
			{Key: "api_key", Kind: settings.KindSecret},
			{Key: "base_url", Kind: settings.KindText},
		},
	}
	store := &fakeConnectors{view: exampleConnectorView()}
	mux, _ := connectorTestServer(t, store)
	store.view = view
	reply := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/tavily", nil, "")
	require.Equal(t, http.StatusOK, reply.Code)
	html := reply.Body.String()
	assert.Regexp(t, `(?s)data-search-provider="kagi"[^>]*>[^<]*.*?href="https://kagi.com/api/keys".*?</fieldset>`, html)
	assert.Regexp(t, `(?s)data-search-provider="tavily"[^>]*>[^<]*.*?href="https://app.tavily.com/".*?</fieldset>`, html)

	values, secrets, _ := draftInput(view, url.Values{
		"field.provider": {"kagi"}, "field.base_url": {"https://other.example.com"},
		"secret.kagi_api_key": {"kagi-example"}, "secret.api_key.remove": {"true"},
	})
	assert.Equal(t, map[string]any{"provider": "kagi"}, values)
	assert.Equal(t, map[string]settings.SecretInput{"kagi_api_key": {Value: "kagi-example"}}, secrets)

	values, secrets, _ = draftInput(view, url.Values{
		"field.provider": {"tavily"}, "field.base_url": {""},
		"secret.kagi_api_key.remove": {"true"}, "secret.api_key": {"tvly-example"},
	})
	assert.Equal(t, map[string]any{"provider": "tavily", "base_url": ""}, values)
	assert.Equal(t, map[string]settings.SecretInput{"api_key": {Value: "tvly-example"}}, secrets)
}

// TestSourcePageShowsItsStatus: while a source is on, its page shows what
// its last test found and its status rows, each value formatted by its
// kind, with the note below.
func TestSourcePageShowsItsStatus(t *testing.T) {
	store := &fakeConnectors{view: exampleConnectorView()}
	mux, _ := connectorTestServer(t, store)
	store.view = settings.View{
		Name: "session_feed", Enabled: true,
		Fields:   []settings.FieldView{{Key: "site", Label: "Site", Kind: settings.KindText, Discovered: true}},
		Settings: settings.Values{"site": "feed.example"},
		LastTest: &settings.TestRecord{Passed: true, TestReport: settings.TestReport{Facts: []settings.Fact{
			{Key: "site", Label: "Site", Value: "feed.example"}, {Key: "who", Label: "Signed in as", Value: "diderot"},
		}}},
	}
	response := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/session_feed", nil, "")
	require.Equal(t, http.StatusOK, response.Code)
	body := response.Body.String()
	assert.Contains(t, body, `<h2 id="status-heading">Status</h2>`)
	assert.Equal(t, 1, strings.Count(body, "<dt>Site</dt><dd>feed.example</dd>"), "a discovered field shows once")
	assert.Contains(t, body, "<dt>Signed in as</dt><dd>diderot</dd>")
	assert.Regexp(t, `<dt>Last read</dt><dd><time [^>]+>1 hour ago</time> <span class="muted">· page 3</span></dd>`, body)
	assert.Contains(t, body, `<dt>Last summary</dt><dd><span class="muted">never</span></dd>`)
	assert.Contains(t, body, "<dt>Sessions seen</dt><dd>1.2K</dd>")
	assert.Contains(t, body, "<dt>Feed</dt><dd>example</dd>")
	assert.Contains(t, body, `<p class="muted">Sessions follow the policy file.</p>`)
	assert.Contains(t, body, "<p>Waiting for the feed.</p>")

	store.view = settings.View{Name: "tavily", Enabled: true}
	response = browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/tavily", nil, "")
	require.Equal(t, http.StatusOK, response.Code)
	assert.NotContains(t, response.Body.String(), "status-heading", "a connector that reports no status shows none")
}

// TestConnectorSecretFieldStates: a secret field shows its deployment
// variable when that sets it, a box to paste into when the install has
// config keys, and otherwise a disabled box that says what to set.
func TestConnectorSecretFieldStates(t *testing.T) {
	render := func(view settings.View) string {
		t.Helper()
		mux, _ := connectorTestServer(t, &fakeConnectors{view: view})
		page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "")
		require.Equal(t, http.StatusOK, page.Code)
		return page.Body.String()
	}

	view := exampleConnectorView()
	view.Secrets["token"] = settings.SecretView{Env: "DIDEROT_EXAMPLE_TOKEN", Saved: true}
	view.CanStoreSecrets = false
	body := render(view)
	assert.Contains(t, body, "<p>Set by the deployment (<code>DIDEROT_EXAMPLE_TOKEN</code>).</p>")
	assert.NotContains(t, body, `name="secret.token"`, "the deployment's value can't be changed here")

	view = exampleConnectorView()
	body = render(view)
	assert.Contains(t, body, `name="secret.token" autocomplete="off" placeholder="Saved. Paste a new one to replace it.">`)
	assert.NotContains(t, body, `name="secret.token.remove"`, "a required secret can't be removed")
	assert.Contains(t, body, `name="secret.extra.remove" value="true"`)
	assert.NotContains(t, body, "DIDEROT_")

	view.Secrets = map[string]settings.SecretView{}
	body = render(view)
	assert.Contains(t, body, `name="secret.token" autocomplete="off">`)
	assert.NotContains(t, body, "Saved.")
	assert.NotContains(t, body, "remove")

	view.CanStoreSecrets = false
	body = render(view)
	assert.Contains(t, body, `<input id="secret-token" type="password" disabled>`)
	assert.Contains(t, body, "To paste it here, set <code>DIDEROT_CONFIG_KEYS</code> in the deployment. Or set <code>DIDEROT_EXAMPLE_TOKEN</code> there instead.")
	assert.NotContains(t, body, `name="secret.token"`)
}

// connectorPoster posts forms to the connector pages: it returns the
// redirect's location, or the body of a page rendered with 200.
func connectorPoster(t *testing.T, mux *http.ServeMux, csrf string) func(string, url.Values) string {
	return func(path string, values url.Values) string {
		t.Helper()
		values.Set("csrf_token", csrf)
		w := browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, values, csrf)
		if w.Code == http.StatusSeeOther {
			return w.Header().Get("Location")
		}
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}
}

// redirectMessage is what a redirect's location tells the page to say,
// and whether as a refusal.
func redirectMessage(t *testing.T, location string) (string, bool) {
	t.Helper()
	parsed, err := url.Parse(location)
	require.NoError(t, err)
	if message := parsed.Query().Get("error"); message != "" {
		return message, true
	}
	return parsed.Query().Get("notice"), false
}

func passingRecord() settings.TestRecord {
	return settings.TestRecord{
		TestReport: settings.TestReport{
			Checks: []settings.Check{{Name: "Token", Passed: true, Detail: "Valid."}},
			Facts:  []settings.Fact{{Key: "site", Label: "Site", Value: "new.example"}},
		},
		Passed: true, By: "operator@example.com", At: time.Now(),
	}
}

func failingRecord() settings.TestRecord {
	return settings.TestRecord{
		TestReport: settings.TestReport{Checks: []settings.Check{
			{Name: "Token", Passed: true, Detail: "Valid."},
			{Name: "Scopes", Passed: false, Detail: "Missing reactions:write.", Remedy: "Add the scope."},
		}},
		By: "operator@example.com", At: time.Now(),
	}
}

// TestConnectorTurnOn: an off connector offers to turn it on, Save, and
// Test. One never set up leads with Save and turn on; once its settings
// have been saved and tested, Save leads and Turn on sits with its state.
// Turning on sends the form to be saved, tested, and turned on; a failed
// test keeps the form saved, leaves it off, and shows the failed checks.
// Save alone saves and tests and leaves it off.
func TestConnectorTurnOn(t *testing.T) {
	store := &fakeConnectors{view: exampleConnectorView(), record: passingRecord()}
	mux, csrf := connectorTestServer(t, store)
	post := connectorPoster(t, mux, csrf)

	const hiddenSave = `<button type="submit" form="connector-settings" hidden tabindex="-1">Save</button>`
	body := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, body, `formaction="/admin/connectors/example/test">Test</button>`)
	assert.Equal(t, 1, strings.Count(body, "button-primary"), "one primary action")
	defaultSave := strings.Index(body, hiddenSave)
	require.GreaterOrEqual(t, defaultSave, 0)
	assert.Less(t, defaultSave, strings.Index(body, ">Save and turn on</button>"),
		"Enter in a field saves: the form's first submit button is a hidden Save, not the one that turns it on")

	saved := exampleConnectorView()
	record := passingRecord()
	saved.LastTest = &record
	savedMux, _ := connectorTestServer(t, &fakeConnectors{view: saved, record: passingRecord()})
	body = browserRequest(t, savedMux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, body, `<button class="button button-primary" type="submit">Save</button>`, "saved before, Save leads")
	assert.Contains(t, body, `formaction="/admin/connectors/example/enable">Turn on</button>`)
	assert.NotContains(t, body, "Save and turn on")
	assert.Less(t, strings.Index(body, hiddenSave), strings.Index(body, ">Turn on</button>"))

	form := url.Values{
		"version": {"4"}, "field.mode": {"one"}, "field.ids": {"A1\nC3"}, "field.every": {"2 hours"}, "field.site": {"forged"},
		"secret.token": {"tok-very-secret"},
	}
	message, refused := redirectMessage(t, post("/admin/connectors/example/enable", form))
	assert.Equal(t, "Example is on.", message)
	assert.False(t, refused)
	assert.True(t, store.enable)
	assert.Equal(t, int64(4), store.version)
	assert.Equal(t, map[string]any{"mode": "one", "ids": "A1\nC3", "deep": "", "every": "2 hours"}, store.saved,
		"an unchecked box is false, and a discovered field is never input")
	assert.Equal(t, map[string]settings.SecretInput{"token": {Value: "tok-very-secret"}}, store.secrets)

	post("/admin/connectors/example/enable", url.Values{"version": {"4"}, "secret.token": {" "}})
	assert.Empty(t, store.secrets, "an empty box keeps the saved value")
	post("/admin/connectors/example/enable", url.Values{"version": {"4"}, "secret.extra.remove": {"true"}})
	assert.Equal(t, map[string]settings.SecretInput{"extra": {Clear: true}}, store.secrets)
	post("/admin/connectors/example/enable", url.Values{"version": {"4"}, "secret.extra": {"extra-new"}, "secret.extra.remove": {"true"}})
	assert.Equal(t, map[string]settings.SecretInput{"extra": {Value: "extra-new"}}, store.secrets, "a pasted value wins over Remove")

	message, _ = redirectMessage(t, post("/admin/connectors/example", form))
	assert.Equal(t, "Saved, and the test passed. Turn Example on when you are ready.", message)
	assert.False(t, store.enable, "Save never turns a connector on")

	store.record = failingRecord()
	body = post("/admin/connectors/example/enable", form)
	assert.Contains(t, body, `role="alert"><p>Example is still off: the test of its settings failed.`)
	assert.Contains(t, body, `<span class="badge badge-neutral">off</span>`)
	assert.Contains(t, body, "Last test,")
	assert.Contains(t, body, "Missing reactions:write.")
	assert.Contains(t, body, "To fix: Add the scope.")
	assert.Contains(t, body, `name="version" value="5"`, "the page carries the version the save wrote")
	assert.NotContains(t, body, "tok-very-secret", "an entered secret is never shown again")
	assert.NotContains(t, body, "Paste it again", "the pasted value was saved")

	store.err = settings.FieldErrors{"token": "is required"}
	w := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/connectors/example/enable",
		url.Values{"version": {"4"}, "csrf_token": {csrf}, "secret.token": {"tok-very-secret"}, "secret.extra.remove": {"true"}}, csrf)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `<p class="error" role="alert">is required</p>`)
	assert.Contains(t, w.Body.String(), "Example is still off: fill in the marked settings.")
	assert.NotContains(t, w.Body.String(), "tok-very-secret")
	assert.Contains(t, w.Body.String(), `name="secret.extra.remove" value="true" checked`, "a rejected form keeps Remove ticked")

	store.err = settings.ErrConflict
	message, refused = redirectMessage(t, post("/admin/connectors/example/enable", form))
	assert.Equal(t, "The settings changed since this page loaded. Check them and try again.", message)
	assert.True(t, refused)
}

// TestConnectorTestSavesNothing: Test sends the form as a draft and shows
// its report under the form, as the form's own.
func TestConnectorTestSavesNothing(t *testing.T) {
	store := &fakeConnectors{view: exampleConnectorView(), record: passingRecord()}
	mux, csrf := connectorTestServer(t, store)
	post := connectorPoster(t, mux, csrf)
	form := url.Values{"version": {"4"}, "field.mode": {"one"}, "secret.token": {"tok-very-secret"}}

	body := post("/admin/connectors/example/test", form)
	require.NotNil(t, store.draft)
	assert.Nil(t, store.saved)
	assert.Equal(t, "one", store.draft.Settings["mode"])
	assert.Equal(t, map[string]settings.SecretInput{"token": {Value: "tok-very-secret"}}, store.draft.Secrets)
	assert.Contains(t, body, `role="status"><p>The test passed. Save to keep these settings.</p>`)
	assert.Contains(t, body, "Test of the settings above,")
	assert.Contains(t, body, "Nothing was saved.")
	assert.Contains(t, body, "<dt>Site</dt><dd>new.example</dd>", "what the draft's test found shows with it")
	assert.Equal(t, 1, strings.Count(body, "Paste it again to save it."), "only the pasted field")
	assert.NotContains(t, body, "tok-very-secret", "an entered secret is never shown again")

	store.record = failingRecord()
	body = post("/admin/connectors/example/test", form)
	assert.Contains(t, body, `role="alert"><p>The test failed. Fix what it reports and test again.</p>`)
}

// TestConnectorSaveWhileEnabled: an enabled connector offers Save, Test,
// and Turn off. A save whose test fails keeps it on, and the page says it
// needs attention until a save passes.
func TestConnectorSaveWhileEnabled(t *testing.T) {
	view := exampleConnectorView()
	view.Enabled = true
	store := &fakeConnectors{view: view, record: passingRecord()}
	mux, csrf := connectorTestServer(t, store)
	post := connectorPoster(t, mux, csrf)

	body := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, body, `<button class="button button-primary" type="submit">Save</button>`)
	assert.Contains(t, body, `<form method="post" action="/admin/connectors/example/disable">`)
	assert.Contains(t, body, `<button class="button button-danger" type="submit">Turn off</button>`)
	assert.NotContains(t, body, "Turn on")

	form := url.Values{"version": {"4"}, "field.mode": {"one"}}
	message, _ := redirectMessage(t, post("/admin/connectors/example", form))
	assert.Equal(t, "Saved, and the test passed.", message)
	assert.False(t, store.enable)

	store.record = failingRecord()
	body = post("/admin/connectors/example", form)
	assert.Contains(t, body, `role="alert"><p>Saved, but the test failed, so Example may not work.`)
	assert.Contains(t, body, `<span class="badge badge-attention">needs attention</span>`, "a failed test doesn't turn it off")
	assert.Contains(t, body, "Missing reactions:write.")

	// Later visits say so too.
	store.view.LastTest = &store.record
	body = browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, body, "Its saved settings failed their last test.")

	message, _ = redirectMessage(t, post("/admin/connectors/example/disable", url.Values{"version": {"5"}}))
	assert.True(t, store.disabled)
	assert.Equal(t, int64(5), store.version)
	assert.Equal(t, "Example is off. Its settings are kept.", message)
}
