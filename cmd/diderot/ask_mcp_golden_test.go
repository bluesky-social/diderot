package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
)

// askMCPGoldenPath records what Ask's MCP wiring registers and sends for a
// Grafana connection behind a gateway and a Sentry connection, so a diff
// against it is a behavior change for every deployment with such
// connections. Regenerate with DIDEROT_UPDATE_GOLDEN=1 only for an intended
// one.
const askMCPGoldenPath = "testdata/ask_mcp_tools.golden.json"

// mcpGrafanaRelease is what Ask sees differ between the mcp-grafana releases
// a gateway may run.
type mcpGrafanaRelease struct {
	name string
	// alertTool is the alert-rules tool's name. A connection lists the one
	// its gateway serves.
	alertTool string
	// structuredQueries repeats query_prometheus's JSON payload as structured
	// content, as mcp-grafana does since 2.1.
	structuredQueries bool
}

// mcpGrafanaReleases are the releases the golden covers, newest
// first. The golden is recorded under the newest.
var mcpGrafanaReleases = []mcpGrafanaRelease{
	{name: "mcp-grafana 2.x", alertTool: "grafana_alerting_rules_read", structuredQueries: true},
}

// gatewayMCPToolNames is the tool list a fake gateway /mcp listener serves:
// the 28 Grafana/Tempo tools the connection names plus tools the gateway
// federates that Ask must never expose.
var gatewayMCPToolNames = []string{
	mcpGrafanaReleases[0].alertTool,
	"grafana_get_dashboard_panel_queries",
	"grafana_get_dashboard_property",
	"grafana_get_dashboard_summary",
	"grafana_get_panel_image",
	"grafana_list_loki_label_names",
	"grafana_list_loki_label_values",
	"grafana_list_prometheus_label_names",
	"grafana_list_prometheus_label_values",
	"grafana_list_prometheus_metric_metadata",
	"grafana_list_prometheus_metric_names",
	"grafana_list_pyroscope_label_names",
	"grafana_list_pyroscope_label_values",
	"grafana_list_pyroscope_profile_types",
	"grafana_query_loki_logs",
	"grafana_query_loki_patterns",
	"grafana_query_loki_stats",
	"grafana_query_prometheus",
	"grafana_query_prometheus_histogram",
	"grafana_query_pyroscope",
	"grafana_search_dashboards",
	"tempo_docs-traceql",
	"tempo_get-attribute-names",
	"tempo_get-attribute-values",
	"tempo_get-trace",
	"tempo_traceql-metrics-instant",
	"tempo_traceql-metrics-range",
	"tempo_traceql-search",
	"grafana_update_dashboard",
	"zendesk_search_tickets",
}

// sentryMCPToolNames is what a fake hosted Sentry MCP serves: the eight
// inspect tools Ask uses plus a write tool it must not expose.
var sentryMCPToolNames = []string{
	"search_issues",
	"search_errors",
	"search_logs",
	"search_traces",
	"search_metrics",
	"search_profiles",
	"search_replays",
	"get_sentry_resource",
	"update_issue",
}

// fakeMCPServer serves tools over the official SDK's stateless streamable
// HTTP handler and records every request it receives.
type fakeMCPServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []goldenMCPRequest
}

type goldenMCPRequest struct {
	HTTPMethod  string            `json:"http_method"`
	Path        string            `json:"path"`
	Query       string            `json:"query,omitempty"`
	RPCMethod   string            `json:"rpc_method"`
	Params      json.RawMessage   `json:"params,omitempty"`
	HeaderNames []string          `json:"header_names"`
	Headers     map[string]string `json:"headers"`
}

// recordedMCPHeaders are compared by value; every other header by name only.
var recordedMCPHeaders = []string{"Accept", "Authorization", "Content-Type", "Mcp-Protocol-Version", "X-Auth-Request-User"}

func newFakeMCPServer(t *testing.T, name string, tools []*mcp.Tool, result func(name string) *mcp.CallToolResult) *fakeMCPServer {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	for _, remote := range tools {
		toolName := remote.Name
		srv.AddTool(remote, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return result(toolName), nil
		})
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true})
	fake := &fakeMCPServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, &envelope))
		}
		recorded := goldenMCPRequest{
			HTTPMethod: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, RPCMethod: envelope.Method,
			HeaderNames: headerNames(r.Header), Headers: map[string]string{},
		}
		if envelope.Method == "tools/call" {
			recorded.Params = envelope.Params
		}
		for _, header := range recordedMCPHeaders {
			if value := r.Header.Get(header); value != "" {
				recorded.Headers[header] = value
			}
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded)
		fake.mu.Unlock()
		transport.ServeHTTP(w, r)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// drain returns and clears the requests recorded so far.
func (f *fakeMCPServer) drain() []goldenMCPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := f.requests
	f.requests = nil
	return requests
}

// endpointFor keeps configured's path and query but dials the fake.
func (f *fakeMCPServer) endpointFor(t *testing.T, configured string) string {
	t.Helper()
	parsed, err := url.Parse(configured)
	require.NoError(t, err)
	fake, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	parsed.Scheme, parsed.Host = fake.Scheme, fake.Host
	return parsed.String()
}

// newGatewayMCP serves gatewayMCPToolNames the way a gateway
// on release does. Every tool answers with a JSON payload as text.
func newGatewayMCP(t *testing.T, release mcpGrafanaRelease) *fakeMCPServer {
	t.Helper()
	tools := make([]*mcp.Tool, 0, len(gatewayMCPToolNames))
	for _, name := range gatewayMCPToolNames {
		if name == mcpGrafanaReleases[0].alertTool {
			name = release.alertTool
		}
		properties := map[string]any{
			"query": map[string]any{"type": "string", "description": "Query for " + name + "."},
		}
		if name == release.alertTool {
			properties["operation"] = map[string]any{"type": "string", "enum": []any{"list", "get", "versions"}}
		}
		tools = append(tools, &mcp.Tool{
			Name: name, Description: "Gateway tool " + name + ".",
			InputSchema: map[string]any{"type": "object", "properties": properties, "required": []any{"query"}},
		})
	}
	return newFakeMCPServer(t, "gateway", tools, func(name string) *mcp.CallToolResult {
		payload := map[string]any{"gateway_result_from": name}
		text, err := json.Marshal(payload)
		require.NoError(t, err)
		result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}
		if release.structuredQueries && name == "grafana_query_prometheus" {
			result.StructuredContent = payload
		}
		return result
	})
}

func newSentryMCP(t *testing.T) *fakeMCPServer {
	t.Helper()
	tools := make([]*mcp.Tool, 0, len(sentryMCPToolNames))
	for _, name := range sentryMCPToolNames {
		destructive := name == "update_issue"
		tools = append(tools, &mcp.Tool{
			Name: name, Description: "Sentry tool " + name + ".",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"naturalLanguageQuery": map[string]any{"type": "string"}},
			},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !destructive, DestructiveHint: &destructive},
		})
	}
	return newFakeMCPServer(t, "sentry", tools, func(name string) *mcp.CallToolResult {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "sentry result from " + name}},
			StructuredContent: map[string]any{"tool": name},
		}
	})
}

// grafanaToolNames are the Grafana/Tempo tools the Grafana connection
// lets the investigator call, with a gateway on mcp-grafana 2.x;
// referenceConnections renames the alert-rules tool for other releases.
var grafanaToolNames = []string{
	"grafana_alerting_rules_read",
	"grafana_get_dashboard_panel_queries",
	"grafana_get_dashboard_property",
	"grafana_get_dashboard_summary",
	"grafana_get_panel_image",
	"grafana_list_loki_label_names",
	"grafana_list_loki_label_values",
	"grafana_list_prometheus_label_names",
	"grafana_list_prometheus_label_values",
	"grafana_list_prometheus_metric_metadata",
	"grafana_list_prometheus_metric_names",
	"grafana_list_pyroscope_label_names",
	"grafana_list_pyroscope_label_values",
	"grafana_list_pyroscope_profile_types",
	"grafana_query_loki_logs",
	"grafana_query_loki_patterns",
	"grafana_query_loki_stats",
	"grafana_query_prometheus",
	"grafana_query_prometheus_histogram",
	"grafana_query_pyroscope",
	"grafana_search_dashboards",
	"tempo_docs-traceql",
	"tempo_get-attribute-names",
	"tempo_get-attribute-values",
	"tempo_get-trace",
	"tempo_traceql-metrics-instant",
	"tempo_traceql-metrics-range",
	"tempo_traceql-search",
}

// referenceConnections are two tool connections as an administrator saves
// them on the Connectors page: the Grafana/Tempo tools a gateway federates
// and Sentry's hosted server, both for the observability investigator, with
// test credentials and release's alert-rules tool. No datasource is pinned.
func referenceConnections(release mcpGrafanaRelease) []askconn.Connection {
	grafanaTools := slices.Clone(grafanaToolNames)
	grafanaTools[slices.Index(grafanaTools, mcpGrafanaReleases[0].alertTool)] = release.alertTool
	grafana := settings.NewResolved(askconn.Row("grafana"), settings.Values{
		askconn.FieldKind: askconn.KindGrafana, askconn.FieldURL: "http://gateway.example.svc.cluster.local:4001/mcp",
		askconn.FieldAgents: askconn.AgentsInvestigator, askconn.FieldActorHeader: "X-Auth-Request-User",
		askconn.FieldTimeout: "2m0s", askconn.FieldTools: grafanaTools,
	}, map[string]string{askconn.FieldToken: "agw-token"})
	sentry := settings.NewResolved(askconn.Row("sentry"), settings.Values{
		askconn.FieldKind: askconn.KindSentry, askconn.FieldSentryOrganization: "example-org", askconn.FieldSentryProject: "web",
		askconn.FieldAgents: askconn.AgentsInvestigator, askconn.FieldAuthScheme: "Sentry-Bearer", askconn.FieldTimeout: "2m0s",
		askconn.FieldTools: []string{
			"search_issues", "search_errors", "search_logs", "search_traces",
			"search_metrics", "search_profiles", "search_replays", "get_sentry_resource",
		},
	}, map[string]string{askconn.FieldToken: "sentry-token"})
	return []askconn.Connection{askconn.FromResolved(grafana), askconn.FromResolved(sentry)}
}

// askMCPWiring is what serve's Ask MCP wiring hands the root agent.
type askMCPWiring struct {
	root           []ask.RegisteredTool
	child          []ask.RegisteredTool
	investigate    ask.RegisteredTool
	pagerDutyReady bool
}

// wireAskMCP builds Ask's MCP-backed tools exactly as serve does,
// dialing the fakes in place of the configured hosts. The connections attach
// every tool without a warning or error.
func wireAskMCP(t *testing.T, ctx context.Context, limits settings.AskLimits, connections []askconn.Connection, model *ask.ConfiguredModel, gateway, sentry *fakeMCPServer) askMCPWiring {
	t.Helper()
	fakes := map[string]*fakeMCPServer{"grafana": gateway, "sentry": sentry}
	require.Len(t, connections, len(fakes))
	for index := range connections {
		c := &connections[index]
		c.URL = fakes[c.Name].endpointFor(t, c.URL)
	}
	require.True(t, strings.HasSuffix(connections[1].Endpoint(), "/mcp/example-org/web?skills=inspect"))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tools := buildAskMCPTools(ctx, limits, connections, logger, askMCPToolsOptions{
		Model: model, Artifacts: &ask.ArtifactStore{}, Metrics: ask.NewMetrics(prometheus.NewRegistry()),
	})
	t.Cleanup(tools.close)
	require.Empty(t, logs.String(), "the connections need no opt-in to unmarked tools and attach cleanly")
	require.NotNil(t, tools.investigate)
	var child []ask.RegisteredTool
	for _, server := range tools.servers {
		child = append(child, server.Tools()...)
	}
	return askMCPWiring{root: tools.root, child: child, investigate: *tools.investigate, pagerDutyReady: tools.grafana}
}

type goldenTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type askMCPGolden struct {
	RootMCPTools        []goldenTool                  `json:"root_mcp_tools"`
	Investigate         goldenTool                    `json:"investigate_tool"`
	PagerDutyReady      bool                          `json:"pagerduty_investigator_ready"`
	ConnectRequests     map[string][]goldenMCPRequest `json:"connect_requests"`
	ToolCalls           map[string]goldenToolCall     `json:"tool_calls"`
	ChildPromptKeys     []string                      `json:"child_instruction_prompt_keys"`
	ChildModelRequest   map[string]json.RawMessage    `json:"child_model_request"`
	ChildReport         string                        `json:"child_report"`
	ChildMCPRequestSeen int                           `json:"child_mcp_requests"`
}

type goldenToolCall struct {
	Output   string             `json:"output"`
	Requests []goldenMCPRequest `json:"requests"`
}

func toolGolden(t *testing.T, ctx context.Context, registration ask.RegisteredTool) goldenTool {
	t.Helper()
	info, err := registration.Tool.Info(ctx)
	require.NoError(t, err)
	parameters, err := info.ToJSONSchema()
	require.NoError(t, err)
	raw, err := json.Marshal(parameters)
	require.NoError(t, err)
	return goldenTool{Name: info.Name, Description: info.Desc, Parameters: raw}
}

// childPromptKeys decomposes the child's instructions into the ordered
// embedded prompts they join, so the golden pins prompt composition without
// copying prompt prose that changes independently.
func childPromptKeys(t *testing.T, instructions string) []string {
	t.Helper()
	bundle := promptcatalog.MustEmbedded()
	var keys []string
	rest := instructions
	for rest != "" {
		best, bestLen := "", 0
		for _, prompt := range bundle.Prompts {
			if len(prompt.Body) > bestLen && strings.HasPrefix(rest, prompt.Body) {
				best, bestLen = prompt.Key, len(prompt.Body)
			}
		}
		require.NotEmpty(t, best, "child instructions contain text outside the prompt catalog: %.200q", rest)
		keys = append(keys, best)
		rest = rest[bestLen:]
		if rest != "" {
			require.True(t, strings.HasPrefix(rest, "\n\n"), "prompts must be joined by a blank line: %.200q", rest)
			rest = rest[2:]
		}
	}
	return keys
}

// TestAskMCPToolsMatchGolden pins Ask's MCP surface: with
// referenceConnections, the observability investigator's research tools,
// their names, descriptions and schemas, the MCP requests that discover and
// call them, and the child model request must match the golden. Each
// mcp-grafana release in mcpGrafanaReleases must produce the same surface but
// for the alert-rules tool's name, so a deployment can move between them in
// either direction.
func TestAskMCPToolsMatchGolden(t *testing.T) {
	newest := mcpGrafanaReleases[0]
	for _, release := range mcpGrafanaReleases {
		t.Run(release.name, func(t *testing.T) {
			encoded := recordAskMCP(t, release)
			for _, other := range mcpGrafanaReleases {
				if other.alertTool != release.alertTool {
					require.NotContains(t, string(encoded), other.alertTool)
				}
			}
			encoded = bytes.ReplaceAll(encoded, []byte(release.alertTool), []byte(newest.alertTool))
			if release == newest && os.Getenv("DIDEROT_UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.MkdirAll(filepath.Dir(askMCPGoldenPath), 0o755))
				require.NoError(t, os.WriteFile(askMCPGoldenPath, encoded, 0o644))
			}
			want, err := os.ReadFile(askMCPGoldenPath)
			require.NoError(t, err)
			require.Equal(t, string(want), string(encoded))
		})
	}
}

// recordAskMCP wires Ask's MCP surface against fakes whose gateway runs
// release, and returns the golden encoding of what it registers and sends.
func recordAskMCP(t *testing.T, release mcpGrafanaRelease) []byte {
	t.Helper()
	inference := newInferenceRecorder(t, 1024)
	gatewayInferenceEnv(t)
	gateway := newGatewayMCP(t, release)
	sentry := newSentryMCP(t)
	_, catalog := loadInference(t, openAIProvider("gateway", inference, "agw-token"))
	models := catalog.resolve(gatewayMigration(gatewayRoles))

	// Long enough for the investigator to admit a child (its reserves total
	// 4.5m); the fakes answer immediately. The child runs inside a root Ask
	// run, whose context carries the run's reasoning effort.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ctx = ask.WithReasoningEffort(ctx, gatewayMigration(gatewayRoles).AskReasoningEffort)
	model, err := ask.NewModel(ctx, ask.ModelConfig{
		BaseURL: models.ask.endpoint.BaseURL, APIKey: models.ask.endpoint.APIKey, Auth: models.ask.endpoint.Auth,
		Model: models.ask.Model, Timeout: models.askLimits.ModelTimeout, MaxTokens: models.askLimits.MaxOutputTokens, ServiceTier: models.askLimits.ServiceTier,
	})
	require.NoError(t, err)

	wiring := wireAskMCP(t, ctx, models.askLimits, referenceConnections(release), model, gateway, sentry)
	got := askMCPGolden{
		RootMCPTools:    []goldenTool{},
		Investigate:     toolGolden(t, ctx, wiring.investigate),
		PagerDutyReady:  wiring.pagerDutyReady,
		ConnectRequests: map[string][]goldenMCPRequest{"gateway": gateway.drain(), "sentry": sentry.drain()},
		ToolCalls:       map[string]goldenToolCall{},
	}
	for _, registration := range wiring.root {
		got.RootMCPTools = append(got.RootMCPTools, toolGolden(t, ctx, registration))
	}

	// One child turn: the recorder answers with a report and no tool calls,
	// so the request carries the child's full instructions and tool list.
	invokable, ok := wiring.investigate.Tool.(tool.InvokableTool)
	require.True(t, ok)
	report, err := invokable.InvokableRun(ctx, `{"request":"Why did the upload path start returning 500s?","time_window_utc":{"from":"now-1h","to":"now"}}`)
	require.NoError(t, err)
	got.ChildReport = report
	child := inference.last()
	require.Equal(t, "/v1/responses", child.Path)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(child.Raw, &body))
	// The child's instructions are its leading system message.
	var input []map[string]any
	require.NoError(t, json.Unmarshal(body["input"], &input))
	require.NotEmpty(t, input)
	require.Equal(t, "system", input[0]["role"])
	instructions, ok := input[0]["content"].(string)
	require.True(t, ok)
	got.ChildPromptKeys = childPromptKeys(t, instructions)
	input[0]["content"] = "<child_instruction_prompt_keys>"
	body["input"], err = json.Marshal(input)
	require.NoError(t, err)
	got.ChildModelRequest = body
	got.ChildMCPRequestSeen = len(gateway.drain()) + len(sentry.drain())

	// Each server's adapted tools call through with the server's credential
	// and normalize results their own way.
	childTools := map[string]tool.InvokableTool{}
	for _, registration := range wiring.child {
		info, err := registration.Tool.Info(ctx)
		require.NoError(t, err)
		if invokable, ok := registration.Tool.(tool.InvokableTool); ok {
			childTools[info.Name] = invokable
		}
	}
	for name, fake := range map[string]*fakeMCPServer{"grafana_query_prometheus": gateway, "search_issues": sentry} {
		output, err := childTools[name].InvokableRun(ctx, `{"query":"rate(errors[5m])"}`)
		require.NoError(t, err)
		got.ToolCalls[name] = goldenToolCall{Output: output, Requests: fake.drain()}
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	require.NoError(t, err)
	return append(encoded, '\n')
}
