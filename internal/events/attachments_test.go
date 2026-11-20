package events

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAttachmentEvent seeds an event in scope with one captioned, mirrored
// image (F-img), one extracted two-chunk document (F-doc), and one image the
// caption worker has not reached (F-new).
func seedAttachmentEvent(t *testing.T, pool *pgxpool.Pool, externalID, scope string) int64 {
	t.Helper()
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, externalID)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE events SET scope = $2 WHERE id = $1`, eventID, scope)
	exec(`
		WITH caption AS (
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			VALUES ($1, 'image_caption', 0, 'a whiteboard sketch', now()) RETURNING id
		)
		INSERT INTO event_images (event_id, source_image_id, storage_key, media_type, bytes, captioned_at, chunk_id, occurred_at)
		SELECT $1, 'F-img', 'images/ab/abc.png', 'image/png', 2048, now(), caption.id, now() FROM caption`, eventID)
	exec(`INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at) VALUES ($1, 'F-new', 'image/jpeg', now())`, eventID)
	exec(`
		WITH doc AS (
			INSERT INTO event_documents (event_id, source_file_id, occurred_at, filename, media_type, doc_format,
			                             storage_key, bytes, extract_model, extracted_at, page_count, pages_extracted, incomplete_reason)
			VALUES ($1, 'F-doc', now(), 'contract.pdf', 'application/pdf', 'pdf', 'docs/cd/cde.pdf', 90000, 'm', now(), 3, 2, 'cost_limit')
			RETURNING id
		)
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, event_document_id, occurred_at)
		SELECT $1, 'document_text', idx, body, doc.id, now()
		  FROM doc, (VALUES (1, 'second page'), (0, 'first page')) AS pages(idx, body)`, eventID)
	return eventID
}

func TestListAttachmentsDescribesImagesAndDocuments(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	first := seedAttachmentEvent(t, pool, "C1:1.0", ScopeChannel("C1"))
	second := seedAttachmentEvent(t, pool, "C1:2.0", ScopeChannel("C1"))
	deleted := seedAttachmentEvent(t, pool, "C1:3.0", ScopeChannel("C1"))
	if _, err := pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}

	got, err := ListAttachments(ctx, pool, []int64{second, deleted, first})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d attachments, want 3 for each live event: %+v", len(got), got)
	}
	// Events in the order asked, images before documents.
	for i, want := range []string{
		"image:2:F-img", "image:2:F-new", "document:2:F-doc",
		"image:1:F-img", "image:1:F-new", "document:1:F-doc",
	} {
		if got[i].String() != want {
			t.Errorf("attachment %d = %s, want %s", i, got[i], want)
		}
	}
	image, pending, doc := got[0], got[1], got[2]
	if image.Caption != "a whiteboard sketch" || image.StorageKey != "images/ab/abc.png" || image.MediaType != "image/png" || image.Bytes != 2048 {
		t.Errorf("captioned image = %+v", image)
	}
	if pending.Caption != "" || pending.StorageKey != "" || pending.MediaType != "image/jpeg" {
		t.Errorf("unprocessed image = %+v", pending)
	}
	if doc.Name != "contract.pdf" || doc.MediaType != "application/pdf" || doc.Bytes != 90000 ||
		!doc.Extracted || doc.IncompleteReason != "cost_limit" || doc.Text != "" {
		t.Errorf("document = %+v", doc)
	}

	if got, err := ListAttachments(ctx, pool, nil); err != nil || got != nil {
		t.Errorf("no events: got %v, %v", got, err)
	}
}

// TestGetAttachmentIsScopedToTheEvent: an attachment is readable exactly when
// its event is live and in the caller's scopes; every other case is the one
// not-found.
func TestGetAttachmentIsScopedToTheEvent(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	open := seedAttachmentEvent(t, pool, "C1:1.0", ScopeChannel("C1"))
	private := seedAttachmentEvent(t, pool, "G2:1.0", ScopeChannel("G2"))
	unstamped := seedAttachmentEvent(t, pool, "C3:1.0", ScopeUnstamped)
	deleted := seedAttachmentEvent(t, pool, "C1:9.0", ScopeChannel("C1"))
	if _, err := pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}
	outsider := []string{ScopePublic, ScopeInternal, ScopeChannel("C1")}
	member := append([]string{ScopeChannel("G2")}, outsider...)
	ref := func(eventID int64, kind, fileID string) AttachmentRef {
		return AttachmentRef{EventID: eventID, Kind: kind, FileID: fileID}
	}

	doc, err := GetAttachment(ctx, pool, ref(open, AttachmentDocument, "F-doc"), outsider)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != "first page\n\nsecond page" || doc.Name != "contract.pdf" {
		t.Errorf("document = %+v, want its chunks joined in order", doc)
	}
	image, err := GetAttachment(ctx, pool, ref(open, AttachmentImage, "F-img"), outsider)
	if err != nil {
		t.Fatal(err)
	}
	if image.Caption != "a whiteboard sketch" || image.StorageKey == "" || image.Text != "" {
		t.Errorf("image = %+v", image)
	}

	for name, attempt := range map[string]struct {
		ref    AttachmentRef
		scopes []string
	}{
		"private image, outsider":    {ref(private, AttachmentImage, "F-img"), outsider},
		"private document, outsider": {ref(private, AttachmentDocument, "F-doc"), outsider},
		"no scopes":                  {ref(open, AttachmentImage, "F-img"), nil},
		"unstamped event":            {ref(unstamped, AttachmentImage, "F-img"), []string{ScopeUnstamped, ScopePublic}},
		"deleted event":              {ref(deleted, AttachmentDocument, "F-doc"), outsider},
		"unknown file":               {ref(open, AttachmentImage, "F-nope"), outsider},
		"file of the other kind":     {ref(open, AttachmentImage, "F-doc"), outsider},
		"unknown event":              {ref(999999, AttachmentImage, "F-img"), outsider},
	} {
		if got, err := GetAttachment(ctx, pool, attempt.ref, attempt.scopes); !errors.Is(err, ErrAttachmentNotFound) {
			t.Errorf("%s: got %+v, %v; want ErrAttachmentNotFound", name, got, err)
		}
	}

	if got, err := GetAttachment(ctx, pool, ref(private, AttachmentDocument, "F-doc"), member); err != nil || got.Text == "" {
		t.Errorf("member reading the private document: %+v, %v", got, err)
	}
}

func TestParseAttachmentRefRejectsMalformedIDs(t *testing.T) {
	for _, id := range []string{"", "image", "image:7", "image:7:", "image:0:F", "image:-1:F", "image:x:F", "video:7:F", ":7:F"} {
		if ref, err := ParseAttachmentRef(id); err == nil {
			t.Errorf("ParseAttachmentRef(%q) = %+v, want an error", id, ref)
		}
	}
	ref, err := ParseAttachmentRef(" document:42:block:key ")
	if err != nil || ref != (AttachmentRef{EventID: 42, Kind: AttachmentDocument, FileID: "block:key"}) {
		t.Errorf("ParseAttachmentRef = %+v, %v", ref, err)
	}
}
