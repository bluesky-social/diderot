package sessiondecode

import (
	"encoding/json"
	"fmt"
	"time"
)

// atifEncodeSchema is the ATIF version EncodeATIF writes.
const atifEncodeSchema = "ATIF-v1.8"

type atifOutTrajectory struct {
	SchemaVersion string        `json:"schema_version"`
	SessionID     string        `json:"session_id"`
	Agent         atifOutAgent  `json:"agent"`
	Steps         []atifOutStep `json:"steps"`
}

type atifOutAgent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type atifOutStep struct {
	StepID           int                 `json:"step_id"`
	Timestamp        string              `json:"timestamp"`
	Source           string              `json:"source"`
	ModelName        string              `json:"model_name,omitempty"`
	Message          json.RawMessage     `json:"message"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []atifToolCall      `json:"tool_calls,omitempty"`
	Observation      *atifOutObservation `json:"observation,omitempty"`
	Extra            map[string]any      `json:"extra,omitempty"`
}

type atifOutObservation struct {
	Results []atifOutResult `json:"results"`
}

type atifOutResult struct {
	SourceCallID string          `json:"source_call_id"`
	Content      json.RawMessage `json:"content"`
}

type atifOutPart struct {
	Type   string         `json:"type"`
	Text   *string        `json:"text,omitempty"`
	Source *atifOutSource `json:"source,omitempty"`
}

type atifOutSource struct {
	MediaType string `json:"media_type"`
	Path      string `json:"path"`
}

// EncodeATIF writes the records another decoder produced as one ATIF
// trajectory, so a program that reads a harness's own session file can send
// it as atif-json. Every record becomes one step, so ATIF.Decode maps the
// trajectory back onto the same records in the same order: a native record
// id travels as the step's stable_step_id, and a positional one becomes the
// step's ordinal. The same records always encode to the same bytes.
//
// ATIF has no place for two things a record can carry, and they are left
// out: a tool result's error flag, and the phase of a record that is not a
// system record. A tool call whose arguments are not a JSON object (a patch
// passed as text) is carried as {"input": "<the text>"}, since ATIF
// arguments are an object. An image is a part with its media type and an
// empty path: records never hold image bytes.
func EncodeATIF(sessionID string, doc Document, records []Record) ([]byte, error) {
	traj := atifOutTrajectory{
		SchemaVersion: atifEncodeSchema, SessionID: sessionID,
		Agent: atifOutAgent{Name: doc.Harness, Version: doc.SourceVersion},
		Steps: make([]atifOutStep, 0, len(records)),
	}
	if traj.Agent.Version == "" {
		traj.Agent.Version = "unknown"
	}
	for _, rec := range records {
		step := atifOutStep{
			StepID: len(traj.Steps) + 1, Timestamp: rec.Time.UTC().Format(time.RFC3339Nano),
			Message: json.RawMessage(`""`),
		}
		if rec.Time.IsZero() {
			return nil, fmt.Errorf("record %q has no time", rec.ID)
		}
		switch rec.Role {
		case "user", "system":
			message, err := atifMessage(rec.Content)
			if err != nil {
				return nil, fmt.Errorf("record %q: %w", rec.ID, err)
			}
			step.Source, step.Message = rec.Role, message
			if rec.Role == "system" && rec.Phase != "" {
				step.Extra = map[string]any{"context_management": map[string]any{"type": rec.Phase}}
			}
		case "reasoning":
			step.Source, step.ModelName, step.ReasoningContent = "agent", rec.Model, rawString(rec.Content)
			if step.ReasoningContent == "" {
				continue
			}
		case "assistant":
			message, err := atifMessage(rec.Content)
			if err != nil {
				return nil, fmt.Errorf("record %q: %w", rec.ID, err)
			}
			step.Source, step.ModelName, step.Message = "agent", rec.Model, message
			for _, call := range rec.ToolCalls {
				step.ToolCalls = append(step.ToolCalls, atifToolCall{
					ToolCallID: call.ID, FunctionName: call.Name, Arguments: atifArguments(call.Args),
				})
			}
			if string(message) == `""` && len(step.ToolCalls) == 0 {
				continue
			}
		case "tool":
			if rec.ToolCallID == "" {
				return nil, fmt.Errorf("tool record %q names no call", rec.ID)
			}
			content, err := atifMessage(rec.Content)
			if err != nil {
				return nil, fmt.Errorf("record %q: %w", rec.ID, err)
			}
			step.Source = "agent"
			step.Observation = &atifOutObservation{Results: []atifOutResult{{SourceCallID: rec.ToolCallID, Content: content}}}
		default:
			return nil, fmt.Errorf("record %q has role %q, which ATIF cannot carry", rec.ID, rec.Role)
		}
		if rec.Fidelity == FidelityNative {
			if step.Extra == nil {
				step.Extra = map[string]any{}
			}
			step.Extra[ATIFStableIDKey] = rec.ID
		}
		traj.Steps = append(traj.Steps, step)
	}
	if len(traj.Steps) == 0 {
		return nil, structural("document has no records")
	}
	return json.Marshal(traj)
}

// atifMessage turns record content (nothing, a string, or content blocks)
// into an ATIF message: a string, or an array of content parts.
func atifMessage(content json.RawMessage) (json.RawMessage, error) {
	if len(content) == 0 || string(content) == "null" {
		return json.RawMessage(`""`), nil
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return content, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("content is neither text nor content blocks: %w", err)
	}
	parts := make([]atifOutPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, atifOutPart{Type: "text", Text: &b.Text})
		case "image":
			parts = append(parts, atifOutPart{Type: "image", Source: &atifOutSource{MediaType: b.MediaType}})
		default:
			return nil, fmt.Errorf("content block type %q, which ATIF cannot carry", b.Type)
		}
	}
	return json.Marshal(parts)
}

// atifArguments is a tool call's arguments as the JSON object ATIF requires.
func atifArguments(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &object) == nil && object != nil {
		return json.RawMessage(args)
	}
	wrapped, _ := json.Marshal(map[string]string{"input": args})
	return wrapped
}
