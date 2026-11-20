package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// otherModelImages matches the images (i, with parent event e) whose caption
// a model other than $1 wrote. An image without a caption chunk (skipped as
// an unsupported format, or never captioned) is not one of them, and neither
// is an image already waiting in the queue.
const otherModelImages = `
	       e.id = i.event_id
	   AND NOT e.deleted
	   AND i.captioned_at IS NOT NULL
	   AND i.chunk_id IS NOT NULL
	   AND i.caption_model IS DISTINCT FROM $1`

// CountOtherModels reports how many images carry a caption written by a
// model other than model.
func CountOtherModels(ctx context.Context, pool *pgxpool.Pool, model string) (int64, error) {
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_images i, events e WHERE`+otherModelImages, model).Scan(&count); err != nil {
		return 0, fmt.Errorf("count images captioned by another model: %w", err)
	}
	return count, nil
}

// RequeueOtherModels puts every image whose caption a model other than model
// wrote back in the caption queue, and returns how many. Each keeps its
// caption, and stays searchable by it, until the worker commits the new one
// (commitCaption replaces the chunk in one transaction). The worker reads the
// bytes from the mirror, so nothing is fetched from the image's source again.
// An image whose new caption fails every attempt keeps its old caption and is
// listed with the other caption failures.
//
// Running it again only queues what the current model has not described, so
// it is safe to repeat after an interruption.
func RequeueOtherModels(ctx context.Context, pool *pgxpool.Pool, model string) (int64, error) {
	if model == "" {
		return 0, errors.New("requeue images: no caption model is configured")
	}
	// The conditions are on the updated row itself, so a second request that
	// waited on the first sees its rows as already queued and leaves them,
	// and the worker's claims on them, alone.
	tag, err := pool.Exec(ctx, `
		UPDATE event_images i
		   SET captioned_at = NULL, failure_count = 0, last_failure = NULL, last_failed_at = NULL,
		       claimed_at = NULL, queued_at = now()
		  FROM events e
		 WHERE`+otherModelImages, model)
	if err != nil {
		return 0, fmt.Errorf("requeue images captioned by another model: %w", err)
	}
	return tag.RowsAffected(), nil
}
