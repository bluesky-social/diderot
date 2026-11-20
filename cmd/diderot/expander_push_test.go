package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestExpandPushedSessionFromOwnStore: with push ingest, expand reads a
// pushed session's records from diderot's store (no session feed), honors the
// caller's scopes, and reports a pruned revision as superseded.
func TestExpandPushedSessionFromOwnStore(test *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(test, "cmd_expander_push"))
	require.NoError(test, err)
	test.Cleanup(pool.Close)
	policy := sessioningest.DefaultPolicy()
	push, err := sessioningest.NewPushStore(pool, sessioningest.PushConfig{
		RevisionKey: []byte("0123456789abcdef0123456789abcdef"), MaxDocumentBytes: 1 << 20,
		OwnerQuotaBytes: 1 << 30, OwnerQuotaRevisions: 100, Policy: policy, Formats: sessioningest.PushFormats(),
	})
	require.NoError(test, err)
	const sessionID = "4d2c6a1e-7f3b-4c8d-9e0f-1a2b3c4d5e6f"
	line := func(uuid, parent, text string) string {
		return `{"type":"user","uuid":"` + uuid + `","parentUuid":` + parent + `,"timestamp":"2026-11-01T10:00:00Z","message":{"content":"` + text + `"}}` + "\n"
	}
	first := line("u1", "null", "PUSHED_FIRST") + line("u2", `"u1"`, "PUSHED_SECOND")
	in := sessioningest.PushInput{Owner: "alice@example.com", SessionID: sessionID, Format: sessiondecode.FormatClaudeCode, Create: true, Body: []byte(first)}
	a, err := push.Accept(ctx, in)
	require.NoError(test, err)

	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	result, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopeInternal, Source: "agent_session", ExternalID: sessionID + ":records:0:2",
		OccurredAt: time.Now().UTC(), Body: "pushed range", SessionID: sessionID, RecordStart: 0, RecordEnd: 2,
		Raw: json.RawMessage(`{"revision":"` + a.Revision + `","event_kind":"native_session_chunk"}`),
	})
	require.NoError(test, err)
	expander := newSourceExpander(source.NewRegistry(&sessionFeedSource{push: push}), source.Deps{Pool: pool, Sink: sink})

	scoped := ask.WithAllowedScopes(ctx, []string{events.ScopeInternal})
	expanded, err := expander.Expand(scoped, result.EventID)
	require.NoError(test, err)
	require.Contains(test, expanded.Content, "PUSHED_FIRST")
	require.Contains(test, expanded.Content, "PUSHED_SECOND")

	_, err = expander.Expand(ask.WithAllowedScopes(ctx, []string{"owner:bob@example.com"}), result.EventID)
	require.ErrorIs(test, err, ask.ErrExpandEventNotFound, "a caller outside the session's scope learns nothing")

	update := in
	update.Create, update.IfMatch = false, []string{a.Revision}
	update.Body = []byte(line("u9", "null", "REPLACED"))
	b, err := push.Accept(ctx, update)
	require.NoError(test, err)
	_, err = pool.Exec(ctx, `UPDATE session_push_revisions SET superseded_at = now() - interval '2 days' WHERE revision = $1`, a.Revision)
	require.NoError(test, err)
	pruned, err := push.PruneSuperseded(ctx, 24*time.Hour, 10)
	require.NoError(test, err)
	require.Equal(test, 1, pruned)
	expanded, err = expander.Expand(scoped, result.EventID)
	require.NoError(test, err)
	require.True(test, strings.HasPrefix(expanded.Content, "[superseded]"), expanded.Content)
	require.Contains(test, expanded.Content, b.Revision)
}
