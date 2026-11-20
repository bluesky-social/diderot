package files

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// captionWith captions imageID through captionOne, then records it as the
// work of model, as if that model had been the captioner at the time.
func captionWith(t *testing.T, pool *pgxpool.Pool, eventID, imageID int64, sourceImageID, model, caption string) int64 {
	t.Helper()
	ctx := context.Background()
	w := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: caption}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	row := claimedRow(t, pool, imageID, eventID, "slack", sourceImageID, "image/png")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_images SET caption_model = $2 WHERE id = $1`, imageID, model); err != nil {
		t.Fatal(err)
	}
	_, _, chunkID := getImageRow(t, pool, imageID)
	if chunkID == nil {
		t.Fatal("captioned image has no chunk")
	}
	return *chunkID
}

// TestRequeueOtherModelsQueuesOnlyAnotherModelsCaptions: the images another
// model described go back in the queue with their captions intact; the
// current model's, a skipped image, an image still waiting, and a deleted
// event's are left alone.
func TestRequeueOtherModelsQueuesOnlyAnotherModelsCaptions(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	earlier := seedImage(t, pool, eventID, "F-earlier")
	current := seedImage(t, pool, eventID, "F-current")
	skipped := seedImage(t, pool, eventID, "F-skipped")
	waiting := seedImage(t, pool, eventID, "F-waiting")
	earlierChunk := captionWith(t, pool, eventID, earlier, "F-earlier", "earlier-model", "first caption")
	captionWith(t, pool, eventID, current, "F-current", "current-model", "kept caption")
	if _, err := pool.Exec(ctx, `
		UPDATE event_images SET captioned_at = now(), caption_model = 'skipped:unsupported-format' WHERE id = $1`, skipped); err != nil {
		t.Fatal(err)
	}
	deletedEvent := seedEventForWorker(t, pool, "C1:2.0")
	onDeleted := seedImage(t, pool, deletedEvent, "F-deleted")
	captionWith(t, pool, deletedEvent, onDeleted, "F-deleted", "earlier-model", "deleted event's caption")
	if _, err := pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, deletedEvent); err != nil {
		t.Fatal(err)
	}
	// A failure left over from before must not count against the new attempt.
	if _, err := pool.Exec(ctx, `
		UPDATE event_images
		   SET failure_count = 3, last_failure = 'old', last_failed_at = now() - interval '3 days',
		       queued_at = now() - interval '3 days'
		 WHERE id = $1`, earlier); err != nil {
		t.Fatal(err)
	}

	count, err := CountOtherModels(ctx, pool, "current-model")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CountOtherModels = %d, want 1", count)
	}
	queued, err := RequeueOtherModels(ctx, pool, "current-model")
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("RequeueOtherModels queued %d, want 1", queued)
	}

	_, captioned, chunkID := getImageRow(t, pool, earlier)
	if captioned {
		t.Error("the earlier model's image should be waiting again")
	}
	if chunkID == nil || *chunkID != earlierChunk {
		t.Errorf("the earlier model's image lost its caption before a new one exists: chunk_id = %v", chunkID)
	}
	var failures int
	if err := pool.QueryRow(ctx, `SELECT failure_count FROM event_images WHERE id = $1`, earlier).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Errorf("failure_count = %d, want 0", failures)
	}
	// Its wait starts now, and nothing of the old failure is left.
	var untimed, queuedNow bool
	if err := pool.QueryRow(ctx, `
		SELECT last_failed_at IS NULL, queued_at > now() - interval '1 minute'
		  FROM event_images WHERE id = $1`, earlier).Scan(&untimed, &queuedNow); err != nil {
		t.Fatal(err)
	}
	if !untimed || !queuedNow {
		t.Errorf("a requeued image: failure time cleared=%v, queued just now=%v", untimed, queuedNow)
	}
	for name, id := range map[string]int64{"current model's": current, "skipped": skipped, "deleted event's": onDeleted} {
		if _, captioned, _ := getImageRow(t, pool, id); !captioned {
			t.Errorf("the %s image was queued", name)
		}
	}
	if _, captioned, chunkID := getImageRow(t, pool, waiting); captioned || chunkID != nil {
		t.Error("the image that was already waiting changed")
	}

	// Once queued, nothing is left for a second request.
	if again, err := RequeueOtherModels(ctx, pool, "current-model"); err != nil || again != 0 {
		t.Fatalf("second RequeueOtherModels = %d, %v; want 0, nil", again, err)
	}
	if _, err := RequeueOtherModels(ctx, pool, ""); err == nil {
		t.Error("RequeueOtherModels with no model should fail, not queue every image")
	}
}

// TestCaptionOneReplacesTheCaptionOfARequeuedImage: describing a queued
// image again swaps its caption chunk in one step and queues the old
// vector's removal.
func TestCaptionOneReplacesTheCaptionOfARequeuedImage(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")
	sibling := seedImage(t, pool, eventID, "F2")
	oldChunk := captionWith(t, pool, eventID, imageID, "F1", "earlier-model", "first caption")
	siblingChunk := captionWith(t, pool, eventID, sibling, "F2", "fake-model", "sibling caption")
	if _, err := RequeueOtherModels(ctx, pool, "fake-model"); err != nil {
		t.Fatal(err)
	}

	w := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: "second caption"}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.imageID != imageID {
		t.Fatalf("claimed %+v, want the requeued image %d", row, imageID)
	}
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}

	model, captioned, chunkID := getImageRow(t, pool, imageID)
	if !captioned || chunkID == nil || *chunkID == oldChunk {
		t.Fatalf("image not described again: captioned=%v chunk=%v (old %d)", captioned, chunkID, oldChunk)
	}
	if model == nil || *model != "fake-model" {
		t.Errorf("caption_model = %v, want fake-model", model)
	}
	rows, err := pool.Query(ctx, `
		SELECT id, content FROM event_chunks WHERE event_id = $1 AND kind = 'image_caption' ORDER BY id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]string{}
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			t.Fatal(err)
		}
		got[id] = content
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("event has %d caption chunks, want the sibling's and the new one: %v", len(got), got)
	}
	if !strings.Contains(got[*chunkID], "second caption") {
		t.Errorf("new chunk content = %q", got[*chunkID])
	}
	if !strings.Contains(got[siblingChunk], "sibling caption") {
		t.Errorf("the sibling image's caption changed: %q", got[siblingChunk])
	}
	var queuedDelete bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM qdrant_delete_queue WHERE point_id = $1)`, oldChunk).Scan(&queuedDelete); err != nil {
		t.Fatal(err)
	}
	if !queuedDelete {
		t.Error("the replaced caption's vector was not queued for removal")
	}
}

// TestRequeuedImageIsClaimedWhileItsSourceIsOff: a requeued image's bytes are
// in the mirror, so it does not wait for its source's connector to run.
func TestRequeuedImageIsClaimedWhileItsSourceIsOff(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")
	captionWith(t, pool, eventID, imageID, "F1", "earlier-model", "first caption")
	neverMirrored := seedImage(t, pool, eventID, "F2")
	if _, err := RequeueOtherModels(ctx, pool, "fake-model"); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &switchableFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.imageID != imageID || row.storageKey == "" {
		t.Fatalf("claimed %+v, want the mirrored image %d", row, imageID)
	}
	// The image that was never fetched still waits for its source.
	if row, err = w.claimUncaptioned(ctx); err != nil || row != nil {
		t.Fatalf("claimed %+v, %v; image %d has no mirrored bytes and its source is off", row, err, neverMirrored)
	}
}

// TestRequeuedImageKeepsItsCaptionWhenTheNewOneFails: a failed attempt leaves
// the old caption in place.
func TestRequeuedImageKeepsItsCaptionWhenTheNewOneFails(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")
	oldChunk := captionWith(t, pool, eventID, imageID, "F1", "earlier-model", "first caption")
	if _, err := RequeueOtherModels(ctx, pool, "fake-model"); err != nil {
		t.Fatal(err)
	}

	w := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: ""}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	if err := w.captionOne(ctx, row); err == nil {
		t.Fatal("an empty caption should fail")
	}
	_, captioned, chunkID := getImageRow(t, pool, imageID)
	if captioned {
		t.Error("a failed attempt should leave the image waiting")
	}
	if chunkID == nil || *chunkID != oldChunk {
		t.Errorf("chunk_id = %v, want the old caption %d kept", chunkID, oldChunk)
	}
	var content string
	if err := pool.QueryRow(ctx, `SELECT content FROM event_chunks WHERE id = $1`, oldChunk).Scan(&content); err != nil {
		t.Fatalf("old caption chunk: %v", err)
	}
	if !strings.Contains(content, "first caption") {
		t.Errorf("old caption content = %q", content)
	}
}

// TestDocumentPictureIsCaptionedFromTheMirror: a picture the document worker
// found inside a document arrives with its bytes mirrored and no source that
// could serve it. The caption worker describes it from the mirror, as a
// caption chunk of the document's event, whichever connectors are running.
func TestDocumentPictureIsCaptionedFromTheMirror(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	var documentID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_documents (event_id, source_file_id, occurred_at, filename, media_type, doc_format)
		SELECT id, 'FDOC', occurred_at, 'shots.docx', 'application/msword', 'docx' FROM events WHERE id = $1
		RETURNING id`, eventID).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	picture := encodePNG(t, 64, 64)
	const key = "slack/ab/abcd.png"
	if err := events.ReconcileDocumentImages(ctx, pool, eventID, documentID, []events.DocumentImage{{
		SourceImageID: "FDOC/word/media/image1.png", Mimetype: "image/png", StorageKey: key,
		SHA256Hex: sha256Hex(picture), Bytes: int64(len(picture)),
	}}); err != nil {
		t.Fatal(err)
	}

	// No fetcher can serve it: its source is off, and would not know the
	// picture's name anyway.
	fetcher := &switchableFetcher{}
	w := newCaptionOneWorker(t, pool, &fakeCaptioner{result: &CaptionResult{Caption: "A login error dialog."}}, fetcher)
	w.S3 = &fakeS3{getKey: key, getData: picture, getCT: "image/png"}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.sourceImageID != "FDOC/word/media/image1.png" {
		t.Fatalf("claimed %+v, want the document's picture", row)
	}
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	var caption string
	var fromDocument bool
	if err := pool.QueryRow(ctx, `
		SELECT c.content, i.event_document_id = $2
		  FROM event_images i JOIN event_chunks c ON c.id = i.chunk_id
		 WHERE i.event_id = $1 AND c.kind = 'image_caption' AND i.captioned_at IS NOT NULL`,
		eventID, documentID).Scan(&caption, &fromDocument); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caption, "A login error dialog.") || !fromDocument {
		t.Errorf("caption %q, still tied to its document: %v", caption, fromDocument)
	}
}
