package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Document-appendix tests for the expand path.
//
// The appendix exists because retrieval alone returns the single
// best-matching chunk, and a question like "review the attached PDF and
// tell me what you think" has no phrase that matches page 4. Expand is
// how the model reads the whole document once it knows one is there —
// which is the literal request that motivated document ingest.

// seedDocForExpand attaches an extracted document with n chunks to an
// existing event, the way the extraction worker does.
func seedDocForExpand(t *testing.T, pool *pgxpool.Pool, eventID int64, filename string, pages int, chunkTexts ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var docID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format,
			 extract_model, extracted_at, page_count, pages_extracted)
		SELECT $1, $2, e.occurred_at, $3, 'application/pdf', 'pdf',
		       'test-model', now(), $4, $4
		  FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID, "F-"+filename, filename, pages).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	for i, txt := range chunkTexts {
		if _, err := pool.Exec(ctx, `
			INSERT INTO event_chunks
				(event_id, kind, chunk_idx, content, event_document_id, occurred_at)
			VALUES ($1, 'document_text', $2, $3, $4, now())
		`, eventID, i, txt, docID); err != nil {
			t.Fatal(err)
		}
	}
	return docID
}

func TestExpandIncludesDocumentText(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	// A thread: someone shares a contract, someone else asks about it.
	rootID := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "1700000000.000100",
		"chester", "here is the request document", base)
	insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000200", "1700000000.000100",
		"sloan", "can you review this", base.Add(time.Minute))

	seedDocForExpand(t, pool, rootID, "contract.pdf", 2,
		"Retention for subscriber records is eighteen months.",
		"Escalation contact for court orders is legal-ops.")

	e := NewExpander(pool)
	// Expand the ASKING message, not the one carrying the document —
	// the appendix must cover the whole thread, since the match rarely
	// lands on the attachment itself.
	res, err := e.Expand(ctx, rootID+1, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"contract.pdf",
		"eighteen months",
		"legal-ops",
		// The model must be told this text isn't the thread's words.
		"not written by the people in the thread",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("expand output missing %q\n---\n%s", want, res.Content)
		}
	}
	// Chunks must be joined in order, not interleaved arbitrarily.
	if i, j := strings.Index(res.Content, "eighteen months"),
		strings.Index(res.Content, "legal-ops"); i > j {
		t.Error("document chunks appear out of chunk_idx order")
	}
}

func TestExpandMarksPartialDocumentText(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "",
		"chester", "see attached", base)
	docID := seedDocForExpand(t, pool, id, "partial.pdf", 5,
		"Pages one through three were extracted.")
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents
		   SET incomplete_reason = 'cost_limit', pages_extracted = 3
		 WHERE id = $1
	`, docID); err != nil {
		t.Fatal(err)
	}

	res, err := NewExpander(pool).Expand(ctx, id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PARTIAL extraction", "cost_limit", "3 of 5 pages extracted"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("partial expand output missing %q:\n%s", want, res.Content)
		}
	}
}

// TestExpandPendingDocumentIsAnnounced covers the fast-follow-up case:
// someone posts a PDF and @-mentions diderot in the same breath.
// Extraction takes a poll interval plus a model call, so the text does
// not exist yet.
//
// Announcing it is the whole point. Silently omitting a pending document
// would make the model answer as though the attachment weren't there —
// a narrower, harder-to-notice version of the exact bug this feature
// exists to fix.
func TestExpandPendingDocumentIsAnnounced(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "", "chester", "see attached", base)

	// Pending: enqueued, not yet extracted, no chunks.
	if _, err := pool.Exec(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format)
		SELECT $1, 'FPEND', e.occurred_at, 'pending.pdf', 'application/pdf', 'pdf'
		  FROM events e WHERE e.id = $1
	`, id); err != nil {
		t.Fatal(err)
	}

	e := NewExpander(pool)
	res, err := e.Expand(ctx, id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "pending.pdf") {
		t.Errorf("pending document not named:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "not extracted yet") {
		t.Errorf("pending state not communicated to the model:\n%s", res.Content)
	}
	// And it must not imply the document was empty.
	if strings.Contains(res.Content, "budget exhausted") {
		t.Errorf("wrong marker for a pending document:\n%s", res.Content)
	}
}

// TestExpandOmitsRejectedDocuments: a document that settled with a
// rejection (refusal, degenerate output, unreadable) has nothing to say
// and nothing to wait for. Unlike the pending case, advertising it would
// only surface a failure the reader can't act on.
func TestExpandOmitsRejectedDocuments(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "", "chester", "see attached", base)

	if _, err := pool.Exec(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format,
			 extracted_at, rejected_reason)
		SELECT $1, 'FREJ', e.occurred_at, 'refused.pdf', 'application/pdf', 'pdf',
		       now(), 'refusal'
		  FROM events e WHERE e.id = $1
	`, id); err != nil {
		t.Fatal(err)
	}

	e := NewExpander(pool)
	res, err := e.Expand(ctx, id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "Attached documents") {
		t.Errorf("appendix rendered for a rejected-only document:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "refused.pdf") {
		t.Errorf("output names a document with no extracted text:\n%s", res.Content)
	}
}

// TestExpandBoundsDocumentText: a very long document must not consume
// the whole model context. The per-document cap applies and the model
// is told text was dropped.
func TestExpandBoundsDocumentText(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "", "chester", "see attached", base)

	huge := strings.Repeat("Retention schedule paragraph with real content. ", 2000)
	seedDocForExpand(t, pool, id, "huge.pdf", 400, huge)

	e := NewExpander(pool)
	res, err := e.Expand(ctx, id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Error("oversized document not marked as truncated")
	}
	// Budget is in runes; allow generous headroom for the thread text
	// and headers, but the appendix must not run to the full document.
	if n := len([]rune(res.Content)); n > docAppendixBudget+8000 {
		t.Errorf("expand output is %d runes, over the appendix budget", n)
	}
}

// TestExpandDocumentAppendixStaysInChannel is the security property.
//
// The appendix follows the whole rendered thread/window rather than just
// the matched message, so its safety rests on the expander's own queries
// being channel-scoped (`d.channel_id = $1` for threads, `JOIN matched m
// ON m.channel_id` for neighbors). A channel has exactly one scope, so a
// caller authorized for the matched event is authorized for every
// message the expander renders — and therefore for their documents. If
// someone ever widens those queries across channels, this test is what
// catches that a private document became reachable from elsewhere.
func TestExpandDocumentAppendixStaysInChannel(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	// Same timestamps in two different channels, so a channel-blind
	// query would happily mix them.
	publicID := insertSlackMessage(t, pool, "CPUB", "general",
		"1700000000.000100", "", "alice", "public chatter", base)
	privateID := insertSlackMessage(t, pool, "CPRIV", "policy-private",
		"1700000000.000101", "", "chester", "confidential attachment", base)

	seedDocForExpand(t, pool, privateID, "secret-contract.pdf", 1,
		"CONFIDENTIAL settlement amount is four million dollars.")

	e := NewExpander(pool)
	res, err := e.Expand(ctx, publicID, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"secret-contract.pdf", "four million", "CONFIDENTIAL"} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("LEAK: expanding a public message surfaced %q from another channel\n---\n%s",
				leaked, res.Content)
		}
	}

	// Positive control: the document IS reachable from its own channel,
	// so the negative result above isn't just "the appendix never runs."
	res, err = e.Expand(ctx, privateID, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "four million") {
		t.Errorf("document not reachable from its own channel:\n%s", res.Content)
	}
}

// TestExpandMultipleDocumentsAllAppear covers a message that carries
// more than one document.
func TestExpandMultipleDocumentsAllAppear(t *testing.T) {
	pool := getExpandPool(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id := insertSlackMessage(t, pool, "C1", "team-policy", "1700000000.000100", "", "chester", "two files", base)

	seedDocForExpand(t, pool, id, "main.pdf", 1, "Primary agreement terms are here.")
	seedDocForExpand(t, pool, id, "appendix.pdf", 1, "Secondary schedule of fees.")

	e := NewExpander(pool)
	res, err := e.Expand(ctx, id, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.pdf", "Primary agreement", "appendix.pdf", "Secondary schedule"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q from multi-document expand:\n%s", want, res.Content)
		}
	}
}
