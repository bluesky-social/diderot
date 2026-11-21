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

func TestCopilotCLIFixture(t *testing.T) {
	doc, records := assertFixture(t, CopilotCLI{}, "copilot-cli", "basic")
	const p = "c0000000-0000-4000-8000-0000000000"
	want := []string{
		p + "04/user",
		p + "08:reasoning:0/reasoning",
		p + "08:assistant:1/assistant",
		p + "0b/tool",
		p + "0c/tool",
		p + "0f/assistant",
		p + "12/tool",
		p + "13/tool",
		p + "15/assistant",
		p + "18/tool",
		p + "19/tool",
		p + "1a/assistant",
		p + "1e/user",
		p + "1f/assistant",
		p + "20/tool",
		p + "21/assistant",
		p + "22/user",
		p + "23/assistant",
		p + "2d/tool",
		p + "2e/assistant",
		p + "2f/user",
		p + "30/system",
		p + "31/user",
		p + "34/assistant",
		p + "37/system",
		p + "39/system",
		p + "3c/tool",
		p + "3e/user",
		p + "3f/system",
		p + "40/assistant",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityNative {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "1.0.114" || doc.Harness != "copilot-cli" || doc.UserTurnCount != 6 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[strings.TrimPrefix(r.ID, p)] = r
	}
	if string(byID["04"].Content) != `"Read notes.txt and calc.py, run calc.py, and fix it."` {
		t.Fatalf("user record kept the harness's wrapper: %s", byID["04"].Content)
	}
	if byID["08:assistant:1"].Phase != "commentary" || len(byID["08:assistant:1"].ToolCalls) != 2 || byID["1f"].Model != "synthetic-model-b" {
		t.Fatal("assistant phase, parallel tool calls, or the switched model lost")
	}
	if !byID["19"].IsError || byID["12"].IsError || !strings.HasPrefix(byID["15"].ToolCalls[0].Args, "*** Begin Patch\n") {
		t.Fatal("tool error flag or patch arguments lost")
	}
	if byID["30"].Phase != "tool_result" || byID["37"].Phase != "abort" || byID["39"].Phase != "compaction" ||
		byID["3f"].Phase != "normalization" || !byID["3c"].IsError {
		t.Fatal("a user-run tool's result, the abort, the compaction, the notice, or the interrupted call lost its shape")
	}
	got := goldenRecords(t, records)
	for _, leaked := range []string{"iVBORw0KGgo", "synthetic classifier", "wc -l", "As a sub-agent", "synthetic system prompt", "sandbox guidance"} {
		if bytes.Contains(got, []byte(leaked)) {
			t.Fatalf("%q reached a record: image bytes, side model calls, sub-agent events, and harness prompts never do", leaked)
		}
	}
}

func TestGeminiCLIFixture(t *testing.T) {
	doc, records := assertFixture(t, GeminiCLI{}, "gemini-cli", "basic")
	const p = "aaaaaaaa-0000-4000-8000-0000000000"
	want := []string{
		"5e1c0a2b3c4d5e6f708192a3b4c5d6e7/system",
		p + "03/system",
		p + "04/assistant",
		p + "06/user",
		p + "07:reasoning:0/reasoning",
		p + "07:assistant:1/assistant",
		p + "08:tool_result:0/tool",
		p + "08:tool_result:1/tool",
		p + "09:reasoning:0/reasoning",
		p + "09:assistant:1/assistant",
		p + "0a/user",
		p + "0b/assistant",
		p + "0d/tool",
		p + "0e/assistant",
		p + "11/user",
		p + "12:reasoning:0/reasoning",
		p + "12:assistant:1/assistant",
		p + "13/tool",
		p + "14/assistant",
		p + "15/user",
		p + "16/assistant",
		p + "17/system",
		p + "18/system",
		p + "19/system",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("live messages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityNative {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if doc.SessionID != "7c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5" || doc.SourceVersion != "" || doc.Harness != "gemini-cli" ||
		doc.UserTurnCount != 4 || doc.WarningRecords != 2 {
		t.Fatalf("document = %+v", doc)
	}
	// Injected context and the compression summary are not the person's
	// words; the expanded @-reference is replaced by what was typed, and
	// its image keeps a placeholder.
	if records[0].Phase != "context" || records[1].Phase != "compaction_summary" ||
		string(records[3].Content) != `[{"type":"text","text":"Check @logo.png and run the tests."},{"type":"image","media_type":"image/png","available":false}]` {
		t.Fatal("injected context, compression summary, or typed prompt lost")
	}
	// Two parallel calls; the image result keeps a placeholder, and a
	// non-zero exit is output, not an error. The cancelled call's result is.
	if len(records[5].ToolCalls) != 2 || records[5].ToolCalls[1].Args != `{"description":"Run the test suite.","command":"make test"}` ||
		!strings.Contains(string(records[6].Content), `"type":"image","media_type":"image/png"`) || records[7].IsError || !records[12].IsError {
		t.Fatal("tool calls, image result, or error flags lost")
	}
	// After the resume patch the subagent turn is read from its parts.
	if records[16].ToolCalls[0].ID != "invoke_agent__call_1004" || records[15].Content == nil {
		t.Fatal("patched turn lost its call or reasoning")
	}
	if records[20].Model != "gemini-synthetic-pro" || records[21].Phase != "error" || records[22].Phase != "warning" || records[23].Phase != "normalization" {
		t.Fatal("model switch or notices lost")
	}
	if bytes.Contains(goldenRecords(t, records), []byte("iVBORw0KGgo")) {
		t.Fatal("inline image bytes reached a record")
	}
}

// TestGeminiCLILogOperations: later copies of a message replace it in place,
// a rewind to an unknown message drops everything, and orderIds moves the
// listed messages to the end.
func TestGeminiCLILogOperations(t *testing.T) {
	const meta = `{"sessionId":"7c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5","projectHash":"x","startTime":"2026-11-01T10:00:00.000Z"}`
	msg := func(id, text string) string {
		return `{"id":"` + id + `","timestamp":"2026-11-01T10:00:00.000Z","type":"user","content":[{"text":"` + text + `"}]}`
	}
	doc := func(lines ...string) []byte {
		return []byte(strings.Join(append([]string{meta}, lines...), "\n") + "\n")
	}
	_, records := assertContract(t, GeminiCLI{}, doc(msg("a", "one"), msg("b", "two"), msg("a", "one again"),
		`{"$patch":{"orderIds":["a"]}}`))
	if got := strings.Join(ids(records), ","); got != "b/user,a/user" || string(records[1].Content) != `"one again"` {
		t.Fatalf("records = %v %s", got, records[1].Content)
	}
	_, records = assertContract(t, GeminiCLI{}, doc(msg("a", "one"), `{"$rewindTo":"gone"}`, msg("b", "two")))
	if got := strings.Join(ids(records), ","); got != "b/user" {
		t.Fatalf("records = %v", got)
	}
	// A rewind follows the order a move left; a removed message written
	// again goes to the end.
	_, records = assertContract(t, GeminiCLI{}, doc(msg("a", "1"), msg("b", "2"), msg("c", "3"),
		`{"$patch":{"orderIds":["a","a","gone"]}}`, `{"$rewindTo":"c"}`, msg("d", "4"),
		`{"$patch":{"removeIds":["b"]}}`, msg("b", "5")))
	if got := strings.Join(ids(records), ","); got != "d/user,b/user" || string(records[1].Content) != `"5"` {
		t.Fatalf("records = %v", got)
	}
	// An unknown message type is a notice whatever its content holds.
	_, records = assertContract(t, GeminiCLI{}, doc(`{"id":"f","timestamp":"2026-11-01T10:00:00.000Z","type":"future_kind","content":7}`))
	if len(records) != 1 || records[0].Phase != "normalization" {
		t.Fatalf("records = %+v", records)
	}
}

func TestGeminiCLIFunctionResponses(t *testing.T) {
	for raw, want := range map[string]struct {
		text  string
		isErr bool
	}{
		`{"output":"ok"}`:                     {"ok", false},
		`{"output":"partial","error":"boom"}`: {"partial\nboom", true},
		`{}`:                                  {"{}", false},
		`{"content":[{"type":"image","data":"iVBORw0KGgo"}]}`: {`{"content":[{"data":"[inline data omitted]","type":"image"}]}`, false},
	} {
		if text, isErr := gcResponseText(json.RawMessage(raw)); text != want.text || isErr != want.isErr {
			t.Errorf("%s: text %q error %v, want %q %v", raw, text, isErr, want.text, want.isErr)
		}
	}
}

func TestPiFixture(t *testing.T) {
	doc, records := assertFixture(t, Pi{}, "pi", "basic")
	want := []string{
		"10000004/user",
		"10000005/assistant",
		"10000008/system",
		"1000000a:user:0/user",
		"1000000a:system:0/system",
		"1000000b:reasoning:0/reasoning",
		"1000000b:assistant:1/assistant",
		"1000000c/tool",
		"1000000d/tool",
		"1000000e/assistant",
		"1000000f/tool",
		"10000010/assistant",
		"10000012/user",
		"10000013:reasoning:0/reasoning",
		"10000013:assistant:1/assistant",
		"10000014/tool",
		"10000015/assistant",
		"10000016/user",
		"10000017:reasoning:0/reasoning",
		"10000017:assistant:1/assistant",
		"10000017:system:2/system",
		"10000018/user",
		"10000019/system",
		"1000001d/system",
		"1000001f/user",
		"10000020/system",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("active path ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityNative {
			t.Fatalf("%s fidelity %s", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "3" || doc.Harness != "pi" || doc.UserTurnCount != 6 || doc.WarningRecords != 0 {
		t.Fatalf("document = %+v", doc)
	}
	phases := []string{records[2].Phase, records[4].Phase, records[20].Phase, records[22].Phase, records[23].Phase, records[25].Phase}
	if strings.Join(phases, ",") != "branch_summary,meta,aborted,custom,compaction,error" {
		t.Fatalf("system phases = %v", phases)
	}
	// A skill prompt is what the person typed, and the skill file Pi sent
	// in its place is kept beside it.
	if string(records[3].Content) != `"/skill:release-notes for the next tag"` ||
		!strings.HasSuffix(rawString(records[4].Content), "Write release notes from the git log since the last tag.") {
		t.Fatalf("skill invocation = %s, then %s", records[3].Content, records[4].Content)
	}
	if !records[8].IsError || records[6].ToolCalls[1].Args != `{"command":"go test ./..."}` || records[14].Model != "claude-synthetic-2" {
		t.Fatal("tool error flag, call arguments, or switched model lost")
	}
	if !strings.Contains(string(records[21].Content), "Command exited with code 1") {
		t.Fatalf("bash execution = %s", records[21].Content)
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
	const gcMeta = `{"sessionId":"7c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5","projectHash":"x"}`
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
		{"copilot-cli unparseable line", CopilotCLI{}, cc(
			`{"type":"user.message","data":{"content":"hi"},"id":"a",`+ts+`}`, `{"type":`)},
		{"copilot-cli event without type", CopilotCLI{}, cc(`{"data":{"content":"hi"},"id":"a",` + ts + `}`)},
		{"copilot-cli missing timestamp", CopilotCLI{}, cc(`{"type":"user.message","data":{"content":"hi"},"id":"a"}`)},
		{"copilot-cli conversation event without id", CopilotCLI{}, cc(`{"type":"user.message","data":{"content":"hi"},` + ts + `}`)},
		{"copilot-cli message without data", CopilotCLI{}, cc(`{"type":"assistant.message","id":"a",` + ts + `}`)},
		{"copilot-cli orphan tool result", CopilotCLI{}, cc(
			`{"type":"tool.execution_complete","data":{"toolCallId":"nope","success":true,"result":{"content":"x"}},"id":"a",` + ts + `}`)},
		{"copilot-cli tool request without id", CopilotCLI{}, cc(
			`{"type":"assistant.message","data":{"content":"","toolRequests":[{"name":"bash","arguments":{}}]},"id":"a",` + ts + `}`)},
		{"copilot-cli duplicate event id", CopilotCLI{}, cc(
			`{"type":"user.message","data":{"content":"a"},"id":"a",`+ts+`}`,
			`{"type":"user.message","data":{"content":"b"},"id":"a",`+ts+`}`)},
		{"copilot-cli no conversation", CopilotCLI{}, cc(
			`{"type":"session.start","data":{"copilotVersion":"1.0.114"},"id":"a",`+ts+`}`,
			`{"type":"session.error","data":{"errorType":"query","message":"Model is not available."},"id":"b",`+ts+`}`)},
		{"gemini-cli orphan tool result", GeminiCLI{}, cc(gcMeta,
			`{"id":"u",`+ts+`,"type":"user","content":[{"functionResponse":{"id":"nope","name":"read_file","response":{"output":"x"}}}]}`)},
		{"gemini-cli tool call without id", GeminiCLI{}, cc(gcMeta,
			`{"id":"g",`+ts+`,"type":"gemini","content":"","toolCalls":[{"name":"read_file","args":{}}]}`)},
		{"gemini-cli functionCall without id", GeminiCLI{}, cc(gcMeta,
			`{"id":"g",`+ts+`,"type":"gemini","content":[{"functionCall":{"name":"read_file","args":{}}}]}`)},
		{"gemini-cli missing timestamp", GeminiCLI{}, cc(gcMeta, `{"id":"u","type":"user","content":[{"text":"hi"}]}`)},
		{"gemini-cli no session metadata", GeminiCLI{}, cc(`{"id":"u",` + ts + `,"type":"user","content":[{"text":"hi"}]}`)},
		{"gemini-cli unparseable content", GeminiCLI{}, cc(gcMeta, `{"id":"u",`+ts+`,"type":"user","content":7}`)},
		{"gemini-cli unparseable line", GeminiCLI{}, cc(gcMeta, `{"id":`)},
		{"gemini-cli message without id", GeminiCLI{}, cc(gcMeta,
			`{"id":"u",`+ts+`,"type":"user","content":[{"text":"hi"}]}`, `{`+ts+`,"type":"gemini","content":"lost"}`)},
		{"gemini-cli message with an empty id", GeminiCLI{}, cc(gcMeta,
			`{"id":"",`+ts+`,"type":"gemini","content":"reply","thoughts":[{"subject":"s","description":"d"}]}`)},
		{"gemini-cli everything rewound", GeminiCLI{}, cc(gcMeta,
			`{"id":"u",`+ts+`,"type":"user","content":[{"text":"hi"}]}`, `{"$rewindTo":"u"}`)},
		{"pi no session header", Pi{}, cc(
			`{"type":"message","id":"a","parentId":null,` + ts + `,"message":{"role":"user","content":"hi"}}`)},
		{"pi unparseable line", Pi{}, cc(piHeader, `{"type":`)},
		{"pi entry without id", Pi{}, cc(piHeader,
			`{"type":"message",`+ts+`,"message":{"role":"user","content":"hi"}}`)},
		{"pi parent cycle", Pi{}, cc(piHeader,
			`{"type":"message","id":"a","parentId":"b",`+ts+`,"message":{"role":"user","content":"hi"}}`,
			`{"type":"message","id":"b","parentId":"a",`+ts+`,"message":{"role":"user","content":"hi"}}`)},
		{"pi missing timestamp", Pi{}, cc(piHeader,
			`{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":"hi"}}`)},
		{"pi orphan tool result", Pi{}, cc(piHeader,
			`{"type":"message","id":"a","parentId":null,`+ts+`,"message":{"role":"toolResult","toolCallId":"nope","content":[{"type":"text","text":"x"}],"isError":false}}`)},
		{"pi tool call without id", Pi{}, cc(piHeader,
			`{"type":"message","id":"a","parentId":null,`+ts+`,"message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{}}]}}`)},
		{"pi message entry without message", Pi{}, cc(piHeader, `{"type":"message","id":"a","parentId":null,`+ts+`}`)},
		{"pi no conversation", Pi{}, cc(piHeader,
			`{"type":"model_change","id":"a","parentId":null,`+ts+`,"provider":"anthropic","modelId":"m"}`)},
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

// TestCopilotCLIEventShapes: what the fixture does not show. A binary result
// that is not an image is a notice, a failed compaction changed nothing, and
// an event outside the conversation namespaces is skipped however it is
// shaped.
func TestCopilotCLIEventShapes(t *testing.T) {
	const ts = `"timestamp":"2026-11-01T10:00:00Z"`
	data := []byte(`{"type":"user.message","data":{"content":"hi"},"id":"a",` + ts + `}
{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"c1","name":"fetch","arguments":{}}]},"id":"c",` + ts + `}
{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true,"result":{"content":"","binaryResultsForLlm":[{"type":"resource","mimeType":"application/pdf","data":"JVBERi0x"}]}},"id":"d",` + ts + `}
{"type":"session.compaction_complete","data":{"success":false,"error":"failed"},"id":"e",` + ts + `}
{"type":"future.kind","data":7,"id":"f",` + ts + `}
`)
	doc, records := assertContract(t, CopilotCLI{}, data)
	if got := strings.Join(ids(records), " "); got != "a/user c/assistant d/tool" {
		t.Fatalf("records = %s", got)
	}
	if doc.WarningRecords != 1 || bytes.Contains(goldenRecords(t, records), []byte("JVBERi0x")) {
		t.Fatalf("binary resource: %+v, warnings %d", records, doc.WarningRecords)
	}
}

const piHeader = `{"type":"session","version":3,"id":"019a3f00-0000-7000-8000-000000000002","timestamp":"2026-11-01T10:00:00.000Z","cwd":"/work"}`

// TestPiActivePath: the live branch is the parent chain of the file's last
// entry, whatever its type; a repeated id's later copy wins, and a parent
// missing from the file ends the chain.
func TestPiActivePath(t *testing.T) {
	const ts = `"timestamp":"2026-11-01T10:00:00.000Z"`
	user := func(id, parent, text string) string {
		return `{"type":"message","id":"` + id + `","parentId":` + parent + `,` + ts + `,"message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}`
	}
	data := []byte(strings.Join([]string{
		piHeader,
		user("u1", `"gone"`, "first"),
		`{"type":"message","id":"a1","parentId":"u1",` + ts + `,"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		user("u2", `"a1"`, "abandoned"),
		user("u3", `"a1"`, "draft"),
		user("u3", `"a1"`, "kept"),
		`{"type":"label","id":"l1","parentId":"u3",` + ts + `,"targetId":"u2","label":"old"}`,
	}, "\n") + "\n")
	_, records := assertContract(t, Pi{}, data)
	if got := strings.Join(ids(records), ","); got != "u1/user,a1/assistant,u3/user" {
		t.Fatalf("path = %s", got)
	}
	if string(records[2].Content) != `"kept"` {
		t.Fatalf("repeated id: %s", records[2].Content)
	}
}

func TestPiEntryShapes(t *testing.T) {
	const ts = `"timestamp":"2026-11-01T10:00:00.000Z"`
	data := []byte(strings.Join([]string{
		piHeader,
		`{"type":"message","id":"s1","parentId":null,` + ts + `,"message":{"role":"system","content":"","sections":{"preamble":"x"},"timestamp":1}}`,
		`{"type":"message","id":"u1","parentId":"s1",` + ts + `,"message":{"role":"user","content":[{"type":"text","text":"<skill name=\"lint\" location=\"/s/lint/SKILL.md\">\nReferences are relative to /s/lint.\n\nRun the linter.\n</skill>"},{"type":"audio","data":"AAAA"}]}}`,
		`{"type":"message","id":"c1","parentId":"u1",` + ts + `,"message":{"role":"custom","customType":"ext","content":[{"type":"text","text":"injected"},{"type":"future_block","text":{"nested":true}}],"display":false}}`,
		`{"type":"message","id":"h1","parentId":"c1",` + ts + `,"message":{"role":"hookMessage","content":"old"}}`,
		`{"type":"future_kind","id":"f1","parentId":"h1",` + ts + `}`,
		`{"type":"message","id":"u2","parentId":"f1",` + ts + `,"message":{"role":"user","content":"<skill name=\"x\">not a skill block"}}`,
		`{"type":"message","id":"a1","parentId":"u2",` + ts + `,"message":{"role":"assistant","content":[],"stopReason":"aborted"}}`,
	}, "\n") + "\n")
	doc, records := assertContract(t, Pi{}, data)
	if got := strings.Join(ids(records), ","); got != "u1:user:0/user,u1:system:0/system,c1/system,h1/system,f1/system,u2/user,a1/system" {
		t.Fatalf("records = %s", got)
	}
	if records[6].Phase != "aborted" {
		t.Fatalf("a cancelled turn with no message left no record: %+v", records[6])
	}
	if !strings.Contains(string(records[0].Content), `"text":"/skill:lint"`) || bytes.Contains(records[0].Content, []byte("Run the linter")) {
		t.Fatalf("skill block = %s", records[0].Content)
	}
	if records[1].Phase != "meta" || rawString(records[1].Content) != "References are relative to /s/lint.\n\nRun the linter." {
		t.Fatalf("skill file = %+v", records[1])
	}
	if records[2].Phase != "custom" || records[3].Phase != "normalization" || records[4].Phase != "normalization" {
		t.Fatalf("phases: %+v", records)
	}
	if rawString(records[5].Content) != `<skill name="x">not a skill block` {
		t.Fatalf("text that is not a skill block changed: %s", records[5].Content)
	}
	if doc.WarningRecords != 4 || bytes.Contains(goldenRecords(t, records), []byte("AAAA")) {
		t.Fatalf("warnings = %d, or audio bytes kept", doc.WarningRecords)
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
	for _, f := range []string{FormatBsky, FormatClaudeCode, FormatCodex, FormatCopilotCLI, FormatGeminiCLI, FormatPi, FormatPolytoken} {
		if _, ok := r.Lookup(f); !ok {
			t.Fatalf("%s not registered", f)
		}
	}
}
