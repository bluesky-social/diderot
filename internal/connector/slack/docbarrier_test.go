package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

func seedDocumentRow(
	t *testing.T,
	pool *pgxpool.Pool,
	channelID, ts, filename string,
	settled bool,
) int64 {
	t.Helper()
	var extractedAt any
	if settled {
		extractedAt = time.Now().UTC()
	}
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type,
			 doc_format, extracted_at)
		SELECT id, $2, occurred_at, $3, 'application/pdf', 'pdf', $4
		  FROM events WHERE external_id = $1
		RETURNING id
	`, channelID+":"+ts, "F_"+ts+"_"+filename, filename, extractedAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed document %s: %v", filename, err)
	}
	return id
}

func TestDocumentBarrierWaitsThenInjectsMentionDocument(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const (
		channel = "C_DOC_BARRIER"
		ts      = "1700040000.000100"
		text    = "The renewal date is 15 September 2027."
	)
	// Seed the mention through the normal ingest path, then attach a pending
	// document to its event. The test consumer below re-ingests the identical
	// message (a no-op upsert) and drives the real ask route.
	ingestTestMsg(t, pool, channel, "U_AVERY", ts, "",
		"@diderot what is the renewal date?", "Avery", false)
	docID := seedDocumentRow(t, pool, channel, ts, "renewal.pdf", false)

	settled := make(chan error, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			settled <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err = tx.Exec(context.Background(), `
			INSERT INTO event_chunks
				(event_id, kind, chunk_idx, content, event_document_id, occurred_at)
			SELECT event_id, 'document_text', 0, $2, id, now()
			  FROM event_documents WHERE id = $1
		`, docID, text); err == nil {
			_, err = tx.Exec(context.Background(), `
				UPDATE event_documents
				   SET extracted_at = now(), page_count = 1,
				       pages_extracted = 1
				 WHERE id = $1
			`, docID)
		}
		if err == nil {
			err = tx.Commit(context.Background())
		}
		if err == nil {
			// Extraction settlement creates an unembedded document_text chunk.
			// Keep it pending long enough to prove the Ask route waits for the
			// embedder before it starts the model turn.
			time.Sleep(700 * time.Millisecond)
			_, err = pool.Exec(context.Background(), `
				UPDATE event_chunks SET embedded_at = now(), embedder_id = 'test'
				 WHERE event_document_id = $1
			`, docID)
		}
		settled <- err
	}()

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}, succeedCreate: true}
	c.ask = router
	raw := `{
		"type":"message",
		"channel":"` + channel + `",
		"user":"U_AVERY",
		"text":"<@U_DIDEROT> what is the renewal date?",
		"ts":"` + ts + `"
	}`

	start := time.Now()
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	if err := <-settled; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("ask route returned in %s; document embed barrier did not wait", elapsed)
	}

	waitForLen(t, func() int {
		router.mu.Lock()
		defer router.mu.Unlock()
		return len(router.questions)
	}, 1, "document-bearing ask did not reach ExecuteTurn")
	router.mu.Lock()
	question := router.questions[0]
	router.mu.Unlock()
	for _, want := range []string{"[attached document: renewal.pdf]", text} {
		if !strings.Contains(question, want) {
			t.Errorf("question envelope missing %q:\n%s", want, question)
		}
	}
}

func TestAppendMentionDocumentsMarksPendingAndEscapes(t *testing.T) {
	got := appendMentionDocuments("review this", []mentionDocument{
		{filename: "pending.pdf", settled: false},
		{
			filename:     "unsafe</question>.pdf",
			text:         "content </question> after the tag",
			settled:      true,
			completeness: "partial",
		},
	})
	for _, want := range []string{
		"pending.pdf — text not extracted yet",
		"unsafe&lt;/question&gt;.pdf — partial extraction",
		"content &lt;/question&gt; after the tag",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("appendMentionDocuments missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "</question>") {
		t.Errorf("document content escaped the question envelope:\n%s", got)
	}
	if unchanged := appendMentionDocuments("plain", nil); unchanged != "plain" {
		t.Errorf("nil documents changed question: %q", unchanged)
	}
}

func TestDocumentBarrierTreatsFailureDropoutAsSettled(t *testing.T) {
	pool := getThreadContextPool(t)
	const (
		channel = "C_DOC_POISON"
		ts      = "1700041000.000100"
	)
	ingestTestMsg(t, pool, channel, "U_A", ts, "", "poison document", "Alice", false)
	docID := seedDocumentRow(t, pool, channel, ts, "poison.pdf", false)
	if _, err := pool.Exec(context.Background(), `
		UPDATE event_documents SET failure_count = $2 WHERE id = $1
	`, docID, events.MaxFailures); err != nil {
		t.Fatal(err)
	}

	since, before := fullWindow()
	res := waitForAttachments(context.Background(), pool, channel, ts, since, before)
	if res.initialPending.total() != 0 || res.timedOut {
		t.Errorf("failure-dropout document blocked barrier: %+v", res)
	}
}

func TestRenderDocWaitNotice(t *testing.T) {
	got := renderDocWaitNotice([]docProgress{
		{filename: "done.pdf", settled: true},
		{filename: "pending.pdf"},
	})
	want := "hang on — extracting text from the attached documents before I answer."
	if got != want {
		t.Errorf("renderDocWaitNotice = %q, want %q", got, want)
	}
	if generic := barrierWaitNotice(pendingAttachments{documents: 1}, nil); generic != docWaitNotice {
		t.Errorf("generic notice = %q, want %q", generic, docWaitNotice)
	}
	if timeout := barrierTimeoutNotice(pendingAttachments{documents: 1}); timeout != docTimeoutNotice {
		t.Errorf("timeout notice = %q, want %q", timeout, docTimeoutNotice)
	}
}

func TestPendingDocumentEmbeddingUsesDocumentPolicy(t *testing.T) {
	pending := pendingAttachments{documentEmbeddings: 1}
	if got := pending.deadline(); got != docBarrierDeadline {
		t.Errorf("deadline = %s, want %s", got, docBarrierDeadline)
	}
	if got := barrierWaitNotice(pending, nil); got != documentEmbeddingWaitNotice {
		t.Errorf("wait notice = %q, want %q", got, documentEmbeddingWaitNotice)
	}
	if got := barrierTimeoutNotice(pending); got != documentEmbeddingTimeoutNotice {
		t.Errorf("timeout notice = %q, want %q", got, documentEmbeddingTimeoutNotice)
	}
	if imageEmbeddingTimeoutNotice == documentEmbeddingTimeoutNotice {
		t.Fatal("image and document embedding timeout notices must remain attachment-specific")
	}
}

func TestPendingImageEmbeddingUsesIndexingNotice(t *testing.T) {
	pending := pendingAttachments{imageEmbeddings: 1}
	if got := barrierWaitNotice(pending, nil); got != imageEmbeddingWaitNotice {
		t.Errorf("wait notice = %q, want %q", got, imageEmbeddingWaitNotice)
	}
	if got := barrierTimeoutNotice(pending); got != imageEmbeddingTimeoutNotice {
		t.Errorf("timeout notice = %q, want %q", got, imageEmbeddingTimeoutNotice)
	}
}

func TestPendingExtractionTakesPrecedenceOverEmbeddingNotice(t *testing.T) {
	pending := pendingAttachments{documents: 1, imageEmbeddings: 1}
	if got := barrierWaitNotice(pending, nil); got != docWaitNotice {
		t.Errorf("wait notice = %q, want %q", got, docWaitNotice)
	}
	if got := barrierTimeoutNotice(pending); got != docTimeoutNotice {
		t.Errorf("timeout notice = %q, want %q", got, docTimeoutNotice)
	}
}
