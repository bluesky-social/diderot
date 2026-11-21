package sessiondecode

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	StepIndex *int          `json:"step_index"`
	Source    string        `json:"source"`
	Type      string        `json:"type"`
	Status    string        `json:"status"`
	CreatedAt string        `json:"created_at"`
	Content   string        `json:"content"`
	Thinking  string        `json:"thinking"`
	Model     string        `json:"model"`
	ToolCalls []agToolCall  `json:"tool_calls"`
	Media     []agMedia     `json:"media"`
	Error     string        `json:"error"`
	ExitCode  *int          `json:"exit_code"`
}

type agToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type agMedia struct {
	MimeType string `json:"mime_type"`
	URI      string `json:"uri"`
}

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
	var pendingCalls []string
	for _, step := range steps {
		idx := *step.StepIndex
		parts, err := agParts(step, &pendingCalls, &doc)
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

type agPart struct {
	kind string
	pos  int
	rec  Record
}

func agParts(step agStep, pendingCalls *[]string, doc *Document) ([]agPart, error) {
	idx := *step.StepIndex
	switch step.Type {
	case "USER_INPUT":
		*pendingCalls = nil
		blocks := agContentBlocks(step.Content, step.Media, doc)
		return []agPart{{kind: "user", rec: Record{Role: "user", Content: blocksContent(blocks)}}}, nil

	case "PLANNER_RESPONSE":
		*pendingCalls = nil
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
			*pendingCalls = append(*pendingCalls, callID)
		}
		var parts []agPart
		if step.Thinking != "" {
			parts = append(parts, agPart{
				kind: "reasoning", pos: 0,
				rec: Record{Role: "reasoning", Model: step.Model, Content: textContent(step.Thinking)},
			})
		}
		if step.Content != "" || len(calls) > 0 {
			rec := Record{Role: "assistant", Model: step.Model, ToolCalls: calls}
			if step.Content != "" {
				rec.Content = textContent(step.Content)
			}
			parts = append(parts, agPart{kind: "assistant", pos: len(parts), rec: rec})
		}
		return parts, nil

	case "RUN_COMMAND", "AGENCY_TOOL_CALL", "GENERIC", "MCP_TOOL", "SEARCH_WEB", "READ_URL_CONTENT", "INVOKE_SUBAGENT":
		callID := ""
		if len(*pendingCalls) > 0 {
			callID = (*pendingCalls)[0]
			*pendingCalls = (*pendingCalls)[1:]
		}
		text := step.Content
		if text == "" {
			text = step.Error
		}
		blocks := agContentBlocks(text, step.Media, doc)
		isErr := step.Status == "ERROR" || step.Error != "" || (step.ExitCode != nil && *step.ExitCode != 0)
		return []agPart{{kind: "tool", rec: Record{
			Role: "tool", ToolCallID: callID, Content: blocksContent(blocks), IsError: isErr,
		}}}, nil

	case "CHECKPOINT":
		text := step.Content
		if text == "" {
			text = "Conversation compacted"
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Phase: "compaction", Content: textContent(text),
		}}}, nil

	case "SYSTEM_MESSAGE":
		blocks := agContentBlocks(step.Content, step.Media, doc)
		if len(blocks) == 0 {
			return nil, nil
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Content: blocksContent(blocks),
		}}}, nil

	case "ERROR_MESSAGE":
		text := step.Content
		if text == "" {
			text = step.Error
		}
		if text == "" {
			return nil, nil
		}
		return []agPart{{kind: "system", rec: Record{
			Role: "system", Phase: "error", IsError: true, Content: textContent(text),
		}}}, nil
	}

	doc.WarningRecords++
	return []agPart{{kind: "system", rec: Record{
		Role: "system", Phase: "normalization",
		Content: blocksContent([]ContentBlock{unsupportedBlock("Antigravity " + step.Type)}),
	}}}, nil
}

func agContentBlocks(text string, media []agMedia, doc *Document) []ContentBlock {
	var blocks []ContentBlock
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text})
	}
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
