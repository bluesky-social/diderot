package sessioningest

import (
	"context"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// TestPushAcceptsAConvertedSession: a harness's session converted to ATIF by
// the client (sessiondecode.EncodeATIF) is accepted as atif-json, queued
// under the harness that produced it, and replaced by a grown conversion of
// the same session.
func TestPushAcceptsAConvertedSession(t *testing.T) {
	push, _, pool := newTestPushStore(t, nil)
	ctx := context.Background()
	convert := func(turns int) []byte {
		doc, records, err := sessiondecode.DecodeAll(sessiondecode.ClaudeCode{}, ccTurns(turns, 10))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := sessiondecode.EncodeATIF(pushSession, doc, records)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	in := create(pushSession, convert(2))
	in.Format = sessiondecode.FormatATIF
	created, err := push.Accept(ctx, in)
	if err != nil || !created.Created {
		t.Fatalf("create = %+v, %v", created, err)
	}
	in = update(pushSession, created.Revision, convert(3))
	in.Format = sessiondecode.FormatATIF
	updated, err := push.Accept(ctx, in)
	if err != nil || updated.Created || updated.Revision == created.Revision {
		t.Fatalf("update = %+v, %v", updated, err)
	}

	var client, scope string
	var records int
	if err := pool.QueryRow(ctx, `SELECT client, visibility_scope, record_count FROM session_ingest_candidates WHERE session_id=$1`,
		pushSession).Scan(&client, &scope, &records); err != nil {
		t.Fatal(err)
	}
	if client != "claude-code" || scope != "internal" || records != 6 {
		t.Fatalf("candidate = %s/%s/%d records, want claude-code/internal/6", client, scope, records)
	}
}
