package ask

import (
	"math"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
	"github.com/stretchr/testify/require"
)

func TestEncodeAgenticMessageRowUserText(t *testing.T) {
	tests := []struct {
		name   string
		msg    *schema.AgenticMessage
		golden string
	}{
		{
			name:   "plain",
			msg:    NewAgenticUserText("what changed?"),
			golden: `{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"what changed?"}}]}`,
		},
		{
			name:   "multiline",
			msg:    NewAgenticUserText("line one\nline two"),
			golden: `{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"line one\nline two"}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := encodeAgenticMessageRow(tt.msg)
			require.NoError(t, err)
			require.Equal(t, "user", row.Role)
			require.Equal(t, AgenticMessageSchemaVersion, row.AgenticMessageSchemaVersion)
			require.JSONEq(t, tt.golden, string(row.JSON))
			require.Nil(t, row.InputTokens)
			require.Nil(t, row.OutputTokens)
			require.Nil(t, row.CacheReadTokens)
			require.Nil(t, row.CacheWriteTokens)
		})
	}
}

func TestEncodeAgenticMessageRowAssistantUsage(t *testing.T) {
	tests := []struct {
		name   string
		msg    *schema.AgenticMessage
		input  int32
		cache  int32
		output int32
	}{
		{
			name:   "populated usage",
			msg:    agenticAssistantUsage(1234, 900, 88, 1322),
			input:  1234,
			cache:  900,
			output: 88,
		},
		{
			name:   "zero usage",
			msg:    agenticAssistantUsage(0, 0, 0, 0),
			input:  0,
			cache:  0,
			output: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := encodeAgenticMessageRow(tt.msg)
			require.NoError(t, err)
			require.Equal(t, "assistant", row.Role)
			require.Equal(t, AgenticMessageSchemaVersion, row.AgenticMessageSchemaVersion)
			require.NotNil(t, row.InputTokens)
			require.Equal(t, tt.input, *row.InputTokens)
			require.NotNil(t, row.OutputTokens)
			require.Equal(t, tt.output, *row.OutputTokens)
			require.NotNil(t, row.CacheReadTokens)
			require.Equal(t, tt.cache, *row.CacheReadTokens)
			require.Nil(t, row.CacheWriteTokens)
		})
	}
}

// TestEncodeAgenticMessageRowStripsEmbeddedNulBytes is a regression test:
// an embedded NUL byte (U+0000) in a content block's text used to pass
// ValidateAgenticMessage untouched, even though Go's json.Marshal encodes a
// NUL byte as the literal 6-character escape sequence "\u0000", which
// Postgres's jsonb input function rejects outright (SQLSTATE 22P05,
// "unsupported Unicode escape sequence"). ValidateAgenticMessage's own
// contract is to "accept only the subset of AgenticMessage that Ask can
// replay losslessly" -- acceptance must imply the message is durably
// persistable, so encodeAgenticMessageRow must strip NUL bytes (matching
// internal/sessioningest/store.go's boundedIndexError, which strips the
// same byte before writing text to Postgres) before any row is built.
//
// This covers all four text-bearing block types the write path accepts, not
// just assistant text, since the root cause is the same for every one of
// them: no validator or projection on the write path looks at text content.
func TestEncodeAgenticMessageRowStripsEmbeddedNulBytes(t *testing.T) {
	tests := []struct {
		name   string
		msg    *schema.AgenticMessage
		golden string
	}{
		{
			name:   "user input text",
			msg:    NewAgenticUserText("value: \x00 end"),
			golden: `{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"value:  end"}}]}`,
		},
		{
			name:   "assistant generated text",
			msg:    NewAgenticAssistantText("value: \x00 end"),
			golden: `{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"value:  end"}}]}`,
		},
		{
			name: "reasoning text",
			msg: agenticTestMsg(schema.AgenticRoleTypeAssistant,
				schema.NewContentBlock(&schema.Reasoning{Text: "value: \x00 end"}),
				agenticAssistantTextBlock("done"),
			),
			golden: `{"role":"assistant","content_blocks":[
				{"type":"reasoning","reasoning":{"text":"value:  end"}},
				{"type":"assistant_gen_text","assistant_gen_text":{"text":"done"}}
			]}`,
		},
		{
			name: "function tool result text",
			msg: agenticTestMsg(schema.AgenticRoleTypeUser,
				agenticResultBlock("call-1", "retrieve", "value: \x00 end"),
			),
			golden: `{"role":"user","content_blocks":[{"type":"function_tool_result","function_tool_result":{
				"call_id":"call-1","name":"retrieve","content":[{"type":"text","text":{"text":"value:  end"}}]
			}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := encodeAgenticMessageRow(tt.msg)
			require.NoError(t, err)
			require.NotContains(t, string(row.JSON), `\u0000`,
				"a literal NUL escape must never reach the row written to jsonb")
			require.JSONEq(t, tt.golden, string(row.JSON))
		})
	}
}

// TestEncodeAgenticMessageRowStripsEmbeddedNulBytesInOpenAIExtensions is a
// regression test for the sweep of this same bug class past the four text
// fields agenticMessageForPersistence knows about explicitly.
// ValidateAgenticMessage rejects a Gemini or Claude response/text
// extension, but an OpenAI one is unrecognized and simply passes through
// untouched, so a NUL in one of its own text fields is not stripped by the
// per-field projection and used to still fail the INSERT with SQLSTATE
// 22P05. EncodeAgenticMessage's generic stripNULFromEncodedAgenticMessage
// backstop is what has to catch these.
func TestEncodeAgenticMessageRowStripsEmbeddedNulBytesInOpenAIExtensions(t *testing.T) {
	tests := []struct {
		name string
		msg  *schema.AgenticMessage
	}{
		{
			name: "reasoning OpenAI extension content text",
			msg: agenticTestMsg(schema.AgenticRoleTypeAssistant,
				schema.NewContentBlock(&schema.Reasoning{
					Text: "summary",
					OpenAIExtension: &openai.ReasoningExtension{
						Content: []*openai.ReasoningContent{{Text: "value: \x00 end"}},
					},
				}),
				agenticAssistantTextBlock("done"),
			),
		},
		{
			name: "assistant text OpenAI refusal reason",
			msg: func() *schema.AgenticMessage {
				m := NewAgenticAssistantText("done")
				m.ContentBlocks[0].AssistantGenText.OpenAIExtension = &openai.AssistantGenTextExtension{
					Refusal: &openai.OutputRefusal{Reason: "value: \x00 end"},
				}
				return m
			}(),
		},
		{
			name: "response meta OpenAI error message",
			msg: func() *schema.AgenticMessage {
				m := NewAgenticAssistantText("done")
				m.ResponseMeta = &schema.AgenticResponseMeta{
					OpenAIExtension: &openai.ResponseMetaExtension{
						Error: &openai.ResponseError{Message: "value: \x00 end"},
					},
				}
				return m
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := encodeAgenticMessageRow(tt.msg)
			require.NoError(t, err)
			require.NotContains(t, string(row.JSON), `\u0000`,
				"a literal NUL escape in any OpenAI-extension field must never reach the row written to jsonb")
		})
	}
}

func TestEncodeAgenticMessageRowStripsOnlyEinoRunMessageID(t *testing.T) {
	msg := NewAgenticAssistantText("done")
	msg.Extra = map[string]any{einoMessageIDExtraKey: "run-message-123"}

	row, err := encodeAgenticMessageRow(msg)
	require.NoError(t, err)
	require.NotContains(t, string(row.JSON), einoMessageIDExtraKey)
	require.Equal(t, map[string]any{einoMessageIDExtraKey: "run-message-123"}, msg.Extra,
		"persistence must not mutate the live Eino message")

	decoded, err := decodeAgenticMessageRow(row)
	require.NoError(t, err)
	require.Nil(t, decoded.Extra)
}

func TestEncodeAgenticMessageRowStillRejectsArbitraryExtra(t *testing.T) {
	for _, extra := range []map[string]any{
		{einoMessageIDExtraKey: "run-message-123", "provider_payload": true},
		{einoMessageIDExtraKey: ""},
		{einoMessageIDExtraKey: 123},
	} {
		msg := NewAgenticAssistantText("done")
		msg.Extra = extra
		_, err := encodeAgenticMessageRow(msg)
		require.ErrorContains(t, err, "message extra is not persisted")
	}
}

func TestEncodeAgenticMessageRowRejectsInvalidTokenCounts(t *testing.T) {
	tests := []struct {
		name string
		msg  *schema.AgenticMessage
		want string
	}{
		{name: "negative prompt", msg: agenticAssistantUsage(-1, 0, 0, 0), want: "negative input tokens"},
		{name: "negative completion", msg: agenticAssistantUsage(0, 0, -1, 0), want: "negative output tokens"},
		{name: "negative cache read", msg: agenticAssistantUsage(0, -1, 0, 0), want: "negative cache read tokens"},
		{name: "negative total", msg: agenticAssistantUsage(0, 0, 0, -1), want: "negative total tokens"},
		{name: "prompt overflows int32", msg: agenticAssistantUsage(math.MaxInt32+1, 0, 0, math.MaxInt32+1), want: "input tokens overflow int32"},
		{name: "completion overflows int32", msg: agenticAssistantUsage(0, 0, math.MaxInt32+1, math.MaxInt32+1), want: "output tokens overflow int32"},
		{name: "cache read overflows int32", msg: agenticAssistantUsage(0, math.MaxInt32+1, 0, math.MaxInt32+1), want: "cache read tokens overflow int32"},
		{name: "total overflows int32", msg: agenticAssistantUsage(0, 0, 0, math.MaxInt32+1), want: "total tokens overflow int32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeAgenticMessageRow(tt.msg)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestDecodeAgenticMessageRowRejectsUnknownVersion(t *testing.T) {
	raw, err := EncodeAgenticMessage(NewAgenticUserText("hello"))
	require.NoError(t, err)
	for _, version := range []int16{0, 2, 99} {
		_, err := decodeAgenticMessageRow(messageRow{Role: "user", AgenticMessageSchemaVersion: version, JSON: raw})
		require.ErrorContains(t, err, "unsupported schema version")
	}
}

func TestDecodeAgenticMessageRowRejectsRoleMismatch(t *testing.T) {
	raw, err := EncodeAgenticMessage(NewAgenticAssistantText("hi"))
	require.NoError(t, err)
	_, err = decodeAgenticMessageRow(messageRow{Role: "user", AgenticMessageSchemaVersion: AgenticMessageSchemaVersion, JSON: raw})
	require.ErrorContains(t, err, "role mismatch")
}

func TestDecodeAgenticMessageRowRejectsTokenDrift(t *testing.T) {
	row, err := encodeAgenticMessageRow(agenticAssistantUsage(1234, 900, 88, 1322))
	require.NoError(t, err)

	for _, tt := range []struct {
		name   string
		mutate func(*messageRow)
		want   string
	}{
		{name: "input value", mutate: func(r *messageRow) { v := int32(1); r.InputTokens = &v }, want: "input_tokens"},
		{name: "output null", mutate: func(r *messageRow) { r.OutputTokens = nil }, want: "output_tokens"},
		{name: "cache read value", mutate: func(r *messageRow) { v := int32(2); r.CacheReadTokens = &v }, want: "cache_read_tokens"},
		{name: "cache write unexpectedly present", mutate: func(r *messageRow) { v := int32(3); r.CacheWriteTokens = &v }, want: "cache_write_tokens"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mutated := row
			tt.mutate(&mutated)
			_, err := decodeAgenticMessageRow(mutated)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestDecodeAgenticMessageRowRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  *schema.AgenticMessage
	}{
		{name: "user text", msg: NewAgenticUserText("what changed?")},
		{name: "assistant with usage", msg: agenticAssistantUsage(1234, 900, 88, 1322)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := encodeAgenticMessageRow(tt.msg)
			require.NoError(t, err)
			got, err := decodeAgenticMessageRow(row)
			require.NoError(t, err)
			require.Equal(t, tt.msg, got)
		})
	}
}

func agenticAssistantUsage(prompt, cached, completion, total int) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.AssistantGenText{Text: "done"}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{
			TokenUsage: &schema.TokenUsage{
				PromptTokens:       prompt,
				PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: cached},
				CompletionTokens:   completion,
				TotalTokens:        total,
			},
		},
	}
}
