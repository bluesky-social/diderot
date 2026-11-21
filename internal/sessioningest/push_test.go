package sessioningest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

var testRevisionKey = []byte("0123456789abcdef0123456789abcdef")

func newTestPushStore(t *testing.T, mutate func(*PushConfig)) (*PushStore, *Store, *pgxpool.Pool) {
	t.Helper()
	store, pool := newTestStore(t)
	policy := DefaultPolicy()
	cfg := PushConfig{
		RevisionKey: testRevisionKey, MaxDocumentBytes: 8 << 20,
		OwnerQuotaBytes: 64 << 20, OwnerQuotaRevisions: 100,
		Policy: policy, Formats: PushFormats(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	push, err := NewPushStore(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return push, store, pool
}

// ccTurns renders a synthetic Claude Code transcript of n user/assistant
// turns, each padded to roughly pad bytes.
func ccTurns(n, pad int) []byte {
	var b bytes.Buffer
	parent := "null"
	for i := range n {
		ts := time.Date(2026, 11, 1, 10, 0, i, 0, time.UTC).Format(time.RFC3339)
		u := fmt.Sprintf("00000000-0000-4000-8000-%012d", 2*i)
		a := fmt.Sprintf("00000000-0000-4000-8000-%012d", 2*i+1)
		fmt.Fprintf(&b, `{"type":"user","uuid":%q,"parentUuid":%s,"timestamp":%q,"version":"2.1.323","message":{"role":"user","content":%q}}`+"\n",
			u, parent, ts, fmt.Sprintf("question %d %s", i, prose(2*i, pad)))
		fmt.Fprintf(&b, `{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":%q,"message":{"role":"assistant","model":"m","content":[{"type":"text","text":%q}]}}`+"\n",
			a, u, ts, fmt.Sprintf("answer %d %s", i, prose(2*i+1, pad)))
		parent = fmt.Sprintf("%q", a)
	}
	return b.Bytes()
}

// prose is deterministic, non-repeating filler: transcripts are text, and a
// long run of one byte would defeat any content-defined chunker.
func prose(seed, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", seed, i)))
		b.WriteString(hex.EncodeToString(sum[:4]))
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

const pushSession = "7b1e9a52-3c4d-4e5f-8a6b-7c8d9e0f1a2b"

func create(sessionID string, body []byte) PushInput {
	return PushInput{Owner: "Alice@Example.com", SessionID: sessionID, Format: sessiondecode.FormatClaudeCode, Create: true, Body: body}
}

func update(sessionID, ifMatch string, body []byte) PushInput {
	return PushInput{Owner: "alice@example.com", SessionID: sessionID, Format: sessiondecode.FormatClaudeCode, IfMatch: []string{ifMatch}, Body: body}
}

func precondition(t *testing.T, err error) *PushPreconditionError {
	t.Helper()
	var pe *PushPreconditionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want precondition failure", err)
	}
	return pe
}

func TestPushPreconditionsAndRetries(t *testing.T) {
	push, store, _ := newTestPushStore(t, nil)
	ctx := context.Background()
	a, b := ccTurns(2, 10), ccTurns(3, 10)

	created, err := push.Accept(ctx, create(pushSession, a))
	if err != nil || !created.Created || !strings.HasPrefix(created.Revision, "hmac-sha256:") {
		t.Fatalf("create = %+v, %v", created, err)
	}
	// A create retried after a lost response: 412, and the body says it landed.
	pe := precondition(t, func() error { _, err := push.Accept(ctx, create(pushSession, a)); return err }())
	if pe.Current != created.Revision || !pe.BodyMatchesCurrent {
		t.Fatalf("identical create retry = %+v", pe)
	}
	pe = precondition(t, func() error { _, err := push.Accept(ctx, create(pushSession, b)); return err }())
	if pe.BodyMatchesCurrent {
		t.Fatal("a different body must not claim to match the head")
	}

	updated, err := push.Accept(ctx, update(pushSession, created.Revision, b))
	if err != nil || updated.Created || updated.Revision == created.Revision {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	// An update retried after a lost response is already applied.
	retried, err := push.Accept(ctx, update(pushSession, created.Revision, b))
	if err != nil || !retried.AlreadyApplied || retried.Revision != updated.Revision {
		t.Fatalf("update retry = %+v, %v", retried, err)
	}
	// A stale writer with different bytes loses.
	pe = precondition(t, func() error {
		_, err := push.Accept(ctx, update(pushSession, created.Revision, ccTurns(4, 10)))
		return err
	}())
	if pe.Current != updated.Revision || pe.BodyMatchesCurrent {
		t.Fatalf("stale update = %+v", pe)
	}

	// A -> B -> A: the head returns to A's token, so a writer holding A
	// passes If-Match again.
	back, err := push.Accept(ctx, update(pushSession, updated.Revision, a))
	if err != nil || back.Revision != created.Revision {
		t.Fatalf("A->B->A = %+v, %v", back, err)
	}
	again, err := push.Accept(ctx, update(pushSession, created.Revision, ccTurns(5, 10)))
	if err != nil || again.AlreadyApplied {
		t.Fatalf("writer holding A after A->B->A = %+v, %v", again, err)
	}

	c, err := store.LoadCandidate(ctx, Identity{Owner: "alice@example.com", Client: "claude-code", SessionID: pushSession})
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != again.Revision || c.RecordCount != 10 || c.UserTurnCount != 5 || !c.Complete {
		t.Fatalf("candidate = %+v", c)
	}
	head, body, err := push.HeadBody(ctx, "alice@example.com", pushSession)
	if err != nil || head.Revision != again.Revision || !bytes.Equal(body, ccTurns(5, 10)) {
		t.Fatalf("head body: %v", err)
	}
}

func TestPushBindingConflicts(t *testing.T) {
	push, store, _ := newTestPushStore(t, nil)
	ctx := context.Background()
	if _, err := push.Accept(ctx, create(pushSession, ccTurns(1, 1))); err != nil {
		t.Fatal(err)
	}
	other := update(pushSession, "*", ccTurns(2, 1))
	other.Owner = "mallory@example.com"
	if _, err := push.Accept(ctx, other); !errors.Is(err, ErrPushForbidden) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err := push.Head(ctx, "mallory@example.com", pushSession); !errors.Is(err, ErrPushForbidden) {
		t.Fatalf("other owner head: %v", err)
	}
	codex := update(pushSession, "*", []byte(`{"timestamp":"2026-11-01T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":"x"}}`+"\n"))
	codex.Format = sessiondecode.FormatCodex
	var conflict *PushConflictError
	if _, err := push.Accept(ctx, codex); !errors.As(err, &conflict) {
		t.Fatalf("format change: %v", err)
	}

	// The feed cannot write a pushed session. That is a deterministic,
	// permanent per-session refusal, so it is skipped and reported back
	// rather than failing the whole page (UpsertChangesScopedAt); the
	// one-change page here still commits with nothing skipped left to
	// land, and the binding keeps belonging to push.
	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decideInternal := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	_, skipped, err := store.UpsertChangesScopedAt(ctx, []SessionChange{
		{ChangeSeq: cursor.ChangeSeq + 1, SessionID: pushSession, Owner: "alice@example.com", Revision: "sha256:x"},
	}, cursor.ChangeSeq, cursor.ChangeSeq+1, decideInternal)
	if err != nil {
		t.Fatalf("feed over push: unexpected page error: %v", err)
	}
	if len(skipped) != 1 || !errors.Is(skipped[0].Err, ErrSessionWriterConflict) {
		t.Fatalf("feed over push: skipped = %+v, want one ErrSessionWriterConflict entry", skipped)
	}
	// ...and push cannot claim a feed session, including one written before
	// bindings existed.
	const feedSession = "11111111-2222-4333-8444-555555555555"
	if _, err := store.UpsertChanges(ctx, []SessionChange{{ChangeSeq: 2, SessionID: feedSession, Owner: "alice@example.com", Revision: "sha256:y"}}, func(SessionChange) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := push.Accept(ctx, create(feedSession, ccTurns(1, 1))); !errors.As(err, &conflict) {
		t.Fatalf("push over feed: %v", err)
	}
	const legacy = "99999999-2222-4333-8444-555555555555"
	if _, err := store.UpsertChanges(ctx, []SessionChange{{ChangeSeq: 3, SessionID: legacy, Owner: "alice@example.com", Revision: "sha256:z"}}, func(SessionChange) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM session_ingest_bindings WHERE session_id=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := push.Accept(ctx, create(legacy, ccTurns(1, 1))); !errors.As(err, &conflict) {
		t.Fatalf("push over pre-binding feed session: %v", err)
	}
}

// TestFeedOwnerChangeRebinds: the feed stays the authority for its own
// sessions' owners, as the candidate row always was.
func TestPushFormatsExcludeTheFeedFormat(t *testing.T) {
	got := strings.Join(PushFormats().Formats(), ",")
	if got != "atif-json,claude-code-jsonl,codex-rollout-jsonl,copilot-cli-events-jsonl,gemini-cli-session-jsonl,hf-sts-jsonl,pi-session-jsonl,polytoken-jsonl" {
		t.Fatalf("push formats = %s", got)
	}
}

func TestFeedOwnerChangeRebinds(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	for i, owner := range []string{"a@example.com", "b@example.com"} {
		if _, err := store.UpsertChanges(ctx, []SessionChange{{ChangeSeq: int64(i + 1), SessionID: "s-owner", Owner: owner, Revision: "sha256:x"}}, func(SessionChange) bool { return true }); err != nil {
			t.Fatalf("owner %s: %v", owner, err)
		}
	}
	var owner, writer string
	if err := store.pool.QueryRow(ctx, `SELECT owner_canonical, writer FROM session_ingest_bindings WHERE session_id='s-owner'`).Scan(&owner, &writer); err != nil {
		t.Fatal(err)
	}
	if owner != "b@example.com" || writer != WriterFeed {
		t.Fatalf("binding = %s/%s", owner, writer)
	}
}

func TestPushQuotasAndDecodeErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("bytes", func(t *testing.T) {
		push, _, _ := newTestPushStore(t, func(c *PushConfig) { c.OwnerQuotaBytes = 4 << 10 })
		var quota *PushQuotaError
		if _, err := push.Accept(ctx, create(pushSession, ccTurns(20, 200))); !errors.As(err, &quota) || quota.Quota != "bytes" {
			t.Fatalf("err = %v", err)
		}
		if _, err := push.Head(ctx, "alice@example.com", pushSession); !errors.Is(err, ErrPushNotFound) {
			t.Fatalf("a refused push left state: %v", err)
		}
	})
	t.Run("revisions", func(t *testing.T) {
		push, _, _ := newTestPushStore(t, func(c *PushConfig) { c.OwnerQuotaRevisions = 1 })
		first, err := push.Accept(ctx, create(pushSession, ccTurns(1, 1)))
		if err != nil {
			t.Fatal(err)
		}
		var quota *PushQuotaError
		if _, err := push.Accept(ctx, update(pushSession, first.Revision, ccTurns(2, 1))); !errors.As(err, &quota) || quota.Quota != "revisions" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reinstating a retained revision at the limit", func(t *testing.T) {
		push, _, _ := newTestPushStore(t, func(c *PushConfig) { c.OwnerQuotaRevisions = 2 })
		a, err := push.Accept(ctx, create(pushSession, ccTurns(1, 1)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := push.Accept(ctx, update(pushSession, a.Revision, ccTurns(2, 1)))
		if err != nil {
			t.Fatal(err)
		}
		back, err := push.Accept(ctx, update(pushSession, b.Revision, ccTurns(1, 1)))
		if err != nil || back.Revision != a.Revision {
			t.Fatalf("B -> A at the revision limit: %+v %v", back, err)
		}
	})
	t.Run("undecodable", func(t *testing.T) {
		push, _, _ := newTestPushStore(t, nil)
		var decode *PushDecodeError
		_, err := push.Accept(ctx, create(pushSession, []byte(`{"type":"user","uuid":"a","parentUuid":null,"message":{"content":[{"type":"tool_result","tool_use_id":"x"}]},"timestamp":"2026-11-01T10:00:00Z"}`+"\n")))
		if !errors.As(err, &decode) || !errors.Is(err, sessiondecode.ErrStructural) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestPushStorageDeduplicatesAppends: an append-only session's revisions cost
// roughly its final size, not the sum of every snapshot.
func TestPushStorageDeduplicatesAppends(t *testing.T) {
	push, _, pool := newTestPushStore(t, nil)
	ctx := context.Background()
	prev, err := push.Accept(ctx, create(pushSession, ccTurns(40, 2000)))
	if err != nil {
		t.Fatal(err)
	}
	var snapshots int64
	for n := 41; n <= 60; n++ {
		body := ccTurns(n, 2000)
		snapshots += int64(len(body))
		next, err := push.Accept(ctx, update(pushSession, prev.Revision, body))
		if err != nil {
			t.Fatal(err)
		}
		prev = next
	}
	var stored, revisions int64
	if err := pool.QueryRow(ctx, `SELECT stored_bytes, revisions FROM session_push_owner_usage WHERE owner_canonical='alice@example.com'`).Scan(&stored, &revisions); err != nil {
		t.Fatal(err)
	}
	var chunkBytes int64
	if err := pool.QueryRow(ctx, `SELECT sum(octet_length(data)) FROM session_push_chunks`).Scan(&chunkBytes); err != nil {
		t.Fatal(err)
	}
	final := int64(len(ccTurns(60, 2000)))
	t.Logf("21 revisions: snapshots %d bytes, final %d, stored %d (raw + records)", snapshots, final, stored)
	// Raw bytes plus materialized records each dedupe to about their final
	// size; allow generous slack for chunk boundaries at each append.
	if stored != chunkBytes || stored > 3*final || revisions != 21 {
		t.Fatalf("stored=%d chunks=%d final=%d snapshots=%d revisions=%d", stored, chunkBytes, final, snapshots, revisions)
	}
}

func TestWorkerIndexesPushedSession(t *testing.T) {
	push, store, _ := newTestPushStore(t, nil)
	ctx := context.Background()
	if err := store.SetSessionSummarizationEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "sessiondecode", "testdata", "claude-code", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := push.Accept(ctx, create(pushSession, data))
	if err != nil {
		t.Fatal(err)
	}
	_, records, err := sessiondecode.DecodeAll(sessiondecode.ClaudeCode{}, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.LoadCandidate(ctx, Identity{Owner: "alice@example.com", Client: "claude-code", SessionID: pushSession})
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Cfg: WorkerConfig{Policy: push.cfg.Policy}, Store: store, Push: push}
	doc, digest, size, err := w.loadDocument(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, r := range records {
		line, _ := json.Marshal(r)
		want = append(want, string(line))
	}
	if string(doc.Transcript) != strings.Join(want, "\n") || doc.RecordCount != len(records) || doc.DecoderVersion == "" {
		t.Fatalf("transcript mismatch:\n%s", doc.Transcript)
	}
	if size != int64(len(data)) || !strings.HasPrefix(digest, "sha256:") || doc.Owner != "alice@example.com" {
		t.Fatalf("size=%d digest=%s owner=%s", size, digest, doc.Owner)
	}

	rec := &recorderPublisher{store: store}
	w = &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: push.cfg.Policy, Thresholds: noQuietTime, Summarize: true},
		Store: store, Push: push, Summarize: staticSummarizer{}, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.published) != 1 || rec.published[0] != pushSession+"|"+out.Revision {
		t.Fatalf("published = %+v", rec.published)
	}
	if err := (&Worker{Cfg: w.Cfg, Store: store, Push: push, Feed: &fakeFeed{}}).Tick(ctx); err == nil {
		t.Fatal("a worker with both a feed and push must refuse to run")
	}
}

func TestPushRematerializesOnDecoderUpgrade(t *testing.T) {
	push, store, pool := newTestPushStore(t, nil)
	ctx := context.Background()
	out, err := push.Accept(ctx, create(pushSession, ccTurns(3, 10)))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a revision accepted under an older decoder version.
	if _, err := pool.Exec(ctx, `UPDATE session_push_materializations SET decoder_version='0' WHERE session_id=$1`, pushSession); err != nil {
		t.Fatal(err)
	}
	c, err := store.LoadCandidate(ctx, Identity{Owner: "alice@example.com", Client: "claude-code", SessionID: pushSession})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, _, err := push.loadDocument(ctx, c)
	if err != nil || doc.RecordCount != 6 {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	var versions []string
	rows, err := pool.Query(ctx, `SELECT decoder_version FROM session_push_materializations WHERE session_id=$1 AND revision=$2 ORDER BY decoder_version`, pushSession, out.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if strings.Join(versions, ",") != "0,"+(sessiondecode.ClaudeCode{}).Version() {
		t.Fatalf("materializations = %v; the old one must stay", versions)
	}
}
