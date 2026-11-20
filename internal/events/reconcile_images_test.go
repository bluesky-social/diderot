package events

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// getReconcilePool returns a pool with the events table truncated
// (which cascades through slack_message_details + event_images +
// event_chunks via FKs). ReconcileImages relies on a pre-existing
// events row, so each test seeds one explicitly.
func getReconcilePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// seedReconcileEvent inserts a minimal events row and returns its
// id. Source defaults to slack to match the existing slack-side FK
// (slack_message_details), but the reconcile path itself is
// source-agnostic — these tests would pass with any source.
func seedReconcileEvent(t *testing.T, pool *pgxpool.Pool, externalID string) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO events
			(source, external_id, occurred_at, actor, body, raw)
		VALUES ('slack', $1, $2, 'tester', 'body', '{}'::jsonb)
		RETURNING id
	`, externalID, time.Now().UTC()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details
			(event_id, channel_id, user_id, has_files, occurred_at)
		VALUES ($1, 'C1', 'U1', true, now())
	`, eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestReconcileImagesInsertsRow(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")

	refs := []ImageRef{
		{SourceImageID: "F123", Mimetype: "image/png"},
	}
	if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_images WHERE event_id = $1`,
		eventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("event_images rows: got %d, want 1", count)
	}

	var imgCount int
	if err := pool.QueryRow(ctx,
		`SELECT image_count FROM events WHERE id = $1`,
		eventID).Scan(&imgCount); err != nil {
		t.Fatal(err)
	}
	if imgCount != 1 {
		t.Errorf("events.image_count: got %d, want 1", imgCount)
	}
}

func TestEventImagesRejectsNonImageMediaType(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:non-image")

	_, err := pool.Exec(ctx, `
		INSERT INTO event_images
			(event_id, source_image_id, media_type, occurred_at)
		VALUES ($1, 'F-PDF', 'application/pdf', now())
	`, eventID)
	if err == nil {
		t.Fatal("inserting a non-image media type succeeded")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("insert error type: got %T, want *pgconn.PgError: %v", err, err)
	}
	if pgErr.ConstraintName != "event_images_media_type_is_image" {
		t.Fatalf("constraint: got %q, want event_images_media_type_is_image", pgErr.ConstraintName)
	}
}

func TestReconcileImagesIdempotent(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")

	refs := []ImageRef{
		{SourceImageID: "F123", Mimetype: "image/png"},
	}
	for i := 0; i < 3; i++ {
		if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_images WHERE event_id = $1`,
		eventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("re-runs should not duplicate rows: got %d, want 1", count)
	}
}

func TestReplaceImagesQueuesFreshCaptionAtomicallyPerEvent(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:replace")
	refs := []ImageRef{{SourceImageID: "F123", Mimetype: "image/png"}}
	if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	var oldImageID, chunkID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM event_images WHERE event_id=$1`, eventID).Scan(&oldImageID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, 'old caption', now()) RETURNING id`, eventID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE event_images SET captioned_at=now(), chunk_id=$2
		 WHERE id=$1`, oldImageID, chunkID); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceImages(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	var newImageID int64
	var captioned bool
	if err := pool.QueryRow(ctx, `
		SELECT id, captioned_at IS NOT NULL FROM event_images WHERE event_id=$1`, eventID).Scan(&newImageID, &captioned); err != nil {
		t.Fatal(err)
	}
	if newImageID == oldImageID {
		t.Fatalf("replacement retained image row %d; want a fresh caption-worker row", oldImageID)
	}
	if captioned {
		t.Fatal("replacement image is already marked captioned")
	}
	var oldChunkCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE id=$1`, chunkID).Scan(&oldChunkCount); err != nil {
		t.Fatal(err)
	}
	if oldChunkCount != 0 {
		t.Fatalf("old caption chunk count = %d, want 0", oldChunkCount)
	}
}

func TestReconcileImagesEditDropsAttachment(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")

	first := []ImageRef{
		{SourceImageID: "F1", Mimetype: "image/png"},
		{SourceImageID: "F2", Mimetype: "image/png"},
	}
	if err := ReconcileImages(ctx, pool, eventID, first); err != nil {
		t.Fatal(err)
	}

	// Simulate a chunk attached to F2 (the row about to be
	// dropped) — checks that the chunk is removed when the
	// attachment is.
	var chunkID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, 'caption for F2', now())
		RETURNING id
	`, eventID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET chunk_id = $1 WHERE source_image_id = 'F2'`,
		chunkID); err != nil {
		t.Fatal(err)
	}

	// Edit drops F2; F1 stays.
	second := []ImageRef{
		{SourceImageID: "F1", Mimetype: "image/png"},
	}
	if err := ReconcileImages(ctx, pool, eventID, second); err != nil {
		t.Fatal(err)
	}

	var fileIDs []string
	rows, err := pool.Query(ctx,
		`SELECT source_image_id FROM event_images WHERE event_id = $1 ORDER BY id`,
		eventID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		fileIDs = append(fileIDs, s)
	}
	rows.Close()
	if len(fileIDs) != 1 || fileIDs[0] != "F1" {
		t.Errorf("after edit: got %v, want [F1]", fileIDs)
	}

	// The dropped row's chunk should be gone too.
	var chunkExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM event_chunks WHERE id = $1)`,
		chunkID).Scan(&chunkExists); err != nil {
		t.Fatal(err)
	}
	if chunkExists {
		t.Error("chunk for dropped attachment should have been deleted")
	}
}

func TestReconcileImagesEditClearsAll(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")

	first := []ImageRef{
		{SourceImageID: "F1", Mimetype: "image/png"},
		{SourceImageID: "F2", Mimetype: "image/png"},
	}
	if err := ReconcileImages(ctx, pool, eventID, first); err != nil {
		t.Fatal(err)
	}

	// Empty edit (user removed every attachment).
	if err := ReconcileImages(ctx, pool, eventID, nil); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_images WHERE event_id = $1`,
		eventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("empty edit should clear all rows: got %d", count)
	}

	var imgCount int
	if err := pool.QueryRow(ctx,
		`SELECT image_count FROM events WHERE id = $1`,
		eventID).Scan(&imgCount); err != nil {
		t.Fatal(err)
	}
	if imgCount != 0 {
		t.Errorf("events.image_count should be 0: got %d", imgCount)
	}
}

// A source re-syncs an unchanged item again and again (a Notion page on
// every walk, a Zendesk ticket on every update). That must not put an image
// that used up its attempts, or one part-way through them, back in the
// queue: only an administrator's retry does.
func TestReconcileImagesReingestIsNotARetry(t *testing.T) {
	for _, failures := range []int{3, MaxFailures} {
		t.Run(fmt.Sprintf("after %d failures", failures), func(t *testing.T) {
			pool := getReconcilePool(t)
			ctx := context.Background()
			eventID := seedReconcileEvent(t, pool, "C1:1.0")

			refs := []ImageRef{
				{SourceImageID: "F123", Mimetype: "image/png"},
			}
			if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				UPDATE event_images
				   SET failure_count = $1, last_failure = 'mock'
				 WHERE source_image_id = 'F123'
			`, failures); err != nil {
				t.Fatal(err)
			}

			if err := ReconcileImages(ctx, pool, eventID, refs); err != nil {
				t.Fatal(err)
			}

			var failureCount int
			var lastFailure *string
			if err := pool.QueryRow(ctx, `
				SELECT failure_count, last_failure
				  FROM event_images
				 WHERE source_image_id = 'F123'
			`).Scan(&failureCount, &lastFailure); err != nil {
				t.Fatal(err)
			}
			if failureCount != failures || lastFailure == nil || *lastFailure != "mock" {
				t.Errorf("re-ingest changed the failure state: failure_count=%d last_failure=%v, want %d and \"mock\"",
					failureCount, lastFailure, failures)
			}
		})
	}
}
