package events

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The kinds of attachment an event can carry: an image the caption worker
// tracks (event_images) or a document the extraction worker tracks
// (event_documents).
const (
	AttachmentImage    = "image"
	AttachmentDocument = "document"
)

// ErrAttachmentNotFound means no such attachment is readable: it does not
// exist, its event is deleted, or the event is outside the caller's scopes.
var ErrAttachmentNotFound = errors.New("attachment not found")

// AttachmentRef names one attachment of one event. FileID is the source's own
// file identifier, unique within the event and kind.
type AttachmentRef struct {
	EventID int64
	Kind    string
	FileID  string
}

// String is the reference's stable text form, "<kind>:<event_id>:<file_id>",
// which ParseAttachmentRef reads back.
func (r AttachmentRef) String() string {
	return r.Kind + ":" + strconv.FormatInt(r.EventID, 10) + ":" + r.FileID
}

// ParseAttachmentRef reads the form String writes. A file ID may itself
// contain colons.
func ParseAttachmentRef(s string) (AttachmentRef, error) {
	kind, rest, _ := strings.Cut(strings.TrimSpace(s), ":")
	event, fileID, _ := strings.Cut(rest, ":")
	eventID, err := strconv.ParseInt(event, 10, 64)
	if (kind != AttachmentImage && kind != AttachmentDocument) || err != nil || eventID <= 0 || fileID == "" {
		return AttachmentRef{}, fmt.Errorf("attachment id %q is not one an expand result lists", s)
	}
	return AttachmentRef{EventID: eventID, Kind: kind, FileID: fileID}, nil
}

// Attachment is one file attached to an event, as the derived tables know it.
type Attachment struct {
	AttachmentRef
	// Name is the file name. The image table records none, so it is empty
	// for an image unless the event's source fills it in.
	Name      string
	MediaType string
	Bytes     int64
	// StorageKey locates the mirrored bytes; empty until they are fetched.
	StorageKey string
	// Caption is an image's generated description, empty until captioned.
	Caption string
	// Extracted, RejectedReason and IncompleteReason are a document's
	// extraction state: not yet extracted, extracted, refused, or cut short.
	Extracted        bool
	RejectedReason   string
	IncompleteReason string
	// Text is a document's extracted text. Only GetAttachment fills it.
	Text string
}

const attachmentColumns = `
	SELECT event_id, kind, file_id, name, media_type, bytes, storage_key,
	       caption, extracted, rejected_reason, incomplete_reason
	  FROM (
	    SELECT ei.event_id, 'image'::text AS kind, ei.source_image_id AS file_id,
	           ''::text AS name, ei.media_type, COALESCE(ei.bytes, 0) AS bytes,
	           COALESCE(ei.storage_key, '') AS storage_key,
	           COALESCE(ec.content, '') AS caption,
	           false AS extracted, ''::text AS rejected_reason, ''::text AS incomplete_reason,
	           ei.id
	      FROM event_images ei
	      LEFT JOIN event_chunks ec ON ec.id = ei.chunk_id AND ec.kind = 'image_caption'
	    UNION ALL
	    SELECT ed.event_id, 'document'::text, ed.source_file_id,
	           ed.filename, ed.media_type, COALESCE(ed.bytes, 0),
	           COALESCE(ed.storage_key, ''), ''::text,
	           ed.extracted_at IS NOT NULL, COALESCE(ed.rejected_reason, ''),
	           COALESCE(ed.incomplete_reason, ''), ed.id
	      FROM event_documents ed
	  ) a`

func scanAttachment(row pgx.CollectableRow) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.EventID, &a.Kind, &a.FileID, &a.Name, &a.MediaType, &a.Bytes,
		&a.StorageKey, &a.Caption, &a.Extracted, &a.RejectedReason, &a.IncompleteReason)
	return a, err
}

// ListAttachments returns the attachments of the given events, grouped by
// event in eventIDs' order, images before documents. It applies no visibility
// check: the caller passes only events it has already authorized.
func ListAttachments(ctx context.Context, pool *pgxpool.Pool, eventIDs []int64) ([]Attachment, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, attachmentColumns+`
		  JOIN events e ON e.id = a.event_id
		 WHERE a.event_id = ANY($1::bigint[]) AND e.deleted = false
		 ORDER BY array_position($1::bigint[], a.event_id), a.kind DESC, a.id`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	attachments, err := pgx.CollectRows(rows, scanAttachment)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	return attachments, nil
}

// GetAttachment returns one attachment, with a document's extracted text, when
// its event is live and in scopes. Anything else is ErrAttachmentNotFound, so
// a hidden attachment reads exactly like a missing one.
func GetAttachment(ctx context.Context, pool *pgxpool.Pool, ref AttachmentRef, scopes []string) (Attachment, error) {
	rows, err := pool.Query(ctx, attachmentColumns+`
		  JOIN events e ON e.id = a.event_id
		 WHERE a.event_id = $1 AND a.kind = $2 AND a.file_id = $3
		   AND e.deleted = false AND e.scope = ANY($4)`,
		ref.EventID, ref.Kind, ref.FileID, ReadableScopes(scopes))
	if err != nil {
		return Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	attachment, err := pgx.CollectExactlyOneRow(rows, scanAttachment)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrAttachmentNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	if attachment.Kind != AttachmentDocument {
		return attachment, nil
	}
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(string_agg(c.content, E'\n\n' ORDER BY c.chunk_idx), '')
		  FROM event_documents ed
		  JOIN event_chunks c ON c.event_document_id = ed.id
		 WHERE ed.event_id = $1 AND ed.source_file_id = $2`,
		ref.EventID, ref.FileID).Scan(&attachment.Text)
	if err != nil {
		return Attachment{}, fmt.Errorf("get attachment text: %w", err)
	}
	return attachment, nil
}
