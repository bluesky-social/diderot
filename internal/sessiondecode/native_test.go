package sessiondecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite decoder golden files")

// goldenRecords renders what a decoder emitted: the model-facing line plus
// the fidelity metadata kept beside it.
func goldenRecords(t *testing.T, records []Record) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		wrapped, err := json.Marshal(struct {
			Fidelity Fidelity        `json:"fidelity"`
			Record   json.RawMessage `json:"record"`
		}{r.Fidelity, line})
		if err != nil {
			t.Fatal(err)
		}
		out.Write(wrapped)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// assertFixture decodes testdata/<dir>/<name>.jsonl under the shared
// contract and compares the records with <name>.records.jsonl.
func assertFixture(t *testing.T, d Decoder, dir, name string) (Document, []Record) {
	t.Helper()
	path := filepath.Join("testdata", dir, name+".jsonl")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join("testdata", dir, name+".json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, records := assertContract(t, d, data)
	got := goldenRecords(t, records)
	goldenPath := filepath.Join("testdata", dir, name+".records.jsonl")
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s/%s records differ from golden (run with -update and review):\n%s", dir, name, got)
	}
	return doc, records
}

func ids(records []Record) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID + "/" + r.Role
	}
	return out
}

func TestClaudeCodeFixture(t *testing.T) {
	doc, records := assertFixture(t, ClaudeCode{}, "claude-code", "basic")
	const p = "11111111-0000-4000-8000-0000000000"
	want := []string{
		p + "01/user",
		p + "03:reasoning:0/reasoning",
		p + "03:assistant:1/assistant",
		p + "04/tool",
		p + "05/assistant",
		p + "06:tool_result:0/tool",
		p + "06:tool_result:1/tool",
		p + "08/user",
		p + "09/assistant",
		p + "0a/system",
		p + "0b/system",
		p + "0c/system",
		p + "0e/user",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("active path ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityNative {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "2.1.323" || doc.Harness != "claude-code" || doc.UserTurnCount != 3 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	if records[9].Phase != "compaction" || records[10].Phase != "compaction_summary" || records[11].Phase != "meta" {
		t.Fatal("compaction marker, compact summary, or meta entry lost its phase")
	}
	if !records[6].IsError || records[2].ToolCalls[0].Args != `{"command":"ls"}` {
		t.Fatal("tool error flag or call arguments lost")
	}
	if bytes.Contains(goldenRecords(t, records), []byte("iVBORw0KGgo")) {
		t.Fatal("inline image bytes reached a record")
	}
}

func TestCodexFixture(t *testing.T) {
	doc, records := assertFixture(t, Codex{}, "codex", "basic")
	want := []string{
		"line:2/system",
		"line:3/user",
		"line:6/user",
		"rs_synthetic_1/reasoning",
		"call_A:call/assistant",
		"call_A:output/tool",
		"ctc_synthetic_1/assistant",
		"ctco_synthetic_1/tool",
		"line:14/system",
		"msg_synthetic_1/assistant",
		"call_C:call/assistant",
		"call_C:output/tool",
		"ws_synthetic_1/assistant",
		"line:19/user",
		"tsc_synthetic_1/system",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	fidelity := map[string]Fidelity{"line:2": FidelityPositional, "line:3": FidelityPositional, "line:6": FidelityPositional,
		"line:14": FidelityPositional, "line:19": FidelityPositional}
	for _, r := range records {
		wantFidelity := FidelityNative
		if f, ok := fidelity[r.ID]; ok {
			wantFidelity = f
		}
		if r.Fidelity != wantFidelity {
			t.Fatalf("%s fidelity %s, want %s", r.ID, r.Fidelity, wantFidelity)
		}
	}
	if doc.SourceVersion != "0.164.0" || doc.Harness != "codex" || doc.UserTurnCount != 3 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	if records[8].Phase != "compaction" || !records[11].IsError || records[3].Model != "gpt-synthetic-codex" {
		t.Fatal("compaction marker, tool error, or turn model lost")
	}
	if bytes.Contains(goldenRecords(t, records), []byte("iVBORw0KGgo")) {
		t.Fatal("inline image bytes reached a record")
	}
}

func TestAntigravityFixture(t *testing.T) {
	doc, records := assertFixture(t, Antigravity{}, "antigravity", "basic")
	want := []string{
		"step:0/user",
		"step:1:reasoning:0/reasoning",
		"step:1:assistant:1/assistant",
		"step:2/tool",
		"step:3/tool",
		"step:4/assistant",
		"step:5/tool",
		"step:6/system",
		"step:7/system",
		"step:8/system",
		"step:9/system",
		"step:10/assistant",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityPositional {
			t.Fatalf("%s fidelity %s, want positional", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "" || doc.Harness != "antigravity" || doc.UserTurnCount != 1 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	// Steps 2 and 3 arrived out of order in the file; sorting by step_index
	// pairs step:2 with call 0 (run_command) and step:3 with call 1 (read_file).
	if records[3].ToolCallID != "step:1:call:0" || !records[3].IsError {
		t.Fatalf("step:2 tool record = %+v", records[3])
	}
	if records[4].ToolCallID != "step:1:call:1" || records[4].IsError {
		t.Fatalf("step:3 tool record = %+v", records[4])
	}
	if records[6].ToolCallID != "step:4:call:0" || !records[6].IsError {
		t.Fatalf("step:5 tool record = %+v", records[6])
	}
	if records[8].Phase != "compaction" || records[9].Phase != "error" || !records[9].IsError || records[10].Phase != "normalization" {
		t.Fatalf("system phases lost: %+v", records[8:11])
	}
	if bytes.Contains(goldenRecords(t, records), []byte("synthetic-screenshot.png")) {
		t.Fatal("media uri path reached a record")
	}
}

func TestNativeDecodersRejectStructuralViolations(t *testing.T) {
	cc := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
	const ts = `"timestamp":"2026-11-01T10:00:00Z"`
	cases := []struct {
		name string
		d    Decoder
		data []byte
	}{
		{"claude-code orphan tool result", ClaudeCode{}, cc(
			`{"type":"user","uuid":"a","parentUuid":null,` + ts + `,"message":{"content":[{"type":"tool_result","tool_use_id":"nope","content":"x"}]}}`)},
		{"claude-code unparseable line", ClaudeCode{}, cc(
			`{"type":"user","uuid":"a","parentUuid":null,`+ts+`,"message":{"content":"hi"}}`, `{"type":`)},
		{"claude-code parent cycle", ClaudeCode{}, cc(
			`{"type":"user","uuid":"a","parentUuid":"b",`+ts+`,"message":{"content":"hi"}}`,
			`{"type":"user","uuid":"b","parentUuid":"a",`+ts+`,"message":{"content":"hi"}}`)},
		{"claude-code missing timestamp", ClaudeCode{}, cc(
			`{"type":"user","uuid":"a","parentUuid":null,"message":{"content":"hi"}}`)},
		{"claude-code tool call without id", ClaudeCode{}, cc(
			`{"type":"assistant","uuid":"a","parentUuid":null,` + ts + `,"message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`)},
		{"claude-code no conversation", ClaudeCode{}, cc(`{"type":"summary","summary":"x","leafUuid":"a"}`)},
		{"claude-code conversation entry without uuid", ClaudeCode{}, cc(
			`{"type":"user","uuid":"a","parentUuid":null,`+ts+`,"message":{"content":"hi"}}`,
			`{"type":"assistant","parentUuid":"a",`+ts+`,"message":{"content":[{"type":"text","text":"lost"}]}}`)},
		{"codex orphan output", Codex{}, cc(
			`{` + ts + `,"type":"response_item","payload":{"type":"function_call_output","call_id":"nope","output":"x"}}`)},
		{"codex call without call_id", Codex{}, cc(
			`{` + ts + `,"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`)},
		{"codex unwrapped legacy line", Codex{}, cc(`{"type":"message","role":"user","content":[]}`)},
		{"codex unparseable line", Codex{}, cc(`not json`)},
		{"codex duplicate item id", Codex{}, cc(
			`{`+ts+`,"type":"response_item","payload":{"type":"message","id":"m","role":"user","content":"a"}}`,
			`{`+ts+`,"type":"response_item","payload":{"type":"message","id":"m","role":"user","content":"b"}}`)},
		{"antigravity missing step_index", Antigravity{}, cc(
			`{"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-01T10:00:00Z","content":"hi"}`)},
		{"antigravity duplicate step_index", Antigravity{}, cc(
			`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-01T10:00:00Z","content":"a"}`,
			`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-01T10:00:01Z","content":"b"}`)},
		{"antigravity orphan tool result", Antigravity{}, cc(
			`{"step_index":0,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","created_at":"2026-11-01T10:00:00Z","content":"ok"}`)},
		{"antigravity missing created_at", Antigravity{}, cc(
			`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"hi"}`)},
		{"antigravity tool call without name", Antigravity{}, cc(
			`{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-01T10:00:00Z","tool_calls":[{"args":{}}]}`)},
		{"antigravity unparseable line", Antigravity{}, cc(`not json`)},
		{"antigravity empty document", Antigravity{}, cc(
			`{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-01T10:00:00Z"}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := DecodeAll(c.d, c.data); !errors.Is(err, ErrStructural) {
				t.Fatalf("err = %v, want ErrStructural", err)
			}
		})
	}
}

// TestClaudeCodeLaterDuplicateWins: a resumed session may rewrite an entry
// with the same uuid; the later copy is the one decoded, once.
func TestClaudeCodeLaterDuplicateWins(t *testing.T) {
	data := []byte(`{"type":"user","uuid":"a","parentUuid":null,"timestamp":"2026-11-01T10:00:00Z","message":{"content":"first"}}
{"type":"user","uuid":"a","parentUuid":null,"timestamp":"2026-11-01T10:00:00Z","message":{"content":"second"}}
`)
	_, records := assertContract(t, ClaudeCode{}, data)
	if len(records) != 1 || string(records[0].Content) != `"second"` {
		t.Fatalf("records = %+v", records)
	}
}

func TestCodexObjectOutputKeepsFailureWithoutContent(t *testing.T) {
	content, failed, err := codexOutput(json.RawMessage(`{"success":false,"error":"permission denied"}`), &Document{})
	if err != nil || !failed || !strings.Contains(string(content), "permission denied") {
		t.Fatalf("content=%s failed=%v err=%v", content, failed, err)
	}
	content, failed, err = codexOutput(json.RawMessage(`{"success":false,"content":[{"type":"input_text","text":"failed"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}`), &Document{})
	if err != nil || !failed || !strings.Contains(string(content), `"text":"failed"`) || strings.Contains(string(content), "iVBORw0KGgo") {
		t.Fatalf("rich object output: content=%s failed=%v err=%v", content, failed, err)
	}
}

func TestClaudeCodeEntryShapes(t *testing.T) {
	const ts = `"timestamp":"2026-11-01T10:00:00Z"`
	for _, missing := range []string{
		`{"type":"user","uuid":"a","parentUuid":null,` + ts + `}`,
		`{"type":"assistant","uuid":"a","parentUuid":null,` + ts + `,"message":null}`,
	} {
		if _, _, err := DecodeAll(ClaudeCode{}, []byte(missing+"\n")); !errors.Is(err, ErrStructural) {
			t.Fatalf("entry without message: err = %v", err)
		}
	}
	server := []byte(`{"type":"assistant","uuid":"a","parentUuid":null,` + ts + `,"message":{"content":[{"type":"server_tool_use","id":"srv_1","name":"code_execution","input":{}},{"type":"code_execution_tool_result","tool_use_id":"srv_1","content":{"type":"code_execution_result","stdout":"42","return_code":0}}]}}` + "\n")
	_, records := assertContract(t, ClaudeCode{}, server)
	if len(records) != 2 || records[1].Role != "tool" || !strings.Contains(string(records[1].Content), "stdout") {
		t.Fatalf("server tool result: %+v", records)
	}
	inline := []byte(`{"type":"assistant","uuid":"a","parentUuid":null,` + ts + `,"message":{"content":[{"type":"tool_use","id":"t1","name":"Screenshot","input":{}}]}}
{"type":"user","uuid":"b","parentUuid":"a",` + ts + `,"message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":{"source":{"type":"base64","data":"iVBORw0KGgo"},"url":"data:image/png;base64,iVBORw0KGgo"}}]}}
{"type":"future_kind","uuid":"c","parentUuid":"b",` + ts + `}
`)
	doc, records := assertContract(t, ClaudeCode{}, inline)
	if bytes.Contains(goldenRecords(t, records), []byte("iVBORw0KGgo")) {
		t.Fatal("inline payload in a structured tool result reached a record")
	}
	if len(records) != 3 || records[2].Phase != "normalization" || doc.WarningRecords != 1 {
		t.Fatalf("unknown on-path entry must leave a notice: %+v", records)
	}
	content, _, _ := codexOutput(json.RawMessage(`{"image":{"data":"iVBORw0KGgo"},"success":true}`), &Document{})
	if bytes.Contains(content, []byte("iVBORw0KGgo")) {
		t.Fatal("inline payload in a structured Codex output reached a record")
	}
}

func TestLaunchRegistry(t *testing.T) {
	r := Launch()
	for _, f := range []string{FormatBsky, FormatClaudeCode, FormatCodex, FormatAntigravity} {
		if _, ok := r.Lookup(f); !ok {
			t.Fatalf("%s not registered", f)
		}
	}
}
