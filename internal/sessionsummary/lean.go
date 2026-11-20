package sessionsummary

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// What the map model reads of each record. The transcript keeps every record
// whole: its chunk boundaries and content hashes, what is stored, and what
// expand returns are unchanged. Only the map request is rendered, and it
// leaves out only what carries nothing to summarize:
//
//	In each record                                  The map model reads
//	IDs, timestamps, model names                    nothing
//	a tool's output                                 the output, named by its tool
//	a tool call's arguments                         the arguments as JSON, not escaped text
//	text repeating an earlier record's text         a note saying so
//	  (of the same role, leanRepeatBytes or more)
//	continuations, call outcomes, capture notices   their first leanNoticeBytes
//	everything else                                 all of it
//
// Content itself is never cut. Cutting long outputs and file contents was
// measured: it saved little beyond the above and cost summaries the
// commands, identifiers and decisions they are later searched for.
const (
	leanRepeatBytes = 256
	leanNoticeBytes = 160
	leanRepeatNote  = "[same as an earlier record]"
)

// leanLine is one record as the map model reads it.
type leanLine struct {
	Role    string     `json:"role"`
	Phase   string     `json:"phase,omitempty"`
	Tool    string     `json:"tool,omitempty"`
	Failed  bool       `json:"failed,omitempty"`
	Content string     `json:"content,omitempty"`
	Calls   []leanCall `json:"tool_calls,omitempty"`
}

type leanCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// LeanContext is what rendering a chunk needs from the rest of its
// transcript: the tool each call ID belongs to, so a result whose call is in
// an earlier chunk is still named, and which records repeat an earlier
// record's text, such as a harness's instructions sent again on every call.
type LeanContext struct {
	names   map[string]string
	repeats map[int]bool
}

// NewLeanContext reads a transcript's records once for LeanContext.Render.
func NewLeanContext(transcript []byte) LeanContext {
	context := LeanContext{names: map[string]string{}, repeats: map[int]bool{}}
	first := map[[sha256.Size]byte]bool{}
	index := 0
	for _, line := range bytes.Split(transcript, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record sessiondecode.Record
		if json.Unmarshal(line, &record) == nil {
			for _, call := range record.ToolCalls {
				context.names[call.ID] = call.Name
			}
			if text := recordText(record.Content); len(text) >= leanRepeatBytes {
				key := sha256.Sum256([]byte(record.Role + "\x00" + text))
				context.repeats[index] = first[key]
				first[key] = true
			}
		}
		index++
	}
	return context
}

// Render renders one chunk of the transcript for the map model. The result
// is never longer than the chunk.
func (c LeanContext) Render(chunk Chunk) []byte {
	var out bytes.Buffer
	index := chunk.RecordStart
	for _, line := range bytes.Split(chunk.Bytes, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rendered, ok := c.record(line, c.repeats[index])
		if !ok {
			rendered = line
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.Write(rendered)
		index++
	}
	if out.Len() >= len(chunk.Bytes) {
		return chunk.Bytes
	}
	return out.Bytes()
}

// record renders one record line, or reports false for a line that is not a
// record, which is sent as it is.
func (c LeanContext) record(line []byte, repeat bool) ([]byte, bool) {
	var record sessiondecode.Record
	if json.Unmarshal(line, &record) != nil {
		return nil, false
	}
	text := recordText(record.Content)
	out := leanLine{Role: record.Role}
	switch record.Role {
	case "user", "assistant", "reasoning":
		for _, call := range record.ToolCalls {
			out.Calls = append(out.Calls, leanCall{Name: call.Name, Args: leanArgs(call.Args)})
		}
	case "tool":
		out.Tool = c.names[record.ToolCallID]
		out.Failed = record.IsError
	default:
		out.Phase = record.Phase
		notice := record.Phase != "" && record.Phase != "compaction" && record.Phase != "compaction_summary"
		if notice || record.SessionRef != "" {
			text = leanPrefix(text, leanNoticeBytes)
		}
	}
	if repeat {
		text = leanRepeatNote
	}
	out.Content = text
	rendered, err := marshalUnescaped(out)
	return rendered, err == nil
}

// recordText flattens a record's content to text: a string as it is, and
// blocks as their text, with images named rather than embedded.
func recordText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []sessiondecode.ContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		var block sessiondecode.ContentBlock
		if json.Unmarshal(raw, &block) != nil {
			return string(raw)
		}
		blocks = []sessiondecode.ContentBlock{block}
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Type == "image" && block.Alt != "":
			parts = append(parts, "[image: "+block.Alt+"]")
		case block.Type == "image":
			parts = append(parts, "[image]")
		case block.Text != "":
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// leanArgs embeds a call's arguments as JSON when they are JSON, rather than
// as a string escaped a second time, and as text otherwise.
func leanArgs(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	if json.Valid([]byte(args)) {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(args)) == nil {
			return compact.Bytes()
		}
	}
	out, _ := marshalUnescaped(args)
	return out
}

// leanPrefix keeps the first limit bytes of text, cut on a UTF-8 boundary.
func leanPrefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + "…"
}

func marshalUnescaped(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
