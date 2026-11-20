package ask

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestListToolSpec confirms the static schema parses + carries the
// no-question, ≥1-filter contract. The model-facing description has
// the load-bearing "find vs for-each" decision rule; if someone
// trims it we want to notice in CI.
func TestListToolSpec(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(listSchema(testSources), &parsed); err != nil {
		t.Fatalf("input schema: %v", err)
	}
	// No "question" property — that's the whole point of `list`.
	props, ok := parsed["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, has := props["question"]; has {
		t.Error("list tool schema must NOT have a question property")
	}
	for _, want := range []string{"sources", "actors", "occurred_after", "occurred_before", "top_k", "thread_roots_only", "session_level_only"} {
		if _, has := props[want]; !has {
			t.Errorf("schema missing property %q", want)
		}
	}
	// The description must say when list, not retrieve, is the tool.
	if !strings.Contains(listToolDescription, "To find the best matches for a topic, use `retrieve`.") {
		t.Error("description does not say when to use retrieve instead")
	}
}

// TestListToolStatus covers the Status() presentation helper.
func TestListToolStatus(t *testing.T) {
	line := listStatus(json.RawMessage(`{"actors":["alex@example.com"],"sources":["slack"]}`))
	if line.Emoji != "card_index_dividers" || line.Verb != "listing" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "actor=alex@example.com source=slack" {
		t.Errorf("detail = %q", line.Detail)
	}

	// No filters present → empty detail.
	line2 := listStatus(json.RawMessage(`{}`))
	if line2.Detail != "" {
		t.Errorf("detail = %q, want empty", line2.Detail)
	}

	// Malformed JSON → no detail, static fields intact.
	line3 := listStatus(json.RawMessage(`{bad`))
	if line3.Emoji != "card_index_dividers" || line3.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line3.Emoji, line3.Detail)
	}
}

func TestIsTruncated(t *testing.T) {
	if isTruncated("plain content") {
		t.Error("plain content flagged as truncated")
	}
	if isTruncated("the row said (truncated)") {
		t.Error("isTruncated accepted text that only ends like the marker")
	}
	if !isTruncated("body\n…(truncated)") {
		t.Error("marker suffix should be detected")
	}
	if isTruncated("") {
		t.Error("empty must not be truncated")
	}
}
