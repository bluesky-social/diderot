package sessiondecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// emitter numbers records, enforces the shared contract, and accumulates the
// session-level counters every native-format decoder reports.
type emitter struct {
	doc   *Document
	guard *contractGuard
	emit  func(Record) error
}

func newEmitter(doc *Document, emit func(Record) error) *emitter {
	return &emitter{doc: doc, guard: newContractGuard(), emit: emit}
}

func (e *emitter) add(rec Record) error {
	rec.Index = e.doc.RecordCount
	if err := e.guard.check(rec); err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return structural("record %q does not encode: %v", rec.ID, err)
	}
	e.doc.ContentBytes += int64(len(line) + 1)
	if rec.Role == "user" {
		e.doc.UserTurnCount++
	}
	if e.doc.RecordCount == 0 || rec.Time.Before(e.doc.StartedAt) {
		e.doc.StartedAt = rec.Time
	}
	if rec.Time.After(e.doc.UpdatedAt) {
		e.doc.UpdatedAt = rec.Time
	}
	e.doc.RecordCount++
	return e.emit(rec)
}

func (e *emitter) finish() error {
	if e.doc.RecordCount == 0 {
		return structural("document has no records")
	}
	return nil
}

// recordTime parses a source timestamp, which every emitted record needs.
func recordTime(raw, where string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, structural("%s has no timestamp", where)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, structural("%s timestamp %q is not RFC3339", where, raw)
	}
	return t, nil
}

// forEachLine calls fn with each non-blank JSONL line and its 1-based line
// number. Any line that is not a JSON object fails the document.
func forEachLine(data []byte, fn func(lineNo int, line []byte) error) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' || !json.Valid(line) {
			return structural("line %d is not a JSON object", lineNo)
		}
		if err := fn(lineNo, line); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return structural("scan: %v", err)
	}
	return nil
}

// textContent encodes plain text as record content.
func textContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// blocksContent encodes rich content; a lone text block collapses to a
// string so plain turns read the same in every format.
func blocksContent(blocks []ContentBlock) json.RawMessage {
	switch {
	case len(blocks) == 0:
		return nil
	case len(blocks) == 1 && blocks[0].Type == "text":
		return textContent(blocks[0].Text)
	}
	b, _ := json.Marshal(blocks)
	return b
}

// imageBlock stands in for inline image bytes, which never reach a model
// input or the record store.
func imageBlock(mediaType string) ContentBlock {
	unavailable := false
	return ContentBlock{Type: "image", MediaType: mediaType, Available: &unavailable}
}

// unsupportedBlock replaces content a decoder does not understand.
func unsupportedBlock(kind string) ContentBlock {
	return ContentBlock{Type: "text", Text: fmt.Sprintf("[Unsupported %s content omitted from the searchable view.]", kind)}
}

// inlineFields are object keys whose string values carry binary payloads.
var inlineFields = map[string]bool{"data": true, "base64": true, "bytes": true}

// scrubInline returns structured JSON with inline binary payloads (data:
// URLs and data/base64/bytes string fields) replaced by a notice, so a
// structured result kept as text never carries image or file bytes. Invalid
// JSON is returned unchanged.
func scrubInline(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	out, err := json.Marshal(scrubValue(v, ""))
	if err != nil {
		return raw
	}
	return out
}

func scrubValue(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = scrubValue(child, k)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = scrubValue(child, "")
		}
		return t
	case string:
		if inlineFields[key] || strings.HasPrefix(t, "data:") {
			return "[inline data omitted]"
		}
	}
	return v
}

// rawString returns a JSON string's value, or the raw JSON text otherwise.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// dataURLMediaType extracts the media type of a data: URL, or "".
func dataURLMediaType(url string) string {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return ""
	}
	mediaType, _, _ := strings.Cut(rest, ";")
	mediaType, _, _ = strings.Cut(mediaType, ",")
	return mediaType
}
