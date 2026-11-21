package sessiondecode

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// FormatGeminiCLI is a Gemini CLI session file
// (~/.gemini/tmp/<project>/chats/session-*.jsonl). Mapping notes: FORMATS.md.
const FormatGeminiCLI = "gemini-cli-session-jsonl"

const geminiCLIDecoderVersion = "1"

// GeminiCLI decodes Gemini CLI session JSONL. The file is a log: message
// lines are appended again whole when they change, and $set, $patch, and
// $rewindTo lines edit what came before. Decoding replays the log as Gemini
// CLI's own loader does and emits the messages that remain, under their
// native ids.
type GeminiCLI struct{}

func (GeminiCLI) Format() string  { return FormatGeminiCLI }
func (GeminiCLI) Version() string { return geminiCLIDecoderVersion }

type gcMessage struct {
	ID             string          `json:"id"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"`
	Content        json.RawMessage `json:"content"`
	DisplayContent json.RawMessage `json:"displayContent"`
	Thoughts       []gcThought     `json:"thoughts"`
	ToolCalls      []gcToolCall    `json:"toolCalls"`
	Model          string          `json:"model"`
}

type gcThought struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

type gcToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type gcPatch struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
	Updates []gcPatch       `json:"updates"`
	Remove  []string        `json:"removeIds"`
	Order   []string        `json:"orderIds"`
}

type gcPart struct {
	Text             *string             `json:"text"`
	Thought          bool                `json:"thought"`
	InlineData       *gcInlineData       `json:"inlineData"`
	FunctionCall     *gcFunctionCall     `json:"functionCall"`
	FunctionResponse *gcFunctionResponse `json:"functionResponse"`
}

type gcInlineData struct {
	MimeType string `json:"mimeType"`
}

type gcFunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type gcFunctionResponse struct {
	ID       string          `json:"id"`
	Response json.RawMessage `json:"response"`
	Parts    json.RawMessage `json:"parts"`
}

// gcLog is the conversation a session file's log leaves: its messages in
// order, a later copy of a message replacing the earlier one in place. Each
// message holds one slot; a slot it left by moving or going is stale, so
// every log operation costs time in proportion to its own size.
type gcLog struct {
	slots []string
	byID  map[string]*gcEntry
}

type gcEntry struct {
	msg  *gcMessage
	slot int
}

func (l *gcLog) live(i int) bool {
	e := l.byID[l.slots[i]]
	return e != nil && e.slot == i
}

func (l *gcLog) put(m *gcMessage) {
	if e, ok := l.byID[m.ID]; ok {
		e.msg = m
		return
	}
	l.byID[m.ID] = &gcEntry{msg: m, slot: len(l.slots)}
	l.slots = append(l.slots, m.ID)
}

// rewind drops the message and everything after it; an unknown message
// drops everything.
func (l *gcLog) rewind(id string) {
	from := 0
	if e, ok := l.byID[id]; ok {
		from = e.slot
	}
	for i := from; i < len(l.slots); i++ {
		if l.live(i) {
			delete(l.byID, l.slots[i])
		}
	}
	l.slots = l.slots[:from]
}

func (l *gcLog) patch(p gcPatch) {
	for _, u := range append([]gcPatch{p}, p.Updates...) {
		if e, ok := l.byID[u.ID]; ok && len(u.Content) > 0 {
			e.msg.Content = u.Content
		}
	}
	for _, id := range p.Remove {
		delete(l.byID, id)
	}
	// The listed messages move to the end in the listed order.
	moved := map[string]bool{}
	for _, id := range p.Order {
		if e, ok := l.byID[id]; ok && !moved[id] {
			moved[id] = true
			e.slot = len(l.slots)
			l.slots = append(l.slots, id)
		}
	}
}

func (l *gcLog) messages() []*gcMessage {
	var out []*gcMessage
	for i, id := range l.slots {
		if l.live(i) {
			out = append(out, l.byID[id].msg)
		}
	}
	return out
}

func (GeminiCLI) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatGeminiCLI, DecoderVersion: geminiCLIDecoderVersion, Harness: "gemini-cli"}
	log := &gcLog{byID: map[string]*gcEntry{}}
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		switch {
		case isJSONString(keys["$rewindTo"]):
			var id string
			_ = json.Unmarshal(keys["$rewindTo"], &id)
			log.rewind(id)
		case isJSONObject(keys["$patch"]):
			var p gcPatch
			if err := json.Unmarshal(keys["$patch"], &p); err != nil {
				return structural("line %d $patch: %v", lineNo, err)
			}
			log.patch(p)
		case isJSONString(keys["id"]) && string(keys["id"]) != `""`:
			var m gcMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return structural("line %d message: %v", lineNo, err)
			}
			log.put(&m)
		case isJSONObject(keys["$set"]):
			var set struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(keys["$set"], &set); err != nil {
				return structural("line %d $set: %v", lineNo, err)
			}
			if set.SessionID != "" {
				doc.SessionID = set.SessionID
			}
		case isJSONString(keys["sessionId"]) && isJSONString(keys["projectHash"]):
			_ = json.Unmarshal(keys["sessionId"], &doc.SessionID)
		default:
			var kind string
			_ = json.Unmarshal(keys["type"], &kind)
			switch kind {
			case "user", "gemini", "info", "warning", "error":
				return structural("line %d: %s message has no id", lineNo, kind)
			}
			// A log operation this decoder does not know edits messages in
			// ways it cannot follow; it is counted, not guessed at.
			doc.WarningRecords++
		}
		return nil
	})
	if err != nil {
		return Document{}, err
	}
	if doc.SessionID == "" {
		return Document{}, structural("no session metadata line declares a sessionId")
	}

	out := newEmitter(&doc, emit)
	for _, m := range log.messages() {
		parts, err := gcRecords(m, &doc)
		if err != nil {
			return Document{}, structural("message %q: %v", m.ID, err)
		}
		if len(parts) == 0 {
			continue
		}
		at, err := recordTime(m.Timestamp, fmt.Sprintf("message %q", m.ID))
		if err != nil {
			return Document{}, err
		}
		for i, p := range parts {
			rec := p.rec
			rec.ID = m.ID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", m.ID, p.kind, i)
			}
			rec.Fidelity, rec.Timestamp, rec.Time = FidelityNative, m.Timestamp, at
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

func isJSONString(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '"' }
func isJSONObject(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '{' }

// gcRecord is one record cut from a message.
type gcRecord struct {
	kind string
	rec  Record
}

func gcRecords(m *gcMessage, doc *Document) ([]gcRecord, error) {
	switch m.Type {
	case "user", "gemini", "info", "warning", "error":
	default:
		doc.WarningRecords++
		return []gcRecord{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
			Content: blocksContent([]ContentBlock{unsupportedBlock("Gemini CLI " + m.Type + " message")})}}}, nil
	}
	parts, err := gcParts(m.Content)
	if err != nil {
		return nil, err
	}
	switch m.Type {
	case "user":
		return gcUserRecords(m, parts, doc)
	case "gemini":
		return gcModelRecords(m, parts, doc)
	}
	blocks := gcBlocks(parts, doc)
	if len(blocks) == 0 {
		return nil, nil
	}
	return []gcRecord{{kind: "system", rec: Record{Role: "system", Phase: m.Type, Content: blocksContent(blocks)}}}, nil
}

// gcParts reads message content, which is text, one part, or a list of
// text and parts.
func gcParts(raw json.RawMessage) ([]gcPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("content: %w", err)
		}
	} else {
		items = []json.RawMessage{raw}
	}
	parts := make([]gcPart, 0, len(items))
	for _, item := range items {
		var text string
		if json.Unmarshal(item, &text) == nil {
			parts = append(parts, gcPart{Text: &text})
			continue
		}
		var p gcPart
		if err := json.Unmarshal(item, &p); err != nil {
			return nil, fmt.Errorf("content part: %w", err)
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// gcBlocks maps parts that are not tool calls or results.
func gcBlocks(parts []gcPart, doc *Document) []ContentBlock {
	var out []ContentBlock
	for _, p := range parts {
		switch {
		case p.Text != nil:
			if *p.Text != "" {
				out = append(out, ContentBlock{Type: "text", Text: *p.Text})
			}
		case p.InlineData != nil:
			out = append(out, gcInline(p.InlineData, doc))
		default:
			doc.WarningRecords++
			out = append(out, unsupportedBlock("Gemini CLI content part"))
		}
	}
	return out
}

// gcInline stands in for inline bytes: an image becomes a placeholder, and
// anything else a notice.
func gcInline(d *gcInlineData, doc *Document) ContentBlock {
	if strings.HasPrefix(d.MimeType, "image/") {
		return imageBlock(d.MimeType)
	}
	doc.WarningRecords++
	return unsupportedBlock("Gemini CLI " + d.MimeType + " inline data")
}

// gcUserRecords maps a user message. Its functionResponse parts are tool
// results; the rest is what was typed (displayContent, when the content was
// expanded from it, with the content's images), unless it is context Gemini
// CLI injected.
func gcUserRecords(m *gcMessage, parts []gcPart, doc *Document) ([]gcRecord, error) {
	var out []gcRecord
	var rest []gcPart
	for _, p := range parts {
		if p.FunctionResponse == nil {
			rest = append(rest, p)
			continue
		}
		rec, err := gcToolResult(p.FunctionResponse, doc)
		if err != nil {
			return nil, err
		}
		out = append(out, gcRecord{kind: "tool_result", rec: rec})
	}
	if len(m.DisplayContent) > 0 && string(m.DisplayContent) != "null" {
		display, err := gcParts(m.DisplayContent)
		if err != nil {
			return nil, err
		}
		// A pasted or referenced image is in the content only; it keeps
		// its placeholder beside what was typed.
		for _, p := range rest {
			if p.InlineData != nil && strings.HasPrefix(p.InlineData.MimeType, "image/") {
				display = append(display, p)
			}
		}
		rest = display
	}
	blocks := gcBlocks(rest, doc)
	if len(blocks) == 0 {
		return out, nil
	}
	var text strings.Builder
	for _, b := range blocks {
		text.WriteString(b.Text)
	}
	role, phase := "user", ""
	switch trimmed := strings.TrimSpace(text.String()); {
	case strings.HasPrefix(trimmed, "<session_context>"), strings.HasPrefix(trimmed, "<hook_context>"):
		role, phase = "system", "context"
	case strings.Contains(trimmed, "<state_snapshot>"):
		role, phase = "system", "compaction_summary"
	}
	return append(out, gcRecord{kind: role, rec: Record{Role: role, Phase: phase, Content: blocksContent(blocks)}}), nil
}

func gcToolResult(fr *gcFunctionResponse, doc *Document) (Record, error) {
	rec := Record{Role: "tool", ToolCallID: fr.ID}
	var blocks []ContentBlock
	if len(fr.Response) > 0 && string(fr.Response) != "null" {
		text, isErr := gcResponseText(fr.Response)
		rec.IsError = isErr
		if text != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: text})
		}
	}
	media, err := gcParts(fr.Parts)
	if err != nil {
		return Record{}, err
	}
	rec.Content = blocksContent(append(blocks, gcBlocks(media, doc)...))
	return rec, nil
}

// gcResponseText reads a function response: {"output": ...} and
// {"error": ...} are text, and any other shape is kept as JSON text.
func gcResponseText(raw json.RawMessage) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || len(obj) == 0 {
		return string(scrubInline(raw)), false
	}
	_, isErr := obj["error"]
	isErr = isErr && string(obj["error"]) != "null" && string(obj["error"]) != `""`
	var pieces []string
	for k, v := range obj {
		var s string
		if (k != "output" && k != "error") || json.Unmarshal(v, &s) != nil {
			return string(scrubInline(raw)), isErr
		}
	}
	for _, k := range []string{"output", "error"} {
		var s string
		if json.Unmarshal(obj[k], &s) == nil && s != "" {
			pieces = append(pieces, s)
		}
	}
	return strings.Join(pieces, "\n"), isErr
}

// gcModelRecords maps a model message. Content that holds thought or
// functionCall parts is the whole turn; otherwise the thoughts and toolCalls
// beside the content carry the reasoning and the calls.
func gcModelRecords(m *gcMessage, parts []gcPart, doc *Document) ([]gcRecord, error) {
	whole := slices.ContainsFunc(parts, func(p gcPart) bool { return p.Thought || p.FunctionCall != nil })
	var reasoning []string
	var calls []ToolCall
	var rest []gcPart
	if whole {
		for _, p := range parts {
			switch {
			case p.FunctionCall != nil:
				if p.FunctionCall.ID == "" {
					return nil, fmt.Errorf("functionCall %q has no id", p.FunctionCall.Name)
				}
				calls = append(calls, ToolCall{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Args: string(p.FunctionCall.Args)})
			case p.Thought && p.Text != nil:
				if *p.Text != "" {
					reasoning = append(reasoning, *p.Text)
				}
			default:
				rest = append(rest, p)
			}
		}
	} else {
		for _, t := range m.Thoughts {
			text := t.Description
			if t.Subject != "" {
				text = "**" + t.Subject + "** " + t.Description
			}
			reasoning = append(reasoning, text)
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				return nil, fmt.Errorf("tool call %q has no id", c.Name)
			}
			calls = append(calls, ToolCall{ID: c.ID, Name: c.Name, Args: string(c.Args)})
		}
		rest = parts
	}
	blocks := gcBlocks(rest, doc)
	var out []gcRecord
	if len(reasoning) > 0 {
		out = append(out, gcRecord{kind: "reasoning", rec: Record{Role: "reasoning", Model: m.Model, Content: textContent(strings.Join(reasoning, "\n\n"))}})
	}
	if len(blocks) > 0 || len(calls) > 0 {
		out = append(out, gcRecord{kind: "assistant", rec: Record{Role: "assistant", Model: m.Model, Content: blocksContent(blocks), ToolCalls: calls}})
	}
	return out, nil
}
