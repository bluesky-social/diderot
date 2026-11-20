package sessiondecode

import (
	"encoding/json"
	"fmt"
)

// FormatClaudeCode is a Claude Code session transcript
// (~/.claude/projects/<project>/<session>.jsonl). Mapping notes: FORMATS.md.
const FormatClaudeCode = "claude-code-jsonl"

const claudeCodeDecoderVersion = "1"

// ClaudeCode decodes Claude Code session JSONL. Every entry carries a native
// uuid and Claude Code never edits an entry once written, so ids (including
// split-part suffixes) are native.
type ClaudeCode struct{}

func (ClaudeCode) Format() string  { return FormatClaudeCode }
func (ClaudeCode) Version() string { return claudeCodeDecoderVersion }

type ccLine struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	UUID              string          `json:"uuid"`
	ParentUUID        *string         `json:"parentUuid"`
	LogicalParentUUID *string         `json:"logicalParentUuid"`
	IsSidechain       bool            `json:"isSidechain"`
	IsMeta            bool            `json:"isMeta"`
	IsCompactSummary  bool            `json:"isCompactSummary"`
	Timestamp         string          `json:"timestamp"`
	Version           string          `json:"version"`
	Content           json.RawMessage `json:"content"`
	Message           *ccMessage      `json:"message"`
}

type ccMessage struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type ccBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		MediaType string `json:"media_type"`
	} `json:"source"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (ClaudeCode) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatClaudeCode, DecoderVersion: claudeCodeDecoderVersion, Harness: "claude-code"}
	byUUID := map[string]*ccLine{}
	leaf := ""
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var ln ccLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if ln.UUID == "" {
			switch ln.Type {
			case "user", "assistant", "system":
				return structural("line %d: %s entry has no uuid", lineNo, ln.Type)
			}
			return nil
		}
		// A resumed session can rewrite an entry; the later copy wins.
		byUUID[ln.UUID] = &ln
		if !ln.IsSidechain {
			leaf = ln.UUID
		}
		return nil
	})
	if err != nil {
		return Document{}, err
	}

	// The live branch is the parent chain of the last main-chain entry.
	// A compact boundary has no parent; its logical parent continues the
	// chain through the compacted history still present in the file.
	var path []*ccLine
	seen := map[string]bool{}
	for cur := leaf; cur != ""; {
		if seen[cur] {
			return Document{}, structural("parent cycle at entry %q", cur)
		}
		seen[cur] = true
		ln, ok := byUUID[cur]
		if !ok {
			break // history absent from this snapshot is never reconstructed
		}
		path = append(path, ln)
		next := deref(ln.ParentUUID)
		if next == "" {
			next = deref(ln.LogicalParentUUID)
		}
		cur = next
	}

	out := newEmitter(&doc, emit)
	for i := len(path) - 1; i >= 0; i-- {
		ln := path[i]
		if ln.Version != "" {
			doc.SourceVersion = ln.Version
		}
		parts, err := ccParts(ln, &doc)
		if err != nil {
			return Document{}, err
		}
		if len(parts) == 0 {
			continue
		}
		at, err := recordTime(ln.Timestamp, fmt.Sprintf("entry %q", ln.UUID))
		if err != nil {
			return Document{}, err
		}
		for _, p := range parts {
			rec := p.rec
			rec.ID = ln.UUID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", ln.UUID, p.kind, p.pos)
			}
			rec.Fidelity = FidelityNative
			rec.Timestamp = ln.Timestamp
			rec.Time = at
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

// ccPart is one record cut from an entry; pos is the index of its first
// content block within the entry.
type ccPart struct {
	kind string
	pos  int
	rec  Record
}

func ccParts(ln *ccLine, doc *Document) ([]ccPart, error) {
	switch ln.Type {
	case "user":
		if ln.Message == nil {
			return nil, structural("%s entry %q has no message", ln.Type, ln.UUID)
		}
		role, phase := "user", ""
		switch {
		case ln.IsCompactSummary:
			role, phase = "system", "compaction_summary"
		case ln.IsMeta:
			role, phase = "system", "meta"
		}
		return ccUserParts(ln, role, phase, doc)
	case "assistant":
		if ln.Message == nil {
			return nil, structural("%s entry %q has no message", ln.Type, ln.UUID)
		}
		return ccAssistantParts(ln, doc)
	case "system":
		text := rawString(ln.Content)
		if ln.Subtype == "compact_boundary" {
			if text == "" {
				text = "Conversation compacted"
			}
			return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: "compaction", Content: textContent(text)}}}, nil
		}
		if len(ln.Content) == 0 || text == "" || text == "null" {
			return nil, nil
		}
		return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: ln.Subtype, Content: textContent(text)}}}, nil
	}
	switch ln.Type {
	case "attachment", "progress":
		// Harness context and progress carry the chain but no conversation.
		return nil, nil
	}
	doc.WarningRecords++
	return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("Claude Code " + ln.Type + " entry")})}}}, nil
}

func ccBlocks(raw json.RawMessage) ([]ccBlock, bool, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ccBlock{{Type: "text", Text: s}}, true, nil
	}
	var blocks []ccBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false, err
	}
	return blocks, false, nil
}

func ccUserParts(ln *ccLine, role, phase string, doc *Document) ([]ccPart, error) {
	blocks, _, err := ccBlocks(ln.Message.Content)
	if err != nil {
		return nil, structural("entry %q content: %v", ln.UUID, err)
	}
	var parts []ccPart
	var open *ccPart
	var openBlocks []ContentBlock
	flush := func() {
		if open != nil {
			open.rec.Content = blocksContent(openBlocks)
			parts = append(parts, *open)
		}
		open, openBlocks = nil, nil
	}
	for i, b := range blocks {
		switch b.Type {
		case "tool_result":
			flush()
			content, isErr, err := ccToolResult(b, doc)
			if err != nil {
				return nil, structural("entry %q tool result: %v", ln.UUID, err)
			}
			parts = append(parts, ccPart{kind: "tool_result", pos: i, rec: Record{
				Role: "tool", ToolCallID: b.ToolUseID, Content: content, IsError: isErr || b.IsError,
			}})
			continue
		}
		if open == nil {
			open = &ccPart{kind: role, pos: i, rec: Record{Role: role, Phase: phase}}
		}
		switch b.Type {
		case "text":
			openBlocks = append(openBlocks, ContentBlock{Type: "text", Text: b.Text})
		case "image":
			openBlocks = append(openBlocks, ccImage(b))
		default:
			doc.WarningRecords++
			openBlocks = append(openBlocks, unsupportedBlock("Claude Code "+b.Type))
		}
	}
	flush()
	return parts, nil
}

func ccImage(b ccBlock) ContentBlock {
	mediaType := ""
	if b.Source != nil {
		mediaType = b.Source.MediaType
	}
	return imageBlock(mediaType)
}

func ccToolResult(b ccBlock, doc *Document) (json.RawMessage, bool, error) {
	if len(b.Content) == 0 || string(b.Content) == "null" {
		return nil, false, nil
	}
	inner, isString, err := ccBlocks(b.Content)
	if err != nil {
		// Server-tool results may carry a structured object; keep it as
		// text without any inline payload.
		return textContent(string(scrubInline(b.Content))), false, nil
	}
	if isString {
		return textContent(inner[0].Text), false, nil
	}
	out := make([]ContentBlock, 0, len(inner))
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			out = append(out, ContentBlock{Type: "text", Text: ib.Text})
		case "image":
			out = append(out, ccImage(ib))
		default:
			doc.WarningRecords++
			out = append(out, unsupportedBlock("Claude Code "+ib.Type))
		}
	}
	return blocksContent(out), false, nil
}

func ccAssistantParts(ln *ccLine, doc *Document) ([]ccPart, error) {
	blocks, _, err := ccBlocks(ln.Message.Content)
	if err != nil {
		return nil, structural("entry %q content: %v", ln.UUID, err)
	}
	var parts []ccPart
	var open *ccPart
	var openBlocks []ContentBlock
	flush := func() {
		if open != nil {
			open.rec.Content = blocksContent(openBlocks)
			if len(open.rec.Content) > 0 || len(open.rec.ToolCalls) > 0 {
				parts = append(parts, *open)
			}
		}
		open, openBlocks = nil, nil
	}
	model := ln.Message.Model
	for i, b := range blocks {
		switch b.Type {
		case "thinking":
			flush()
			if b.Thinking != "" {
				parts = append(parts, ccPart{kind: "reasoning", pos: i, rec: Record{Role: "reasoning", Model: model, Content: textContent(b.Thinking)}})
			}
			continue
		case "redacted_thinking":
			continue
		case "tool_result", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "mcp_tool_result":
			flush()
			content, isErr, err := ccToolResult(b, doc)
			if err != nil {
				return nil, structural("entry %q tool result: %v", ln.UUID, err)
			}
			if b.Type != "tool_result" && content == nil {
				content = textContent(fmt.Sprintf("[%s]", b.Type))
			}
			parts = append(parts, ccPart{kind: "tool_result", pos: i, rec: Record{
				Role: "tool", ToolCallID: b.ToolUseID, Content: content, IsError: isErr || b.IsError,
			}})
			continue
		}
		if open == nil {
			open = &ccPart{kind: "assistant", pos: i, rec: Record{Role: "assistant", Model: model}}
		}
		switch b.Type {
		case "text":
			openBlocks = append(openBlocks, ContentBlock{Type: "text", Text: b.Text})
		case "tool_use", "server_tool_use", "mcp_tool_use":
			if b.ID == "" {
				return nil, structural("entry %q has a tool call without an id", ln.UUID)
			}
			open.rec.ToolCalls = append(open.rec.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
		case "image":
			openBlocks = append(openBlocks, ccImage(b))
		default:
			doc.WarningRecords++
			openBlocks = append(openBlocks, unsupportedBlock("Claude Code "+b.Type))
		}
	}
	flush()
	return parts, nil
}
