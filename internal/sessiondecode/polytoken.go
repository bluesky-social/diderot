package sessiondecode

import (
	"encoding/json"
	"fmt"
)

// FormatPolytoken is a Polytoken session log
// (~/.local/share/polytoken/sessions-v1/<session>/log.jsonl). Mapping notes:
// FORMATS.md.
const FormatPolytoken = "polytoken-jsonl"

const polytokenDecoderVersion = "1"

// Polytoken decodes Polytoken's session log. A current line wraps one
// session event in an envelope whose lineage and index name it (native ids);
// a line written before that envelope existed is the bare event, named by its
// line number (positional).
type Polytoken struct{}

func (Polytoken) Format() string  { return FormatPolytoken }
func (Polytoken) Version() string { return polytokenDecoderVersion }

type ptLine struct {
	Index       *int64 `json:"index"`
	Lineage     string `json:"lineage"`
	CommittedAt string `json:"committed_at"`
	Attribution *struct {
		Model string `json:"model"`
	} `json:"attribution"`
	Message json.RawMessage `json:"message"`
}

// ptEvent is one session event: its kind and time, and the fields of the
// kinds the decoder maps. Each kind is read only for its own fields, so an
// event of an unknown kind never fails to parse.
type ptEvent struct {
	Type      string
	EmittedAt string
	PromptID  string
	Kind      string
	Content   string
	Blocks    []json.RawMessage
	Results   []ptResult
	Text      string
	MediaType string
	ToModel   string
}

type ptResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (Polytoken) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatPolytoken, DecoderVersion: polytokenDecoderVersion, Harness: "polytoken"}
	out := newEmitter(&doc, emit)
	model := ""
	// Images attached to a prompt are logged just before its user message,
	// under the same prompt id. One that never meets its message (the copy
	// re-attached after a compaction) is not shown again.
	images := map[string][]ContentBlock{}
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		// A bare event always has a type and an envelope never does, so no
		// field of a bare event is read as envelope metadata.
		var ln ptLine
		base, fidelity, body := fmt.Sprintf("line:%d", lineNo), FidelityPositional, json.RawMessage(raw)
		if head.Type == "" {
			if err := json.Unmarshal(raw, &ln); err != nil {
				return structural("line %d: %v", lineNo, err)
			}
			if len(ln.Message) == 0 || ln.Index == nil || ln.Lineage == "" {
				return structural("line %d is neither a session event nor an envelope with a lineage and index", lineNo)
			}
			base, fidelity, body = fmt.Sprintf("%s:%d", ln.Lineage, *ln.Index), FidelityNative, ln.Message
		}
		ev, err := ptReadEvent(body)
		if err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if ev.ToModel != "" {
			model = ev.ToModel
		}
		recModel := model
		if ln.Attribution != nil && ln.Attribution.Model != "" {
			recModel = ln.Attribution.Model
		}
		if ev.Type == "image_reference" {
			images[ev.PromptID] = append(images[ev.PromptID], imageBlock(ev.MediaType))
			return nil
		}
		parts, err := ptParts(ev, recModel, images, &doc)
		if err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if len(parts) == 0 {
			return nil
		}
		stamp := ev.EmittedAt
		if stamp == "" {
			stamp = ln.CommittedAt
		}
		at, err := recordTime(stamp, fmt.Sprintf("line %d", lineNo))
		if err != nil {
			return err
		}
		for _, p := range parts {
			rec := p.rec
			rec.ID = base
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", base, p.kind, p.pos)
			}
			rec.Fidelity, rec.Timestamp, rec.Time = fidelity, stamp, at
			if err := out.add(rec); err != nil {
				return err
			}
		}
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

// ptReadEvent reads an event's kind and time, then the fields its kind has.
func ptReadEvent(raw json.RawMessage) (ptEvent, error) {
	var head struct {
		Type      string `json:"type"`
		EmittedAt string `json:"emitted_at"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ptEvent{}, err
	}
	if head.Type == "" {
		return ptEvent{}, fmt.Errorf("not a session event (no type)")
	}
	ev := ptEvent{Type: head.Type, EmittedAt: head.EmittedAt}
	var err error
	switch ev.Type {
	case "user":
		var v struct {
			PromptID string `json:"prompt_id"`
			Kind     string `json:"kind"`
			Content  string `json:"content"`
		}
		err = json.Unmarshal(raw, &v)
		ev.PromptID, ev.Kind, ev.Content = v.PromptID, v.Kind, v.Content
	case "assistant":
		var v struct {
			Blocks []json.RawMessage `json:"blocks"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Blocks = v.Blocks
	case "tool_result":
		var v struct {
			Results []ptResult `json:"results"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Results = v.Results
	case "system_reminder":
		// The reminder carries its own time; the event has none.
		var v struct {
			Reminder struct {
				Body      string `json:"body"`
				EmittedAt string `json:"emitted_at"`
			} `json:"reminder"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Text, ev.EmittedAt = v.Reminder.Body, v.Reminder.EmittedAt
	case "tool_exposure_reminder":
		var v struct {
			Text string `json:"text"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Text = v.Text
	case "compaction_fencepost":
		// Only the summary: the continuation prompt repeats the conversation.
		var v struct {
			Summary string `json:"summary"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Text = v.Summary
	case "subagent_complete":
		var v struct {
			Summary string `json:"result_summary"`
		}
		err = json.Unmarshal(raw, &v)
		ev.Text = v.Summary
	case "image_reference":
		// The image's bytes and local path are never read.
		var v struct {
			PromptID  string `json:"prompt_id"`
			MediaType string `json:"media_type"`
		}
		err = json.Unmarshal(raw, &v)
		ev.PromptID, ev.MediaType = v.PromptID, v.MediaType
	case "model_switch_v2":
		var v struct {
			Selection struct {
				ToModel string `json:"to_model"`
			} `json:"selection"`
		}
		err = json.Unmarshal(raw, &v)
		ev.ToModel = v.Selection.ToModel
	}
	if err != nil {
		return ptEvent{}, fmt.Errorf("%s event: %w", ev.Type, err)
	}
	return ev, nil
}

// ptParts maps one session event onto records; none for bookkeeping that is
// not conversation.
func ptParts(ev ptEvent, model string, images map[string][]ContentBlock, doc *Document) ([]ccPart, error) {
	switch ev.Type {
	case "user":
		var blocks []ContentBlock
		if ev.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: ev.Content})
		}
		blocks = append(blocks, images[ev.PromptID]...)
		delete(images, ev.PromptID)
		if ev.Kind != "human" {
			// Reiterated input (a goal continuation, a parent's message to
			// a subagent) is the harness speaking, not the person.
			return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: ev.Kind, Content: blocksContent(blocks)}}}, nil
		}
		return []ccPart{{kind: "user", rec: Record{Role: "user", Content: blocksContent(blocks)}}}, nil
	case "assistant":
		return ptAssistantParts(ev.Blocks, model, doc)
	case "tool_result":
		parts := make([]ccPart, 0, len(ev.Results))
		for i, r := range ev.Results {
			if r.ToolUseID == "" {
				return nil, fmt.Errorf("tool result %d has no tool_use_id", i)
			}
			parts = append(parts, ccPart{kind: "tool_result", pos: i, rec: Record{
				Role: "tool", ToolCallID: r.ToolUseID, Content: ptResultContent(r.Content, doc), IsError: r.IsError,
			}})
		}
		return parts, nil
	case "system_reminder", "tool_exposure_reminder":
		return ptSystem("system_reminder", ev.Text), nil
	case "compaction_fencepost":
		text := ev.Text
		if text == "" {
			text = "Conversation compacted"
		}
		return ptSystem("compaction", text), nil
	case "context_cleared":
		return ptSystem("context_cleared", "Context cleared"), nil
	case "subagent_complete":
		return ptSystem("subagent_complete", ev.Text), nil
	case "state_update", "session_lifecycle", "model_switch_v2", "facet_switch",
		"subagent_spawn_v1", "subagent_spawn_v2", "notification", "classifier_decision_v1":
		// Bookkeeping: state changes, the model and facet in force, the
		// subagent launch its tool call already shows, permission decisions,
		// and notifications a reminder delivers.
		return nil, nil
	}
	doc.WarningRecords++
	return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("Polytoken " + ev.Type + " event")})}}}, nil
}

func ptSystem(phase, text string) []ccPart {
	if text == "" {
		return nil
	}
	return []ccPart{{kind: "system", rec: Record{Role: "system", Phase: phase, Content: textContent(text)}}}
}

// ptAssistantParts splits an assistant event at each reasoning block, as for
// Claude Code. Only displayable text is read: a thinking block's signature and
// an opaque reasoning block's data are provider continuation state.
func ptAssistantParts(raw []json.RawMessage, model string, doc *Document) ([]ccPart, error) {
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
	for i, rb := range raw {
		var b struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rb, &b); err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		switch b.Type {
		case "thinking", "open_ai_reasoning_opaque":
			flush()
			var r struct {
				Text    string `json:"text"`
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(rb, &r); err != nil {
				return nil, fmt.Errorf("block %d: %w", i, err)
			}
			text := r.Text
			if b.Type == "open_ai_reasoning_opaque" {
				text = r.Summary
			}
			if text != "" {
				parts = append(parts, ccPart{kind: "reasoning", pos: i, rec: Record{Role: "reasoning", Model: model, Content: textContent(text)}})
			}
			continue
		}
		if open == nil {
			open = &ccPart{kind: "assistant", pos: i, rec: Record{Role: "assistant", Model: model}}
		}
		switch b.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(rb, &t); err != nil {
				return nil, fmt.Errorf("block %d: %w", i, err)
			}
			openBlocks = append(openBlocks, ContentBlock{Type: "text", Text: t.Text})
		case "tool_use":
			var u struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(rb, &u); err != nil {
				return nil, fmt.Errorf("block %d: %w", i, err)
			}
			if u.ID == "" {
				return nil, fmt.Errorf("tool call %d has no id", i)
			}
			open.rec.ToolCalls = append(open.rec.ToolCalls, ToolCall{ID: u.ID, Name: u.Name, Args: string(u.Input)})
		default:
			doc.WarningRecords++
			openBlocks = append(openBlocks, unsupportedBlock("Polytoken "+b.Type))
		}
	}
	flush()
	return parts, nil
}

// ptResultContent maps a tool result's content: an object carrying text, an
// image (whose bytes are dropped), or both.
func ptResultContent(raw json.RawMessage, doc *Document) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var c struct {
		Text  *string `json:"text"`
		Image *struct {
			MediaType string `json:"media_type"`
		} `json:"image"`
	}
	if json.Unmarshal(raw, &c) != nil || (c.Text == nil && c.Image == nil) {
		doc.WarningRecords++
		return textContent(string(scrubInline(raw)))
	}
	var blocks []ContentBlock
	if c.Text != nil {
		blocks = append(blocks, ContentBlock{Type: "text", Text: *c.Text})
	}
	if c.Image != nil {
		blocks = append(blocks, imageBlock(c.Image.MediaType))
	}
	return blocksContent(blocks)
}
