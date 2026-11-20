package sessiondecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/sessionfixture"
)

// assertContract checks the decoder contract every launch decoder shares on
// one accepted document and returns its records.
func assertContract(t *testing.T, d Decoder, data []byte) (Document, []Record) {
	t.Helper()
	doc, records, err := DecodeAll(d, data)
	if err != nil {
		t.Fatalf("%s decode: %v", d.Format(), err)
	}
	again, recordsAgain, err := DecodeAll(d, bytes.Clone(data))
	if err != nil || !reflect.DeepEqual(doc, again) || !reflect.DeepEqual(records, recordsAgain) {
		t.Fatalf("%s: decoding the same bytes twice differed (err=%v)", d.Format(), err)
	}
	if doc.Format != d.Format() || doc.DecoderVersion != d.Version() {
		t.Fatalf("document names %s@%s, decoder is %s@%s", doc.Format, doc.DecoderVersion, d.Format(), d.Version())
	}
	if doc.RecordCount != len(records) {
		t.Fatalf("record_count=%d but emitted %d", doc.RecordCount, len(records))
	}
	if doc.UpdatedAt.Before(doc.StartedAt) || doc.StartedAt.IsZero() {
		t.Fatalf("source times %v..%v", doc.StartedAt, doc.UpdatedAt)
	}
	ids := map[string]bool{}
	calls := map[string]bool{}
	users := 0
	for i, r := range records {
		if r.Index != i {
			t.Fatalf("record %d has index %d", i, r.Index)
		}
		if r.ID == "" || ids[r.ID] {
			t.Fatalf("record %d id %q is empty or duplicated", i, r.ID)
		}
		ids[r.ID] = true
		if r.Fidelity != FidelityNative && r.Fidelity != FidelityPositional {
			t.Fatalf("record %s has fidelity %q", r.ID, r.Fidelity)
		}
		if parsed, err := time.Parse(time.RFC3339, r.Timestamp); err != nil || !parsed.Equal(r.Time) {
			t.Fatalf("record %s timestamp %q does not match %v", r.ID, r.Timestamp, r.Time)
		}
		if !knownRoles[r.Role] {
			t.Fatalf("record %s has role %q", r.ID, r.Role)
		}
		if r.Role == "user" {
			users++
		}
		for _, c := range r.ToolCalls {
			calls[c.ID] = true
		}
		if r.Role == "tool" && !calls[r.ToolCallID] {
			t.Fatalf("tool record %s answers unknown call %q", r.ID, r.ToolCallID)
		}
		line, err := json.Marshal(r)
		if err != nil || !json.Valid(line) {
			t.Fatalf("record %s does not encode: %v", r.ID, err)
		}
	}
	if users != doc.UserTurnCount {
		t.Fatalf("user_turn_count=%d but emitted %d user records", doc.UserTurnCount, users)
	}
	return doc, records
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(Bsky{})
	if d, ok := r.Lookup(FormatBsky); !ok || d.Format() != FormatBsky {
		t.Fatal("bsky decoder not registered")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unknown format resolved")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate format did not panic")
		}
	}()
	NewRegistry(Bsky{}, Bsky{})
}

func TestBskySyntheticFixturesSatisfyContract(t *testing.T) {
	bundle, err := sessionfixture.Load(filepath.Join("..", "sessionfixture", "testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range bundle.Manifest.Sessions {
		if !session.Complete {
			continue
		}
		doc, records := assertContract(t, Bsky{}, bundle.Documents[session.SessionID])
		if doc.Bsky == nil || doc.Bsky.SessionID != session.SessionID || doc.Bsky.Revision != session.Revision {
			t.Fatalf("%s: envelope %+v", session.SessionID, doc.Bsky)
		}
		for _, r := range records {
			if r.Fidelity != FidelityNative {
				t.Fatalf("bsky record %s fidelity %q, want native", r.ID, r.Fidelity)
			}
		}
	}
}

func bskyDoc(records ...string) []byte {
	body := strings.Join(records, "\n") + "\n"
	users := strings.Count(body, `"role":"user"`)
	return []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses_1","revision":"sha256:abc","record_count":%d,"user_turn_count":%d,"content_bytes":%d,"complete":true,"owner":"a@x","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}`+"\n%s",
		len(records), users, len(body), body))
}

// TestBskyDecodeEnforcesContractButFeedPathIsUnchanged: the generic entry
// point rejects duplicate ids and orphaned tool results; the feed's pinned
// path keeps accepting what it accepted before decoders existed.
func TestBskyDecodeEnforcesContractButFeedPathIsUnchanged(t *testing.T) {
	for name, data := range map[string][]byte{
		"duplicate id": bskyDoc(
			`{"index":0,"id":"r","role":"user","content":"a","timestamp":"2026-07-24T18:00:00Z"}`,
			`{"index":1,"id":"r","role":"assistant","content":"b","timestamp":"2026-07-24T18:01:00Z"}`),
		"orphan tool result": bskyDoc(
			`{"index":0,"id":"r0","role":"user","content":"a","timestamp":"2026-07-24T18:00:00Z"}`,
			`{"index":1,"id":"r1","role":"tool","tool_call_id":"nope","content":"b","timestamp":"2026-07-24T18:01:00Z"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeAll(Bsky{}, data); !errors.Is(err, ErrStructural) {
				t.Fatalf("Decode err = %v, want ErrStructural", err)
			}
			if _, err := (Bsky{}).DecodePinned(data, "ses_1", "sha256:abc", func(Record) error { return nil }); err != nil {
				t.Fatalf("feed path now rejects a document it accepted before: %v", err)
			}
		})
	}
	paired := bskyDoc(
		`{"index":0,"id":"r0","role":"assistant","tool_calls":[{"id":"c1","name":"Bash","args":"{}"}],"timestamp":"2026-07-24T18:00:00Z"}`,
		`{"index":1,"id":"r1","role":"tool","tool_call_id":"c1","content":"ok","timestamp":"2026-07-24T18:01:00Z"}`)
	assertContract(t, Bsky{}, paired)
}

func TestBskyEmitErrorAborts(t *testing.T) {
	bundle, err := sessionfixture.Load(filepath.Join("..", "sessionfixture", "testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	_, err = Bsky{}.Decode(bundle.Documents[bundle.Manifest.Sessions[0].SessionID], func(Record) error { return stop })
	if !errors.Is(err, stop) || errors.Is(err, ErrStructural) {
		t.Fatalf("emit error = %v, want stop unclassified", err)
	}
}

// TestBskyErrorsAreStructuralAndKeepLegacyWording: every decoder-originated
// failure of the generic entry point is ErrStructural, while the feed path's
// messages keep the type names they always reported.
func TestBskyErrorsAreStructuralAndKeepLegacyWording(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"bad json":  []byte("{"),
		"bad index": bskyDoc(`{"index":"bad","id":"r","role":"user","content":"a","timestamp":"2026-07-24T18:00:00Z"}`),
		"no id":     bskyDoc(`{"index":0,"role":"user","content":"a","timestamp":"2026-07-24T18:00:00Z"}`),
	} {
		if _, _, err := DecodeAll(Bsky{}, data); !errors.Is(err, ErrStructural) {
			t.Errorf("%s: err = %v, want ErrStructural", name, err)
		}
	}
	_, err := Bsky{}.DecodePinned(bskyDoc(`{"index":"bad","id":"r","role":"user","content":"a","timestamp":"2026-07-24T18:00:00Z"}`), "", "", func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Go struct field sessionRecord.index") {
		t.Fatalf("record type error = %v", err)
	}
	_, err = Bsky{}.DecodePinned(bskyDoc(`{"index":0,"id":"r","role":"assistant","tool_calls":[{"id":7}],"timestamp":"2026-07-24T18:00:00Z"}`), "", "", func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Go struct field sessionRecord.tool_calls.0.id") {
		t.Fatalf("nested tool call type error = %v", err)
	}
	_, err = Bsky{}.DecodePinned([]byte(`{"lineage":{"kind":7}}`), "", "", func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Go struct field sessionEnvelope.lineage.kind") {
		t.Fatalf("nested lineage type error = %v", err)
	}
	_, err = Bsky{}.DecodePinned([]byte(`{"record_count":"x"}`), "", "", func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Go struct field sessionEnvelope.record_count") {
		t.Fatalf("envelope type error = %v", err)
	}
}
