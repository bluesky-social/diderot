// Package setup computes the fresh-install checklist behind /admin/settings and
// performs its actions. Each step
// reports its status and links to the page where the work is done; the
// actions write the org row of connector_settings through internal/settings.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Step IDs. The first six are required; the rest, and each registered tool's
// and automation's (asktool.Info.Step, automation.Info.Step), are shown,
// never required.
const (
	StepOrganization   = "organization"
	StepModels         = "models"
	StepSources        = "sources"
	StepPrompts        = "prompts"
	StepAccess         = "access"
	StepBootstrapAdmin = "bootstrap-admin"

	StepAskMCPTools      = "ask-mcp-tools"
	StepScheduledReports = "scheduled-reports"
)

// Actions, as the API and the browser forms name them.
const (
	ActionAcknowledgeAccess     = "acknowledge-access"
	ActionAcceptExamplePrompts  = "accept-example-prompts"
	ActionTestModels            = "test-models"
	ActionDisableBootstrapAdmin = "disable-bootstrap-admin"
	ActionGoLive                = "go-live"
	ActionBackToSetup           = "back-to-setup"
)

// Actions lists every action Do accepts.
var Actions = []string{
	ActionAcknowledgeAccess, ActionAcceptExamplePrompts, ActionTestModels,
	ActionDisableBootstrapAdmin, ActionGoLive, ActionBackToSetup,
}

// SeedPromptAuthor is the created_by of the example prompt release the server
// seeds into an empty database.
const SeedPromptAuthor = "diderot:seed"

// ModelTestTimeout bounds one "Test models" run.
const ModelTestTimeout = 30 * time.Second

// DocsURL is where the checklist links steps whose pages don't exist yet.
const DocsURL = "https://github.com/bluesky-social/diderot/blob/main/docs/"

// Registries are the connectors a build registers, by kind.
type Registries struct {
	Sources     source.Registry
	Tools       asktool.Registry
	Automations automation.Registry
}

// Names returns every registered connector's name in the order pages list
// them: the sources, then the tools, then the automations, each in
// registration order.
func (r Registries) Names() []string {
	return append(append(r.Sources.Names(), r.Tools.Names()...), r.Automations.Names()...)
}

// Title is how pages name a connector: its registered title, or else its
// name capitalized.
func (r Registries) Title(name string) string {
	for _, src := range r.Sources.Sources() {
		if src.Name() == name {
			return src.Info().Title
		}
	}
	if tool, ok := r.Tools.Lookup(name); ok {
		return tool.Info().Title
	}
	if automation, ok := r.Automations.Lookup(name); ok {
		return automation.Info().Title
	}
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// Status reads the named connector's status, a source's through deps, and
// reports false for a connector that reports none.
func (r Registries) Status(ctx context.Context, deps source.Deps, name string) (settings.Status, bool, error) {
	if src, ok := r.Sources.Named(name); ok {
		if reporter, ok := src.(source.StatusReporter); ok {
			status, err := reporter.Status(ctx, deps.For(name))
			return status, true, err
		}
	}
	if auto, ok := r.Automations.Lookup(name); ok {
		if reporter, ok := auto.(automation.StatusReporter); ok {
			status, err := reporter.Status(ctx)
			return status, true, err
		}
	}
	return settings.Status{}, false, nil
}

// Org field limits. Both are single-line prompt variables.
const (
	MaxOrgNameRunes        = 200
	MaxOrgDescriptionRunes = 500
)

var (
	// ErrNotReady means Go live was asked for before every required step
	// was done.
	ErrNotReady = errors.New("setup: every required step must be done before going live")
	// ErrBootstrapSession means the bootstrap admin tried to turn itself
	// off: only a session from another sign-in proves another admin works.
	ErrBootstrapSession = errors.New("setup: sign in another way to turn off the bootstrap admin; doing it as the bootstrap admin would not prove another sign-in works")
	// ErrUnknownAction names an action Do does not have.
	ErrUnknownAction = errors.New("setup: unknown action")
)

// Actor is who performs an action. Bootstrap marks the bootstrap admin.
type Actor struct {
	Email     string
	RequestID string
	Bootstrap bool
}

func (a Actor) settings() settings.Actor {
	return settings.Actor{Email: a.Email, RequestID: a.RequestID}
}

// Facts is what the checklist knows from configuration, fixed at boot.
type Facts struct {
	Access Access
	// SessionPush is agent-session push ingest, nil while it is off.
	SessionPush *SessionPush
}

// SessionPush is agent-session push ingest as the deployment sets it up. It
// counts as a source while someone can push and pushed sessions are
// summarized, which makes them searchable.
type SessionPush struct {
	// NoPusher says why no caller can push, empty when one can.
	NoPusher string
	// Summaries says whether sessions are summarized now
	// (*sessioningest.SummaryControl).
	Summaries SessionSummaries
}

// SessionSummaries reads whether agent sessions are summarized now.
type SessionSummaries interface {
	State(context.Context) (sessioningest.SummaryControlState, error)
}

// Access is who can reach Diderot, from the auth configuration.
type Access struct {
	AuthMode          string   `json:"auth_mode"`
	BrowserSignIn     []string `json:"browser_sign_in"`
	AdminGroups       []string `json:"admin_groups"`
	SelfServiceGroups []string `json:"self_service_groups"`
	PolicyPath        string   `json:"authorization_policy_path,omitempty"`
}

// ModelTester makes one tiny call to each enabled role's model under saved,
// within ctx's deadline, and reports each outcome without provider text or
// credentials. A role's result names it by its ModelRole title. With roles
// (ModelRole keys), it tests only those; without, every enabled role and the
// embedding model.
type ModelTester func(ctx context.Context, saved settings.Models, roles ...string) []settings.RoleTest

// Step is one checklist row.
type Step struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Required bool   `json:"required"`
	Done     bool   `json:"done"`
	// Summary says what the step is for; Status, its state now.
	Summary string `json:"summary"`
	Status  string `json:"status"`
	// By and At are who the state is owed to and when, for the steps that
	// record it.
	By string     `json:"by,omitempty"`
	At *time.Time `json:"at,omitempty"`
	// Setting names the deployment variable that turns on a step the admin
	// pages cannot.
	Setting string `json:"setting,omitempty"`
	Link    string `json:"link"`
}

// Checklist is the whole setup page.
type Checklist struct {
	State      string     `json:"state"`
	WentLiveBy string     `json:"went_live_by,omitempty"`
	WentLiveAt *time.Time `json:"went_live_at,omitempty"`
	// Version is the org row's version; every action posts it back.
	Version               int64  `json:"version"`
	OrgName               string `json:"org_name"`
	OrgDescription        string `json:"org_description"`
	BootstrapAdminEnabled bool   `json:"bootstrap_admin_enabled"`
	// Ready reports every required step done, which Go live needs.
	Ready bool   `json:"ready"`
	Steps []Step `json:"steps"`
	// Models is the Models page, with its own version for a save.
	Models ModelsView `json:"models"`
}

// Step returns the step with id, or a zero Step.
func (c Checklist) Step(id string) Step {
	for _, step := range c.Steps {
		if step.ID == id {
			return step
		}
	}
	return Step{}
}

// Service computes the checklist and performs the setup actions.
type Service struct {
	pool       *pgxpool.Pool
	settings   *settings.Store
	registries Registries
	facts      Facts
	catalog    Catalog
	test       ModelTester
	now        func() time.Time

	// testing serializes model tests on this replica.
	testing sync.Mutex
}

// New returns a Service. registries are the connectors this build
// registers: the checklist asks for one of the sources to be on, and names
// the others by their titles. catalog and test may be nil in a server
// without models.
func New(pool *pgxpool.Pool, store *settings.Store, registries Registries, facts Facts, catalog Catalog, test ModelTester) *Service {
	return &Service{pool: pool, settings: store, registries: registries, facts: facts, catalog: catalog, test: test, now: time.Now}
}

// Facts returns the configuration the checklist was built with.
func (s *Service) Facts() Facts { return s.facts }

// Org returns the cached org row, for banners on every admin page.
func (s *Service) Org() settings.OrgRow { return s.settings.Org() }

// ReadOrg reads the org row from the database, for a page that posts its
// version back.
func (s *Service) ReadOrg(ctx context.Context) (settings.OrgRow, error) {
	return s.settings.ReadOrg(ctx)
}

// Checklist reads the org row from the database (not the cache, so a page
// shows the version it posts back) and every step's state.
func (s *Service) Checklist(ctx context.Context) (Checklist, error) {
	row, err := s.settings.ReadOrg(ctx)
	if err != nil {
		return Checklist{}, err
	}
	return s.checklist(ctx, row)
}

func (s *Service) checklist(ctx context.Context, row settings.OrgRow) (Checklist, error) {
	org := row.Org
	views, err := s.settings.List(ctx)
	if err != nil {
		return Checklist{}, err
	}
	// enabled has an entry for every registered connector.
	enabled := map[string]bool{}
	for _, view := range views {
		enabled[view.Name] = view.Enabled
	}
	prompts, err := s.promptsStep(ctx, org)
	if err != nil {
		return Checklist{}, err
	}
	reports, err := s.scheduledReportsStep(ctx)
	if err != nil {
		return Checklist{}, err
	}
	models, err := s.Models(ctx)
	if err != nil {
		return Checklist{}, err
	}
	sources, err := s.sourcesStep(ctx, enabled)
	if err != nil {
		return Checklist{}, err
	}
	steps := []Step{
		organizationStep(org),
		modelsStep(org, models),
		sources,
		prompts,
		accessStep(org),
		bootstrapStep(org),
	}
	for _, tool := range s.registries.Tools.Tools() {
		info := tool.Info()
		steps = append(steps, connectorStep(enabled, info.Step, tool.Name(), info.Title, info.Summary))
	}
	connections, err := s.settings.ListFamily(ctx, askconn.Prefix)
	if err != nil {
		return Checklist{}, err
	}
	steps = append(steps, connectionsStep(connections), reports)
	for _, auto := range s.registries.Automations.Automations() {
		info := auto.Info()
		steps = append(steps, connectorStep(enabled, info.Step, auto.Name(), info.Title, info.Summary))
	}
	checklist := Checklist{
		State: org.State, WentLiveBy: org.WentLiveBy, WentLiveAt: org.WentLiveAt,
		Version: row.Version, OrgName: org.Name, OrgDescription: org.Description,
		BootstrapAdminEnabled: org.BootstrapAdminEnabled, Ready: true, Steps: steps, Models: models,
	}
	for _, step := range steps {
		if step.Required && !step.Done {
			checklist.Ready = false
		}
	}
	return checklist, nil
}

func organizationStep(org settings.Org) Step {
	step := Step{
		ID: StepOrganization, Title: "Organization", Required: true, Link: "/admin/settings/organization",
		Summary: "Name your organization, so answers and prompts refer to it correctly.",
	}
	if org.Name == "" {
		step.Status = "No name saved."
		return step
	}
	step.Done, step.Status = true, "Named "+org.Name+"."
	return step
}

// modelsStep is done when the last test passed on the models in use now.
func modelsStep(org settings.Org, models ModelsView) Step {
	step := Step{
		ID: StepModels, Title: "Models", Required: true, Link: "/admin/settings/models",
		Summary: "Add a model provider, then choose the embedding model and each job's model; saving tests them.",
	}
	test := org.ModelsTest
	switch {
	case test == nil:
		step.Status = "Not tested yet."
	case test.Passed && !testedInUse(*test, models):
		step.Status = "The models changed since the last test. Test them again."
	case test.Passed:
		step.Done, step.Status = true, "The last test passed."
		step.By, step.At = test.By, &test.At
	default:
		var failed []string
		for _, role := range test.Roles {
			if !role.Passed {
				failed = append(failed, role.Role)
			}
		}
		step.Status = fmt.Sprintf("The last test failed: %s.", strings.Join(failed, ", "))
		step.By, step.At = test.By, &test.At
	}
	return step
}

// testedInUse reports whether test covered every role that is on, with the
// model it uses now.
func testedInUse(test settings.ModelsTest, models ModelsView) bool {
	for _, role := range models.Roles {
		if role.Model == "" {
			continue
		}
		tested := slices.ContainsFunc(test.Roles, func(result settings.RoleTest) bool {
			return result.Role == role.Title && result.Model == role.Model
		})
		if !tested {
			return false
		}
	}
	return true
}

// sessionPushTitle is how the checklist names agent-session push ingest.
const sessionPushTitle = "Agent-session push"

// sourcesStep is done once a source connector is enabled, or agent sessions
// can be pushed and are summarized. While push is set up but can't make
// sessions searchable, it says why.
func (s *Service) sourcesStep(ctx context.Context, enabled map[string]bool) (Step, error) {
	step := Step{
		ID: StepSources, Title: "Sources", Required: true, Link: "/admin/connectors",
		Summary: "Connect at least one source, such as Slack, for Diderot to index and search.",
	}
	var on []string
	for _, src := range s.registries.Sources.Sources() {
		if enabled[src.Name()] {
			on = append(on, src.Info().Title)
		}
	}
	var pushNote string
	if push := s.facts.SessionPush; push != nil {
		state, err := push.Summaries.State(ctx)
		if err != nil {
			return Step{}, fmt.Errorf("setup: session summaries: %w", err)
		}
		switch {
		case push.NoPusher != "":
			pushNote = push.NoPusher
		case !state.EffectiveEnabled:
			pushNote = "Pushed agent sessions aren't searchable yet. " + state.Reason
		default:
			on = append(on, sessionPushTitle)
		}
	}
	switch {
	case len(on) > 0:
		step.Done, step.Status = true, "On: "+strings.Join(on, ", ")+"."
		if pushNote != "" {
			step.Status += " " + pushNote
		}
	case pushNote != "":
		step.Status = pushNote
	default:
		step.Status = "No source is on. Set one up on the Connectors page."
	}
	return step, nil
}

// promptsStep is done when the active prompt release is not the seeded
// example, or an administrator chose to use the example as-is.
func (s *Service) promptsStep(ctx context.Context, org settings.Org) (Step, error) {
	step := Step{
		ID: StepPrompts, Title: "Prompts", Required: true, Link: "/admin/prompts",
		Summary: "Review the instructions Ask follows. A new install starts with an example set.",
	}
	var createdBy string
	err := s.pool.QueryRow(ctx, `
		SELECT r.created_by
		  FROM prompt_active_release a JOIN prompt_releases r ON r.id = a.release_id
		 WHERE a.singleton`).Scan(&createdBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		step.Status = "No prompts are active."
		return step, nil
	case err != nil:
		return Step{}, fmt.Errorf("setup: prompts: %w", err)
	case createdBy != SeedPromptAuthor:
		step.Done, step.Status = true, "Edited prompts are active."
	case org.PromptsExampleAcceptedBy != "":
		step.Done, step.Status, step.By = true, "Using the example prompts as-is.", org.PromptsExampleAcceptedBy
	default:
		step.Status = "The example prompts are active. Edit them, or use them as-is."
	}
	return step, nil
}

func accessStep(org settings.Org) Step {
	step := Step{
		ID: StepAccess, Title: "Access", Required: true, Link: "/admin/settings/access",
		Summary: "Check who can sign in and where Ask answers before you open it to everyone.",
	}
	if org.AccessAcknowledgedBy == "" {
		step.Status = "Not reviewed yet."
		return step
	}
	step.Done, step.Status, step.By = true, "Reviewed.", org.AccessAcknowledgedBy
	return step
}

func bootstrapStep(org settings.Org) Step {
	step := Step{
		ID: StepBootstrapAdmin, Title: "Replace the bootstrap admin", Required: true, Link: "/admin/settings#bootstrap-admin",
		Summary: "A new install lets anyone sign in as admin / admin. Sign in another way, then turn that account off.",
	}
	if org.BootstrapAdminEnabled {
		step.Status = "The bootstrap admin is on."
		return step
	}
	step.Done, step.Status = true, "The bootstrap admin is off."
	return step
}

// connectorStep is the optional step for a tool or automation, done while
// it is enabled.
func connectorStep(enabled map[string]bool, id, connector, title, summary string) Step {
	step := Step{
		ID: id, Title: title, Done: enabled[connector], Summary: summary, Status: "Off.",
		Link: "/admin/connectors/" + connector,
	}
	if step.Done {
		step.Status = "On."
	}
	return step
}

// connectionsStep is the tool connections' step: done while one is on.
func connectionsStep(connections []settings.View) Step {
	step := Step{
		ID: StepAskMCPTools, Title: "Tool connections", Link: ConnectionsPath,
		Summary: "Gives Ask the tools of MCP servers you run, such as Grafana or Sentry.",
		Status:  "None on.",
	}
	var on []string
	for _, view := range connections {
		if view.Enabled {
			on = append(on, askconn.Name(view.Name))
		}
	}
	if len(on) > 0 {
		step.Done, step.Status = true, "On: "+strings.Join(on, ", ")+"."
	}
	return step
}

// ConnectionsPath is where the Connectors page lists the tool connections.
const ConnectionsPath = "/admin/connectors#connections"

func (s *Service) scheduledReportsStep(ctx context.Context) (Step, error) {
	step := Step{
		ID: StepScheduledReports, Title: "Scheduled reports", Link: "/admin/reports",
		Summary: "Runs a question on a schedule and delivers the answer to Slack.",
	}
	var enabled int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_reports WHERE enabled`).Scan(&enabled); err != nil {
		return Step{}, fmt.Errorf("setup: scheduled reports: %w", err)
	}
	if enabled == 0 {
		step.Status = "None on."
		return step, nil
	}
	step.Done, step.Status = true, fmt.Sprintf("%d on.", enabled)
	return step, nil
}

// SaveOrg saves the organization's name and description.
func (s *Service) SaveOrg(ctx context.Context, actor Actor, version int64, name, description, dashboard string) (settings.OrgRow, error) {
	name, err := settings.ValidateText("The name", name, MaxOrgNameRunes)
	if err != nil {
		return settings.OrgRow{}, err
	}
	description, err = settings.ValidateText("The description", description, MaxOrgDescriptionRunes)
	if err != nil {
		return settings.OrgRow{}, err
	}
	if dashboard, err = validateDashboardURL(dashboard); err != nil {
		return settings.OrgRow{}, err
	}
	return s.settings.UpdateOrg(ctx, version, actor.settings(), "setup.organization.save", func(org *settings.Org) error {
		org.Name, org.Description, org.DashboardURL = name, description, dashboard
		return nil
	})
}

// MaxDashboardURLRunes bounds the operational dashboard's address.
const MaxDashboardURLRunes = 2000

// validateDashboardURL admits an empty link or an http(s) address without
// credentials, which every admin page links to.
func validateDashboardURL(raw string) (string, error) {
	raw, err := settings.ValidateText("The dashboard address", raw, MaxDashboardURLRunes)
	if err != nil || raw == "" {
		return raw, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: the dashboard address must be an http or https address without a user name or password", settings.ErrInvalid)
	}
	return raw, nil
}

// Do performs one setup action against the org row at version.
func (s *Service) Do(ctx context.Context, actor Actor, version int64, action string) (settings.OrgRow, error) {
	now := s.now().UTC()
	by := strings.ToLower(strings.TrimSpace(actor.Email))
	update := func(change func(*settings.Org) error) (settings.OrgRow, error) {
		return s.settings.UpdateOrg(ctx, version, actor.settings(), "setup."+strings.ReplaceAll(action, "-", "_"), change)
	}
	switch action {
	case ActionAcknowledgeAccess:
		return update(func(org *settings.Org) error {
			org.AccessAcknowledgedBy, org.AccessAcknowledgedAt = by, &now
			return nil
		})
	case ActionAcceptExamplePrompts:
		return update(func(org *settings.Org) error {
			org.PromptsExampleAcceptedBy, org.PromptsExampleAcceptedAt = by, &now
			return nil
		})
	case ActionTestModels:
		return s.testModels(ctx, version, update, by)
	case ActionDisableBootstrapAdmin:
		if actor.Bootstrap {
			return settings.OrgRow{}, ErrBootstrapSession
		}
		return update(func(org *settings.Org) error {
			org.BootstrapAdminEnabled = false
			return nil
		})
	case ActionGoLive:
		row, err := s.settings.ReadOrg(ctx)
		if err != nil {
			return settings.OrgRow{}, err
		}
		if row.Version != version {
			return settings.OrgRow{}, settings.ErrConflict
		}
		checklist, err := s.checklist(ctx, row)
		if err != nil {
			return settings.OrgRow{}, err
		}
		if !checklist.Ready {
			return settings.OrgRow{}, ErrNotReady
		}
		return update(func(org *settings.Org) error {
			org.State, org.WentLiveBy, org.WentLiveAt = settings.StateLive, by, &now
			return nil
		})
	case ActionBackToSetup:
		return update(func(org *settings.Org) error {
			org.State, org.WentLiveBy, org.WentLiveAt = settings.StateSetup, "", nil
			return nil
		})
	default:
		return settings.OrgRow{}, fmt.Errorf("%w %q", ErrUnknownAction, action)
	}
}

// testModels runs the model test and records its result. A version that is
// already stale fails before any model is called.
func (s *Service) testModels(ctx context.Context, version int64, update func(func(*settings.Org) error) (settings.OrgRow, error), by string) (settings.OrgRow, error) {
	if current, err := s.settings.ReadOrg(ctx); err != nil {
		return settings.OrgRow{}, err
	} else if current.Version != version {
		return settings.OrgRow{}, settings.ErrConflict
	}
	// The saved choices, not this replica's cache, which may not have seen a
	// save made on another replica yet.
	models, err := s.settings.ReadModels(ctx)
	if err != nil {
		return settings.OrgRow{}, err
	}
	s.testing.Lock()
	defer s.testing.Unlock()
	var roles []settings.RoleTest
	if s.test != nil {
		testCtx, cancel := context.WithTimeout(ctx, ModelTestTimeout)
		roles = s.test(testCtx, models.Models)
		cancel()
	}
	result := &settings.ModelsTest{Passed: len(roles) > 0, By: by, At: s.now().UTC(), Roles: roles}
	for _, role := range roles {
		if !role.Passed {
			result.Passed = false
		}
	}
	if len(roles) == 0 {
		result.Roles = []settings.RoleTest{{Role: "all", Passed: false, Error: "no model is chosen for any job"}}
	}
	return update(func(org *settings.Org) error {
		org.ModelsTest = result
		return nil
	})
}

// ResetBootstrapAdmin re-enables the bootstrap admin. It is break-glass for
// an install whose administrators are locked out, run on the server with
// database access, so it skips the version check.
func ResetBootstrapAdmin(ctx context.Context, store *settings.Store, actor string) (settings.OrgRow, error) {
	return store.ForceUpdateOrg(ctx, settings.Actor{Email: actor}, "setup.bootstrap_admin.reset", func(org *settings.Org) error {
		org.BootstrapAdminEnabled = true
		return nil
	})
}

// AccessSummary is the Access page: Facts.Access plus the Slack channels
// with Ask enabled.
type AccessSummary struct {
	Access
	Acknowledged   bool                `json:"acknowledged"`
	AcknowledgedBy string              `json:"acknowledged_by,omitempty"`
	Channels       []AskEnabledChannel `json:"ask_enabled_channels"`
}

// AskEnabledChannel is one Slack channel where Ask is on.
type AskEnabledChannel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	ExternalUsers bool   `json:"allow_external_ask"`
}

// Access reads the access summary.
func (s *Service) Access(ctx context.Context, org settings.Org) (AccessSummary, error) {
	summary := AccessSummary{
		Access:       s.facts.Access,
		Acknowledged: org.AccessAcknowledgedBy != "", AcknowledgedBy: org.AccessAcknowledgedBy,
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.channel_id, COALESCE(i.name, ''), COALESCE(i.is_private, false), c.allow_external_ask
		  FROM slack_channel_config c
		  LEFT JOIN slack_channel_info i ON i.channel_id = c.channel_id
		 WHERE c.ask_enabled
		 ORDER BY COALESCE(i.name, c.channel_id)`)
	if err != nil {
		return AccessSummary{}, fmt.Errorf("setup: Ask channels: %w", err)
	}
	channels, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AskEnabledChannel, error) {
		var channel AskEnabledChannel
		err := row.Scan(&channel.ID, &channel.Name, &channel.Private, &channel.ExternalUsers)
		return channel, err
	})
	if err != nil {
		return AccessSummary{}, fmt.Errorf("setup: Ask channels: %w", err)
	}
	summary.Channels = channels
	if summary.Channels == nil {
		summary.Channels = []AskEnabledChannel{}
	}
	return summary, nil
}
