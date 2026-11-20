package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestReconcileDocumentsInsertsRow is the happy path: a document
// attachment becomes a pending event_documents row with occurred_at
// derived from the parent event.
func TestReconcileDocumentsInsertsRow(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "F1", Filename: "contract.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}

	var (
		filename, mediaType, docFormat string
		extractedAt, claimedAt         *string
		failures                       int
		occurredMatches                bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT d.filename, d.media_type, d.doc_format,
		       d.extracted_at::text, d.claimed_at::text, d.failure_count,
		       d.occurred_at = e.occurred_at
		  FROM event_documents d JOIN events e ON e.id = d.event_id
		 WHERE d.event_id = $1 AND d.source_file_id = 'F1'
	`, eventID).Scan(&filename, &mediaType, &docFormat,
		&extractedAt, &claimedAt, &failures, &occurredMatches); err != nil {
		t.Fatal(err)
	}
	if filename != "contract.pdf" || mediaType != "application/pdf" || docFormat != "pdf" {
		t.Errorf("row = %q/%q/%q", filename, mediaType, docFormat)
	}
	if extractedAt != nil || claimedAt != nil {
		t.Errorf("new row should be unclaimed + unextracted, got %v/%v", extractedAt, claimedAt)
	}
	if failures != 0 {
		t.Errorf("failure_count = %d, want 0", failures)
	}
	// occurred_at is denormalized from the parent so the claim query can
	// order newest-first without a join; it must never drift.
	if !occurredMatches {
		t.Error("occurred_at does not match the parent event's")
	}
}

// TestReconcileDocumentsIdempotent: re-ingesting unchanged content must
// be a no-op. If it weren't, every Slack edit or link unfurl on a
// document-bearing message would re-queue the document and re-pay for
// extraction.
func TestReconcileDocumentsIdempotent(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")
	refs := []DocumentRef{{SourceFileID: "F1", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"}}

	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	// Simulate a completed extraction, then reconcile again.
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents SET extracted_at = now(), extract_model = 'm'
		 WHERE event_id = $1
	`, eventID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}

	var extracted *string
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(extracted_at::text) FROM event_documents WHERE event_id = $1
	`, eventID).Scan(&n, &extracted); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (no duplicate)", n)
	}
	if extracted == nil {
		t.Error("re-reconcile cleared extracted_at; it must not re-queue an extracted document")
	}
}

// TestReconcileDocumentsEditDropsAttachment is the sensitive-content
// case: someone posts a document, then edits it away. Its extracted
// text must not linger in retrieval.
func TestReconcileDocumentsEditDropsAttachment(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "FA", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
		{SourceFileID: "FB", Filename: "b.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}
	// Give each document a chunk, as the worker would.
	seedDocChunks(t, pool, eventID, "FA", 2)
	seedDocChunks(t, pool, eventID, "FB", 3)

	// The edit removes FA only.
	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "FB", Filename: "b.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}

	var docs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_documents WHERE event_id = $1`, eventID).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 1 {
		t.Errorf("event_documents count = %d, want 1", docs)
	}
	// FA's chunks must be gone (cascade), FB's must survive. This is the
	// per-document precision event_chunks.event_document_id exists for.
	var total, surviving int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_chunks WHERE event_id = $1 AND kind = 'document_text'),
		       (SELECT count(*) FROM event_chunks c
		          JOIN event_documents d ON d.id = c.event_document_id
		         WHERE d.source_file_id = 'FB')
	`, eventID).Scan(&total, &surviving); err != nil {
		t.Fatal(err)
	}
	if total != 3 || surviving != 3 {
		t.Errorf("chunks: total=%d surviving-FB=%d, want 3/3 (FA's 2 chunks cascaded away)", total, surviving)
	}
}

// TestReconcileDocumentsClearsAll: removing every attachment removes
// every row. Exercises the empty-array != ALL predicate, which is the
// easy thing to get wrong (NOT IN () is a syntax error in postgres).
func TestReconcileDocumentsClearsAll(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "FA", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}
	seedDocChunks(t, pool, eventID, "FA", 2)

	if err := ReconcileDocuments(ctx, pool, eventID, nil); err != nil {
		t.Fatal(err)
	}

	var docs, chunks int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_documents WHERE event_id = $1),
		       (SELECT count(*) FROM event_chunks WHERE event_id = $1 AND kind = 'document_text')
	`, eventID).Scan(&docs, &chunks); err != nil {
		t.Fatal(err)
	}
	if docs != 0 || chunks != 0 {
		t.Errorf("docs=%d chunks=%d, want 0/0", docs, chunks)
	}
}

func TestReconcileDocumentsDoesNotResetFailures(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")
	refs := []DocumentRef{{SourceFileID: "F1", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"}}

	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents SET failure_count = $2, last_failure = 'boom' WHERE event_id = $1
	`, eventID, MaxFailures); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}

	var failures int
	var lastFailure *string
	if err := pool.QueryRow(ctx, `
		SELECT failure_count, last_failure FROM event_documents WHERE event_id = $1
	`, eventID).Scan(&failures, &lastFailure); err != nil {
		t.Fatal(err)
	}
	if failures != MaxFailures || lastFailure == nil || *lastFailure != "boom" {
		t.Errorf("failure_count=%d last_failure=%v, want unchanged", failures, lastFailure)
	}
}

func TestReconcileDocumentsDoesNotRequeueGateClosed(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:gate")
	refs := []DocumentRef{{
		SourceFileID: "F1", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf",
	}}
	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents
		   SET extracted_at = now(), rejected_reason = 'gate_closed'
		 WHERE event_id = $1
	`, eventID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, refs); err != nil {
		t.Fatal(err)
	}
	var extractedAt, rejected *string
	if err := pool.QueryRow(ctx, `
		SELECT extracted_at::text, rejected_reason
		  FROM event_documents WHERE event_id = $1
	`, eventID).Scan(&extractedAt, &rejected); err != nil {
		t.Fatal(err)
	}
	if extractedAt == nil || rejected == nil || *rejected != "gate_closed" {
		t.Errorf("row was unexpectedly requeued: extracted_at=%v rejected_reason=%v", extractedAt, rejected)
	}
}

// TestReconcileDocumentsRefreshesMetadata: a re-ingest may carry a
// corrected mimetype (Slack sometimes reports octet-stream first).
func TestReconcileDocumentsRefreshesMetadata(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "F1", Filename: "a", Mimetype: "application/octet-stream", DocFormat: ""},
	}); err != nil {
		t.Fatal(err)
	}
	// Poison it so the DO UPDATE branch is reachable, then re-reconcile
	// with better metadata.
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents SET failure_count = $2 WHERE event_id = $1
	`, eventID, MaxFailures); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "F1", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}

	var filename, mediaType string
	var docFormat *string
	if err := pool.QueryRow(ctx, `
		SELECT filename, media_type, doc_format FROM event_documents WHERE event_id = $1
	`, eventID).Scan(&filename, &mediaType, &docFormat); err != nil {
		t.Fatal(err)
	}
	if filename != "a.pdf" || mediaType != "application/pdf" || docFormat == nil || *docFormat != "pdf" {
		t.Errorf("metadata not refreshed: %q/%q/%v", filename, mediaType, docFormat)
	}
}

// TestReconcileDocumentsUnnamedFile: filename is NOT NULL, and Slack
// omits the name on some bot uploads. Ingest must not fail over it.
func TestReconcileDocumentsUnnamedFile(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "F1", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}
	var filename string
	if err := pool.QueryRow(ctx,
		`SELECT filename FROM event_documents WHERE event_id = $1`, eventID).Scan(&filename); err != nil {
		t.Fatal(err)
	}
	if filename != "(unnamed)" {
		t.Errorf("filename = %q, want the (unnamed) placeholder", filename)
	}
}

// TestEventDeleteCascadesDocuments: deleting the parent event removes
// its documents and their chunks. Retention and hard-delete both rely
// on this.
func TestEventDeleteCascadesDocuments(t *testing.T) {
	pool := getReconcilePool(t)
	ctx := context.Background()
	eventID := seedReconcileEvent(t, pool, "C1:1")

	if err := ReconcileDocuments(ctx, pool, eventID, []DocumentRef{
		{SourceFileID: "FA", Filename: "a.pdf", Mimetype: "application/pdf", DocFormat: "pdf"},
	}); err != nil {
		t.Fatal(err)
	}
	seedDocChunks(t, pool, eventID, "FA", 3)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}

	var docs, chunks int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_documents),
		       (SELECT count(*) FROM event_chunks WHERE kind = 'document_text')
	`).Scan(&docs, &chunks); err != nil {
		t.Fatal(err)
	}
	if docs != 0 || chunks != 0 {
		t.Errorf("after event delete: docs=%d chunks=%d, want 0/0", docs, chunks)
	}
}

// seedDocChunks inserts n document_text chunks owned by the named
// document, the way the extraction worker would. The event_document_id
// linkage is the point: without it these chunks couldn't be attributed
// to one of several documents on the same event.
func seedDocChunks(t *testing.T, pool *pgxpool.Pool, eventID int64, sourceFileID string, n int) {
	t.Helper()
	ctx := context.Background()
	var docID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM event_documents WHERE event_id = $1 AND source_file_id = $2
	`, eventID, sourceFileID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO event_chunks
				(event_id, kind, chunk_idx, content, event_document_id, occurred_at)
			VALUES ($1, 'document_text', $2, $3, $4, now())
		`, eventID, i, sourceFileID+" chunk", docID); err != nil {
			t.Fatal(err)
		}
	}
}
