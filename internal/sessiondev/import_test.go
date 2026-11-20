package sessiondev

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessionfixture"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestImportDiderotSeedsPublishedAndPendingState(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "sessiondev"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bundle, err := sessionfixture.Load(filepath.Join("..", "sessionfixture", "testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportDiderot(ctx, pool, bundle, "http://127.0.0.1:18083"); err != nil {
		t.Fatal(err)
	}

	var candidates, summaries, publications, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_candidates`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_summaries`).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_published`).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source='agent_session'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if candidates != 4 || summaries != 4 || publications != 3 || events != 3 {
		t.Fatalf("imported candidates=%d summaries=%d publications=%d events=%d", candidates, summaries, publications, events)
	}

	var nonCanonicalIDs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND external_id IS DISTINCT FROM session_id || ':session'
	`).Scan(&nonCanonicalIDs); err != nil {
		t.Fatal(err)
	}
	if nonCanonicalIDs != 0 {
		t.Fatalf("fixture events must use UUID-only canonical addressing: %d tuple IDs remain", nonCanonicalIDs)
	}

	var revision string
	var rewriteEpoch, recordCount, userTurnCount int
	var contentBytes int64
	var publishedAt any
	if err := pool.QueryRow(ctx, `
		SELECT published_revision, published_rewrite_epoch, published_record_count,
		       published_user_turn_count, published_content_bytes, published_at
		  FROM session_ingest_candidates
		 WHERE session_id='22222222-2222-4222-8222-222222222222'
	`).Scan(&revision, &rewriteEpoch, &recordCount, &userTurnCount, &contentBytes, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if revision != "" || rewriteEpoch != 0 || recordCount != 0 || userTurnCount != 0 || contentBytes != 0 || publishedAt != nil {
		t.Fatalf("incomplete session has published state: revision=%q epoch=%d records=%d turns=%d bytes=%d at=%v",
			revision, rewriteEpoch, recordCount, userTurnCount, contentBytes, publishedAt)
	}
}
