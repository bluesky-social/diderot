package sessiondecode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestATIFFixture(t *testing.T) {
	doc, records := assertFixture(t, ATIF{}, "atif", "basic")
	want := []string{
		"step:1/system",
		"u-0001/user",
		"step:3:reasoning:0/reasoning",
		"step:3:assistant:1/assistant",
		"step:3:observation:2/tool",
		"step:4:assistant:0/assistant",
		"step:4:observation:1/tool",
		"step:4:observation:2/tool",
		"step:5/system",
		"a-0006/assistant",
		"step:7:assistant:0/assistant",
		"step:7:observation:1/system",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		want := FidelityPositional
		if r.ID == "u-0001" || r.ID == "a-0006" {
			want = FidelityNative
		}
		if r.Fidelity != want {
			t.Fatalf("%s fidelity %s, want %s", r.ID, r.Fidelity, want)
		}
	}
	if doc.SourceVersion != "ATIF-v1.8" || doc.Harness != "terminus-2" || doc.UserTurnCount != 1 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	// Step 1 has no timestamp: it takes the first one in the trajectory.
	if records[0].Timestamp != "2026-11-01T12:00:00Z" || records[5].Timestamp != "2026-11-01T12:00:05Z" {
		t.Fatalf("filled timestamps: %s, %s", records[0].Timestamp, records[5].Timestamp)
	}
	if records[8].Phase != "compaction" || records[3].ToolCalls[0].Args != `{"cmd":"make"}` || records[3].Model != "synthetic-model-1" || records[5].Model != "synthetic-model-default" {
		t.Fatal("compaction marker, tool arguments, or model lost")
	}
	if bytes.Contains(goldenRecords(t, records), []byte("traj-verify")) == false {
		t.Fatal("subagent reference lost")
	}
}

func TestATIFEdgeCases(t *testing.T) {
	// A subagent reference survives beside result content.
	data := `{"schema_version":"ATIF-v1.8","agent":{},"steps":[{"step_id":1,"source":"agent","message":"x","timestamp":"2026-11-01T12:00:00Z",
		"tool_calls":[{"tool_call_id":"c","function_name":"delegate","arguments":{}}],
		"observation":{"results":[{"source_call_id":"c","content":"done","subagent_trajectory_ref":[{"trajectory_id":"sub-1"}]}]}}]}`
	_, records := assertContract(t, ATIF{}, []byte(data))
	if got := string(records[len(records)-1].Content); !strings.Contains(got, "done") || !strings.Contains(got, "sub-1") {
		t.Fatalf("result content = %s", got)
	}
	// A timestamp on a step that emits nothing still dates its neighbours.
	data = `{"schema_version":"ATIF-v1.8","agent":{},"steps":[{"step_id":1,"source":"user","message":"q"},
		{"step_id":2,"source":"agent","message":"","timestamp":"2026-11-01T12:00:07Z"},
		{"step_id":3,"source":"user","message":"again"}]}`
	_, records = assertContract(t, ATIF{}, []byte(data))
	if records[0].Timestamp != "2026-11-01T12:00:07Z" || records[1].Timestamp != "2026-11-01T12:00:07Z" {
		t.Fatalf("timestamps = %s, %s", records[0].Timestamp, records[1].Timestamp)
	}
	// Trailing delimiters are not a trajectory.
	for _, suffix := range []string{"}", "]", " {}"} {
		bad := `{"schema_version":"ATIF-v1.8","agent":{},"steps":[{"step_id":1,"source":"user","message":"q","timestamp":"2026-11-01T12:00:00Z"}]}` + suffix
		if _, _, err := DecodeAll(ATIF{}, []byte(bad)); !errors.Is(err, ErrStructural) {
			t.Fatalf("suffix %q accepted: %v", suffix, err)
		}
	}
}

func TestSTSFixture(t *testing.T) {
	doc, records := assertFixture(t, STS{}, "sts", "basic")
	want := []string{
		"line:2/system",
		"line:3/user",
		"line:4:reasoning:0/reasoning",
		"line:4:assistant:1/assistant",
		"line:5/tool",
		"line:6/assistant",
		"line:7/system",
	}
	if got := ids(records); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range records {
		if r.Fidelity != FidelityPositional {
			t.Fatalf("%s fidelity %s; STS ids are positional only", r.ID, r.Fidelity)
		}
	}
	if doc.SourceVersion != "" || doc.Harness != "my-agent" || doc.UserTurnCount != 1 || doc.WarningRecords != 1 {
		t.Fatalf("document = %+v", doc)
	}
	if records[0].Timestamp != "2026-11-01T12:00:00Z" || records[4].Timestamp != "2026-11-01T12:00:01Z" || records[5].Timestamp != "2026-11-01T12:00:02.5Z" {
		t.Fatalf("timestamps: %s %s %s", records[0].Timestamp, records[4].Timestamp, records[5].Timestamp)
	}
}

func TestInterchangeDecodersRejectStructuralViolations(t *testing.T) {
	for name, c := range map[string]struct {
		d    Decoder
		data string
	}{
		"atif not v1":            {ATIF{}, `{"schema_version":"ATIF-v2.0","agent":{},"steps":[{"step_id":1,"source":"user","message":"x","timestamp":"2026-11-01T12:00:00Z"}]}`},
		"atif step gap":          {ATIF{}, `{"schema_version":"ATIF-v1.6","agent":{},"steps":[{"step_id":2,"source":"user","message":"x","timestamp":"2026-11-01T12:00:00Z"}]}`},
		"atif unknown source":    {ATIF{}, `{"schema_version":"ATIF-v1.6","agent":{},"steps":[{"step_id":1,"source":"robot","message":"x","timestamp":"2026-11-01T12:00:00Z"}]}`},
		"atif orphan result":     {ATIF{}, `{"schema_version":"ATIF-v1.6","agent":{},"steps":[{"step_id":1,"source":"agent","message":"x","timestamp":"2026-11-01T12:00:00Z","observation":{"results":[{"source_call_id":"nope","content":"y"}]}}]}`},
		"atif no timestamps":     {ATIF{}, `{"schema_version":"ATIF-v1.6","agent":{},"steps":[{"step_id":1,"source":"user","message":"x"}]}`},
		"atif trailing data":     {ATIF{}, `{"schema_version":"ATIF-v1.6","agent":{},"steps":[]} {}`},
		"sts no header":          {STS{}, `{"type":"message","message":{"role":"user","content":"x","timestamp":1}}` + "\n"},
		"sts orphan tool":        {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message","message":{"role":"tool","toolCallId":"t9","content":"x","timestamp":1}}` + "\n"},
		"sts no timestamps":      {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message","message":{"role":"user","content":"x"}}` + "\n"},
		"sts call without id":    {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message","message":{"role":"assistant","content":"","toolCalls":[{"function":{"name":"f","arguments":"{}"}}],"timestamp":1}}` + "\n"},
		"sts message missing":    {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message"}` + "\n"},
		"sts timestamp overflow": {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message","message":{"role":"user","content":"x","timestamp":1e20}}` + "\n"},
		"sts negative timestamp": {STS{}, `{"type":"session","harness":"h","id":"i"}` + "\n" + `{"type":"message","message":{"role":"user","content":"x","timestamp":-5}}` + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeAll(c.d, []byte(c.data)); !errors.Is(err, ErrStructural) {
				t.Fatalf("err = %v, want ErrStructural", err)
			}
		})
	}
}
