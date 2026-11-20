package askconn

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/settings"
)

// Facts a connection's Test reports, beside its checks: one per tool the
// server offers (its label the tool's name, its value its admission), and
// one per datasource a Grafana server lists (FactDatasource plus the pin
// kind; its value the datasource's name, its ID the UID).
const (
	FactTool       = "tool"
	FactDatasource = "datasource:"
)

// Definitions makes each connection's settings.Definition.
type Definitions struct {
	// Standalone admits a loopback address (DIDEROT_DEPLOYMENT=standalone).
	Standalone bool
	// Images reports that the deployment has object storage, where Ask keeps
	// the Grafana panel images it renders: without it, Test refuses the
	// panel-image tool, as an Ask build would.
	Images bool
}

// Family is the settings family of every connection.
func (d Definitions) Family() settings.Family {
	return settings.Family{Prefix: Prefix, Define: d.Define}
}

// Define returns the definition of the connection whose settings row is
// row.
func (d Definitions) Define(row string) settings.Definition {
	return definition{row: row, defs: d}
}

// definition is one connection's settings.Definition.
type definition struct {
	row  string
	defs Definitions
}

func (d definition) Name() string { return d.row }

// SecretEnv is the variable that may set the connection's credential in
// place of a pasted one: DIDEROT_CONNECTION_<NAME>_TOKEN.
func (d definition) SecretEnv(field string) string { return EnvName(Name(d.row), field) }

// EnvName is the variable that sets a connection's secret field.
func EnvName(name, field string) string {
	return "DIDEROT_CONNECTION_" + strings.ToUpper(name+"_"+field)
}

// ReadEnv reads every connection credential variable that is set, from
// environ (os.Environ), for settings.Secrets.Env.
func ReadEnv(environ []string) map[string]string {
	env := map[string]string{}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "DIDEROT_CONNECTION_") && strings.HasSuffix(name, "_"+strings.ToUpper(FieldToken)) {
			if value = strings.TrimSpace(value); value != "" {
				env[name] = value
			}
		}
	}
	return env
}

func (definition) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldKind, Label: "Kind", Kind: settings.KindEnum, Options: Kinds,
			OptionLabels: []string{KindTitle(KindGrafana), KindTitle(KindSentry), KindTitle(KindGeneric)}, Default: KindGeneric},
		{Key: FieldURL, Label: "Server address", Kind: settings.KindText},
		{Key: FieldSentryOrganization, Label: "Organization", Kind: settings.KindText},
		{Key: FieldSentryProject, Label: "Project", Kind: settings.KindText},
		{Key: FieldToken, Label: "Credential", Kind: settings.KindSecret},
		{Key: FieldAuthScheme, Label: "Authorization scheme", Kind: settings.KindText, Default: "Bearer"},
		{Key: FieldActorHeader, Label: "Identity header", Kind: settings.KindText},
		{Key: FieldAgents, Label: "Who uses its tools", Kind: settings.KindEnum,
			Options:      []string{AgentsInvestigator, AgentsAsk, AgentsBoth},
			OptionLabels: []string{"The observability investigator", "Ask", "Both"}, Default: AgentsInvestigator},
		{Key: FieldTools, Label: "Tools Ask may call", Kind: settings.KindList},
		{Key: FieldAllowUnreviewed, Label: "Allow tools that are not marked read-only", Kind: settings.KindBool},
		{Key: FieldTimeout, Label: "Request timeout", Kind: settings.KindDuration, Default: defaultTimeout.String()},
		{Key: FieldPrometheus, Label: "Prometheus datasource", Kind: settings.KindText},
		{Key: FieldLoki, Label: "Loki datasource", Kind: settings.KindText},
		{Key: FieldPyroscope, Label: "Pyroscope datasource", Kind: settings.KindText},
	}
}

// Validate checks the fields the connection's kind uses, and clears the
// fields it does not use.
func (d definition) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	kind, _ := values[FieldKind].(string)
	text := func(key string) string { s, _ := values[key].(string); return s }
	if raw := text(FieldURL); raw != "" {
		raw = strings.TrimRight(raw, "/")
		values[FieldURL] = raw
		if err := checkURL(kind, raw, d.defs.Standalone); err != nil {
			errs[FieldURL] = err.Error()
		}
	}
	if kind == KindSentry {
		for _, key := range []string{FieldSentryOrganization, FieldSentryProject} {
			if slug := text(key); slug != "" && !sentrySlug.MatchString(slug) {
				errs[key] = "enter it as it appears in Sentry's addresses: lowercase letters, digits, - or _"
			}
		}
	} else {
		values[FieldSentryOrganization], values[FieldSentryProject] = "", ""
	}
	if scheme := text(FieldAuthScheme); scheme != "" && !httpguts.ValidHeaderFieldName(scheme) {
		errs[FieldAuthScheme] = "enter one word, such as Bearer"
	}
	if header := text(FieldActorHeader); header != "" {
		if err := checkActorHeader(header); err != nil {
			errs[FieldActorHeader] = err.Error()
		}
	}
	if kind != KindGeneric && values[FieldAgents] != AgentsInvestigator {
		errs[FieldAgents] = "a " + KindTitle(kind) + " connection serves the observability investigator only"
	}
	tools, _ := values[FieldTools].([]string)
	if err := checkTools(tools); err != nil {
		errs[FieldTools] = err.Error()
	}
	for _, key := range []string{FieldPrometheus, FieldLoki, FieldPyroscope} {
		switch uid := text(key); {
		case kind != KindGrafana:
			values[key] = ""
		case uid != "" && !grafanaUID.MatchString(uid):
			errs[key] = "enter a datasource UID: up to 40 letters, digits, - or _"
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// Test connects with the settings, lists what the server offers, and checks
// that every chosen tool is offered and may be called, as an Ask build
// would connect.
func (d definition) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	c := FromResolved(r)
	var report settings.TestReport
	if missing := c.missing(); missing != "" {
		report.Checks = append(report.Checks, settings.Check{
			Name: "Connect", Detail: "There is nothing to connect to yet.", Remedy: missing + ", then test again.",
		})
		return report
	}
	probe, err := ask.ProbeMCPServer(ctx, c.Options())
	if err != nil {
		report.Checks = append(report.Checks, settings.Check{
			Name: "Connect", Detail: "Diderot could not list the server's tools: " + err.Error() + ".",
			Remedy: connectRemedy(r, c),
		})
		return report
	}
	report.Checks = append(report.Checks, settings.Check{
		Name: "Connect", Passed: true, Detail: "The server offers " + countTools(len(probe.Tools)) + ".",
	})
	offered := map[string]ask.ProbedTool{}
	for _, tool := range probe.Tools {
		offered[tool.Name] = tool
		report.Facts = append(report.Facts, settings.Fact{Key: FactTool, Label: tool.Name, Value: tool.Admission})
	}
	report.Checks = append(report.Checks, d.toolsCheck(ctx, c, offered))
	if c.Kind == KindGrafana {
		for _, datasource := range probe.Datasources {
			report.Facts = append(report.Facts, settings.Fact{
				Key: FactDatasource + datasource.Kind, Label: datasource.Name, Value: datasource.Name, ID: datasource.UID,
			})
		}
		if check, ok := datasourcesCheck(c, probe); ok {
			report.Checks = append(report.Checks, check)
		}
	}
	return report
}

// toolsCheck checks the chosen tools: each is offered and may be called,
// and the whole set attaches the way an Ask build attaches it.
func (d definition) toolsCheck(ctx context.Context, c Connection, offered map[string]ask.ProbedTool) settings.Check {
	check := settings.Check{Name: "Tools"}
	if len(c.Tools) == 0 {
		check.Detail = "No tool is chosen, so Ask would get none."
		check.Remedy = "Choose the tools Ask may call from the list of tools the server offers, then save."
		return check
	}
	var missing, unreviewed []string
	for _, name := range c.Tools {
		tool, ok := offered[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case tool.Admission == ask.AdmissionNone:
			unreviewed = append(unreviewed, name)
		}
	}
	switch {
	case len(missing) > 0:
		check.Detail = "The server does not offer " + strings.Join(missing, ", ") + "."
		check.Remedy = "Untick them, or connect to a server that offers them."
		return check
	case len(unreviewed) > 0 && !c.AllowUnreviewed:
		check.Detail = strings.Join(unreviewed, ", ") + " " + isAre(len(unreviewed)) +
			" not known to be read-only. Ask calls a connection's tools with its credential for anyone who asks."
		check.Remedy = "Untick them, or allow tools that are not marked read-only if you are sure they change nothing."
		return check
	}
	opts := c.Options()
	if d.defs.Images {
		// A test calls no tool, so nothing is ever stored here.
		opts.Artifacts = &ask.ArtifactStore{}
	}
	server, err := ask.ConnectMCPToolServer(ctx, opts)
	if err != nil {
		check.Detail = "The chosen tools could not be attached: " + strings.TrimPrefix(err.Error(), fmt.Sprintf("ask MCP server %q: ", c.Name)) + "."
		check.Remedy = "Untick the tool it names, or fix what it says."
		return check
	}
	_ = server.Close()
	check.Passed = true
	check.Detail = countTools(len(c.Tools)) + " chosen; Ask may call each."
	if len(unreviewed) > 0 {
		check.Detail += " " + strings.Join(unreviewed, ", ") + " " + isAre(len(unreviewed)) + " not marked read-only."
	}
	return check
}

// datasourcesCheck checks a Grafana connection's pins against the
// datasources the server lists, when it lists them and something is pinned.
func datasourcesCheck(c Connection, probe ask.MCPServerProbe) (settings.Check, bool) {
	if len(c.Datasources) == 0 {
		return settings.Check{}, false
	}
	check := settings.Check{Name: "Datasources"}
	if probe.DatasourcesErr != nil || probe.Datasources == nil {
		check.Passed = true
		check.Detail = "The server's datasource list could not be read, so the pinned datasources were not checked."
		return check, true
	}
	var unknown []string
	for _, kind := range []string{ask.GrafanaPrometheus, ask.GrafanaLoki, ask.GrafanaPyroscope} {
		uid := c.Datasources[kind]
		if uid != "" && !slices.ContainsFunc(probe.Datasources, func(d ask.GrafanaDatasource) bool { return d.UID == uid && d.Kind == kind }) {
			unknown = append(unknown, kind+" "+uid)
		}
	}
	if len(unknown) > 0 {
		check.Detail = "Grafana lists no such datasource: " + strings.Join(unknown, ", ") + "."
		check.Remedy = "Choose datasources from those Grafana lists."
		return check, true
	}
	check.Passed = true
	check.Detail = "Every pinned datasource is one Grafana lists."
	return check, true
}

// connectRemedy says what to check when the server could not be reached.
func connectRemedy(r settings.Resolved, c Connection) string {
	credential := r.SupplySecret(FieldToken, "a credential the server accepts")
	if c.Kind == KindSentry {
		return "Check the organization and project, and " + lowerFirst(credential) + "."
	}
	return "Check the server address, and " + lowerFirst(credential) + "."
}

func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}

func countTools(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return strconv.Itoa(n) + " tools"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
