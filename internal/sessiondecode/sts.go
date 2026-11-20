package sessiondecode

import (
	"encoding/json"
	"fmt"
	"time"
)

// FormatSTS is Hugging Face's Session Trace Simple Format
// (https://huggingface.co/docs/hub/en/session-traces-format). Mapping notes:
// FORMATS.md.
const FormatSTS = "hf-sts-jsonl"

const stsDecoderVersion = "1"

// STS decodes STS-Format JSONL. Message lines carry no ids and the format has
// no extension slot on them, so every id is positional: the line number.
type STS struct{}

func (STS) Format() string  { return FormatSTS }
func (STS) Version() string { return stsDecoderVersion }

type stsLine struct {
	Type    string      `json:"type"`
	Harness string      `json:"harness"`
	ID      string      `json:"id"`
	Message *stsMessage `json:"message"`
}

type stsMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoningContent"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"toolCalls"`
	ToolCallID string   `json:"toolCallId"`
	Timestamp  *float64 `json:"timestamp"`
	Model      string   `json:"model"`
}

func (STS) Decode(data []byte, emit func(Record) error) (Document, error) {
	doc := Document{Format: FormatSTS, DecoderVersion: stsDecoderVersion}
	fill := &timeFiller{out: newEmitter(&doc, emit)}
	header := false
	err := forEachLine(data, func(lineNo int, raw []byte) error {
		var ln stsLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		if !header {
			if ln.Type != "session" || ln.Harness == "" || ln.ID == "" {
				return structural("line %d is not an STS session header (type, harness, id)", lineNo)
			}
			header = true
			doc.Harness = ln.Harness
			return nil
		}
		id := fmt.Sprintf("line:%d", lineNo)
		if ln.Type != "message" {
			doc.WarningRecords++
			return fill.add(Record{ID: id, Fidelity: FidelityPositional, Role: "system", Phase: "normalization",
				Content: blocksContent([]ContentBlock{unsupportedBlock("STS " + ln.Type + " line")})}, time.Time{}, false)
		}
		if ln.Message == nil {
			return structural("line %d: message line without a message", lineNo)
		}
		m := ln.Message
		var at time.Time
		if m.Timestamp != nil {
			// Epoch milliseconds within years 1970..9999, so the
			// record's RFC 3339 timestamp stays representable.
			if ms := *m.Timestamp; !(ms >= 0 && ms <= 253402300799999) {
				return structural("line %d: timestamp %v is out of range", lineNo, ms)
			}
			at = time.UnixMilli(int64(*m.Timestamp)).UTC()
		}
		recs, err := stsRecords(m, id, &doc)
		if err != nil {
			return structural("line %d: %v", lineNo, err)
		}
		for _, r := range recs {
			if err := fill.add(r, at, m.Timestamp != nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Document{}, err
	}
	if !header {
		return Document{}, structural("no STS session header")
	}
	if err := fill.finish(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func stsRecords(m *stsMessage, id string, doc *Document) ([]Record, error) {
	content, err := stsContent(m.Content, doc)
	if err != nil {
		return nil, err
	}
	var main Record
	switch m.Role {
	case "user", "system":
		main = Record{Role: m.Role, Content: content}
	case "assistant":
		main = Record{Role: "assistant", Model: m.Model, Content: content}
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				return nil, fmt.Errorf("tool call without an id")
			}
			main.ToolCalls = append(main.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Args: rawString(c.Function.Arguments)})
		}
	case "tool":
		main = Record{Role: "tool", ToolCallID: m.ToolCallID, Content: content}
	default:
		doc.WarningRecords++
		main = Record{Role: "system", Phase: "normalization", Content: blocksContent([]ContentBlock{unsupportedBlock("STS " + m.Role + " message")})}
	}
	main.ID, main.Fidelity = id, FidelityPositional
	if m.Role != "assistant" || m.ReasoningContent == "" {
		return []Record{main}, nil
	}
	reasoning := Record{ID: id + ":reasoning:0", Fidelity: FidelityPositional, Role: "reasoning", Model: m.Model, Content: textContent(m.ReasoningContent)}
	main.ID = id + ":assistant:1"
	if len(main.Content) == 0 && len(main.ToolCalls) == 0 {
		return []Record{reasoning}, nil
	}
	return []Record{reasoning, main}, nil
}

// stsContent accepts the documented text content and, leniently, an array of
// text blocks.
func stsContent(raw json.RawMessage, doc *Document) (json.RawMessage, error) {
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
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	blocks := make([]ContentBlock, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: p.Text})
			continue
		}
		doc.WarningRecords++
		blocks = append(blocks, unsupportedBlock("STS "+p.Type))
	}
	return blocksContent(blocks), nil
}
