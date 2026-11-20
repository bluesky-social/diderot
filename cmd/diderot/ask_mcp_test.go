package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/settings"
)

// A connection's tools may not shadow one of Ask's own; askconn's reserved
// list must keep up with the tools ask builds.
func TestToolConnectionsReserveEveryBuiltinTool(t *testing.T) {
	infos, err := ask.BuiltinToolInfos(t.Context(), ask.Wording{}, sourceRegistry(nil, nil, nil, nil, nil, nil, false).Infos())
	require.NoError(t, err)
	names := []string{ask.InvestigateToolName}
	for _, info := range infos {
		names = append(names, info.Name)
	}
	def := askconn.Definitions{}.Define(askconn.Row("s"))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			err := def.Validate(settings.Values{
				askconn.FieldKind: askconn.KindGeneric, askconn.FieldAgents: askconn.AgentsAsk, askconn.FieldTools: []string{name},
			})
			require.ErrorContains(t, err, "one of Ask's own tools")
		})
	}
}

// testConnection is a generic connection to url that gives tools to agents.
func testConnection(name, url, agents string, tools []string) askconn.Connection {
	return askconn.Connection{
		Name: name, Kind: askconn.KindGeneric, URL: url, Agents: agents, Tools: tools,
		AuthScheme: "Bearer", Timeout: time.Minute, Datasources: map[string]string{},
	}
}

func askMCPTestTool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name: name, Description: "Tool " + name + ".",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
}

func newAskMCPTestServer(t *testing.T, names ...string) *fakeMCPServer {
	t.Helper()
	tools := make([]*mcp.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, askMCPTestTool(name))
	}
	return newFakeMCPServer(t, "test", tools, func(name string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result from " + name}}}
	})
}

func askMCPRegistrationNames(t *testing.T, registrations []ask.RegisteredTool) []string {
	t.Helper()
	names := make([]string, 0, len(registrations))
	for _, registration := range registrations {
		info, err := registration.Tool.Info(t.Context())
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	return names
}

func TestBuildAskMCPToolsPlacesConnectionsByAgent(t *testing.T) {
	ctx := context.Background()
	tickets := newAskMCPTestServer(t, "search_tickets", "get_ticket")
	metrics := newAskMCPTestServer(t, "query_metrics")
	model, _ := newAskMCPTestModel(t)
	options := askMCPToolsOptions{Model: model, Metrics: ask.NewMetrics(prometheus.NewRegistry())}
	limits := settings.AskLimits{}

	t.Run("ask and generic investigator", func(t *testing.T) {
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("tickets", tickets.server.URL, askconn.AgentsAsk, []string{"search_tickets"}),
			testConnection("metrics", metrics.server.URL, askconn.AgentsInvestigator, []string{"query_metrics"}),
		}, testLogger(), options)
		t.Cleanup(tools.close)
		require.Equal(t, []string{"search_tickets"}, askMCPRegistrationNames(t, tools.root))
		require.NotNil(t, tools.investigate)
		require.False(t, tools.grafana, "PagerDuty needs a Grafana connection, not any investigator connection")
		require.Len(t, tools.servers, 2)
		require.False(t, tools.failed)
	})

	t.Run("failed connection is left out", func(t *testing.T) {
		var logs bytes.Buffer
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("more_tickets", tickets.server.URL, askconn.AgentsAsk, []string{"get_ticket"}),
			testConnection("tickets", tickets.server.URL, askconn.AgentsAsk, []string{"search_tickets", "missing_tool"}),
		}, slog.New(slog.NewTextHandler(&logs, nil)), options)
		t.Cleanup(tools.close)
		require.Equal(t, []string{"get_ticket"}, askMCPRegistrationNames(t, tools.root))
		require.Contains(t, logs.String(), `msg="tool connection not attached; ordinary Ask remains available" connection=tickets`)
		require.Contains(t, logs.String(), "missing missing_tool")
		require.Nil(t, tools.investigate)
		require.True(t, tools.failed, "the replica tries a failed connection again later")
	})

	// Two connections may not give one tool: the first by name keeps it.
	t.Run("a tool another connection gives", func(t *testing.T) {
		var logs bytes.Buffer
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("first", tickets.server.URL, askconn.AgentsAsk, []string{"search_tickets"}),
			testConnection("second", tickets.server.URL, askconn.AgentsAsk, []string{"get_ticket", "search_tickets"}),
		}, slog.New(slog.NewTextHandler(&logs, nil)), options)
		t.Cleanup(tools.close)
		require.Equal(t, []string{"search_tickets"}, askMCPRegistrationNames(t, tools.root))
		require.Contains(t, logs.String(), "connection=second other=first")
		require.True(t, tools.failed)
	})

	// A tool Ask's registry would reject must not take all of Ask with it:
	// the connection is left out at connect time.
	t.Run("tool Ask cannot register", func(t *testing.T) {
		undescribed := askMCPTestTool("search_wiki")
		undescribed.Description = ""
		wiki := newFakeMCPServer(t, "wiki", []*mcp.Tool{undescribed}, func(string) *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}
		})
		var logs bytes.Buffer
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("tickets", tickets.server.URL, askconn.AgentsAsk, []string{"search_tickets"}),
			testConnection("wiki", wiki.server.URL, askconn.AgentsAsk, []string{"search_wiki"}),
		}, slog.New(slog.NewTextHandler(&logs, nil)), options)
		t.Cleanup(tools.close)
		require.Equal(t, []string{"search_tickets"}, askMCPRegistrationNames(t, tools.root))
		require.Len(t, tools.servers, 1)
		require.Contains(t, logs.String(), "connection=wiki")
		require.Contains(t, logs.String(), "description is empty")
		_, _, err := ask.NewToolRegistry(ctx, tools.root)
		require.NoError(t, err)
	})

	t.Run("no connections, no investigator", func(t *testing.T) {
		tools := buildAskMCPTools(ctx, limits, nil, testLogger(), options)
		require.Nil(t, tools.investigate)
		require.Empty(t, tools.root)
		require.Empty(t, tools.servers)
		require.False(t, tools.failed)
	})
}

// A connection both agents use is connected and admitted once, and its
// tools reach both Ask and the investigator over that one connection.
func TestBuildAskMCPToolsConnectionOnBothAgents(t *testing.T) {
	ctx := context.Background()
	model, inference := newAskMCPTestModel(t)
	options := askMCPToolsOptions{Model: model, Metrics: ask.NewMetrics(prometheus.NewRegistry())}
	limits := settings.AskLimits{}

	t.Run("tools on both agents", func(t *testing.T) {
		changes := newAskMCPTestServer(t, "list_changes")
		metrics := newAskMCPTestServer(t, "query_metrics")
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("changes", changes.server.URL, askconn.AgentsBoth, []string{"list_changes"}),
			testConnection("metrics", metrics.server.URL, askconn.AgentsInvestigator, []string{"query_metrics"}),
		}, testLogger(), options)
		t.Cleanup(tools.close)
		require.Equal(t, []string{"list_changes"}, askMCPRegistrationNames(t, tools.root))
		require.NotNil(t, tools.investigate)
		require.Equal(t, []string{"list_changes", "query_metrics"}, investigatorToolNames(t, *tools.investigate, inference))
		require.Len(t, tools.servers, 2, "one connection per server, not per agent")
		// One discovery, and the two tool listings of one admission, exactly
		// as for a connection one agent uses.
		require.Equal(t, []string{"server/discover", "tools/list", "tools/list"}, mcpRPCMethods(changes.drain()))
	})

	t.Run("server down at start is left out of both", func(t *testing.T) {
		down := newAskMCPTestServer(t, "list_changes")
		down.server.Close()
		metrics := newAskMCPTestServer(t, "query_metrics")
		var logs bytes.Buffer
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("changes", down.server.URL, askconn.AgentsBoth, []string{"list_changes"}),
			testConnection("metrics", metrics.server.URL, askconn.AgentsInvestigator, []string{"query_metrics"}),
		}, slog.New(slog.NewTextHandler(&logs, nil)), options)
		t.Cleanup(tools.close)
		require.Empty(t, tools.root)
		require.NotNil(t, tools.investigate)
		require.Equal(t, []string{"query_metrics"}, investigatorToolNames(t, *tools.investigate, inference))
		require.Len(t, tools.servers, 1)
		require.Contains(t, logs.String(), `msg="tool connection not attached; ordinary Ask remains available" connection=changes agents=both`)
	})

	t.Run("sole investigator connection down attaches no investigator", func(t *testing.T) {
		down := newAskMCPTestServer(t, "list_changes")
		down.server.Close()
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("changes", down.server.URL, askconn.AgentsBoth, []string{"list_changes"}),
		}, testLogger(), options)
		require.Empty(t, tools.root)
		require.Nil(t, tools.investigate)
		require.Empty(t, tools.servers)
	})

	// The investigator failing to build closes the connections only it
	// used; a connection Ask shares stays open for Ask.
	t.Run("investigator failure keeps Ask's connection open", func(t *testing.T) {
		changes := newAskMCPTestServer(t, "list_changes")
		metrics := newAskMCPTestServer(t, "query_metrics")
		tools := buildAskMCPTools(ctx, limits, []askconn.Connection{
			testConnection("changes", changes.server.URL, askconn.AgentsBoth, []string{"list_changes"}),
			testConnection("metrics", metrics.server.URL, askconn.AgentsInvestigator, []string{"query_metrics"}),
		}, testLogger(), askMCPToolsOptions{Metrics: options.Metrics})
		t.Cleanup(tools.close)
		require.Nil(t, tools.investigate, "an investigator needs a model")
		require.Len(t, tools.servers, 1)
		require.Equal(t, "changes", tools.servers[0].Name())
		invokable, ok := tools.root[0].Tool.(tool.InvokableTool)
		require.True(t, ok)
		output, err := invokable.InvokableRun(ctx, `{"query":"uploads"}`)
		require.NoError(t, err)
		require.Contains(t, output, "result from list_changes")
	})
}

func mcpRPCMethods(requests []goldenMCPRequest) []string {
	methods := make([]string, 0, len(requests))
	for _, request := range requests {
		methods = append(methods, request.RPCMethod)
	}
	return methods
}

// investigatorToolNames runs the investigator once against the inference
// recorder, which answers with a report and no tool calls, and returns the
// research tools the child's model request offered.
func investigatorToolNames(t *testing.T, investigate ask.RegisteredTool, inference *inferenceRecorder) []string {
	t.Helper()
	invokable, ok := investigate.Tool.(tool.InvokableTool)
	require.True(t, ok)
	ctx := ask.WithReasoningEffort(t.Context(), gatewayMigration(gatewayRoles).AskReasoningEffort)
	_, err := invokable.InvokableRun(ctx, `{"request":"Which changes preceded the upload error spike?"}`)
	require.NoError(t, err)
	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(inference.last().Raw, &body))
	names := make([]string, 0, len(body.Tools))
	for _, offered := range body.Tools {
		names = append(names, offered.Name)
	}
	return names
}

// newAskMCPTestModel builds a model whose requests the returned recorder
// answers with a short text reply.
func newAskMCPTestModel(t *testing.T) (*ask.ConfiguredModel, *inferenceRecorder) {
	t.Helper()
	inference := newInferenceRecorder(t, 1024)
	gatewayInferenceEnv(t)
	_, catalog := loadInference(t, openAIProvider("gateway", inference, "agw-token"))
	models := catalog.resolve(gatewayMigration(gatewayRoles))
	model, err := ask.NewModel(t.Context(), ask.ModelConfig{
		BaseURL: models.ask.endpoint.BaseURL, APIKey: models.ask.endpoint.APIKey, Auth: models.ask.endpoint.Auth,
		Model: models.ask.Model, MaxTokens: 1024,
	})
	require.NoError(t, err)
	return model, inference
}
