package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedDocumentWithPicture gives an event one attached image, one document,
// and one picture found inside the document, each with a caption chunk.
// It returns the document's row ID.
func seedDocumentWithPicture(t *testing.T, pool *pgxpool.Pool, eventID int64) int64 {
	t.Helper()
	ctx := context.Background()
	if err := ReconcileImages(ctx, pool, eventID, []ImageRef{{SourceImageID: "FIMG", Mimetype: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{{SourceFileID: "FDOC", Filename: "shots.docx", Mimetype: "application/msword", DocFormat: "docx"}}); err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM event_documents WHERE event_id = $1`, eventID).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocumentImages(ctx, pool, eventID, documentID, []DocumentImage{{
		SourceImageID: "FDOC/word/media/image1.png", Mimetype: "image/png",
		StorageKey: "slack/ab/abcd.png", SHA256Hex: "abcd", Bytes: 8192,
	}}); err != nil {
		t.Fatal(err)
	}
	// The caption worker describes both.
	if _, err := pool.Exec(ctx, `
		WITH captions AS (
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			SELECT $1, 'image_caption', row_number() OVER (ORDER BY i.id) - 1, 'caption of ' || i.source_image_id, now()
			  FROM event_images i WHERE i.event_id = $1
			RETURNING id, content
		)
		UPDATE event_images i SET captioned_at = now(), chunk_id = c.id
		  FROM captions c WHERE i.event_id = $1 AND c.content = 'caption of ' || i.source_image_id
	`, eventID); err != nil {
		t.Fatal(err)
	}
	return documentID
}

func imagesAndCaptions(t *testing.T, pool *pgxpool.Pool, eventID int64) (images []string, captions int) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT source_image_id FROM event_images WHERE event_id = $1 ORDER BY source_image_id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		images = append(images, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE event_id = $1 AND kind = 'image_caption'`, eventID).Scan(&captions); err != nil {
		t.Fatal(err)
	}
	return images, captions
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A picture found inside a document is not an attachment its source lists.
// Matching the event's images to that list, or rebuilding them, must leave
// it and its caption alone.
func TestReconcileImagesLeavesDocumentPicturesAlone(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")
	seedDocumentWithPicture(t, pool, eventID)
	picture := "FDOC/word/media/image1.png"

	// The source lists the same attachment again, as every re-sync does.
	if err := ReconcileImages(ctx, pool, eventID, []ImageRef{{SourceImageID: "FIMG", Mimetype: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	if images, captions := imagesAndCaptions(t, pool, eventID); !equalStrings(images, []string{picture, "FIMG"}) || captions != 2 {
		t.Fatalf("after a re-sync: images %v, captions %d", images, captions)
	}

	// The message's own image is removed: only it goes.
	if err := ReconcileImages(ctx, pool, eventID, nil); err != nil {
		t.Fatal(err)
	}
	if images, captions := imagesAndCaptions(t, pool, eventID); !equalStrings(images, []string{picture}) || captions != 1 {
		t.Fatalf("after the attached image was removed: images %v, captions %d", images, captions)
	}

	// A rebuild of the event's images replaces what the source lists.
	if err := ReplaceImages(ctx, pool, eventID, []ImageRef{{SourceImageID: "FIMG", Mimetype: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	if images, captions := imagesAndCaptions(t, pool, eventID); !equalStrings(images, []string{picture, "FIMG"}) || captions != 1 {
		t.Fatalf("after a rebuild: images %v, captions %d (the picture keeps its caption)", images, captions)
	}
}

// Reading a document again leaves its recorded pictures as they are, and a
// document that goes takes its pictures and their captions with it.
func TestDocumentPicturesFollowTheirDocument(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")
	documentID := seedDocumentWithPicture(t, pool, eventID)
	first := DocumentImage{SourceImageID: "FDOC/word/media/image1.png", Mimetype: "image/png", StorageKey: "slack/ab/abcd.png", SHA256Hex: "abcd", Bytes: 8192}
	second := DocumentImage{SourceImageID: "FDOC/word/media/image2.png", Mimetype: "image/png", StorageKey: "slack/ef/ef01.png", SHA256Hex: "ef01", Bytes: 4096}

	// Read again, with one more picture: the first keeps its caption and is
	// not queued again; the second waits, mirrored.
	if err := ReconcileDocumentImages(ctx, pool, eventID, documentID, []DocumentImage{first, second}); err != nil {
		t.Fatal(err)
	}
	var firstCaptioned, secondWaiting bool
	var secondKey string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT captioned_at IS NOT NULL AND chunk_id IS NOT NULL FROM event_images WHERE source_image_id = $1),
		       (SELECT captioned_at IS NULL AND event_document_id = $3 FROM event_images WHERE source_image_id = $2),
		       (SELECT storage_key FROM event_images WHERE source_image_id = $2)
	`, first.SourceImageID, second.SourceImageID, documentID).Scan(&firstCaptioned, &secondWaiting, &secondKey); err != nil {
		t.Fatal(err)
	}
	if !firstCaptioned || !secondWaiting || secondKey != second.StorageKey {
		t.Fatalf("first still captioned=%v, second waiting=%v with key %q", firstCaptioned, secondWaiting, secondKey)
	}

	// A picture no longer in the document goes, with its caption.
	if err := ReconcileDocumentImages(ctx, pool, eventID, documentID, []DocumentImage{second}); err != nil {
		t.Fatal(err)
	}
	if images, captions := imagesAndCaptions(t, pool, eventID); !equalStrings(images, []string{second.SourceImageID, "FIMG"}) || captions != 1 {
		t.Fatalf("after the first picture left the document: images %v, captions %d", images, captions)
	}

	// The document is removed from the message: its pictures go too, and the
	// message's own image stays.
	if err := ReconcileDocumentImages(ctx, pool, eventID, documentID, []DocumentImage{first, second}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH caption AS (
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			VALUES ($1, 'image_caption', 7, 'caption of a picture', now()) RETURNING id
		)
		UPDATE event_images SET captioned_at = now(), chunk_id = (SELECT id FROM caption) WHERE source_image_id = $2
	`, eventID, first.SourceImageID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, nil); err != nil {
		t.Fatal(err)
	}
	if images, captions := imagesAndCaptions(t, pool, eventID); !equalStrings(images, []string{"FIMG"}) || captions != 1 {
		t.Fatalf("after the document was removed: images %v, captions %d (only the attached image's)", images, captions)
	}
}

// Nothing is queued for an event deleted while its document was being read.
func TestDocumentPicturesAreNotRecordedForADeletedEvent(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1.0")
	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{{SourceFileID: "FDOC", Filename: "shots.docx", Mimetype: "application/msword", DocFormat: "docx"}}); err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM event_documents WHERE event_id = $1`, eventID).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocumentImages(ctx, pool, eventID, documentID, []DocumentImage{{
		SourceImageID: "FDOC/word/media/image1.png", Mimetype: "image/png", StorageKey: "slack/ab/abcd.png", SHA256Hex: "abcd", Bytes: 8192,
	}}); err != nil {
		t.Fatal(err)
	}
	if images, _ := imagesAndCaptions(t, pool, eventID); len(images) != 0 {
		t.Fatalf("a deleted event got images: %v", images)
	}
}
