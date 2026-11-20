package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// testObservabilityTools is a grafana allowlist covering both special-cased
// tools and ordinary ones.
var testObservabilityTools = []string{
	alertRulesToolName,
	PanelImageToolName,
	"grafana_query_prometheus",
	"grafana_query_loki_logs",
	"tempo_traceql-search",
}

// unlistedObservabilityTool is served by the test server but in no allowlist.
const unlistedObservabilityTool = "grafana_update_dashboard"

// observabilityMCPTestServer serves an allowlist's tools plus
// unlistedObservabilityTool over the official SDK's streamable-HTTP handler,
// and records the headers of every request and the arguments of every
// panel-image call.
type observabilityMCPTestServer struct {
	server    *httptest.Server
	mu        sync.Mutex
	headers   []string
	panelArgs []json.RawMessage
}

func newObservabilityMCPTestServer(t *testing.T, panelPNG []byte) *observabilityMCPTestServer {
	t.Helper()
	return newObservabilityMCPTestServerWith(t, panelPNG, observabilityTestServerOptions{})
}

type observabilityTestServerOptions struct {
	// served defaults to testObservabilityTools.
	served []string
	// writableAlerts adds a write operation to the alert-rules tool's schema.
	writableAlerts bool
	// annotations are the per-tool annotations the server reports; a tool
	// without an entry reports none.
	annotations map[string]*mcp.ToolAnnotations
	// panelStructured is the panel tool's structured content, and
	// panelOmitText drops its deep-link text block.
	panelStructured any
	panelOmitText   bool
}

// testPanelDeepLink is the deep link the test server's panel tool returns.
const testPanelDeepLink = "https://grafana.example/d/abc"

func newObservabilityMCPTestServerWith(t *testing.T, panelPNG []byte, opts observabilityTestServerOptions) *observabilityMCPTestServer {
	t.Helper()
	harness := &observabilityMCPTestServer{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "observability-test", Version: "1"}, nil)
	objectSchema := map[string]any{"type": "object"}
	alertOperations := []any{"list", "get", "versions"}
	if opts.writableAlerts {
		alertOperations = append(alertOperations, "create")
	}
	served := opts.served
	if served == nil {
		served = testObservabilityTools
	}
	if !slices.Contains(served, unlistedObservabilityTool) {
		served = append(slices.Clone(served), unlistedObservabilityTool)
	}
	for _, name := range served {
		remote := &mcp.Tool{Name: name, Description: "test " + name, InputSchema: objectSchema, Annotations: opts.annotations[name]}
		switch name {
		case alertRulesToolName:
			remote.InputSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation": map[string]any{"type": "string", "enum": alertOperations},
				},
			}
		case PanelImageToolName:
			srv.AddTool(remote, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				harness.mu.Lock()
				harness.panelArgs = append(harness.panelArgs, req.Params.Arguments)
				harness.mu.Unlock()
				content := []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: panelPNG}}
				if !opts.panelOmitText {
					content = append(content, &mcp.TextContent{Text: testPanelDeepLink})
				}
				return &mcp.CallToolResult{Content: content, StructuredContent: opts.panelStructured}, nil
			})
			continue
		}
		// Like mcp-grafana's query_prometheus since 2.1, an ordinary tool
		// returns its JSON payload as text and again as structured content.
		payload := map[string]any{"result_from": name}
		text, err := json.Marshal(payload)
		require.NoError(t, err)
		srv.AddTool(remote, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
				StructuredContent: payload,
			}, nil
		})
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true})
	harness.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.mu.Lock()
		harness.headers = append(harness.headers, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Forwarded-User"))
		harness.mu.Unlock()
		transport.ServeHTTP(w, r)
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

func TestGrafanaMCPDiscoversToolsAndSendsGatewayIdentity(t *testing.T) {
	harness := newObservabilityMCPTestServer(t, testPanelPNG(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, err := ConnectMCPToolServer(ctx, MCPToolServerOptions{
		Name:        "grafana",
		Kind:        MCPServerGrafana,
		Endpoint:    harness.server.URL,
		Token:       "secret",
		ActorHeader: "X-Forwarded-User",
		Tools:       testObservabilityTools,
		Timeout:     10 * time.Second,
		Artifacts:   &ArtifactStore{blob: &memoryArtifactBlob{}}, // panel calls are covered in the DB test
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	registrations := remote.Tools()
	require.Equal(t, "grafana", remote.Name())
	require.Equal(t, MCPServerGrafana, remote.Kind())
	require.ElementsMatch(t, testObservabilityTools, registeredToolNames(t, registrations))

	var ordinary tool.InvokableTool
	for _, registration := range registrations {
		info, err := registration.Tool.Info(ctx)
		require.NoError(t, err)
		if info.Name == "grafana_query_prometheus" {
			ordinary = registration.Tool.(tool.InvokableTool)
		}
	}
	require.NotNil(t, ordinary)
	out, err := ordinary.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Len(t, result.Content, 1)
	require.Equal(t, "text", result.Content[0].Type)
	require.Equal(t, `{"result_from":"grafana_query_prometheus"}`, result.Content[0].Text, "the payload reaches the model once")

	harness.mu.Lock()
	defer harness.mu.Unlock()
	require.NotEmpty(t, harness.headers)
	for _, header := range harness.headers {
		require.Equal(t, "Bearer secret|svc:diderot", header)
	}
}

// The panel image arrives as base64 on the wire; the SDK hands render the
// decoded bytes, which must match the PNG the server sent.
func TestGrafanaMCPPanelImageRender(t *testing.T) {
	png := testPanelPNG(t)
	harness := newObservabilityMCPTestServer(t, png)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, err := ConnectMCPToolServer(ctx, MCPToolServerOptions{
		Name:        "grafana",
		Kind:        MCPServerGrafana,
		Endpoint:    harness.server.URL,
		Token:       "secret",
		ActorHeader: "X-Forwarded-User",
		Tools:       testObservabilityTools,
		Timeout:     10 * time.Second,
		Artifacts:   &ArtifactStore{blob: &memoryArtifactBlob{}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	var panel *panelImageMCPTool
	for _, registration := range remote.Tools() {
		if typed, ok := registration.Tool.(*panelImageMCPTool); ok {
			panel = typed
		}
	}
	require.NotNil(t, panel)
	arguments := map[string]any{"dashboardUid": "abc", "panelId": 2, "timeRange": map[string]any{"from": "1786579200000", "to": "now"}}
	data, deepLink, err := panel.render(ctx, arguments)
	require.NoError(t, err)
	require.Equal(t, png, data)
	require.Equal(t, "https://grafana.example/d/abc", deepLink)

	harness.mu.Lock()
	defer harness.mu.Unlock()
	require.Len(t, harness.panelArgs, 1)
	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(harness.panelArgs[0], &forwarded))
	require.Equal(t, "abc", forwarded["dashboardUid"])
	require.Equal(t, "1786579200000", forwarded["timeRange"].(map[string]any)["from"])
}

// mcp-grafana 1.6 and later return the deep link twice, as a text block and
// as {"deeplink": url} structured content. That shape renders; any other
// structured content, or a second link that disagrees, is refused.
func TestGrafanaMCPPanelImageStructuredDeepLink(t *testing.T) {
	for _, tc := range []struct {
		name       string
		structured any
		omitText   bool
		wantLink   string
		wantErr    string
	}{
		{name: "text and structured link", structured: map[string]any{"deeplink": testPanelDeepLink}, wantLink: testPanelDeepLink},
		{name: "structured link only", structured: map[string]any{"deeplink": testPanelDeepLink}, omitText: true, wantLink: testPanelDeepLink},
		{name: "neither", omitText: true, wantLink: ""},
		{name: "conflicting links", structured: map[string]any{"deeplink": "https://grafana.example/d/other"}, wantErr: "conflicting deep links"},
		{name: "extra structured field", structured: map[string]any{"deeplink": testPanelDeepLink, "panel": 2}, wantErr: "unexpected structured content"},
		{name: "non-string link", structured: map[string]any{"deeplink": 2}, wantErr: "unexpected structured content"},
		{name: "other structured content", structured: map[string]any{"rows": []any{}}, wantErr: "unexpected structured content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			png := testPanelPNG(t)
			harness := newObservabilityMCPTestServerWith(t, png, observabilityTestServerOptions{
				panelStructured: tc.structured, panelOmitText: tc.omitText,
			})
			registrations, err := connectObservabilityMCP(t, harness, MCPToolServerOptions{Tools: []string{PanelImageToolName}})
			require.NoError(t, err)
			require.Len(t, registrations, 1)
			panel := registrations[0].Tool.(*panelImageMCPTool)
			data, deepLink, err := panel.render(t.Context(), map[string]any{
				"dashboardUid": "abc", "panelId": 2, "timeRange": map[string]any{"from": "now-1h", "to": "now"},
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, png, data)
			require.Equal(t, tc.wantLink, deepLink)
		})
	}
}

func registeredToolNames(t *testing.T, registrations []RegisteredTool) []string {
	t.Helper()
	names := make([]string, 0, len(registrations))
	for _, registration := range registrations {
		info, err := registration.Tool.Info(t.Context())
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	return names
}

// The allowlist is operator configuration: exactly the listed tools are
// exposed, the panel and alert-rule special cases apply only when listed, and
// a listed tool the server lacks fails discovery.
func TestGrafanaMCPHonorsConfiguredAllowlist(t *testing.T) {
	connect := func(t *testing.T, harness *observabilityMCPTestServer, tools []string) ([]RegisteredTool, error) {
		t.Helper()
		return connectObservabilityMCP(t, harness, MCPToolServerOptions{Tools: tools})
	}

	t.Run("subset without special cases", func(t *testing.T) {
		// The served alert-rules tool is writable, but it is not listed, so it
		// is neither checked nor exposed.
		harness := newObservabilityMCPTestServerWith(t, testPanelPNG(t), observabilityTestServerOptions{writableAlerts: true})
		tools := []string{"grafana_query_prometheus", "tempo_traceql-search"}
		registrations, err := connect(t, harness, tools)
		require.NoError(t, err)
		require.ElementsMatch(t, tools, registeredToolNames(t, registrations))
	})

	t.Run("panel only", func(t *testing.T) {
		// Adapting zero ordinary tools must not fall through to adapting
		// every tool the server lists.
		harness := newObservabilityMCPTestServer(t, testPanelPNG(t))
		registrations, err := connect(t, harness, []string{PanelImageToolName})
		require.NoError(t, err)
		require.Equal(t, []string{PanelImageToolName}, registeredToolNames(t, registrations))
		require.IsType(t, &panelImageMCPTool{}, registrations[0].Tool)
	})

	t.Run("listed alert rules must be read-only", func(t *testing.T) {
		harness := newObservabilityMCPTestServerWith(t, testPanelPNG(t), observabilityTestServerOptions{writableAlerts: true})
		_, err := connect(t, harness, testObservabilityTools)
		require.ErrorContains(t, err, "is not read-only")
	})

	// The alert-rules tool attaches on its review alone (the test server
	// reports no annotations) with the read-only enum, and is refused by name
	// with a write operation.
	for _, alertTool := range []string{alertRulesToolName} {
		tools := []string{alertTool, "grafana_query_prometheus"}
		t.Run("alert rules as "+alertTool, func(t *testing.T) {
			harness := newObservabilityMCPTestServerWith(t, testPanelPNG(t), observabilityTestServerOptions{served: tools})
			registrations, err := connect(t, harness, tools)
			require.NoError(t, err)
			require.ElementsMatch(t, tools, registeredToolNames(t, registrations))

			writable := newObservabilityMCPTestServerWith(t, testPanelPNG(t), observabilityTestServerOptions{served: tools, writableAlerts: true})
			_, err = connect(t, writable, tools)
			require.ErrorContains(t, err, alertTool+" is not read-only: operations=[list get versions create]")
		})
	}

	t.Run("missing tool", func(t *testing.T) {
		harness := newObservabilityMCPTestServer(t, testPanelPNG(t))
		_, err := connect(t, harness, []string{"grafana_query_prometheus", "grafana_query_pyroscope"})
		require.ErrorContains(t, err, "tool allowlist is incomplete; missing grafana_query_pyroscope")
	})

	t.Run("allowlist required", func(t *testing.T) {
		harness := newObservabilityMCPTestServer(t, testPanelPNG(t))
		_, err := connect(t, harness, nil)
		require.ErrorContains(t, err, "tool allowlist is required")
	})

	t.Run("panel needs the artifact store", func(t *testing.T) {
		harness := newObservabilityMCPTestServer(t, testPanelPNG(t))
		_, err := ConnectMCPToolServer(t.Context(), MCPToolServerOptions{
			Name: "grafana", Kind: MCPServerGrafana, Endpoint: harness.server.URL, Tools: []string{PanelImageToolName},
		})
		require.ErrorContains(t, err, "needs the artifact store")
		// Without the panel tool, no store is needed.
		registrations, err := connectObservabilityMCP(t, harness, MCPToolServerOptions{Tools: []string{"grafana_query_prometheus"}, Artifacts: nil})
		require.NoError(t, err)
		require.Len(t, registrations, 1)
	})
}

// connectObservabilityMCP connects a grafana server to harness. A nil
// opts.Artifacts gets an in-memory store unless the allowlist omits the panel
// tool.
func connectObservabilityMCP(t *testing.T, harness *observabilityMCPTestServer, opts MCPToolServerOptions) ([]RegisteredTool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	opts.Name, opts.Kind = "grafana", MCPServerGrafana
	opts.Endpoint, opts.Token, opts.Timeout = harness.server.URL, "secret", 10*time.Second
	if opts.Artifacts == nil && slices.Contains(opts.Tools, PanelImageToolName) {
		opts.Artifacts = &ArtifactStore{blob: &memoryArtifactBlob{}}
	}
	remote, err := ConnectMCPToolServer(ctx, opts)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	return remote.Tools(), nil
}

// The investigator calls its tools with Diderot's credential on behalf of any
// Ask caller, so a configurable allowlist must not become a way to hand it
// write tools. A tool outside the reviewed read-only set needs a read-only,
// non-destructive annotation, or the operator's explicit opt-in.
func TestGrafanaMCPAdmitsOnlyReadOnlyTools(t *testing.T) {
	const annotatedTool = "grafana_list_datasources"
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	explicitlySafe := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false)}
	destructive := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(true)}
	writable := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: new(false)}
	served := append(slices.Clone(testObservabilityTools), annotatedTool)

	for _, tc := range []struct {
		name        string
		annotations map[string]*mcp.ToolAnnotations
		tools       []string
		allow       bool
		wantErr     string
		wantWarned  []string
	}{
		{name: "unannotated unreviewed tool refused", tools: []string{"grafana_query_prometheus", unlistedObservabilityTool},
			wantErr: `ask MCP server "grafana": tools grafana_update_dashboard are not reviewed read-only tools`},
		{name: "explicitly writable tool refused", annotations: map[string]*mcp.ToolAnnotations{unlistedObservabilityTool: writable},
			tools: []string{unlistedObservabilityTool}, wantErr: "grafana_update_dashboard are not reviewed"},
		{name: "destructive tool refused", annotations: map[string]*mcp.ToolAnnotations{annotatedTool: destructive},
			tools: []string{annotatedTool}, wantErr: "grafana_list_datasources are not reviewed"},
		{name: "read-only annotation admits", annotations: map[string]*mcp.ToolAnnotations{annotatedTool: readOnly},
			tools: []string{"grafana_query_prometheus", annotatedTool}},
		{name: "explicitly non-destructive annotation admits", annotations: map[string]*mcp.ToolAnnotations{annotatedTool: explicitlySafe},
			tools: []string{annotatedTool}},
		// The review, not the server's self-description, makes these
		// read-only, so a reviewed tool attaches whatever its server reports.
		{name: "reviewed tools admitted despite annotations", annotations: map[string]*mcp.ToolAnnotations{
			"grafana_query_prometheus": writable, "tempo_traceql-search": destructive}, tools: testObservabilityTools},
		{name: "opt-in admits and warns per tool", annotations: map[string]*mcp.ToolAnnotations{annotatedTool: destructive},
			tools: []string{"grafana_query_prometheus", unlistedObservabilityTool, annotatedTool}, allow: true,
			wantWarned: []string{unlistedObservabilityTool, annotatedTool}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newObservabilityMCPTestServerWith(t, testPanelPNG(t), observabilityTestServerOptions{served: served, annotations: tc.annotations})
			var logs bytes.Buffer
			registrations, err := connectObservabilityMCP(t, harness, MCPToolServerOptions{
				Tools: tc.tools, AllowUnreviewedTools: tc.allow, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, registrations)
				return
			}
			require.NoError(t, err)
			require.ElementsMatch(t, tc.tools, registeredToolNames(t, registrations))
			var warned []string
			for line := range strings.Lines(logs.String()) {
				var record struct {
					Level string `json:"level"`
					Tool  string `json:"tool"`
				}
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				require.Equal(t, "WARN", record.Level)
				warned = append(warned, record.Tool)
			}
			require.ElementsMatch(t, tc.wantWarned, warned)
		})
	}
}
