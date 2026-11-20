package sessioningest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

func expandLines(t *testing.T, push *PushStore, req ExpandRequest) []string {
	t.Helper()
	if req.AllowedScopes == nil {
		req.AllowedScopes = []string{events.ScopeInternal}
	}
	body, err := push.ExpandRange(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(body, "\n")
}

func TestPushExpandServesStoredRecords(t *testing.T) {
	push, _, pool := newTestPushStore(t, nil)
	ctx := context.Background()
	body := ccTurns(3, 10)
	out, err := push.Accept(ctx, create(pushSession, body))
	if err != nil {
		t.Fatal(err)
	}
	_, records, err := sessiondecode.DecodeAll(sessiondecode.ClaudeCode{}, body)
	if err != nil {
		t.Fatal(err)
	}
	lines := expandLines(t, push, ExpandRequest{SessionID: pushSession, Revision: out.Revision, Start: 1, End: 3})
	for i, line := range lines {
		want, _ := json.Marshal(records[1+i])
		if line != string(want) {
			t.Fatalf("line %d = %s, want %s", i, line, want)
		}
	}
	if _, err := push.ExpandRange(ctx, ExpandRequest{SessionID: pushSession, Revision: out.Revision, Start: 0, End: 1,
		AllowedScopes: []string{"owner:someone@example.com"}}); !errors.Is(err, ErrPushNotFound) {
		t.Fatalf("out of scope: %v", err)
	}
	if _, err := push.ExpandRange(ctx, ExpandRequest{SessionID: pushSession, Revision: out.Revision, Start: 0, End: 1}); !errors.Is(err, ErrPushNotFound) {
		t.Fatalf("no scopes installed must fail closed: %v", err)
	}
	if _, err := push.ExpandRange(ctx, ExpandRequest{SessionID: pushSession, Revision: out.Revision, Start: 0, End: 99,
		AllowedScopes: []string{events.ScopeInternal}}); err == nil {
		t.Fatal("a range past the records must fail")
	}

	// A citation built from an earlier decoder version keeps resolving to
	// that version's records after an upgrade adds a new materialization.
	if _, err := pool.Exec(ctx, `UPDATE session_push_materializations SET decoder_version='0' WHERE session_id=$1`, pushSession); err != nil {
		t.Fatal(err)
	}
	current := expandLines(t, push, ExpandRequest{SessionID: pushSession, Revision: out.Revision, Start: 0, End: 1})
	old := expandLines(t, push, ExpandRequest{SessionID: pushSession, Revision: out.Revision, DecoderVersion: "0", Start: 0, End: 1})
	if current[0] != old[0] {
		t.Fatalf("same decoder output, different versions: %s vs %s", current[0], old[0])
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_push_materializations WHERE session_id=$1`, pushSession).Scan(&n); err != nil || n != 2 {
		t.Fatalf("materializations = %d (%v), want the old one kept beside the new", n, err)
	}
}

func usage(t *testing.T, push *PushStore) (int64, int64) {
	t.Helper()
	var stored, revisions int64
	if err := push.pool.QueryRow(context.Background(), `SELECT stored_bytes, revisions FROM session_push_owner_usage
		WHERE owner_canonical='alice@example.com'`).Scan(&stored, &revisions); err != nil {
		t.Fatal(err)
	}
	var chunks int64
	if err := push.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(octet_length(data)), 0) FROM session_push_chunks`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != stored {
		t.Fatalf("usage %d disagrees with stored chunks %d", stored, chunks)
	}
	return stored, revisions
}

func TestPushRetentionPrunesSupersededRevisions(t *testing.T) {
	push, _, pool := newTestPushStore(t, nil)
	ctx := context.Background()
	a, err := push.Accept(ctx, create(pushSession, ccTurns(30, 500)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := push.Accept(ctx, update(pushSession, a.Revision, ccTurns(3, 500)))
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, beforeRevisions := usage(t, push)

	if n, err := push.PruneSuperseded(ctx, time.Hour, 10); err != nil || n != 0 {
		t.Fatalf("inside the window: pruned %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_push_revisions SET superseded_at = now() - interval '2 hours' WHERE superseded_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if n, err := push.PruneSuperseded(ctx, time.Hour, 10); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	afterBytes, afterRevisions := usage(t, push)
	if afterRevisions != beforeRevisions-1 || afterBytes >= beforeBytes {
		t.Fatalf("usage %d/%d -> %d/%d", beforeBytes, beforeRevisions, afterBytes, afterRevisions)
	}
	var sup *SupersededError
	_, err = push.ExpandRange(ctx, ExpandRequest{SessionID: pushSession, Revision: a.Revision, Start: 0, End: 1, AllowedScopes: []string{events.ScopeInternal}})
	if !errors.As(err, &sup) || sup.Current != b.Revision || sup.Revision != a.Revision {
		t.Fatalf("expand pruned revision: %v", err)
	}
	if _, body, err := push.HeadBody(ctx, "alice@example.com", pushSession); err != nil || string(body) != string(ccTurns(3, 500)) {
		t.Fatalf("head damaged by prune: %v", err)
	}

	// Pushing the pruned bytes again restores them as a stored revision.
	back, err := push.Accept(ctx, update(pushSession, b.Revision, ccTurns(30, 500)))
	if err != nil || back.Revision != a.Revision {
		t.Fatalf("re-push pruned revision: %+v %v", back, err)
	}
	if _, revisions := usage(t, push); revisions != beforeRevisions {
		t.Fatalf("revisions after restore = %d, want %d", revisions, beforeRevisions)
	}
	expandLines(t, push, ExpandRequest{SessionID: pushSession, Revision: a.Revision, Start: 0, End: 1})

	// The head is never pruned, even if it was superseded once before.
	if _, err := pool.Exec(ctx, `UPDATE session_push_revisions SET superseded_at = now() - interval '2 hours' WHERE revision = $1`, a.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_push_revisions SET superseded_at = now() - interval '2 hours' WHERE superseded_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := push.PruneSuperseded(ctx, time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	if _, body, err := push.HeadBody(ctx, "alice@example.com", pushSession); err != nil || string(body) != string(ccTurns(30, 500)) {
		t.Fatalf("head pruned: %v", err)
	}
}
