package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	claudeschema "github.com/cloudwego/eino/schema/claude"
	"github.com/stretchr/testify/require"
)

func TestClaudePersistenceProjectionStripsOnlyTransientExtensions(t *testing.T) {
	t.Parallel()

	msg := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.Reasoning{Text: "check", Signature: "sig_123"}),
			schema.NewContentBlock(&schema.AssistantGenText{
				Text: "answer",
				ClaudeExtension: &claudeschema.AssistantGenTextExtension{
					Citations: []*claudeschema.TextCitation{},
				},
			}),
			schema.NewContentBlock(&schema.FunctionToolCall{
				CallID: "toolu_123", Name: "lookup", Arguments: `{"q":"x"}`,
			}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{
			TokenUsage:      &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
			ClaudeExtension: &claudeschema.ResponseMetaExtension{ID: "msg_123", StopReason: "tool_use"},
		},
		Extra: map[string]any{einoMessageIDExtraKey: "run-local-id"},
	}

	persisted := agenticMessageForPersistence(msg)
	require.NotSame(t, msg, persisted)
	require.Nil(t, persisted.Extra)
	require.Nil(t, persisted.ResponseMeta.ClaudeExtension)
	require.Nil(t, persisted.ContentBlocks[1].AssistantGenText.ClaudeExtension)
	require.Equal(t, "sig_123", persisted.ContentBlocks[0].Reasoning.Signature)
	require.Equal(t, "toolu_123", persisted.ContentBlocks[2].FunctionToolCall.CallID)
	require.NoError(t, ValidateAgenticMessage(persisted))

	// The live Eino message is still intact for completion handling and other
	// in-process consumers.
	require.NotNil(t, msg.ResponseMeta.ClaudeExtension)
	require.NotNil(t, msg.ContentBlocks[1].AssistantGenText.ClaudeExtension)
	require.Equal(t, "run-local-id", msg.Extra[einoMessageIDExtraKey])
}

func TestNativeClaudeToolRoundTripPersistsAndReplaysSignedThinking(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeModelRequest(t, r)
		require.Equal(t, "/v1/messages", request.path)
		w.Header().Set("Content-Type", "application/json")

		switch calls.Add(1) {
		case 1:
			require.Equal(t, "high", request.body["output_config"].(map[string]any)["effort"])
			writeClaudeMessage(t, w, map[string]any{
				"id": "msg_tool", "stop_reason": "tool_use",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "need lookup", "signature": "sig_tool"},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "x"}},
				},
			})
		case 2:
			messages := request.body["messages"].([]any)
			require.Len(t, messages, 3)

			assistant := messages[1].(map[string]any)
			require.Equal(t, "assistant", assistant["role"])
			assistantContent := assistant["content"].([]any)
			require.Len(t, assistantContent, 2)
			thinking := assistantContent[0].(map[string]any)
			require.Equal(t, "thinking", thinking["type"])
			require.Equal(t, "need lookup", thinking["thinking"])
			require.Equal(t, "sig_tool", thinking["signature"])
			toolUse := assistantContent[1].(map[string]any)
			require.Equal(t, "tool_use", toolUse["type"])
			require.Equal(t, "toolu_1", toolUse["id"])

			result := messages[2].(map[string]any)
			require.Equal(t, "user", result["role"])
			resultBlock := result["content"].([]any)[0].(map[string]any)
			require.Equal(t, "tool_result", resultBlock["type"])
			require.Equal(t, "toolu_1", resultBlock["tool_use_id"])

			writeClaudeMessage(t, w, map[string]any{
				"id": "msg_final", "stop_reason": "end_turn",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "done", "signature": "sig_final"},
					map[string]any{"type": "text", "text": "answer"},
				},
			})
		default:
			t.Fatalf("unexpected native Claude request %d", calls.Load())
		}
	}))
	defer server.Close()

	mdl, err := NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL + "/v1", APIKey: "gateway-token",
		Model: registryModel(t, "claude-opus-5:main"), MaxTokens: 1024, ReasoningEffort: "high",
	})
	require.NoError(t, err)

	user := NewAgenticUserText("look this up")
	toolResponse, err := mdl.Generate(context.Background(), []*schema.AgenticMessage{user})
	require.NoError(t, err)
	require.NotNil(t, toolResponse.ResponseMeta.ClaudeExtension)
	require.Equal(t, "sig_tool", toolResponse.ContentBlocks[0].Reasoning.Signature)
	require.Equal(t, "toolu_1", AgenticMessageToolCalls(toolResponse)[0].CallID)

	toolRow, err := encodeAgenticMessageRow(toolResponse)
	require.NoError(t, err)
	require.NotContains(t, string(toolRow.JSON), "claude_extension")
	replayedToolResponse, err := decodeAgenticMessageRow(toolRow)
	require.NoError(t, err)
	require.Equal(t, "sig_tool", replayedToolResponse.ContentBlocks[0].Reasoning.Signature)

	toolResult := newAgenticToolResult("toolu_1", "lookup", `{"value":42}`)
	final, err := mdl.Generate(context.Background(), []*schema.AgenticMessage{user, replayedToolResponse, toolResult})
	require.NoError(t, err)
	require.NotNil(t, final.ResponseMeta.ClaudeExtension)

	finalRow, err := encodeAgenticMessageRow(final)
	require.NoError(t, err)
	require.NotContains(t, string(finalRow.JSON), "claude_extension")
	replayedFinal, err := decodeAgenticMessageRow(finalRow)
	require.NoError(t, err)
	require.Equal(t, "sig_final", replayedFinal.ContentBlocks[0].Reasoning.Signature)
	require.Equal(t, "answer", AgenticMessageText(replayedFinal))
	require.Equal(t, int32(2), calls.Load())
}

func writeClaudeMessage(t *testing.T, w http.ResponseWriter, fields map[string]any) {
	t.Helper()
	response := map[string]any{
		"type": "message", "role": "assistant", "model": "claude-opus-5:main",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 10, "output_tokens": 4},
	}
	for key, value := range fields {
		response[key] = value
	}
	require.NoError(t, json.NewEncoder(w).Encode(response))
}
