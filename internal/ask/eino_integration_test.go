package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestEinoResponsesReActContract is the offline contract test. It uses
// Eino's real Responses client and typed ADK runner against an OpenAI-compatible
// fake, so it catches wire and hook changes without requiring gateway credentials.
func TestEinoResponsesReActContract(t *testing.T) {
	t.Parallel()

	type recordedRequest struct {
		Header http.Header
		Body   map[string]any
	}
	var (
		requestMu sync.Mutex
		requests  []recordedRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, "Bearer test-gateway-token", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requestMu.Lock()
		requests = append(requests, recordedRequest{Header: r.Header.Clone(), Body: body})
		call := len(requests)
		requestMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tools", "object": "response", "created_at": 1,
				"status": "completed", "model": "claude-opus-5:main",
				"parallel_tool_calls": true,
				"output": []any{
					map[string]any{"type": "function_call", "id": "fc_alpha", "call_id": "call_alpha", "name": "lookup_alpha", "arguments": `{"value":"alpha"}`, "status": "completed"},
					map[string]any{"type": "function_call", "id": "fc_beta", "call_id": "call_beta", "name": "lookup_beta", "arguments": `{"value":"beta"}`, "status": "completed"},
				},
				"usage": responsesUsage(11, 7, 3),
			})
		case 2:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_final", "object": "response", "created_at": 2,
				"status": "completed", "model": "claude-opus-5:main",
				"parallel_tool_calls": true,
				"output": []any{map[string]any{
					"type": "message", "id": "msg_final", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "both lookups completed", "annotations": []any{}}},
				}},
				"usage": responsesUsage(23, 5, 9),
			})
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL:              server.URL + "/v1",
		APIKey:               "test-gateway-token",
		ModelID:              "claude-opus-5:main",
		Timeout:              5 * time.Second,
		MaxRetries:           1,
		PromptCacheRetention: "in_memory",
	})
	require.NoError(t, err)

	type lookupInput struct {
		Value string `json:"value" jsonschema:"description=value to look up"`
	}
	type lookupOutput struct {
		Value string `json:"value"`
	}

	allStarted := make(chan struct{})
	var (
		started   atomic.Int32
		completed atomic.Int32
		startOnce sync.Once
	)
	newLookup := func(name string) tool.InvokableTool {
		result, toolErr := utils.InferTool(name, "Test a parallel lookup", func(ctx context.Context, in lookupInput) (lookupOutput, error) {
			if started.Add(1) == 2 {
				startOnce.Do(func() { close(allStarted) })
			}
			select {
			case <-allStarted:
			case <-ctx.Done():
				return lookupOutput{}, ctx.Err()
			}
			completed.Add(1)
			return lookupOutput{Value: in.Value + "-done"}, nil
		})
		require.NoError(t, toolErr)
		return result
	}

	agent, err := adk.NewTypedChatModelAgent(context.Background(), &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:          "diderot-stage0-contract",
		Description:   "Exercises the typed AgenticMessage ReAct path",
		Instruction:   "Call both lookup tools, then answer.",
		Model:         mdl,
		MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools:               []tool.BaseTool{newLookup("lookup_alpha"), newLookup("lookup_beta")},
			ExecuteSequentially: false,
		}},
	})
	require.NoError(t, err)

	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hookCalls atomic.Int32
	sessionID := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	metadataOption, err := withAskRunMetadata(AskRunMetadata{SessionID: sessionID})
	require.NoError(t, err)
	iter := runner.Query(ctx, "run the contract test",
		metadataOption,
		adk.WithAfterToolCallsHook(func(context.Context) error {
			if got := completed.Load(); got != 2 {
				return fmt.Errorf("hook fired before the complete parallel batch: completed=%d", got)
			}
			hookCalls.Add(1)
			return nil
		}),
	)

	var final *schema.AgenticMessage
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, event.Err)
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, getErr := event.Output.MessageOutput.GetMessage()
		require.NoError(t, getErr)
		if msg != nil {
			final = msg
		}
	}

	require.Equal(t, int32(2), started.Load(), "both tools must be scheduled")
	require.Equal(t, int32(2), completed.Load(), "both tools must complete")
	require.Equal(t, int32(1), hookCalls.Load(), "one hook per completed tool batch")
	require.NotNil(t, final)
	require.Equal(t, "both lookups completed", stage0AgenticMessageText(final))
	require.NotNil(t, final.ResponseMeta)
	require.NotNil(t, final.ResponseMeta.TokenUsage)
	require.Equal(t, 23, final.ResponseMeta.TokenUsage.PromptTokens)
	require.Equal(t, 9, final.ResponseMeta.TokenUsage.PromptTokenDetails.CachedTokens)
	require.Equal(t, 5, final.ResponseMeta.TokenUsage.CompletionTokens)
	require.Equal(t, "resp_final", final.ResponseMeta.OpenAIExtension.ID)
	frameworkMessageID, ok := final.Extra[einoMessageIDExtraKey].(string)
	require.True(t, ok)
	require.NotEmpty(t, frameworkMessageID, "pinned Eino must attach its run-local message ID")
	persistedFinal, err := encodeAgenticMessageRow(final)
	require.NoError(t, err, "the persistence boundary must accept a real Eino model output")
	require.NotContains(t, string(persistedFinal.JSON), einoMessageIDExtraKey)

	requestMu.Lock()
	recorded := append([]recordedRequest(nil), requests...)
	requestMu.Unlock()
	require.Len(t, recorded, 2)
	require.NotContains(t, recorded[0].Body, "metadata")
	require.Equal(t, sessionID.String(), recorded[0].Header.Get("x-agent-parent-session-id"))
	require.Empty(t, recorded[0].Header.Get("x-bedrock-metadata"))
	require.Equal(t, "claude-opus-5:main", recorded[0].Body["model"])
	require.Equal(t, "in_memory", recorded[0].Body["prompt_cache_retention"])

	var resultCallIDs []string
	for _, item := range recorded[1].Body["input"].([]any) {
		entry, ok := item.(map[string]any)
		if !ok || entry["type"] != "function_call_output" {
			continue
		}
		resultCallIDs = append(resultCallIDs, entry["call_id"].(string))
	}
	sort.Strings(resultCallIDs)
	require.Equal(t, []string{"call_alpha", "call_beta"}, resultCallIDs)
}

func TestEinoResponsesToolErrorIsModelVisible(t *testing.T) {
	t.Parallel()

	var secondRequest map[string]any
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_error_tool", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_broken", "call_id": "call_broken",
					"name": "broken_lookup", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
			return
		}
		secondRequest = body
		writeResponsesFixture(t, w, map[string]any{
			"id": "resp_recovered", "object": "response", "created_at": 2,
			"status": "completed", "model": "test",
			"output": []any{
				map[string]any{
					"type": "message", "id": "msg_recovered", "status": "completed", "role": "assistant",
					"content": []any{
						map[string]any{"type": "output_text", "text": "recovered", "annotations": []any{}},
					},
				},
			},
			"usage": responsesUsage(2, 1, 0),
		})
	}))
	defer server.Close()

	mdl := newStage0ResponsesModel(t, server.URL)
	type emptyInput struct{}
	type emptyOutput struct{}
	broken, err := utils.InferTool("broken_lookup", "Always fails safely", func(context.Context, emptyInput) (emptyOutput, error) {
		return emptyOutput{}, errors.New("database unavailable")
	})
	require.NoError(t, err)
	wrapped := utils.WrapToolWithErrorHandler(broken, func(context.Context, error) string {
		return `{"error":"safe tool error"}`
	})
	agent, err := adk.NewTypedChatModelAgent(context.Background(), &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "tool-error-contract", Description: "test", Model: mdl, MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{wrapped}}},
	})
	require.NoError(t, err)

	var hookCalls atomic.Int32
	iter := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent}).Query(
		context.Background(), "fail safely", adk.WithAfterToolCallsHook(func(context.Context) error {
			hookCalls.Add(1)
			return nil
		}),
	)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, event.Err)
	}
	require.Equal(t, int32(1), hookCalls.Load())
	require.Equal(t, int32(2), calls.Load())
	encoded, err := json.Marshal(secondRequest)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"call_id":"call_broken"`)
	require.Contains(t, string(encoded), "safe tool error")
}

func TestEinoResponsesMaxIterations(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeResponsesFixture(t, w, map[string]any{
			"id": fmt.Sprintf("resp_%d", call), "object": "response", "created_at": call,
			"status": "completed", "model": "test",
			"output": []any{map[string]any{
				"type": "function_call", "id": fmt.Sprintf("fc_%d", call), "call_id": fmt.Sprintf("call_%d", call),
				"name": "loop_forever", "arguments": `{}`, "status": "completed",
			}},
			"usage": responsesUsage(1, 1, 0),
		})
	}))
	defer server.Close()

	mdl := newStage0ResponsesModel(t, server.URL)
	type emptyInput struct{}
	type emptyOutput struct {
		OK bool `json:"ok"`
	}
	loopTool, err := utils.InferTool("loop_forever", "Keeps the model in the loop", func(context.Context, emptyInput) (emptyOutput, error) {
		return emptyOutput{OK: true}, nil
	})
	require.NoError(t, err)
	agent, err := adk.NewTypedChatModelAgent(context.Background(), &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "max-iterations-contract", Description: "test", Model: mdl, MaxIterations: 2,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{loopTool}}},
	})
	require.NoError(t, err)

	iter := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent}).Query(context.Background(), "loop")
	var runErr error
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			runErr = event.Err
		}
	}
	require.ErrorIs(t, runErr, adk.ErrExceedMaxIterations)
	require.Equal(t, int32(2), calls.Load())
}

func writeResponsesFixture(t *testing.T, w http.ResponseWriter, body map[string]any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func responsesUsage(input, output, cached int) map[string]any {
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          input + output,
	}
}

func stage0AgenticMessageText(msg *schema.AgenticMessage) string {
	var text string
	for _, block := range msg.ContentBlocks {
		if block != nil && block.AssistantGenText != nil {
			if text != "" {
				text += "\n\n"
			}
			text += block.AssistantGenText.Text
		}
	}
	return text
}

func newStage0ResponsesModel(t *testing.T, serverURL string) *agenticopenai.ResponsesModel {
	t.Helper()
	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: serverURL + "/v1", APIKey: "test", ModelID: "test",
		Timeout: 5 * time.Second, MaxRetries: 1,
	})
	require.NoError(t, err)
	return mdl
}
