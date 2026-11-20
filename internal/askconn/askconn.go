// Package askconn defines Ask's tool connections: the MCP servers, such as
// Grafana and Sentry, whose tools an administrator gives Ask on the
// Connectors page. Each connection is a settings row of its own (a
// settings.Family), saved, tested, turned on and off, and audited like a
// connector; Ask's builds read the enabled ones from the settings cache.
package askconn

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/settings"
)

// Prefix starts every connection's settings row name.
const Prefix = "connection:"

// Row is the settings row of the connection name.
func Row(name string) string { return Prefix + name }

// Name is the connection a settings row holds.
func Name(row string) string { return strings.TrimPrefix(row, Prefix) }

// namePattern is a connection's name: it appears in logs, in Ask's status
// lines, and in the name of the variable that may set its credential.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// CheckName says what is wrong with a new connection's name, or nil.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("use 1 to 40 lowercase letters, digits or _, starting with a letter")
	}
	return nil
}

// Kinds of connection. A kind selects code reviewed against one upstream
// server: which tools count as read-only, how results are handled, and the
// investigator's prompts for it.
const (
	KindGeneric = "generic"
	KindGrafana = "grafana"
	KindSentry  = "sentry"
)

// Kinds are every kind, in the order pages offer them.
var Kinds = []string{KindGrafana, KindSentry, KindGeneric}

// KindTitle is what pages call a kind.
func KindTitle(kind string) string {
	switch kind {
	case KindGrafana:
		return "Grafana"
	case KindSentry:
		return "Sentry"
	}
	return "MCP server"
}

// Who may use a connection's tools.
const (
	// AgentsInvestigator gives them to the observability investigator, a
	// child agent Ask hands an investigation to, which researches in its
	// own context and reports back.
	AgentsInvestigator = "investigator"
	// AgentsAsk gives them to Ask itself.
	AgentsAsk = "ask"
	// AgentsBoth gives them to both, over one connection.
	AgentsBoth = "both"
)

// Fields of a connection.
const (
	FieldKind               = "kind"
	FieldURL                = "url"
	FieldSentryOrganization = "sentry_organization"
	FieldSentryProject      = "sentry_project"
	FieldToken              = "token"
	FieldAuthScheme         = "auth_scheme"
	FieldActorHeader        = "actor_header"
	FieldAgents             = "agents"
	FieldTools              = "tools"
	FieldAllowUnreviewed    = "allow_unreviewed"
	FieldTimeout            = "timeout"
	FieldPrometheus         = "prometheus"
	FieldLoki               = "loki"
	FieldPyroscope          = "pyroscope"
)

// DatasourceFields are the Grafana pins' fields, by the datasource kind each
// pins.
var DatasourceFields = map[string]string{
	ask.GrafanaPrometheus: FieldPrometheus, ask.GrafanaLoki: FieldLoki, ask.GrafanaPyroscope: FieldPyroscope,
}

// SentryHost is the hosted Sentry MCP server, a Sentry connection's address
// when none is given.
const SentryHost = "https://mcp.sentry.dev"

const defaultTimeout = 2 * time.Minute

// reservedTools are Ask's own tools, which a connection's tools may not
// shadow.
var reservedTools = []string{
	"retrieve", "list", "expand", "web_search", "web_fetch", "code_search",
	"code_read", "investigate_observability",
}

var (
	// toolName is the Responses API's tool-name rule.
	toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// grafanaUID is a Grafana datasource UID: at most 40 letters, digits, -
	// or _.
	grafanaUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	sentrySlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Connection is one connection's settings, ready to connect with. It prints
// and logs without its credential.
type Connection struct {
	Name, Kind string
	// Version is its settings row's version.
	Version int64
	URL     string
	// SentryOrganization and SentryProject scope a Sentry connection.
	SentryOrganization, SentryProject string
	Token, AuthScheme                 string
	ActorHeader                       string
	Agents                            string
	Tools                             []string
	AllowUnreviewed                   bool
	Timeout                           time.Duration
	// Datasources pins a Grafana connection's queries of each kind
	// (ask.GrafanaPrometheus, ...) to one datasource UID.
	Datasources map[string]string
}

// FromResolved reads a connection's settings.
func FromResolved(r settings.Resolved) Connection {
	c := Connection{
		Name: Name(r.Connector), Kind: cmp.Or(r.String(FieldKind), KindGeneric), Version: r.Version,
		URL:                strings.TrimRight(strings.TrimSpace(r.String(FieldURL)), "/"),
		SentryOrganization: r.String(FieldSentryOrganization), SentryProject: r.String(FieldSentryProject),
		Token: r.Secret(FieldToken), AuthScheme: cmp.Or(r.String(FieldAuthScheme), "Bearer"),
		ActorHeader: r.String(FieldActorHeader), Agents: cmp.Or(r.String(FieldAgents), AgentsInvestigator),
		Tools: r.List(FieldTools), AllowUnreviewed: r.Bool(FieldAllowUnreviewed),
		Timeout: cmp.Or(r.Duration(FieldTimeout), defaultTimeout), Datasources: map[string]string{},
	}
	if c.Kind == KindSentry && c.URL == "" {
		c.URL = SentryHost
	}
	if c.Kind == KindGrafana {
		for kind, field := range DatasourceFields {
			if uid := r.String(field); uid != "" {
				c.Datasources[kind] = uid
			}
		}
	}
	return c
}

// Format keeps the credential out of every fmt verb.
func (c Connection) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "askconn.Connection{%s %s v%d}", c.Name, c.Kind, c.Version)
}

// Endpoint is the address Diderot connects to: the URL, or for Sentry the
// hosted server's address scoped to the organization, the project, and the
// read-only inspect skill.
func (c Connection) Endpoint() string {
	if c.Kind != KindSentry {
		return c.URL
	}
	return c.URL + "/mcp/" + url.PathEscape(c.SentryOrganization) + "/" + url.PathEscape(c.SentryProject) + "?skills=inspect"
}

// ForAsk reports that Ask itself may use its tools.
func (c Connection) ForAsk() bool { return c.Agents == AgentsAsk || c.Agents == AgentsBoth }

// ForInvestigator reports that the observability investigator may use its
// tools.
func (c Connection) ForInvestigator() bool {
	return c.Agents == AgentsInvestigator || c.Agents == AgentsBoth
}

// Options are what ask needs to connect, for the tools the connection
// allows.
func (c Connection) Options() ask.MCPToolServerOptions {
	return ask.MCPToolServerOptions{
		Name: c.Name, Kind: ask.MCPServerKind(c.Kind), Endpoint: c.Endpoint(),
		Token: c.Token, AuthScheme: c.AuthScheme, ActorHeader: c.ActorHeader,
		Tools: c.Tools, AllowUnreviewedTools: c.AllowUnreviewed, Timeout: c.Timeout,
		GrafanaDatasources: c.Datasources,
	}
}

// missing says what a connection needs before Diderot can connect, or "".
func (c Connection) missing() string {
	switch {
	case c.Kind == KindSentry && (c.SentryOrganization == "" || c.SentryProject == ""):
		return "Enter the Sentry organization and project"
	case c.URL == "":
		return "Enter the server address"
	}
	return ""
}

// checkURL admits a connection's address. A credential rides every request,
// so plain http is allowed only to an in-cluster Service, or to a loopback
// address (a port-forward, a sidecar) in a standalone deployment. A Sentry
// address is the server's alone: Diderot adds the path.
func checkURL(kind, raw string, standalone bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return errors.New("enter an http or https address without a user name or password")
	}
	if u.Fragment != "" {
		return errors.New("remove the # part of the address")
	}
	if kind == KindSentry {
		if u.Scheme != "https" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return errors.New("enter only the server's https address, such as " + SentryHost)
		}
		return nil
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	inCluster := strings.HasSuffix(host, ".svc.cluster.local")
	addr, err := netip.ParseAddr(host)
	loopback := standalone && (host == "localhost" || (err == nil && addr.IsLoopback()))
	if !inCluster && !loopback {
		return errors.New("use https; plain http is allowed only for a Kubernetes Service (*.svc.cluster.local)")
	}
	return nil
}

// checkActorHeader refuses a header Diderot's own requests need.
func checkActorHeader(name string) error {
	if !httpguts.ValidHeaderFieldName(name) {
		return errors.New("enter an HTTP header name, such as X-Actor")
	}
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "accept", "content-type", "content-length", "host":
		return fmt.Errorf("%s is set by Diderot itself; choose another header", name)
	}
	if strings.HasPrefix(lower, "mcp-") {
		return fmt.Errorf("%s is set by Diderot itself; choose another header", name)
	}
	return nil
}

// checkTools says what is wrong with an allowlist's names, or nil.
func checkTools(tools []string) error {
	for _, tool := range tools {
		if !toolName.MatchString(tool) {
			return fmt.Errorf("%q is not a tool name", tool)
		}
		if slices.Contains(reservedTools, tool) {
			return fmt.Errorf("%s is one of Ask's own tools", tool)
		}
	}
	return nil
}
