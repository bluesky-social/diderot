package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/tavily"
)

// fixedWeb is a web client that never changes.
func fixedWeb(client WebClient) func() WebClient {
	return func() WebClient { return client }
}

// fixedCode is a code index that never changes.
func fixedCode(client CodeClient, ix CodeIndex) func() (CodeClient, CodeIndex) {
	return func() (CodeClient, CodeIndex) { return client, ix }
}

// settingsLike stands in for the cached connector settings the production
// providers read: a pointer swapped between runs.
type settingsLike[T any] struct{ current atomic.Pointer[T] }

func (s *settingsLike[T]) set(value *T) { s.current.Store(value) }

// countingWebClient counts every call that reaches the web.
type countingWebClient struct{ calls atomic.Int32 }

func (c *countingWebClient) Search(context.Context, tavily.SearchParams) (*tavily.SearchResult, error) {
	c.calls.Add(1)
	return &tavily.SearchResult{}, nil
}

func (c *countingWebClient) Extract(context.Context, []string, string) (*tavily.ExtractResult, error) {
	c.calls.Add(1)
	return &tavily.ExtractResult{}, nil
}

type codeSettings struct {
	client CodeClient
	index  CodeIndex
}

// TestRunsOfferOnlyAvailableTools proves that each run assembles its tools
// from the settings current as it starts: web and code tools appear and
// disappear between runs of the same agents, the code tools' wording follows
// the settings, and a run never lists, or runs, a tool that was off as it
// started.
func TestRunsOfferOnlyAvailableTools(t *testing.T) {
	type modelRequest struct {
		names        []string
		descriptions map[string]string
	}
	var (
		mu       sync.Mutex
		requests []modelRequest
		forge    atomic.Bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		seen := modelRequest{descriptions: map[string]string{}}
		for _, advertised := range body.Tools {
			seen.names = append(seen.names, advertised.Name)
			seen.descriptions[advertised.Name] = advertised.Description
		}
		mu.Lock()
		requests = append(requests, seen)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if forge.CompareAndSwap(true, false) {
			// The model calls a tool the run doesn't offer.
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_forged", "object": "response", "created_at": 1, "status": "completed", "model": "test",
				"output": []any{map[string]any{"type": "function_call", "id": "fc_web", "call_id": "call_web",
					"name": "web_search", "arguments": `{"query":"q"}`, "status": "completed"}},
				"usage": responsesUsage(10, 3, 1),
			})
			return
		}
		writeAskAgentsFixture(t, w, "resp_final", "done")
	}))
	defer server.Close()

	lookup, err := utils.InferTool("lookup", "always offered", func(context.Context, struct{}) (string, error) { return "", nil })
	require.NoError(t, err)
	var web settingsLike[countingWebClient]
	var code settingsLike[codeSettings]
	currentWeb := func() WebClient {
		if client := web.current.Load(); client != nil {
			return client
		}
		return nil
	}
	currentCode := func() (CodeClient, CodeIndex) {
		if s := code.current.Load(); s != nil {
			return s.client, s.index
		}
		return nil, CodeIndex{}
	}
	registrations := []RegisteredTool{
		NewEinoWebSearchTool(currentWeb), NewEinoWebFetchTool(currentWeb),
		NewEinoCodeSearchTool(currentCode), NewEinoCodeReadTool(currentCode),
	}
	tools := []tool.BaseTool{lookup}
	for _, registration := range registrations {
		tools = append(tools, registration.Tool)
	}
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), tools, "fixed prompt", 5)
	require.NoError(t, err)
	run := func() modelRequest {
		t.Helper()
		mu.Lock()
		before := len(requests)
		mu.Unlock()
		drainAgentRun(t, agents.HTTP)
		mu.Lock()
		defer mu.Unlock()
		require.Greater(t, len(requests), before)
		for _, request := range requests[before+1:] {
			require.Equal(t, requests[before].names, request.names, "a run keeps the tools it started with")
		}
		return requests[before]
	}

	require.Equal(t, []string{"lookup"}, run().names, "both connectors off")

	tavilyClient := &countingWebClient{}
	web.set(tavilyClient)
	require.Equal(t, []string{"lookup", "web_search", "web_fetch"}, run().names, "web search enabled")

	code.set(&codeSettings{client: &stubCodeClient{}, index: CodeIndex{SearchScope: "every Example repository", ReadScope: "an Example repository"}})
	both := run()
	require.Equal(t, []string{"lookup", "web_search", "web_fetch", "code_search", "code_read"}, both.names)
	require.Contains(t, both.descriptions["code_search"], "Search the code of every Example repository.")
	require.Contains(t, both.descriptions["code_read"], "Read the full content of ONE file from an Example repository.")

	code.set(&codeSettings{client: &stubCodeClient{}, index: CodeIndex{SearchScope: "the Widgets monorepo"}})
	web.set(nil)
	forge.Store(true)
	codeOnly := run()
	require.Equal(t, []string{"lookup", "code_search", "code_read"}, codeOnly.names, "web search disabled")
	require.Contains(t, codeOnly.descriptions["code_search"], "Search the code of the Widgets monorepo.")
	require.Contains(t, codeOnly.descriptions["code_read"], DefaultCodeReadScope, "a cleared field reads neutrally")
	require.False(t, forge.Load(), "the model was given its forged call")
	require.Zero(t, tavilyClient.calls.Load(), "a tool the run doesn't offer never runs")
}

func drainAgentRun(t *testing.T, runner *adk.TypedRunner[*schema.AgenticMessage]) {
	t.Helper()
	iter := runner.Query(context.Background(), "a question")
	for {
		event, ok := iter.Next()
		if !ok {
			return
		}
		require.NoError(t, event.Err)
	}
}

// TestToolPolicyAsksAvailabilityAsEachRunStarts checks the run assembly
// itself: unavailable optional tools are left out, others kept, and the
// agent's own list is not modified.
func TestToolPolicyAsksAvailabilityAsEachRunStarts(t *testing.T) {
	var on atomic.Bool
	optional := &optionalTool{InvokableTool: mustInferTool(t, "optional"), available: on.Load}
	optional.info = optional.InvokableTool.Info
	always := mustInferTool(t, "always")
	wrapped, err := WrapRecoverableToolErrors([]tool.BaseTool{always, optional}, RootToolResultMaxBytes)
	require.NoError(t, err)
	middleware := newAgenticToolPolicyMiddleware()
	names := func() []string {
		_, runCtx, err := middleware.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{Tools: wrapped})
		require.NoError(t, err)
		var names []string
		for _, candidate := range runCtx.Tools {
			info, err := candidate.Info(context.Background())
			require.NoError(t, err)
			names = append(names, info.Name)
		}
		return names
	}
	require.Equal(t, []string{"always"}, names())
	on.Store(true)
	require.Equal(t, []string{"always", "optional"}, names())
	require.Len(t, wrapped, 2)
}

func mustInferTool(t *testing.T, name string) tool.InvokableTool {
	t.Helper()
	invokable, err := utils.InferTool(name, "test tool", func(context.Context, struct{}) (string, error) { return "", nil })
	require.NoError(t, err)
	return invokable
}
