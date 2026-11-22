package sessiondecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FormatAntigravity is an Antigravity conversation transcript
// (<appDataDir>/brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl).
// Mapping notes: FORMATS.md.
const FormatAntigravity = "antigravity-jsonl"

const antigravityDecoderVersion = "1"

// Antigravity decodes Antigravity transcript JSONL. Steps carry a 0-based
// step_index ordinal and may be flushed out of step_index order when
// concurrent steps complete, so the decoder sorts by step_index before
// emitting. Ids are positional (step:<step_index>, with split suffixes when a
// planner response carries both thinking and assistant output).
type Antigravity struct{}

func (Antigravity) Format() string  { return FormatAntigravity }
func (Antigravity) Version() string { return antigravityDecoderVersion }

type agStep struct {
	StepIndex *int            `json:"step_index"`
	Source    string          `json:"source"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	Content   json.RawMessage `json:"content"`
	Thinking  string          `json:"thinking"`
	Model     string          `json:"model"`
	ToolCalls []agToolCall    `json:"tool_calls"`
	Media     []agMedia       `json:"media"`
	Error     string          `json:"error"`
	ExitCode  *int            `json:"exit_code"`
}

type agToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type agMedia struct {
	MimeType string `json:"mime_type"`
	URI      string `json:"uri"`
}

var (
	agUserRequestRe    = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)
	agSettingsChangeRe = regexp.MustCompile(`(?s)<USER_SETTINGS_CHANGE>(.*?)</USER_SETTINGS_CHANGE>`)
	agModelSelectionRe = regexp.MustCompile("(?m)Model Selection`\\s+from\\s+.+?\\s+to\\s+(.+?)\\.(?:\\s|$)")
)

func (Antigravity) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatAntigravity, DecoderVersion: antigravityDecoderVersion, Harness: "antigravity"}
	var steps []agStep
	seenIdx := map[int]bool{}
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var step agStep
		if err := json.Unmarshal(raw, &step); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if step.StepIndex == nil {
			return structural("line %d has no step_index", lineNo)
		}
		if step.Type == "" {
			return structural("line %d has no type", lineNo)
		}
		idx := *step.StepIndex
		if seenIdx[idx] {
			return structural("duplicate step_index %d", idx)
		}
		seenIdx[idx] = true
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		return Document{}, err
	}

	sort.Slice(steps, func(i, j int) bool {
		return *steps[i].StepIndex < *steps[j].StepIndex
	})

	out := newEmitter(&doc, emit)
	var state agState
	for _, step := range steps {
		idx := *step.StepIndex
		parts, err := agParts(step, &state, &doc)
		if err != nil {
			return Document{}, err
		}
		if len(parts) == 0 {
			continue
		}
		at, err := recordTime(step.CreatedAt, fmt.Sprintf("step %d", idx))
		if err != nil {
			return Document{}, err
		}
		baseID := fmt.Sprintf("step:%d", idx)
		for _, p := range parts {
			rec := p.rec
			rec.ID = baseID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", baseID, p.kind, p.pos)
			}
			rec.Fidelity = FidelityPositional
			rec.Timestamp = step.CreatedAt
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

type agState struct {
	pendingCalls []string
	// settingsModel holds the latest model named in a <USER_SETTINGS_CHANGE>
	// block, used as a fallback when PLANNER_RESPONSE omits the model field.
	settingsModel string
}

type agPart struct {
	kind string
	pos  int
	rec  Record
}

func agParts(step agStep, state *agState, doc *Document) ([]agPart, error) {
	idx := *step.StepIndex
	switch step.Type {
	case "USER_INPUT":
		state.pendingCalls = nil
		blocks, text := agStepContent(step.Content, doc)
		if text != "" {
			if m := agSettingsModel(text); m != "" {
				state.settingsModel = m
			}
			unwrapped := agUnwrapUserRequest(text)
			if unwrapped == "" {
				blocks = nil
			} else {
				blocks = []ContentBlock{{Type: "text", Text: unwrapped}}
			}
		}
		blocks = agAppendMedia(blocks, step.Media, doc)
		return []agPart{{kind: "user", rec: Record{Role: "user", Content: blocksContent(blocks)}}}, nil

	case "PLANNER_RESPONSE":
		state.pendingCalls = nil
		model := step.Model
		if model == "" {
			model = state.settingsModel
		}
		var calls []ToolCall
		for i, tc := range step.ToolCalls {
			if tc.Name == "" {
				return nil, structural("step %d has a tool call without a name", idx)
			}
			callID := fmt.Sprintf("step:%d:call:%d", idx, i)
			args := "{}"
			if trimmed := bytes.TrimSpace(tc.Args); len(trimmed) > 0 && string(trimmed) != "null" {
				args = string(scrubInline(trimmed))
			}
			calls = append(calls, ToolCall{ID: callID, Name: tc.Name, Args: args})
			state.pendingCalls = append(state.pendingCalls, callID)
		}
		blocks, _ := agStepContent(step.Content, doc)
		var parts []agPart
		if step.Thinking != "" {
			parts = append(parts, agPart{
				kind: "reasoning", pos: 0,
				rec: Record{Role: "reasoning", Model: model, Content: textContent(step.Thinking)},
			})
		}
		if len(blocks) > 0 || len(calls) > 0 {
			rec := Record{Role: "assistant", Model: model, Content: blocksContent(blocks), ToolCalls: calls}
			parts = append(parts, agPart{kind: "assistant", pos: len(parts), rec: rec})
		}
		return parts, nil

	case "CONVERSATION_HISTORY":
		return nil, nil

	case "CHECKPOINT":
		blocks, _ := agStepContent(step.Content, doc)
		if len(blocks) == 0 {
			blocks = []ContentBlock{{Type: "text", Text: "Conversation compacted"}}
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Phase: "compaction", Content: blocksContent(blocks),
		}}}, nil

	case "SYSTEM_MESSAGE":
		blocks, _ := agContentBlocks(step.Content, step.Media, doc)
		if len(blocks) == 0 {
			return nil, nil
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Content: blocksContent(blocks),
		}}}, nil

	case "ERROR_MESSAGE":
		blocks, text := agStepContent(step.Content, doc)
		if step.Error != "" {
			switch {
			case len(blocks) == 0:
				blocks = []ContentBlock{{Type: "text", Text: step.Error}}
			case text != "" && !strings.Contains(text, step.Error):
				blocks[0].Text = text + "\n" + step.Error
			}
		}
		if len(blocks) == 0 {
			return nil, nil
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Phase: "error", IsError: true, Content: blocksContent(blocks),
		}}}, nil
	}

	// Antigravity names tool result step types after the tool (RUN_COMMAND,
	// VIEW_FILE, GREP_SEARCH, LIST_DIRECTORY, GENERIC, MCP_TOOL, ...). Pair
	// any other step type FIFO with pending calls; when no call is pending,
	// emit a warning notice rather than failing so a new non-tool step type
	// never drops the session.
	if len(state.pendingCalls) == 0 {
		doc.WarningRecords++
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Phase: "normalization",
			Content: blocksContent([]ContentBlock{unsupportedBlock("Antigravity " + step.Type)}),
		}}}, nil
	}
	// Backgrounded commands stay status "RUNNING" mid-session with valid
	// tool output, so non-DONE steps are emitted rather than skipped.
	callID := state.pendingCalls[0]
	state.pendingCalls = state.pendingCalls[1:]
	blocks, text := agContentBlocks(step.Content, step.Media, doc)
	if step.Error != "" {
		switch {
		case len(blocks) == 0:
			blocks = append(blocks, ContentBlock{Type: "text", Text: step.Error})
		case text != "" && !strings.Contains(text, step.Error):
			blocks[0].Text = text + "\n" + step.Error
		case text == "":
			blocks = append([]ContentBlock{{Type: "text", Text: step.Error}}, blocks...)
		}
	}
	isErr := step.Status == "ERROR" || step.Error != "" || (step.ExitCode != nil && *step.ExitCode != 0)
	return []agPart{{kind: "tool", rec: Record{
		Role: "tool", ToolCallID: callID, Content: blocksContent(blocks), IsError: isErr,
	}}}, nil
}

// agUnwrapUserRequest extracts the user's prompt from <USER_REQUEST> tags,
// stripping <ADDITIONAL_METADATA> and <USER_SETTINGS_CHANGE> wrappers. Plain
// text without <USER_REQUEST> tags passes through unchanged.
func agUnwrapUserRequest(text string) string {
	matches := agUserRequestRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text
	}
	var parts []string
	for _, m := range matches {
		if body := strings.TrimSpace(m[1]); body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n\n")
}

// agSettingsModel extracts the last Model Selection value from any
// <USER_SETTINGS_CHANGE> block in a user step.
func agSettingsModel(text string) string {
	model := ""
	for _, block := range agSettingsChangeRe.FindAllStringSubmatch(text, -1) {
		for _, m := range agModelSelectionRe.FindAllStringSubmatch(block[1], -1) {
			if v := strings.TrimSpace(m[1]); v != "" {
				model = v
			}
		}
	}
	return model
}

// agStepContent decodes a step's content field. Real transcripts always write
// a JSON string; any non-string value becomes a warning notice rather than
// failing the document.
func agStepContent(raw json.RawMessage, doc *Document) ([]ContentBlock, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		if s == "" {
			return nil, ""
		}
		return []ContentBlock{{Type: "text", Text: s}}, s
	}
	doc.WarningRecords++
	return []ContentBlock{unsupportedBlock("Antigravity content")}, ""
}

func agAppendMedia(blocks []ContentBlock, media []agMedia, doc *Document) []ContentBlock {
	for _, m := range media {
		switch {
		case m.MimeType == "" || strings.HasPrefix(m.MimeType, "image/"):
			blocks = append(blocks, imageBlock(m.MimeType))
		default:
			doc.WarningRecords++
			blocks = append(blocks, unsupportedBlock("Antigravity "+m.MimeType))
		}
	}
	return blocks
}

func agContentBlocks(raw json.RawMessage, media []agMedia, doc *Document) ([]ContentBlock, string) {
	blocks, text := agStepContent(raw, doc)
	return agAppendMedia(blocks, media, doc), text
}
