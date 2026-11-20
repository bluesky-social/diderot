package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

// The Settings section: the setup checklist, which is its landing page, and
// the Organization, Models and Access tabs.
var (
	setupPage        = mustPage("setup.html")
	organizationPage = mustPage("organization.html", "setup_progress.html")
	modelsPage       = mustPage("models.html", "setup_progress.html")
	accessPage       = mustPage("access.html", "setup_progress.html")
)

type setupData struct {
	pageData
	Checklist setup.Checklist
	CSRFToken string
	Notice    string
	Error     string
	// SignedInAsBootstrap withholds the turn-off button: only another
	// sign-in proves the install keeps an administrator.
	SignedInAsBootstrap bool
	// OIDCSignIn and TokenSignIn are the other ways to sign in that this
	// deployment offers, which the bootstrap admin step points to.
	OIDCSignIn  bool
	TokenSignIn bool
}

// setupStep is a required step as the checklist draws it.
type setupStep struct {
	setup.Step
	// Current marks the first step that is not done: the one to do now.
	Current bool
	// Action labels the link to the page where the step is done.
	Action string
}

// setupStepActions label each required step's link. The bootstrap admin step
// is done on the checklist itself.
var setupStepActions = map[string]string{
	setup.StepOrganization: "Name your organization",
	setup.StepModels:       "Set up models",
	setup.StepSources:      "Connect a source",
	setup.StepPrompts:      "Review the prompts",
	setup.StepAccess:       "Review access",
}

// Live reports whether Ask is open to everyone.
func (d setupData) Live() bool { return d.Checklist.State == settings.StateLive }

// Required is the required steps in order, the first unfinished one marked.
func (d setupData) Required() []setupStep {
	var steps []setupStep
	current := false
	for _, step := range d.Checklist.Steps {
		if !step.Required {
			continue
		}
		view := setupStep{Step: step, Action: setupStepActions[step.ID]}
		if !step.Done && !current {
			view.Current, current = true, true
		}
		steps = append(steps, view)
	}
	return steps
}

// Remaining counts the required steps left.
func (d setupData) Remaining() int {
	remaining := 0
	for _, step := range d.Checklist.Steps {
		if step.Required && !step.Done {
			remaining++
		}
	}
	return remaining
}

// Optional is the steps nothing requires.
func (d setupData) Optional() []setup.Step {
	var steps []setup.Step
	for _, step := range d.Checklist.Steps {
		if !step.Required {
			steps = append(steps, step)
		}
	}
	return steps
}

// setupProgress is the way on that a step's own page offers while the
// install is in setup: back to the checklist, or on to the next step.
type setupProgress struct {
	// StepDone reports whether this page's step is done.
	StepDone bool
	Done     int
	Total    int
	// NextTitle and NextHref are the next unfinished step after this one;
	// empty when none is left, and the checklist's Go live is next.
	NextTitle string
	NextHref  string
}

// setupProgress is nil once the install is live.
func (s *Server) setupProgress(ctx context.Context, stepID string) *setupProgress {
	if s.setup.Org().Live() {
		return nil
	}
	checklist, err := s.setup.Checklist(ctx)
	if err != nil {
		s.logger.Error("setup checklist failed", "err", err)
		return nil
	}
	return newSetupProgress(checklist, stepID)
}

func newSetupProgress(checklist setup.Checklist, stepID string) *setupProgress {
	progress := &setupProgress{}
	for _, step := range checklist.Steps {
		if !step.Required {
			continue
		}
		progress.Total++
		switch {
		case step.ID == stepID:
			progress.StepDone = step.Done
		case !step.Done && progress.NextHref == "":
			progress.NextTitle, progress.NextHref = step.Title, step.Link
		}
		if step.Done {
			progress.Done++
		}
	}
	return progress
}

type organizationData struct {
	pageData
	Org       settings.OrgRow
	CSRFToken string
	Notice    string
	// Error is a rejected save; Org then carries the submitted values so
	// the form keeps what was typed.
	Error   string
	MaxName int
	MaxDesc int
	Setup   *setupProgress
}

type modelsData struct {
	pageData
	Models setup.ModelsView
	Test   *settings.ModelsTest
	// Providers are the model providers, each job's models come from.
	Providers []providerSummary
	// OrgVersion is the org row version the test posts back; Models.Version
	// the models row's, which a save posts back. Version is the layout's
	// build version.
	OrgVersion int64
	// Efforts are the reasoning efforts a save accepts, besides the
	// model's default; MaxToolTurns is the most tool turns it accepts.
	Efforts      []string
	MaxToolTurns int
	// ServiceTiers are the service tiers a save accepts, besides the
	// provider's default; Limits is Ask's other limits as the form shows
	// them, and Min and MaxOutputTokens the range a reply's length takes.
	ServiceTiers                     []string
	Limits                           askLimitsForm
	MinOutputTokens, MaxOutputTokens int
	CSRFToken                        string
	Notice                           string
	// Error is a rejected save; Models then carries the submitted choices
	// so the form keeps them.
	Error string
	Setup *setupProgress
	// Reembed asks the administrator to confirm an embedding choice that
	// embeds every chunk again; EmbeddingRef and Adopt are what they chose.
	Reembed      bool
	EmbeddingRef string
	Adopt        string
	// Spend says, by job, what the job's recorded use over the last
	// spendWindowDays came to; only jobs whose use is recorded have one.
	Spend map[string]string
}

// spendWindowDays is how far back the Models page adds up a job's use.
const spendWindowDays = 30

// jobSpend adds up what is recorded of each job's use: Ask's model calls
// with its present model, priced at what is published for it, and the cost
// document extraction records per page. Other jobs record no use.
func (s *Server) jobSpend(ctx context.Context, models setup.ModelsView) map[string]string {
	spend := map[string]string{}
	if s.pool == nil {
		return spend
	}
	ask := models.Role(string(modelregistry.RoleAsk))
	if _, id, ok := modelregistry.ParseRef(ask.Model); ok {
		var calls, in, out int64
		err := s.pool.QueryRow(ctx, `
			SELECT count(*), COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0)
			  FROM ask_inference_calls
			 WHERE model_id = $1 AND started_at >= now() - make_interval(days => $2)`, id, spendWindowDays).Scan(&calls, &in, &out)
		if err == nil && calls > 0 {
			line := fmt.Sprintf("In the last %d days it read %s tokens and wrote %s over %s calls", spendWindowDays, formatCount(in), formatCount(out), formatCount(calls))
			if option, ok := ask.Option(ask.Model); ok && option.InputPrice != nil && option.OutputPrice != nil {
				line += ": about " + dollars((float64(in)**option.InputPrice+float64(out)**option.OutputPrice)/1e6) +
					" at its published price, before any discount for input read from a cache"
			}
			spend[ask.Key] = line + "."
		}
	}
	var pages int64
	var cost float64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(cost_usd), 0)::float8 FROM event_document_extract_parts
		 WHERE created_at >= now() - make_interval(days => $1)`, spendWindowDays).Scan(&pages, &cost)
	if err == nil && pages > 0 {
		spend[string(modelregistry.RoleDocumentExtraction)] = fmt.Sprintf("In the last %d days it read %s pages, recorded as %s.", spendWindowDays, formatCount(pages), dollars(cost))
	}
	return spend
}

// OptionGroups are a role's options by provider, for the form's select.
func (d modelsData) OptionGroups(options []setup.ModelOption) []optionGroup {
	var groups []optionGroup
	for _, option := range options {
		if len(groups) == 0 || groups[len(groups)-1].Provider != option.Provider {
			groups = append(groups, optionGroup{Provider: option.Provider})
		}
		groups[len(groups)-1].Options = append(groups[len(groups)-1].Options, option)
	}
	return groups
}

// optionGroup is one provider's options.
type optionGroup struct {
	Provider string
	Options  []setup.ModelOption
}

// Price is what a role's model costs, as its provider lists it, or "".
func (d modelsData) Price(role setup.RoleView) string {
	option, ok := role.Option(role.Model)
	if !ok {
		return ""
	}
	return modelPrice(option.InputPrice, option.OutputPrice)
}

// Available is the jobs this deployment can run, which the form offers.
func (d modelsData) Available() []setup.RoleView {
	var roles []setup.RoleView
	for _, role := range d.Models.Roles {
		if role.Unavailable == "" {
			roles = append(roles, role)
		}
	}
	return roles
}

// unavailableJobs is the jobs one missing deployment setting turns off.
type unavailableJobs struct {
	Titles []string
	Reason string
}

// Unavailable groups the jobs this deployment cannot run by what would turn
// them on, so each reason reads once.
func (d modelsData) Unavailable() []unavailableJobs {
	var groups []unavailableJobs
	for _, role := range d.Models.Roles {
		if role.Unavailable == "" {
			continue
		}
		index := slices.IndexFunc(groups, func(group unavailableJobs) bool { return group.Reason == role.Unavailable })
		if index < 0 {
			groups = append(groups, unavailableJobs{Reason: role.Unavailable})
			index = len(groups) - 1
		}
		groups[index].Titles = append(groups[index].Titles, role.Title)
	}
	return groups
}

type accessData struct {
	pageData
	Summary setup.AccessSummary
	// SlackOn says the Slack connector is on, so Ask can answer in Slack.
	SlackOn    bool
	OrgVersion int64
	CSRFToken  string
	Notice     string
	Error      string
	Setup      *setupProgress
}

// APISignIn says how API and MCP callers sign in, in words; the deployment
// sets it with DIDEROT_AUTH_MODE.
func (d accessData) APISignIn() string {
	switch d.Summary.AuthMode {
	case "static":
		return "An API token"
	case "oidc":
		return "Your identity provider (OIDC)"
	case "none":
		return "No sign-in: anyone who can reach the server"
	}
	return d.Summary.AuthMode
}

// PrivateChannels counts the Ask-enabled Slack channels that are private.
func (d accessData) PrivateChannels() int {
	count := 0
	for _, channel := range d.Summary.Channels {
		if channel.Private {
			count++
		}
	}
	return count
}

// ExternalChannels counts the Ask-enabled Slack channels where members of
// other organizations may ask.
func (d accessData) ExternalChannels() int {
	count := 0
	for _, channel := range d.Summary.Channels {
		if channel.ExternalUsers {
			count++
		}
	}
	return count
}

func (s *Server) mountSetupRoutes(mux Mux) {
	mux.Handle("GET "+settingsPath, s.authedBrowser(http.HandlerFunc(s.handleSetup)))
	mux.Handle("POST "+settingsPath+"/actions/{action}", s.authedBrowser(http.HandlerFunc(s.handleSetupAction)))
	mux.Handle("GET "+organizationPath, s.authedBrowser(http.HandlerFunc(s.handleOrganization)))
	mux.Handle("POST "+organizationPath, s.authedBrowser(http.HandlerFunc(s.handleOrganizationSave)))
	mux.Handle("GET "+modelsPath, s.authedBrowser(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST "+modelsPath, s.authedBrowser(http.HandlerFunc(s.handleModelsSave)))
	mux.Handle("POST "+modelsPath+"/embedding", s.authedBrowser(http.HandlerFunc(s.handleEmbeddingSave)))
	mux.Handle("GET "+modelPath, s.authedBrowser(http.HandlerFunc(s.handleModelEntry)))
	mux.Handle("POST "+modelPath, s.authedBrowser(http.HandlerFunc(s.handleModelEntrySave)))
	mux.Handle("POST "+modelPath+"/remove", s.authedBrowser(http.HandlerFunc(s.handleModelEntryRemove)))
	mux.Handle("POST "+modelPath+"/match", s.authedBrowser(http.HandlerFunc(s.handleModelMatch)))
	mux.Handle("POST "+modelPath+"/published", s.authedBrowser(http.HandlerFunc(s.handleModelPublished)))
	mux.Handle("POST "+modelsPath+"/facts", s.authedBrowser(http.HandlerFunc(s.handleModelFacts)))
	mux.Handle("GET "+accessPath, s.authedBrowser(http.HandlerFunc(s.handleAccess)))
}

// requireSetup answers 503 when the process has no setup service.
func (s *Server) requireSetup(w http.ResponseWriter) bool {
	if s.setup == nil {
		http.Error(w, "setup is unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// settingsPage is pageData for a tab of the Settings section.
func (s *Server) settingsPage(r *http.Request, title, tab string) pageData {
	page := s.page(r, title, navSettings)
	page.CurrentTab = tab
	return page
}

// redirectWithResult sends the browser back to path after an action: with
// what it did, or, for one that was refused, why.
func redirectWithResult(w http.ResponseWriter, r *http.Request, path, message string, refused bool) {
	key := "notice"
	if refused {
		key = "error"
	}
	http.Redirect(w, r, path+"?"+key+"="+url.QueryEscape(message), http.StatusSeeOther)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	checklist, err := s.setup.Checklist(r.Context())
	if err != nil {
		s.logger.Error("setup checklist failed", "err", err)
		http.Error(w, "setup checklist failed", http.StatusInternalServerError)
		return
	}
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	principal, _ := PrincipalFromContext(r.Context())
	data := setupData{
		pageData: s.settingsPage(r, "Setup checklist", tabSettingsSetup), Checklist: checklist, CSRFToken: token,
		Notice: cleanNotice(r.URL.Query().Get("notice")), Error: cleanNotice(r.URL.Query().Get("error")),
		SignedInAsBootstrap: principal.Bootstrap,
	}
	if s.login != nil {
		data.OIDCSignIn, data.TokenSignIn = s.login.OIDCEnabled(), s.login.StaticTokensEnabled()
	}
	s.render(w, setupPage, data)
}

// setupActionReturn is the page each action goes back to.
var setupActionReturn = map[string]string{
	setup.ActionTestModels:        modelsPath,
	setup.ActionAcknowledgeAccess: accessPath,
}

var setupActionDone = map[string]string{
	setup.ActionAcknowledgeAccess:     "Access marked as reviewed.",
	setup.ActionAcceptExamplePrompts:  "Using the example prompts as-is.",
	setup.ActionTestModels:            "Model test finished.",
	setup.ActionDisableBootstrapAdmin: "The bootstrap admin is off.",
	setup.ActionGoLive:                "Diderot is live: everyone who can sign in can use Ask.",
	setup.ActionBackToSetup:           "Diderot is back in setup: only administrators can use Ask.",
}

func (s *Server) handleSetupAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	action := r.PathValue("action")
	if !slices.Contains(setup.Actions, action) {
		http.NotFound(w, r)
		return
	}
	back := setupActionReturn[action]
	if back == "" {
		back = settingsPath
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	_, err = s.setup.Do(r.Context(), s.setupActor(r), version, action)
	notice, ok := setupNotice(err)
	if !ok {
		s.logger.Error("setup action failed", "action", action, "err", err)
		http.Error(w, "setup action failed", http.StatusInternalServerError)
		return
	}
	if err == nil {
		notice = setupActionDone[action]
	}
	redirectWithResult(w, r, back, notice, err != nil)
}

func (s *Server) setupActor(r *http.Request) setup.Actor {
	principal, _ := PrincipalFromContext(r.Context())
	return setup.Actor{
		Email: principal.Email, RequestID: strings.TrimSpace(r.Header.Get("X-Request-ID")),
		Bootstrap: principal.Bootstrap,
	}
}

// setupNotice turns an expected setup error into the notice the page shows;
// ok is false for an unexpected one.
func setupNotice(err error) (string, bool) {
	switch {
	case err == nil:
		return "", true
	case errors.Is(err, settings.ErrConflict):
		return "Someone changed this while the page was open. Check the page and try again.", true
	case errors.Is(err, setup.ErrNotReady):
		return "Finish every required step before going live.", true
	case errors.Is(err, setup.ErrBootstrapSession):
		return "Sign in another way (your identity provider or an API token) to turn off the bootstrap admin.", true
	case errors.Is(err, settings.ErrInvalid):
		return strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": "), true
	}
	return "", false
}

func (s *Server) handleOrganization(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	row, err := s.setup.ReadOrg(r.Context())
	if err != nil {
		s.logger.Error("organization read failed", "err", err)
		http.Error(w, "organization read failed", http.StatusInternalServerError)
		return
	}
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	s.render(w, organizationPage, organizationData{
		pageData: s.settingsPage(r, "Organization", tabSettingsOrganization), Org: row, CSRFToken: token,
		Notice: cleanNotice(r.URL.Query().Get("notice")), Error: cleanNotice(r.URL.Query().Get("error")),
		MaxName: setup.MaxOrgNameRunes, MaxDesc: setup.MaxOrgDescriptionRunes,
		Setup: s.setupProgress(r.Context(), setup.StepOrganization),
	})
}

// renderOrganizationError re-renders the form with the rejected values and
// the reason, instead of redirecting and losing what was typed.
func (s *Server) renderOrganizationError(w http.ResponseWriter, r *http.Request, version int64, err error) {
	row, readErr := s.setup.ReadOrg(r.Context())
	if readErr != nil {
		s.logger.Error("organization read failed", "err", readErr)
		http.Error(w, "organization read failed", http.StatusInternalServerError)
		return
	}
	token, tokenErr := issueCSRFToken(w, r)
	if tokenErr != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	row.Name, row.Description, row.DashboardURL = r.Form.Get("name"), r.Form.Get("description"), r.Form.Get("dashboard")
	row.Version = version
	message, _ := setupNotice(err)
	s.renderStatus(w, organizationPage, organizationData{
		pageData: s.settingsPage(r, "Organization", tabSettingsOrganization), Org: row, CSRFToken: token,
		Error:   message,
		MaxName: setup.MaxOrgNameRunes, MaxDesc: setup.MaxOrgDescriptionRunes,
		Setup: s.setupProgress(r.Context(), setup.StepOrganization),
	}, http.StatusUnprocessableEntity)
}

func (s *Server) handleOrganizationSave(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	_, err = s.setup.SaveOrg(r.Context(), s.setupActor(r), version, r.Form.Get("name"), r.Form.Get("description"), r.Form.Get("dashboard"))
	if errors.Is(err, settings.ErrInvalid) {
		s.renderOrganizationError(w, r, version, err)
		return
	}
	notice, ok := setupNotice(err)
	if !ok {
		s.logger.Error("organization save failed", "err", err)
		http.Error(w, "organization save failed", http.StatusInternalServerError)
		return
	}
	if err == nil {
		notice = "Organization saved."
	}
	redirectWithResult(w, r, organizationPath, notice, err != nil)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	s.renderModels(w, r, http.StatusOK, nil, "")
}

// renderModels renders the Models page. A rejected save passes the form it
// posted, and its reason, so the page keeps the submitted choices.
func (s *Server) renderModels(w http.ResponseWriter, r *http.Request, status int, posted *modelsForm, message string) {
	s.renderModelsWith(w, r, status, posted, message, nil)
}

// renderModelsWith renders the Models page, with extra changing what the
// page shows before it renders.
func (s *Server) renderModelsWith(w http.ResponseWriter, r *http.Request, status int, posted *modelsForm, message string, extra func(*modelsData)) {
	row, err := s.setup.ReadOrg(r.Context())
	if err != nil {
		s.logger.Error("models read failed", "err", err)
		http.Error(w, "models read failed", http.StatusInternalServerError)
		return
	}
	models, err := s.setup.Models(r.Context())
	if err != nil {
		s.logger.Error("models read failed", "err", err)
		http.Error(w, "models read failed", http.StatusInternalServerError)
		return
	}
	if posted != nil {
		posted.apply(&models)
	}
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	if message == "" {
		message = cleanNotice(r.URL.Query().Get("error"))
	}
	providers, err := s.providerSummaries(r.Context())
	if err != nil {
		s.logger.Error("providers read failed", "err", err)
		http.Error(w, "models read failed", http.StatusInternalServerError)
		return
	}
	data := modelsData{
		pageData: s.settingsPage(r, "Models", tabSettingsModels), Models: models, OrgVersion: row.Version,
		Providers: providers,
		Test:      row.ModelsTest, Efforts: modelregistry.ReasoningEfforts, MaxToolTurns: settings.MaxAskToolTurns,
		CSRFToken: token, Notice: cleanNotice(r.URL.Query().Get("notice")), Error: message,
		Setup: s.setupProgress(r.Context(), setup.StepModels), EmbeddingRef: models.EmbeddingRef(),
		Spend: s.jobSpend(r.Context(), models),

		ServiceTiers: modelregistry.ServiceTiers, Limits: askLimitsFields(models.AskLimits),
		MinOutputTokens: settings.MinAskMaxOutputTokens, MaxOutputTokens: settings.MaxAskMaxOutputTokens,
	}
	if posted != nil && posted.limits != nil {
		data.Limits = *posted.limits
	}
	// The limits most visits leave alone open by themselves once one is
	// changed, or a save of them is refused.
	data.Limits.Open = posted != nil || models.AskLimits != (settings.Models{}).AskLimits()
	if extra != nil {
		extra(&data)
	}
	s.renderStatus(w, modelsPage, data, status)
}

// modelsForm is a posted Models page.
type modelsForm struct {
	version int64
	update  setup.ModelsUpdate
	// turns is the tool turns as typed, which may not be a number.
	turns string
	// limits are Ask's other limits as typed, nil when the form had none.
	limits *askLimitsForm
}

// askLimitsForm is Ask's limits beside its effort and tool turns as the
// Models page shows them and as they are typed: a length of time in words,
// and a number that may not be one.
type askLimitsForm struct {
	ServiceTier, MaxOutputTokens                                        string
	ModelTimeout, TurnDeadline, InvestigatorDeadline, InvestigatorGrace string
	// Open shows them without the administrator opening them.
	Open bool
}

func askLimitsFields(limits settings.AskLimits) askLimitsForm {
	return askLimitsForm{
		ServiceTier: limits.ServiceTier, MaxOutputTokens: strconv.Itoa(limits.MaxOutputTokens),
		ModelTimeout: settings.FormatDuration(limits.ModelTimeout), TurnDeadline: settings.FormatDuration(limits.TurnDeadline),
		InvestigatorDeadline: settings.FormatDuration(limits.InvestigatorDeadline),
		InvestigatorGrace:    settings.FormatDuration(limits.InvestigatorGrace),
	}
}

// limits reads what was typed. A value that can't be read becomes zero,
// which is out of range, so the save refuses it and names the field.
func (f askLimitsForm) limits() settings.AskLimits {
	tokens, _ := strconv.Atoi(f.MaxOutputTokens)
	length := func(text string) time.Duration {
		d, _ := settings.ParseDuration(text)
		return d
	}
	return settings.AskLimits{
		ServiceTier: f.ServiceTier, MaxOutputTokens: tokens,
		ModelTimeout: length(f.ModelTimeout), TurnDeadline: length(f.TurnDeadline),
		InvestigatorDeadline: length(f.InvestigatorDeadline), InvestigatorGrace: length(f.InvestigatorGrace),
	}
}

// parseModelsForm reads the page's form: a role.<key> select per role that
// is on, and Ask's limits while Ask is.
func parseModelsForm(r *http.Request) (*modelsForm, error) {
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		return nil, errors.New("version is required")
	}
	form := &modelsForm{version: version, update: setup.ModelsUpdate{Roles: map[string]string{}}}
	for key := range r.Form {
		if role, ok := strings.CutPrefix(key, "role."); ok {
			form.update.Roles[role] = r.Form.Get(key)
		}
	}
	if r.Form.Has("ask_reasoning_effort") {
		effort := r.Form.Get("ask_reasoning_effort")
		form.update.AskReasoningEffort = &effort
	}
	if r.Form.Has("ask_max_tool_turns") {
		form.turns = strings.TrimSpace(r.Form.Get("ask_max_tool_turns"))
		turns, err := strconv.Atoi(form.turns)
		if err != nil {
			turns = 0 // out of range: the save refuses it
		}
		form.update.AskMaxToolTurns = &turns
	}
	if r.Form.Has("ask_model_timeout") {
		field := func(name string) string { return strings.TrimSpace(r.Form.Get(name)) }
		form.limits = &askLimitsForm{
			ServiceTier: field("ask_service_tier"), MaxOutputTokens: field("ask_max_output_tokens"),
			ModelTimeout: field("ask_model_timeout"), TurnDeadline: field("ask_turn_deadline"),
			InvestigatorDeadline: field("ask_investigator_deadline"), InvestigatorGrace: field("ask_investigator_grace"),
		}
		limits := form.limits.limits()
		form.update.AskLimits = &limits
	}
	return form, nil
}

// apply shows the form's choices on view.
func (f *modelsForm) apply(view *setup.ModelsView) {
	view.Version = f.version
	for index := range view.Roles {
		if model, ok := f.update.Roles[view.Roles[index].Key]; ok {
			view.Roles[index].Saved, view.Roles[index].Warning = model, ""
		}
	}
	if f.update.AskReasoningEffort != nil {
		view.AskReasoningEffort, view.AskReasoningEffortWarning = *f.update.AskReasoningEffort, ""
	}
	if turns, err := strconv.Atoi(f.turns); err == nil {
		view.AskMaxToolTurns = turns
	}
}

func (s *Server) handleModelsSave(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	form, err := parseModelsForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = s.setup.SaveModels(r.Context(), s.setupActor(r), form.version, form.update)
	var failed *setup.ModelTestError
	switch {
	case errors.As(err, &failed):
		s.renderModels(w, r, http.StatusUnprocessableEntity, form, failed.Error())
		return
	case errors.Is(err, settings.ErrInvalid):
		message, _ := setupNotice(err)
		s.renderModels(w, r, http.StatusUnprocessableEntity, form, message)
		return
	}
	notice, ok := setupNotice(err)
	if !ok {
		s.logger.Error("models save failed", "err", err)
		http.Error(w, "models save failed", http.StatusInternalServerError)
		return
	}
	if err == nil {
		notice = "Models saved."
	}
	redirectWithResult(w, r, modelsPath, notice, err != nil)
}

func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	row, err := s.setup.ReadOrg(r.Context())
	if err != nil {
		s.logger.Error("access read failed", "err", err)
		http.Error(w, "access read failed", http.StatusInternalServerError)
		return
	}
	summary, err := s.setup.Access(r.Context(), row.Org)
	if err != nil {
		s.logger.Error("access summary failed", "err", err)
		http.Error(w, "access summary failed", http.StatusInternalServerError)
		return
	}
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	slackOn, err := s.sourceOn(r.Context(), slackEventSource)
	if err != nil {
		s.logger.Error("access summary failed", "err", err)
		http.Error(w, "access summary failed", http.StatusInternalServerError)
		return
	}
	s.render(w, accessPage, accessData{
		pageData: s.settingsPage(r, "Access", tabSettingsAccess), Summary: summary, OrgVersion: row.Version, SlackOn: slackOn,
		CSRFToken: token, Notice: cleanNotice(r.URL.Query().Get("notice")), Error: cleanNotice(r.URL.Query().Get("error")),
		Setup: s.setupProgress(r.Context(), setup.StepAccess),
	})
}
