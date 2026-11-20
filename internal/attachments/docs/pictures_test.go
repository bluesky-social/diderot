package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
)

// docxPart is one file inside a test .docx.
type docxPart struct {
	name string
	data []byte
}

// picture is a stand-in for a pasted screenshot: bytes of a plausible size.
// Nothing here decodes it; the caption worker is what reads a picture.
func picture(fill byte, size int) []byte { return bytes.Repeat([]byte{fill}, size) }

// newTestDocxWith assembles a .docx whose body is bodyXML and which stores
// the given parts beside it, in order.
func newTestDocxWith(t *testing.T, bodyXML string, parts ...docxPart) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	write("word/document.xml", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>`+bodyXML+`</w:body></w:document>`))
	for _, part := range parts {
		write(part.name, part.data)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// People paste screenshots into a Word file and send that. The parser hands
// back the pictures it can have described and leaves out what it cannot.
func TestDocxPicturesAreReturnedForCaptions(t *testing.T) {
	data := newTestDocxWith(t, para(""),
		docxPart{"word/media/image1.png", picture(1, 8<<10)},
		docxPart{"word/media/image2.JPG", picture(2, 16<<10)},
		docxPart{"word/media/bullet.png", picture(3, 512)},          // an icon, not a screenshot
		docxPart{"word/media/diagram.emf", picture(4, 8<<10)},       // a format the caption model can't read
		docxPart{"word/header/media/logo.png", picture(5, 8<<10)},   // not stored with the main text
		docxPart{"word/media/nested/image9.png", picture(6, 8<<10)}, // nor this
		docxPart{"word/media/image3.jpeg", picture(7, 8<<10)},
	)
	result, err := DocumentParser{}.Extract(context.Background(), "docx", "shots.docx", data)
	require.NoError(t, err, "a Word file with no text is not an error")
	assert.Empty(t, result.Markdown)
	require.Len(t, result.Pictures, 3)
	assert.Equal(t, []string{"word/media/image1.png", "word/media/image2.JPG", "word/media/image3.jpeg"},
		[]string{result.Pictures[0].Name, result.Pictures[1].Name, result.Pictures[2].Name}, "in the order the file stores them")
	assert.Equal(t, []string{"image/png", "image/jpeg", "image/jpeg"},
		[]string{result.Pictures[0].MediaType, result.Pictures[1].MediaType, result.Pictures[2].MediaType})
	assert.Equal(t, picture(2, 16<<10), result.Pictures[1].Data)

	// A document with text keeps its text and its pictures.
	both, err := DocumentParser{}.Extract(context.Background(), "docx", "notes.docx",
		newTestDocxWith(t, para("See the error below."), docxPart{"word/media/image1.png", picture(1, 8<<10)}))
	require.NoError(t, err)
	assert.Contains(t, both.Markdown, "See the error below.")
	assert.Len(t, both.Pictures, 1)

	// Neither text nor pictures is still not an error: the worker settles it.
	neither, err := DocumentParser{}.Extract(context.Background(), "docx", "blank.docx", newTestDocx(t, para("")+para("   ")))
	require.NoError(t, err)
	assert.Empty(t, neither.Markdown)
	assert.Empty(t, neither.Pictures)
}

// Each picture costs a caption call, so one document's are capped.
func TestDocxPicturesAreCappedPerDocument(t *testing.T) {
	var parts []docxPart
	for i := 1; i <= MaxPictures+15; i++ {
		parts = append(parts, docxPart{fmt.Sprintf("word/media/image%d.png", i), picture(byte(i), 8<<10)})
	}
	result, err := DocumentParser{}.Extract(context.Background(), "docx", "album.docx", newTestDocxWith(t, para("An album."), parts...))
	require.NoError(t, err)
	require.Len(t, result.Pictures, MaxPictures)
	assert.Equal(t, "word/media/image1.png", result.Pictures[0].Name)
	assert.Equal(t, fmt.Sprintf("word/media/image%d.png", MaxPictures), result.Pictures[MaxPictures-1].Name, "the first ones stored")
}

// docxDocument points a seeded document row at a Word file the fetcher
// serves.
func docxDocument(t *testing.T, w *Worker, docID int64, data []byte) {
	t.Helper()
	_, err := w.Pool.Exec(context.Background(), `
		UPDATE event_documents
		   SET filename = 'shots.docx', doc_format = 'docx',
		       media_type = 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
		 WHERE id = $1`, docID)
	require.NoError(t, err)
	w.Fetchers = map[string]files.Fetcher{"slack": &fakeFetcher{data: data}}
}

type storedPicture struct {
	SourceImageID, MediaType, StorageKey string
	Bytes                                int64
	Waiting                              bool
}

func storedPictures(t *testing.T, w *Worker, docID int64) []storedPicture {
	t.Helper()
	rows, err := w.Pool.Query(context.Background(), `
		SELECT source_image_id, media_type, storage_key, bytes, captioned_at IS NULL AND failure_count = 0
		  FROM event_images WHERE event_document_id = $1 ORDER BY id`, docID)
	require.NoError(t, err)
	defer rows.Close()
	var out []storedPicture
	for rows.Next() {
		var p storedPicture
		require.NoError(t, rows.Scan(&p.SourceImageID, &p.MediaType, &p.StorageKey, &p.Bytes, &p.Waiting))
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

// A Word file that is only screenshots used to fail as having no text. Its
// pictures now wait for captions as images of the same message, with their
// bytes already mirrored, and the document settles instead of failing.
func TestWorkerQueuesADocumentsPicturesForCaptions(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docID := seedDoc(t, pool, "CYES", "F1", true)
	w := newTestWorker(pool, &fakeExtractor{})
	first, second := picture(1, 8<<10), picture(2, 16<<10)
	docxDocument(t, w, docID, newTestDocxWith(t, para(""),
		docxPart{"word/media/image1.png", first}, docxPart{"word/media/image2.jpeg", second}))

	row, err := w.claimPending(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NoError(t, w.extractOne(ctx, row))

	pictures := storedPictures(t, w, docID)
	require.Len(t, pictures, 2)
	assert.Equal(t, "F1/word/media/image1.png", pictures[0].SourceImageID)
	assert.Equal(t, "image/png", pictures[0].MediaType)
	assert.Equal(t, int64(len(first)), pictures[0].Bytes)
	assert.True(t, pictures[0].Waiting, "it waits in the caption queue")
	assert.Equal(t, files.StorageKeyFor("slack", files.SHA256Hex(second), ".jpeg"), pictures[1].StorageKey)
	mirrored, _, err := w.S3.Get(ctx, pictures[1].StorageKey)
	require.NoError(t, err, "the caption worker reads the picture from the mirror")
	assert.Equal(t, second, mirrored)

	var settled bool
	var rejected, lastFailure *string
	var failures int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT extracted_at IS NOT NULL, rejected_reason, last_failure, failure_count
		  FROM event_documents WHERE id = $1`, docID).Scan(&settled, &rejected, &lastFailure, &failures))
	assert.True(t, settled)
	require.NotNil(t, rejected)
	assert.Equal(t, "empty", *rejected, "the document itself has no text, and says so")
	assert.Nil(t, lastFailure, "having no text is not a failure")
	assert.Zero(t, failures)

	// The pictures are attachments of the message, beside the document.
	attachments, err := events.ListAttachments(ctx, pool, []int64{eventID})
	require.NoError(t, err)
	var kinds []string
	for _, attachment := range attachments {
		kinds = append(kinds, attachment.Kind+" "+attachment.FileID)
	}
	assert.Equal(t, []string{"image F1/word/media/image1.png", "image F1/word/media/image2.jpeg", "document F1"}, kinds)

	// Reading the document again records nothing twice and re-queues nothing.
	_, err = pool.Exec(ctx, `UPDATE event_images SET captioned_at = now() WHERE event_document_id = $1`, docID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE event_documents SET extracted_at = NULL, rejected_reason = NULL, claimed_at = NULL WHERE id = $1`, docID)
	require.NoError(t, err)
	row, err = w.claimPending(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NoError(t, w.extractOne(ctx, row))
	again := storedPictures(t, w, docID)
	require.Len(t, again, 2)
	assert.False(t, again[0].Waiting, "a picture already described is not described again")
}

// A document with text and pictures gets both. A channel that opts out of
// document reading gives up the pictures with the text.
func TestWorkerKeepsADocumentsTextAndPicturesUntilItsChannelOptsOut(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docID := seedDoc(t, pool, "CYES", "F1", true)
	w := newTestWorker(pool, &fakeExtractor{})
	docxDocument(t, w, docID, newTestDocxWith(t, para("The login page shows this error."),
		docxPart{"word/media/image1.png", picture(1, 8<<10)}))

	row, err := w.claimPending(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NoError(t, w.extractOne(ctx, row))
	var textChunks int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE event_document_id = $1`, docID).Scan(&textChunks))
	assert.Equal(t, 1, textChunks)
	require.Len(t, storedPictures(t, w, docID), 1)

	// The picture is described, then the channel opts out and the document
	// is settled again.
	var captionID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, '[Image] A login error.', now()) RETURNING id`, eventID).Scan(&captionID))
	_, err = pool.Exec(ctx, `UPDATE event_images SET captioned_at = now(), chunk_id = $2 WHERE event_document_id = $1`, docID, captionID)
	require.NoError(t, err)
	require.NoError(t, w.settleRejected(ctx, docID, "gate_closed", nil))
	assert.Empty(t, storedPictures(t, w, docID))
	var captions int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE event_id = $1`, eventID).Scan(&captions))
	assert.Zero(t, captions, "neither the text nor the picture's caption stays searchable")
}

// A document that is only pictures settles without the commit that checks
// the channel's opt-in one last time. If the channel opts out while the
// document is being read, after its pictures were recorded, settling it
// must take them back.
func TestWorkerDropsPicturesWhenTheChannelOptsOutBeforeTheDocumentSettles(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CYES", "F1", true)
	w := newTestWorker(pool, &fakeExtractor{})
	data := newTestDocxWith(t, para(""), docxPart{"word/media/image1.png", picture(1, 8<<10)})
	docxDocument(t, w, docID, data)
	row, err := w.claimPending(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	result, err := DocumentParser{}.Extract(ctx, "docx", "shots.docx", data)
	require.NoError(t, err)
	require.NoError(t, w.storePictures(ctx, row, result.Pictures))
	require.Len(t, storedPictures(t, w, docID), 1)

	_, err = pool.Exec(ctx, `UPDATE slack_channel_config SET doc_ingest_enabled = false WHERE channel_id = 'CYES'`)
	require.NoError(t, err)
	require.NoError(t, w.settleRejected(ctx, docID, string(RejectEmpty), result))
	assert.Empty(t, storedPictures(t, w, docID), "pictures recorded before the opt-out are taken back")

	// Opted out before the pictures are recorded, none is queued at all: a
	// queued picture is sent to the caption model at once.
	require.NoError(t, w.storePictures(ctx, row, result.Pictures))

	assert.Empty(t, storedPictures(t, w, docID), "nothing of the document is left to be described")
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT rejected_reason FROM event_documents WHERE id = $1`, docID).Scan(&reason))
	assert.Equal(t, "gate_closed", reason)
}
