package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// End-to-end worker tests against a real Postgres, with the model and
// S3 faked. These cover the parts unit tests can't: the claim/commit
// transaction, per-document chunk replacement, the in-transaction gate
// re-check, and the settle-vs-retry split.

type fakeExtractor struct {
	markdown     string
	err          error
	truncated    bool
	inputTokens  int
	outputTokens int
	calls        int
}

func (f *fakeExtractor) Extract(context.Context, string, string, []byte) (*Result, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	// PagesExtracted tracks PageCount, matching what a model transport can
	// actually know (see GatewayExtractor): every page it counted is a page
	// it transcribed. A fake that left it zero would make every worker
	// test's document settles with missing_pages.
	markers := countPageMarkers(f.markdown)
	return &Result{
		Markdown:       f.markdown,
		PageCount:      markers,
		PagesExtracted: markers,
		ModelID:        "fake-model",
		Truncated:      f.truncated,
		InputTokens:    f.inputTokens,
		OutputTokens:   f.outputTokens,
	}, nil
}
func (f *fakeExtractor) ModelID() string { return "fake-model" }

type fakeCheckpointingExtractor struct {
	result *Result
	calls  int
}

func (f *fakeCheckpointingExtractor) ModelID() string { return DefaultModelID }

func (f *fakeCheckpointingExtractor) Extract(
	context.Context, string, string, []byte,
) (*Result, error) {
	return f.result, nil
}

func (f *fakeCheckpointingExtractor) ExtractWithCheckpoints(
	ctx context.Context, _, _ string, _ []byte, checkpoints PageCheckpointStore,
) (*Result, error) {
	f.calls++
	_, err := checkpoints.Save(ctx, f.ModelID(), PageCheckpoint{
		PageNumber: 1,
		Markdown:   f.result.Markdown,
		CostUSD:    f.result.CostUSD,
	})
	if err != nil {
		return nil, err
	}
	return f.result, nil
}

type fakeS3 struct {
	objects map[string][]byte
}

func (f *fakeS3) Put(_ context.Context, key, _ string, data []byte) error {
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key] = data
	return nil
}
func (f *fakeS3) Get(_ context.Context, key string) ([]byte, string, error) {
	if b, ok := f.objects[key]; ok {
		return b, "application/pdf", nil
	}
	return nil, "", context.Canceled
}
func (f *fakeS3) Bucket() string { return "fake-bucket" }

type fakeFetcher struct{ data []byte }

func (f *fakeFetcher) Fetch(context.Context, string) ([]byte, string, error) {
	return f.data, "application/pdf", nil
}

// dbGate reads the real slack_channel_config table, so the worker's
// in-transaction gate re-check is exercised against real rows rather
// than a stub that can't reflect a mid-flight flip.
type dbGate struct{ pool *pgxpool.Pool }

func (g dbGate) DocIngestEnabled(ctx context.Context, channelID string) (bool, error) {
	var enabled bool
	err := g.pool.QueryRow(ctx,
		`SELECT coalesce(doc_ingest_enabled, false) FROM slack_channel_config WHERE channel_id = $1`,
		channelID).Scan(&enabled)
	if err != nil {
		return false, nil // no row = default-deny
	}
	return enabled, nil
}

func workerTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "slackdocs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_channel_config RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// seedDoc creates an event in channelID plus one pending
// event_documents row, and sets the channel's opt-in flag.
func seedDoc(t *testing.T, pool *pgxpool.Pool, channelID, fileID string, optIn bool) (eventID, docID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, actor, body, raw, scope)
		VALUES ('slack', $1, $2, 'tester', 'see attached', '{}'::jsonb, 'public')
		RETURNING id
	`, channelID+":"+fileID, time.Now().UTC()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, user_id, has_files, occurred_at)
		VALUES ($1, $2, 'U1', true, now())
	`, eventID, channelID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, doc_ingest_enabled)
		VALUES ($1, $2) ON CONFLICT (channel_id) DO UPDATE SET doc_ingest_enabled = $2
	`, channelID, optIn); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format)
		SELECT $1, $2, e.occurred_at, 'contract.pdf', 'application/pdf', 'pdf'
		  FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID, fileID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	return eventID, docID
}

// newTestWorker wires the fake extractor as the router's PDF arm, so the
// seeded fixtures (doc_format 'pdf') route to it. Standing up the real
// pdfium arm here would put a WASM pool in every worker test for no gain:
// these tests are about the claim/commit transaction, and the arm's own
// behavior is covered in pdf_test.go. xlsx/docx route to the real local
// parsers instead, which is the point of the router.
func newTestWorker(pool *pgxpool.Pool, ex Extractor) *Worker {
	w := &Worker{
		Pool:     pool,
		S3:       &fakeS3{},
		Router:   &Router{PDF: ex},
		Fetchers: map[string]files.Fetcher{"slack": &fakeFetcher{data: []byte("%PDF-1.4 bytes")}},
		Gate:     dbGate{pool: pool},
		Config:   Config{Concurrency: 1},
	}
	w.Config.fillDefaults()
	w.metrics = newWorkerMetrics(nil)
	return w
}

func TestZendeskDocumentsProcessWithoutSlackAndDisabledSourcesStayQueued(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	extractor := &fakeExtractor{markdown: "--- PAGE 1 ---\nSupport attachment"}
	worker := newTestWorker(pool, extractor)
	eventID, documentID := seedDoc(t, pool, "CUNAPPROVED", "zendesk-file", false)
	_, err := pool.Exec(ctx, `UPDATE events SET source = 'zendesk', scope = 'corpus:zendesk' WHERE id = $1`, eventID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM slack_message_details WHERE event_id = $1`, eventID)
	require.NoError(t, err)
	row, err := worker.claimPending(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, row)
	worker.Fetchers = map[string]files.Fetcher{"zendesk": &fakeFetcher{data: []byte("%PDF-1.4 bytes")}}
	row, err = worker.claimPending(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, documentID, row.documentID)
	require.Empty(t, row.channelID)
	require.NoError(t, worker.extractOne(ctx, row))
	require.Equal(t, 1, extractor.calls)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks chunk JOIN events parent ON parent.id = chunk.event_id
		WHERE parent.id = $1 AND parent.scope = 'corpus:zendesk' AND chunk.kind = 'document_text'`, eventID).Scan(&count))
	require.Positive(t, count)
}

// TestWorkerExtractsAndChunks is the happy path through the real
// claim + commit transaction.
func TestWorkerExtractsAndChunks(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docID := seedDoc(t, pool, "CYES", "F1", true)

	ex := &fakeExtractor{markdown: "--- PAGE 1 ---\nRetention is 18 months.\n--- PAGE 2 ---\nContact legal-ops."}
	w := newTestWorker(pool, ex)

	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v row=%v", err, row)
	}
	if row.channelID != "CYES" {
		t.Errorf("channelID = %q, want CYES (needed for the gate check)", row.channelID)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}

	// Two pages -> two chunks, each owned by this document, with a
	// context prefix and its own chunk_idx sequence.
	rows, err := pool.Query(ctx, `
		SELECT chunk_idx, content, coalesce(context_prefix,''), event_document_id
		  FROM event_chunks WHERE event_id = $1 AND kind = 'document_text'
		 ORDER BY chunk_idx
	`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for rows.Next() {
		var idx int
		var content, prefix string
		var owner int64
		if err := rows.Scan(&idx, &content, &prefix, &owner); err != nil {
			t.Fatal(err)
		}
		if owner != docID {
			t.Errorf("chunk %d owner = %d, want %d", idx, owner, docID)
		}
		if !strings.Contains(prefix, "contract.pdf") {
			t.Errorf("chunk %d prefix missing filename: %q", idx, prefix)
		}
		if strings.Contains(content, "--- PAGE") {
			t.Errorf("chunk %d leaked a page marker", idx)
		}
		n++
	}
	rows.Close()
	if n != 2 {
		t.Errorf("got %d chunks, want 2", n)
	}

	var extractModel string
	var pageCount int
	var rejected *string
	if err := pool.QueryRow(ctx, `
		SELECT extract_model, page_count, rejected_reason
		  FROM event_documents WHERE id = $1
	`, docID).Scan(&extractModel, &pageCount, &rejected); err != nil {
		t.Fatal(err)
	}
	if extractModel != "fake-model" || pageCount != 2 || rejected != nil {
		t.Errorf("row = model=%q pages=%d rejected=%v", extractModel, pageCount, rejected)
	}

	// The denormalized document chunk occurred_at must mirror the
	// parent event's SOURCE recency: extraction is a re-processing
	// path, and the derived value must never be a per-extract
	// processing timestamp.
	var eventOcc, chunkOcc time.Time
	if err := pool.QueryRow(ctx, `
		SELECT e.occurred_at,
		       (SELECT MIN(c.occurred_at) FROM event_chunks c
		         WHERE c.event_id = e.id AND c.kind = 'document_text')
		  FROM events e WHERE e.id = $1
	`, eventID).Scan(&eventOcc, &chunkOcc); err != nil {
		t.Fatal(err)
	}
	if !chunkOcc.Equal(eventOcc) {
		t.Errorf("document chunk occurred_at = %v, want parent event source recency %v", chunkOcc, eventOcc)
	}
}

// TestWorkerMissingPDFModelLeavesPDFPending proves the worker's claim query
// honors the router capability gate. Fixing credentials and restarting then
// makes the same untouched row eligible; no backfill/reset is required.
func TestWorkerMissingPDFModelLeavesPDFPending(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CMODEL", "FMODEL", true)

	w := newTestWorker(pool, &fakeExtractor{markdown: "unused"})
	w.Router.PDF = &PDFParser{} // PDFium exists, sparse-page model does not.
	row, err := w.claimPending(ctx, w.Router.ClaimFormats())
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("claimed PDF %d without sparse-page model", row.documentID)
	}

	var claimedAt *time.Time
	var failureCount int
	var extractedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT claimed_at, failure_count, extracted_at
		  FROM event_documents WHERE id = $1
	`, docID).Scan(&claimedAt, &failureCount, &extractedAt); err != nil {
		t.Fatal(err)
	}
	if claimedAt != nil || failureCount != 0 || extractedAt != nil {
		t.Fatalf("blocked PDF mutated: claimed=%v failures=%d extracted=%v",
			claimedAt, failureCount, extractedAt)
	}
}

func TestWorkerExtractionCheckpointLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       *Result
		wantParts    int
		wantReason   *string
		wantRejected *string
	}{
		{
			name: "complete deletes checkpoints atomically",
			result: &Result{
				Markdown:       "--- PAGE 1 ---\ncomplete text",
				PageCount:      1,
				PagesExtracted: 1,
				ModelID:        DefaultModelID,
				InputTokens:    100,
				OutputTokens:   20,
				CostUSD:        0.00055,
			},
		},
		{
			name: "cost limited retains checkpoints",
			result: &Result{
				Markdown:       "--- PAGE 1 ---\npartial text",
				PageCount:      2,
				PagesExtracted: 1,
				ModelID:        DefaultModelID,
				InputTokens:    4_000_001,
				CostUSD:        10.000003,
				CostLimited:    true,
			},
			wantParts:  1,
			wantReason: stringPtr("cost_limit"),
		},
		{
			name: "empty extraction deletes checkpoints",
			result: &Result{
				Markdown:       "  ",
				PageCount:      1,
				PagesExtracted: 1,
				ModelID:        DefaultModelID,
				InputTokens:    100,
				OutputTokens:   20,
				CostUSD:        0.00055,
			},
			wantRejected: stringPtr(string(RejectEmpty)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := workerTestPool(t)
			ctx := context.Background()
			_, docID := seedDoc(t, pool, "CYES", "F1", true)
			ex := &fakeCheckpointingExtractor{result: tc.result}
			w := newTestWorker(pool, ex)

			row, err := w.claimPending(ctx, nil)
			if err != nil || row == nil {
				t.Fatalf("claim: %v row=%v", err, row)
			}
			if err := w.extractOne(ctx, row); err != nil {
				t.Fatalf("extractOne: %v", err)
			}
			if ex.calls != 1 {
				t.Fatalf("checkpointing extraction calls = %d, want 1", ex.calls)
			}

			var parts int
			var reason, rejected *string
			var cost *float64
			if err := pool.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM event_document_extract_parts
				         WHERE event_document_id = ed.id),
				       ed.incomplete_reason, ed.rejected_reason, ed.extract_cost_usd
				  FROM event_documents ed WHERE ed.id = $1
			`, docID).Scan(&parts, &reason, &rejected, &cost); err != nil {
				t.Fatal(err)
			}
			if parts != tc.wantParts {
				t.Errorf("checkpoint rows = %d, want %d", parts, tc.wantParts)
			}
			if !equalStringPtr(reason, tc.wantReason) {
				t.Errorf("incomplete_reason = %v, want %v", reason, tc.wantReason)
			}
			if !equalStringPtr(rejected, tc.wantRejected) {
				t.Errorf("rejected_reason = %v, want %v", rejected, tc.wantRejected)
			}
			if cost == nil || *cost != tc.result.CostUSD {
				t.Errorf("extract_cost_usd = %v, want %f", cost, tc.result.CostUSD)
			}
		})
	}
}

func stringPtr(s string) *string { return &s }

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestWorkerGateClosedSettlesWithoutExtracting: an opted-out channel
// must cost no model call.
func TestWorkerGateClosedSettlesWithoutExtracting(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CNO", "F1", false)

	ex := &fakeExtractor{markdown: "should never be produced"}
	w := newTestWorker(pool, ex)

	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	if ex.calls != 0 {
		t.Errorf("extractor called %d times for an opted-out channel, want 0", ex.calls)
	}
	var reason *string
	var chunks int
	if err := pool.QueryRow(ctx, `
		SELECT ed.rejected_reason,
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = ed.id)
		  FROM event_documents ed WHERE ed.id = $1
	`, docID).Scan(&reason, &chunks); err != nil {
		t.Fatal(err)
	}
	if reason == nil || *reason != "gate_closed" {
		t.Errorf("rejected_reason = %v, want gate_closed", reason)
	}
	if chunks != 0 {
		t.Errorf("wrote %d chunks for an opted-out channel", chunks)
	}
}

// TestWorkerOptOutDuringExtractionDiscards is the in-transaction gate
// re-check. The channel is opted in at claim time and flipped off while
// the "model call" is in flight, so only a check inside the commit tx
// catches it — a check before BEGIN would have already passed.
func TestWorkerOptOutDuringExtractionDiscards(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CFLIP", "F1", true)

	flipped := false
	ex := &fakeExtractor{markdown: "--- PAGE 1 ---\nSensitive contract text."}
	w := newTestWorker(pool, ex)
	// Flip the gate off inside the extractor call, i.e. after the
	// pre-call check has passed and before commit runs.
	w.Router.PDF = &hookExtractor{inner: ex, before: func() {
		if flipped {
			return
		}
		flipped = true
		if _, err := pool.Exec(ctx,
			`UPDATE slack_channel_config SET doc_ingest_enabled = false WHERE channel_id = 'CFLIP'`); err != nil {
			t.Fatal(err)
		}
	}}

	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}

	var reason *string
	var chunks int
	if err := pool.QueryRow(ctx, `
		SELECT ed.rejected_reason,
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = ed.id)
		  FROM event_documents ed WHERE ed.id = $1
	`, docID).Scan(&reason, &chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Errorf("committed %d chunks after the channel opted out mid-extraction", chunks)
	}
	if reason == nil || *reason != "gate_closed" {
		t.Errorf("rejected_reason = %v, want gate_closed", reason)
	}
}

// TestWorkerParentDeletedDuringExtractionCommitsNothing covers the
// claim→commit race for a soft delete: the event is live at claim time
// and marked deleted while the "model call" is in flight. The delete
// path has already dropped the event's chunks and the embedder skips
// deleted events, so commit must refuse rather than write chunks that
// would never embed.
func TestWorkerParentDeletedDuringExtractionCommitsNothing(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docID := seedDoc(t, pool, "CDEL", "F1", true)

	ex := &fakeExtractor{markdown: "--- PAGE 1 ---\nContract text."}
	w := newTestWorker(pool, ex)
	w.Router.PDF = &hookExtractor{inner: ex, before: func() {
		if _, err := pool.Exec(ctx,
			`UPDATE events SET deleted = true WHERE id = $1`, eventID); err != nil {
			t.Fatal(err)
		}
	}}

	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	err = w.extractOne(ctx, row)
	if err == nil || !strings.Contains(err.Error(), "parent event deleted") {
		t.Fatalf("extractOne err = %v, want parent event deleted", err)
	}

	var chunks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM event_chunks WHERE event_document_id = $1
	`, docID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Errorf("committed %d chunks for a deleted event", chunks)
	}
}

// hookExtractor runs a side effect immediately before delegating, to
// simulate concurrent state changes during the model call.
type hookExtractor struct {
	inner  Extractor
	before func()
}

func (h *hookExtractor) Extract(ctx context.Context, f, n string, d []byte) (*Result, error) {
	h.before()
	return h.inner.Extract(ctx, f, n, d)
}
func (h *hookExtractor) ModelID() string { return h.inner.ModelID() }

// Each document commit must leave sibling-document chunks intact.
func TestWorkerSiblingDocumentsDoNotClobber(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docA := seedDoc(t, pool, "CYES", "FA", true)
	// Second document on the SAME event.
	var docB int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format)
		SELECT $1, 'FB', e.occurred_at, 'appendix.pdf', 'application/pdf', 'pdf'
		  FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID).Scan(&docB); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(pool, &fakeExtractor{
		// Long enough to clear the minUsefulRunes floor after page
		// markers are stripped — a two-word fixture reads as "empty",
		// which is the reject guard working correctly.
		markdown: "--- PAGE 1 ---\nRetention for subscriber records is eighteen months.\n" +
			"--- PAGE 2 ---\nEscalation contact for court orders is legal-ops.",
	})
	// Drain both documents.
	for range 2 {
		row, err := w.claimPending(ctx, nil)
		if err != nil || row == nil {
			t.Fatalf("claim: %v row=%v", err, row)
		}
		if err := w.extractOne(ctx, row); err != nil {
			t.Fatalf("extractOne: %v", err)
		}
	}

	var aChunks, bChunks int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_chunks WHERE event_document_id = $1),
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = $2)
	`, docA, docB).Scan(&aChunks, &bChunks); err != nil {
		t.Fatal(err)
	}
	if aChunks != 2 || bChunks != 2 {
		t.Errorf("chunks A=%d B=%d, want 2/2 — the second commit erased the first document's chunks",
			aChunks, bChunks)
	}

	// Re-extracting A must also leave B alone.
	if _, err := pool.Exec(ctx, `
		UPDATE event_documents SET extracted_at = NULL, claimed_at = NULL WHERE id = $1
	`, docA); err != nil {
		t.Fatal(err)
	}
	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("re-claim: %v", err)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_chunks WHERE event_document_id = $1),
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = $2)
	`, docA, docB).Scan(&aChunks, &bChunks); err != nil {
		t.Fatal(err)
	}
	if aChunks != 2 || bChunks != 2 {
		t.Errorf("after re-extract: A=%d B=%d, want 2/2 (no duplicates, sibling intact)",
			aChunks, bChunks)
	}
}

// TestWorkerRejectedOutputSettlesNoChunks: degenerate model output
// settles the row without chunks and without a retry.
func TestWorkerRejectedOutputSettlesNoChunks(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CYES", "F1", true)

	// Empty model output is settled and still records the call's cost.
	w := newTestWorker(pool, &fakeExtractor{
		markdown:     " ",
		inputTokens:  500,
		outputTokens: 120,
	})
	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}

	var reason *string
	var extracted *time.Time
	var chunks int
	var incompleteReason *string
	var cost *float64
	if err := pool.QueryRow(ctx, `
		SELECT ed.rejected_reason, ed.extracted_at, ed.incomplete_reason,
		       ed.extract_cost_usd,
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = ed.id)
		  FROM event_documents ed WHERE ed.id = $1
	`, docID).Scan(&reason, &extracted, &incompleteReason, &cost, &chunks); err != nil {
		t.Fatal(err)
	}
	if reason == nil || *reason != string(RejectEmpty) {
		t.Errorf("rejected_reason = %v, want empty", reason)
	}
	if extracted == nil {
		t.Error("extracted_at not stamped; the row would be retried forever")
	}
	if chunks != 0 {
		t.Errorf("wrote %d chunks for empty output", chunks)
	}
	if incompleteReason != nil {
		t.Errorf("incomplete_reason = %v, want nil for a rejection", *incompleteReason)
	}
	if cost == nil {
		t.Error("rejected model call did not record its cost")
	}

	// Settled means it leaves the queue.
	if next, err := w.claimPending(ctx, nil); err != nil {
		t.Fatal(err)
	} else if next != nil {
		t.Errorf("rejected row was re-claimed (document_id=%d)", next.documentID)
	}
}

// A rejected retry must not leave a prior attempt's chunks searchable.
func TestWorkerSettleRejectedDeletesStaleChunks(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	eventID, docID := seedDoc(t, pool, "CYES", "F1", true)

	// Simulate a prior successful extraction: real chunks already
	// committed against this document.
	if _, err := pool.Exec(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, event_document_id, occurred_at)
		VALUES ($1, 'document_text', 0, 'stale content from a prior attempt', $2, now())
	`, eventID, docID); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(pool, &fakeExtractor{})
	if err := w.settleRejected(ctx, docID, "test_reason", nil); err != nil {
		t.Fatalf("settleRejected: %v", err)
	}

	var chunks int
	var reason *string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_chunks WHERE event_document_id = $1),
		       (SELECT rejected_reason FROM event_documents WHERE id = $1)
	`, docID).Scan(&chunks, &reason); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Errorf("chunks = %d after settleRejected, want 0 — a rejected row must not "+
			"keep serving a prior attempt's stale chunks", chunks)
	}
	if reason == nil || *reason != "test_reason" {
		t.Errorf("rejected_reason = %v, want test_reason", reason)
	}
}

// TestWorkerClaimIsExclusive: two claims in a row must not hand out the
// same document twice, or we'd pay for it twice.
func TestWorkerClaimIsExclusive(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	seedDoc(t, pool, "CYES", "F1", true)

	w := newTestWorker(pool, &fakeExtractor{markdown: "text"})
	first, err := w.claimPending(ctx, nil)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := w.claimPending(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Errorf("same document claimed twice (ids %d, %d)", first.documentID, second.documentID)
	}
}

// TestWorkerSkipsPastFailureBudget: a poisoned row leaves the queue.
func TestWorkerSkipsPastFailureBudget(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CYES", "F1", true)
	if _, err := pool.Exec(ctx,
		`UPDATE event_documents SET failure_count = $2 WHERE id = $1`,
		docID, events.MaxFailures); err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(pool, &fakeExtractor{markdown: "text"})
	if row, err := w.claimPending(ctx, nil); err != nil {
		t.Fatal(err)
	} else if row != nil {
		t.Error("claimed a row past the retry limit")
	}
}

// Every statement that records a failed attempt also records when, by the
// database's clock, and reading the document clears the time with the
// error: the Failures page dates a failure by it.
func TestRecordedFailuresAreTimedAndClearedOnSuccess(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CYES", "F1", true)
	w := newTestWorker(pool, &fakeExtractor{markdown: "--- PAGE 1 ---\nRetention is 18 months."})

	failedJustNow := func(step string) {
		t.Helper()
		var timed, justNow bool
		if err := pool.QueryRow(ctx, `
			SELECT last_failed_at IS NOT NULL,
			       COALESCE(last_failed_at BETWEEN now() - interval '1 minute' AND now(), false)
			  FROM event_documents WHERE id = $1`, docID).Scan(&timed, &justNow); err != nil {
			t.Fatal(err)
		}
		if !timed || !justNow {
			t.Errorf("%s: last_failed_at set=%v, within the last minute=%v", step, timed, justNow)
		}
	}
	age := func() {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE event_documents SET last_failed_at = now() - interval '3 days' WHERE id = $1`, docID); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.bumpFailure(ctx, docID, errors.New("route F1: no extractor")); err != nil {
		t.Fatal(err)
	}
	failedJustNow("a failed attempt")

	// An older failure's time is replaced by the newer one's.
	age()
	if err := w.recordExhaustion(ctx, docID, &files.TransientError{Underlying: errors.New("status 503")}); err != nil {
		t.Fatal(err)
	}
	failedJustNow("retries used up on a passing fault")

	age()
	if err := w.bumpFailure(ctx, docID, &files.PermanentError{Underlying: errors.New("gone for good")}); err != nil {
		t.Fatal(err)
	}
	failedJustNow("a failure that ends the attempts")

	// Put back in the queue and read: nothing of the failure is left.
	if _, err := pool.Exec(ctx,
		`UPDATE event_documents SET failure_count = 1 WHERE id = $1`, docID); err != nil {
		t.Fatal(err)
	}
	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v row=%v", err, row)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	var lastFailure *string
	var lastFailedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_failure, last_failed_at FROM event_documents WHERE id = $1 AND extracted_at IS NOT NULL`,
		docID).Scan(&lastFailure, &lastFailedAt); err != nil {
		t.Fatal(err)
	}
	if lastFailure != nil || lastFailedAt != nil {
		t.Errorf("a document that was read keeps no failure: last_failure=%v last_failed_at=%v", lastFailure, lastFailedAt)
	}
}

// TestClaimLifetimeExceedsCallTimeout pins a relationship between two
// constants in different files that has already drifted once.
//
// If a claim can go stale while the original worker is still inside a
// live model call, a second worker re-claims the row and both pay for the
// same extraction. That is a pure spend bug with no error signature —
// both calls succeed, the second commit overwrites the first, and nothing
// in the logs says anything happened. perCallTimeout is 20 minutes (a
// 100-page document needs about 19 at the measured worst case), so a claim
// window shorter than that inverts the relationship; this test makes such
// a change fail loudly.
func TestClaimLifetimeExceedsCallTimeout(t *testing.T) {
	// Slack fetch, S3 mirror, chunk, and commit all happen outside the
	// model call but inside the claim, so require real headroom rather
	// than a bare inequality.
	const minHeadroom = 5 * time.Minute
	if staleClaimAge < perCallTimeout+minHeadroom {
		t.Errorf("staleClaimAge (%v) must exceed perCallTimeout (%v) by at "+
			"least %v, or a second worker re-claims a row mid-call and both "+
			"pay for the same extraction",
			staleClaimAge, perCallTimeout, minHeadroom)
	}
}

// TestStageHistogramCoversCallTimeout: a histogram whose top bucket is
// below the timeout puts every slow extraction in +Inf, which is exactly
// where an operator needs resolution — "some calls took over 180s" is not
// actionable when the timeout is 20 minutes.
func TestStageHistogramCoversCallTimeout(t *testing.T) {
	m := newWorkerMetrics(nil)
	if m.stage == nil {
		t.Fatal("stage histogram not constructed")
	}
	// Reading the configured buckets back out isn't exposed by the
	// prometheus API, so assert on the source of truth instead: the
	// constant the buckets are built from must reach the timeout.
	if stageHistogramMaxSeconds < perCallTimeout.Seconds() {
		t.Errorf("stage histogram tops out at %vs but a call may run %vs; "+
			"slow extractions would all land in +Inf",
			stageHistogramMaxSeconds, perCallTimeout.Seconds())
	}
}

// Truncated output keeps its usable chunks and is marked partial.
func TestWorkerTruncatedExtractionLandsAsPartialWithChunks(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	_, docID := seedDoc(t, pool, "CYES", "F1", true)

	ex := &fakeExtractor{
		markdown:  "--- PAGE 1 ---\nRetention is 18 months.\n--- PAGE 2 ---\nContact legal-ops.",
		truncated: true,
	}
	w := newTestWorker(pool, ex)

	row, err := w.claimPending(ctx, nil)
	if err != nil || row == nil {
		t.Fatalf("claim: %v row=%v", err, row)
	}
	if err := w.extractOne(ctx, row); err != nil {
		t.Fatalf("extractOne: %v", err)
	}

	var incompleteReason, rejectedReason *string
	var chunks int
	if err := pool.QueryRow(ctx, `
		SELECT ed.incomplete_reason, ed.rejected_reason,
		       (SELECT count(*) FROM event_chunks WHERE event_document_id = ed.id)
		  FROM event_documents ed WHERE ed.id = $1
	`, docID).Scan(&incompleteReason, &rejectedReason, &chunks); err != nil {
		t.Fatal(err)
	}
	if incompleteReason == nil || *incompleteReason != "output_truncated" {
		t.Errorf("incomplete_reason = %v, want output_truncated", incompleteReason)
	}
	if rejectedReason != nil {
		t.Errorf("rejected_reason = %v, want NULL — a truncated document is not a rejection",
			*rejectedReason)
	}
	if chunks != 2 {
		t.Errorf("chunks = %d, want 2 — truncated output must keep its real content", chunks)
	}
}
