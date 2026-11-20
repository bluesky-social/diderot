package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestUpsertSanitizesBeforeValidate pins the order: Upsert sanitizes
// before validate() runs. In the other order an Event whose Body is
// nothing but a NUL byte passes validate() (Body != ""), is then sanitized
// to "", and fails deep inside upsertTx with "events: chunker produced
// zero chunks" — a second, undocumented spelling of a permanent failure.
// Sanitizing first makes this case the ordinary, connector-recognizable
// "Body is required" validation error.
//
// This only decides which error comes back; it is still an error (nothing
// readable survives a NUL-only body). The companion guard is each
// connector's own "nothing to index" check — see
// internal/connector/discord/nulbyte_test.go,
// internal/connector/notion/nulbyte_test.go, and the NUL-only-text case in
// internal/connector/slack/parse.go — so that a connector never calls
// Upsert with such a body in the first place.
func TestUpsertSanitizesBeforeValidate(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	_, err := sink.Upsert(ctx, Event{
		Source: "proptest", ExternalID: "nul-only-body",
		OccurredAt: time.Unix(1_700_000_000, 0),
		Body:       "\x00\x00\x00",
		Scope:      ScopePublic,
	})
	if err == nil {
		t.Fatal("Upsert with a NUL-only body: want an error (nothing left to index), got nil")
	}
	if !strings.Contains(err.Error(), "Body is required") {
		t.Fatalf("Upsert with a NUL-only body: err = %q, want it to contain %q (the ordinary validate() error)\n"+
			"a different message (e.g. \"chunker produced zero chunks\") means validate() ran before "+
			"sanitizing again", err.Error(), "Body is required")
	}
}

// TestReconcileDocumentsSanitizesFilename pins that ReconcileDocuments
// sanitizes DocumentRef.Filename before writing event_documents.filename.
// Discord and Zendesk both call ReconcileDocuments right after Sink.Upsert
// and return its error up the paths that withhold their cursor and
// FullReadAt, so an unsanitized attachment filename carrying a NUL byte
// would stall them exactly as an unsanitized event body would — just one
// field later in the same write.
func TestReconcileDocumentsSanitizesFilename(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "F1", Filename: "report\x00.pdf", Mimetype: "application/pdf\x00", DocFormat: "pdf"},
	})
	if err != nil {
		t.Fatalf("ReconcileDocuments with a NUL byte in Filename/Mimetype: unexpected error: %v", err)
	}

	var filename, mediaType string
	if err := pool.QueryRow(ctx, `
		SELECT filename, media_type FROM event_documents WHERE event_id = $1 AND source_file_id = 'F1'
	`, eventID).Scan(&filename, &mediaType); err != nil {
		t.Fatal(err)
	}
	if filename != "report.pdf" {
		t.Errorf("filename = %q, want %q (NUL byte stripped)", filename, "report.pdf")
	}
	if mediaType != "application/pdf" {
		t.Errorf("media_type = %q, want %q (NUL byte stripped)", mediaType, "application/pdf")
	}
}

// TestSanitizeRawJSONKeyCollisionIsDeterministic: object keys that
// collide after NUL-stripping (e.g. "a" and "a\x00", both -> "a") must not
// resolve in Go's randomized map iteration order, or the same input could
// sanitize to a different result from one call to the next. Re-ingesting an otherwise-unchanged
// item could then flip which value survived the collision, making
// raw::text look different on every sync and churning a raw-only
// UPDATE (and the connector's "wrote" observer) indefinitely.
func TestSanitizeRawJSONKeyCollisionIsDeterministic(t *testing.T) {
	raw := json.RawMessage(`{"a\u0000":"first","a":"second"}`)
	var want json.RawMessage
	for i := 0; i < 20; i++ {
		got := SanitizeJSON(raw)
		if i == 0 {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("SanitizeJSON(%s) is nondeterministic: run 0 = %s, run %d = %s", raw, want, i, got)
		}
	}
	// The lexicographically-last original key ("a\x00", since a proper
	// prefix of a string sorts before it, and "a\x00" is "a" plus a
	// trailing byte) wins: "first" survives, not "second".
	var decoded map[string]string
	if err := json.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("unmarshal sanitized raw: %v", err)
	}
	if decoded["a"] != "first" {
		t.Fatalf("sanitized raw = %s, want {\"a\":\"first\"} (lexicographically-last original key wins)", want)
	}
}

// TestSanitizeRawJSONRejectsTrailingData: json.Decoder.Decode reads only
// the first JSON value and silently ignores anything after it, so without
// an explicit check a Raw with trailing garbage — which Postgres's jsonb
// parser would have rejected outright — comes back silently truncated to just the
// first value instead of falling back to the original bytes (and letting
// Postgres reject it as before). Two shapes of trailing data are covered:
// a second well-formed JSON value, and a lone closing delimiter left over
// from a caller that double-wrapped a document — the latter is the gap an
// earlier version of this function had, because dec.More() reports false
// for a stray `}` or `]` (it isn't the start of another value), so that
// case slipped through as if the input were clean.
func TestSanitizeRawJSONRejectsTrailingData(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"a":"b"}{"trailing":"garbage"}`),
		json.RawMessage(`{"a":"b"}}`),
		json.RawMessage(`{"a":"b"}]`),
	} {
		got := SanitizeJSON(raw)
		if string(got) != string(raw) {
			t.Errorf("SanitizeJSON(%s) = %s, want the original bytes unchanged (trailing data must not be silently dropped)", raw, got)
		}
	}
}

// TestReconcileImagesSanitizesMediaType covers the same class for
// ReconcileImages' media_type column (shared by Slack/Discord/Notion
// image attachments, not just documents).
func TestReconcileImagesSanitizesMediaType(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:2")

	if err := ReconcileImages(ctx, pool, eventID, []ImageRef{
		{SourceImageID: "I1", Mimetype: "image/png\x00"},
	}); err != nil {
		t.Fatalf("ReconcileImages with a NUL byte in Mimetype: unexpected error: %v", err)
	}

	var mediaType string
	if err := pool.QueryRow(ctx, `
		SELECT media_type FROM event_images WHERE event_id = $1 AND source_image_id = 'I1'
	`, eventID).Scan(&mediaType); err != nil {
		t.Fatal(err)
	}
	if mediaType != "image/png" {
		t.Errorf("media_type = %q, want %q (NUL byte stripped)", mediaType, "image/png")
	}
}
