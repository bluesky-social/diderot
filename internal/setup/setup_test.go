package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

var operator = Actor{Email: "op@example.com"}

type harness struct {
	pool    *pgxpool.Pool
	store   *settings.Store
	service *Service
	catalog *fakeCatalog
	tests   []settings.RoleTest
	// tested is the saved models the last model test was given.
	tested settings.Models
}

// testRoles are three roles as cmd/diderot builds them: Ask and titles,
// each offered three models of provider p, and captions off.
var testRoles = []ModelRole{
	{Key: "ask", Title: "Ask", Description: "Answers questions.", Options: []ModelOption{
		{Ref: "p/big", ID: "big", Provider: "p"}, {Ref: "p/plain", ID: "plain", Provider: "p"}, {Ref: "p/small", ID: "small", Provider: "p"},
	}},
	{Key: "titles", Title: "Titles", Description: "Writes titles.", Options: []ModelOption{
		{Ref: "p/small", ID: "small", Provider: "p"}, {Ref: "p/big", ID: "big", Provider: "p"},
	}},
	{Key: "image_captions", Title: "Image captions", Description: "Describes images.", Unavailable: "Needs file storage."},
}

// fakeCatalog is the server's catalog over testRoles: a role uses its saved
// model while the model's entry passed the role's test. A test of a ref in
// fails fails with its reason; every other passes and records the role.
type fakeCatalog struct {
	// published is what is published about a model, by ref.
	published map[string]modelregistry.Published
	fails     map[string]string
	// tests are the role and ref of every test, in order; effort and tier
	// are the effort and service tier of the last Ask test.
	tests  []string
	effort string
	tier   string
	// dimensions is what an embedding call answers with, by ref.
	dimensions map[string]int
	// prepared are the collections PrepareCollection was given; missing
	// are those an adoption finds absent.
	prepared []settings.Embedding
	missing  map[string]bool
}

func (c *fakeCatalog) Roles(_ context.Context, saved settings.Models) ([]RoleView, error) {
	var views []RoleView
	for _, role := range testRoles {
		view := RoleView{ModelRole: role, Saved: saved.Roles[role.Key]}
		if role.Unavailable == "" && view.Saved != "" {
			entry, ok := entryOf(saved.Available, view.Saved)
			if ok && slices.Contains(entry.TestedFor, modelregistry.Role(role.Key)) {
				view.Model = view.Saved
			} else {
				view.Warning = "It has not passed this job's test."
			}
		}
		views = append(views, view)
	}
	return views, nil
}

func (c *fakeCatalog) TestRole(_ context.Context, saved settings.Models, role, ref, effort string) (modelregistry.Model, error) {
	c.tests = append(c.tests, role+" "+ref)
	if role == "ask" {
		c.effort, c.tier = effort, saved.AskServiceTier
	}
	if reason, fails := c.fails[ref]; fails {
		return modelregistry.Model{}, &modelregistry.TestError{Reason: reason}
	}
	entry, ok := entryOf(saved.Available, ref)
	if !ok {
		provider, id, _ := modelregistry.ParseRef(ref)
		entry = modelregistry.Model{Provider: provider, ID: id}
	}
	entry.TestedFor = append(slices.Clone(entry.TestedFor), modelregistry.Role(role))
	if effort != "" {
		entry.ReasoningEfforts = append(slices.Clone(entry.ReasoningEfforts), effort)
	}
	return entry, nil
}

// FactsStatus, Published and Detail: the fake knows what its facts field says is
// published, and nothing else.
func (c *fakeCatalog) FactsStatus(_ context.Context, saved settings.Models) (FactsStatus, error) {
	return FactsStatus{Off: saved.PublishedFactsOff}, nil
}

func (c *fakeCatalog) Published(_ context.Context, _ settings.Models, ref string, pin *string) (modelregistry.Published, error) {
	if pin != nil && *pin == "nowhere/none" {
		return modelregistry.Published{}, fmt.Errorf("%w: the published catalog has no entry %s", settings.ErrInvalid, *pin)
	}
	return c.published[ref], nil
}

func (c *fakeCatalog) Detail(_ context.Context, saved settings.Models, ref string) (ModelDetail, error) {
	detail := ModelDetail{}
	for _, entry := range saved.Available {
		if entry.Ref() == ref {
			detail.Match.Pin = entry.Catalog
			detail.Differs = !entry.SameRequests(entry.WithPublished(c.published[ref], true))
		}
	}
	return detail, nil
}

// CanServe refuses the roles in cannot, and an image job on an entry that
// takes none.
func (c *fakeCatalog) CanServe(_ settings.Models, role string, entry modelregistry.Model) error {
	if role == "titles" && !entry.Images {
		return errors.New("the job sends images, which the model would no longer accept")
	}
	return nil
}

func (c *fakeCatalog) Embedding(context.Context, settings.Models) ([]ModelOption, string, string, error) {
	return []ModelOption{{Ref: "p/embed", ID: "embed", Provider: "p"}, {Ref: "p/embed-2", ID: "embed-2", Provider: "p"}}, "", "", nil
}

func (c *fakeCatalog) EmbeddingDimensions(_ context.Context, _ settings.Models, ref string) (int, error) {
	if reason, fails := c.fails[ref]; fails {
		return 0, &modelregistry.TestError{Reason: reason}
	}
	return c.dimensions[ref], nil
}

func (c *fakeCatalog) PrepareCollection(_ context.Context, embedding settings.Embedding, adopt bool) error {
	if adopt && c.missing[embedding.Collection] {
		return fmt.Errorf("%w: Qdrant has no collection named %s", settings.ErrInvalid, embedding.Collection)
	}
	c.prepared = append(c.prepared, embedding)
	return nil
}

// passing is a passing test of the models view uses.
func passing(view ModelsView) []settings.RoleTest {
	var results []settings.RoleTest
	for _, role := range view.Roles {
		if role.Model != "" {
			results = append(results, settings.RoleTest{Role: role.Title, Model: role.Model, Passed: true})
		}
	}
	return append(results, settings.RoleTest{Role: "Embeddings", Model: "embedder", Passed: true})
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

// namedTool is a registered tool with only a name and its facts.
type namedTool struct {
	asktool.Tool
	name, title, step, summary string
}

func (t namedTool) Name() string { return t.name }
func (t namedTool) Info() asktool.Info {
	return asktool.Info{Title: t.title, Step: t.step, Summary: t.summary}
}

// namedAutomation is a registered automation with only a name and its facts.
type namedAutomation struct {
	automation.Automation
	name, title, step, summary string
}

func (a namedAutomation) Name() string { return a.name }
func (a namedAutomation) Info() automation.Info {
	return automation.Info{Title: a.title, Step: a.step, Summary: a.summary}
}

// fakeConnector is a registered connector whose test always passes.
type fakeConnector string

func (f fakeConnector) Name() string                               { return string(f) }
func (fakeConnector) Fields() []settings.Field                     { return nil }
func (fakeConnector) Validate(settings.Values) error               { return nil }
func (fakeConnector) Run(context.Context, settings.Resolved) error { return nil }
func (fakeConnector) Test(context.Context, settings.Resolved) settings.TestReport {
	return settings.TestReport{Checks: []settings.Check{{Name: "ok", Passed: true}}}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "setup"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		TRUNCATE admin_audit_log, events, scheduled_reports, slack_channel_config, slack_channel_info,
		         prompt_release_activations, prompt_active_release, prompt_release_entries, prompt_releases
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE connector_settings
		   SET settings = '{"ORG_NAME":"","ORG_DESCRIPTION":"","state":"setup","bootstrap_admin_enabled":true}',
		       version = 1
		 WHERE connector = 'org'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE connector <> 'org'`)
	require.NoError(t, err)
	// PagerDuty and Zendesk aren't registered, as in a build without them.
	registry := settings.NewRegistry(fakeConnector("slack"), fakeConnector("notion"), fakeConnector("session_feed"),
		fakeConnector("tavily"), fakeConnector("sourcebot"))
	h := &harness{pool: pool, store: settings.NewStore(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithConnectors(registry, settings.Secrets{}).WithFamily(askconn.Definitions{}.Family())}
	require.NoError(t, h.store.Load(ctx))
	h.tests = []settings.RoleTest{{Role: "Ask", Model: "big", Passed: true}}
	registries := Registries{
		Sources: source.NewRegistry(testSource("slack", "Slack"), testSource("notion", "Notion"), testSource("session_feed", "Session feed")),
		Tools: asktool.NewRegistry(
			namedTool{name: "tavily", title: "Web search", step: "web-search", summary: "Lets Ask search the web."},
			namedTool{name: "sourcebot", title: "Code search", step: "code-search", summary: "Lets Ask search and read your code."}),
		Automations: automation.NewRegistry(namedAutomation{name: "pages", title: "Pages", step: "pages", summary: "Answers pages."}),
	}
	h.catalog = &fakeCatalog{fails: map[string]string{}, dimensions: map[string]int{"p/embed": 1024, "p/embed-2": 768}, missing: map[string]bool{}}
	h.service = New(pool, h.store, registries, Facts{
		Access: Access{AuthMode: "static", AdminGroups: []string{"diderot-admins"}},
	}, h.catalog, func(ctx context.Context, saved settings.Models, roles ...string) []settings.RoleTest {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "model tests run under a deadline")
		require.LessOrEqual(t, time.Until(deadline), ModelTestTimeout)
		require.Empty(t, roles, "Test models tests every role")
		h.tested = saved
		return h.tests
	})
	return h
}

func (h *harness) models(t *testing.T) ModelsView {
	t.Helper()
	view, err := h.service.Models(context.Background())
	require.NoError(t, err)
	return view
}

func (h *harness) saveModels(t *testing.T, update ModelsUpdate) error {
	t.Helper()
	_, err := h.service.SaveModels(context.Background(), operator, h.models(t).Version, update)
	return err
}

func (h *harness) seedPrompts(t *testing.T, createdBy string) {
	t.Helper()
	ctx := context.Background()
	var id int64
	require.NoError(t, h.pool.QueryRow(ctx, `
		INSERT INTO prompt_releases (source_revision, bundle_sha256, schema_version, created_by)
		VALUES ($1, repeat('a', 64), 1, $2) RETURNING id`, "rev-"+createdBy, createdBy).Scan(&id))
	_, err := h.pool.Exec(ctx, `
		INSERT INTO prompt_active_release (singleton, release_id, activated_by) VALUES (true, $1, $2)
		ON CONFLICT (singleton) DO UPDATE SET release_id = EXCLUDED.release_id`, id, createdBy)
	require.NoError(t, err)
}

// setConnector turns a registered connector on or off.
func (h *harness) setConnector(t *testing.T, name string, on bool) {
	t.Helper()
	ctx := context.Background()
	view, err := h.store.View(ctx, name)
	require.NoError(t, err)
	actor := settings.Actor{Email: operator.Email}
	if on {
		view, err = h.store.Save(ctx, actor, name, view.Version, nil, nil, true)
	} else {
		view, err = h.store.Disable(ctx, actor, name, view.Version)
	}
	require.NoError(t, err)
	require.Equal(t, on, view.Enabled)
}

func (h *harness) checklist(t *testing.T) Checklist {
	t.Helper()
	c, err := h.service.Checklist(context.Background())
	require.NoError(t, err)
	return c
}

func (h *harness) do(t *testing.T, actor Actor, action string) (settings.OrgRow, error) {
	t.Helper()
	return h.service.Do(context.Background(), actor, h.checklist(t).Version, action)
}

func TestFreshChecklistIsNotReady(t *testing.T) {
	h := newHarness(t)
	h.seedPrompts(t, SeedPromptAuthor)
	c := h.checklist(t)
	assert.Equal(t, settings.StateSetup, c.State)
	assert.True(t, c.BootstrapAdminEnabled)
	assert.False(t, c.Ready)
	var required, optional []string
	for _, step := range c.Steps {
		if step.Required {
			required = append(required, step.ID)
			assert.False(t, step.Done, step.ID)
		} else {
			optional = append(optional, step.ID)
		}
		assert.NotEmpty(t, step.Status, step.ID)
		assert.NotEmpty(t, step.Link, step.ID)
	}
	assert.Equal(t, []string{StepOrganization, StepModels, StepSources, StepPrompts, StepAccess, StepBootstrapAdmin}, required)
	assert.Equal(t, []string{"web-search", "code-search", StepAskMCPTools, StepScheduledReports, "pages"}, optional,
		"each registered tool's step, then MCP and reports, then each registered automation's")
	assert.Equal(t, "No source is on. Set one up on the Connectors page.", c.Step(StepSources).Status)
}

// TestConnectorStepsFollowConnectors: the Sources step is done once a
// source connector is on, and each optional tool step shows its connector.
func TestConnectorStepsFollowConnectors(t *testing.T) {
	h := newHarness(t)
	h.seedPrompts(t, SeedPromptAuthor)
	c := h.checklist(t)
	web := c.Step("web-search")
	assert.Equal(t, Step{
		ID: "web-search", Title: "Web search", Summary: "Lets Ask search the web.", Status: "Off.", Link: "/admin/connectors/tavily",
	}, web)
	assert.Equal(t, "/admin/connectors/sourcebot", c.Step("code-search").Link)
	assert.Equal(t, "Code search", c.Step("code-search").Title)
	assert.Equal(t, Step{ID: "pages", Title: "Pages", Summary: "Answers pages.", Status: "Off.", Link: "/admin/connectors/pages"}, c.Step("pages"))

	h.setConnector(t, "tavily", true)
	c = h.checklist(t)
	assert.True(t, c.Step("web-search").Done)
	assert.False(t, c.Step(StepSources).Done, "a tool is not a source")

	h.setConnector(t, "session_feed", true)
	h.setConnector(t, "slack", true)
	c = h.checklist(t)
	assert.True(t, c.Step(StepSources).Done)
	assert.Equal(t, "On: Slack, Session feed.", c.Step(StepSources).Status)

	h.setConnector(t, "slack", false)
	h.setConnector(t, "session_feed", false)
	assert.False(t, h.checklist(t).Step(StepSources).Done)
}

// fixedSummaries is a fixed agent-session summary state.
type fixedSummaries sessioningest.SummaryControlState

func (f fixedSummaries) State(context.Context) (sessioningest.SummaryControlState, error) {
	return sessioningest.SummaryControlState(f), nil
}

// TestSessionPushCountsAsASource: agent-session push completes the Sources
// step while someone can push and pushed sessions are summarized; otherwise
// the step says why it doesn't count.
func TestSessionPushCountsAsASource(t *testing.T) {
	h := newHarness(t)
	h.seedPrompts(t, SeedPromptAuthor)
	paused := fixedSummaries{Blocked: sessioningest.SummaryPaused, Reason: "Session summaries are paused: resume them on the Jobs and maintenance page, under Operations."}

	h.service.facts.SessionPush = &SessionPush{Summaries: fixedSummaries{EffectiveEnabled: true}}
	step := h.checklist(t).Step(StepSources)
	assert.True(t, step.Done)
	assert.Equal(t, "On: Agent-session push.", step.Status)

	h.service.facts.SessionPush = &SessionPush{Summaries: paused}
	step = h.checklist(t).Step(StepSources)
	assert.False(t, step.Done, "pushed sessions that are never summarized are never searchable")
	assert.Equal(t, "Pushed agent sessions aren't searchable yet. Session summaries are paused: resume them on the Jobs and maintenance page, under Operations.", step.Status)

	h.setConnector(t, "slack", true)
	step = h.checklist(t).Step(StepSources)
	assert.True(t, step.Done)
	assert.Equal(t, "On: Slack. Pushed agent sessions aren't searchable yet. Session summaries are paused: resume them on the Jobs and maintenance page, under Operations.", step.Status)
	h.setConnector(t, "slack", false)

	h.service.facts.SessionPush = &SessionPush{NoPusher: "Nobody can push agent sessions.", Summaries: fixedSummaries{EffectiveEnabled: true}}
	step = h.checklist(t).Step(StepSources)
	assert.False(t, step.Done)
	assert.Equal(t, "Nobody can push agent sessions.", step.Status)
}

// TestGoLiveNeedsEveryRequiredStep walks the checklist to ready, refusing Go
// live until the last required step is done.
func TestGoLiveNeedsEveryRequiredStep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPrompts(t, SeedPromptAuthor)
	goLive := func() error {
		_, err := h.do(t, operator, ActionGoLive)
		return err
	}
	require.ErrorIs(t, goLive(), ErrNotReady)

	_, err := h.service.SaveOrg(ctx, operator, h.checklist(t).Version, " Example Corp ", "Builds examples", "")
	require.NoError(t, err)
	assert.Equal(t, "Example Corp", h.store.OrgName())
	assert.True(t, h.checklist(t).Step(StepOrganization).Done)

	h.tests = passing(h.models(t))
	h.tests[len(h.tests)-1] = settings.RoleTest{Role: "Embeddings", Passed: false, Error: "HTTP 401"}
	_, err = h.do(t, operator, ActionTestModels)
	require.NoError(t, err)
	assert.False(t, h.checklist(t).Step(StepModels).Done, "a failing role fails the step")
	assert.Equal(t, "The last test failed: Embeddings.", h.checklist(t).Step(StepModels).Status)
	h.tests = passing(h.models(t))
	_, err = h.do(t, operator, ActionTestModels)
	require.NoError(t, err)
	models := h.checklist(t).Step(StepModels)
	assert.True(t, models.Done)
	assert.Equal(t, "The last test passed.", models.Status, "who and when are fields, not part of the sentence")
	assert.Equal(t, operator.Email, models.By)
	require.NotNil(t, models.At)
	assert.WithinDuration(t, time.Now(), *models.At, time.Minute)

	h.setConnector(t, "notion", true)
	assert.True(t, h.checklist(t).Step(StepSources).Done)

	_, err = h.do(t, operator, ActionAcceptExamplePrompts)
	require.NoError(t, err)
	assert.True(t, h.checklist(t).Step(StepPrompts).Done)
	assert.Equal(t, operator.Email, h.checklist(t).Step(StepPrompts).By)

	_, err = h.do(t, operator, ActionAcknowledgeAccess)
	require.NoError(t, err)
	access := h.checklist(t).Step(StepAccess)
	assert.True(t, access.Done)
	assert.Equal(t, "Reviewed.", access.Status)
	assert.Equal(t, operator.Email, access.By)
	connections := h.checklist(t).Step(StepAskMCPTools)
	assert.Equal(t, "None on.", connections.Status)
	assert.False(t, connections.Done)
	_, err = h.pool.Exec(ctx, `
		INSERT INTO connector_settings (connector, enabled, settings, secrets, version, updated_by, updated_at)
		VALUES ($1, true, '{"kind":"grafana"}', '{}', 1, 'op@example.com', now())`, askconn.Row("grafana"))
	require.NoError(t, err)
	connections = h.checklist(t).Step(StepAskMCPTools)
	assert.True(t, connections.Done)
	assert.Equal(t, "On: grafana.", connections.Status)
	assert.Equal(t, ConnectionsPath, connections.Link)
	for _, step := range h.checklist(t).Steps {
		assert.NotEmpty(t, step.Summary, step.ID)
	}
	require.ErrorIs(t, goLive(), ErrNotReady, "the bootstrap admin is still on")

	_, err = h.do(t, operator, ActionDisableBootstrapAdmin)
	require.NoError(t, err)
	c := h.checklist(t)
	assert.True(t, c.Ready)
	assert.False(t, h.store.AskAllowed(false))

	row, err := h.service.Do(ctx, operator, c.Version, ActionGoLive)
	require.NoError(t, err)
	assert.Equal(t, settings.StateLive, row.State)
	assert.Equal(t, "op@example.com", row.WentLiveBy)
	assert.NotNil(t, row.WentLiveAt)
	assert.True(t, h.store.AskAllowed(false), "going live opens Ask on this replica at once")

	var audited []string
	rows, err := h.pool.Query(ctx, `SELECT action FROM admin_audit_log ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var action string
		require.NoError(t, rows.Scan(&action))
		audited = append(audited, action)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"setup.organization.save", "setup.test_models", "setup.test_models", "connectors.notion.enable", "setup.accept_example_prompts",
		"setup.acknowledge_access", "setup.disable_bootstrap_admin", "setup.go_live",
	}, audited)

	// An incomplete step later takes nothing offline.
	h.setConnector(t, "notion", false)
	c = h.checklist(t)
	assert.Equal(t, settings.StateLive, c.State)
	assert.False(t, c.Ready)
	assert.True(t, h.store.AskAllowed(false))

	row, err = h.do(t, operator, ActionBackToSetup)
	require.NoError(t, err)
	assert.Equal(t, settings.StateSetup, row.State)
	assert.Empty(t, row.WentLiveBy)
	assert.False(t, h.store.AskAllowed(false))
}

func TestBootstrapAdminCannotTurnItselfOff(t *testing.T) {
	h := newHarness(t)
	_, err := h.do(t, Actor{Email: "admin@bootstrap.invalid", Bootstrap: true}, ActionDisableBootstrapAdmin)
	require.ErrorIs(t, err, ErrBootstrapSession)
	assert.True(t, h.checklist(t).BootstrapAdminEnabled)

	_, err = h.do(t, operator, ActionDisableBootstrapAdmin)
	require.NoError(t, err)
	enabled, err := h.store.BootstrapAdminEnabled(context.Background())
	require.NoError(t, err)
	assert.False(t, enabled)

	row, err := ResetBootstrapAdmin(context.Background(), h.store, "root@server")
	require.NoError(t, err)
	assert.True(t, row.BootstrapAdminEnabled)
	var actor, action string
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT actor_email, action FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&actor, &action))
	assert.Equal(t, "root@server", actor)
	assert.Equal(t, "setup.bootstrap_admin.reset", action)
}

func TestPromptsStepFollowsActiveRelease(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, "No prompts are active.", h.checklist(t).Step(StepPrompts).Status)
	h.seedPrompts(t, SeedPromptAuthor)
	assert.False(t, h.checklist(t).Step(StepPrompts).Done)
	h.seedPrompts(t, "op@example.com")
	assert.True(t, h.checklist(t).Step(StepPrompts).Done, "an edited release completes the step")
}

func TestActionsCheckVersionAndInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, err := h.service.Do(ctx, operator, 99, ActionAcknowledgeAccess)
	require.ErrorIs(t, err, settings.ErrConflict)
	_, err = h.service.Do(ctx, operator, 99, ActionTestModels)
	require.ErrorIs(t, err, settings.ErrConflict, "a stale form fails before any model is called")
	_, err = h.service.Do(ctx, operator, 1, "reboot")
	require.ErrorIs(t, err, ErrUnknownAction)
	_, err = h.service.SaveOrg(ctx, operator, 1, "{{ORG_NAME}}", "", "")
	require.ErrorIs(t, err, settings.ErrInvalid)
	_, err = h.service.SaveOrg(ctx, operator, 1, "Example", "two\nlines", "")
	require.ErrorIs(t, err, settings.ErrInvalid)
}

// TestModelsAreOffUntilChosen: with nothing saved no role has a model and no
// embedding model is chosen; Ask's limits are the code's, and the checklist
// carries the page.
func TestModelsAreOffUntilChosen(t *testing.T) {
	h := newHarness(t)
	view := h.models(t)
	assert.Equal(t, int64(1), view.Version)
	for _, role := range view.Roles {
		assert.Empty(t, role.Model, role.Key)
		assert.Empty(t, role.Warning, role.Key)
	}
	assert.Equal(t, "Needs file storage.", view.Role("image_captions").Unavailable)
	assert.Nil(t, view.Embedding)
	assert.Len(t, view.EmbeddingOptions, 2)
	assert.Empty(t, view.AskReasoningEffort, "the model's default")
	assert.Equal(t, settings.DefaultAskMaxToolTurns, view.AskMaxToolTurns)
	assert.Equal(t, view, h.checklist(t).Models)
}

// TestSaveModelsTestsEachNewChoice: a model newly given to a role passes the
// role's test before anything is saved, and its entry keeps what each test
// found; a failure, an offer no provider makes, or a role that is off saves
// nothing.
func TestSaveModelsTestsEachNewChoice(t *testing.T) {
	h := newHarness(t)
	effort := func(value string) *string { return &value }
	turns := func(value int) *int { return &value }
	for _, tc := range []struct {
		name   string
		update ModelsUpdate
		want   string
	}{
		{"model no provider offers", ModelsUpdate{Roles: map[string]string{"ask": "p/gpt-9"}}, `no enabled provider offers "p/gpt-9" for Ask`},
		{"role that is off", ModelsUpdate{Roles: map[string]string{"image_captions": "p/big"}}, "Image captions is off: Needs file storage."},
		{"no tool turns", ModelsUpdate{AskMaxToolTurns: turns(0)}, "Maximum tool turns must be a number from 1 to 50"},
		{"unknown effort", ModelsUpdate{AskReasoningEffort: effort("ultra")}, "Reasoning effort must be one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := h.saveModels(t, tc.update)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.ErrorContains(t, err, tc.want)
			assert.Equal(t, int64(1), h.models(t).Version, "nothing is saved")
		})
	}
	assert.Empty(t, h.catalog.tests, "a refused form calls no model")

	h.catalog.fails["p/plain"] = "HTTP 400 Bad Request"
	err := h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/plain", "titles": "p/small"}})
	var failed *ModelTestError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, "plain didn't pass the test for Ask: HTTP 400 Bad Request. Nothing was saved.", err.Error())
	assert.Equal(t, int64(1), h.models(t).Version, "not even the role whose test passed")

	h.catalog.tests = nil
	require.NoError(t, h.saveModels(t, ModelsUpdate{
		Roles: map[string]string{"ask": "p/big", "titles": "p/big"}, AskReasoningEffort: effort("high"), AskMaxToolTurns: turns(16),
	}))
	assert.Equal(t, []string{"ask p/big", "titles p/big"}, h.catalog.tests)
	assert.Equal(t, "high", h.catalog.effort, "Ask is tested with the effort the save gives it")
	view := h.models(t)
	assert.Equal(t, "p/big", view.Role("ask").Model)
	assert.Equal(t, "p/big", view.Role("titles").Model)
	assert.Equal(t, "high", view.AskReasoningEffort)
	assert.Equal(t, 16, view.AskMaxToolTurns)
	require.Len(t, view.Available, 1, "one entry per model, whatever its jobs")
	assert.Equal(t, []modelregistry.Role{"ask", "titles"}, view.Available[0].TestedFor)

	// Unchanged choices call no model; limits alone call none either.
	h.catalog.tests = nil
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/big"}, AskMaxToolTurns: turns(12)}))
	assert.Empty(t, h.catalog.tests)
	// A new effort tests Ask again.
	require.NoError(t, h.saveModels(t, ModelsUpdate{AskReasoningEffort: effort("low")}))
	assert.Equal(t, []string{"ask p/big"}, h.catalog.tests)
	assert.Equal(t, "low", h.catalog.effort)
	// An empty choice turns a role off and keeps the model's entry.
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"titles": ""}}))
	view = h.models(t)
	assert.Empty(t, view.Role("titles").Model)
	assert.Equal(t, "p/big", view.Role("ask").Model, "a role left out keeps its choice")
	assert.Len(t, view.Available, 1)

	_, err = h.service.SaveModels(context.Background(), operator, 1, ModelsUpdate{})
	require.ErrorIs(t, err, settings.ErrConflict)
	var action string
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT action FROM admin_audit_log WHERE target = 'connector_settings:models' ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.save", action)
}

// TestModelEntriesAreCorrectedAndRemoved: an administrator's correction is
// checked against the provider's kind and saved; an entry a job uses can't
// be removed.
func TestModelEntriesAreCorrectedAndRemoved(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/big"}}))
	ctx := context.Background()
	view := h.models(t)
	_, err := h.service.UpdateModelEntry(ctx, operator, view.Version, "p/big", modelregistry.KindOpenAI,
		ModelEntryUpdate{ReasoningEfforts: []string{"turbo"}}, true)
	require.ErrorIs(t, err, settings.ErrInvalid)
	correction := ModelEntryUpdate{
		ReasoningEfforts: []string{"low", "high"}, Images: true, ChatMaxCompletionTokens: true, ChatReasoningTokenAllowance: 25000,
		ChatOmitTemperature: true, ChatExtraBody: map[string]any{"verbosity": "low"},
	}
	// Ask uses the model: a change to what it sends is named, and waits
	// for a confirmation.
	_, err = h.service.UpdateModelEntry(ctx, operator, view.Version, "p/big", modelregistry.KindOpenAI, correction, false)
	var unconfirmed *EntryChangeError
	require.ErrorAs(t, err, &unconfirmed)
	assert.Equal(t, []string{"Ask"}, unconfirmed.Jobs)
	assert.Empty(t, unconfirmed.Stops)
	var texts []string
	for _, change := range unconfirmed.Changes {
		texts = append(texts, change.Text)
	}
	assert.Contains(t, texts, "Accepts images: no becomes yes")
	assert.Contains(t, texts, `Extra request fields: none becomes {"verbosity":"low"}`)
	assert.Empty(t, h.models(t).Available[0].ChatExtraBody, "nothing is saved until it is confirmed")
	_, err = h.service.UpdateModelEntry(ctx, operator, view.Version, "p/big", modelregistry.KindOpenAI, correction, true)
	require.NoError(t, err)
	entry := h.models(t).Available[0]
	assert.Equal(t, modelregistry.SourceAdmin, entry.SourceOf(modelregistry.FactExtraBody), "what an administrator set is theirs")
	assert.Equal(t, modelregistry.SourceAdmin, entry.SourceOf(modelregistry.FactImages))
	assert.Empty(t, entry.SourceOf(modelregistry.FactServiceTier), "what they left alone is not")
	// Saving the same again changes nothing, so asks nothing.
	_, err = h.service.UpdateModelEntry(ctx, operator, h.models(t).Version, "p/big", modelregistry.KindOpenAI, correction, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"low", "high"}, entry.ReasoningEfforts)
	assert.Equal(t, "max_completion_tokens", string(entry.ChatTokenParam))
	assert.True(t, entry.ChatOmitTemperature)
	assert.Equal(t, map[string]any{"verbosity": "low"}, entry.ChatExtraBody)
	assert.Equal(t, []modelregistry.Role{"ask"}, entry.TestedFor, "what the tests found stays")

	_, err = h.service.RemoveModelEntry(ctx, operator, h.models(t).Version, "p/big")
	require.ErrorIs(t, err, settings.ErrInvalid)
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": ""}}))
	_, err = h.service.RemoveModelEntry(ctx, operator, h.models(t).Version, "p/big")
	require.NoError(t, err)
	assert.Empty(t, h.models(t).Available)
}

// TestAnEntryChangeThatWouldStopAJobIsRefused: unticking what a job in use
// needs is refused, confirmed or not, and names the job.
func TestAnEntryChangeThatWouldStopAJobIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"titles": "p/big"}}))
	// The fake catalog's titles need images, as captions do.
	_, err := h.service.UpdateModelEntry(ctx, operator, h.models(t).Version, "p/big", modelregistry.KindOpenAI, ModelEntryUpdate{Images: true}, true)
	require.NoError(t, err)
	_, err = h.service.UpdateModelEntry(ctx, operator, h.models(t).Version, "p/big", modelregistry.KindOpenAI, ModelEntryUpdate{Images: false}, true)
	var refused *EntryChangeError
	require.ErrorAs(t, err, &refused)
	require.Len(t, refused.Stops, 1)
	assert.Contains(t, refused.Stops[0], "Titles uses this model and would stop")
	assert.Contains(t, refused.Error(), "Give Titles another model")
	assert.True(t, h.models(t).Available[0].Images, "nothing is saved")

	// With the job given another model, the same change is the
	// administrator's to make, with nothing to confirm.
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"titles": ""}}))
	_, err = h.service.UpdateModelEntry(ctx, operator, h.models(t).Version, "p/big", modelregistry.KindOpenAI, ModelEntryUpdate{Images: false}, false)
	require.NoError(t, err)
}

// TestChooseEmbedding: the first choice of an empty corpus learns the
// dimension, names its collection after the model, and is used at once.
// Choosing the model in use again changes nothing. Another model is saved
// as the one being filled, once confirmed, while search keeps the first.
func TestChooseEmbedding(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	choose := func(choice EmbeddingChoice) (settings.ModelsRow, EmbeddingOutcome, error) {
		return h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, choice)
	}

	_, _, err := choose(EmbeddingChoice{Ref: "p/none"})
	require.ErrorIs(t, err, settings.ErrInvalid)
	h.catalog.fails["p/embed"] = "HTTP 404 Not Found"
	_, _, err = choose(EmbeddingChoice{Ref: "p/embed"})
	var failed *ModelTestError
	require.ErrorAs(t, err, &failed)
	delete(h.catalog.fails, "p/embed")
	assert.Nil(t, h.models(t).Embedding, "nothing is saved")

	row, outcome, err := choose(EmbeddingChoice{Ref: "p/embed"})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingInUse, outcome)
	first := settings.Embedding{Provider: "p", Model: "embed", Dimensions: 1024, Collection: "diderot_chunks_embed_1024"}
	assert.Equal(t, &first, row.Embedding, "a fresh install embeds nothing again")
	assert.Equal(t, []settings.Embedding{first}, h.catalog.prepared, "its collection is created")

	// The same model again: nothing happens, not even a call to it.
	h.catalog.fails["p/embed"] = "would have been called"
	version := row.Version
	row, outcome, err = choose(EmbeddingChoice{Ref: "p/embed"})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingUnchanged, outcome)
	assert.Equal(t, version, row.Version)
	assert.Len(t, h.catalog.prepared, 1)
	delete(h.catalog.fails, "p/embed")

	_, _, err = choose(EmbeddingChoice{Ref: "p/embed-2"})
	require.ErrorIs(t, err, ErrReembedUnconfirmed)
	row, outcome, err = choose(EmbeddingChoice{Ref: "p/embed-2", Reembed: true})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingFilling, outcome)
	assert.Equal(t, &first, row.Embedding, "search keeps the model it has")
	assert.Equal(t, &settings.Embedding{Provider: "p", Model: "embed-2", Dimensions: 768, Collection: "diderot_chunks_embed_2_768"}, row.Filling)
	view := h.models(t)
	require.NotNil(t, view.Filling)
	assert.Zero(t, view.FillingDone)

	// Part way through, the change can't be undone by choosing the old
	// model, and choosing the new one again changes nothing.
	_, _, err = choose(EmbeddingChoice{Ref: "p/embed"})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "can't be undone part way")
	_, outcome, err = choose(EmbeddingChoice{Ref: "p/embed-2"})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingUnchanged, outcome)

	var action string
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT action FROM admin_audit_log WHERE target = 'connector_settings:models' ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.embedding", action)
}

// TestChoosingTheModelOfAnAdoptedCollectionAgainKeepsIt is the bug that
// emptied search: an adopted collection does not have the name Diderot
// would give the model's own, and choosing the same model again must not
// move search to that empty one.
func TestChoosingTheModelOfAnAdoptedCollectionAgainKeepsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedEmbeddedChunk(t, h)
	adopted := settings.Embedding{Provider: "p", Model: "embed", Dimensions: 1024, Collection: "diderot_chunks_legacy_20260907"}
	row, outcome, err := h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed", Adopt: adopted.Collection})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingInUse, outcome)
	require.Equal(t, &adopted, row.Embedding)

	for _, choice := range []EmbeddingChoice{{Ref: "p/embed"}, {Ref: "p/embed", Reembed: true}, {Ref: "p/embed", Adopt: adopted.Collection}} {
		row, outcome, err = h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, choice)
		require.NoError(t, err)
		assert.Equal(t, EmbeddingUnchanged, outcome)
		assert.Equal(t, &adopted, row.Embedding, "the adopted collection is kept")
		assert.Nil(t, row.Filling, "and nothing is embedded again")
	}
	assert.Len(t, h.catalog.prepared, 1, "no other collection was created")
}

func seedEmbeddedChunk(t *testing.T, h *harness) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), `
		WITH e AS (
		  INSERT INTO events (source, external_id, occurred_at, actor, body, raw)
		  VALUES ('slack', 'adopt-1', now(), 'u', 'body', '{}') RETURNING id)
		INSERT INTO event_chunks (event_id, chunk_idx, content, occurred_at, embedded_at, embedder_id)
		SELECT id, 0, 'body', now(), now(), 'self:embed:v1' FROM e`)
	require.NoError(t, err)
}

// TestChooseEmbeddingOnAnInstallWithVectors: chunks embedded before any
// choice was saved (an upgrade) are kept by adopting their collection. A
// model chosen without naming it is filled first, and search is not moved
// to its empty collection.
func TestChooseEmbeddingOnAnInstallWithVectors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedEmbeddedChunk(t, h)
	assert.Equal(t, int64(1), h.models(t).EmbeddedChunks)

	// Jobs wait for the embedding model: jobs saved without one is what a
	// models row that lost its embedding looks like.
	err := h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/big"}})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "choose the embedding model first")

	_, _, err = h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed"})
	require.ErrorIs(t, err, ErrReembedUnconfirmed, "a new collection would leave the existing vectors behind")
	h.catalog.missing["nowhere"] = true
	_, _, err = h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed", Adopt: "nowhere"})
	require.ErrorIs(t, err, settings.ErrInvalid)
	_, _, err = h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed", Adopt: "bad name"})
	require.ErrorIs(t, err, settings.ErrInvalid)

	row, outcome, err := h.service.ChooseEmbedding(ctx, operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed", Reembed: true})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingFilling, outcome)
	assert.Nil(t, row.Embedding, "search stays off, not on an empty collection")
	require.NotNil(t, row.Filling)
	assert.Equal(t, "diderot_chunks_embed_1024", row.Filling.Collection)
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/big"}}), "jobs can be chosen while it fills")
}

func TestAdoptingACollectionKeepsEveryVector(t *testing.T) {
	h := newHarness(t)
	seedEmbeddedChunk(t, h)
	row, outcome, err := h.service.ChooseEmbedding(context.Background(), operator, h.models(t).Version, EmbeddingChoice{Ref: "p/embed", Adopt: "chunks_pinned"})
	require.NoError(t, err)
	assert.Equal(t, EmbeddingInUse, outcome)
	assert.Equal(t, &settings.Embedding{Provider: "p", Model: "embed", Dimensions: 1024, Collection: "chunks_pinned"}, row.Embedding,
		"adopting keeps every vector: no re-embed")
	assert.Nil(t, row.Filling)
}

// TestModelsStepNeedsATestOfTheModelsInUse: Test models tests the saved
// choices, and the step is done only while the last passing test covered the
// models in use.
func TestModelsStepNeedsATestOfTheModelsInUse(t *testing.T) {
	h := newHarness(t)
	h.tests = passing(h.models(t))
	_, err := h.do(t, operator, ActionTestModels)
	require.NoError(t, err)
	assert.True(t, h.checklist(t).Step(StepModels).Done)

	// A save records a test of what it saved; one that fails leaves the
	// step open.
	h.tests = []settings.RoleTest{{Role: "Titles", Model: "p/big", Passed: false, Error: "HTTP 500"}}
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"titles": "p/big"}}))
	assert.Equal(t, map[string]string{"titles": "p/big"}, h.tested.Roles, "the test is given the saved choices")
	step := h.checklist(t).Step(StepModels)
	assert.False(t, step.Done)
	assert.Equal(t, "The last test failed: Titles.", step.Status)

	// A test that did not cover the model in use leaves the step open too.
	h.tests = []settings.RoleTest{{Role: "Titles", Model: "p/big", Passed: true}}
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"titles": "p/small"}}))
	assert.Equal(t, "The models changed since the last test. Test them again.", h.checklist(t).Step(StepModels).Status)
}

func TestAccessSummaryListsAskChannels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_private) VALUES ('C1', 'general', false), ('C2', 'secret', true);
		INSERT INTO slack_channel_config (channel_id, ask_enabled, allow_external_ask) VALUES
		    ('C1', true, false), ('C2', true, true), ('C3', false, false)`)
	require.NoError(t, err)
	summary, err := h.service.Access(ctx, h.store.Org().Org)
	require.NoError(t, err)
	assert.Equal(t, "static", summary.AuthMode)
	assert.False(t, summary.Acknowledged)
	assert.Equal(t, []AskEnabledChannel{
		{ID: "C1", Name: "general"},
		{ID: "C2", Name: "secret", Private: true, ExternalUsers: true},
	}, summary.Channels)
}
