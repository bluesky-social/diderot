package slack

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// attachFiles records raw as the message's stored Slack payload and marks it
// as carrying files, as ingest does for a message with a files array.
func attachFiles(t *testing.T, pool *pgxpool.Pool, eventID int64, raw string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE events SET raw = $2::jsonb WHERE id = $1`, eventID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_message_details SET has_files = true WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
}

// seedCaptionedImage adds a mirrored, captioned image to an event.
func seedCaptionedImage(t *testing.T, pool *pgxpool.Pool, eventID int64, fileID, caption string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		WITH chunk AS (
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			VALUES ($1, 'image_caption', 100, $3, now()) RETURNING id
		)
		INSERT INTO event_images (event_id, source_image_id, storage_key, media_type, bytes, captioned_at, chunk_id, occurred_at)
		SELECT $1, $2, 'images/' || $2, 'image/png', 4096, now(), chunk.id, now() FROM chunk
	`, eventID, fileID, caption); err != nil {
		t.Fatal(err)
	}
}

// TestSlackExpandListsThreadAttachments: expand lists every file on the
// thread's messages, in message and Slack order: the captioned image and the
// document Diderot holds with their ids, and a file it did not ingest by
// name alone.
func TestSlackExpandListsThreadAttachments(t *testing.T) {
	pool := getExpandPool(t)
	root := insertSlackMessage(t, pool, "CFILES", "files", tsFor(0), tsFor(0), "alice", "see the graph", at(0))
	reply := insertSlackMessage(t, pool, "CFILES", "", tsFor(1), tsFor(0), "bob", "and the contract", at(1))
	insertSlackMessage(t, pool, "CFILES", "", tsFor(2), tsFor(0), "carol", "no files here", at(2))

	attachFiles(t, pool, root, `{"type":"message","channel":"CFILES","ts":"1.0","files":[
		{"id":"FSNIP","name":"trace.txt","mimetype":"text/plain","size":120,"mode":"snippet"},
		{"id":"FIMG","name":"latency.png","title":"p99 latency","mimetype":"image/png","size":4096,"mode":"hosted"}]}`)
	seedCaptionedImage(t, pool, root, "FIMG", "a graph of p99 latency climbing")
	attachFiles(t, pool, reply, `{"type":"message","channel":"CFILES","ts":"2.0","files":[
		{"id":"F-contract.pdf","name":"contract.pdf","mimetype":"application/pdf","size":90000,"mode":"hosted"}]}`)
	seedDocForExpand(t, pool, reply, "contract.pdf", 2, "clause one")

	// Expanding from the reply returns the same thread and the same files.
	for _, eventID := range []int64{root, reply} {
		res, err := NewExpander(pool).Expand(context.Background(), eventID, []string{"public"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Attachments) != 3 {
			t.Fatalf("attachments = %+v, want 3", res.Attachments)
		}
		snippet, image, doc := res.Attachments[0], res.Attachments[1], res.Attachments[2]
		if snippet.Kind != "" || snippet.EventID != root || snippet.Name != "trace.txt" || snippet.MediaType != "text/plain" || snippet.Bytes != 120 {
			t.Errorf("uningested file = %+v, want its name and no kind", snippet)
		}
		if image.AttachmentRef != (events.AttachmentRef{EventID: root, Kind: events.AttachmentImage, FileID: "FIMG"}) ||
			image.Name != "latency.png" || image.Caption != "a graph of p99 latency climbing" || image.Bytes != 4096 {
			t.Errorf("image = %+v", image)
		}
		if doc.Kind != events.AttachmentDocument || doc.EventID != reply || doc.Name != "contract.pdf" || !doc.Extracted {
			t.Errorf("document = %+v", doc)
		}
	}
}

// TestSlackExpandAttachmentsFollowMessageScope: a file on a message the
// caller cannot read is not listed, in a thread or a channel window, even
// when the message beside it is readable.
func TestSlackExpandAttachmentsFollowMessageScope(t *testing.T) {
	for _, threaded := range []bool{true, false} {
		pool := getExpandPool(t)
		thread := ""
		if threaded {
			thread = tsFor(0)
		}
		visible := insertSlackMessage(t, pool, "CMIXED", "mixed", tsFor(0), thread, "alice", "visible", at(0))
		hidden := insertSlackMessage(t, pool, "CMIXED", "", tsFor(1), thread, "bob", "hidden", at(1))
		if _, err := pool.Exec(context.Background(), `UPDATE events SET scope = 'slack-channel:GPRIVATE' WHERE id = $1`, hidden); err != nil {
			t.Fatal(err)
		}
		for eventID, fileID := range map[int64]string{visible: "FVISIBLE", hidden: "FHIDDEN"} {
			attachFiles(t, pool, eventID, `{"type":"message","channel":"CMIXED","ts":"1.0","files":[
				{"id":"`+fileID+`","name":"`+fileID+`.png","mimetype":"image/png","mode":"hosted"}]}`)
			seedCaptionedImage(t, pool, eventID, fileID, "caption of "+fileID)
		}

		res, err := NewExpander(pool).Expand(context.Background(), visible, []string{"public"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Attachments) != 1 || res.Attachments[0].FileID != "FVISIBLE" {
			t.Errorf("threaded=%v: attachments = %+v, want only the readable message's file", threaded, res.Attachments)
		}

		res, err = NewExpander(pool).Expand(context.Background(), visible, []string{"public", "slack-channel:GPRIVATE"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Attachments) != 2 {
			t.Errorf("threaded=%v: a member of both scopes sees both files, got %+v", threaded, res.Attachments)
		}
	}
}

func TestSlackExpandWithoutFilesListsNoAttachments(t *testing.T) {
	pool := getExpandPool(t)
	id := insertSlackMessage(t, pool, "CPLAIN", "plain", tsFor(0), "", "alice", "just text", at(0))
	res, err := NewExpander(pool).Expand(context.Background(), id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attachments) != 0 {
		t.Errorf("attachments = %+v, want none", res.Attachments)
	}
}
