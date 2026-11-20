package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"pgregory.net/rapid"
)

// sink_prop_test.go is a property test (pgregory.net/rapid) of the sink's
// sanitizing: for any Event whose text fields and raw JSON
// document carry a NUL byte or invalid UTF-8 at a generated position,
// DBSink.Upsert must:
//
//  1. succeed (never permanently fail, forever, on content that never
//     changes);
//  2. report NoChange on a second, identical Upsert (a re-ingest of the
//     same dirty item must still cost no embedding); and
//  3. store genuinely clean input exactly as given — sanitizing must not
//     perturb content that was never dirty.

func newPropPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// dirtyString draws a readable base string for label, then with roughly
// 2-in-5 odds injects either a literal NUL byte or an invalid UTF-8 byte
// at a generated position within it. Returns the string and whether it
// carries a dirty byte.
func dirtyString(t *rapid.T, label string) (string, bool) {
	base := label + "-" + rapid.StringOfN(rapid.RuneFrom([]rune("abcdefgh ")), 0, 10, -1).Draw(t, label+"-base")
	switch rapid.SampledFrom([]string{"clean", "clean", "clean", "nul", "badutf8"}).Draw(t, label+"-kind") {
	case "nul":
		pos := rapid.IntRange(0, len(base)).Draw(t, label+"-pos")
		return base[:pos] + "\x00" + base[pos:], true
	case "badutf8":
		pos := rapid.IntRange(0, len(base)).Draw(t, label+"-pos")
		return base[:pos] + "\xff" + base[pos:], true
	default:
		return base, false
	}
}

// dirtyBody is dirtyString for the Body field specifically, with one more
// generated case: a body that is NUL bytes and nothing else, so it
// sanitizes to "". dirtyString can't produce this — its base always
// carries the "body-" label prefix, so Body never sanitizes to empty —
// which is the case that distinguishes sanitizing before validate() from
// after: in the wrong order it reaches upsertTx and fails with
// "events: chunker produced zero chunks" instead of the ordinary "Body is
// required". Returns the body and whether Upsert is expected to fail
// (sanitizes to empty) rather than whether it merely carries a dirty byte.
func dirtyBody(t *rapid.T) (string, bool) {
	if rapid.SampledFrom([]string{"ordinary", "ordinary", "ordinary", "ordinary", "nul_only"}).Draw(t, "body-shape") == "nul_only" {
		n := rapid.IntRange(1, 4).Draw(t, "body-nul-only-count")
		return strings.Repeat("\x00", n), true
	}
	body, _ := dirtyString(t, "body")
	return body, false
}

// dirtyRaw draws a small JSON document (what a connector's json.Marshal
// of its source-native payload looks like). "nul_escape" embeds a Go
// string with an actual NUL byte, which json.Marshal always escapes as
// the 6-character JSON text \u0000 — the real-world shape of the 22P05
// failure (a connector's struct had a NUL byte in some field, and
// marshalling it produced this escape). "literal_backslash_u0000" embeds
// a Go string that is a literal backslash followed by the text "u0000"
// (no NUL byte anywhere) — json.Marshal escapes the backslash itself,
// producing the 8-character JSON text \\u0000. The two must be told
// apart: the fix strips the NUL character the first decodes to, and
// must leave the second's literal backslash-then-text alone.
func dirtyRaw(t *rapid.T, counter int) (json.RawMessage, bool) {
	switch rapid.SampledFrom([]string{"clean", "clean", "nul_escape", "literal_backslash_u0000"}).Draw(t, "raw-kind") {
	case "nul_escape":
		raw, err := json.Marshal(map[string]any{"n": counter, "v": "a\x00b"})
		if err != nil {
			t.Fatalf("marshal dirty raw: %v", err)
		}
		return raw, true
	case "literal_backslash_u0000":
		raw, err := json.Marshal(map[string]any{"n": counter, "v": `a\u0000b`})
		if err != nil {
			t.Fatalf("marshal raw with literal backslash: %v", err)
		}
		return raw, false
	default:
		raw, err := json.Marshal(map[string]any{"n": counter})
		if err != nil {
			t.Fatalf("marshal clean raw: %v", err)
		}
		return raw, false
	}
}

// decodeJSONNumberAware parses s the same way SanitizeJSON does
// (json.Number, not float64), so a structural comparison doesn't fail on
// a large integer's precision instead of a real content difference.
func decodeJSONNumberAware(t *rapid.T, label, s string) any {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s %q: %v", label, s, err)
	}
	return v
}

func TestPropSinkSanitizesDirtyBytesAndPreservesCleanInput(t *testing.T) {
	pool := newPropPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	n := 0
	rapid.Check(t, func(t *rapid.T) {
		n++
		extID := fmt.Sprintf("prop-%d", n)

		title, _ := dirtyString(t, "title")
		body, bodyWantsEmpty := dirtyBody(t)
		actor, _ := dirtyString(t, "actor")
		actorCanonical, _ := dirtyString(t, "actor_canonical")
		actorID, _ := dirtyString(t, "actor_id")
		threadID, _ := dirtyString(t, "thread_id")
		url, _ := dirtyString(t, "url")
		ctxPrefix, _ := dirtyString(t, "context_prefix")
		embedding, _ := dirtyString(t, "embedding_text")
		raw, rawDirty := dirtyRaw(t, n)

		ev := Event{
			Source: "proptest", ExternalID: extID,
			OccurredAt:     time.Unix(1_700_000_000+int64(n), 0),
			Title:          title,
			Body:           body,
			Actor:          actor,
			ActorCanonical: actorCanonical,
			ActorID:        actorID,
			ThreadID:       threadID,
			URL:            url,
			ContextPrefix:  ctxPrefix,
			EmbeddingText:  embedding,
			Raw:            raw,
			Scope:          ScopePublic,
		}

		// Property 1a: a Body that sanitizes to "" (nothing but NUL
		// bytes) is the one case Upsert must still reject — there is
		// nothing left to index — but with the ordinary, documented
		// "Body is required" validate() error, not a different failure
		// reached by sanitizing after validate() already passed (see
		// TestUpsertSanitizesBeforeValidate for the single-case version
		// of this). Connectors are expected to recognize this case
		// themselves (Discord's render, Notion's ingestOne, Slack's
		// ParseMessage) and never call Upsert with such a Body at all;
		// this only pins which error a connector that doesn't see it
		// coming gets.
		if bodyWantsEmpty {
			_, err := sink.Upsert(ctx, ev)
			if err == nil {
				t.Fatalf("Upsert with Body=%q (sanitizes to empty): want an error, got nil", body)
			}
			if !strings.Contains(err.Error(), "Body is required") {
				t.Fatalf("Upsert with Body=%q (sanitizes to empty): err = %q, want it to contain %q",
					body, err.Error(), "Body is required")
			}
			return
		}

		// Property 1b: Upsert never fails on any OTHER dirty content.
		// Unsanitized, any one of these fields carrying a NUL byte (or
		// Raw carrying a \u0000 escape) makes Postgres reject the write
		// with SQLSTATE 22021 or 22P05, which stalls a connector's sync
		// loop forever.
		res1, err := sink.Upsert(ctx, ev)
		if err != nil {
			t.Fatalf("Upsert #1 (title=%q body=%q actor=%q raw=%q): unexpected error: %v",
				title, body, actor, string(raw), err)
		}

		// Property 2: a second, identical Upsert is a no-op. The
		// unchanged-content short-circuit must compare the SANITIZED
		// form on both sides, or a re-ingest of the same dirty item
		// would churn chunks/embeddings forever instead of costing
		// nothing.
		res2, err := sink.Upsert(ctx, ev)
		if err != nil {
			t.Fatalf("Upsert #2 (identical input): unexpected error: %v", err)
		}
		if !res2.NoChange {
			t.Fatalf("Upsert #2 (identical input) reported a change (res1=%+v res2=%+v); want NoChange", res1, res2)
		}

		// Property 3: every text column holds exactly SanitizeText of
		// what was given — for clean input (the common case) that is a
		// byte-for-byte no-op; for dirty input it is the documented
		// sanitizing rule, not some other transform.
		var gotTitle, gotActor, gotActorID, gotThreadID, gotURL, gotEmbedding *string
		var gotBody, gotActorCanonical string
		if err := pool.QueryRow(ctx, `
			SELECT title, body, actor, actor_canonical, actor_id, thread_id, url, embedding_text
			  FROM events WHERE source = 'proptest' AND external_id = $1
		`, extID).Scan(&gotTitle, &gotBody, &gotActor, &gotActorCanonical, &gotActorID, &gotThreadID, &gotURL, &gotEmbedding); err != nil {
			t.Fatalf("read back row: %v", err)
		}
		checkNullable := func(field, want string, got *string) {
			gotStr := ""
			if got != nil {
				gotStr = *got
			}
			if wantSanitized := SanitizeText(want); gotStr != wantSanitized {
				t.Fatalf("%s = %q, want %q (SanitizeText(%q))", field, gotStr, wantSanitized, want)
			}
		}
		checkNullable("title", title, gotTitle)
		checkNullable("actor", actor, gotActor)
		checkNullable("actor_id", actorID, gotActorID)
		checkNullable("thread_id", threadID, gotThreadID)
		checkNullable("url", url, gotURL)
		checkNullable("embedding_text", embedding, gotEmbedding)
		if wantBody := SanitizeText(body); gotBody != wantBody {
			t.Fatalf("body = %q, want %q (SanitizeText(%q))", gotBody, wantBody, body)
		}
		if wantActorCanonical := SanitizeText(actorCanonical); gotActorCanonical != wantActorCanonical {
			t.Fatalf("actor_canonical = %q, want %q (SanitizeText(%q))", gotActorCanonical, wantActorCanonical, actorCanonical)
		}

		// Property 3, Raw: clean raw is stored with the exact same
		// structure it was given (byte-for-byte text comparison isn't
		// meaningful for jsonb — Postgres reformats on storage
		// regardless of how the fix writes it).
		if !rawDirty {
			var storedRawText string
			if err := pool.QueryRow(ctx,
				`SELECT raw::text FROM events WHERE source = 'proptest' AND external_id = $1`,
				extID,
			).Scan(&storedRawText); err != nil {
				t.Fatalf("read back raw: %v", err)
			}
			want := decodeJSONNumberAware(t, "input raw", string(raw))
			got := decodeJSONNumberAware(t, "stored raw", storedRawText)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("stored raw = %v, want %v (clean raw must round-trip structurally unchanged)", got, want)
			}
		}
	})
}
