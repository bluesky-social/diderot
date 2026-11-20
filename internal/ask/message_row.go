package ask

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// messageRow is one persisted ask_messages row in the v1 Eino AgenticMessage
// shape: the denormalized role column, the versioned JSON payload, and the
// nullable int32 token usage columns derived from assistant message usage.
type messageRow struct {
	Role                        string
	AgenticMessageSchemaVersion int16
	JSON                        json.RawMessage
	InputTokens                 *int32
	OutputTokens                *int32
	CacheReadTokens             *int32
	CacheWriteTokens            *int32
}

// einoMessageIDExtraKey is the private ADK key injected into every model and
// tool-result message before it reaches Eino handlers/events. The ID is useful
// only inside one Eino run; replay assigns fresh IDs, so it is deliberately not
// part of the durable v1 transcript shape.
//
// This value is pinned by github.com/cloudwego/eino/adk/internal, which
// cannot be imported outside its parent module. Keep the regression tests in
// this package in sync with an Eino upgrade.
const einoMessageIDExtraKey = "_eino_msg_id"

// encodeAgenticMessageRow encodes msg for persistence, deriving the
// denormalized role and nullable token columns from the AgenticMessage.
// Negative token counts and values that overflow int32 are rejected; user rows
// and assistant rows without usage leave the token columns nil.
func encodeAgenticMessageRow(msg *schema.AgenticMessage) (messageRow, error) {
	persisted := agenticMessageForPersistence(msg)
	raw, err := EncodeAgenticMessage(persisted)
	if err != nil {
		return messageRow{}, err
	}
	row := messageRow{
		Role:                        string(persisted.Role),
		AgenticMessageSchemaVersion: AgenticMessageSchemaVersion,
		JSON:                        raw,
	}
	usage := AgenticMessageUsage(persisted)
	if usage == nil {
		return row, nil
	}
	if usage.PromptTokens < 0 {
		return messageRow{}, errors.New("ask message: negative input tokens")
	}
	if usage.CompletionTokens < 0 {
		return messageRow{}, errors.New("ask message: negative output tokens")
	}
	if usage.PromptTokenDetails.CachedTokens < 0 {
		return messageRow{}, errors.New("ask message: negative cache read tokens")
	}
	if usage.TotalTokens < 0 {
		return messageRow{}, errors.New("ask message: negative total tokens")
	}
	input, err := tokenCountInt32("input", usage.PromptTokens)
	if err != nil {
		return messageRow{}, err
	}
	output, err := tokenCountInt32("output", usage.CompletionTokens)
	if err != nil {
		return messageRow{}, err
	}
	cacheRead, err := tokenCountInt32("cache read", usage.PromptTokenDetails.CachedTokens)
	if err != nil {
		return messageRow{}, err
	}
	if usage.TotalTokens > math.MaxInt32 {
		return messageRow{}, fmt.Errorf("ask message: total tokens overflow int32: %d", usage.TotalTokens)
	}
	row.InputTokens = input
	row.OutputTokens = output
	row.CacheReadTokens = cacheRead
	return row, nil
}

// agenticMessageForPersistence returns a non-mutating durable projection of an
// Eino message. It removes Eino's run-local message ID and transient native
// Claude response/text extensions, and it strips any embedded NUL byte
// (U+0000) from the message's text fields. Claude replay-critical state is
// carried by ordinary bounded fields instead: Reasoning.Signature and
// function call/result CallID values remain untouched.
//
// NUL stripping exists because Postgres's jsonb input function rejects the
// "\u0000" escape sequence Go's json.Marshal produces for an embedded NUL
// byte (SQLSTATE 22P05), for every text-bearing block type this function
// accepts (user input text, assistant generated text, reasoning, and
// function tool result text). Without this, ValidateAgenticMessage's
// contract -- that acceptance implies the message can be replayed losslessly
// -- did not hold: a message with an otherwise-valid NUL byte in its text
// failed the INSERT itself, discarding the whole batch (internal/
// sessioningest/store.go's boundedIndexError strips the same byte for the
// same reason, for its own column). A Claude thinking block is a partial
// exception: this strips Reasoning.Text but leaves Signature untouched, so a
// NUL in a signed Claude thinking block's text makes that signature no
// longer match on replay, which the provider may reject on a later turn in
// the same session. This needs a literal NUL in Claude thinking output,
// which is rare; nothing detects or repairs that case.
//
// Any other Extra on the copy is rejected by EncodeAgenticMessage, keeping
// the bounded durable schema; an unrecognized provider extension, such as
// OpenAIExtension, is not and remains on the copy as-is. Its own text (e.g.
// Reasoning.OpenAIExtension.Content[].Text) is not stripped here --
// stripNULFromEncodedAgenticMessage, called from EncodeAgenticMessage,
// backstops every such field generically.
func agenticMessageForPersistence(msg *schema.AgenticMessage) *schema.AgenticMessage {
	if msg == nil {
		return msg
	}
	stripMessageID := false
	if id, ok := msg.Extra[einoMessageIDExtraKey].(string); ok && id != "" {
		stripMessageID = true
	}
	stripClaudeMeta := msg.ResponseMeta != nil && msg.ResponseMeta.ClaudeExtension != nil
	stripClaudeText := false
	hasNUL := false
	for _, block := range msg.ContentBlocks {
		if block != nil && block.AssistantGenText != nil && block.AssistantGenText.ClaudeExtension != nil {
			stripClaudeText = true
		}
		if contentBlockHasNUL(block) {
			hasNUL = true
		}
	}
	if !stripMessageID && !stripClaudeMeta && !stripClaudeText && !hasNUL {
		return msg
	}

	cp := *msg
	if stripMessageID {
		if len(msg.Extra) == 1 {
			cp.Extra = nil
		} else {
			cp.Extra = make(map[string]any, len(msg.Extra)-1)
			for key, value := range msg.Extra {
				if key != einoMessageIDExtraKey {
					cp.Extra[key] = value
				}
			}
		}
	}
	if stripClaudeMeta {
		meta := *msg.ResponseMeta
		meta.ClaudeExtension = nil
		cp.ResponseMeta = &meta
	}
	if stripClaudeText || hasNUL {
		cp.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
		for i, block := range msg.ContentBlocks {
			if block == nil {
				continue
			}
			blockCopy := block
			if block.AssistantGenText != nil && block.AssistantGenText.ClaudeExtension != nil {
				bc := *block
				textCopy := *block.AssistantGenText
				textCopy.ClaudeExtension = nil
				bc.AssistantGenText = &textCopy
				blockCopy = &bc
			}
			cp.ContentBlocks[i] = stripContentBlockNUL(blockCopy)
		}
	}
	return &cp
}

// stripNUL removes embedded NUL bytes (U+0000) from s. Postgres rejects a
// literal NUL both in jsonb (the "\u0000" escape json.Marshal produces,
// SQLSTATE 22P05) and in a plain text column (the raw byte, SQLSTATE
// 22021), so any text reaching a persistence boundary needs this, matching
// internal/sessioningest/store.go's boundedIndexError.
func stripNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// contentBlockHasNUL reports whether any text field of block contains an
// embedded NUL byte.
func contentBlockHasNUL(block *schema.ContentBlock) bool {
	if block == nil {
		return false
	}
	switch {
	case block.UserInputText != nil:
		return strings.ContainsRune(block.UserInputText.Text, 0)
	case block.AssistantGenText != nil:
		return strings.ContainsRune(block.AssistantGenText.Text, 0)
	case block.Reasoning != nil:
		return strings.ContainsRune(block.Reasoning.Text, 0)
	case block.FunctionToolResult != nil:
		for _, content := range block.FunctionToolResult.Content {
			if content != nil && content.Text != nil && strings.ContainsRune(content.Text.Text, 0) {
				return true
			}
		}
	}
	return false
}

// stripContentBlockNUL returns a copy of block with embedded NUL bytes
// removed from its text fields, or block itself when it has none.
func stripContentBlockNUL(block *schema.ContentBlock) *schema.ContentBlock {
	if !contentBlockHasNUL(block) {
		return block
	}
	cp := *block
	switch {
	case cp.UserInputText != nil:
		t := *cp.UserInputText
		t.Text = stripNUL(t.Text)
		cp.UserInputText = &t
	case cp.AssistantGenText != nil:
		t := *cp.AssistantGenText
		t.Text = stripNUL(t.Text)
		cp.AssistantGenText = &t
	case cp.Reasoning != nil:
		r := *cp.Reasoning
		r.Text = stripNUL(r.Text)
		cp.Reasoning = &r
	case cp.FunctionToolResult != nil:
		res := *cp.FunctionToolResult
		res.Content = append([]*schema.FunctionToolResultContentBlock(nil), res.Content...)
		for i, content := range res.Content {
			if content == nil || content.Text == nil || !strings.ContainsRune(content.Text.Text, 0) {
				continue
			}
			contentCopy := *content
			textCopy := *content.Text
			textCopy.Text = stripNUL(textCopy.Text)
			contentCopy.Text = &textCopy
			res.Content[i] = &contentCopy
		}
		cp.FunctionToolResult = &res
	}
	return &cp
}

// tokenCountInt32 converts an int token count into the DB column value,
// rejecting values that overflow int32.
func tokenCountInt32(name string, count int) (*int32, error) {
	if count > math.MaxInt32 {
		return nil, fmt.Errorf("ask message: %s tokens overflow int32: %d", name, count)
	}
	v := int32(count)
	return &v, nil
}

// decodeAgenticMessageRow validates a persisted row and returns the canonical
// AgenticMessage. Unsupported schema versions and a role that does not match
// the persisted JSON are rejected.
func decodeAgenticMessageRow(row messageRow) (*schema.AgenticMessage, error) {
	if row.AgenticMessageSchemaVersion != AgenticMessageSchemaVersion {
		return nil, fmt.Errorf("ask message: unsupported schema version %d", row.AgenticMessageSchemaVersion)
	}
	msg, err := DecodeAgenticMessage(row.JSON)
	if err != nil {
		return nil, err
	}
	if row.Role != string(msg.Role) {
		return nil, fmt.Errorf("ask message: role mismatch: column %q, message %q", row.Role, msg.Role)
	}
	expected, err := encodeAgenticMessageRow(msg)
	if err != nil {
		return nil, err
	}
	for _, count := range []struct {
		name string
		got  *int32
		want *int32
	}{
		{name: "input_tokens", got: row.InputTokens, want: expected.InputTokens},
		{name: "output_tokens", got: row.OutputTokens, want: expected.OutputTokens},
		{name: "cache_read_tokens", got: row.CacheReadTokens, want: expected.CacheReadTokens},
		{name: "cache_write_tokens", got: row.CacheWriteTokens, want: expected.CacheWriteTokens},
	} {
		if !equalNullableInt32(count.got, count.want) {
			return nil, fmt.Errorf("ask message: %s does not match message usage", count.name)
		}
	}
	return msg, nil
}

func equalNullableInt32(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
