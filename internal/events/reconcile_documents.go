package events

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DocumentRef is the source-independent identity and extraction metadata for
// one attachment.
type DocumentRef struct {
	SourceFileID string
	Filename     string
	Mimetype     string
	DocFormat    string
}

// DocFormatForMediaType returns a supported document format. The filename is
// used only when the source MIME type is missing or generic.
func DocFormatForMediaType(mediaType, filename string) string {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	switch mt {
	case "application/pdf":
		return "pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	}
	// Slack reports application/octet-stream for a fair number of
	// uploads, so fall back to the filename extension
	// for those rather than dropping a readable document. Consulted ONLY
	// when the mimetype is uninformative: a definitive mimetype always
	// wins, since a filename extension is chosen by whoever uploaded the
	// file and shouldn't be able to override the server's own typing.
	if mt == "" || mt == "application/octet-stream" {
		lower := strings.ToLower(filename)
		for ext, format := range map[string]string{
			".pdf": "pdf", ".docx": "docx", ".xlsx": "xlsx",
		} {
			if strings.HasSuffix(lower, ext) {
				return format
			}
		}
	}
	return ""
}

// ReconcileDocuments makes one event's document rows match refs. Removed rows
// cascade to their chunks and checkpoints, and take the pictures found inside
// them and those pictures' captions; unchanged rows keep extraction state.
func ReconcileDocuments(
	ctx context.Context,
	pool *pgxpool.Pool,
	eventID int64,
	refs []DocumentRef,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The document worker records a document's pictures holding this lock
	// (ReconcileDocumentImagesTx). Taking it first means none can arrive
	// between the cleanup of a removed document's pictures below and the
	// removal itself: one that did would be deleted with the document while
	// its caption stayed searchable.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, eventID); err != nil {
		return fmt.Errorf("lock event: %w", err)
	}

	// Empty is valid (poster removed every document attachment); the
	// != ALL($2::text[]) predicate handles it because empty-array
	// != ALL is vacuously true, so every existing row for this event is
	// deleted — which is exactly right.
	currentIDs := make([]string, 0, len(refs))
	for _, r := range refs {
		currentIDs = append(currentIDs, r.SourceFileID)
	}

	// A removed document's pictures go with it (the image rows cascade),
	// but a picture's caption is a chunk of its own that nothing cascades
	// to: delete those first, or they would stay searchable.
	if err := deleteDocumentImages(ctx, tx, `event_document_id IN (
		SELECT id FROM event_documents WHERE event_id = $1 AND source_file_id != ALL($2::text[]))`,
		eventID, currentIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_documents
		 WHERE event_id = $1
		   AND source_file_id != ALL($2::text[])
	`, eventID, currentIDs); err != nil {
		return fmt.Errorf("delete stale documents: %w", err)
	}

	// occurred_at is derived from the parent event via the SELECT rather
	// than passed in, so it can never drift from the row it mirrors. The
	// DO UPDATE branch deliberately leaves it alone: it's immutable once
	// set, and a poison-reset re-share should keep the original
	// recency rather than jumping the queue at reconcile time.
	//
	// Re-ingest may refresh metadata, but it is not an implicit retry.
	for _, r := range refs {
		_, err := tx.Exec(ctx, `
			INSERT INTO event_documents
				(event_id, source_file_id, occurred_at, filename, media_type, doc_format)
			SELECT $1, $2, e.occurred_at, $3, $4, $5 FROM events e WHERE e.id = $1
			ON CONFLICT (event_id, source_file_id) DO UPDATE SET
				filename   = EXCLUDED.filename,
				media_type = EXCLUDED.media_type,
				doc_format = EXCLUDED.doc_format
		`, eventID, r.SourceFileID, defaultFilename(r.Filename),
			defaultMediaType(r.Mimetype), nullIfEmpty(r.DocFormat))
		if err != nil {
			return fmt.Errorf("upsert event_documents %s: %w", r.SourceFileID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

// defaultFilename keeps the NOT NULL filename column populated when a
// source omits the name (Slack occasionally does on bot uploads), and
// sanitizes it: an attachment filename is as much externally-controlled
// text as a message body, and this insert runs right after the sink's
// own (sanitized) Upsert — it must not undo that by handing Postgres a
// raw NUL byte of its own.
func defaultFilename(s string) string {
	s = SanitizeText(s)
	if s == "" {
		return "(unnamed)"
	}
	return s
}

// nullIfEmpty maps "" to a SQL NULL so doc_format is genuinely absent
// rather than an empty string when unresolved.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
