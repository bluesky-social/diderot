package sessiondecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatCopilotCLI is a GitHub Copilot CLI session event log
// (~/.copilot/session-state/<session>/events.jsonl). Mapping notes:
// FORMATS.md.
const FormatCopilotCLI = "copilot-cli-events-jsonl"

const copilotCLIDecoderVersion = "1"

// CopilotCLI decodes Copilot CLI session events. Every event carries a native
// UUID id, and a rewind rewrites the log without the rewound events but keeps
// the ids of the rest, so ids (including split-part suffixes) are native.
type CopilotCLI struct{}

func (CopilotCLI) Format() string  { return FormatCopilotCLI }
func (CopilotCLI) Version() string { return copilotCLIDecoderVersion }

type copilotEvent struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	AgentID   string          `json:"agentId"`
	Data      json.RawMessage `json:"data"`
}

type copilotStart struct {
	CopilotVersion string `json:"copilotVersion"`
}

type copilotUserMessage struct {
	Content     string `json:"content"`
	Attachments []struct {
		MimeType string `json:"mimeType"`
	} `json:"attachments"`
}

type copilotAssistantMessage struct {
	Content       string `json:"content"`
	Model         string `json:"model"`
	Phase         string `json:"phase"`
	ReasoningText string `json:"reasoningText"`
	ToolRequests  []struct {
		ToolCallID string          `json:"toolCallId"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
	} `json:"toolRequests"`
}

type copilotUserRequested struct {
	ToolName  string          `json:"toolName"`
	Arguments json.RawMessage `json:"arguments"`
}

type copilotToolComplete struct {
	ToolCallID      string `json:"toolCallId"`
	Success         bool   `json:"success"`
	IsUserRequested bool   `json:"isUserRequested"`
	Result          *struct {
		Content             string `json:"content"`
		BinaryResultsForLlm []struct {
			Type     string `json:"type"`
			MimeType string `json:"mimeType"`
		} `json:"binaryResultsForLlm"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type copilotAbort struct {
	Reason string `json:"reason"`
}

type copilotCompaction struct {
	Success        bool   `json:"success"`
	SummaryContent string `json:"summaryContent"`
}

func (CopilotCLI) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatCopilotCLI, DecoderVersion: copilotCLIDecoderVersion, Harness: "copilot-cli"}
	out := newEmitter(&doc, emit)
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var ev copilotEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if ev.Type == "" {
			return structural("line %d is not a session event (no type)", lineNo)
		}
		// A sub-agent's events share the log, tagged with its agent id; the
		// parent's task tool call and its result carry what it returned.
		if ev.AgentID != "" {
			return nil
		}
		if ev.Type == "session.start" {
			var start copilotStart
			if err := json.Unmarshal(ev.Data, &start); err != nil {
				return structural("line %d session.start: %v", lineNo, err)
			}
			doc.SourceVersion = start.CopilotVersion
			return nil
		}
		parts, err := copilotRecords(ev, &doc)
		if err != nil {
			return structural("line %d %s: %v", lineNo, ev.Type, err)
		}
		if len(parts) == 0 {
			return nil
		}
		if ev.ID == "" {
			return structural("line %d: %s event has no id", lineNo, ev.Type)
		}
		at, err := recordTime(ev.Timestamp, fmt.Sprintf("line %d", lineNo))
		if err != nil {
			return err
		}
		for i, rec := range parts {
			rec.ID = ev.ID
			if len(parts) > 1 {
				rec.ID = fmt.Sprintf("%s:%s:%d", ev.ID, rec.Role, i)
			}
			rec.Fidelity = FidelityNative
			rec.Timestamp, rec.Time = ev.Timestamp, at
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

// copilotRecords maps one main-agent event. Only the user, assistant, tool,
// and system namespaces carry conversation; every other namespace (session
// lifecycle, model-call telemetry, hooks, permissions, MCP, sub-agent
// bookkeeping, ...) emits nothing.
func copilotRecords(ev copilotEvent, doc *Document) ([]Record, error) {
	switch ev.Type {
	case "user.message":
		var m copilotUserMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return nil, err
		}
		// content is what the person typed; transformedContent is the same
		// text wrapped in the context the harness adds for the model.
		var blocks []ContentBlock
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: m.Content})
		}
		for _, a := range m.Attachments {
			if strings.HasPrefix(a.MimeType, "image/") {
				blocks = append(blocks, imageBlock(a.MimeType))
			}
		}
		return []Record{{Role: "user", Content: blocksContent(blocks)}}, nil
	case "assistant.message":
		var m copilotAssistantMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return nil, err
		}
		var recs []Record
		if m.ReasoningText != "" {
			recs = append(recs, Record{Role: "reasoning", Model: m.Model, Content: textContent(m.ReasoningText)})
		}
		rec := Record{Role: "assistant", Model: m.Model, Phase: m.Phase}
		if m.Content != "" {
			rec.Content = textContent(m.Content)
		}
		for _, r := range m.ToolRequests {
			if r.ToolCallID == "" {
				return nil, fmt.Errorf("tool request %q has no toolCallId", r.Name)
			}
			rec.ToolCalls = append(rec.ToolCalls, ToolCall{ID: r.ToolCallID, Name: r.Name, Args: rawString(r.Arguments)})
		}
		if len(rec.Content) > 0 || len(rec.ToolCalls) > 0 {
			recs = append(recs, rec)
		}
		return recs, nil
	case "tool.user_requested":
		// A tool the person ran themselves (a ! shell command), not the model.
		var r copilotUserRequested
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return nil, err
		}
		return []Record{{Role: "user", Content: textContent(fmt.Sprintf("[%s] %s", r.ToolName, rawString(r.Arguments)))}}, nil
	case "tool.execution_complete":
		return copilotToolResult(ev, doc)
	case "abort":
		var a copilotAbort
		if err := json.Unmarshal(ev.Data, &a); err != nil {
			return nil, err
		}
		text := "Turn aborted"
		if a.Reason != "" {
			text += " (" + a.Reason + ")"
		}
		return []Record{{Role: "system", Phase: "abort", Content: textContent(text)}}, nil
	case "session.compaction_complete":
		var c copilotCompaction
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			return nil, err
		}
		if !c.Success {
			return nil, nil
		}
		text := c.SummaryContent
		if text == "" {
			text = "Conversation compacted"
		}
		return []Record{{Role: "system", Phase: "compaction", Content: textContent(text)}}, nil
	case "assistant.turn_start", "assistant.turn_end", "tool.execution_start", "system.message":
		// Turn bookkeeping; a start that repeats the model's tool request; the
		// harness's own system prompt.
		return nil, nil
	}
	namespace, _, _ := strings.Cut(ev.Type, ".")
	switch namespace {
	case "user", "assistant", "tool", "system":
		doc.WarningRecords++
		return []Record{{Role: "system", Phase: "normalization",
			Content: blocksContent([]ContentBlock{unsupportedBlock("Copilot CLI " + ev.Type + " event")})}}, nil
	}
	return nil, nil
}

// copilotToolResult maps a tool's completion, a tool record answering the
// model's request. The result of a tool the person ran themselves answers no
// request, so it is a system record.
func copilotToolResult(ev copilotEvent, doc *Document) ([]Record, error) {
	var c copilotToolComplete
	if err := json.Unmarshal(ev.Data, &c); err != nil {
		return nil, err
	}
	var blocks []ContentBlock
	text := ""
	if c.Result != nil {
		text = c.Result.Content
	}
	if text == "" && c.Error != nil {
		text = c.Error.Message
	}
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text})
	}
	if c.Result != nil {
		for _, b := range c.Result.BinaryResultsForLlm {
			if b.Type == "image" {
				blocks = append(blocks, imageBlock(b.MimeType))
				continue
			}
			doc.WarningRecords++
			blocks = append(blocks, unsupportedBlock("Copilot CLI "+b.Type+" result"))
		}
	}
	if c.IsUserRequested {
		return []Record{{Role: "system", Phase: "tool_result", Content: blocksContent(blocks)}}, nil
	}
	return []Record{{Role: "tool", ToolCallID: c.ToolCallID, Content: blocksContent(blocks), IsError: !c.Success}}, nil
}
