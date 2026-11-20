package sessioningest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

func TestSubagentSessionReferenceSurvivesSafeEncoding(t *testing.T) {
	const childID = "01900000-0000-7000-8000-000000000005"
	record := fmt.Sprintf(`{"index":0,"id":"subagent:%s","role":"system","session_ref":"%s","content":[{"type":"text","text":"Subagent session"}],"timestamp":"2026-09-20T12:00:00Z"}`+"\n", childID, childID)
	data := fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"01900000-0000-7000-8000-000000000004","revision":"sha256:test","record_count":1,"user_turn_count":0,"content_bytes":%d,"complete":true,"owner":"alice@example.com","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-09-20T12:00:00Z","updated_at":"2026-09-20T12:00:00Z"}`+"\n%s", len(record), record)
	doc, err := parseSessionDocument([]byte(data), Identity{}, "sha256:test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc.Transcript, []byte(`"session_ref":"`+childID+`"`)) {
		t.Fatal("subagent link lost from summarizer representation")
	}
	if !DefaultPolicy().admit("human", "primary", "codex", "", "alice@example.com") || DefaultPolicy().admit("human", "secondary", "codex", "", "alice@example.com") {
		t.Fatal("default retrieval must admit roots and exclude children")
	}
	for _, invalid := range []sessiondecode.Record{{Role: "system", SessionRef: "../bad"}, {Role: "user", SessionRef: childID}} {
		if _, err := sessiondecode.NormalizeBskyRecord(&invalid); err == nil {
			t.Fatal("invalid session reference accepted")
		}
	}
}

func TestNativeLineageDocuments(t *testing.T) {
	directory := os.Getenv("DIDEROT_NATIVE_LINEAGE_DOCUMENTS")
	if directory == "" {
		t.Skip("set DIDEROT_NATIVE_LINEAGE_DOCUMENTS to a directory of the session feed's native lineage acceptance documents")
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.ndjson"))
	if err != nil || len(files) != 4 {
		t.Fatalf("expected four native probe documents: files=%d err=%v", len(files), err)
	}
	children, references := 0, 0
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := parseSessionDocument(body, Identity{}, "", 8<<20)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		admitted := DefaultPolicy().admit(doc.Bsky.OwnerKind, doc.Bsky.Role, doc.Bsky.Source, "", doc.Bsky.Owner)
		if admitted != (doc.Bsky.Lineage == nil) {
			t.Fatal("default retrieval must admit roots and exclude children")
		}
		if doc.Bsky.Lineage != nil {
			children++
			if doc.Bsky.Role != "secondary" {
				t.Fatal("child session has wrong role")
			}
		}
		for _, line := range bytes.Split(doc.Transcript, []byte("\n")) {
			var record sessiondecode.Record
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			if record.SessionRef == "" {
				continue
			}
			references++
		}
	}
	if children != 2 || references != 2 {
		t.Fatalf("children=%d references=%d; want two of each", children, references)
	}
}

func TestLineageCannotClaimPrimaryRole(t *testing.T) {
	const parentID = "01900000-0000-7000-8000-000000000004"
	const childID = "01900000-0000-7000-8000-000000000005"
	for _, role := range []string{"primary", "secondary"} {
		envelope := sessiondecode.BskyEnvelope{Type: "session", Schema: sessiondecode.FormatBsky, SessionID: childID,
			Source: "codex", Owner: "alice@example.com", OwnerKind: "human", Role: role,
			Revision: "sha256:test", Complete: true, StartedAt: "2026-09-20T12:00:00Z", UpdatedAt: "2026-09-20T12:00:00Z",
			Lineage: &sessiondecode.BskyLineage{SessionID: parentID, Kind: "parent"}}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseSessionDocument(encoded, Identity{}, "", 1<<20)
		if role == "primary" && err == nil {
			t.Fatal("lineage-bearing primary was accepted")
		}
		if role == "secondary" && err != nil {
			t.Fatal(err)
		}
		if role == "secondary" {
			for _, alias := range []string{childID, strings.ToUpper(childID), "urn:uuid:" + childID} {
				envelope.Lineage.SessionID = alias
				encoded, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parseSessionDocument(encoded, Identity{}, "", 1<<20); err == nil {
					t.Fatal("self-referencing UUID alias was accepted")
				}
			}
		}
	}
}
