package adminui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/sessioningest"
)

var errTrailingJSON = errors.New("trailing JSON value")

// Stable fixture identities keep links between the synthetic pages useful.
const (
	DevAskSessionID       = "11111111-1111-4111-8111-111111111111"
	DevAskFailedSessionID = "33333333-3333-4333-8333-333333333333"
	DevReportID           = "22222222-2222-4222-8222-222222222222"
	DevSlackChannel       = "CPLATFORM"
)

// devFixture is intentionally just an envelope. Each page is decoded into its
// production view model at request time, so template and fixture drift fails
// loudly while edits to the JSON are visible on browser refresh.
type devFixture struct {
	Pages map[string]json.RawMessage `json:"pages"`
}

type devRoute struct {
	key, title, nav, stylesheet, script string
	template                            *template.Template
	newData                             func() any
	// tab is the route's tab in its section, where the section has tabs.
	tab string
	// live renders the route as a live install: no setup banner.
	live bool
}

// Routes only the ui-dev server has: the style guide, which shows every
// shared component in every state, and states of the setup checklist that
// one fixture install cannot be in at once.
const (
	DevStyleGuidePath    = "/admin/dev/styleguide"
	DevSettingsFreshPath = "/admin/dev/settings-fresh"
	DevSettingsLivePath  = "/admin/dev/settings-live"
	DevSelfDeniedPath    = "/admin/dev/sessions-denied"
	// An install with nothing yet: no questions, sources, or jobs.
	DevOverviewEmptyPath   = "/admin/dev/overview-empty"
	DevOperationsEmptyPath = "/admin/dev/operations-empty"
	DevJobsEmptyPath       = "/admin/dev/jobs-empty"
	// Jobs and maintenance with nothing running, the Notion page with its
	// shared pages open, and the Slack channels without a user token.
	DevJobsIdlePath      = "/admin/dev/jobs-idle"
	DevNotionSharedPath  = "/admin/dev/notion-shared"
	DevSlackNoTokenPath  = "/admin/dev/slack-channels-no-token"
	DevAdminDeniedPath   = "/admin/dev/admin-denied"
	DevSessionsEmptyPath = "/admin/dev/sessions-empty"
	DevAskEmptyPath      = "/admin/dev/ask-empty"
	// Feedback before anyone has rated an answer in the span shown.
	DevFeedbackEmptyPath = "/admin/dev/feedback-empty"
	// A Sentry connection just added, and a connection whose latest check
	// failed.
	DevConnectionNewSentryPath = "/admin/dev/connection-sentry-new"
	DevConnectionFailingPath   = "/admin/dev/connection-failing"
	// The Models page of an install with no provider yet, one whose
	// embedding change waits for confirmation, and a provider whose
	// latest listing failed.
	DevModelsFreshPath   = "/admin/dev/models-fresh"
	DevModelsReembedPath = "/admin/dev/models-reembed"
	// The Models page of a deployment whose caption model is retired
	// tomorrow, with one published list failing and an embedding change
	// under way; a model's page for a model the catalog only suggests an
	// entry for, and for the model being retired.
	DevModelsRetiringPath    = "/admin/dev/models-retiring"
	DevModelSuggestedPath    = "/admin/dev/model-suggested"
	DevModelRetiringPath     = "/admin/dev/model-retiring"
	DevProviderFailingPath   = "/admin/dev/provider-failing"
	DevProviderNoListingPath = "/admin/dev/provider-no-listing"
)

var styleGuidePage = mustPage("styleguide.html")

// styleGuideData is the style guide's sample values.
type styleGuideData struct {
	pageData
	Now       time.Time
	Minutes   time.Time
	Hours     time.Time
	Days      time.Time
	Old       time.Time
	Upcoming  time.Time
	Never     *time.Time
	Durations []time.Duration
	Counts    []int64
	Sizes     []int64
	// Chart, SplitChart, Pages, NamedPages and CountedTabs feed the
	// partials' examples.
	Chart       barChart
	SplitChart  barChart
	Pages       pagination
	NamedPages  pagination
	CountedTabs tabSet
}

func newStyleGuideData() any {
	now := time.Now()
	chart := barChart{Caption: "Questions per hour", From: "12 hours ago", To: "Now", Summary: "Example: questions per hour over 12 hours."}
	for i, count := range []int64{2, 1, 0, 3, 6, 11, 14, 9, 12, 7, 4, 2} {
		chart.Bars = append(chart.Bars, chartBar{Start: now.Add(time.Duration(i-12) * time.Hour).Truncate(time.Hour), Count: count})
	}
	split := barChart{
		Caption: "Ratings per week", From: "8 weeks ago", To: "Now", BadLabel: "Not helpful", RestLabel: "Helpful",
		Summary: "Example: ratings per week over 8 weeks, with the not-helpful ones on top.",
		Note:    "Too few ratings to show a trend: read these as counts, not as a direction.",
	}
	for i, counts := range [][2]int64{{1, 0}, {3, 1}, {0, 0}, {2, 0}, {4, 2}, {1, 0}, {2, 1}, {3, 0}} {
		split.Bars = append(split.Bars, chartBar{Start: now.AddDate(0, 0, 7*(i-8)), Count: counts[0], Bad: counts[1]})
	}
	return &styleGuideData{
		Now: now, Minutes: now.Add(-3 * time.Minute), Hours: now.Add(-5 * time.Hour),
		Days: now.Add(-2 * 24 * time.Hour), Old: now.Add(-400 * 24 * time.Hour), Upcoming: now.Add(4 * time.Hour),
		Durations:  []time.Duration{83 * time.Millisecond, 1200 * time.Millisecond, 31 * time.Second, 8*time.Minute + 20*time.Second, 2*time.Hour + 5*time.Minute, 76 * time.Hour},
		Counts:     []int64{950, 12_340, 4_100_000},
		Sizes:      []int64{512, 3_565_158, 12_884_901_888},
		Chart:      chart,
		SplitChart: split,
		Pages:      pagination{Summary: "50 changes shown, newest first", BackURL: "#pagination", ForwardURL: "#pagination"},
		NamedPages: pagination{
			Summary: "51–100 of 250 channels", BackURL: "#pagination", BackLabel: "Previous", ForwardURL: "#pagination", ForwardLabel: "Next",
		},
		CountedTabs: tabSet{Label: "Example views", Tabs: []navTab{
			{Label: "All", Href: "#counted-tabs", Current: true, Count: 250, Counted: true},
			{Label: "Ask on", Href: "#counted-tabs", Count: 104, Counted: true},
			{Label: "Can't be read", Href: "#counted-tabs", Count: 2, Counted: true},
		}},
	}
}

var devRoutes = map[string]devRoute{
	"/sessions":                                    {key: "sessions", title: "My sessions", live: true, template: ownerSessionsPage, newData: func() any { return &ownerSessionsData{} }},
	"/admin/":                                      {key: "overview", title: "Overview", nav: "overview", stylesheet: "overview.css", live: true, template: overviewPage, newData: func() any { return &overviewData{} }},
	"/admin/ask/sessions":                          {key: "ask_sessions", title: "Conversations", nav: navAsk, stylesheet: "asks.css", live: true, template: askSessionsPage, newData: func() any { return &askSessionsData{} }},
	"/admin/ask/sessions/" + DevAskSessionID:       {key: "ask_session", title: "What changed in the platform rollout?", nav: navAsk, stylesheet: "asks.css", live: true, template: askSessionPage, newData: func() any { return &askSessionData{} }},
	settingsPath:                                   {key: "setup", title: "Setup checklist", nav: navSettings, tab: tabSettingsSetup, template: setupPage, newData: func() any { return &setupData{} }},
	DevSettingsFreshPath:                           {key: "setup_fresh", title: "Setup checklist", nav: navSettings, tab: tabSettingsSetup, template: setupPage, newData: func() any { return &setupData{} }},
	DevSettingsLivePath:                            {key: "setup_live", title: "Setup checklist", nav: navSettings, tab: tabSettingsSetup, live: true, template: setupPage, newData: func() any { return &setupData{} }},
	organizationPath:                               {key: "organization", title: "Organization", nav: navSettings, tab: tabSettingsOrganization, template: organizationPage, newData: func() any { return &organizationData{} }},
	modelsPath:                                     {key: "models", title: "Models", nav: navSettings, tab: tabSettingsModels, template: modelsPage, newData: func() any { return &modelsData{} }},
	DevModelsFreshPath:                             {key: "models_fresh", title: "Models", nav: navSettings, tab: tabSettingsModels, template: modelsPage, newData: func() any { return &modelsData{} }},
	DevModelsReembedPath:                           {key: "models_reembed", title: "Models", nav: navSettings, tab: tabSettingsModels, live: true, template: modelsPage, newData: func() any { return &modelsData{} }},
	DevModelsRetiringPath:                          {key: "models_retiring", title: "Models", nav: navSettings, tab: tabSettingsModels, live: true, template: modelsPage, newData: func() any { return &modelsData{} }},
	DevModelSuggestedPath:                          {key: "model_entry_suggested", title: "deepseek-v4.1-flash:main", nav: navSettings, tab: tabSettingsModels, live: true, template: modelEntryPage, newData: func() any { return &modelEntryData{} }},
	DevModelRetiringPath:                           {key: "model_entry_retiring", title: "gemini-3.6-flash", nav: navSettings, tab: tabSettingsModels, live: true, template: modelEntryPage, newData: func() any { return &modelEntryData{} }},
	modelPath:                                      {key: "model_entry", title: "gpt-6-luna", nav: navSettings, tab: tabSettingsModels, live: true, template: modelEntryPage, newData: func() any { return &modelEntryData{} }},
	providersPath + "/gateway":                     {key: "provider", title: "gateway", nav: navSettings, tab: tabSettingsModels, stylesheet: "connectors.css", live: true, template: providerPage, newData: func() any { return &providerData{} }},
	providersPath + "/new":                         {key: "provider_new", title: "Add a model provider", nav: navSettings, tab: tabSettingsModels, stylesheet: "connectors.css", template: newProviderPage, newData: func() any { return &newProviderData{} }},
	DevProviderFailingPath:                         {key: "provider_failing", title: "openai", nav: navSettings, tab: tabSettingsModels, stylesheet: "connectors.css", live: true, template: providerPage, newData: func() any { return &providerData{} }},
	DevProviderNoListingPath:                       {key: "provider_no_listing", title: "local", nav: navSettings, tab: tabSettingsModels, stylesheet: "connectors.css", template: providerPage, newData: func() any { return &providerData{} }},
	accessPath:                                     {key: "access", title: "Access", nav: navSettings, tab: tabSettingsAccess, template: accessPage, newData: func() any { return &accessData{} }},
	DevStyleGuidePath:                              {key: "styleguide", title: "Style guide", live: true, template: styleGuidePage, newData: newStyleGuideData},
	DevSelfDeniedPath:                              {key: "sessions_denied", title: "Access needed", template: selfDeniedPage, newData: func() any { return &selfDeniedData{} }},
	askFeedbackPath:                                {key: "ask_feedback", title: "Feedback", nav: navAsk, tab: tabAskFeedback, stylesheet: "asks.css", live: true, template: askFeedbackPage, newData: func() any { return &askFeedbackData{} }},
	DevFeedbackEmptyPath:                           {key: "ask_feedback_empty", title: "Feedback", nav: navAsk, tab: tabAskFeedback, stylesheet: "asks.css", live: true, template: askFeedbackPage, newData: func() any { return &askFeedbackData{} }},
	DevAskEmptyPath:                                {key: "ask_sessions_empty", title: "Conversations", nav: navAsk, stylesheet: "asks.css", live: true, template: askSessionsPage, newData: func() any { return &askSessionsData{} }},
	DevAdminDeniedPath:                             {key: "admin_denied", title: "Access needed", template: selfDeniedPage, newData: func() any { return &selfDeniedData{} }},
	DevSessionsEmptyPath:                           {key: "sessions_empty", title: "My sessions", live: true, template: ownerSessionsPage, newData: func() any { return &ownerSessionsData{} }},
	"/admin/connectors":                            {key: "connectors", title: "Connectors", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorsPage, newData: func() any { return &connectorsData{} }},
	"/admin/connectors/slack":                      {key: "connector", title: "Slack", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorPage, newData: func() any { return &connectorData{} }},
	"/admin/connectors/notion":                     {key: "connector_notion", title: "Notion", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorPage, newData: func() any { return &connectorData{} }},
	"/admin/connectors/discord":                    {key: "connector_discord", title: "Discord", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorPage, newData: func() any { return &connectorData{} }},
	"/admin/connectors/markdown":                   {key: "connector_markdown", title: "Markdown folder", nav: "connectors", stylesheet: "connectors.css", template: connectorPage, newData: func() any { return &connectorData{} }},
	connectionsPath + "/grafana":                   {key: "connection", title: "grafana", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectionPage, newData: func() any { return &connectionData{} }},
	connectionsPath + "/new":                       {key: "connection_new", title: "Add a tool connection", nav: "connectors", stylesheet: "connectors.css", live: true, template: newConnectionPage, newData: func() any { return &newConnectionData{} }},
	DevConnectionNewSentryPath:                     {key: "connection_sentry", title: "sentry", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectionPage, newData: func() any { return &connectionData{} }},
	DevConnectionFailingPath:                       {key: "connection_failing", title: "deployments", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectionPage, newData: func() any { return &connectionData{} }},
	"/admin/connectors/pagerduty":                  {key: "connector_pagerduty", title: "PagerDuty", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorPage, newData: func() any { return &connectorData{} }},
	slackChannelsPath:                              {key: "slack_channels", title: "Slack channels", nav: "connectors", stylesheet: "connectors.css", live: true, template: slackChannelsPage, newData: func() any { return &slackChannelsData{} }},
	slackChannelsPath + "/" + DevSlackChannel:      {key: "source_slack", title: "#platform", nav: "connectors", stylesheet: "connectors.css", script: "source_slack.js", live: true, template: slackSourcePage, newData: func() any { return &slackSourceData{} }},
	slackChannelsPath + "/CLEGAL":                  {key: "source_slack_blocked", title: "#legal", nav: "connectors", stylesheet: "connectors.css", script: "source_slack.js", live: true, template: slackSourcePage, newData: func() any { return &slackSourceData{} }},
	operationsPath:                                 {key: "operations", title: "Status", nav: navOperations, tab: tabOperationsStatus, stylesheet: "operations.css", live: true, template: operationsPage, newData: func() any { return &statusData{} }},
	jobsPath:                                       {key: "operations_jobs", title: "Jobs and maintenance", nav: navOperations, tab: tabOperationsJobs, script: "operations.js", live: true, template: jobsPage, newData: func() any { return &jobsData{} }},
	failuresPath:                                   {key: "operations_failures", title: "Failures", nav: navOperations, tab: tabOperationsFailures, stylesheet: "operations.css", live: true, template: failuresPage, newData: func() any { return &failuresData{} }},
	DevOverviewEmptyPath:                           {key: "overview_empty", title: "Overview", nav: navOverview, stylesheet: "overview.css", live: true, template: overviewPage, newData: func() any { return &overviewData{} }},
	DevOperationsEmptyPath:                         {key: "operations_empty", title: "Status", nav: navOperations, tab: tabOperationsStatus, stylesheet: "operations.css", live: true, template: operationsPage, newData: func() any { return &statusData{} }},
	DevJobsEmptyPath:                               {key: "operations_jobs_empty", title: "Jobs and maintenance", nav: navOperations, tab: tabOperationsJobs, script: "operations.js", live: true, template: jobsPage, newData: func() any { return &jobsData{} }},
	DevJobsIdlePath:                                {key: "operations_jobs_idle", title: "Jobs and maintenance", nav: navOperations, tab: tabOperationsJobs, script: "operations.js", live: true, template: jobsPage, newData: func() any { return &jobsData{} }},
	DevNotionSharedPath:                            {key: "connector_notion_shared", title: "Notion", nav: "connectors", stylesheet: "connectors.css", live: true, template: connectorPage, newData: func() any { return &connectorData{} }},
	DevSlackNoTokenPath:                            {key: "slack_channels_no_token", title: "Slack channels", nav: "connectors", stylesheet: "connectors.css", live: true, template: slackChannelsPage, newData: func() any { return &slackChannelsData{} }},
	auditPath:                                      {key: "audit", title: "Audit log", nav: navOperations, tab: tabOperationsAudit, live: true, template: auditPage, newData: func() any { return &auditData{} }},
	"/admin/ask/sessions/" + DevAskFailedSessionID: {key: "ask_session_failed", title: "Summarize the latest incident notes", nav: navAsk, stylesheet: "asks.css", live: true, template: askSessionPage, newData: func() any { return &askSessionData{} }},
	"/admin/prompts":                               {key: "prompts", title: "Prompts", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptsPage, newData: func() any { return &promptsData{} }},
	"/admin/dev/prompts-setup":                     {key: "prompts_setup", title: "Prompts", nav: "prompts", stylesheet: "prompts.css", template: promptsPage, newData: func() any { return &promptsData{} }},
	"/admin/prompts/ask.base":                      {key: "prompt", title: "Role and research", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptPage, newData: func() any { return &promptPageData{} }},
	"/admin/dev/prompt-version":                    {key: "prompt_viewing", title: "Answer rules", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptPage, newData: func() any { return &promptPageData{} }},
	"/admin/dev/prompt-drafted":                    {key: "prompt_drafted", title: "Answers in Slack", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptPage, newData: func() any { return &promptPageData{} }},
	"/admin/prompts/ask.organization":              {key: "prompt_note", title: "About your organization", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptPage, newData: func() any { return &promptPageData{} }},
	"/admin/prompts/draft":                         {key: "prompt_draft", title: "Draft", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptDraftPage, newData: func() any { return &promptDraftData{} }},
	"/admin/dev/prompt-draft-blocked":              {key: "prompt_draft_blocked", title: "Draft", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptDraftPage, newData: func() any { return &promptDraftData{} }},
	"/admin/dev/prompt-draft-empty":                {key: "prompt_draft_empty", title: "Draft", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptDraftPage, newData: func() any { return &promptDraftData{} }},
	"/admin/prompts/shipped":                       {key: "prompt_shipped", title: "Shipped text", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptShippedPage, newData: func() any { return &promptShippedData{} }},
	"/admin/dev/prompt-shipped-same":               {key: "prompt_shipped_same", title: "Shipped text", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptShippedPage, newData: func() any { return &promptShippedData{} }},
	"/admin/prompts/versions":                      {key: "prompt_versions", title: "Version history", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptVersionsPage, newData: func() any { return &promptVersionsData{} }},
	"/admin/prompts/versions/174":                  {key: "prompt_version", title: "Version 174", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptVersionPage, newData: func() any { return &promptVersionData{} }},
	"/admin/prompts/tools":                         {key: "prompt_tools", title: "Tools Ask can use", nav: "prompts", stylesheet: "prompts.css", live: true, template: promptToolsPage, newData: func() any { return &promptToolsData{} }},
	"/admin/prompts/legacy/17":                     {key: "prompt_legacy", title: "Earlier Ask prompt 17", nav: "prompts", stylesheet: "prompts.css", live: true, template: legacyPromptPage, newData: func() any { return &legacyPromptData{} }},
	"/admin/reports":                               {key: "reports", title: "Reports", nav: "reports", stylesheet: "reports.css", live: true, template: reportsPage, newData: func() any { return &reportsData{} }},
	"/admin/reports/" + DevReportID:                {key: "report", title: "Fixture report", nav: "reports", stylesheet: "reports.css", script: "reports.js", live: true, template: reportPage, newData: func() any { return &reportData{} }},
	"/admin/reports/new":                           {key: "report_new", title: "New report", nav: "reports", stylesheet: "reports.css", script: "reports.js", live: true, template: reportFormPage, newData: func() any { return &reportFormData{} }},
	"/admin/reports/" + DevReportID + "/edit":      {key: "report_edit", title: "Fixture report", nav: "reports", stylesheet: "reports.css", script: "reports.js", live: true, template: reportFormPage, newData: func() any { return &reportFormData{} }},
	"/admin/dev/report-new-invalid":                {key: "report_new_invalid", title: "New report", nav: "reports", stylesheet: "reports.css", script: "reports.js", live: true, template: reportFormPage, newData: func() any { return &reportFormData{} }},
	"/admin/dev/reports-no-slack":                  {key: "reports_no_slack", title: "Reports", nav: "reports", stylesheet: "reports.css", live: true, template: reportsPage, newData: func() any { return &reportsData{} }},
	"/admin/dev/reports-waiting":                   {key: "reports_waiting", title: "Reports", nav: "reports", stylesheet: "reports.css", live: true, template: reportsPage, newData: func() any { return &reportsData{} }},
	"/admin/dev/reports-empty":                     {key: "reports_empty", title: "Reports", nav: "reports", stylesheet: "reports.css", live: true, template: reportsPage, newData: func() any { return &reportsData{} }},
	"/admin/dev/report-off":                        {key: "report_off", title: "Fixture report", nav: "reports", stylesheet: "reports.css", script: "reports.js", live: true, template: reportPage, newData: func() any { return &reportData{} }},
}

type devSessionState struct {
	mu        sync.RWMutex
	overrides map[sessioningest.SessionRef]bool
}

func (s *devSessionState) get(session sessioningest.SessionRef, fallback bool) (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if value, ok := s.overrides[session]; ok {
		return value, true
	}
	return fallback, false
}

func (s *devSessionState) set(sessions []sessioningest.SessionRef, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range sessions {
		s.overrides[session] = value
	}
}

// NewDevHandler serves the real embedded administrator templates and assets
// over synthetic on-disk view models. It deliberately skips authentication,
// Postgres, and every mutation dependency; callers should bind it to loopback.
func NewDevHandler(fixturePath string, logger *slog.Logger) (http.Handler, error) {
	abs, err := filepath.Abs(fixturePath)
	if err != nil {
		return nil, fmt.Errorf("adminui ui-dev: resolve fixture: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := loadDevFixture(abs); err != nil {
		return nil, err
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("adminui ui-dev: static fs: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /admin/static/", http.StripPrefix("/admin/static/", http.FileServer(http.FS(staticFS))))
	mux.Handle("GET /sessions/static/", http.StripPrefix("/sessions/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
	})
	sessionState := &devSessionState{overrides: make(map[sessioningest.SessionRef]bool)}
	renderPage := func(w http.ResponseWriter, r *http.Request) {
		route, ok := devRoutes[r.URL.Path]
		if !ok {
			route = devRoute{key: "not_found", title: missingPage.heading, live: true, template: notFoundPage, newData: func() any {
				return &notFoundData{Heading: missingPage.heading, Detail: missingPage.detail, BackHref: missingPage.backHref, BackLabel: missingPage.backLabel}
			}}
		}
		fixture, err := loadDevFixture(abs)
		if err != nil {
			logger.Error("ui-dev fixture reload failed", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := route.newData()
		raw, ok := fixture.Pages[route.key]
		if !ok {
			// The style guide's data is all sample values, built in Go,
			// and the not-found page's is its own words.
			switch data.(type) {
			case *styleGuideData, *notFoundData:
			default:
				http.Error(w, "ui-dev fixture has no page "+route.key, http.StatusInternalServerError)
				return
			}
			raw = json.RawMessage("{}")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(data); err != nil {
			http.Error(w, fmt.Sprintf("decode ui-dev page %s: %v", route.key, err), http.StatusInternalServerError)
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			http.Error(w, fmt.Sprintf("decode ui-dev page %s: %v", route.key, err), http.StatusInternalServerError)
			return
		}
		setDevPageData(data, pageData{
			Title: route.title, CurrentNav: route.nav, CurrentTab: route.tab,
			Principal: auth.Principal{Email: "operator@example.test", Groups: []string{"admins"}}, OrgName: "Example Corp",
			Version: "ui-dev", GrafanaURL: "https://grafana.example.test/d/diderot/diderot",
			Stylesheet: route.stylesheet, Script: route.script,
			SignOutURL: auth.LogoutPath, SignOutToken: "ui-dev",
			InSetup: !route.live, BootstrapAdminEnabled: !route.live,
		})
		// ?notice= and ?error= show a page as it is after an action, on
		// any page that shows the result of one.
		page := reflect.ValueOf(data).Elem()
		for name, message := range map[string]string{
			"Notice": cleanNotice(r.URL.Query().Get("notice")), "Error": cleanNotice(r.URL.Query().Get("error")),
		} {
			if field := page.FieldByName(name); message != "" && field.IsValid() && field.Kind() == reflect.String {
				field.SetString(message)
			}
		}
		templateName := "layout"
		if _, ok := data.(*selfDeniedData); ok {
			templateName = "self_layout"
		}
		if sessions, ok := data.(*ownerSessionsData); ok {
			templateName = "self_layout"
			token, err := issueCSRFTokenCookie(w, r, selfCSRFCookieName, "/sessions", false)
			if err != nil {
				http.Error(w, "ui-dev CSRF token: "+err.Error(), http.StatusInternalServerError)
				return
			}
			sessions.CSRFToken = token
			for i := range sessions.Sessions {
				ref, err := decodeSessionSelection(sessions.Sessions[i].SelectionKey)
				if err != nil {
					http.Error(w, "ui-dev invalid session selection key", http.StatusInternalServerError)
					return
				}
				ownerOnly, overridden := sessionState.get(ref, sessions.Sessions[i].OwnerOnly)
				sessions.Sessions[i].OwnerOnly = ownerOnly
				if overridden {
					sessions.Sessions[i].PrivacyExplicit = true
				}
			}
		}
		var body bytes.Buffer
		if err := route.template.ExecuteTemplate(&body, templateName, data); err != nil {
			logger.Error("ui-dev render failed", "page", route.key, "err", err)
			http.Error(w, "render ui-dev page: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, missing := data.(*notFoundData); missing {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = w.Write(body.Bytes())
	}
	mux.HandleFunc("GET /admin/", renderPage)
	// The sign-in page with every method on, and after a refused sign-in
	// (?failed=1).
	mux.HandleFunc("GET "+signInStylesheetPath, handleSignInStylesheet)
	mountIcons(mux)
	mux.HandleFunc("GET "+auth.LoginPath, func(w http.ResponseWriter, r *http.Request) {
		data := signInData{
			Title: "Sign in", ReturnTo: "/admin/", CSRFToken: "ui-dev",
			OIDC: oidcLoginPath, Bootstrap: true, StaticToken: true,
		}
		if r.URL.Query().Has("failed") {
			data.Error = "Wrong username or password."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := signInPage.ExecuteTemplate(w, "signin", data); err != nil {
			logger.Error("ui-dev render failed", "page", "signin", "err", err)
		}
	})
	mux.HandleFunc("GET /sessions", renderPage)
	mux.HandleFunc("POST /sessions/privacy", func(w http.ResponseWriter, r *http.Request) {
		publicOrigin, ok := devRequestOrigin(r)
		if !ok || !requireFormMutation(w, r, publicOrigin, selfCSRFCookieName) {
			return
		}
		raw := r.Form.Get("owner_only")
		if raw != "true" && raw != "false" {
			http.Error(w, "owner_only must be true or false", http.StatusBadRequest)
			return
		}
		selected := r.Form["session"]
		if message := ownerSelectionProblem(len(selected)); message != "" {
			redirectSelf(w, r, ownerSessionsReturn(r.Form.Get("before")), "error", message)
			return
		}
		refs := make([]sessioningest.SessionRef, 0, len(selected))
		for _, encoded := range selected {
			ref, err := decodeSessionSelection(encoded)
			if err != nil {
				http.Error(w, "invalid session selection", http.StatusBadRequest)
				return
			}
			refs = append(refs, ref)
		}
		sessionState.set(refs, raw == "true")
		http.Redirect(w, r, "/sessions?notice="+url.QueryEscape(fmt.Sprintf("Local preview updated %d selected session(s). Production was not changed.", len(refs))), http.StatusSeeOther)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setBrowserSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			(r.Method != http.MethodPost || r.URL.Path != "/sessions/privacy") {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "ui-dev is read-only", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func devRequestOrigin(r *http.Request) (string, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return canonicalOrigin(&url.URL{Scheme: scheme, Host: r.Host})
}

func loadDevFixture(path string) (*devFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adminui ui-dev: read fixture %q: %w", path, err)
	}
	var fixture devFixture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("adminui ui-dev: parse fixture %q: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("adminui ui-dev: parse fixture %q: trailing JSON: %w", path, err)
	}
	if len(fixture.Pages) == 0 {
		return nil, fmt.Errorf("adminui ui-dev: fixture %q contains no pages", path)
	}
	return &fixture, nil
}

func requireJSONEOF(decoder interface{ Decode(any) error }) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errTrailingJSON
		}
		return err
	}
	return nil
}

func setDevPageData(data any, page pageData) {
	switch value := data.(type) {
	case *overviewData:
		value.pageData = page
	case *askSessionsData:
		value.pageData = page
	case *askSessionData:
		value.pageData = page
	case *askFeedbackData:
		value.pageData = page
	case *slackSourceData:
		value.pageData = page
	case *slackChannelsData:
		value.pageData = page
	case *connectorData:
		value.pageData = page
	case *connectorsData:
		value.pageData = page
	case *connectionData:
		value.pageData = page
	case *newConnectionData:
		value.pageData = page
	case *setupData:
		value.pageData = page
	case *organizationData:
		value.pageData = page
	case *modelsData:
		value.pageData = page
	case *modelEntryData:
		value.pageData = page
	case *providerData:
		value.pageData = page
	case *newProviderData:
		value.pageData = page
	case *accessData:
		value.pageData = page
	case *promptsData:
		value.pageData = page
	case *promptPageData:
		value.pageData = page
	case *promptDraftData:
		value.pageData = page
	case *promptShippedData:
		value.pageData = page
	case *promptVersionsData:
		value.pageData = page
	case *promptVersionData:
		value.pageData = page
	case *promptToolsData:
		value.pageData = page
	case *legacyPromptData:
		value.pageData = page
	case *reportsData:
		value.pageData = page
	case *reportData:
		value.pageData = page
	case *reportFormData:
		value.pageData = page
	case *notFoundData:
		value.pageData = page
	case *statusData:
		value.pageData = page
	case *jobsData:
		value.pageData = page
	case *failuresData:
		value.pageData = page
	case *auditData:
		value.pageData = page
	case *styleGuideData:
		value.pageData = page
	case *ownerSessionsData:
		value.selfPageData = selfPageData{
			Title: page.Title, Principal: page.Principal, Version: page.Version,
			SignOutURL: page.SignOutURL, SignOutToken: page.SignOutToken, SessionsLink: "current",
		}
	case *selfDeniedData:
		// The fixture says whether the refused account may open My sessions.
		value.selfPageData = selfPageData{
			Title: page.Title, Principal: page.Principal, Version: page.Version,
			SignOutURL: page.SignOutURL, SignOutToken: page.SignOutToken, SessionsLink: value.SessionsLink,
		}
	default:
		panic("unsupported ui-dev page data " + strings.TrimPrefix(fmt.Sprintf("%T", data), "*"))
	}
}
