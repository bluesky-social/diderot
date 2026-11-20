package ask

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
	"github.com/stretchr/testify/require"
)

func TestAgenticMessageV1GoldenRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *schema.AgenticMessage
		want string
	}{
		{
			name: "user text",
			msg:  NewAgenticUserText("what changed?"),
			want: `{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"what changed?"}}]}`,
		},
		{
			name: "assistant text reasoning and usage",
			msg: &schema.AgenticMessage{
				Role: schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{
					schema.NewContentBlock(&schema.Reasoning{Text: "checked sources", Signature: "sig_123"}),
					schema.NewContentBlock(&schema.AssistantGenText{Text: "The migration is complete."}),
				},
				ResponseMeta: &schema.AgenticResponseMeta{
					TokenUsage: &schema.TokenUsage{
						PromptTokens:       100,
						PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 75},
						CompletionTokens:   20,
						TotalTokens:        120,
					},
					OpenAIExtension: &openai.ResponseMetaExtension{ID: "resp_123"},
				},
			},
			want: `{"role":"assistant","content_blocks":[{"type":"reasoning","reasoning":{"text":"checked sources","signature":"sig_123"}},{"type":"assistant_gen_text","assistant_gen_text":{"text":"The migration is complete."}}],"response_meta":{"token_usage":{"prompt_tokens":100,"prompt_token_details":{"cached_tokens":75,"cache_write_tokens":0},"completion_tokens":20,"total_tokens":120,"completion_token_details":{}},"openai_extension":{"id":"resp_123"}}}`,
		},
		{
			name: "assistant parallel function calls",
			msg: &schema.AgenticMessage{
				Role: schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{
					schema.NewContentBlock(&schema.FunctionToolCall{CallID: "call_a", Name: "retrieve", Arguments: `{"question":"a"}`}),
					schema.NewContentBlock(&schema.FunctionToolCall{CallID: "call_b", Name: "retrieve", Arguments: `{"question":"b"}`}),
				},
			},
			want: `{"role":"assistant","content_blocks":[{"type":"function_tool_call","function_tool_call":{"call_id":"call_a","name":"retrieve","arguments":"{\"question\":\"a\"}"}},{"type":"function_tool_call","function_tool_call":{"call_id":"call_b","name":"retrieve","arguments":"{\"question\":\"b\"}"}}]}`,
		},
		{
			name: "assistant response item replay metadata",
			msg: &schema.AgenticMessage{
				Role: schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{{
					Type:             schema.ContentBlockTypeAssistantGenText,
					AssistantGenText: &schema.AssistantGenText{Text: "done"},
					Extra: map[string]any{
						"openai-item-id":     "msg_123",
						"openai-item-status": "completed",
					},
				}},
			},
			want: `{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"done"},"extra":{"openai-item-id":"msg_123","openai-item-status":"completed"}}]}`,
		},
		{
			name: "assistant namespaced tool-call replay metadata",
			msg: &schema.AgenticMessage{
				Role: schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{{
					Type: schema.ContentBlockTypeFunctionToolCall,
					FunctionToolCall: &schema.FunctionToolCall{
						CallID:    "call_123",
						Name:      "lookup",
						Arguments: `{"query":"hello"}`,
					},
					Extra: map[string]any{"openai-tool-call-namespace": "functions"},
				}},
			},
			want: `{"role":"assistant","content_blocks":[{"type":"function_tool_call","function_tool_call":{"call_id":"call_123","name":"lookup","arguments":"{\"query\":\"hello\"}"},"extra":{"openai-tool-call-namespace":"functions"}}]}`,
		},
		{
			name: "tool results including model visible error",
			msg: &schema.AgenticMessage{
				Role: schema.AgenticRoleTypeUser,
				ContentBlocks: []*schema.ContentBlock{
					schema.NewContentBlock(&schema.FunctionToolResult{
						CallID: "call_a", Name: "retrieve",
						Content: []*schema.FunctionToolResultContentBlock{agenticTextToolResult(`{"hits":[]}`)},
					}),
					schema.NewContentBlock(&schema.FunctionToolResult{
						CallID: "call_b", Name: "retrieve",
						Content: []*schema.FunctionToolResultContentBlock{agenticTextToolResult(`tool error: deadline exceeded`)},
					}),
				},
			},
			want: `{"role":"user","content_blocks":[{"type":"function_tool_result","function_tool_result":{"call_id":"call_a","name":"retrieve","content":[{"type":"text","text":{"text":"{\"hits\":[]}"}}]}},{"type":"function_tool_result","function_tool_result":{"call_id":"call_b","name":"retrieve","content":[{"type":"text","text":{"text":"tool error: deadline exceeded"}}]}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := EncodeAgenticMessage(tt.msg)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(raw))
			decoded, err := DecodeAgenticMessage(raw)
			require.NoError(t, err)
			rawAgain, err := EncodeAgenticMessage(decoded)
			require.NoError(t, err)
			require.Equal(t, string(raw), string(rawAgain))
		})
	}
}

func TestValidateAgenticMessageRejectsUnsupportedOrMalformedShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *schema.AgenticMessage
	}{
		{name: "nil"},
		{name: "system role", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeSystem, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{Text: "x"})}}},
		{name: "empty blocks", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser}},
		{name: "message extra", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: NewAgenticUserText("x").ContentBlocks, Extra: map[string]any{"unbounded": true}}},
		{name: "wrong role", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "x"})}}},
		{name: "two variants", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeAssistantGenText, AssistantGenText: &schema.AssistantGenText{Text: "x"}, Reasoning: &schema.Reasoning{Text: "y"}}}}},
		{name: "arbitrary block extra", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeAssistantGenText, AssistantGenText: &schema.AssistantGenText{Text: "x"}, Extra: map[string]any{"provider_payload": true}}}}},
		{name: "malformed response item extra", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeAssistantGenText, AssistantGenText: &schema.AssistantGenText{Text: "x"}, Extra: map[string]any{"openai-item-id": 123}}}}},
		{name: "malformed response namespace extra", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolCall, FunctionToolCall: &schema.FunctionToolCall{CallID: "call_123", Name: "lookup", Arguments: `{}`}, Extra: map[string]any{"openai-tool-call-namespace": ""}}}}},
		{name: "unsupported image", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputImage{URL: "https://example.test/image.png"})}}},
		{name: "missing call id", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{Name: "retrieve", Arguments: `{}`})}}},
		{name: "non object arguments", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "c", Name: "retrieve", Arguments: `[]`})}}},
		{name: "empty result", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "c", Name: "retrieve"})}}},
		{name: "multimodal result", msg: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "c", Name: "retrieve", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeImage, Image: &schema.UserInputImage{URL: "https://example.test/image.png"}}}})}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, ValidateAgenticMessage(tt.msg))
		})
	}
}

func TestDecodeAgenticMessageRejectsUnknownJSON(t *testing.T) {
	t.Parallel()
	_, err := DecodeAgenticMessage([]byte(`{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"x"},"future_field":true}]}`))
	require.ErrorContains(t, err, "unknown field")

	_, err = DecodeAgenticMessage([]byte(`{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"x"}}]} {}`))
	require.Error(t, err)
}

func TestAgenticMessageAccessors(t *testing.T) {
	t.Parallel()
	msg := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.AssistantGenText{Text: "first"}),
			schema.NewContentBlock(&schema.FunctionToolCall{CallID: "c1", Name: "retrieve", Arguments: `{}`}),
			schema.NewContentBlock(&schema.AssistantGenText{Text: "second"}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 12}},
	}
	require.Equal(t, "first\n\nsecond", AgenticMessageText(msg))
	require.Equal(t, []schema.FunctionToolCall{{CallID: "c1", Name: "retrieve", Arguments: `{}`}}, AgenticMessageToolCalls(msg))
	require.Empty(t, AgenticMessageToolResults(msg))
	usage := AgenticMessageUsage(msg)
	require.Equal(t, 12, usage.PromptTokens)
	usage.PromptTokens = 99
	require.Equal(t, 12, msg.ResponseMeta.TokenUsage.PromptTokens, "usage accessor must return a copy")
}

func agenticTextToolResult(text string) *schema.FunctionToolResultContentBlock {
	return &schema.FunctionToolResultContentBlock{
		Type: schema.FunctionToolResultContentBlockTypeText,
		Text: &schema.UserInputText{Text: text},
	}
}

func TestAgenticMessageSchemaVersionIsOne(t *testing.T) {
	// Makes accidental version changes visible independently of a JSON diff.
	require.Equal(t, int16(1), AgenticMessageSchemaVersion)
}
