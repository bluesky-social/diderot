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

	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

var (
	providerPage    = mustPage("provider.html", "connector.html")
	newProviderPage = mustPage("provider_new.html", "connector.html")
)

// providersPath is where a model provider's page lives, under its name.
const providersPath = "/admin/settings/providers"

// providerHref is a provider's page.
func providerHref(name string) string { return providersPath + "/" + url.PathEscape(name) }

// providerSummary is one provider's row on the Models page.
type providerSummary struct {
	Name, Kind, Address string
	State               connectorState
	// Models counts the models it offers jobs.
	Models int
	// NoListing marks a provider without a model list.
	NoListing bool
}

// Href is the provider's page.
func (p providerSummary) Href() string { return providerHref(p.Name) }

// providerData is a provider's page.
type providerData struct {
	pageData
	CSRFToken string
	Name      string
	// KindTitle and BaseURL are fixed once the provider is added.
	KindTitle, BaseURL string
	View               settings.View
	State              connectorState
	Notice, Error      string
	// Report is the test shown below the form: the form's own when Draft,
	// otherwise the saved settings' last test. Check is the leader's latest
	// listing of the saved settings, while it is on.
	Report *settings.TestRecord
	Draft  bool
	Check  *settings.TestRecord
	Key    connectorField
	// Offered is Models offered, one ID per line.
	Offered     string
	FieldErrors map[string]string
	// Menu is what the provider offers jobs; Jobs are the jobs whose saved
	// model it serves, and Search is set when it serves the embedding
	// model.
	Menu   modelprovider.Menu
	Jobs   []string
	Search bool
	// ConfirmOff asks the administrator to confirm turning it off, which
	// stops what Depends lists.
	ConfirmOff bool
	// NoKey marks a server that takes no key, which AllowsNoKey says its
	// kind may be. Headers are its extra request headers, one per line, and
	// SecretHeaders those kept as a credential, which are never shown.
	NoKey, AllowsNoKey bool
	Headers            string
	SecretHeaders      connectorField
}

// Depends lists what stops while the provider is off: the jobs it serves,
// and search.
func (d providerData) Depends() []string {
	depends := slices.Clone(d.Jobs)
	if d.Search {
		depends = append(depends, "Search, and Ask, which searches (it serves the embedding model)")
	}
	return depends
}

// providerDependents lists the jobs whose saved model the provider name
// serves, and whether it serves the embedding model.
func (s *Server) providerDependents(ctx context.Context, name string) (jobs []string, search bool) {
	if s.setup == nil {
		return nil, false
	}
	models, err := s.setup.Models(ctx)
	if err != nil {
		return nil, false
	}
	for _, role := range models.Roles {
		if provider, _, _ := modelregistry.ParseRef(role.Saved); provider == name && role.Unavailable == "" {
			jobs = append(jobs, role.Title)
		}
	}
	for _, embedding := range []*settings.Embedding{models.Embedding, models.Filling} {
		if embedding != nil && embedding.Provider == name {
			search = true
		}
	}
	return jobs, search
}

// Price is a listed model's price, in words, or "".
func (d providerData) Price(listed modelregistry.Listed) string {
	return modelPrice(listed.InputPrice, listed.OutputPrice)
}

// ImageInput says whether the listing says a model takes images: yes, no,
// or nothing when it is silent.
func (d providerData) ImageInput(listed modelregistry.Listed) string {
	switch {
	case listed.Images == nil:
		return ""
	case *listed.Images:
		return "yes"
	}
	return "no"
}

// NeverSetUp reports a provider whose settings no save has tested.
func (d providerData) NeverSetUp() bool { return !d.View.Enabled && d.View.LastTest == nil }

// StateText says in a sentence what the state means.
func (d providerData) StateText() string {
	switch {
	case d.State.Reason != "":
		return d.State.Reason
	case !d.View.Enabled:
		return "Jobs can't use its models."
	}
	return "Its models are offered to every job on the Models page."
}

// newProviderData is the page that adds a provider.
type newProviderData struct {
	pageData
	CSRFToken string
	Kinds     []fieldOption
	Kind      string
	Name      string
	BaseURL   string
	// Offered is Models offered, one ID per line.
	Offered string
	// NoKey marks a server that takes no key, and Headers its extra request
	// headers, one per line.
	NoKey   bool
	Headers string
	// FieldErrors marks the form's fields a save refused.
	FieldErrors map[string]string
	Error       string
}

func (s *Server) mountProviderRoutes(mux Mux) {
	mux.Handle("GET "+providersPath+"/new", s.authedBrowser(http.HandlerFunc(s.handleNewProvider)))
	mux.Handle("POST "+providersPath, s.authedBrowser(http.HandlerFunc(s.handleProviderCreate)))
	mux.Handle("GET "+providersPath+"/{name}", s.authedBrowser(http.HandlerFunc(s.handleProvider)))
	mux.Handle("POST "+providersPath+"/{name}", s.authedBrowser(http.HandlerFunc(s.handleProviderSave)))
	mux.Handle("POST "+providersPath+"/{name}/enable", s.authedBrowser(http.HandlerFunc(s.handleProviderSave)))
	mux.Handle("POST "+providersPath+"/{name}/test", s.authedBrowser(http.HandlerFunc(s.handleProviderTest)))
	mux.Handle("POST "+providersPath+"/{name}/disable", s.authedBrowser(http.HandlerFunc(s.handleProviderDisable)))
	mux.Handle("POST "+providersPath+"/{name}/delete", s.authedBrowser(http.HandlerFunc(s.handleProviderDelete)))
}

func (s *Server) requireProviders(w http.ResponseWriter) bool {
	if s.providers == nil {
		http.Error(w, "model providers are unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// providerSummaries lists every provider for the Models page.
func (s *Server) providerSummaries(ctx context.Context) ([]providerSummary, error) {
	if s.providers == nil {
		return nil, nil
	}
	views, err := s.providers.ListFamily(ctx, modelprovider.Prefix)
	if err != nil {
		return nil, err
	}
	checks, err := modelprovider.ReadChecks(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	summaries := make([]providerSummary, 0, len(views))
	for _, view := range views {
		var check *modelprovider.Check
		if c, ok := checks[view.Name]; ok {
			check = &c
		}
		menu := modelprovider.MenuOf(view, check)
		summaries = append(summaries, providerSummary{
			Name: menu.Name, Kind: menu.Kind.Title(), Address: providerAddress(view),
			State: providerState(view, menu), Models: len(menu.Models), NoListing: menu.NoListing,
		})
	}
	return summaries, nil
}

// providerAddress is the host a provider is reached at.
func providerAddress(view settings.View) string {
	raw, _ := view.Settings[modelprovider.FieldBaseURL].(string)
	if parsed, err := url.Parse(raw); err == nil {
		return parsed.Host
	}
	return ""
}

// providerState is a provider's state: off, on, or needs attention while its
// latest listing failed.
func providerState(view settings.View, menu modelprovider.Menu) connectorState {
	if view.Enabled && menu.Error != "" {
		return connectorState{Tone: toneAttention, Word: "needs attention", Reason: menu.Error}
	}
	return stateOf(view, "")
}

// providerAttention is one Overview item for each provider that is on and
// failing.
func (s *Server) providerAttention(ctx context.Context) ([]attentionItem, error) {
	summaries, err := s.providerSummaries(ctx)
	if err != nil {
		return nil, err
	}
	var items []attentionItem
	for _, summary := range summaries {
		if summary.State.Attention() {
			items = append(items, attentionItem{
				Text: "Model provider " + summary.Name + ": " + lowerFirst(summary.State.Reason),
				Href: summary.Href(), LinkText: summary.Name,
			})
		}
	}
	return items, nil
}

func (s *Server) handleNewProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviders(w) {
		return
	}
	// A link may fill the form in, such as the quickstart's for its local
	// embedder.
	query := r.URL.Query()
	s.renderNewProvider(w, r, newProviderData{
		Kind: query.Get("kind"), Name: query.Get("name"), BaseURL: query.Get("base_url"), Offered: query.Get("models"),
	}, http.StatusOK)
}

func (s *Server) renderNewProvider(w http.ResponseWriter, r *http.Request, data newProviderData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.settingsPage(r, "Add a model provider", tabSettingsModels)
	data.Stylesheet = "connectors.css"
	data.CSRFToken = token
	data.Kinds = nil
	for _, kind := range modelregistry.Kinds {
		data.Kinds = append(data.Kinds, fieldOption{Value: string(kind), Label: kind.Title()})
	}
	if !slices.Contains(modelregistry.Kinds, modelregistry.Kind(data.Kind)) {
		data.Kind = string(modelregistry.KindOpenAI)
	}
	s.renderStatus(w, newProviderPage, data, code)
}

// handleProviderCreate adds a provider with its kind and base URL, which
// stay fixed, then saves its key, tests it, and turns it on when the test
// passes.
func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviders(w) || !s.requireMutation(w, r) {
		return
	}
	data := newProviderData{
		Kind: r.Form.Get("kind"), Name: strings.TrimSpace(r.Form.Get("name")),
		BaseURL: strings.TrimSpace(r.Form.Get("base_url")), Offered: r.Form.Get("models"),
		NoKey: r.Form.Get("no_key") == "true", Headers: r.Form.Get("headers"),
	}
	if !slices.Contains(modelregistry.Kinds, modelregistry.Kind(data.Kind)) {
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if err := modelprovider.CheckName(data.Name); err != nil {
		data.Error = "Not added: for the name, " + err.Error() + "."
		s.renderNewProvider(w, r, data, http.StatusBadRequest)
		return
	}
	view, err := s.providers.Create(r.Context(), s.connectorActor(r), modelprovider.Row(data.Name),
		map[string]any{
			modelprovider.FieldKind: data.Kind, modelprovider.FieldBaseURL: data.BaseURL, modelprovider.FieldModels: data.Offered,
			modelprovider.FieldNoKey: data.NoKey, modelprovider.FieldHeaders: data.Headers,
		})
	var fieldErrs settings.FieldErrors
	switch {
	case errors.Is(err, settings.ErrExists):
		data.Error = "Not added: a provider named " + data.Name + " already exists."
		s.renderNewProvider(w, r, data, http.StatusBadRequest)
		return
	case errors.As(err, &fieldErrs):
		data.FieldErrors = fieldErrs
		data.Error = "Not added: fix the marked settings."
		s.renderNewProvider(w, r, data, http.StatusBadRequest)
		return
	case err != nil:
		s.logger.Error("provider create failed", "err", err)
		http.Error(w, "provider create failed", http.StatusInternalServerError)
		return
	}
	secrets := map[string]settings.SecretInput{}
	if key := r.Form.Get("secret." + modelprovider.FieldKey); strings.TrimSpace(key) != "" {
		secrets[modelprovider.FieldKey] = settings.SecretInput{Value: key}
	}
	if headers := r.Form.Get("secret." + modelprovider.FieldSecretHeaders); strings.TrimSpace(headers) != "" {
		secrets[modelprovider.FieldSecretHeaders] = settings.SecretInput{Value: strings.ReplaceAll(headers, "\r\n", "\n")}
	}
	if problem := secretHeadersProblem(secrets); problem != "" {
		data.FieldErrors = map[string]string{modelprovider.FieldSecretHeaders: problem}
		data.Error = "Not added: fix the marked settings."
		s.renderNewProvider(w, r, data, http.StatusBadRequest)
		return
	}
	saved, err := s.providers.Save(r.Context(), s.connectorActor(r), view.Name, view.Version, nil, secrets, true)
	switch {
	case err != nil:
		s.providerFailure(w, r, data.Name, "save", err)
	case saved.Enabled:
		redirectWithResult(w, r, modelsPath, data.Name+" is on. Choose the model for each job below.", false)
	default:
		redirectWithResult(w, r, providerHref(data.Name),
			data.Name+" is added but still off: the test of its settings failed. Fix what it reports, then turn it on.", true)
	}
}

// providerView reads the provider the request names.
func (s *Server) providerView(w http.ResponseWriter, r *http.Request) (settings.View, bool) {
	name := r.PathValue("name")
	if modelprovider.CheckName(name) != nil {
		s.renderNotFound(w, r, missingProvider)
		return settings.View{}, false
	}
	view, err := s.providers.View(r.Context(), modelprovider.Row(name))
	switch {
	case err == nil:
		return view, true
	case errors.Is(err, settings.ErrUnknownConnector):
		s.renderNotFound(w, r, missingProvider)
	default:
		s.logger.Error("provider read failed", "err", err)
		http.Error(w, "provider read failed", http.StatusInternalServerError)
	}
	return settings.View{}, false
}

func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviders(w) {
		return
	}
	view, ok := s.providerView(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	data := savedProvider(view)
	data.Report = view.LastTest
	data.Notice, data.Error = cleanNotice(query.Get("notice")), cleanNotice(query.Get("error"))
	s.renderProvider(w, r, data, http.StatusOK)
}

// savedProvider is the form filled from the saved settings.
func savedProvider(view settings.View) providerData {
	kind, _ := view.Settings[modelprovider.FieldKind].(string)
	baseURL, _ := view.Settings[modelprovider.FieldBaseURL].(string)
	data := providerData{
		View: view, KindTitle: modelregistry.Kind(kind).Title(), BaseURL: baseURL,
		Offered: strings.Join(stringList(view.Settings[modelprovider.FieldModels]), "\n"),
	}
	for _, field := range savedFields(view) {
		switch field.Key {
		case modelprovider.FieldKey:
			data.Key = field
			data.Key.Help = keyHelp(modelregistry.Kind(kind))
		case modelprovider.FieldSecretHeaders:
			data.SecretHeaders = field
		}
	}
	data.NoKey, _ = view.Settings[modelprovider.FieldNoKey].(bool)
	data.AllowsNoKey = modelregistry.Kind(kind).AllowsNoKey()
	data.Headers = strings.Join(stringList(view.Settings[modelprovider.FieldHeaders]), "\n")
	return data
}

// keyHelp says what key a kind of provider takes.
func keyHelp(kind modelregistry.Kind) string {
	if kind == modelregistry.KindAnthropic {
		return "An Anthropic API key. Diderot sends it as x-api-key."
	}
	return "The key the provider accepts, sent as Authorization: Bearer and the key. For a server that takes none, tick the box below instead."
}

// renderProvider fills in the page around data's form and renders it.
func (s *Server) renderProvider(w http.ResponseWriter, r *http.Request, data providerData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.Name = modelprovider.Name(data.View.Name)
	data.pageData = s.settingsPage(r, data.Name, tabSettingsModels)
	data.Stylesheet = "connectors.css"
	data.CSRFToken = token
	checks, err := modelprovider.ReadChecks(r.Context(), s.pool)
	if err != nil {
		s.logger.Error("provider checks failed", "err", err)
		http.Error(w, "provider read failed", http.StatusInternalServerError)
		return
	}
	check, checked := checks[data.View.Name]
	var menuCheck *modelprovider.Check
	if checked {
		menuCheck = &check
	}
	data.Menu = modelprovider.MenuOf(data.View, menuCheck)
	if data.Draft && data.Report != nil {
		draft := data.View
		draft.LastTest = data.Report
		data.Menu = modelprovider.MenuOf(draft, nil)
	}
	data.State = providerState(data.View, modelprovider.MenuOf(data.View, menuCheck))
	if data.View.Enabled && checked && check.Version == data.View.Version {
		data.Check = &check.Test
	}
	data.Jobs, data.Search = s.providerDependents(r.Context(), data.Name)
	data.Key.Error = data.FieldErrors[modelprovider.FieldKey]
	s.renderStatus(w, providerPage, data, code)
}

// providerInput reads the form over view: Models offered and the key the
// form changes, and the page's fields to show again, which never carry an
// entered key. The kind and base URL are not read: they are fixed.
func providerInput(view settings.View, form url.Values) (map[string]any, map[string]settings.SecretInput, providerData) {
	data := savedProvider(view)
	values := map[string]any{}
	if form.Has("field." + modelprovider.FieldModels) {
		data.Offered = form.Get("field." + modelprovider.FieldModels)
		values[modelprovider.FieldModels] = data.Offered
		// The settings form carries these too; an unticked box is absent.
		data.NoKey = form.Get("field."+modelprovider.FieldNoKey) == "true"
		values[modelprovider.FieldNoKey] = data.NoKey
		data.Headers = form.Get("field." + modelprovider.FieldHeaders)
		values[modelprovider.FieldHeaders] = data.Headers
	}
	secrets := map[string]settings.SecretInput{}
	data.SecretHeaders.Remove = form.Get("secret."+modelprovider.FieldSecretHeaders+".remove") == "true"
	switch headers := form.Get("secret." + modelprovider.FieldSecretHeaders); {
	case strings.TrimSpace(headers) != "":
		secrets[modelprovider.FieldSecretHeaders] = settings.SecretInput{Value: strings.ReplaceAll(headers, "\r\n", "\n")}
		data.SecretHeaders.Pasted = true
	case data.SecretHeaders.Remove:
		secrets[modelprovider.FieldSecretHeaders] = settings.SecretInput{Clear: true}
	}
	value := form.Get("secret." + modelprovider.FieldKey)
	data.Key.Remove = form.Get("secret."+modelprovider.FieldKey+".remove") == "true"
	switch {
	case strings.TrimSpace(value) != "":
		secrets[modelprovider.FieldKey] = settings.SecretInput{Value: value}
		data.Key.Pasted = true
	case data.Key.Remove:
		secrets[modelprovider.FieldKey] = settings.SecretInput{Clear: true}
	}
	return values, secrets, data
}

// secretHeadersProblem says what is wrong with secret request headers a
// form carries, by line number and never by their text, or is empty. A
// provider is never saved with headers that can't be read: its requests
// would go without them.
func secretHeadersProblem(secrets map[string]settings.SecretInput) string {
	input, ok := secrets[modelprovider.FieldSecretHeaders]
	if !ok || input.Clear {
		return ""
	}
	if _, err := modelprovider.ParseHeaders(strings.Split(input.Value, "\n")); err != nil {
		return "Enter each header as Name: value: " + err.Error() + "."
	}
	return ""
}

// providerForm reads the provider a mutation names and the form's version.
func (s *Server) providerForm(w http.ResponseWriter, r *http.Request) (settings.View, int64, bool) {
	if !s.requireProviders(w) || !s.requireMutation(w, r) {
		return settings.View{}, 0, false
	}
	view, ok := s.providerView(w, r)
	if !ok {
		return settings.View{}, 0, false
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return settings.View{}, 0, false
	}
	return view, version, true
}

// handleProviderSave saves the form and tests what it saved: Save, or, on
// the /enable path, Turn on, which also turns the provider on when the test
// passes.
func (s *Server) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.providerForm(w, r)
	if !ok {
		return
	}
	enable := strings.HasSuffix(r.URL.Path, "/enable")
	values, secrets, data := providerInput(view, r.Form)
	if problem := secretHeadersProblem(secrets); problem != "" {
		data.FieldErrors, data.Report = map[string]string{modelprovider.FieldSecretHeaders: problem}, view.LastTest
		data.Error = "Not saved: fix the marked settings."
		s.renderProvider(w, r, data, http.StatusBadRequest)
		return
	}
	// Removing the key of a provider that is on stops everything it serves,
	// and leaves the server unable to run its saved models: turn it off
	// first.
	if secrets[modelprovider.FieldKey].Clear && view.Enabled {
		if jobs, search := s.providerDependents(r.Context(), modelprovider.Name(view.Name)); len(jobs) > 0 || search {
			data.Jobs, data.Search = jobs, search
			data.Report = view.LastTest
			data.Error = "Not saved: " + strings.Join(data.Depends(), ", ") + " use this provider and would stop without its key. Turn the provider off first, or paste the new key in place of the old one."
			s.renderProvider(w, r, data, http.StatusUnprocessableEntity)
			return
		}
	}
	saved, err := s.providers.Save(r.Context(), s.connectorActor(r), view.Name, version, values, secrets, enable)
	name := modelprovider.Name(view.Name)
	var fieldErrs settings.FieldErrors
	switch {
	case errors.As(err, &fieldErrs):
		data.FieldErrors, data.Report = fieldErrs, view.LastTest
		data.Error = "Not saved: fix the marked settings."
		s.renderProvider(w, r, data, http.StatusBadRequest)
	case err != nil:
		s.providerFailure(w, r, name, "save", err)
	case saved.LastTest != nil && saved.LastTest.Passed:
		notice := "Saved, and the test passed."
		switch {
		case enable && !view.Enabled:
			notice = name + " is on. Its models are offered to every job on the Models page."
		case !saved.Enabled:
			notice = "Saved, and the test passed. Turn " + name + " on when you are ready."
		}
		redirectWithResult(w, r, providerHref(name), notice, false)
	default:
		data := savedProvider(saved)
		data.Report = saved.LastTest
		data.Error = "Saved, but the test failed. Fix what it reports and save again."
		if enable && !view.Enabled {
			data.Error = name + " is still off: the test of its settings failed. Fix what the test reports, then turn it on again."
		}
		s.renderProvider(w, r, data, http.StatusOK)
	}
}

func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	view, _, ok := s.providerForm(w, r)
	if !ok {
		return
	}
	values, secrets, data := providerInput(view, r.Form)
	record, err := s.providers.Test(r.Context(), s.connectorActor(r), view.Name, &settings.Draft{Settings: values, Secrets: secrets})
	var fieldErrs settings.FieldErrors
	switch {
	case err == nil:
		data.Report, data.Draft = &record, true
		if record.Passed {
			data.Notice = "The test passed. Save to keep these settings."
		} else {
			data.Error = "The test failed. Fix what it reports and test again."
		}
		if data.Key.Pasted {
			data.FieldErrors = map[string]string{modelprovider.FieldKey: "Paste it again to save it."}
		}
		s.renderProvider(w, r, data, http.StatusOK)
	case errors.As(err, &fieldErrs):
		data.FieldErrors, data.Report = fieldErrs, view.LastTest
		data.Error = "Not tested: fix the marked settings."
		s.renderProvider(w, r, data, http.StatusBadRequest)
	default:
		s.providerFailure(w, r, modelprovider.Name(view.Name), "test", err)
	}
}

func (s *Server) handleProviderDisable(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.providerForm(w, r)
	if !ok {
		return
	}
	name := modelprovider.Name(view.Name)
	// Turning it off stops every job whose model it serves, and search:
	// say which, and ask.
	if jobs, search := s.providerDependents(r.Context(), name); (len(jobs) > 0 || search) && r.Form.Get("confirm") != "off" {
		data := savedProvider(view)
		data.Report, data.ConfirmOff = view.LastTest, true
		s.renderProvider(w, r, data, http.StatusUnprocessableEntity)
		return
	}
	if _, err := s.providers.Disable(r.Context(), s.connectorActor(r), view.Name, version); err != nil {
		s.providerFailure(w, r, name, "disable", err)
		return
	}
	redirectWithResult(w, r, providerHref(name), name+" is off. Jobs whose model it serves stop until it is on again or they are given another model; its settings are kept.", false)
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.providerForm(w, r)
	if !ok {
		return
	}
	name := modelprovider.Name(view.Name)
	if r.Form.Get("confirm") != "remove" {
		redirectWithResult(w, r, providerHref(name), "Nothing was removed: tick the box to confirm.", true)
		return
	}
	// A provider something still uses is not removed: the saved models
	// would name a provider that does not exist.
	if jobs, search := s.providerDependents(r.Context(), name); len(jobs) > 0 || search {
		depends := providerData{Jobs: jobs, Search: search}.Depends()
		redirectWithResult(w, r, providerHref(name), "Nothing was removed: "+strings.Join(depends, ", ")+
			" still use this provider. Give them another provider's model on the Models page first. To stop them for now, turn the provider off instead.", true)
		return
	}
	if err := s.providers.Delete(r.Context(), s.connectorActor(r), view.Name, version); err != nil {
		s.providerFailure(w, r, name, "remove", err)
		return
	}
	redirectWithResult(w, r, modelsPath, name+" is removed, with its saved key.", false)
}

// providerFailure turns an expected error into a message on the page.
func (s *Server) providerFailure(w http.ResponseWriter, r *http.Request, name, action string, err error) {
	switch {
	case errors.Is(err, settings.ErrConflict):
		redirectWithResult(w, r, providerHref(name), "The settings changed since this page loaded. Check them and try again.", true)
	case errors.Is(err, settings.ErrInvalid):
		redirectWithResult(w, r, providerHref(name), strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": "), true)
	default:
		s.logger.Error("provider action failed", "provider", name, "action", action, "err", err)
		http.Error(w, "provider action failed", http.StatusInternalServerError)
	}
}

// modelPrice is a listed price per million tokens, in words.
func modelPrice(in, out *float64) string {
	if in == nil || out == nil {
		return ""
	}
	return fmt.Sprintf("$%s in, $%s out per million tokens", trimPrice(*in), trimPrice(*out))
}

func trimPrice(price float64) string {
	text := strconv.FormatFloat(price, 'f', 3, 64)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "" {
		return "0"
	}
	return text
}
