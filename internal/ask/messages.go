package ask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// AgenticMessageSchemaVersion is the canonical on-disk schema version of
// Ask's persisted Eino AgenticMessage rows (the ask_messages.message JSONB
// column, alongside the denormalized role and nullable token columns). It is
// the stable contract for every Ask reader and writer. Changing the accepted
// JSON shape requires a database migration and a new version; upgrading Eino
// alone must not rewrite persisted history.
const AgenticMessageSchemaVersion int16 = 1

// DecodeAgenticMessage decodes and validates the bounded Eino message shape
// persisted by Ask. DisallowUnknownFields is intentional: otherwise a newer
// Eino block could be silently discarded and replayed incorrectly.
func DecodeAgenticMessage(raw []byte) (*schema.AgenticMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("ask message: empty JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var msg schema.AgenticMessage
	if err := dec.Decode(&msg); err != nil {
		return nil, fmt.Errorf("ask message: decode: %w", err)
	}
	if err := consumeJSONEOF(dec); err != nil {
		return nil, err
	}
	if err := ValidateAgenticMessage(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func consumeJSONEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("ask message: multiple JSON values")
		}
		return fmt.Errorf("ask message: trailing JSON: %w", err)
	}
	return nil
}

// EncodeAgenticMessage produces the canonical v1 payload written to JSONB.
func EncodeAgenticMessage(msg *schema.AgenticMessage) (json.RawMessage, error) {
	if err := ValidateAgenticMessage(msg); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("ask message: encode: %w", err)
	}
	raw, err = stripNULFromEncodedAgenticMessage(raw)
	if err != nil {
		return nil, fmt.Errorf("ask message: strip embedded NUL bytes: %w", err)
	}
	return raw, nil
}

// stripNULFromEncodedAgenticMessage is a backstop at the one place every
// persisted message is serialized to JSONB. agenticMessageForPersistence
// already strips an embedded NUL byte (U+0000) from the text fields it
// knows about (user/assistant text, reasoning text, function-tool-result
// text) before this function is ever reached; this catches any other
// text-bearing field validateAgenticContentBlock accepts without stripping.
// Confirmed live: Reasoning.OpenAIExtension.Content[].Text, a field
// agenticopenai populates from provider reasoning content, passes
// ValidateAgenticMessage untouched and still fails the INSERT with
// SQLSTATE 22P05 if it carries a NUL. The same backstop also covers
// AssistantGenText.OpenAIExtension's Refusal and Annotations,
// ResponseMeta.OpenAIExtension.Error.Message, and any block type this
// function is not updated for. It only pays the decode/re-encode cost (see
// stripNULFromJSON) when a literal "\u0000" escape is actually present.
func stripNULFromEncodedAgenticMessage(raw json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw, nil
	}
	return stripNULFromJSON(raw)
}

// ValidateAgenticMessage accepts only the subset of AgenticMessage that Ask
// can replay losslessly. Provider-specific arbitrary Extra/Extension values
// are deliberately excluded from the durable transcript.
func ValidateAgenticMessage(msg *schema.AgenticMessage) error {
	if msg == nil {
		return errors.New("ask message: nil")
	}
	if msg.Role != schema.AgenticRoleTypeUser && msg.Role != schema.AgenticRoleTypeAssistant {
		return fmt.Errorf("ask message: unsupported role %q", msg.Role)
	}
	if len(msg.ContentBlocks) == 0 {
		return errors.New("ask message: no content blocks")
	}
	if len(msg.Extra) != 0 {
		return errors.New("ask message: message extra is not persisted")
	}
	if err := validateAgenticResponseMeta(msg); err != nil {
		return err
	}
	for i, block := range msg.ContentBlocks {
		if err := validateAgenticContentBlock(msg.Role, block); err != nil {
			return fmt.Errorf("ask message: block %d: %w", i, err)
		}
	}
	return nil
}

func validateAgenticResponseMeta(msg *schema.AgenticMessage) error {
	meta := msg.ResponseMeta
	if meta == nil {
		return nil
	}
	if msg.Role != schema.AgenticRoleTypeAssistant {
		return errors.New("response metadata is only valid on assistant messages")
	}
	if meta.GeminiExtension != nil || meta.ClaudeExtension != nil || meta.Extension != nil {
		return errors.New("unsupported response metadata extension")
	}
	return nil
}

func validateAgenticContentBlock(role schema.AgenticRoleType, block *schema.ContentBlock) error {
	if block == nil {
		return errors.New("nil content block")
	}
	if block.StreamingMeta != nil {
		return errors.New("streaming metadata is not persisted")
	}
	if err := validateAgenticContentBlockExtra(role, block.Extra); err != nil {
		return err
	}
	if n := populatedAgenticVariants(block); n != 1 {
		return fmt.Errorf("content block has %d populated variants, want 1", n)
	}

	switch block.Type {
	case schema.ContentBlockTypeUserInputText:
		if role != schema.AgenticRoleTypeUser || block.UserInputText == nil {
			return errors.New("user_input_text requires user role and payload")
		}
	case schema.ContentBlockTypeAssistantGenText:
		if role != schema.AgenticRoleTypeAssistant || block.AssistantGenText == nil {
			return errors.New("assistant_gen_text requires assistant role and payload")
		}
		if block.AssistantGenText.ClaudeExtension != nil || block.AssistantGenText.Extension != nil {
			return errors.New("unsupported assistant text extension")
		}
	case schema.ContentBlockTypeReasoning:
		if role != schema.AgenticRoleTypeAssistant || block.Reasoning == nil {
			return errors.New("reasoning requires assistant role and payload")
		}
	case schema.ContentBlockTypeFunctionToolCall:
		if role != schema.AgenticRoleTypeAssistant || block.FunctionToolCall == nil {
			return errors.New("function_tool_call requires assistant role and payload")
		}
		if err := validateFunctionToolCall(block.FunctionToolCall); err != nil {
			return err
		}
	case schema.ContentBlockTypeFunctionToolResult:
		if role != schema.AgenticRoleTypeUser || block.FunctionToolResult == nil {
			return errors.New("function_tool_result requires user role and payload")
		}
		if err := validateFunctionToolResult(block.FunctionToolResult); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported content block type %q", block.Type)
	}
	return nil
}

// validateAgenticContentBlockExtra permits only the bounded Responses fields
// the pinned agenticopenai adapter needs to replay assistant output items
// faithfully. Arbitrary provider payloads remain outside the durable
// transcript surface.
func validateAgenticContentBlockExtra(role schema.AgenticRoleType, extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	if role != schema.AgenticRoleTypeAssistant {
		return errors.New("content block extra is only valid on assistant messages")
	}
	for key, value := range extra {
		switch key {
		case "openai-item-id", "openai-item-status", "openai-tool-call-namespace":
			rv := reflect.ValueOf(value)
			if !rv.IsValid() || rv.Kind() != reflect.String || rv.String() == "" {
				return fmt.Errorf("content block extra %q must be a non-empty string", key)
			}
		default:
			return fmt.Errorf("content block extra %q is not persisted", key)
		}
	}
	return nil
}

func populatedAgenticVariants(b *schema.ContentBlock) int {
	n := 0
	variants := []bool{
		b.Reasoning != nil,
		b.UserInputText != nil,
		b.UserInputImage != nil,
		b.UserInputAudio != nil,
		b.UserInputVideo != nil,
		b.UserInputFile != nil,
		b.AssistantGenText != nil,
		b.AssistantGenImage != nil,
		b.AssistantGenAudio != nil,
		b.AssistantGenVideo != nil,
		b.FunctionToolCall != nil,
		b.FunctionToolResult != nil,
		b.ToolSearchFunctionToolResult != nil,
		b.ServerToolCall != nil,
		b.ServerToolResult != nil,
		b.MCPToolCall != nil,
		b.MCPToolResult != nil,
		b.MCPListToolsResult != nil,
		b.MCPToolApprovalRequest != nil,
		b.MCPToolApprovalResponse != nil,
	}
	for _, populated := range variants {
		if populated {
			n++
		}
	}
	return n
}

func validateFunctionToolCall(call *schema.FunctionToolCall) error {
	if call.CallID == "" || call.Name == "" {
		return errors.New("function tool call requires call_id and name")
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return fmt.Errorf("function tool call arguments must be a JSON object: %w", err)
	}
	if args == nil {
		return errors.New("function tool call arguments must be a JSON object")
	}
	return nil
}

func validateFunctionToolResult(result *schema.FunctionToolResult) error {
	if result.CallID == "" || result.Name == "" {
		return errors.New("function tool result requires call_id and name")
	}
	if len(result.Content) == 0 {
		return errors.New("function tool result requires content")
	}
	for i, content := range result.Content {
		if content == nil {
			return fmt.Errorf("function tool result content %d is nil", i)
		}
		if len(content.Extra) != 0 {
			return fmt.Errorf("function tool result content %d has extra metadata", i)
		}
		if content.Type != schema.FunctionToolResultContentBlockTypeText || content.Text == nil ||
			content.Image != nil || content.Audio != nil || content.Video != nil || content.File != nil {
			return fmt.Errorf("function tool result content %d is not exactly one text block", i)
		}
	}
	return nil
}

// AgenticMessageText returns user- or assistant-visible text in block order.
func AgenticMessageText(msg *schema.AgenticMessage) string {
	if msg == nil {
		return ""
	}
	parts := make([]string, 0, len(msg.ContentBlocks))
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		switch block.Type {
		case schema.ContentBlockTypeUserInputText:
			if block.UserInputText != nil && block.UserInputText.Text != "" {
				parts = append(parts, block.UserInputText.Text)
			}
		case schema.ContentBlockTypeAssistantGenText:
			if block.AssistantGenText != nil && block.AssistantGenText.Text != "" {
				parts = append(parts, block.AssistantGenText.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func AgenticMessageToolCalls(msg *schema.AgenticMessage) []schema.FunctionToolCall {
	if msg == nil {
		return nil
	}
	var calls []schema.FunctionToolCall
	for _, block := range msg.ContentBlocks {
		if block != nil && block.Type == schema.ContentBlockTypeFunctionToolCall && block.FunctionToolCall != nil {
			calls = append(calls, *block.FunctionToolCall)
		}
	}
	return calls
}

func AgenticMessageToolResults(msg *schema.AgenticMessage) []schema.FunctionToolResult {
	if msg == nil {
		return nil
	}
	var results []schema.FunctionToolResult
	for _, block := range msg.ContentBlocks {
		if block != nil && block.Type == schema.ContentBlockTypeFunctionToolResult && block.FunctionToolResult != nil {
			results = append(results, *block.FunctionToolResult)
		}
	}
	return results
}

func AgenticMessageUsage(msg *schema.AgenticMessage) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
		return nil
	}
	usage := *msg.ResponseMeta.TokenUsage
	return &usage
}

func NewAgenticUserText(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{
			Text: text,
		})},
	}
}

func NewAgenticAssistantText(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{
			Text: text,
		})},
	}
}
