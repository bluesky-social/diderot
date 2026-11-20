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

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
)

var (
	connectorsPage = mustPage("connectors.html", "setup_progress.html")
	connectorPage  = mustPage("connector.html", "connector_sections.html", "setup_progress.html")
)

// connectorsPath is the connectors list.
const connectorsPath = "/admin/connectors"

// ConnectorSettings is the connector settings store (*settings.Store). Its
// views and test records never carry a secret value.
type ConnectorSettings interface {
	List(ctx context.Context) ([]settings.View, error)
	View(ctx context.Context, name string) (settings.View, error)
	Save(ctx context.Context, actor settings.Actor, name string, version int64, values map[string]any, secrets map[string]settings.SecretInput, enable bool) (settings.View, error)
	Test(ctx context.Context, actor settings.Actor, name string, draft *settings.Draft) (settings.TestRecord, error)
	Disable(ctx context.Context, actor settings.Actor, name string, version int64) (settings.View, error)
}

// connectorsData lists the sources, the tools, and the automations, each in
// registration order. A connector of no registered kind lists with the
// tools.
type connectorsData struct {
	pageData
	Sources     []connectorSummary
	Tools       []connectorSummary
	Automations []connectorSummary
	// Connections are the tool connections, in name order, shown while the
	// page can manage them (ConnectionsShown).
	Connections      []connectionSummary
	ConnectionsShown bool
	// Setup is the way on while the install is in setup: connecting a
	// source is a step of it.
	Setup *setupProgress
}

// connectorSummary is one connector's row in the list.
type connectorSummary struct {
	Name, Title, Summary string
	State                connectorState
	// NeverSetUp reports that no save has tested its settings yet.
	NeverSetUp bool
	// Newest is when a source's newest indexed item happened; nil when it
	// has none, or is off.
	Newest *time.Time
}

// connectorState is a connector's state as the pages show it: on, off, or
// needs attention, and what needs it.
type connectorState struct {
	Tone, Word string
	Reason     string
	// Note says what a source that is working is doing.
	Note string
}

// Attention reports that the connector is on but something keeps it from
// working.
func (s connectorState) Attention() bool { return s.Tone == toneAttention }

// stateOf is the state of the connector view, which, while it is on,
// reason says is not working, or "" when nothing does.
func stateOf(view settings.View, reason string) connectorState {
	switch {
	case !view.Enabled:
		return connectorState{Tone: toneNeutral, Word: "off"}
	case reason != "":
		return connectorState{Tone: toneAttention, Word: "needs attention", Reason: reason}
	case view.LastTest != nil && !view.LastTest.Passed:
		return connectorState{Tone: toneAttention, Word: "needs attention", Reason: failedTestReason}
	}
	return connectorState{Tone: toneGood, Word: "on"}
}

// connectorKind is what a connector is, which decides how its page talks
// about it.
type connectorKind string

const (
	kindSource     connectorKind = "source"
	kindTool       connectorKind = "tool"
	kindAutomation connectorKind = "automation"
)

type connectorData struct {
	pageData
	// Name is the connector's title, and Summary what it does.
	Name    string
	Summary string
	Kind    connectorKind
	View    settings.View
	Fields  []connectorField
	// Report is the test shown below the form: the form's own when Draft,
	// otherwise the saved settings' last test.
	Report    *settings.TestRecord
	Draft     bool
	CSRFToken string
	// Notice is what an action did; Error why one was refused or failed.
	Notice string
	Error  string
	State  connectorState
	// Status is what it reports about its work, and what its last test
	// found out, while it is on; nil when there is nothing to show.
	Status *statusView
	// Backfill is the source's re-read form, nil when it has none.
	Backfill *source.Backfill
	// Tabs are the page's own tabs, nil when it has none.
	Tabs *tabSet
	// Setup is the way on while the install is in setup, on a source's
	// page.
	Setup *setupProgress
	connectorSections
}

// NeverSetUp reports a connector that is off and whose settings no save has
// tested: its page leads with the action that saves them and turns it on.
func (d connectorData) NeverSetUp() bool { return !d.View.Enabled && d.View.LastTest == nil }

// StateText says in a sentence what the state means for this kind of
// connector.
func (d connectorData) StateText() string {
	if d.State.Reason != "" {
		return d.State.Reason
	}
	on := d.View.Enabled
	switch d.Kind {
	case kindSource:
		if on {
			return "Diderot keeps it indexed, and Ask searches it."
		}
		return "Diderot is not indexing it."
	case kindAutomation:
		if on {
			return "It is running."
		}
		return "It is not running."
	}
	if on {
		return "Ask can use it."
	}
	return "Ask can't use it."
}

// connectorField is one generated form field with its value.
type connectorField struct {
	Key, Label, Help string
	Kind             settings.FieldKind
	Required         bool
	Options          []fieldOption
	PromptVar        string
	// SlackChannel marks a field that takes a Slack channel's name, ID or
	// link, saves its ID, and shows its name.
	SlackChannel bool
	// Value is a text, enum, int, or duration value, or a list one item per
	// line.
	Value   string
	Checked bool
	// Secret is where a secret field's value comes from, Env the deployment
	// variable that would set it, and CanStore whether a pasted value can be
	// saved.
	Secret   settings.SecretView
	Env      string
	CanStore bool
	// Pasted reports that the form carried a value for a secret field, which
	// is never shown again; Remove, that it asked to remove the saved one.
	Pasted bool
	Remove bool
	Error  string
}

// fieldOption is one choice of an enum field: the value saved, and what the
// page calls it.
type fieldOption struct{ Value, Label string }

func fieldOptions(f settings.FieldView) []fieldOption {
	options := make([]fieldOption, 0, len(f.Options))
	for i, value := range f.Options {
		option := fieldOption{Value: value, Label: value}
		if i < len(f.OptionLabels) {
			option.Label = f.OptionLabels[i]
		}
		options = append(options, option)
	}
	return options
}

// Optional reports a field that may be left empty: a text, list, or secret
// one nothing requires. A box, a choice, or a number always has a value.
func (f connectorField) Optional() bool {
	switch f.Kind {
	case settings.KindText, settings.KindList, settings.KindSecret:
		return !f.Required
	}
	return false
}

type statusView struct {
	Rows []statusRow
	Note string
}

// statusRow is one fact: a time (nil while it has never happened) or text.
type statusRow struct {
	Label  string
	IsTime bool
	Time   *time.Time
	Text   string
	// ID is the identifier of what Text names, shown beside it.
	ID     string
	Detail string
}

// newStatusView is what the connector's page shows while it is on: the
// values its last test found out (its discovered fields, then its other
// facts), then status's rows and note. It is nil when there is nothing.
func newStatusView(view settings.View, status settings.Status) *statusView {
	out := &statusView{Note: status.Note}
	discovered := map[string]bool{}
	for _, field := range view.Fields {
		if !field.Discovered {
			continue
		}
		discovered[field.Key] = true
		if value, _ := view.Settings[field.Key].(string); value != "" {
			out.Rows = append(out.Rows, statusRow{Label: field.Label, Text: value})
		}
	}
	if view.LastTest != nil {
		for _, fact := range view.LastTest.Facts {
			if !discovered[fact.Key] && fact.Value != "" {
				out.Rows = append(out.Rows, statusRow{Label: fact.Label, Text: fact.Value, ID: fact.ID})
			}
		}
	}
	for _, row := range status.Rows {
		view := statusRow{Label: row.Label, Detail: row.Detail}
		switch value := row.Value.(type) {
		case *time.Time:
			view.IsTime, view.Time = true, value
		case int64:
			view.Text = formatCount(value)
		case string:
			view.Text = value
		default:
			view.Text = fmt.Sprint(value)
		}
		out.Rows = append(out.Rows, view)
	}
	if len(out.Rows) == 0 && out.Note == "" {
		return nil
	}
	return out
}

func (s *Server) mountConnectorRoutes(mux Mux) {
	mux.Handle("GET "+connectorsPath, s.authedBrowser(http.HandlerFunc(s.handleConnectors)))
	mux.Handle("GET /admin/connectors/{name}", s.authedBrowser(http.HandlerFunc(s.handleConnector)))
	mux.Handle("POST /admin/connectors/{name}", s.authedBrowser(http.HandlerFunc(s.handleConnectorSave)))
	mux.Handle("POST /admin/connectors/{name}/test", s.authedBrowser(http.HandlerFunc(s.handleConnectorTest)))
	mux.Handle("POST /admin/connectors/{name}/enable", s.authedBrowser(http.HandlerFunc(s.handleConnectorSave)))
	mux.Handle("POST /admin/connectors/{name}/disable", s.authedBrowser(http.HandlerFunc(s.handleConnectorDisable)))
	mux.Handle("GET "+slackChannelsPath, s.authedBrowser(http.HandlerFunc(s.handleSlackChannels)))
	mux.Handle("GET "+slackChannelsPath+"/{id}", s.authedBrowser(http.HandlerFunc(s.handleSlackSource)))
	mux.Handle("POST "+slackChannelsPath+"/invite", s.authedBrowser(http.HandlerFunc(s.handleSlackChannelInvite)))
	mux.Handle("POST "+slackChannelsPath+"/{id}/config", s.authedBrowser(http.HandlerFunc(s.handleSlackSourceConfig)))
	mux.Handle("POST "+slackChannelsPath+"/{id}/blacklist", s.authedBrowser(http.HandlerFunc(s.handleSlackSourceBlacklist)))
	mux.Handle("POST "+slackChannelsPath+"/{id}/membership", s.authedBrowser(http.HandlerFunc(s.handleSlackSourceMembership)))
	mux.Handle("POST "+slackChannelsPath+"/{id}/backfill", s.authedBrowser(http.HandlerFunc(s.handleSlackSourceBackfill)))
}

func (s *Server) requireConnectors(w http.ResponseWriter) bool {
	if s.connectors == nil {
		http.Error(w, "connector settings are unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// slackWorkspaceHost is the saved Slack workspace host, empty while Slack
// is off.
func (s *Server) slackWorkspaceHost() string {
	if s.slackHost == nil {
		return ""
	}
	return s.slackHost()
}

// connectorKindOf is what the named connector is. A connector of no
// registered kind is a tool.
func (s *Server) connectorKindOf(name string) connectorKind {
	if _, ok := s.registries.Sources.Named(name); ok {
		return kindSource
	}
	if _, ok := s.registries.Automations.Lookup(name); ok {
		return kindAutomation
	}
	return kindTool
}

// connectorSummaryText says what the named connector does, from its
// registered facts.
func (s *Server) connectorSummaryText(name string) string {
	if src, ok := s.registries.Sources.Named(name); ok {
		return src.Info().Summary
	}
	if tool, ok := s.registries.Tools.Lookup(name); ok {
		return tool.Info().Summary
	}
	if auto, ok := s.registries.Automations.Lookup(name); ok {
		return auto.Info().Summary
	}
	return ""
}

// stateOfConnector is the connector's state: a source's is its health, and a
// tool's or automation's what it reports is keeping it from working.
func (s *Server) stateOfConnector(ctx context.Context, view settings.View) (connectorState, settings.Status, error) {
	var status settings.Status
	if view.Enabled {
		var err error
		if status, _, err = s.registries.Status(ctx, s.sourceDeps, view.Name); err != nil {
			return connectorState{}, status, err
		}
	}
	if src, ok := s.registries.Sources.Named(view.Name); ok {
		health, err := s.sourceHealthOf(ctx, src, view)
		return health.state(), status, err
	}
	return stateOf(view, status.Waiting), status, nil
}

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectors(w) {
		return
	}
	ctx := r.Context()
	views, err := s.connectors.List(ctx)
	if err != nil {
		s.logger.Error("connector list failed", "err", err)
		http.Error(w, "connector list failed", http.StatusInternalServerError)
		return
	}
	data := connectorsData{pageData: s.page(r, "Connectors", navConnectors)}
	data.Stylesheet = "connectors.css"
	if s.setup != nil {
		data.Setup = s.setupProgress(ctx, setup.StepSources)
	}
	var eventSources []string
	for _, view := range views {
		summary := connectorSummary{
			Name: view.Name, Title: s.registries.Title(view.Name), Summary: s.connectorSummaryText(view.Name),
			NeverSetUp: view.LastTest == nil,
		}
		var err error
		if summary.State, _, err = s.stateOfConnector(ctx, view); err != nil {
			s.logger.Error("connector status failed", "connector", view.Name, "err", err)
			summary.State = stateOf(view, "Diderot couldn't read its status.")
		}
		switch s.connectorKindOf(view.Name) {
		case kindSource:
			data.Sources = append(data.Sources, summary)
			if view.Enabled {
				src, _ := s.registries.Sources.Named(view.Name)
				eventSources = append(eventSources, src.Info().EventSource)
			}
		case kindAutomation:
			data.Automations = append(data.Automations, summary)
		default:
			data.Tools = append(data.Tools, summary)
		}
	}
	newest, err := s.newestItems(ctx, eventSources)
	if err != nil {
		s.logger.Error("connector newest items failed", "err", err)
		http.Error(w, "connector list failed", http.StatusInternalServerError)
		return
	}
	for index, summary := range data.Sources {
		if src, ok := s.registries.Sources.Named(summary.Name); ok {
			data.Sources[index].Newest = newest[src.Info().EventSource]
		}
	}
	data.ConnectionsShown = s.connections != nil
	if data.Connections, err = s.connectionSummaries(ctx); err != nil {
		s.logger.Error("connection list failed", "err", err)
		http.Error(w, "connector list failed", http.StatusInternalServerError)
		return
	}
	order := s.registries.Names()
	slices.SortStableFunc(data.Sources, byListOrder(order))
	slices.SortStableFunc(data.Tools, byListOrder(order))
	slices.SortStableFunc(data.Automations, byListOrder(order))
	s.render(w, connectorsPage, data)
}

// newestItems reads when the newest indexed item of each event source
// happened. A source with none has no entry.
func (s *Server) newestItems(ctx context.Context, eventSources []string) (map[string]*time.Time, error) {
	newest := map[string]*time.Time{}
	if len(eventSources) == 0 {
		return newest, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT name, (SELECT max(occurred_at) FROM events WHERE source = name AND NOT deleted)
		  FROM unnest($1::text[]) AS name`, eventSources)
	if err != nil {
		return nil, fmt.Errorf("newest items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var at *time.Time
		if err := rows.Scan(&name, &at); err != nil {
			return nil, fmt.Errorf("newest items scan: %w", err)
		}
		if at != nil {
			newest[name] = at
		}
	}
	return newest, rows.Err()
}

// byListOrder orders connectors as names lists them, any it doesn't list
// last.
func byListOrder(names []string) func(a, b connectorSummary) int {
	rank := func(name string) int {
		if i := slices.Index(names, name); i >= 0 {
			return i
		}
		return len(names)
	}
	return func(a, b connectorSummary) int { return rank(a.Name) - rank(b.Name) }
}

func (s *Server) handleConnector(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectors(w) {
		return
	}
	view, err := s.connectors.View(r.Context(), r.PathValue("name"))
	if errors.Is(err, settings.ErrUnknownConnector) {
		s.renderNotFound(w, r, missingConnector)
		return
	}
	if err != nil {
		s.logger.Error("connector read failed", "err", err)
		http.Error(w, "connector read failed", http.StatusInternalServerError)
		return
	}
	query := r.URL.Query()
	s.renderConnector(w, r, connectorData{
		View: view, Fields: savedFields(view), Report: view.LastTest,
		Notice: cleanNotice(query.Get("notice")), Error: cleanNotice(query.Get("error")),
	}, http.StatusOK)
}

// renderConnector renders data, which sets View, Fields, Report, Draft, and
// the notice or error; it fills in the rest.
func (s *Server) renderConnector(w http.ResponseWriter, r *http.Request, data connectorData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	view := data.View
	data.Name, data.Summary, data.Kind = s.registries.Title(view.Name), s.connectorSummaryText(view.Name), s.connectorKindOf(view.Name)
	data.pageData = s.page(r, data.Name, navConnectors)
	data.Stylesheet = "connectors.css"
	data.CSRFToken = token
	var status settings.Status
	if data.State, status, err = s.stateOfConnector(ctx, view); err != nil {
		s.sourcePageError(w, data.Name+" status", err)
		return
	}
	if view.Enabled {
		data.Status = newStatusView(view, status)
	}
	if data.Kind == kindSource {
		if s.setup != nil {
			data.Setup = s.setupProgress(ctx, setup.StepSources)
		}
		src, _ := s.registries.Sources.Named(view.Name)
		if backfiller, ok := src.(source.Backfiller); ok && view.Enabled {
			backfill := backfiller.Backfill()
			data.Backfill = &backfill
		}
	}
	if s.sectionOf(view.Name) == slackChannelsSection {
		data.Tabs = slackTabs(false)
	}
	if data.connectorSections, err = s.loadConnectorSections(r, view); err != nil {
		s.sourcePageError(w, data.Name+" status", err)
		return
	}
	if data.Discord != nil {
		data.Discord.setCSRF(token)
	}
	if err := s.nameChannelFields(ctx, data.Fields); err != nil {
		s.sourcePageError(w, data.Name+" settings", err)
		return
	}
	s.renderStatus(w, connectorPage, data, code)
}

// savedFields fills the form from the saved settings. Discovered fields are
// facts a test finds, not settings, so the page shows them with its status.
func savedFields(view settings.View) []connectorField {
	fields := make([]connectorField, 0, len(view.Fields))
	for _, f := range view.Fields {
		if f.Discovered {
			continue
		}
		field := connectorField{
			Key: f.Key, Label: f.Label, Help: f.Help, Kind: f.Kind, Required: f.Required,
			Options: fieldOptions(f), PromptVar: f.PromptVar, SlackChannel: f.SlackChannel,
		}
		switch f.Kind {
		case settings.KindSecret:
			field.Secret, field.Env, field.CanStore = view.Secrets[f.Key], f.Env, view.CanStoreSecrets
		case settings.KindDuration:
			field.Value, _ = formatValue(view.Settings[f.Key])
			if d, err := time.ParseDuration(field.Value); err == nil {
				field.Value = settings.FormatDuration(d)
			}
		default:
			field.Value, field.Checked = formatValue(view.Settings[f.Key])
		}
		fields = append(fields, field)
	}
	return fields
}

func formatValue(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case bool:
		return strconv.FormatBool(v), v
	case []string:
		return strings.Join(v, "\n"), false
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, fmt.Sprint(item))
		}
		return strings.Join(items, "\n"), false
	}
	return fmt.Sprint(value), false
}

// draftInput reads the form: every field's value, and each secret the form
// changes. It also returns the fields to show again, which never carry an
// entered secret value.
func draftInput(view settings.View, form url.Values) (map[string]any, map[string]settings.SecretInput, []connectorField) {
	values := map[string]any{}
	secrets := map[string]settings.SecretInput{}
	fields := savedFields(view)
	for i, field := range fields {
		switch field.Kind {
		case settings.KindSecret:
			// An empty box keeps the saved value; a pasted one replaces it.
			value := form.Get("secret." + field.Key)
			fields[i].Remove = form.Get("secret."+field.Key+".remove") == "true"
			switch {
			case strings.TrimSpace(value) != "":
				secrets[field.Key] = settings.SecretInput{Value: value}
				fields[i].Pasted = true
			case fields[i].Remove:
				secrets[field.Key] = settings.SecretInput{Clear: true}
			}
		case settings.KindBool:
			value := form.Get("field." + field.Key)
			values[field.Key] = value
			fields[i].Checked = value == "true"
		default:
			value := form.Get("field." + field.Key)
			values[field.Key] = value
			fields[i].Value = value
		}
	}
	return values, secrets, fields
}

// resolveChannelFields turns what the form typed in each Slack channel
// field, a name, an ID or a link, into the channel's ID in values, and
// shows the channel's name in fields. A well-formed ID Diderot does not
// know is kept as typed: the bot may not be in that channel yet. It returns
// what is wrong with the others, by field.
func (s *Server) resolveChannelFields(ctx context.Context, values map[string]any, fields []connectorField) (settings.FieldErrors, error) {
	problems := settings.FieldErrors{}
	for i, field := range fields {
		typed, _ := values[field.Key].(string)
		if !field.SlackChannel || strings.TrimSpace(typed) == "" {
			continue
		}
		id, name, problem, err := s.resolveReportChannel(ctx, typed)
		switch key := slackChannelKey(typed); {
		case err != nil:
			return nil, err
		case problem == "":
			values[field.Key], fields[i].Value = id, name
		case slackChannelIDPattern.MatchString(key):
			values[field.Key] = key
		default:
			problems[field.Key] = problem
		}
	}
	return problems, nil
}

// nameChannelFields shows the name of the channel whose ID a Slack channel
// field holds, where Diderot knows it.
func (s *Server) nameChannelFields(ctx context.Context, fields []connectorField) error {
	channels := map[string]string{}
	for _, field := range fields {
		if field.SlackChannel && slackChannelIDPattern.MatchString(field.Value) {
			channels[field.Value] = ""
		}
	}
	if err := s.nameChannels(ctx, channels); err != nil {
		return err
	}
	for i, field := range fields {
		if name := channels[field.Value]; field.SlackChannel && name != "" {
			fields[i].Value = "#" + name
		}
	}
	return nil
}

func (s *Server) connectorActor(r *http.Request) settings.Actor {
	principal, _ := PrincipalFromContext(r.Context())
	return settings.Actor{Email: principal.Email, RequestID: strings.TrimSpace(r.Header.Get("X-Request-ID"))}
}

// connectorForm reads the connector a mutation names and the form's
// version.
func (s *Server) connectorForm(w http.ResponseWriter, r *http.Request) (settings.View, int64, bool) {
	if !s.requireConnectors(w) || !s.requireMutation(w, r) {
		return settings.View{}, 0, false
	}
	view, err := s.connectors.View(r.Context(), r.PathValue("name"))
	if errors.Is(err, settings.ErrUnknownConnector) {
		http.NotFound(w, r)
		return settings.View{}, 0, false
	}
	if err != nil {
		s.logger.Error("connector read failed", "err", err)
		http.Error(w, "connector read failed", http.StatusInternalServerError)
		return settings.View{}, 0, false
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return settings.View{}, 0, false
	}
	return view, version, true
}

// handleConnectorSave saves the form and tests what it saved: Save, or, on
// the /enable path, Turn on, which also turns the connector on when the test
// passes. A failed test leaves the settings saved and shows its checks.
func (s *Server) handleConnectorSave(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.connectorForm(w, r)
	if !ok {
		return
	}
	enable := strings.HasSuffix(r.URL.Path, "/enable")
	title := s.registries.Title(view.Name)
	values, secrets, fields := draftInput(view, r.Form)
	channelErrs, err := s.resolveChannelFields(r.Context(), values, fields)
	var saved settings.View
	if err == nil && len(channelErrs) > 0 {
		err = channelErrs
	}
	if err == nil {
		saved, err = s.connectors.Save(r.Context(), s.connectorActor(r), view.Name, version, values, secrets, enable)
	}
	var fieldErrs settings.FieldErrors
	switch {
	case errors.As(err, &fieldErrs):
		message := "Not saved: fix the marked settings."
		if enable {
			message = title + " is still off: fill in the marked settings."
		}
		s.renderConnector(w, r, connectorData{
			View: view, Fields: withErrors(fields, fieldErrs), Report: view.LastTest, Error: message,
		}, http.StatusBadRequest)
	case err != nil:
		s.connectorFailure(w, r, view.Name, "save", err)
	case saved.LastTest != nil && saved.LastTest.Passed:
		notice := "Saved, and the test passed."
		switch {
		case enable && !view.Enabled:
			notice = title + " is on."
		case !saved.Enabled:
			notice = "Saved, and the test passed. Turn " + title + " on when you are ready."
		}
		s.redirectConnector(w, r, view.Name, notice, false)
	default:
		message := "Saved, but the test failed. Fix what it reports and save again."
		switch {
		case enable && !view.Enabled:
			message = title + " is still off: the test of its settings failed. Fix what the test reports, then turn it on again."
		case saved.Enabled:
			message = "Saved, but the test failed, so " + title + " may not work. Fix what the test reports and save again."
		}
		s.renderConnector(w, r, connectorData{
			View: saved, Fields: savedFields(saved), Report: saved.LastTest, Error: message,
		}, http.StatusOK)
	}
}

func (s *Server) handleConnectorTest(w http.ResponseWriter, r *http.Request) {
	view, _, ok := s.connectorForm(w, r)
	if !ok {
		return
	}
	values, secrets, fields := draftInput(view, r.Form)
	channelErrs, err := s.resolveChannelFields(r.Context(), values, fields)
	var record settings.TestRecord
	if err == nil && len(channelErrs) > 0 {
		err = channelErrs
	}
	if err == nil {
		record, err = s.connectors.Test(r.Context(), s.connectorActor(r), view.Name, &settings.Draft{Settings: values, Secrets: secrets})
	}
	var fieldErrs settings.FieldErrors
	switch {
	case err == nil:
		data := connectorData{View: view, Fields: fields, Report: &record, Draft: true}
		if record.Passed {
			data.Notice = "The test passed. Save to keep these settings."
		} else {
			data.Error = "The test failed. Fix what it reports and test again."
		}
		for i := range data.Fields {
			if data.Fields[i].Pasted {
				data.Fields[i].Error = "Paste it again to save it."
			}
		}
		s.renderConnector(w, r, data, http.StatusOK)
	case errors.As(err, &fieldErrs):
		s.renderConnector(w, r, connectorData{
			View: view, Fields: withErrors(fields, fieldErrs), Report: view.LastTest,
			Error: "Not tested: fix the marked settings.",
		}, http.StatusBadRequest)
	default:
		s.connectorFailure(w, r, view.Name, "test", err)
	}
}

func (s *Server) handleConnectorDisable(w http.ResponseWriter, r *http.Request) {
	view, version, ok := s.connectorForm(w, r)
	if !ok {
		return
	}
	if _, err := s.connectors.Disable(r.Context(), s.connectorActor(r), view.Name, version); err != nil {
		s.connectorFailure(w, r, view.Name, "disable", err)
		return
	}
	notice := s.registries.Title(view.Name) + " is off. Its settings are kept."
	if s.connectorKindOf(view.Name) == kindSource {
		notice = s.registries.Title(view.Name) + " is off. Its settings and what it indexed are kept."
	}
	s.redirectConnector(w, r, view.Name, notice, false)
}

func withErrors(fields []connectorField, errs settings.FieldErrors) []connectorField {
	for i := range fields {
		if message, ok := errs[fields[i].Key]; ok {
			fields[i].Error = message
		}
	}
	return fields
}

// redirectConnector sends the browser back to the connector's page with
// what an action did, or why it was refused.
func (s *Server) redirectConnector(w http.ResponseWriter, r *http.Request, name, message string, refused bool) {
	redirectWithResult(w, r, connectorsPath+"/"+url.PathEscape(name), message, refused)
}

// connectorFailure turns an expected error into a message on the page.
func (s *Server) connectorFailure(w http.ResponseWriter, r *http.Request, name, action string, err error) {
	switch {
	case errors.Is(err, settings.ErrConflict):
		s.redirectConnector(w, r, name, "The settings changed since this page loaded. Check them and try again.", true)
	case errors.Is(err, settings.ErrInvalid):
		s.redirectConnector(w, r, name, strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": "), true)
	default:
		s.logger.Error("connector action failed", "connector", name, "action", action, "err", err)
		http.Error(w, "connector action failed", http.StatusInternalServerError)
	}
}
