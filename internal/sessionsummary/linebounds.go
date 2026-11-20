package sessionsummary

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MaxLineBytes bounds one JSONL line — typically one Claude
// message. Lines larger than this get their tool_result content
// truncated with a visible marker so the line shrinks below the
// cap without corrupting its JSON shape.
//
// Why this cap exists: the chunker splits only on line boundaries
// (splitting mid-line would break JSON). A single line larger than
// the model context produces a chunk that fails at the model call
// with "prompt is too long". Tool_result blocks from big file reads
// or shell output dumps are the common culprit — a full file of
// source code can easily be 200KB in one line.
//
// Target: stay well under DefaultChunkBytes per line so a chunk of
// ~4-8 lines still fits under the model's effective context. 32KB is
// ~40K tokens at our observed ratio (~0.8 bytes/token) — generous
// for legitimate content, aggressive against tool_result dumps.
const MaxLineBytes = 32 * 1024

// BoundLineSizes walks a JSONL transcript and truncates any line
// whose size exceeds MaxLineBytes. Truncation targets the largest
// text payload in the line (tool_result content, tool_use input,
// assistant text, etc.) and preserves surrounding message structure.
//
// Unparseable lines that are also oversized fall through to a
// replacement placeholder — we'd corrupt the transcript worse by
// trying to edit bytes we can't parse.
//
// Returns the bounded transcript and a count of lines touched for
// log observability.
func BoundLineSizes(transcript []byte) ([]byte, int) {
	if len(transcript) == 0 {
		return transcript, 0
	}
	lines := splitJSONLines(transcript)
	var out bytes.Buffer
	touched := 0
	for i, line := range lines {
		if len(line) > MaxLineBytes {
			if bounded, ok := truncateLineToolResults(line); ok {
				line = bounded
				touched++
			} else {
				// Parsed OK but can't shrink below
				// MaxLineBytes — or unparseable. Replace
				// with a minimal placeholder preserving
				// whatever we can read of the role so the
				// chunker's guarantee holds.
				line = placeholderForOversized(line)
				touched++
			}
		}
		out.Write(line)
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), touched
}

// placeholderForOversized returns a short replacement line preserving
// the role field when we can read it. Worst-case (unparseable + too
// big) falls back to a fully anonymous marker.
func placeholderForOversized(line []byte) []byte {
	role := ""
	if msg, err := decodeLinePreservingNumbers(line); err == nil {
		if r, _ := msg["role"].(string); r != "" {
			role = r
		}
	}
	if role == "" {
		role = "unknown"
	}
	return []byte(fmt.Sprintf(
		`{"role":%q,"content":"[line replaced: original length %d bytes exceeded MaxLineBytes and could not be bounded]"}`,
		role, len(line)))
}

// truncateLineToolResults parses a JSONL line and truncates the
// largest text payload in it — tool_result content, tool_use input
// fields, assistant text blocks, whichever is biggest. Repeats on
// the next-largest if the line is still over the cap. Returns the
// bounded bytes + true on success, nil + false if the line can't be
// parsed or can't be shrunk below MaxLineBytes even after
// truncating everything worth truncating.
func truncateLineToolResults(line []byte) ([]byte, bool) {
	msg, err := decodeLinePreservingNumbers(line)
	if err != nil {
		return nil, false
	}

	// Up to 8 passes — enough to walk through a line with many
	// moderately-sized text blocks without looping forever.
	for pass := 0; pass < 8; pass++ {
		if !truncateBiggestTextPayload(msg) {
			break // nothing worth truncating left
		}
		out, err := json.Marshal(msg)
		if err != nil {
			return nil, false
		}
		if len(out) <= MaxLineBytes {
			return out, true
		}
	}
	// Nothing more to truncate; if we're still over, bail.
	out, err := json.Marshal(msg)
	if err != nil || len(out) > MaxLineBytes {
		return nil, false
	}
	return out, true
}

// truncateBiggestTextPayload finds the largest truncatable text
// payload anywhere in the message and shrinks it to keep the first
// truncatedBodySize bytes plus a visible marker. Payloads scanned:
// tool_result content (string or array shape), tool_use input,
// text blocks, top-level bare-string content.
//
// Returns true if something was shrunk. False when every
// truncatable payload is already small.
//
// truncatedBodySize is deliberately much smaller than MaxLineBytes
// (8KB vs 32KB) so a single pass usually drops the whole line under
// the cap.
func truncateBiggestTextPayload(msg map[string]any) bool {
	const truncatedBodySize = 8 * 1024
	const worthTruncating = 2 * 1024 // don't bother with tiny ones

	type candidate struct {
		setValue func(newText string)
		text     string
	}
	var cands []candidate

	// Top-level: msg.content can be a bare string.
	if s, ok := msg["content"].(string); ok && len(s) >= worthTruncating {
		cands = append(cands, candidate{
			text:     s,
			setValue: func(r string) { msg["content"] = r },
		})
	}

	walkContentTexts(msg, worthTruncating, func(text string, set func(string)) {
		cands = append(cands, candidate{text: text, setValue: set})
	})

	if len(cands) == 0 {
		return false
	}
	biggest := cands[0]
	for _, c := range cands[1:] {
		if len(c.text) > len(biggest.text) {
			biggest = c
		}
	}
	biggest.setValue(truncateWithPrefix(biggest.text, truncatedBodySize))
	return true
}

// truncateWithPrefix keeps the first keep bytes of s and appends a
// marker describing how many were dropped. The prefix matters — it
// gives the summarizer a hint of what the content was, which is
// often enough to describe the tool call even when the bulk body
// was dropped.
func truncateWithPrefix(s string, keep int) string {
	if len(s) <= keep {
		return s
	}
	// Cut on a rune boundary so we never produce invalid UTF-8
	// from slicing mid-codepoint.
	cut := keep
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf(
		"%s\n[... truncated: original length %d bytes, kept first %d]",
		s[:cut], len(s), cut)
}

// utf8RuneStart reports whether b is a valid first byte of a UTF-8
// sequence. Standard Go pattern avoids importing unicode/utf8 for a
// one-liner.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// walkContentTexts walks a message's content blocks and calls fn for
// each text payload worth truncating (≥ minLen bytes). fn receives
// the current text plus a setter that updates it in-place.
// Handles:
//   - tool_result content (string or [{type:text,text:...}] shapes)
//   - text blocks ({type:text,text:"..."})
//   - tool_use input (serialized JSON; replaced with a marker)
func walkContentTexts(msg map[string]any, minLen int, fn func(text string, set func(string))) {
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		t, _ := b["type"].(string)
		switch t {
		case "tool_result":
			// String-body shape.
			if s, ok := b["content"].(string); ok && len(s) >= minLen {
				fn(s, func(r string) { b["content"] = r })
				continue
			}
			// Array-of-blocks shape.
			inner, ok := b["content"].([]any)
			if !ok {
				continue
			}
			for j, sub := range inner {
				m, ok := sub.(map[string]any)
				if !ok {
					continue
				}
				if s, _ := m["text"].(string); len(s) >= minLen {
					jCopy := j
					fn(s, func(r string) {
						m["text"] = r
						inner[jCopy] = m
						b["content"] = inner
					})
				}
			}
		case "text":
			if s, _ := b["text"].(string); len(s) >= minLen {
				fn(s, func(r string) { b["text"] = r })
			}
		case "tool_use":
			// tool_use.input is a JSON object; we marshal
			// to measure, and on truncation replace with a
			// marker map since we can't do a sensible
			// textual prefix of a structured payload.
			if in, ok := b["input"].(map[string]any); ok {
				raw, err := json.Marshal(in)
				if err == nil && len(raw) >= minLen {
					rawStr := string(raw)
					fn(rawStr, func(r string) {
						b["input"] = map[string]any{
							"_truncated": r,
						}
					})
				}
			}
		}
	}
}

// decodeLinePreservingNumbers is the same trick as the redact
// package's helper: decode with UseNumber so large integer IDs
// don't lose precision on re-marshal. Local copy rather than
// importing to keep the summary package free of the redact dep.
func decodeLinePreservingNumbers(line []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}
