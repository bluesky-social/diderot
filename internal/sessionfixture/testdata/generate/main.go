package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/sessionfixture"
)

type record struct {
	Index     int            `json:"index"`
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Content   []contentBlock `json:"content"`
	Timestamp string         `json:"timestamp"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type fixtureSession struct {
	ID           string
	Owner        string
	Client       string
	Harness      string
	OriginApp    string
	RevisionByte string
	Complete     bool
	StartedAt    string
	UpdatedAt    string
	ChangeSeq    int64
	Title        string
	Privacy      string
	Records      []record
}

func main() {
	output := filepath.Join("testdata", "synthetic-v1")
	if err := os.MkdirAll(filepath.Join(output, "documents"), 0o755); err != nil {
		panic(err)
	}
	fixtures := []fixtureSession{
		{
			ID: "11111111-1111-4111-8111-111111111111", Owner: "alice@example.invalid", Client: "codex", Harness: "codex", OriginApp: "codex",
			RevisionByte: "a", Complete: true, StartedAt: "2026-08-30T18:00:00Z", UpdatedAt: "2026-08-30T18:08:00Z", ChangeSeq: 40,
			Title: "Implement local privacy controls", Privacy: sessionfixture.PrivacyOwnerOnly,
			Records: []record{
				{Index: 0, ID: "codex-user-1", Role: "user", Content: []contentBlock{{Type: "text", Text: "Add per-session privacy controls."}}, Timestamp: "2026-08-30T18:00:00Z"},
				{Index: 1, ID: "codex-assistant-1", Role: "assistant", Content: []contentBlock{{Type: "text", Text: "Implemented the owner-only selection flow."}}, Timestamp: "2026-08-30T18:08:00Z"},
			},
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Owner: "alice@example.invalid", Client: "pi", Harness: "pi", OriginApp: "pi",
			RevisionByte: "b", Complete: false, StartedAt: "2026-08-29T20:00:00Z", UpdatedAt: "2026-08-29T20:01:00Z", ChangeSeq: 30,
			Title: "Investigate a truncated Pi projection", Privacy: sessionfixture.PrivacyTeam,
			Records: []record{
				{Index: 0, ID: "pi-user-1", Role: "user", Content: []contentBlock{{Type: "text", Text: "Why is this projection incomplete?"}}, Timestamp: "2026-08-29T20:00:00Z"},
			},
		},
		{
			ID: "33333333-3333-4333-8333-333333333333", Owner: "bob@example.invalid", Client: "claude-code", Harness: "claude-code", OriginApp: "claude-code",
			RevisionByte: "c", Complete: true, StartedAt: "2026-08-28T16:00:00Z", UpdatedAt: "2026-08-28T16:15:00Z", ChangeSeq: 20,
			Title: "Review session fixture validation", Privacy: sessionfixture.PrivacyTeam,
			Records: []record{
				{Index: 0, ID: "claude-user-1", Role: "user", Content: []contentBlock{{Type: "text", Text: "Review the shared fixture format."}}, Timestamp: "2026-08-28T16:00:00Z"},
				{Index: 1, ID: "claude-assistant-1", Role: "assistant", Content: []contentBlock{{Type: "text", Text: "The manifest and document agree."}}, Timestamp: "2026-08-28T16:15:00Z"},
			},
		},
		{
			ID: "44444444-4444-4444-8444-444444444444", Owner: "bob@example.invalid", Client: "pi", Harness: "pi", OriginApp: "pi",
			RevisionByte: "d", Complete: true, StartedAt: "2026-08-27T14:00:00Z", UpdatedAt: "2026-08-27T14:12:00Z", ChangeSeq: 10,
			Title: "Verify older session ordering", Privacy: sessionfixture.PrivacyOwnerOnly,
			Records: []record{
				{Index: 0, ID: "pi-user-2", Role: "user", Content: []contentBlock{{Type: "text", Text: "Keep newest sessions first."}}, Timestamp: "2026-08-27T14:00:00Z"},
				{Index: 1, ID: "pi-assistant-2", Role: "assistant", Content: []contentBlock{{Type: "text", Text: "Ordering verified."}}, Timestamp: "2026-08-27T14:12:00Z"},
			},
		},
	}

	manifest := sessionfixture.Manifest{
		SchemaVersion:   sessionfixture.SchemaV1,
		CapturedAt:      "2026-08-31T22:00:00Z",
		SourceRevisions: sessionfixture.SourceRevisions{Diderot: "synthetic-v1"},
		AllowedPersonas: []string{"alice@example.invalid", "bob@example.invalid"},
	}
	for _, fixture := range fixtures {
		revision := "sha256:" + strings.Repeat(fixture.RevisionByte, 64)
		document, recordCount, userTurns, contentBytes := makeDocument(fixture, revision)
		stored := gzipBytes(document)
		path := filepath.ToSlash(filepath.Join("documents", fixture.ID+".ndjson.gz"))
		if err := os.WriteFile(filepath.Join(output, filepath.FromSlash(path)), stored, 0o644); err != nil {
			panic(err)
		}
		manifest.Sessions = append(manifest.Sessions, sessionfixture.Session{
			SessionID: fixture.ID, Owner: fixture.Owner, Client: fixture.Client, Harness: fixture.Harness,
			OwnerKind: "human", SessionRole: "primary", OriginApp: fixture.OriginApp,
			Revision: revision, RewriteEpoch: 1, Complete: fixture.Complete,
			StartedAt: fixture.StartedAt, UpdatedAt: fixture.UpdatedAt, ChangeSeq: fixture.ChangeSeq,
			RecordCount: recordCount, UserTurnCount: userTurns, ContentBytes: contentBytes,
			Title: fixture.Title, Privacy: fixture.Privacy,
			Document: sessionfixture.Object{
				Path: path, MediaType: sessionfixture.SessionMediaType, Revision: revision,
				StoredSHA256: digest(stored), UncompressedSHA256: digest(document),
			},
		})
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		panic(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), encoded, 0o644); err != nil {
		panic(err)
	}
}

func makeDocument(fixture fixtureSession, revision string) ([]byte, int, int, int64) {
	var records bytes.Buffer
	userTurns := 0
	for _, record := range fixture.Records {
		if record.Role == "user" {
			userTurns++
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			panic(err)
		}
		records.Write(encoded)
		records.WriteByte('\n')
	}
	envelope := map[string]any{
		"type": "session", "schema": "bsky.session.v1", "session_id": fixture.ID,
		"source": fixture.Client, "owner": fixture.Owner, "owner_kind": "human",
		"session_role": "primary", "origin_app": fixture.OriginApp,
		"revision": revision, "rewrite_epoch": int64(1), "record_count": len(fixture.Records),
		"user_turn_count": userTurns, "content_bytes": int64(records.Len()),
		"started_at": fixture.StartedAt, "updated_at": fixture.UpdatedAt, "complete": fixture.Complete,
		"accounting": map[string]any{"invocations": len(fixture.Records)},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	document := append(encoded, '\n')
	document = append(document, records.Bytes()...)
	return document, len(fixture.Records), userTurns, int64(records.Len())
}

func gzipBytes(data []byte) []byte {
	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.BestCompression)
	if err != nil {
		panic(err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return output.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func init() {
	if len(os.Args) != 1 {
		panic(fmt.Sprintf("unexpected arguments: %q", os.Args[1:]))
	}
}
