package sessionsummary

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func leanRecordLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	base := map[string]any{"index": 7, "id": "rec-7", "timestamp": "2026-11-18T10:00:00Z",
		"timestamp_source": "native", "model": "m", "provider_model": "p", "invocation_id": "inv-1"}
	for key, value := range fields {
		base[key] = value
	}
	line, err := json.Marshal(base)
	require.NoError(t, err)
	return string(line)
}

func leanLines(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(string(out), "\n") {
		var value map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &value), line)
		lines = append(lines, value)
	}
	return lines
}

func leanRender(transcript string, start int) []byte {
	chunk := Chunk{RecordStart: start, Bytes: []byte(transcript)}
	return NewLeanContext([]byte(transcript)).Render(chunk)
}

func TestLeanRenderKeepsContentAndDropsWhatCarriesNothing(t *testing.T) {
	fileBody := strings.Repeat("package main // body\n", 600)
	failure := "BUILD START\n" + strings.Repeat("compiling module\n", 600) + "FAIL: TestExportAcrossMidnight expected 2026-03-09, got 2026-03-10\n"
	harness := strings.Repeat("You are a coding agent. ", 40)
	notice := "Some captured content could not be included in this view; " + strings.Repeat("the original is kept in the session store. ", 10)
	chunk := strings.Join([]string{
		leanRecordLine(t, map[string]any{"role": "system", "content": harness}),
		leanRecordLine(t, map[string]any{"role": "user", "content": "Move the nightly export onto the job queue."}),
		leanRecordLine(t, map[string]any{"role": "assistant", "content": "I'll read the exporter first.", "tool_calls": []map[string]any{
			{"id": "call-read", "name": "Read", "args": `{"file_path":"cmd/exporter/cron.go"}`},
			{"id": "call-write", "name": "Write", "args": fmt.Sprintf(`{"file_path":"internal/exportworker/worker.go","content":%q}`, fileBody)},
			{"id": "call-test", "name": "Bash", "args": `{"command":"go test ./internal/exportworker/ 2>&1 | tail -5"}`},
		}}),
		leanRecordLine(t, map[string]any{"role": "tool", "tool_call_id": "call-read", "content": fileBody}),
		leanRecordLine(t, map[string]any{"role": "tool", "tool_call_id": "call-test", "is_error": true, "content": failure}),
		leanRecordLine(t, map[string]any{"role": "system", "content": harness}),
		leanRecordLine(t, map[string]any{"role": "system", "phase": "normalization", "content": notice}),
		leanRecordLine(t, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "Here is the screenshot"}, {"type": "image", "media_type": "image/png", "available": false}}}),
	}, "\n")
	out := leanRender(chunk, 0)
	lines := leanLines(t, out)
	require.Len(t, lines, 8)

	for _, field := range []string{`"invocation_id"`, `"provider_model"`, `"timestamp"`, `"id"`} {
		assert.NotContains(t, string(out), field, "record metadata is left out")
	}
	assert.Equal(t, harness, lines[0]["content"], "a harness's instructions are kept the first time")
	assert.Equal(t, "Move the nightly export onto the job queue.", lines[1]["content"])

	calls := lines[2]["tool_calls"].([]any)
	write := calls[1].(map[string]any)["args"].(map[string]any)
	assert.Equal(t, "internal/exportworker/worker.go", write["file_path"], "arguments are JSON, not escaped text")
	assert.Equal(t, fileBody, write["content"], "content is never cut")
	test := calls[2].(map[string]any)["args"].(map[string]any)
	assert.Equal(t, "go test ./internal/exportworker/ 2>&1 | tail -5", test["command"], "commands keep their characters unescaped")

	assert.Equal(t, "Read", lines[3]["tool"], "a result names its tool")
	assert.Equal(t, fileBody, lines[3]["content"])
	assert.Equal(t, true, lines[4]["failed"])
	assert.Equal(t, "Bash", lines[4]["tool"])
	assert.Equal(t, failure, lines[4]["content"])

	assert.Equal(t, "[same as an earlier record]", lines[5]["content"], "instructions sent again are named, not repeated")
	assert.Equal(t, "normalization", lines[6]["phase"])
	assert.LessOrEqual(t, len(lines[6]["content"].(string)), leanNoticeBytes+len("…"), "a notice keeps its start")
	assert.Equal(t, "Here is the screenshot\n[image]", lines[7]["content"])

	assert.Less(t, len(out), len(chunk))
}

func TestLeanRenderNeverGrowsAChunk(t *testing.T) {
	chunk := `{"role":"user","content":"hi"}`
	assert.Equal(t, []byte(chunk), leanRender(chunk, 0))
	assert.Equal(t, []byte("not a record"), leanRender("not a record", 0))
}

func TestLeanPrefixCutsOnCharacterBoundaries(t *testing.T) {
	out := leanPrefix(strings.Repeat("证据🦋", 100), leanNoticeBytes)
	assert.LessOrEqual(t, len(out), leanNoticeBytes+len("…"))
	for _, r := range out {
		assert.NotEqual(t, '�', r)
	}
}

// What a chunk is rendered with comes from the whole transcript before it: a
// result whose call is in an earlier chunk is named, and text first seen in
// an earlier chunk is a repeat.
func TestLeanContextSpansChunks(t *testing.T) {
	repeated := strings.Repeat("the same long instructions ", 20)
	transcript := strings.Join([]string{
		leanRecordLine(t, map[string]any{"role": "system", "content": repeated}),
		leanRecordLine(t, map[string]any{"role": "assistant", "tool_calls": []map[string]any{{"id": "call-1", "name": "Grep", "args": `{"pattern":"TODO"}`}}}),
		leanRecordLine(t, map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "a.go:1: TODO"}),
		leanRecordLine(t, map[string]any{"role": "system", "content": repeated}),
	}, "\n")
	records := strings.Split(transcript, "\n")
	context := NewLeanContext([]byte(transcript))
	lines := leanLines(t, context.Render(Chunk{RecordStart: 2, Bytes: []byte(records[2] + "\n" + records[3])}))
	assert.Equal(t, "Grep", lines[0]["tool"])
	assert.Equal(t, "[same as an earlier record]", lines[1]["content"])
	first := leanLines(t, context.Render(Chunk{RecordStart: 0, Bytes: []byte(records[0])}))
	assert.Equal(t, repeated, first[0]["content"], "the first occurrence is kept")
}
