package ask

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDescribeToolCall(t *testing.T) {
	for _, tc := range []struct {
		name, input, output string
		asked, found        string
	}{
		{"retrieve", `{"question":"platform rollout"}`, `{"hits":[{"source":"slack"},{"source":"notion"}]}`, "platform rollout", "Found 2 relevant results"},
		{"list", `{"actors":["op@example.test"]}`, `{"hits":[],"count":0,"next_cursor":"x"}`, "actor=op@example.test", "Found 0 matching events; more available"},
		{"expand", `{"event_id":42}`, `{"source":"slack","supported":true,"truncated":true}`, "event=42", "Loaded source context from slack (truncated)"},
		{"web_search", `{"query":"go release notes"}`, `{"results":[{"title":"Go 1.26"}]}`, "go release notes", "Found 1 web result"},
		{"web_fetch", `{"url":"https://example.test/a"}`, `{"ok":false,"note":"blocked"}`, "https://example.test/a", "Could not read the page: blocked"},
		{"code_read", `{"repo":"org/repo","path":"main.go"}`, `{"ok":true,"repo":"org/repo","path":"main.go"}`, "org/repo/main.go", "Loaded org/repo/main.go"},
		{InvestigateToolName, `{"request":"why is p99 up"}`, `"report"`, "why is p99 up", ""},
		{"retrieve", `{"question":"cut off"}`, ``, "cut off", ""},
		{"retrieve", `{"question":"not json"}`, `plain text`, "not json", ""},
		{"sentry_search_issues", `{"query":"x"}`, `{}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output json.RawMessage
			if tc.output != "" {
				output = json.RawMessage(tc.output)
			}
			asked, found := DescribeToolCall(tc.name, json.RawMessage(tc.input), output)
			assert.Equal(t, tc.asked, asked)
			assert.Equal(t, tc.found, found)
		})
	}
}
