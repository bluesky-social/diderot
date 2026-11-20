package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

func TestCorpusOnlyToolDeclarationsArePerTurn(t *testing.T) {
	middleware := newAgenticToolPolicyMiddleware()
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		ToolInfos:         []*schema.ToolInfo{{Name: "retrieve"}, {Name: "list"}, {Name: "expand"}, {Name: "code_search"}, {Name: "investigate"}, {Name: "web_fetch"}, {Name: "list_deployments"}},
		DeferredToolInfos: []*schema.ToolInfo{{Name: "web_search"}, {Name: "expand"}},
	}
	ctx := withCorpusOnly(context.Background(), true)
	_, restricted, err := middleware.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.Equal(t, []*schema.ToolInfo{{Name: "retrieve"}, {Name: "list"}, {Name: "expand"}}, restricted.ToolInfos)
	require.Equal(t, []*schema.ToolInfo{{Name: "expand"}}, restricted.DeferredToolInfos)
	require.Len(t, state.ToolInfos, 7)
	require.Len(t, state.DeferredToolInfos, 2)
	_, ordinary, err := middleware.BeforeModelRewriteState(withCorpusOnly(ctx, false), state, nil)
	require.NoError(t, err)
	require.Same(t, state, ordinary)
}

func TestCorpusOnlyToolExecutionBoundary(t *testing.T) {
	for _, name := range []string{"retrieve", "list", "expand", "code_search", "code_read", "web_search", "web_fetch", "investigate", "list_deployments", "future_tool"} {
		t.Run(name, func(t *testing.T) {
			middleware := newAgenticToolPolicyMiddleware()
			ctx := withCorpusOnly(context.Background(), true)
			calls := 0
			endpoint, err := middleware.WrapInvokableToolCall(ctx, func(context.Context, string, ...tool.Option) (string, error) {
				calls++
				return "result", nil
			}, &adk.ToolContext{Name: name})
			require.NoError(t, err)
			result, err := endpoint(ctx, `{}`)
			require.NoError(t, err)
			if corpusToolAllowed(name) {
				require.Equal(t, 1, calls)
				require.Equal(t, "result", result)
			} else {
				require.Zero(t, calls)
				require.JSONEq(t, toolAccessDenied, result)
			}
			enhanced, err := middleware.WrapEnhancedInvokableToolCall(ctx, func(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
				calls++
				return &schema.ToolResult{}, nil
			}, &adk.ToolContext{Name: name})
			require.NoError(t, err)
			enhancedResult, err := enhanced(ctx, &schema.ToolArgument{})
			require.NoError(t, err)
			if corpusToolAllowed(name) {
				require.Equal(t, 2, calls)
			} else {
				require.Zero(t, calls)
				require.Len(t, enhancedResult.Parts, 1)
				require.JSONEq(t, toolAccessDenied, enhancedResult.Parts[0].Text)
			}
			_, err = endpoint(withCorpusOnly(ctx, false), `{}`)
			require.NoError(t, err)
			require.Positive(t, calls)
		})
	}
}

func TestAskServiceCorpusOnlyRejectsForgedToolCalls(t *testing.T) {
	for _, surface := range []AskRunSurface{AskRunSurfaceSlack, AskRunSurfaceScheduled} {
		t.Run(string(surface), func(t *testing.T) {
			pool := testPool(t)
			manager := NewManager(pool, nil)
			var modelCalls, allowedCalls, deniedCalls atomic.Int32
			names := []string{"retrieve", "list", "expand", "code_search", "code_read", "web_search", "web_fetch", InvestigateToolName}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
				var advertised []string
				for _, raw := range body["tools"].([]any) {
					advertised = append(advertised, raw.(map[string]any)["name"].(string))
				}
				require.ElementsMatch(t, []string{"retrieve", "list", "expand"}, advertised)
				writer.Header().Set("Content-Type", "application/json")
				if modelCalls.Add(1) == 1 {
					var output []any
					for _, name := range names {
						output = append(output, map[string]any{"type": "function_call", "id": "fc_" + name, "call_id": "call_" + name, "name": name, "arguments": `{}`, "status": "completed"})
					}
					writeResponsesFixture(t, writer, map[string]any{
						"id": "resp_tools", "object": "response", "created_at": 1,
						"status": "completed", "model": "test", "parallel_tool_calls": true,
						"output": output, "usage": responsesUsage(10, 3, 1),
					})
				} else {
					writeRespondFixture(t, writer, "resp_final", "Zendesk answer")
				}
			}))
			defer server.Close()
			var tools []tool.BaseTool
			for _, name := range names {
				base, err := utils.InferTool(name, "test data access", func(ctx context.Context, input testToolInput) (string, error) {
					if corpusToolAllowed(name) {
						allowedCalls.Add(1)
						require.Equal(t, []string{"corpus:zendesk"}, AllowedScopesFromContext(ctx))
					} else {
						deniedCalls.Add(1)
					}
					return "tool result", nil
				})
				require.NoError(t, err)
				tools = append(tools, base)
			}
			service := newServiceForTest(t, manager, server.URL+"/v1", tools, time.Now())
			result, err := service.ExecuteTurn(context.Background(), TurnRequest{
				SessionID: newAgenticSession(t, manager), UserInput: "find the ticket", Surface: surface,
				AllowedScopes: []string{"corpus:zendesk"}, CorpusOnly: true,
			})
			require.NoError(t, err)
			require.Equal(t, "Zendesk answer", result.Reply)
			require.Equal(t, int32(3), allowedCalls.Load())
			require.Zero(t, deniedCalls.Load())
			require.Equal(t, int32(2), modelCalls.Load())
		})
	}
}
