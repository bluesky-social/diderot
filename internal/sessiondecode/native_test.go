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

func TestPolytokenFixture(t *testing.T) {
	doc, records := assertFixture(t, Polytoken{}, "polytoken", "basic")
	const a, b, c = "AAAAAAAAAAAAAAAAAAAAAA:", "BBBBBBBBBBBBBBBBBBBBBB:", "CCCCCCCCCCCCCCCCCCCCCC:"
	want := []string{
		a + "3/user",
		a + "4/system",
		a + "6/system",
		a + "7:reasoning:0/reasoning",
		a + "7:assistant:1/assistant",
		a + "9:tool_result:0/tool",
		a + "9:tool_result:1/tool",
		a + "9:tool_result:2/tool",
		a + "10/assistant",
		a + "11/tool",
		a + "12/assistant",
		a + "13/user",
		a + "14/assistant",
		a + "15/tool",
		a + "17/user",
		a + "18/assistant",
		a + "20/user",
		a + "21/assistant",
		a + "22/tool",
		a + "23:reasoning:0/reasoning",
		a + "23:assistant:1/assistant",
		a + "25/tool",
		a + "26/system",
		a + "27/assistant",
		a + "28/user",
		a + "29/assistant",
		a + "30/tool",
		a + "31/system",
		b + "33/system",
		b + "34/system",
		b + "35/assistant",
		b + "36/system",
		c + "37/system",
		c + "38/user",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityNative {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "" || doc.Harness != "polytoken" || doc.UserTurnCount != 6 || doc.WarningRecords != 2 {
		t.Fatalf("document = %+v", doc)
	}
	// Three parallel calls in one assistant record, paired with one result
	// event; a failed command and a cancelled one are errors.
	if len(records[4].ToolCalls) != 3 || records[4].ToolCalls[2].Args != `{"command":"ls -la"}` || !records[9].IsError || !records[26].IsError {
		t.Fatal("parallel calls, call arguments, or error flags lost")
	}
	// The model in force when each record was written, across a switch.
	if records[3].Model != "synthetic-claude" || records[20].Model != "synthetic-gpt" || records[0].Model != "" {
		t.Fatal("record models lost")
	}
	phases := map[int]string{1: "system_reminder", 2: "system_reminder", 22: "subagent_complete", 27: "compaction",
		28: "system_reminder", 29: "reiterated", 31: "context_cleared", 32: "normalization"}
	for i, phase := range phases {
		if records[i].Role != "system" || records[i].Phase != phase {
			t.Fatalf("record %d is %s/%s, want system/%s", i, records[i].Role, records[i].Phase, phase)
		}
	}
	// A pasted image joins its prompt's user record; the copy re-attached
	// after the compaction is not shown again.
	if got := string(records[14].Content); got != `[{"type":"text","text":"What colour is this?"},{"type":"image","media_type":"image/png","available":false}]` {
		t.Fatalf("user record with image = %s", got)
	}
	if got := string(records[18].Content); got != `[{"type":"image","media_type":"image/png","available":false}]` {
		t.Fatalf("tool result image = %s", got)
	}
	golden := goldenRecords(t, records)
	for _, never := range []string{"iVBORw0KGgo", "SIGNATURE-SYNTHETIC", "OPAQUE-SYNTHETIC", "CONTINUATION-SYNTHETIC", "pasted-image-1.png", "ARTIFACTSYNTHETIC"} {
		if bytes.Contains(golden, []byte(never)) {
			t.Fatalf("%s reached a record", never)
		}
	}
}

// TestPolytokenOlderLayout: a log written before the envelope existed holds
// the bare events; ids are line numbers and the model comes from the switch.
func TestPolytokenOlderLayout(t *testing.T) {
	doc, records := assertFixture(t, Polytoken{}, "polytoken", "legacy")
	want := []string{"line:3/user", "line:4/system", "line:5:reasoning:0/reasoning", "line:5:assistant:1/assistant", "line:6/tool", "line:8/assistant"}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityPositional {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if records[5].Model != "synthetic-claude" || doc.UserTurnCount != 1 || doc.WarningRecords != 0 {
		t.Fatalf("model %q, document %+v", records[5].Model, doc)
	}
	if bytes.Contains(goldenRecords(t, records), []byte("SIGNATURE-SYNTHETIC")) {
		t.Fatal("a thinking signature reached a record")
	}
}

func TestNativeDecodersRejectStructuralViolations(t *testing.T) {
	cc := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
	const ts = `"timestamp":"2026-11-01T10:00:00Z"`
	const pt = `"emitted_at":"2026-11-01T10:00:00Z"`
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
		{"polytoken orphan tool result", Polytoken{}, cc(
			`{"type":"tool_result","results":[{"tool_use_id":"nope","content":{"text":"x"},"is_error":false}],` + pt + `}`)},
		{"polytoken unparseable line", Polytoken{}, cc(
			`{"type":"user","content":"hi","kind":"human",`+pt+`}`, `{"type":`)},
		{"polytoken missing timestamp", Polytoken{}, cc(`{"type":"user","content":"hi","kind":"human"}`)},
		{"polytoken tool call without id", Polytoken{}, cc(
			`{"type":"assistant","blocks":[{"type":"tool_use","name":"shell_exec","input":{}}],` + pt + `}`)},
		{"polytoken tool result without call id", Polytoken{}, cc(
			`{"type":"tool_result","results":[{"content":{"text":"x"},"is_error":false}],` + pt + `}`)},
		{"polytoken no conversation", Polytoken{}, cc(`{"type":"session_lifecycle","kind":"created","session_id":"s",` + pt + `}`)},
		{"polytoken event without a type", Polytoken{}, cc(`{"content":"hi",` + pt + `}`)},
		{"polytoken known event of the wrong shape", Polytoken{}, cc(`{"type":"user","content":7,"kind":"human",` + pt + `}`)},
		{"polytoken envelope without a lineage", Polytoken{}, cc(
			`{"index":0,"committed_at":"2026-11-01T10:00:00Z","message":{"type":"user","content":"hi","kind":"human",` + pt + `}}`)},
		{"polytoken duplicate envelope", Polytoken{}, cc(
			`{"index":0,"lineage":"L","committed_at":"2026-11-01T10:00:00Z","message":{"type":"user","content":"a","kind":"human",`+pt+`}}`,
			`{"index":0,"lineage":"L","committed_at":"2026-11-01T10:00:00Z","message":{"type":"user","content":"b","kind":"human",`+pt+`}}`)},
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

// TestPolytokenUnknownShapes: an event or block of a kind the decoder does
// not know becomes a notice whatever its fields hold, and an event with no
// time of its own takes the envelope's.
func TestPolytokenUnknownShapes(t *testing.T) {
	data := []byte(`{"index":0,"lineage":"L","committed_at":"2026-11-01T10:00:00Z","message":{"type":"user","content":"hi","kind":"human","emitted_at":"2026-11-01T10:00:00Z"}}
{"index":1,"lineage":"L","committed_at":"2026-11-01T10:00:02Z","message":{"type":"future_event","content":7,"blocks":"x","results":{},"reminder":[]}}
{"index":2,"lineage":"L","committed_at":"2026-11-01T10:00:03Z","message":{"type":"assistant","blocks":[{"type":"future_block","text":{"nested":true}}],"emitted_at":"2026-11-01T10:00:03Z"}}
{"index":3,"lineage":"L","committed_at":"2026-11-01T10:00:04Z","message":{"type":"tool_result","results":[],"emitted_at":"2026-11-01T10:00:04Z"}}
`)
	doc, records := assertContract(t, Polytoken{}, data)
	if len(records) != 3 || doc.WarningRecords != 2 || records[1].Phase != "normalization" || records[1].Timestamp != "2026-11-01T10:00:02Z" {
		t.Fatalf("records = %+v, document = %+v", records, doc)
	}
	// A bare event's own fields never pass for an envelope's.
	bare := []byte(`{"type":"user","content":"hi","kind":"human","emitted_at":"2026-11-01T10:00:00Z"}
{"type":"future_event","message":"notice","index":"x","attribution":"y","lineage":7,"emitted_at":"2026-11-01T10:00:01Z"}
`)
	doc, records = assertContract(t, Polytoken{}, bare)
	if len(records) != 2 || records[1].ID != "line:2" || records[1].Phase != "normalization" || doc.WarningRecords != 1 {
		t.Fatalf("records = %+v, document = %+v", records, doc)
	}
	content := ptResultContent(json.RawMessage(`{"blob":{"data":"iVBORw0KGgo"}}`), &Document{})
	if bytes.Contains(content, []byte("iVBORw0KGgo")) {
		t.Fatal("inline payload in an unknown tool result reached a record")
	}
}

func TestLaunchRegistry(t *testing.T) {
	r := Launch()
	for _, f := range []string{FormatBsky, FormatClaudeCode, FormatCodex, FormatPolytoken} {
		if _, ok := r.Lookup(f); !ok {
			t.Fatalf("%s not registered", f)
		}
	}
}
