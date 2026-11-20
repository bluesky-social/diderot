package zendesk

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func expandTestSink(t *testing.T) (*pgxpool.Pool, *events.DBSink) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "zendesk"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `TRUNCATE events, zendesk_sync, zendesk_retry CASCADE`)
	require.NoError(t, err)
	return pool, events.NewDBSink(pool, prometheus.NewRegistry())
}

func TestConversationFiltersAnchorSiblingsAndAttachments(t *testing.T) {
	ctx := context.Background()
	pool, sink := expandTestSink(t)
	link := "https://synthetic.zendesk.com/hc/en-us/articles/7"
	seed := func(externalID, thread, scope, body string) int64 {
		result, err := sink.Upsert(ctx, events.Event{
			Source: "zendesk", ExternalID: externalID, ThreadID: thread, Scope: scope,
			Body: body, URL: link, OccurredAt: time.Now(),
		})
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			VALUES ($1, 'image_caption', 0, $2, now())`, result.EventID, body+" image")
		require.NoError(t, err)
		return result.EventID
	}
	privateID := seed("article:7:en-us:restricted", "article:7:en-us", events.ScopeZendesk, "PRIVATE draft")
	publicID := seed("article:7:en-us:public", "article:7:en-us", events.ScopePublic, "public text")
	seed("article:8:en-us:public", "article:8:en-us", events.ScopePublic, "unrelated")
	seed("article:7:en-us:unstamped", "article:7:en-us", events.ScopeUnstamped, "unclassified")
	content, truncated, err := conversation(ctx, pool, publicID, []string{events.ScopePublic, events.ScopeUnstamped})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Contains(t, content, "public text image")
	require.NotContains(t, content, "PRIVATE")
	require.NotContains(t, content, "unrelated")
	require.NotContains(t, content, "unclassified")
	_, _, err = conversation(ctx, pool, privateID, []string{events.ScopePublic})
	require.ErrorIs(t, err, events.ErrEventNotFound)
	_, _, err = conversation(ctx, pool, publicID, nil)
	require.ErrorIs(t, err, events.ErrEventNotFound)
	content, _, err = conversation(ctx, pool, publicID, []string{events.ScopePublic, events.ScopeZendesk})
	require.NoError(t, err)
	require.Contains(t, content, "PRIVATE draft image")
	_, err = sink.Delete(ctx, "zendesk", "article:7:en-us:restricted")
	require.NoError(t, err)
	content, _, err = conversation(ctx, pool, publicID, []string{events.ScopePublic, events.ScopeZendesk})
	require.NoError(t, err)
	require.NotContains(t, content, "PRIVATE")
}

func TestConversationIncludesDocumentCoverageAndBoundsText(t *testing.T) {
	ctx := context.Background()
	pool, sink := expandTestSink(t)
	result, err := sink.Upsert(ctx, events.Event{
		Source: "zendesk", ExternalID: "ticket:12", ThreadID: "ticket:12", Scope: events.ScopeZendesk,
		Body: "Synthetic ticket", OccurredAt: time.Now(),
	})
	require.NoError(t, err)
	var docID int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO event_documents
		(event_id, source_file_id, filename, media_type, doc_format, occurred_at, page_count, pages_extracted, incomplete_reason)
		VALUES ($1, 'doc', 'report.pdf', 'application/pdf', 'pdf', now(), 10, 2, 'cost_limit') RETURNING id`, result.EventID).Scan(&docID))
	_, err = pool.Exec(ctx, `INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at, event_document_id)
		VALUES ($1, 'document_text', 0, $2, now(), $3)`, result.EventID, strings.Repeat("界", 30000), docID)
	require.NoError(t, err)
	content, truncated, err := conversation(ctx, pool, result.EventID, []string{events.ScopeZendesk})
	require.NoError(t, err)
	require.True(t, truncated)
	require.True(t, utf8.ValidString(content))
	require.LessOrEqual(t, len(content), 64<<10)
	require.Contains(t, content, "completeness=partial")
	require.Contains(t, content, "pages=10; pages_extracted=2")
	require.Contains(t, content, "report.pdf")
	require.Contains(t, content, "[Truncated:")
}
