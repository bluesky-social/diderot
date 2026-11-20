package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/settings"
)

var (
	connectionPage    = mustPage("connection.html", "connector.html")
	newConnectionPage = mustPage("connection_new.html")
)

// connectionsPath is where a tool connection's page lives, under its name.
const connectionsPath = "/admin/connections"

// ConnectionSettings is where tool connections are saved (*settings.Store).
// Its views and test records never carry a secret value.
type ConnectionSettings interface {
	ListFamily(ctx context.Context, prefix string) ([]settings.View, error)
	View(ctx context.Context, name string) (settings.View, error)
	Create(ctx context.Context, actor settings.Actor, name string, values map[string]any) (settings.View, error)
	Save(ctx context.Context, actor settings.Actor, name string, version int64, values map[string]any, secrets map[string]settings.SecretInput, enable bool) (settings.View, error)
	Test(ctx context.Context, actor settings.Actor, name string, draft *settings.Draft) (settings.TestRecord, error)
	Disable(ctx context.Context, actor settings.Actor, name string, version int64) (settings.View, error)
	Delete(ctx context.Context, actor settings.Actor, name string, version int64) error
}

// ConnectionCheckKey is the source_state key under a connection's row
// where the leader keeps its latest periodic check of the saved settings.
const ConnectionCheckKey = "check"

// ConnectionCheck is the leader's latest check of a connection's saved
// settings: the test it ran, and the settings version it ran on.
type ConnectionCheck struct {
	Version int64               `json:"version"`
	Test    settings.TestRecord `json:"test"`
}

// connectionSummary is one connection's row on the Connectors page.
type connectionSummary struct {
	Name, Kind, Address string
	State               connectorState
	NeverSetUp          bool
}

// connectionTool is one tool a connection's server offers, or one it is
// set to allow.
type connectionTool struct {
	Name string
	// Admission is the tool's ask admission, empty when the server was not
	// listed or no longer offers it.
	Admission string
	Checked   bool
}

// AdmissionText says why Ask may call the tool, or that it may not.
func (t connectionTool) AdmissionText() string {
	switch t.Admission {
	case ask.AdmittedReviewed:
		return "Reviewed as read-only"
	case ask.AdmittedAnnotated:
		return "Marked read-only by the server"
	case ask.AdmissionNone:
		return "Not marked read-only"
	}
	return "Not offered by the server"
}

// datasourcePin is a Grafana connection's pin for one kind of query.
type datasourcePin struct {
	Key, Label, Value string
	// Options are the datasources of the kind the server lists; without
	// them the page takes a UID as text.
	Options []fieldOption
	Error   string
}

// connectionData is a connection's page.
type connectionData struct {
	pageData
	CSRFToken string
	// Name is the connection's name, Kind its kind, and View its saved
	// settings.
	Name, Kind string
	View       settings.View
	State      connectorState
	Notice     string
	Error      string
	// Report is the test shown below the form: the form's own when Draft,
	// otherwise the saved settings' last test. Check is the leader's latest
	// check of the saved settings, while it is on.
	Report *settings.TestRecord
	Draft  bool
	Check  *settings.TestRecord
	// The form's values.
	URL, Organization, Project string
	Credential                 connectorField
	Agents                     string
	AuthScheme, ActorHeader    string
	Timeout                    string
	AllowUnreviewed            bool
	Tools                      []connectionTool
	Datasources                []datasourcePin
	FieldErrors                map[string]string
	AgentOptions               []fieldOption
	SentryHost                 string
	DefaultTimeout             string
	// ToolsListed reports that a test listed the server's tools.
	ToolsListed bool
}

// KindTitle is what the page calls the connection's kind.
func (d connectionData) KindTitle() string { return askconn.KindTitle(d.Kind) }

// NeverSetUp reports a connection whose settings no save has tested.
func (d connectionData) NeverSetUp() bool { return !d.View.Enabled && d.View.LastTest == nil }

// StateText says in a sentence what the state means.
func (d connectionData) StateText() string {
	if d.State.Reason != "" {
		return d.State.Reason
	}
	if !d.View.Enabled {
		return "Ask can't use its tools."
	}
	switch d.Agents {
	case askconn.AgentsAsk:
		return "Ask can use its tools."
	case askconn.AgentsBoth:
		return "Ask and its observability investigator can use its tools."
	}
	return "Ask's observability investigator can use its tools."
}

// newConnectionData is the page that adds a connection.
type newConnectionData struct {
	pageData
	CSRFToken string
	Kinds     []fieldOption
	Kind      string
	Name      string
	Error     string
}

func (s *Server) mountConnectionRoutes(mux Mux) {
	mux.Handle("GET "+connectionsPath+"/new", s.authedBrowser(http.HandlerFunc(s.handleNewConnection)))
	mux.Handle("POST "+connectionsPath, s.authedBrowser(http.HandlerFunc(s.handleConnectionCreate)))
	mux.Handle("GET "+connectionsPath+"/{name}", s.authedBrowser(http.HandlerFunc(s.handleConnection)))
	mux.Handle("POST "+connectionsPath+"/{name}", s.authedBrowser(http.HandlerFunc(s.handleConnectionSave)))
	mux.Handle("POST "+connectionsPath+"/{name}/enable", s.authedBrowser(http.HandlerFunc(s.handleConnectionSave)))
	mux.Handle("POST "+connectionsPath+"/{name}/test", s.authedBrowser(http.HandlerFunc(s.handleConnectionTest)))
	mux.Handle("POST "+connectionsPath+"/{name}/disable", s.authedBrowser(http.HandlerFunc(s.handleConnectionDisable)))
	mux.Handle("POST "+connectionsPath+"/{name}/delete", s.authedBrowser(http.HandlerFunc(s.handleConnectionDelete)))
}

func (s *Server) requireConnections(w http.ResponseWriter) bool {
	if s.connections == nil {
		http.Error(w, "tool connections are unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// connectionHref is a connection's page.
func connectionHref(name string) string { return connectionsPath + "/" + url.PathEscape(name) }

// connectionSummaries lists every connection for the Connectors page.
func (s *Server) connectionSummaries(ctx context.Context) ([]connectionSummary, error) {
	if s.connections == nil {
		return nil, nil
	}
	views, err := s.connections.ListFamily(ctx, askconn.Prefix)
	if err != nil {
		return nil, err
	}
	checks, err := s.connectionChecks(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]connectionSummary, 0, len(views))
	for _, view := range views {
		kind, _ := view.Settings[askconn.FieldKind].(string)
		summaries = append(summaries, connectionSummary{
			Name: askconn.Name(view.Name), Kind: askconn.KindTitle(kind), Address: connectionAddress(view),
			State: connectionState(view, checks[view.Name]), NeverSetUp: view.LastTest == nil,
		})
	}
	return summaries, nil
}

// connectionAddress is what the server a connection reaches is called on
// pages: its host, or a Sentry project.
func connectionAddress(view settings.View) string {
	text := func(key string) string { value, _ := view.Settings[key].(string); return value }
	if text(askconn.FieldKind) == askconn.KindSentry {
		if text(askconn.FieldSentryOrganization) == "" {
			return ""
		}
		return text(askconn.FieldSentryOrganization) + " / " + text(askconn.FieldSentryProject)
	}
	if parsed, err := url.Parse(text(askconn.FieldURL)); err == nil {
		return parsed.Host
	}
	return ""
}

// connectionChecks reads the leader's latest check of each connection, by
// settings row.
func (s *Server) connectionChecks(ctx context.Context) (map[string]ConnectionCheck, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, value FROM source_state
		 WHERE starts_with(source, $1) AND key = $2`, askconn.Prefix, ConnectionCheckKey)
	if err != nil {
		return nil, fmt.Errorf("connection checks: %w", err)
	}
	checks := map[string]ConnectionCheck{}
	var row string
	var raw []byte
	_, err = pgx.ForEachRow(rows, []any{&row, &raw}, func() error {
		var check ConnectionCheck
		if json.Unmarshal(raw, &check) == nil {
			checks[row] = check
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("connection checks: %w", err)
	}
	return checks, nil
}

// connectionState is a connection's state: off, on, or needs attention
// while its last test or the leader's latest check of what is saved failed.
func connectionState(view settings.View, check ConnectionCheck) connectorState {
	if failing := failingCheck(view, check); view.Enabled && failing != "" {
		return connectorState{Tone: toneAttention, Word: "needs attention", Reason: failing}
	}
	return stateOf(view, "")
}

// failingCheck says what is failing about a connection's saved settings, or
// "": the leader's latest check of them, or else their last test.
func failingCheck(view settings.View, check ConnectionCheck) string {
	if check.Version == view.Version && !check.Test.Passed {
		for _, item := range check.Test.Checks {
			if !item.Passed {
				return "Its latest check failed: " + lowerFirst(item.Detail)
			}
		}
	}
	if view.LastTest != nil && !view.LastTest.Passed {
		return failedTestReason
	}
	return ""
}

// connectionAttention is one Overview item for each connection that is on
// and failing.
func (s *Server) connectionAttention(ctx context.Context) ([]attentionItem, error) {
	summaries, err := s.connectionSummaries(ctx)
	if err != nil {
		return nil, err
	}
	var items []attentionItem
	for _, summary := range summaries {
		if summary.State.Attention() {
			items = append(items, attentionItem{
				Text: "Tool connection " + summary.Name + ": " + lowerFirst(summary.State.Reason),
				Href: connectionHref(summary.Name), LinkText: summary.Name,
			})
		}
	}
	return items, nil
}

func (s *Server) handleNewConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnections(w) {
		return
	}
	s.renderNewConnection(w, r, newConnectionData{Kind: r.URL.Query().Get("kind")}, http.StatusOK)
}

func (s *Server) renderNewConnection(w http.ResponseWriter, r *http.Request, data newConnectionData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, "Add a tool connection", navConnectors)
	data.Stylesheet = "connectors.css"
	data.CSRFToken = token
	for _, kind := range askconn.Kinds {
		data.Kinds = append(data.Kinds, fieldOption{Value: kind, Label: askconn.KindTitle(kind)})
	}
	if !slices.Contains(askconn.Kinds, data.Kind) {
		data.Kind = askconn.KindGrafana
	}
	s.renderStatus(w, newConnectionPage, data, code)
}

func (s *Server) handleConnectionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnections(w) || !s.requireMutation(w, r) {
		return
	}
	data := newConnectionData{Kind: r.Form.Get("kind"), Name: strings.TrimSpace(r.Form.Get("name"))}
	if !slices.Contains(askconn.Kinds, data.Kind) {
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if err := askconn.CheckName(data.Name); err != nil {
		data.Error = "Not added: for the name, " + err.Error() + "."
		s.renderNewConnection(w, r, data, http.StatusBadRequest)
		return
	}
	_, err := s.connections.Create(r.Context(), s.connectorActor(r), askconn.Row(data.Name), map[string]any{askconn.FieldKind: data.Kind})
	switch {
	case errors.Is(err, settings.ErrExists):
		data.Error = "Not added: a connection named " + data.Name + " already exists."
		s.renderNewConnection(w, r, data, http.StatusBadRequest)
	case err != nil:
		s.logger.Error("connection create failed", "err", err)
		http.Error(w, "connection create failed", http.StatusInternalServerError)
	default:
		redirectWithResult(w, r, connectionHref(data.Name),
			"Connection added. Fill in its settings, then save and turn it on.", false)
	}
}

// connectionView reads the connection the request names.
func (s *Server) connectionView(w http.ResponseWriter, r *http.Request) (settings.View, bool) {
	name := r.PathValue("name")
	if askconn.CheckName(name) != nil {
		s.renderNotFound(w, r, missingConnection)
		return settings.View{}, false
	}
	view, err := s.connections.View(r.Context(), askconn.Row(name))
	switch {
	case err == nil:
		return view, true
	case errors.Is(err, settings.ErrUnknownConnector):
		s.renderNotFound(w, r, missingConnection)
	default:
		s.logger.Error("connection read failed", "err", err)
		http.Error(w, "connection read failed", http.StatusInternalServerError)
	}
	return settings.View{}, false
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnections(w) {
		return
	}
	view, ok := s.connectionView(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	data := savedConnection(view)
	data.Report = view.LastTest
	data.Notice, data.Error = cleanNotice(query.Get("notice")), cleanNotice(query.Get("error"))
	s.renderConnection(w, r, data, http.StatusOK)
}

// renderConnection fills in the page around data's form and renders it.
func (s *Server) renderConnection(w http.ResponseWriter, r *http.Request, data connectionData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.Name = askconn.Name(data.View.Name)
	data.pageData = s.page(r, data.Name, navConnectors)
	data.Stylesheet = "connectors.css"
	data.CSRFToken = token
	checks, err := s.connectionChecks(r.Context())
	if err != nil {
		s.logger.Error("connection checks failed", "err", err)
		http.Error(w, "connection read failed", http.StatusInternalServerError)
		return
	}
	check := checks[data.View.Name]
	data.State = connectionState(data.View, check)
	if data.View.Enabled && check.Version == data.View.Version {
		data.Check = &check.Test
	}
	data.AgentOptions = []fieldOption{
		{Value: askconn.AgentsInvestigator, Label: "The observability investigator"},
		{Value: askconn.AgentsAsk, Label: "Ask"},
		{Value: askconn.AgentsBoth, Label: "Both"},
	}
	data.SentryHost, data.DefaultTimeout = askconn.SentryHost, "2 minutes"
	report := data.View.LastTest
	if data.Draft {
		report = data.Report
	}
	data.Tools, data.ToolsListed = connectionTools(report, data.Tools)
	data.Datasources = datasourcePins(report, data.Datasources)
	for i := range data.Datasources {
		data.Datasources[i].Error = data.FieldErrors[data.Datasources[i].Key]
	}
	data.Credential.Error = data.FieldErrors[askconn.FieldToken]
	s.renderStatus(w, connectionPage, data, code)
}

// savedConnection is the form filled from the saved settings.
func savedConnection(view settings.View) connectionData {
	text := func(key string) string { value, _ := view.Settings[key].(string); return value }
	data := connectionData{
		View: view, Kind: text(askconn.FieldKind), URL: text(askconn.FieldURL),
		Organization: text(askconn.FieldSentryOrganization), Project: text(askconn.FieldSentryProject),
		Agents: text(askconn.FieldAgents), AuthScheme: text(askconn.FieldAuthScheme), ActorHeader: text(askconn.FieldActorHeader),
	}
	data.AllowUnreviewed, _ = view.Settings[askconn.FieldAllowUnreviewed].(bool)
	if d, err := time.ParseDuration(text(askconn.FieldTimeout)); err == nil {
		data.Timeout = settings.FormatDuration(d)
	}
	for _, name := range stringList(view.Settings[askconn.FieldTools]) {
		data.Tools = append(data.Tools, connectionTool{Name: name, Checked: true})
	}
	for _, kind := range []string{ask.GrafanaPrometheus, ask.GrafanaLoki, ask.GrafanaPyroscope} {
		key := askconn.DatasourceFields[kind]
		data.Datasources = append(data.Datasources, datasourcePin{Key: key, Value: text(key)})
	}
	for _, field := range savedFields(view) {
		if field.Key == askconn.FieldToken {
			data.Credential = field
			data.Credential.Label, data.Credential.Help = "Credential", credentialHelp(data.Kind)
			// Grafana and Sentry refuse an unauthenticated connection; the
			// test says so if it is missing.
			data.Credential.Required = data.Kind != askconn.KindGeneric
		}
	}
	return data
}

// credentialHelp says what credential a kind of server takes.
func credentialHelp(kind string) string {
	switch kind {
	case askconn.KindGrafana:
		return "The token Grafana's MCP server, or the gateway in front of it, accepts: a Grafana service account token with the Viewer role, for example."
	case askconn.KindSentry:
		return "A Sentry user auth token that can read the organization."
	}
	return "The token the server accepts, sent as Authorization: Bearer and the token. Leave it empty for a server that needs none."
}

func stringList(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		var out []string
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// connectionTools are the tools the report says the server offers, each
// checked when chosen holds it, then any chosen tool the report lacks.
// listed is false when no report lists the server's tools.
func connectionTools(report *settings.TestRecord, chosen []connectionTool) (tools []connectionTool, listed bool) {
	checked := map[string]bool{}
	for _, tool := range chosen {
		checked[tool.Name] = tool.Checked
	}
	seen := map[string]bool{}
	if report != nil {
		for _, fact := range report.Facts {
			if fact.Key != askconn.FactTool {
				continue
			}
			listed = true
			seen[fact.Label] = true
			tools = append(tools, connectionTool{Name: fact.Label, Admission: fact.Value, Checked: checked[fact.Label]})
		}
	}
	for _, tool := range chosen {
		if !seen[tool.Name] && tool.Checked {
			tools = append(tools, connectionTool{Name: tool.Name, Checked: true})
		}
	}
	return tools, listed
}

// datasourcePins offers, for each pin, the datasources of its kind the
// report lists, with the pinned one kept even when the list lacks it.
func datasourcePins(report *settings.TestRecord, pins []datasourcePin) []datasourcePin {
	labels := map[string]string{
		askconn.FieldPrometheus: "Prometheus", askconn.FieldLoki: "Loki", askconn.FieldPyroscope: "Pyroscope",
	}
	out := make([]datasourcePin, 0, len(pins))
	for _, pin := range pins {
		pin.Label = labels[pin.Key]
		kind := ""
		for k, field := range askconn.DatasourceFields {
			if field == pin.Key {
				kind = k
			}
		}
		listed := false
		if report != nil {
			for _, fact := range report.Facts {
				if fact.Key == askconn.FactDatasource+kind {
					listed = true
					pin.Options = append(pin.Options, fieldOption{Value: fact.ID, Label: fact.Value + " (" + fact.ID + ")"})
				}
			}
		}
		if listed && pin.Value != "" && !slices.ContainsFunc(pin.Options, func(o fieldOption) bool { return o.Value == pin.Value }) {
			pin.Options = append(pin.Options, fieldOption{Value: pin.Value, Label: pin.Value + " (not listed by Grafana)"})
		}
		out = append(out, pin)
	}
	return out
}

// connectionInput reads the form over view: the settings, the credential
// the form changes, and the page's fields to show again, which never carry
// an entered credential.
func connectionInput(view settings.View, form url.Values) (map[string]any, map[string]settings.SecretInput, connectionData) {
	data := savedConnection(view)
	values := map[string]any{}
	set := func(key string, target *string) {
		if form.Has("field." + key) {
			*target = form.Get("field." + key)
			values[key] = *target
		}
	}
	set(askconn.FieldURL, &data.URL)
	set(askconn.FieldSentryOrganization, &data.Organization)
	set(askconn.FieldSentryProject, &data.Project)
	set(askconn.FieldAgents, &data.Agents)
	set(askconn.FieldAuthScheme, &data.AuthScheme)
	set(askconn.FieldActorHeader, &data.ActorHeader)
	set(askconn.FieldTimeout, &data.Timeout)
	data.AllowUnreviewed = form.Get("field."+askconn.FieldAllowUnreviewed) == "true"
	values[askconn.FieldAllowUnreviewed] = data.AllowUnreviewed
	// The tool list is posted only when the page shows it.
	if form.Has("tools") {
		chosen := form["tool"]
		values[askconn.FieldTools] = chosen
		data.Tools = nil
		for _, name := range chosen {
			data.Tools = append(data.Tools, connectionTool{Name: name, Checked: true})
		}
	}
	for i := range data.Datasources {
		set(data.Datasources[i].Key, &data.Datasources[i].Value)
	}
	secrets := map[string]settings.SecretInput{}
	value := form.Get("secret." + askconn.FieldToken)
	data.Credential.Remove = form.Get("secret."+askconn.FieldToken+".remove") == "true"
	switch {
	case strings.TrimSpace(value) != "":
		secrets[askconn.FieldToken] = settings.SecretInput{Value: value}
		data.Credential.Pasted = true
	case data.Credential.Remove:
		secrets[askconn.FieldToken] = settings.SecretInput{Clear: true}
	}
	return values, secrets, data
}

// connectionForm reads the connection a mutation names and the form's
// version.
func (s *Server) connectionForm(w http.ResponseWriter, r *http.Request) (settings.View, int64, bool) {
	if !s.requireConnections(w) || !s.requireMutation(w, r) {
		return settings.View{}, 0, false
	}
	view, ok := s.connectionView(w, r)
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

// handleConnectionSave saves the form and tests what it saved: Save, or, on
// the /enable path, Turn on, which also turns the connection on when the
// test passes. A failed test leaves the settings saved and shows its checks.
func (s *Server) handleConnectionSave(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.connectionForm(w, r)
	if !ok {
		return
	}
	enable := strings.HasSuffix(r.URL.Path, "/enable")
	values, secrets, data := connectionInput(view, r.Form)
	saved, err := s.connections.Save(r.Context(), s.connectorActor(r), view.Name, version, values, secrets, enable)
	name := askconn.Name(view.Name)
	var fieldErrs settings.FieldErrors
	switch {
	case errors.As(err, &fieldErrs):
		data.FieldErrors, data.Report = fieldErrs, view.LastTest
		data.Error = "Not saved: fix the marked settings."
		if enable {
			data.Error = name + " is still off: fill in the marked settings."
		}
		s.renderConnection(w, r, data, http.StatusBadRequest)
	case err != nil:
		s.connectionFailure(w, r, name, "save", err)
	case saved.LastTest != nil && saved.LastTest.Passed:
		notice := "Saved, and the test passed."
		switch {
		case enable && !view.Enabled:
			notice = name + " is on. Ask uses its tools from its next question."
		case !saved.Enabled:
			notice = "Saved, and the test passed. Turn " + name + " on when you are ready."
		}
		redirectWithResult(w, r, connectionHref(name), notice, false)
	default:
		data := savedConnection(saved)
		data.Report = saved.LastTest
		data.Error = "Saved, but the test failed. Fix what it reports and save again."
		switch {
		case enable && !view.Enabled:
			data.Error = name + " is still off: the test of its settings failed. Fix what the test reports, then turn it on again."
		case saved.Enabled:
			data.Error = "Saved, but the test failed, so Ask may be without " + name + "'s tools. Fix what the test reports and save again."
		}
		s.renderConnection(w, r, data, http.StatusOK)
	}
}

func (s *Server) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	view, _, ok := s.connectionForm(w, r)
	if !ok {
		return
	}
	values, secrets, data := connectionInput(view, r.Form)
	record, err := s.connections.Test(r.Context(), s.connectorActor(r), view.Name, &settings.Draft{Settings: values, Secrets: secrets})
	var fieldErrs settings.FieldErrors
	switch {
	case err == nil:
		data.Report, data.Draft = &record, true
		if record.Passed {
			data.Notice = "The test passed. Save to keep these settings."
		} else {
			data.Error = "The test failed. Fix what it reports and test again."
		}
		if data.Credential.Pasted {
			data.FieldErrors = map[string]string{askconn.FieldToken: "Paste it again to save it."}
		}
		s.renderConnection(w, r, data, http.StatusOK)
	case errors.As(err, &fieldErrs):
		data.FieldErrors, data.Report = fieldErrs, view.LastTest
		data.Error = "Not tested: fix the marked settings."
		s.renderConnection(w, r, data, http.StatusBadRequest)
	default:
		s.connectionFailure(w, r, askconn.Name(view.Name), "test", err)
	}
}

func (s *Server) handleConnectionDisable(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.connectionForm(w, r)
	if !ok {
		return
	}
	name := askconn.Name(view.Name)
	if _, err := s.connections.Disable(r.Context(), s.connectorActor(r), view.Name, version); err != nil {
		s.connectionFailure(w, r, name, "disable", err)
		return
	}
	redirectWithResult(w, r, connectionHref(name), name+" is off. Ask stops using its tools from its next question; its settings are kept.", false)
}

func (s *Server) handleConnectionDelete(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.connectionForm(w, r)
	if !ok {
		return
	}
	name := askconn.Name(view.Name)
	if r.Form.Get("confirm") != "remove" {
		redirectWithResult(w, r, connectionHref(name), "Nothing was removed: tick the box to confirm.", true)
		return
	}
	if err := s.connections.Delete(r.Context(), s.connectorActor(r), view.Name, version); err != nil {
		s.connectionFailure(w, r, name, "remove", err)
		return
	}
	redirectWithResult(w, r, connectorsPath, name+" is removed, with its saved credential.", false)
}

// connectionFailure turns an expected error into a message on the page.
func (s *Server) connectionFailure(w http.ResponseWriter, r *http.Request, name, action string, err error) {
	switch {
	case errors.Is(err, settings.ErrConflict):
		redirectWithResult(w, r, connectionHref(name), "The settings changed since this page loaded. Check them and try again.", true)
	case errors.Is(err, settings.ErrInvalid):
		redirectWithResult(w, r, connectionHref(name), strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": "), true)
	default:
		s.logger.Error("connection action failed", "connection", name, "action", action, "err", err)
		http.Error(w, "connection action failed", http.StatusInternalServerError)
	}
}
