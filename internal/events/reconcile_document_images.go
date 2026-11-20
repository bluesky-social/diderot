package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DocumentImage is a picture stored inside one of an event's documents, such
// as a screenshot pasted into a Word file. The document worker has already
// mirrored its bytes, so the caption worker describes it from the mirror
// like any image it is asked to describe again; no source is asked for it.
type DocumentImage struct {
	// SourceImageID names the picture within the event: the document's file
	// ID, then the picture's name inside the document.
	SourceImageID string
	Mimetype      string
	StorageKey    string
	SHA256Hex     string
	Bytes         int64
}

// ReconcileDocumentImages makes the pictures recorded for one document match
// images: new ones join the caption queue, ones no longer in the document go
// with their captions, and ones already recorded are left as they are, so
// reading a document again does not describe its pictures again.
//
// The pictures are images of the document's event, so they are found and
// listed with it. ReconcileImages, which matches an event's images to the
// attachments its source lists, leaves them alone: the source lists the
// document, not what is inside it. Removing the document removes them
// (ReconcileDocuments).
//
// An event deleted meanwhile gets nothing: its chunks are gone and nothing
// would embed a caption.
func ReconcileDocumentImages(ctx context.Context, pool *pgxpool.Pool, eventID, documentID int64, images []DocumentImage) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ReconcileDocumentImagesTx(ctx, tx, eventID, documentID, images); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReconcileDocumentImagesTx is ReconcileDocumentImages inside the caller's
// transaction, for a caller with a condition of its own to check before a
// picture joins the caption queue.
func ReconcileDocumentImagesTx(ctx context.Context, tx pgx.Tx, eventID, documentID int64, images []DocumentImage) error {
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT deleted FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&deleted); err != nil {
		return fmt.Errorf("lock event: %w", err)
	}
	if deleted {
		return nil
	}
	current := make([]string, 0, len(images))
	for _, image := range images {
		current = append(current, image.SourceImageID)
	}
	if err := deleteDocumentImages(ctx, tx, `event_document_id = $1 AND source_image_id != ALL($2::text[])`, documentID, current); err != nil {
		return err
	}
	for _, image := range images {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_images
				(event_id, source_image_id, media_type, occurred_at, storage_key, sha256, bytes, event_document_id)
			SELECT e.id, $2, $3, e.occurred_at, $4, decode($5, 'hex'), $6, $7 FROM events e WHERE e.id = $1
			ON CONFLICT (event_id, source_image_id) DO NOTHING
		`, eventID, image.SourceImageID, defaultMediaType(image.Mimetype), image.StorageKey, image.SHA256Hex,
			image.Bytes, documentID); err != nil {
			return fmt.Errorf("insert document image %s: %w", image.SourceImageID, err)
		}
	}
	return nil
}

// DeleteDocumentImages removes every picture recorded for one document,
// with its caption, inside the caller's transaction.
func DeleteDocumentImages(ctx context.Context, tx pgx.Tx, documentID int64) error {
	return deleteDocumentImages(ctx, tx, `event_document_id = $1`, documentID)
}

// deleteDocumentImages removes the document pictures where matches, captions
// first: a caption chunk is not deleted with its image row, and one left
// behind would stay searchable.
func deleteDocumentImages(ctx context.Context, tx pgx.Tx, where string, args ...any) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_chunks
		 WHERE id IN (SELECT chunk_id FROM event_images WHERE `+where+` AND chunk_id IS NOT NULL)`, args...); err != nil {
		return fmt.Errorf("delete document image captions: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM event_images WHERE `+where, args...); err != nil {
		return fmt.Errorf("delete document images: %w", err)
	}
	return nil
}
