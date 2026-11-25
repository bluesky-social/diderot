package sessiondecode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FormatPi is a Pi coding agent session
// (~/.pi/agent/sessions/--<cwd>--/<timestamp>_<session>.jsonl). Mapping notes:
// FORMATS.md.
const FormatPi = "pi-session-jsonl"

const piDecoderVersion = "1"

// Pi decodes Pi session JSONL: a header line, then entries that form a tree
// through id and parentId. Pi only appends entries and never edits one, so
// ids (including split-part suffixes) are native.
type Pi struct{}

func (Pi) Format() string  { return FormatPi }
func (Pi) Version() string { return piDecoderVersion }

type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Version   *int            `json:"version"`
	Message   *piMessage      `json:"message"`
	Summary   string          `json:"summary"`
	Content   json.RawMessage `json:"content"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Model        string          `json:"model"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	ToolCallID   string          `json:"toolCallId"`
	IsError      bool            `json:"isError"`
	Command      string          `json:"command"`
	Output       string          `json:"output"`
	ExitCode     *int            `json:"exitCode"`
	Cancelled    bool            `json:"cancelled"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	MimeType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (Pi) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatPi, DecoderVersion: piDecoderVersion, Harness: "pi"}
	byID := map[string]*piEntry{}
	leaf := ""
	header := false
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var e piEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if !header {
			// Pi reads a file whose first entry is not its header as no session.
			if e.Type != "session" || e.ID == "" {
				return structural("line %d is not a Pi session header", lineNo)
			}
			header = true
			if e.Version != nil {
				doc.SourceVersion = strconv.Itoa(*e.Version)
			}
			return nil
		}
		if e.Type == "session" {
			return nil
		}
		if e.ID == "" {
			return structural("line %d: %s entry has no id", lineNo, e.Type)
		}
		// The later copy of a repeated id wins, as in Pi's own index.
		byID[e.ID] = &e
		leaf = e.ID
		return nil
	})
	if err != nil {
		return Document{}, err
	}

	// Pi's current position is the last entry in the file; the live branch is
	// its parent chain. A parent missing from the file ends the chain.
	var path []*piEntry
	seen := map[string]bool{}
	for cur := leaf; cur != ""; {
		if seen[cur] {
			return Document{}, structural("parent cycle at entry %q", cur)
		}
		seen[cur] = true
		e, ok := byID[cur]
		if !ok {
			break
		}
		path = append(path, e)
		cur = deref(e.ParentID)
	}

	out := newEmitter(&doc, emit)
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		parts, err := piParts(e, &doc)
		if err != nil {
			return Document{}, err
		}
		if len(parts) == 0 {
			continue
		}
		at, err := recordTime(e.Timestamp, fmt.Sprintf("entry %q", e.ID))
		if err != nil {
			return Document{}, err
		}
		for _, p := range parts {
			rec := p.rec
			rec.ID = e.ID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", e.ID, p.kind, p.pos)
			}
			rec.Fidelity = FidelityNative
			rec.Timestamp = e.Timestamp
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

// piPart is one record cut from an entry; pos is the index of its first
// content block within the entry.
type piPart struct {
	kind string
	pos  int
	rec  Record
}

func piParts(e *piEntry, doc *Document) ([]piPart, error) {
	system := func(phase, text string) []piPart {
		return []piPart{{kind: "system", rec: Record{Role: "system", Phase: phase, Content: textContent(text)}}}
	}
	switch e.Type {
	case "message":
		if e.Message == nil {
			return nil, structural("message entry %q has no message", e.ID)
		}
		return piMessageParts(e, doc)
	case "compaction":
		text := e.Summary
		if text == "" {
			text = "Conversation compacted"
		}
		return system("compaction", text), nil
	case "branch_summary":
		if e.Summary == "" {
			return nil, nil
		}
		return system("branch_summary", e.Summary), nil
	case "custom_message":
		return piCustomParts(e.ID, e.Content, doc)
	case "model_change", "thinking_level_change", "usage", "custom", "label", "session_info", "context_edit":
		// Session state and bookmarks: they carry the chain but no conversation.
		return nil, nil
	}
	doc.WarningRecords++
	return []piPart{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("Pi " + e.Type + " entry")})}}}, nil
}

func piMessageParts(e *piEntry, doc *Document) ([]piPart, error) {
	m := e.Message
	switch m.Role {
	case "user":
		blocks, err := piContent(m.Content, doc)
		if err != nil {
			return nil, structural("entry %q content: %v", e.ID, err)
		}
		if len(blocks) == 0 {
			return nil, nil
		}
		// A skill prompt is what the person typed; the skill file Pi put in
		// its place follows it as injected text.
		var skills []piPart
		for i, b := range blocks {
			if b.Type != "text" {
				continue
			}
			if typed, skill, ok := piSkillPrompt(b.Text); ok {
				blocks[i].Text = typed
				skills = append(skills, piPart{kind: "system", pos: i, rec: Record{Role: "system", Phase: "meta", Content: textContent(skill)}})
			}
		}
		return append([]piPart{{kind: "user", rec: Record{Role: "user", Content: blocksContent(blocks)}}}, skills...), nil
	case "assistant":
		return piAssistantParts(e, doc)
	case "toolResult":
		blocks, err := piContent(m.Content, doc)
		if err != nil {
			return nil, structural("entry %q content: %v", e.ID, err)
		}
		return []piPart{{kind: "tool_result", rec: Record{
			Role: "tool", ToolCallID: m.ToolCallID, Content: blocksContent(blocks), IsError: m.IsError,
		}}}, nil
	case "bashExecution":
		// A command the person ran with `!`; Pi gives it to the model as
		// this user text.
		text := "Ran `" + m.Command + "`\n"
		if m.Output != "" {
			text += "```\n" + m.Output + "\n```"
		} else {
			text += "(no output)"
		}
		if m.Cancelled {
			text += "\n\n(command cancelled)"
		} else if m.ExitCode != nil && *m.ExitCode != 0 {
			text += fmt.Sprintf("\n\nCommand exited with code %d", *m.ExitCode)
		}
		return []piPart{{kind: "user", rec: Record{Role: "user", Content: textContent(text)}}}, nil
	case "custom":
		return piCustomParts(e.ID, m.Content, doc)
	case "system":
		// The prompt and tool declarations Pi sends, not conversation.
		return nil, nil
	}
	doc.WarningRecords++
	return []piPart{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("Pi " + m.Role + " message")})}}}, nil
}

// piCustomParts maps a message an extension added to the model's context.
func piCustomParts(id string, content json.RawMessage, doc *Document) ([]piPart, error) {
	blocks, err := piContent(content, doc)
	if err != nil {
		return nil, structural("entry %q content: %v", id, err)
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	return []piPart{{kind: "system", rec: Record{Role: "system", Phase: "custom", Content: blocksContent(blocks)}}}, nil
}

func piBlocks(raw json.RawMessage) ([]piBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []piBlock{{Type: "text", Text: s}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	// Only the kinds the decoder reads are decoded whole, so a block of
	// another kind becomes a notice whatever its fields hold.
	blocks := make([]piBlock, len(items))
	for i, item := range items {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			return nil, err
		}
		blocks[i].Type = head.Type
		switch head.Type {
		case "text", "image", "thinking", "toolCall":
			if err := json.Unmarshal(item, &blocks[i]); err != nil {
				return nil, err
			}
		}
	}
	return blocks, nil
}

// piContent maps user, tool result, and custom content: text and images.
func piContent(raw json.RawMessage, doc *Document) ([]ContentBlock, error) {
	blocks, err := piBlocks(raw)
	if err != nil {
		return nil, err
	}
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, ContentBlock{Type: "text", Text: b.Text})
		case "image":
			out = append(out, imageBlock(b.MimeType))
		default:
			doc.WarningRecords++
			out = append(out, unsupportedBlock("Pi "+b.Type))
		}
	}
	return out, nil
}

func piAssistantParts(e *piEntry, doc *Document) ([]piPart, error) {
	m := e.Message
	blocks, err := piBlocks(m.Content)
	if err != nil {
		return nil, structural("entry %q content: %v", e.ID, err)
	}
	var parts []piPart
	var open *piPart
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
	for i, b := range blocks {
		if b.Type == "thinking" {
			flush()
			if b.Thinking != "" {
				parts = append(parts, piPart{kind: "reasoning", pos: i, rec: Record{Role: "reasoning", Model: m.Model, Content: textContent(b.Thinking)}})
			}
			continue
		}
		if open == nil {
			open = &piPart{kind: "assistant", pos: i, rec: Record{Role: "assistant", Model: m.Model}}
		}
		switch b.Type {
		case "text":
			openBlocks = append(openBlocks, ContentBlock{Type: "text", Text: b.Text})
		case "toolCall":
			if b.ID == "" {
				return nil, structural("entry %q has a tool call without an id", e.ID)
			}
			open.rec.ToolCalls = append(open.rec.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Arguments)})
		default:
			doc.WarningRecords++
			openBlocks = append(openBlocks, unsupportedBlock("Pi "+b.Type))
		}
	}
	flush()
	// A turn that failed or was cancelled ends with Pi's reason.
	if m.StopReason == "error" || m.StopReason == "aborted" {
		text := m.ErrorMessage
		if text == "" {
			text = "Assistant turn ended: " + m.StopReason
		}
		parts = append(parts, piPart{kind: "system", pos: len(blocks), rec: Record{
			Role: "system", Phase: m.StopReason, Content: textContent(text),
		}})
	}
	return parts, nil
}

// piSkill is the block Pi puts in place of a `/skill:<name> <args>` prompt:
// the skill file's text, then the arguments.
var piSkill = regexp.MustCompile(`^<skill name="([^"]+)" location="([^"]+)">\n([\s\S]*?)\n</skill>(?:\n\n([\s\S]+))?$`)

// piSkillPrompt splits an expanded skill invocation into what the person
// typed and the text of the skill file.
func piSkillPrompt(text string) (typed, skill string, ok bool) {
	m := piSkill.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	typed = "/skill:" + m[1]
	if args := strings.TrimSpace(m[4]); args != "" {
		typed += " " + args
	}
	return typed, m[3], true
}
