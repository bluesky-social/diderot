package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestZendeskEventIDPrefersPublicThreadRoots(t *testing.T) {
	ctx := context.Background()
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	link := "https://synthetic.zendesk.com/hc/en-us/articles/7"
	seed := func(externalID, thread, scope, body string) int64 {
		result, err := sink.Upsert(ctx, Event{
			Source: "zendesk", ExternalID: externalID, ThreadID: thread, Scope: scope,
			Body: body, URL: link, OccurredAt: time.Now(),
		})
		require.NoError(t, err)
		return result.EventID
	}
	privateID := seed("article:7:en-us:restricted", "article:7:en-us", ScopeZendesk, "PRIVATE draft")
	publicID := seed("article:7:en-us:public", "article:7:en-us", ScopePublic, "public text")
	seed("article:8:en-us:public", "article:8:en-us", ScopePublic, "unrelated")
	seed("article:7:en-us:unstamped", "article:7:en-us", ScopeUnstamped, "unclassified")
	id, err := sink.ZendeskEventID(ctx, link, []string{ScopePublic})
	require.NoError(t, err)
	require.Equal(t, publicID, id)
	id, err = sink.ZendeskEventID(ctx, link, []string{ScopeZendesk})
	require.NoError(t, err)
	require.Equal(t, privateID, id)
	_, err = sink.ZendeskEventID(ctx, strings.Replace(link, "synthetic", "other", 1), []string{ScopePublic, ScopeZendesk})
	require.ErrorIs(t, err, ErrEventNotFound)
}
