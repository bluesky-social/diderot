package sessiondecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatCodex is a Codex CLI rollout
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl). Mapping notes: FORMATS.md.
const FormatCodex = "codex-rollout-jsonl"

const codexDecoderVersion = "1"

// Codex decodes Codex rollout JSONL. Item ids are native where the rollout
// carries them; otherwise a tool call or output takes its call_id plus the
// record kind, and anything else is positional (its line number).
type Codex struct{}

func (Codex) Format() string  { return FormatCodex }
func (Codex) Version() string { return codexDecoderVersion }

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Phase     string          `json:"phase"`
	Content   json.RawMessage `json:"content"`
	Summary   []codexPart     `json:"summary"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
}

type codexPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

type codexMeta struct {
	CLIVersion string `json:"cli_version"`
}

type codexTurnContext struct {
	Model string `json:"model"`
}

type codexCompacted struct {
	Message string `json:"message"`
}

func (Codex) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatCodex, DecoderVersion: codexDecoderVersion, Harness: "codex"}
	out := newEmitter(&doc, emit)
	model := ""
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var ln codexLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if ln.Type == "" || len(ln.Payload) == 0 {
			return structural("line %d is not a rollout item (no type/payload)", lineNo)
		}
		switch ln.Type {
		case "session_meta":
			var meta codexMeta
			if err := json.Unmarshal(ln.Payload, &meta); err != nil {
				return structural("line %d session_meta: %v", lineNo, err)
			}
			doc.SourceVersion = meta.CLIVersion
			return nil
		case "turn_context":
			var tc codexTurnContext
			if err := json.Unmarshal(ln.Payload, &tc); err != nil {
				return structural("line %d turn_context: %v", lineNo, err)
			}
			model = tc.Model
			return nil
		case "compacted":
			var c codexCompacted
			if err := json.Unmarshal(ln.Payload, &c); err != nil {
				return structural("line %d compacted: %v", lineNo, err)
			}
			text := c.Message
			if text == "" {
				text = "Conversation compacted"
			}
			return codexAdd(out, ln, lineNo, Record{
				ID: fmt.Sprintf("line:%d", lineNo), Fidelity: FidelityPositional,
				Role: "system", Phase: "compaction", Content: textContent(text),
			})
		case "response_item":
			var item codexItem
			if err := json.Unmarshal(ln.Payload, &item); err != nil {
				return structural("line %d response_item: %v", lineNo, err)
			}
			rec, ok, err := codexRecord(item, model, &doc)
			if err != nil {
				return structural("line %d: %v", lineNo, err)
			}
			if !ok {
				return nil
			}
			rec.ID, rec.Fidelity = codexID(item, lineNo)
			return codexAdd(out, ln, lineNo, rec)
		}
		// event_msg duplicates response items for the UI; token and state
		// bookkeeping carries no conversation.
		return nil
	})
	if err != nil {
		return Document{}, err
	}
	if err := out.finish(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func codexAdd(out *emitter, ln codexLine, lineNo int, rec Record) error {
	at, err := recordTime(ln.Timestamp, fmt.Sprintf("line %d", lineNo))
	if err != nil {
		return err
	}
	rec.Timestamp, rec.Time = ln.Timestamp, at
	return out.add(rec)
}

func codexID(item codexItem, lineNo int) (string, Fidelity) {
	if item.ID != "" {
		return item.ID, FidelityNative
	}
	if item.CallID != "" {
		switch item.Type {
		case "function_call", "custom_tool_call", "local_shell_call":
			return item.CallID + ":call", FidelityNative
		case "function_call_output", "custom_tool_call_output":
			return item.CallID + ":output", FidelityNative
		}
	}
	return fmt.Sprintf("line:%d", lineNo), FidelityPositional
}

// codexRecord maps one response item; ok=false skips items with nothing to
// show (reasoning that carries only encrypted content).
func codexRecord(item codexItem, model string, doc *Document) (Record, bool, error) {
	switch item.Type {
	case "message":
		role, phase := item.Role, item.Phase
		switch item.Role {
		case "user", "assistant":
		case "developer", "system":
			role, phase = "system", item.Role
		default:
			doc.WarningRecords++
			return Record{Role: "system", Phase: "normalization", Content: blocksContent([]ContentBlock{unsupportedBlock("Codex " + item.Role + " message")})}, true, nil
		}
		blocks, err := codexBlocks(item.Content, doc)
		if err != nil {
			return Record{}, false, err
		}
		rec := Record{Role: role, Phase: phase, Content: blocksContent(blocks)}
		if role == "assistant" {
			rec.Model = model
		}
		return rec, true, nil
	case "reasoning":
		var texts []string
		for _, p := range item.Summary {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		var content []codexPart
		if len(item.Content) > 0 && string(item.Content) != "null" {
			if err := json.Unmarshal(item.Content, &content); err != nil {
				return Record{}, false, fmt.Errorf("reasoning content: %w", err)
			}
		}
		for _, p := range content {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			return Record{}, false, nil
		}
		return Record{Role: "reasoning", Model: model, Content: textContent(strings.Join(texts, "\n\n"))}, true, nil
	case "function_call", "custom_tool_call", "local_shell_call":
		if item.CallID == "" {
			return Record{}, false, fmt.Errorf("%s has no call_id", item.Type)
		}
		name, args := item.Name, rawString(item.Arguments)
		switch item.Type {
		case "custom_tool_call":
			args = rawString(item.Input)
		case "local_shell_call":
			name, args = "local_shell", string(item.Action)
		}
		if item.Namespace != "" {
			name = item.Namespace + "." + name
		}
		return Record{Role: "assistant", Model: model, ToolCalls: []ToolCall{{ID: item.CallID, Name: name, Args: args}}}, true, nil
	case "function_call_output", "custom_tool_call_output":
		content, isErr, err := codexOutput(item.Output, doc)
		if err != nil {
			return Record{}, false, err
		}
		return Record{Role: "tool", ToolCallID: item.CallID, Content: content, IsError: isErr}, true, nil
	case "web_search_call":
		return Record{Role: "assistant", Model: model, Content: textContent("[web search] " + string(item.Action))}, true, nil
	case "agent_message":
		blocks, err := codexBlocks(item.Content, doc)
		if err != nil {
			return Record{}, false, err
		}
		return Record{Role: "system", Phase: "agent_message", Content: blocksContent(blocks)}, true, nil
	}
	doc.WarningRecords++
	return Record{Role: "system", Phase: "normalization", Content: blocksContent([]ContentBlock{unsupportedBlock("Codex " + item.Type)})}, true, nil
}

func codexBlocks(raw json.RawMessage, doc *Document) ([]ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}
	var parts []codexPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "summary_text", "reasoning_text":
			out = append(out, ContentBlock{Type: "text", Text: p.Text})
		case "input_image":
			out = append(out, imageBlock(dataURLMediaType(p.ImageURL)))
		default:
			doc.WarningRecords++
			out = append(out, unsupportedBlock("Codex "+p.Type))
		}
	}
	return out, nil
}

// codexOutput maps a tool output: a string, a content-item array, or (in
// older rollouts) an object carrying content and a success flag.
func codexOutput(raw json.RawMessage, doc *Document) (json.RawMessage, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return textContent(s), false, nil
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		blocks, err := codexBlocks(raw, doc)
		return blocksContent(blocks), false, err
	}
	var obj struct {
		Content json.RawMessage `json:"content"`
		Success *bool           `json:"success"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return textContent(string(scrubInline(raw))), false, nil
	}
	failed := obj.Success != nil && !*obj.Success
	if len(obj.Content) == 0 || string(obj.Content) == "null" {
		return textContent(string(scrubInline(raw))), failed, nil
	}
	blocks, err := codexBlocks(obj.Content, doc)
	if err != nil {
		return textContent(string(scrubInline(raw))), failed, nil
	}
	return blocksContent(blocks), failed, nil
}
