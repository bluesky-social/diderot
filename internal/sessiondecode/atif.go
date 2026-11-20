package sessiondecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FormatATIF is Harbor's Agent Trajectory Interchange Format: one JSON
// document per run (harbor rfcs/0001-trajectory-format.md). Mapping notes:
// FORMATS.md.
const FormatATIF = "atif-json"

const atifDecoderVersion = "1"

// ATIFStableIDKey is the step.extra key a producer may set to a stable,
// source-native step id. Without it, ids are positional (the step_id
// ordinal).
const ATIFStableIDKey = "stable_step_id"

// ATIF decodes ATIF trajectories (schema versions ATIF-v1.0 through v1.8).
type ATIF struct{}

func (ATIF) Format() string  { return FormatATIF }
func (ATIF) Version() string { return atifDecoderVersion }

type atifTrajectory struct {
	SchemaVersion string `json:"schema_version"`
	Agent         struct {
		Name      string `json:"name"`
		ModelName string `json:"model_name"`
	} `json:"agent"`
	Steps []atifStep `json:"steps"`
}

type atifStep struct {
	StepID           *int             `json:"step_id"`
	Timestamp        string           `json:"timestamp"`
	Source           string           `json:"source"`
	ModelName        string           `json:"model_name"`
	Message          json.RawMessage  `json:"message"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []atifToolCall   `json:"tool_calls"`
	Observation      *atifObservation `json:"observation"`
	Extra            map[string]any   `json:"extra"`
}

type atifToolCall struct {
	ToolCallID   string          `json:"tool_call_id"`
	FunctionName string          `json:"function_name"`
	Arguments    json.RawMessage `json:"arguments"`
}

type atifObservation struct {
	Results []struct {
		SourceCallID          string          `json:"source_call_id"`
		Content               json.RawMessage `json:"content"`
		SubagentTrajectoryRef json.RawMessage `json:"subagent_trajectory_ref"`
	} `json:"results"`
}

type atifPart struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		MediaType string `json:"media_type"`
	} `json:"source"`
}

func (ATIF) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatATIF, DecoderVersion: atifDecoderVersion}
	var traj atifTrajectory
	if !json.Valid(data) {
		return Document{}, structural("trajectory is not one JSON document")
	}
	if err := json.Unmarshal(data, &traj); err != nil {
		return Document{}, structural("trajectory: %v", err)
	}
	if !strings.HasPrefix(traj.SchemaVersion, "ATIF-v1.") {
		return Document{}, structural("schema_version %q is not ATIF v1", traj.SchemaVersion)
	}
	doc.SourceVersion = traj.SchemaVersion
	doc.Harness = traj.Agent.Name
	if doc.Harness == "" {
		doc.Harness = "atif"
	}
	fill := &timeFiller{out: newEmitter(&doc, emit)}
	for i, step := range traj.Steps {
		if step.StepID == nil || *step.StepID != i+1 {
			return Document{}, structural("step %d: step_id must be the ordinal %d", i, i+1)
		}
		recs, err := atifRecords(step, traj.Agent.ModelName, &doc)
		if err != nil {
			return Document{}, structural("step %d: %v", i+1, err)
		}
		var at time.Time
		hasTime := step.Timestamp != ""
		if hasTime {
			if at, err = time.Parse(time.RFC3339, step.Timestamp); err != nil {
				return Document{}, structural("step %d timestamp %q is not ISO 8601", i+1, step.Timestamp)
			}
		}
		if len(recs) == 0 && hasTime {
			if err := fill.mark(at); err != nil {
				return Document{}, err
			}
		}
		for _, r := range recs {
			if err := fill.add(r, at, hasTime); err != nil {
				return Document{}, err
			}
		}
	}
	if err := fill.finish(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

type atifSplit struct {
	kind string
	rec  Record
}

func atifRecords(step atifStep, agentModel string, doc *Document) ([]Record, error) {
	model := step.ModelName
	if model == "" {
		model = agentModel
	}
	var parts []atifSplit
	content, err := atifContent(step.Message, doc)
	if err != nil {
		return nil, err
	}
	switch step.Source {
	case "user":
		parts = append(parts, atifSplit{"user", Record{Role: "user", Content: content}})
	case "system":
		rec := Record{Role: "system", Content: content}
		if cm, ok := step.Extra["context_management"].(map[string]any); ok {
			if kind, _ := cm["type"].(string); kind != "" {
				rec.Phase = kind
			}
		}
		parts = append(parts, atifSplit{"system", rec})
	case "agent":
		if step.ReasoningContent != "" {
			parts = append(parts, atifSplit{"reasoning", Record{Role: "reasoning", Model: model, Content: textContent(step.ReasoningContent)}})
		}
		rec := Record{Role: "assistant", Model: model, Content: content}
		for _, c := range step.ToolCalls {
			if c.ToolCallID == "" {
				return nil, fmt.Errorf("tool call without tool_call_id")
			}
			rec.ToolCalls = append(rec.ToolCalls, ToolCall{ID: c.ToolCallID, Name: c.FunctionName, Args: compactJSON(c.Arguments)})
		}
		if len(rec.Content) > 0 || len(rec.ToolCalls) > 0 {
			parts = append(parts, atifSplit{"assistant", rec})
		}
	default:
		return nil, fmt.Errorf("source %q is not system, user, or agent", step.Source)
	}
	if step.Observation != nil {
		for _, res := range step.Observation.Results {
			out, err := atifContent(res.Content, doc)
			if err != nil {
				return nil, err
			}
			if len(res.SubagentTrajectoryRef) > 0 && string(res.SubagentTrajectoryRef) != "null" {
				out = appendText(out, "[subagent trajectory "+compactJSON(res.SubagentTrajectoryRef)+"]")
			}
			if res.SourceCallID == "" {
				parts = append(parts, atifSplit{"observation", Record{Role: "system", Phase: "observation", Content: out}})
				continue
			}
			parts = append(parts, atifSplit{"observation", Record{Role: "tool", ToolCallID: res.SourceCallID, Content: out}})
		}
	}

	base, fidelity := fmt.Sprintf("step:%d", *step.StepID), FidelityPositional
	if id, ok := step.Extra[ATIFStableIDKey].(string); ok && id != "" {
		base, fidelity = id, FidelityNative
	}
	recs := make([]Record, len(parts))
	for i, p := range parts {
		p.rec.ID, p.rec.Fidelity = base, fidelity
		if len(parts) > 1 {
			p.rec.ID = fmt.Sprintf("%s:%s:%d", base, p.kind, i)
		}
		recs[i] = p.rec
	}
	return recs, nil
}

// appendText adds a text block after existing record content.
func appendText(content json.RawMessage, text string) json.RawMessage {
	if len(content) == 0 {
		return textContent(text)
	}
	var blocks []ContentBlock
	var s string
	if json.Unmarshal(content, &s) == nil {
		blocks = []ContentBlock{{Type: "text", Text: s}}
	} else if json.Unmarshal(content, &blocks) != nil {
		return content
	}
	b, _ := json.Marshal(append(blocks, ContentBlock{Type: "text", Text: text}))
	return b
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func atifContent(raw json.RawMessage, doc *Document) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return textContent(s), nil
	}
	var parts []atifPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	blocks := make([]ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, ContentBlock{Type: "text", Text: p.Text})
		case "image":
			mediaType := ""
			if p.Source != nil {
				mediaType = p.Source.MediaType
			}
			blocks = append(blocks, imageBlock(mediaType))
		default:
			doc.WarningRecords++
			blocks = append(blocks, unsupportedBlock("ATIF "+p.Type))
		}
	}
	return blocksContent(blocks), nil
}
