package sessionsummary

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundLineSizes_SmallLinesUntouched(t *testing.T) {
	transcript := []byte(`{"role":"user","content":"hi"}
{"role":"assistant","content":"hello"}`)
	out, touched := BoundLineSizes(transcript)
	if touched != 0 {
		t.Fatalf("want 0 touched, got %d", touched)
	}
	if string(out) != string(transcript) {
		t.Fatalf("small lines should pass through untouched")
	}
}

func TestBoundLineSizes_OversizedToolResultString(t *testing.T) {
	// Typical culprit: a big file read / shell dump landing in
	// a tool_result as a bare string.
	bigBody := strings.Repeat("A", MaxLineBytes*2)
	raw := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"` + bigBody + `"}]}`
	transcript := []byte(raw)

	out, touched := BoundLineSizes(transcript)
	if touched != 1 {
		t.Fatalf("want 1 touched, got %d", touched)
	}
	if len(out) > MaxLineBytes+1024 {
		// Allow small slack for marker bytes, but the bounded
		// line should be radically smaller than the original.
		t.Fatalf("bounded line still too big: %d bytes (want <= %d)",
			len(out), MaxLineBytes+1024)
	}
	if !strings.Contains(string(out), "truncated") {
		t.Fatalf("missing truncation marker in output")
	}

	// The output must still be valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("bounded line is not valid JSON: %v", err)
	}
}

func TestBoundLineSizes_OversizedToolResultArrayShape(t *testing.T) {
	// Alternate tool_result shape: array of {type:text,text:...}
	// blocks. Common from Claude's own tool API.
	bigText := strings.Repeat("B", MaxLineBytes*2)
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_Y",
				"content": []any{
					map[string]any{"type": "text", "text": bigText},
				},
			},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	out, touched := BoundLineSizes(raw)
	if touched != 1 {
		t.Fatalf("want 1 touched, got %d", touched)
	}
	if len(out) > MaxLineBytes+1024 {
		t.Fatalf("bounded line still too big: %d bytes", len(out))
	}

	// Output must still be JSON-parseable and preserve the
	// tool_result shape.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("bounded line is not valid JSON: %v", err)
	}
	content, ok := parsed["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected 1 content block, got %v", parsed["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_result" {
		t.Fatalf("expected tool_result type, got %v", block["type"])
	}
}

func TestBoundLineSizes_LinesUnderCapStayIntact(t *testing.T) {
	// A line with a "medium" tool_result that's under the cap
	// shouldn't get touched, even though it's noticeably sized.
	mediumBody := strings.Repeat("C", 16*1024)
	raw := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_M","content":"` + mediumBody + `"}]}`
	transcript := []byte(raw)

	out, touched := BoundLineSizes(transcript)
	if touched != 0 {
		t.Fatalf("medium line should not be touched, got %d", touched)
	}
	if string(out) != string(transcript) {
		t.Fatalf("under-cap line modified: len in=%d out=%d",
			len(transcript), len(out))
	}
}

func TestBoundLineSizes_UnparseableOversizedLine(t *testing.T) {
	// A line that's both oversized AND unparseable should not
	// crash — it gets replaced with a placeholder.
	bad := []byte(strings.Repeat("not json ", MaxLineBytes/5))
	if len(bad) <= MaxLineBytes {
		t.Fatal("test setup: input should be oversized")
	}
	out, touched := BoundLineSizes(bad)
	if touched != 1 {
		t.Fatalf("want 1 touched, got %d", touched)
	}
	if len(out) > MaxLineBytes {
		t.Fatalf("replacement placeholder too big: %d", len(out))
	}
	if !strings.Contains(string(out), "[line replaced") {
		t.Fatalf("placeholder missing:\n%s", out)
	}
}

func TestBoundLineSizes_MultipleToolResults(t *testing.T) {
	// If multiple tool_results are big, repeatedly truncate the
	// largest until the line fits.
	body := strings.Repeat("D", MaxLineBytes)
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_A",
				"content":     body,
			},
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_B",
				"content":     body,
			},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	out, touched := BoundLineSizes(raw)
	if touched != 1 {
		t.Fatalf("want 1 touched, got %d", touched)
	}
	if len(out) > MaxLineBytes+2048 {
		t.Fatalf("multi-tool-result line still too big: %d", len(out))
	}
}

func TestBoundLineSizes_LargeIntegerPreservation(t *testing.T) {
	// Re-marshaling must not corrupt large integer IDs. Same
	// regression class as the redact package's test.
	const bigID = "1715644800123456"
	bigBody := strings.Repeat("X", MaxLineBytes*2)
	raw := `{"message_id":` + bigID + `,"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_P","content":"` + bigBody + `"}]}`
	out, _ := BoundLineSizes([]byte(raw))
	if !strings.Contains(string(out), bigID) {
		t.Fatalf("large integer precision lost on re-marshal:\n%s", out)
	}
}
