package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestExpandCanonicalSessionWithoutProvenanceSelectors(test *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(test, "cmd_expander_canonical"))
	require.NoError(test, err)
	test.Cleanup(pool.Close)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	const sessionID = "019f0083-e3fe-7355-b31c-78efefcc1615"
	const revision = "sha256:canonical-range-test"
	result, err := sink.Upsert(ctx, events.Event{
		Scope:       events.ScopeInternal,
		Source:      "agent_session",
		ExternalID:  sessionID + ":records:0:2",
		OccurredAt:  time.Now().UTC(),
		Body:        "canonical range fixture",
		SessionID:   sessionID,
		RecordStart: 0,
		RecordEnd:   2,
		Raw:         json.RawMessage(`{"revision":"` + revision + `","event_kind":"native_session_chunk"}`),
	})
	require.NoError(test, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(test, "/api/v1/sessions/"+sessionID+"/records/redacted", request.URL.Path)
		require.Equal(test, revision, request.URL.Query().Get("revision"))
		require.Equal(test, "0", request.URL.Query().Get("start"))
		require.Equal(test, "2", request.URL.Query().Get("end"))
		_, _ = writer.Write([]byte("CANONICAL_RANGE_OK\n"))
	}))
	test.Cleanup(server.Close)
	feed := sessionfeed.NewClient(server.URL, "test-only", server.Client())
	feedSource := &sessionFeedSource{feed: func() *sessionfeed.Client { return feed }}
	expander := newSourceExpander(source.NewRegistry(feedSource), source.Deps{Pool: pool, Sink: sink})
	expanded, err := expander.Expand(ask.WithAllowedScopes(ctx, []string{events.ScopeInternal}), result.EventID)
	require.NoError(test, err)
	require.Equal(test, "agent_session", expanded.Source)
	require.Equal(test, "CANONICAL_RANGE_OK\n", expanded.Content)
}
