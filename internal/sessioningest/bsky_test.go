package sessioningest

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestParseSessionDocument_Valid(t *testing.T) {
	records := `{"index":0,"id":"rec0","role":"user","content":"hello","timestamp":"2026-07-24T18:00:00Z"}
{"index":1,"id":"rec1","role":"assistant","content":{"type":"text","text":"hi"},"timestamp":"2026-07-24T18:01:00Z"}
`
	body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":2,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}
%s`, len(records), records))
	doc, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if doc.RecordCount != 2 {
		t.Fatalf("record_count = %d, want 2", doc.RecordCount)
	}
}

func TestParseSessionDocument_RejectsMissingOrReversedSourceTimes(t *testing.T) {
	record := `{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}` + "\n"
	for name, times := range map[string]string{
		"missing start": `"updated_at":"2026-07-24T18:42:00Z"`,
		"reversed":      `"started_at":"2026-07-24T19:00:00Z","updated_at":"2026-07-24T18:42:00Z"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":1,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary",%s}
%s`, len(record), times, record))
			if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
				t.Fatal("invalid source times must fail")
			}
		})
	}
}

func TestParseSessionDocument_RejectsMismatchedRevision(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:OTHER","complete":true,"owner":"a","source":"codex"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("mismatched revision should fail")
	}
}

func TestParseSessionDocument_UnsupportedRecordsBecomeNotices(t *testing.T) {
	for _, record := range []string{
		`{"index":1,"id":"r1","role":"mystery","content":"private unsupported content","tool_calls":[{"id":"future","name":"private unsupported content","args":"private unsupported content"}],"tool_call_id":"private unsupported content","model":"private unsupported content","provider_model":"private unsupported content","invocation_id":"private unsupported content","session_ref":"private unsupported content","timestamp_source":"private unsupported content","timestamp":"2026-07-24T18:01:00Z"}`,
		`{"index":1,"id":"r1","role":"assistant","content":[{"type":"text","text":"known middle"},{"type":"future_block","data":"private unsupported content"}],"timestamp":"2026-07-24T18:01:00Z"}`,
		`{"index":1,"id":"r1","role":"assistant","content":[{"type":"text","text":"known middle"},42],"timestamp":"2026-07-24T18:01:00Z"}`,
	} {
		records := `{"index":0,"id":"r0","role":"user","content":"before","timestamp":"2026-07-24T18:00:00Z"}` + "\n" + record + "\n" +
			`{"index":2,"id":"r2","role":"assistant","content":"after","timestamp":"2026-07-24T18:02:00Z"}` + "\n"
		body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":3,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}`+"\n%s", len(records), records))
		document, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0)
		if err != nil {
			t.Fatal(err)
		}
		if document.RecordCount != 3 || document.WarningRecords != 1 || !bytes.Contains(document.Transcript, []byte("before")) || !bytes.Contains(document.Transcript, []byte("after")) {
			t.Fatalf("unsupported record interrupted coverage: records=%d warnings=%d", document.RecordCount, document.WarningRecords)
		}
		if bytes.Contains(document.Transcript, []byte("private unsupported content")) {
			t.Fatal("unsupported content leaked into a model input")
		}
	}
}

func TestParseSessionDocument_ByteLimit(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a","source":"codex"}
{"index":0,"id":"r","role":"user","content":"this is a long-ish body for byte-limit","timestamp":"2026-07-24T18:00:00Z"}
`)
	// cap far below the body size must reject
	if _, err := parseSessionDocument(body, Identity{Owner: "a", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 10); err == nil {
		t.Fatal("byte cap should reject oversized document")
	}
}

func TestParseSessionDocument_RejectsIncomplete(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":false,"owner":"a","source":"codex"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("incomplete session must be rejected")
	}
}

func TestParseSessionDocument_RejectsSessionIDMismatch(t *testing.T) {
	records := `{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`
	body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_OTHER","revision":"sha256:abc","record_count":1,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}
%s`, len(records), records))
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("session_id mismatch must fail")
	}
}

func TestParseSessionDocument_OwnerClientAreProvenanceNotIdentity(t *testing.T) {
	records := `{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`
	body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":1,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"b@y","owner_kind":"human","source":"claude-code","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}
%s`, len(records), records))
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err != nil {
		t.Fatalf("owner/client mismatch must be tolerated (provenance, not identity): %v", err)
	}
}

func TestParseSessionDocument_RejectsMissingSource(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("missing source must fail")
	}
}

func TestParseSessionDocument_RejectsNonContiguousIndex(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","source":"codex","updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"id":"r0","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
{"index":2,"id":"r1","role":"user","content":"x","timestamp":"2026-07-24T18:01:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("non-contiguous record index must fail")
	}
}

func TestParseSessionDocument_RejectsMalformedTimestamp(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","source":"codex","updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"not-a-time"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("malformed timestamp must fail")
	}
}

func TestParseSessionDocument_RejectsMissingRecordID(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","source":"codex","updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("missing record ID must fail")
	}
}

func TestParseSessionDocument_RejectsTrailingGarbage(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","source":"codex","updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
this is not json
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("trailing malformed line must fail")
	}
}

func TestParseSessionDocument_RejectsEnvelopeCounterMismatch(t *testing.T) {
	body := []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","complete":true,"owner":"a@x","source":"codex","record_count":5,"user_turn_count":1,"updated_at":"2026-07-24T18:42:00Z"}
{"index":0,"id":"r","role":"user","content":"x","timestamp":"2026-07-24T18:00:00Z"}
`)
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil {
		t.Fatal("envelope record_count vs parsed records mismatch must fail")
	}
}

func TestParseSessionDocument_ReencodesAllowlistedSummaryInput(t *testing.T) {
	record := `{"index":0,"id":"r","role":"assistant","content":[{"type":"text","text":"safe","future_secret":"do-not-send"},{"type":"image","alt":"redacted caption","asset_id":"img_1","sha256":"sha256:abc","available":false,"future_secret":"also-do-not-send"}],"timestamp":"2026-07-24T18:00:00Z","model":"safe-model","future_record_secret":"never-send"}` + "\n"
	body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":1,"user_turn_count":0,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}
%s`, len(record), record))
	doc, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	safe := string(doc.Transcript)
	for _, forbidden := range []string{"future_secret", "future_record_secret", "do-not-send", "also-do-not-send"} {
		if strings.Contains(safe, forbidden) {
			t.Fatalf("safe summary line leaked %q: %s", forbidden, safe)
		}
	}
	for _, want := range []string{`"text":"safe"`, `"alt":"redacted caption"`, `"asset_id":"img_1"`, `"sha256":"sha256:abc"`} {
		if !strings.Contains(safe, want) {
			t.Fatalf("safe summary line missing %q: %s", want, safe)
		}
	}
}

func TestParseSessionDocument_RejectsInlineImagePayloads(t *testing.T) {
	record := `{"index":0,"id":"r","role":"user","content":[{"type":"image","asset_id":"img_1","data":"base64-secret"}],"timestamp":"2026-07-24T18:00:00Z"}` + "\n"
	body := []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":1,"user_turn_count":1,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}
%s`, len(record), record))
	if _, err := parseSessionDocument(body, Identity{Owner: "a@x", Client: "codex", SessionID: "ses_1"}, "sha256:abc", 0); err == nil || !strings.Contains(err.Error(), "forbidden inline field") {
		t.Fatalf("inline image payload error = %v", err)
	}
}
