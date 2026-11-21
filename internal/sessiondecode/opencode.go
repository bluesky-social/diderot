package sessiondecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FormatOpenCode is an OpenCode 2 session as `opencode session export`
// writes it: one JSON document, {"info": …, "messages": […]}. OpenCode keeps
// its sessions in a SQLite database, from which diderot sessions builds the
// same document. Mapping notes: FORMATS.md.
const FormatOpenCode = "opencode-export-json"

const openCodeDecoderVersion = "1"

// OpenCode decodes an OpenCode session export. Every message carries its
// own id (msg_…), which OpenCode keeps while it rewrites the message, so ids
// are native.
type OpenCode struct{}

func (OpenCode) Format() string  { return FormatOpenCode }
func (OpenCode) Version() string { return openCodeDecoderVersion }

type ocExport struct {
	Info *struct {
		Revert *struct {
			MessageID string `json:"messageID"`
		} `json:"revert"`
	} `json:"info"`
	Messages *[]json.RawMessage `json:"messages"`
}

type ocModel struct {
	ID string `json:"id"`
}

type ocError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ocFile struct {
	Mime string `json:"mime"`
	Name string `json:"name"`
}

type ocToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	Metadata json.RawMessage `json:"metadata"`
	Error    *ocError        `json:"error"`
}

func (OpenCode) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatOpenCode, DecoderVersion: openCodeDecoderVersion, Harness: "opencode"}
	var export ocExport
	if err := json.Unmarshal(data, &export); err != nil {
		return Document{}, structural("not an OpenCode session export: %v", err)
	}
	if export.Messages == nil {
		return Document{}, structural("not an OpenCode session export (no messages)")
	}
	// A staged revert hides the message it names and every later one until
	// OpenCode deletes them or the revert is undone.
	revertAt := ""
	if export.Info != nil && export.Info.Revert != nil {
		revertAt = export.Info.Revert.MessageID
	}
	out := newEmitter(&doc, emit)
	for i, raw := range *export.Messages {
		// Each type is read only for its own fields, so a message of an
		// unknown type never fails to parse.
		var head struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Time struct {
				Created int64 `json:"created"`
			} `json:"time"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return Document{}, structural("message %d: %v", i, err)
		}
		if head.ID == "" || head.Type == "" {
			return Document{}, structural("message %d has no id or type", i)
		}
		if head.ID == revertAt {
			break
		}
		parts, err := ocParts(head.Type, raw, &doc)
		if err != nil {
			return Document{}, structural("message %q: %v", head.ID, err)
		}
		if len(parts) == 0 {
			continue
		}
		if head.Time.Created <= 0 {
			return Document{}, structural("message %q has no time", head.ID)
		}
		at := time.UnixMilli(head.Time.Created).UTC()
		for _, p := range parts {
			rec := p.rec
			rec.ID = head.ID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", head.ID, p.kind, p.pos)
			}
			rec.Fidelity, rec.Timestamp, rec.Time = FidelityNative, at.Format(time.RFC3339Nano), at
			if err := out.add(rec); err != nil {
				return Document{}, err
			}
		}
	}
	if err := out.finish(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// ocParts maps one message onto records; none for bookkeeping that is not
// conversation.
func ocParts(typ string, raw json.RawMessage, doc *Document) ([]ccPart, error) {
	switch typ {
	case "user":
		var v struct {
			Text  string   `json:"text"`
			Files []ocFile `json:"files"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		var blocks []ContentBlock
		if v.Text != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: v.Text})
		}
		for _, f := range v.Files {
			blocks = append(blocks, ocAttachment(f.Mime, f.Name))
		}
		return []ccPart{{kind: "user", rec: Record{Role: "user", Content: blocksContent(blocks)}}}, nil
	case "assistant":
		var v struct {
			Model   ocModel           `json:"model"`
			Content []json.RawMessage `json:"content"`
			Error   *ocError          `json:"error"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		parts, err := ocAssistantParts(v.Content, v.Model.ID, doc)
		if err != nil {
			return nil, err
		}
		// A step that failed or was interrupted says so after whatever it
		// produced.
		if v.Error != nil && v.Error.Message != "" {
			phase := "error"
			if v.Error.Type == "aborted" {
				phase = "interrupted"
			}
			parts = append(parts, ccPart{kind: "system", pos: len(v.Content), rec: Record{
				Role: "system", Phase: phase, Content: textContent(v.Error.Message),
			}})
		}
		return parts, nil
	case "synthetic", "system", "skill":
		// Text OpenCode puts in front of the model: a subagent's report, a
		// notice that the tools changed, a skill's instructions.
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return ocSystem(typ, v.Text), nil
	case "shell":
		// A command the person ran directly, not through the model.
		var v struct {
			Command string `json:"command"`
			Exit    *int   `json:"exit"`
			Output  *struct {
				Output string `json:"output"`
			} `json:"output"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		text := "$ " + v.Command
		if v.Output != nil && v.Output.Output != "" {
			text += "\n" + v.Output.Output
		}
		if v.Exit != nil && *v.Exit != 0 {
			text += fmt.Sprintf("\n[exit %d]", *v.Exit)
		}
		return ocSystem("shell", text), nil
	case "compaction":
		var v struct {
			Status  string `json:"status"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.Status != "completed" {
			return nil, nil
		}
		text := v.Summary
		if text == "" {
			text = "Conversation compacted"
		}
		return ocSystem("compaction", text), nil
	case "idle", "agent-switched", "model-switched", "location-switched":
		// Bookkeeping: the end of a turn, and the agent, model and directory
		// in force, which each assistant message records itself.
		return nil, nil
	}
	doc.WarningRecords++
	return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("OpenCode " + typ + " message")})}}}, nil
}

func ocSystem(phase, text string) []ccPart {
	if text == "" {
		return nil
	}
	return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: phase, Content: textContent(text)}}}
}

// ocAttachment stands in for an attached file, whose bytes (inline data, or
// a media: reference to OpenCode's blob table) are never read.
func ocAttachment(mime, name string) ContentBlock {
	if strings.HasPrefix(mime, "image/") {
		return imageBlock(mime)
	}
	label := mime
	if name != "" {
		label = name + " (" + mime + ")"
	}
	return ContentBlock{Type: "text", Text: "[Attached file " + label + " omitted from the searchable view.]"}
}

// ocAssistantParts splits an assistant message at each reasoning part, as
// for Claude Code. OpenCode keeps a tool call and its result in one part, so
// each tool part adds a call to the open assistant record and a tool record
// after it; text that follows a call starts a new assistant record after the
// results. A part's native data (thinking signatures, provider state) is
// never read.
func ocAssistantParts(content []json.RawMessage, model string, doc *Document) ([]ccPart, error) {
	var parts, results []ccPart
	var open *ccPart
	var openBlocks []ContentBlock
	flush := func() {
		if open != nil {
			open.rec.Content = blocksContent(openBlocks)
			if len(open.rec.Content) > 0 || len(open.rec.ToolCalls) > 0 {
				parts = append(parts, *open)
			}
		}
		parts = append(parts, results...)
		open, openBlocks, results = nil, nil, nil
	}
	for i, raw := range content {
		var p struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		switch p.Type {
		case "reasoning":
			flush()
			var r struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("part %d: %w", i, err)
			}
			if r.Text != "" {
				parts = append(parts, ccPart{kind: "reasoning", pos: i, rec: Record{Role: "reasoning", Model: model, Content: textContent(r.Text)}})
			}
			continue
		case "text":
			if len(results) > 0 {
				flush()
			}
		}
		if open == nil {
			open = &ccPart{kind: "assistant", pos: i, rec: Record{Role: "assistant", Model: model}}
		}
		switch p.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, fmt.Errorf("part %d: %w", i, err)
			}
			openBlocks = append(openBlocks, ContentBlock{Type: "text", Text: t.Text})
		case "tool":
			var t struct {
				ID    string      `json:"id"`
				Name  string      `json:"name"`
				State ocToolState `json:"state"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, fmt.Errorf("part %d: %w", i, err)
			}
			if t.ID == "" {
				return nil, fmt.Errorf("tool part %d has no id", i)
			}
			open.rec.ToolCalls = append(open.rec.ToolCalls, ToolCall{ID: t.ID, Name: t.Name, Args: ocToolArgs(t.State.Input)})
			if result, ok := ocToolResult(t.Name, t.State, doc); ok {
				result.ToolCallID = t.ID
				results = append(results, ccPart{kind: "tool_result", pos: i, rec: result})
			}
		default:
			doc.WarningRecords++
			openBlocks = append(openBlocks, unsupportedBlock("OpenCode "+p.Type))
		}
	}
	flush()
	return parts, nil
}

// ocToolArgs is a call's input: a JSON object once the call is complete,
// compacted so an export's indentation does not change it, or the text
// streamed so far while it is not.
func ocToolArgs(input json.RawMessage) string {
	if len(input) == 0 || string(input) == "null" {
		return ""
	}
	var compact bytes.Buffer
	if input[0] == '{' && json.Compact(&compact, input) == nil {
		return compact.String()
	}
	return rawString(input)
}

// ocToolResult maps a settled call's result; ok is false for a call still
// streaming or running, which has none yet.
func ocToolResult(name string, state ocToolState, doc *Document) (Record, bool) {
	switch state.Status {
	case "completed", "error":
	default:
		return Record{}, false
	}
	blocks := ocToolContent(state.Content, doc)
	if state.Status == "error" && len(blocks) == 0 && state.Error != nil {
		blocks = append(blocks, ContentBlock{Type: "text", Text: state.Error.Message})
	}
	return Record{Role: "tool", Content: blocksContent(blocks), IsError: ocToolFailed(name, state)}, true
}

// ocToolFailed is OpenCode's own rule for a failed call: an error, or a
// shell command that completed with a non-zero exit or timed out, or a Code
// Mode script that reported an error or ran a call that failed.
func ocToolFailed(name string, state ocToolState) bool {
	if state.Status == "error" {
		return true
	}
	var m struct {
		Exit      *float64 `json:"exit"`
		Timeout   *bool    `json:"timeout"`
		Error     *bool    `json:"error"`
		ToolCalls []struct {
			Status string `json:"status"`
		} `json:"toolCalls"`
	}
	if json.Unmarshal(state.Metadata, &m) != nil {
		return false
	}
	switch name {
	case "shell":
		return (m.Timeout != nil && *m.Timeout) || (m.Exit != nil && *m.Exit != 0)
	case "execute":
		if m.Error != nil && *m.Error {
			return true
		}
		for _, c := range m.ToolCalls {
			if c.Status == "error" {
				return true
			}
		}
	}
	return false
}

// ocToolContent maps a result's content items: text, or a file whose
// bytes (a data: URI, or a media: reference) are never read.
func ocToolContent(raw json.RawMessage, doc *Document) []ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		doc.WarningRecords++
		return []ContentBlock{{Type: "text", Text: string(scrubInline(raw))}}
	}
	out := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Mime string `json:"mime"`
			Name string `json:"name"`
		}
		if json.Unmarshal(item, &c) != nil {
			c.Type = ""
		}
		switch c.Type {
		case "text":
			out = append(out, ContentBlock{Type: "text", Text: c.Text})
		case "file":
			out = append(out, ocAttachment(c.Mime, c.Name))
		default:
			doc.WarningRecords++
			out = append(out, unsupportedBlock("OpenCode tool "+c.Type))
		}
	}
	return out
}
