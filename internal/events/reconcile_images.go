package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxFailures is the per-image caption-failure budget: an
// event_images row drops out of the caption queue once
// failure_count reaches this threshold, so transient retries can't
// burn forever on poison images (deleted-from-Slack, content-policy
// refusals, malformed bytes). It matches the partial index
// event_images_uncaptioned (`captioned_at IS NULL AND
// failure_count < 5`).
//
// It lives here, in the events package, rather than in the caption
// worker (package files) because more than one consumer of the
// event_images schema needs the exact threshold and they can't all
// import files: package files already imports package slack (for the
// Slack API limiter), so the ask caption barrier in package slack
// cannot import files without a cycle. events is the leaf both sides
// already depend on, and event_images is fundamentally an
// events-layer table, so the invariant lives with the schema. The
// worker's settled/uncaptioned predicate and the barrier's must not
// drift, so both reference this one const.
const MaxFailures = 5

// ImageRef is the source-agnostic projection of one image
// attachment to be reconciled into event_images. Lives here, not
// per-connector, because every source's image-bearing event ends
// up in the same shared event_images table — the captioner +
// embedder are source-agnostic and so is this reconcile path.
//
// Fields:
//
//   - SourceImageID is whatever stable identifier the source uses to
//     re-find this image on later re-syncs. For slack: the file id
//     (Fxxxxxxxx). For notion: the composite "<block_id>:<contentKey>"
//     where contentKey is a short hash of the underlying file URL, so
//     an image edited in place within the same block gets a new ID.
//
//   - Mimetype is the source-reported MIME. Captioner overrides at
//     fetch time if the HTTP Content-Type disagrees, so a slightly
//     stale value here is harmless.
type ImageRef struct {
	SourceImageID string
	Mimetype      string
}

// ReconcileImages reconciles event_images rows for the current
// attachment list of one event. Source-agnostic; takes a generic
// []ImageRef so any connector can call it after Upsert.
//
// Behavior:
//
//   - For each currently-present image, INSERT ... ON CONFLICT
//     (event_id, source_image_id) DO NOTHING, so re-ingesting an
//     image already recorded changes nothing. In particular it is not
//     a retry: an image that used up its failure budget stays given
//     up until an administrator retries it. Sources re-sync an
//     unchanged item many times (a Notion page on every walk, a
//     Zendesk ticket on every update), and retrying on each one
//     captioned the same broken images again without end.
//
//   - For each previously-present image that's no longer in the
//     refs list, DELETE the chunk first (FK is event_images.chunk_id
//     → event_chunks.id, ON DELETE SET NULL — if we DELETE
//     event_images first the chunk leaks and stays searchable),
//     then DELETE the event_images row. Critical for sensitive-
//     content edits: a poster who deletes an image must not have
//     its caption linger in retrieval.
//
//   - A picture stored inside one of the event's documents
//     (event_document_id set) is not an attachment the source lists, so
//     it is never stale here, in either mode: it stays until its document
//     goes (ReconcileDocumentImages, ReconcileDocuments).
//
//   - UPDATE events.image_count at the end to keep the
//     denormalized column consistent with the row count.
//
// Single transaction. Idempotent.
//
// Captioning + S3 mirror happen out-of-band in the caption worker.
// This function is the ingest-time half of the pipeline; the worker
// runs the slow LLM/S3 work.
func ReconcileImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	eventID int64,
	refs []ImageRef,
) error {
	return reconcileImages(ctx, pool, eventID, refs, false)
}

// ReplaceImages atomically discards every derived image and caption chunk for
// one event and recreates the current refs as fresh caption-worker work. It is
// intentionally narrower than a channel-wide reset: parse failures happen
// before this call, and any database failure rolls back the old derived state.
func ReplaceImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	eventID int64,
	refs []ImageRef,
) error {
	return reconcileImages(ctx, pool, eventID, refs, true)
}

func reconcileImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	eventID int64,
	refs []ImageRef,
	replace bool,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the parent event row first, before touching any event_images
	// row, exactly as every other writer of this table does
	// (ReconcileDocuments, ReconcileDocumentImagesTx, the docs worker,
	// files.Worker.commitCaption). That gives every caller touching one
	// event's images a single, consistent lock order, so two concurrent
	// reconciles of the same event (divergent ref order, a shrinking
	// edit racing a fuller one, or a caption commit racing the removal
	// of the image it's describing) serialize on this lock instead of
	// taking event_images row locks in data-dependent orders that can
	// deadlock (Postgres SQLSTATE 40P01).
	if _, err := tx.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, eventID); err != nil {
		return fmt.Errorf("lock event: %w", err)
	}

	// Build the array of currently-present source_image_ids.
	// Empty is valid (poster removed all attachments); the
	// !=ALL($2::text[]) predicate handles it because empty-array
	// !=ALL is vacuously true → all existing event_images for
	// this event get deleted.
	currentIDs := make([]string, 0, len(refs))
	for _, r := range refs {
		currentIDs = append(currentIDs, r.SourceImageID)
	}

	// Step 1: DELETE stale caption chunks first. Replace mode deliberately
	// treats every existing image as stale so an administrator replay queues a
	// fresh caption without exposing a reset/rebuild transaction gap.
	// event_images.chunk_id → event_chunks.id with ON DELETE SET
	// NULL. Reverse order would leak chunks (still searchable).
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_chunks
		 WHERE id IN (
		     SELECT chunk_id FROM event_images
		      WHERE event_id = $1
		        AND event_document_id IS NULL
		        AND ($3 OR source_image_id != ALL($2::text[]))
		        AND chunk_id IS NOT NULL
		 )
	`, eventID, currentIDs, replace); err != nil {
		return fmt.Errorf("delete stale chunks: %w", err)
	}

	// Step 2: DELETE the now-stale event_images rows (all rows in replace mode).
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_images
		 WHERE event_id = $1
		   AND event_document_id IS NULL
		   AND ($3 OR source_image_id != ALL($2::text[]))
	`, eventID, currentIDs, replace); err != nil {
		return fmt.Errorf("delete stale images: %w", err)
	}

	// Step 3: INSERT one event_images row per current image not
	// yet recorded. First sighting creates the row with every
	// worker-populated column NULL (except occurred_at); the worker
	// picks it up through the uncaptioned partial index on its next
	// sweep. A row that already exists is left exactly as it is:
	// re-ingest is not an implicit retry (see ReconcileImages).
	//
	// occurred_at is derived from the parent event via the SELECT
	// FROM events, not passed in as a param: it's a denormalized
	// copy of events.occurred_at (see migration
	// 20260722000001_event_images_occurred_at.sql) and deriving it
	// here means it can never drift from the row it mirrors.
	for _, r := range refs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_images
				(event_id, source_image_id, media_type, occurred_at)
			SELECT $1, $2, $3, e.occurred_at FROM events e WHERE e.id = $1
			ON CONFLICT (event_id, source_image_id) DO NOTHING
		`, eventID, r.SourceImageID, defaultMediaType(r.Mimetype)); err != nil {
			return fmt.Errorf("upsert event_images %s: %w", r.SourceImageID, err)
		}
	}

	// Step 4: maintain events.image_count denormalization.
	// Authoritative against actual row state; safe under concurrent
	// reconcilers of the same event (live Slack vs. the tail sweep, or a
	// caption commit) because the event-row lock taken above serializes
	// them.
	if _, err := tx.Exec(ctx, `
		UPDATE events e
		   SET image_count = (
		     SELECT count(*) FROM event_images i WHERE i.event_id = e.id
		   )
		 WHERE e.id = $1
	`, eventID); err != nil {
		return fmt.Errorf("update image_count: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// defaultMediaType returns the source-reported mimetype, sanitized, or
// "application/octet-stream" when the source omitted the field. The
// schema requires media_type NOT NULL and we don't want ingest to fail
// on a missing-field edge case for an obscure file type — nor, same as
// defaultFilename, on a dirty byte in a source-reported string that
// reaches this insert after the sink's own Upsert already succeeded.
func defaultMediaType(s string) string {
	s = SanitizeText(s)
	if s == "" {
		return "application/octet-stream"
	}
	return s
}
