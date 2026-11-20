package askconn

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// fakeServer serves tools over MCP's streamable HTTP and records each
// request's Authorization header and path.
type fakeServer struct {
	*httptest.Server
	auth, paths []string
}

func newFakeServer(t *testing.T, tools []*mcp.Tool, result func(name string) *mcp.CallToolResult) *fakeServer {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	for _, remote := range tools {
		name := remote.Name
		srv.AddTool(remote, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return result(name), nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	fake := &fakeServer{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.auth = append(fake.auth, r.Header.Get("Authorization"))
		fake.paths = append(fake.paths, r.URL.RequestURI())
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fake.Close)
	return fake
}

func tool(name string, readOnly bool) *mcp.Tool {
	notDestructive := false
	return &mcp.Tool{
		Name: name, Description: "Tool " + name + ".",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"datasourceUid": map[string]any{"type": "string"}, "expr": map[string]any{"type": "string"},
		}},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &notDestructive},
	}
}

func resolved(name string, values settings.Values, token string) settings.Resolved {
	secrets := map[string]string{}
	if token != "" {
		secrets[FieldToken] = token
	}
	return settings.NewResolved(Row(name), values, secrets)
}

func checks(report settings.TestReport) map[string]settings.Check {
	out := map[string]settings.Check{}
	for _, check := range report.Checks {
		out[check.Name] = check
	}
	return out
}

func TestCheckName(t *testing.T) {
	for _, name := range []string{"grafana", "changes", "deploy_history", "a1"} {
		assert.NoError(t, CheckName(name), name)
	}
	for _, name := range []string{"", "Grafana", "1grafana", "changes-mcp", "a.b", strings.Repeat("a", 41)} {
		assert.Error(t, CheckName(name), name)
	}
	assert.Equal(t, "DIDEROT_CONNECTION_DEPLOY_HISTORY_TOKEN", EnvName("deploy_history", FieldToken))
	assert.Equal(t, map[string]string{"DIDEROT_CONNECTION_GRAFANA_TOKEN": "t"},
		ReadEnv([]string{"DIDEROT_CONNECTION_GRAFANA_TOKEN=t", "DIDEROT_CONNECTION_EMPTY_TOKEN= ", "DIDEROT_SLACK_BOT_TOKEN=x", "OTHER=1"}))
}

// TestFromResolvedComposesSentrysScopedAddress: an administrator enters a
// Sentry organization and project, never a URL path.
func TestFromResolvedComposesSentrysScopedAddress(t *testing.T) {
	c := FromResolved(resolved("sentry", settings.Values{
		FieldKind: KindSentry, FieldSentryOrganization: "example-org", FieldSentryProject: "web", FieldTools: []string{"search_issues"},
	}, "tok"))
	assert.Equal(t, "https://mcp.sentry.dev/mcp/example-org/web?skills=inspect", c.Endpoint())
	assert.Equal(t, AgentsInvestigator, c.Agents)
	assert.Equal(t, "Bearer", c.AuthScheme)
	assert.Equal(t, 2*time.Minute, c.Timeout)
	assert.Empty(t, c.Datasources)
	assert.NotContains(t, fmt.Sprintf("%v %+v %s", c, c, c), "tok", "a connection prints without its credential")

	g := FromResolved(resolved("grafana", settings.Values{
		FieldKind: KindGrafana, FieldURL: "https://grafana.example/mcp/", FieldLoki: "logs-main", FieldTimeout: "30s",
	}, ""))
	assert.Equal(t, "https://grafana.example/mcp", g.Endpoint())
	assert.Equal(t, map[string]string{"loki": "logs-main"}, g.Datasources)
	assert.Equal(t, 30*time.Second, g.Timeout)
	assert.True(t, g.ForInvestigator())
	assert.False(t, g.ForAsk())
}

func TestValidate(t *testing.T) {
	def := Definitions{}.Define(Row("c"))
	valid := func(values settings.Values) settings.Values {
		t.Helper()
		require.NoError(t, def.Validate(values))
		return values
	}
	refused := func(values settings.Values, field, message string) {
		t.Helper()
		err := def.Validate(values)
		var errs settings.FieldErrors
		require.ErrorAs(t, err, &errs)
		assert.Contains(t, errs[field], message)
	}
	values := valid(settings.Values{FieldKind: KindGrafana, FieldURL: "https://grafana.example/mcp/", FieldAgents: AgentsInvestigator,
		FieldSentryOrganization: "stale", FieldPrometheus: "prom-main"})
	assert.Equal(t, "https://grafana.example/mcp", values[FieldURL], "a trailing slash is dropped")
	assert.Equal(t, "", values[FieldSentryOrganization], "fields another kind uses are cleared")
	valid(settings.Values{FieldKind: KindGeneric, FieldURL: "http://changes.example.svc.cluster.local/mcp", FieldAgents: AgentsBoth})

	refused(settings.Values{FieldKind: KindGeneric, FieldURL: "http://mcp.example.com/mcp", FieldAgents: AgentsAsk}, FieldURL, "use https")
	refused(settings.Values{FieldKind: KindGeneric, FieldURL: "http://localhost:4001/mcp", FieldAgents: AgentsAsk}, FieldURL, "use https")
	require.NoError(t, Definitions{Standalone: true}.Define(Row("c")).Validate(
		settings.Values{FieldKind: KindGeneric, FieldURL: "http://localhost:4001/mcp", FieldAgents: AgentsAsk}), "loopback is for standalone")
	refused(settings.Values{FieldKind: KindGeneric, FieldURL: "https://user:pw@mcp.example.com", FieldAgents: AgentsAsk}, FieldURL, "without a user name")
	refused(settings.Values{FieldKind: KindSentry, FieldURL: "https://mcp.sentry.dev/mcp/org/p", FieldAgents: AgentsInvestigator}, FieldURL, "only the server's https address")
	refused(settings.Values{FieldKind: KindSentry, FieldSentryOrganization: "Org Name", FieldAgents: AgentsInvestigator}, FieldSentryOrganization, "lowercase")
	refused(settings.Values{FieldKind: KindGrafana, FieldAgents: AgentsAsk}, FieldAgents, "observability investigator only")
	refused(settings.Values{FieldKind: KindGeneric, FieldAgents: AgentsAsk, FieldTools: []string{"web_search"}}, FieldTools, "one of Ask's own tools")
	refused(settings.Values{FieldKind: KindGeneric, FieldAgents: AgentsAsk, FieldActorHeader: "Authorization"}, FieldActorHeader, "set by Diderot itself")
	refused(settings.Values{FieldKind: KindGeneric, FieldAgents: AgentsAsk, FieldAuthScheme: "Two Words"}, FieldAuthScheme, "one word")
	refused(settings.Values{FieldKind: KindGrafana, FieldAgents: AgentsInvestigator, FieldLoki: "not a uid!"}, FieldLoki, "datasource UID")
}

// TestTestListsToolsChecksTheChosenAndOffersDatasources: a test connects
// with the credential, lists every tool with whether Ask may call it, checks
// the chosen ones as an Ask build would, and lists a Grafana server's
// datasources for pinning.
func TestTestListsToolsChecksTheChosenAndOffersDatasources(t *testing.T) {
	datasources := `[{"id":1,"uid":"prom-main","name":"Prometheus","type":"prometheus"},` +
		`{"id":2,"uid":"logs-main","name":"Loki","type":"loki"},` +
		`{"id":3,"uid":"profiles","name":"Profiles","type":"grafana-pyroscope-datasource"},` +
		`{"id":4,"uid":"pg","name":"Postgres","type":"postgres"}]`
	server := newFakeServer(t, []*mcp.Tool{
		tool("grafana_query_prometheus", false), tool("grafana_list_datasources", true),
		tool("grafana_update_dashboard", false), tool("zendesk_search", true),
	}, func(name string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: datasources}}}
	})
	def := Definitions{}.Define(Row("grafana"))
	values := settings.Values{FieldKind: KindGrafana, FieldURL: server.URL + "/mcp", FieldAgents: AgentsInvestigator, FieldTimeout: "10s"}

	report := def.Test(context.Background(), resolved("grafana", values, "grafana-token"))
	assert.False(t, report.Passed(), "no tool is chosen yet")
	assert.True(t, checks(report)["Connect"].Passed)
	assert.Contains(t, checks(report)["Tools"].Remedy, "Choose the tools")
	assert.Equal(t, "Bearer grafana-token", server.auth[0])
	var tools, pins []string
	for _, fact := range report.Facts {
		switch {
		case fact.Key == FactTool:
			tools = append(tools, fact.Label+"="+fact.Value)
		case strings.HasPrefix(fact.Key, FactDatasource):
			pins = append(pins, fact.Key+"="+fact.ID)
		}
	}
	assert.Equal(t, []string{
		"grafana_list_datasources=annotated", "grafana_query_prometheus=reviewed",
		"grafana_update_dashboard=none", "zendesk_search=annotated",
	}, tools)
	assert.Equal(t, []string{"datasource:loki=logs-main", "datasource:prometheus=prom-main", "datasource:pyroscope=profiles"}, pins)

	values[FieldTools] = []string{"grafana_query_prometheus", "grafana_update_dashboard"}
	report = def.Test(context.Background(), resolved("grafana", values, "grafana-token"))
	assert.Contains(t, checks(report)["Tools"].Detail, "grafana_update_dashboard is not known to be read-only")

	values[FieldTools] = []string{"grafana_query_prometheus", "missing_tool"}
	report = def.Test(context.Background(), resolved("grafana", values, "grafana-token"))
	assert.Contains(t, checks(report)["Tools"].Detail, "does not offer missing_tool")

	values[FieldTools], values[FieldPrometheus], values[FieldLoki] = []string{"grafana_query_prometheus"}, "prom-main", "nope"
	report = def.Test(context.Background(), resolved("grafana", values, "grafana-token"))
	assert.True(t, checks(report)["Tools"].Passed)
	assert.Equal(t, "Grafana lists no such datasource: loki nope.", checks(report)["Datasources"].Detail)

	values[FieldLoki] = ""
	report = def.Test(context.Background(), resolved("grafana", values, "grafana-token"))
	assert.True(t, report.Passed(), "%+v", report.Checks)
}

func TestTestSaysWhatIsMissingOrUnreachable(t *testing.T) {
	def := Definitions{}.Define(Row("sentry"))
	report := def.Test(context.Background(), resolved("sentry", settings.Values{FieldKind: KindSentry}, ""))
	assert.Equal(t, "Enter the Sentry organization and project, then test again.", checks(report)["Connect"].Remedy)

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(refusing.Close)
	r := resolved("ops", settings.Values{FieldKind: KindGeneric, FieldURL: refusing.URL, FieldTimeout: "5s"}, "x").
		WithSecretEnv(FieldToken, "DIDEROT_CONNECTION_OPS_TOKEN")
	report = Definitions{}.Define(Row("ops")).Test(context.Background(), r)
	connect := checks(report)["Connect"]
	assert.False(t, connect.Passed)
	assert.Contains(t, connect.Detail, "could not list the server's tools")
	assert.Equal(t, "Check the server address, and set DIDEROT_CONNECTION_OPS_TOKEN in the deployment to a credential the server accepts.", connect.Remedy)
}

// TestTestReachesSentrysScopedAddress: the Sentry address is the server's
// plus the organization, project and read-only skill.
func TestTestReachesSentrysScopedAddress(t *testing.T) {
	server := newFakeServer(t, []*mcp.Tool{tool("search_issues", true)}, func(string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "[]"}}}
	})
	// Plain http is refused for a Sentry address on save; a test takes the
	// settings as they come.
	report := Definitions{}.Define(Row("sentry")).Test(context.Background(), resolved("sentry", settings.Values{
		FieldKind: KindSentry, FieldURL: server.URL, FieldSentryOrganization: "example-org", FieldSentryProject: "web",
		FieldAuthScheme: "Sentry-Bearer", FieldTools: []string{"search_issues"},
	}, "sentry-token"))
	assert.True(t, report.Passed(), "%+v", report.Checks)
	assert.Equal(t, "/mcp/example-org/web?skills=inspect", server.paths[0])
	assert.Equal(t, "Sentry-Bearer sentry-token", server.auth[0])
}
