package slack

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedDocumentBackfillEvent(
	t *testing.T, pool *pgxpool.Pool, channelID string, seq int,
) int64 {
	t.Helper()
	ts := fmt.Sprintf("1700000000.%06d", seq)
	raw := fmt.Sprintf(`{
		"type":"message","subtype":"file_share","channel":%q,"user":"U1","ts":%q,
		"message":{"user":"U1","ts":%q,"files":[{
			"id":%q,"name":"contract.pdf","mimetype":"application/pdf","mode":"hosted"
		}]}}`, channelID, ts, ts, fmt.Sprintf("F%d", seq))
	var eventID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO events (source, external_id, occurred_at, actor, body, raw, scope)
		VALUES ('slack', $1, now(), 'tester', 'file', $2::jsonb, 'public')
		RETURNING id
	`, channelID+":"+ts, raw).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO slack_message_details
			(event_id, channel_id, user_id, has_files, occurred_at)
		VALUES ($1, $2, 'U1', true, now())
	`, eventID, channelID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestDocumentBackfillWorkerEnqueuesHistory(t *testing.T) {
	pool := getChannelConfigPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	seedDocumentBackfillEvent(t, pool, "C1", 1)
	seedDocumentBackfillEvent(t, pool, "C1", 2)

	store := newTestStore(t, pool, nil, nil)
	if err := store.SetDocIngestEnabled(ctx, "C1", true, "test"); err != nil {
		t.Fatal(err)
	}
	worker := NewDocumentBackfillWorker(pool, nil)
	if err := worker.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	var docs int
	var cursor *int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_documents), doc_backfill_cursor
		  FROM slack_channel_config WHERE channel_id = 'C1'
	`).Scan(&docs, &cursor); err != nil {
		t.Fatal(err)
	}
	if docs != 2 {
		t.Errorf("documents = %d, want 2", docs)
	}
	if cursor != nil {
		t.Errorf("cursor = %v, want complete", cursor)
	}
}

func TestDocumentBackfillRequestLifecycle(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, nil, nil)
	ctx := context.Background()

	if err := store.SetDocIngestEnabled(ctx, "C1", true, "test"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Get(ctx, "C1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocBackfillCursor == nil || *cfg.DocBackfillCursor != 0 {
		t.Fatalf("initial cursor = %v, want 0", cfg.DocBackfillCursor)
	}

	zero := int64(0)
	if _, err := pool.Exec(ctx, `
		UPDATE slack_channel_config SET doc_backfill_cursor = $2 WHERE channel_id = $1
	`, "C1", zero); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDocIngestEnabled(ctx, "C1", false, "test"); err != nil {
		t.Fatal(err)
	}
	cfg, err = store.Get(ctx, "C1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocIngestEnabled || cfg.DocBackfillCursor != nil {
		t.Errorf("disabled config = enabled %v cursor %v", cfg.DocIngestEnabled, cfg.DocBackfillCursor)
	}
}
