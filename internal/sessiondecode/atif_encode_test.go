package sessiondecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// normalContent is record content as ATIF carries it: no content and an
// empty string are the same message.
func normalContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == `""` {
		return ""
	}
	return string(raw)
}

// TestEncodeATIFRoundTrips: a harness's own session, converted to ATIF, is
// accepted by the ATIF decoder (what the server runs on an atif-json push)
// and decodes to the records the harness's decoder produced.
func TestEncodeATIFRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		decoder Decoder
		fixture string
	}{
		{ClaudeCode{}, "claude-code/basic.jsonl"},
		{Codex{}, "codex/basic.jsonl"},
		{CopilotCLI{}, "copilot-cli/basic.jsonl"},
		{GeminiCLI{}, "gemini-cli/basic.jsonl"},
		{Pi{}, "pi/basic.jsonl"},
		{Polytoken{}, "polytoken/basic.jsonl"},
	} {
		t.Run(tc.decoder.Format(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			doc, want, err := DecodeAll(tc.decoder, data)
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "7b1e9a52-3c4d-4e5f-8a6b-7c8d9e0f1a2b"
			encoded, err := EncodeATIF(sessionID, doc, want)
			if err != nil {
				t.Fatal(err)
			}
			again, err := EncodeATIF(sessionID, doc, want)
			if err != nil || !bytes.Equal(encoded, again) {
				t.Fatalf("encoding the same records twice differed (err=%v)", err)
			}
			gotDoc, got := assertContract(t, ATIF{}, encoded)
			if len(got) != len(want) {
				t.Fatalf("%d records came back, want %d", len(got), len(want))
			}
			for i, w := range want {
				g := got[i]
				if w.Fidelity == FidelityNative {
					if g.ID != w.ID || g.Fidelity != FidelityNative {
						t.Errorf("record %d: id %q (%s), want native %q", i, g.ID, g.Fidelity, w.ID)
					}
				} else if !strings.HasPrefix(g.ID, "step:") || g.Fidelity != FidelityPositional {
					t.Errorf("record %d: positional id %q came back as %q (%s)", i, w.ID, g.ID, g.Fidelity)
				}
				if g.Role != w.Role || g.Model != w.Model || g.ToolCallID != w.ToolCallID || !g.Time.Equal(w.Time) {
					t.Errorf("record %d: got %s/%s/%s/%v, want %s/%s/%s/%v", i, g.Role, g.Model, g.ToolCallID, g.Time, w.Role, w.Model, w.ToolCallID, w.Time)
				}
				if normalContent(g.Content) != normalContent(w.Content) {
					t.Errorf("record %d: content %s, want %s", i, g.Content, w.Content)
				}
				if w.Role == "system" && g.Phase != w.Phase {
					t.Errorf("record %d: phase %q, want %q", i, g.Phase, w.Phase)
				}
				if len(g.ToolCalls) != len(w.ToolCalls) {
					t.Fatalf("record %d: %d tool calls, want %d", i, len(g.ToolCalls), len(w.ToolCalls))
				}
				for j, call := range w.ToolCalls {
					wantArgs := compactJSON(atifArguments(call.Args))
					if g.ToolCalls[j].ID != call.ID || g.ToolCalls[j].Name != call.Name || g.ToolCalls[j].Args != wantArgs {
						t.Errorf("record %d call %d: %+v, want %+v with args %s", i, j, g.ToolCalls[j], call, wantArgs)
					}
				}
			}
			if gotDoc.Harness != doc.Harness || gotDoc.UserTurnCount != doc.UserTurnCount ||
				!gotDoc.StartedAt.Equal(doc.StartedAt) || !gotDoc.UpdatedAt.Equal(doc.UpdatedAt) {
				t.Fatalf("document = %+v, want the harness, turns and times of %+v", gotDoc, doc)
			}
			wantVersion := doc.SourceVersion
			if wantVersion == "" {
				wantVersion = "unknown"
			}
			if !bytes.Contains(encoded, []byte(`"version":"`+wantVersion+`"`)) {
				t.Fatalf("the harness version %q is not in the trajectory", wantVersion)
			}
		})
	}
}

func TestEncodeATIFShapes(t *testing.T) {
	codex := []byte(`{"timestamp":"2026-11-01T10:00:00Z","type":"session_meta","payload":{"cli_version":"0.164.0"}}
{"timestamp":"2026-11-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"patch it"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}
{"timestamp":"2026-11-01T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}}
{"timestamp":"2026-11-01T10:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}}
`)
	doc, records, err := DecodeAll(Codex{}, codex)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeATIF("7b1e9a52-3c4d-4e5f-8a6b-7c8d9e0f1a2b", doc, records)
	if err != nil {
		t.Fatal(err)
	}
	_, got := assertContract(t, ATIF{}, encoded)
	if len(got) != 3 {
		t.Fatalf("records = %v", ids(got))
	}
	// The image keeps its placeholder, and no image bytes travel.
	if normalContent(got[0].Content) != normalContent(records[0].Content) || bytes.Contains(encoded, []byte("AAAA")) {
		t.Fatalf("image content = %s", got[0].Content)
	}
	// Arguments that are not a JSON object are carried whole under "input".
	if got[1].ToolCalls[0].Args != `{"input":"*** Begin Patch\n*** End Patch"}` {
		t.Fatalf("args = %s", got[1].ToolCalls[0].Args)
	}
	// The first record's id was the rollout line; it is now the step ordinal.
	if got[0].ID != "step:1" || got[1].ID != "call_1:call" || got[2].ID != "call_1:output" {
		t.Fatalf("ids = %v", ids(got))
	}

	if _, err := EncodeATIF("s", doc, nil); err == nil {
		t.Fatal("a session with no records encoded")
	}
	if _, err := EncodeATIF("s", doc, []Record{{ID: "x", Role: "narrator", Time: records[0].Time}}); err == nil {
		t.Fatal("an unknown role encoded")
	}
}
