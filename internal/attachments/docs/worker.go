package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
)

// Config governs polling cadence + concurrency.
type Config struct {
	// Concurrency is how many extractions run in parallel.
	Concurrency int
	// PollInterval is the sleep when the queue is empty.
	PollInterval time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
}

func (c *Config) fillDefaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = 5 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 2 * time.Minute
	}
}

// GateChecker reports whether a channel is opted in to document
// ingest. Consulted by the worker so revoking the flag stops work that
// is already queued, not merely future work.
type GateChecker interface {
	DocIngestEnabled(ctx context.Context, channelID string) (bool, error)
}

// newRedactor builds the worker's redactor; a test replaces it.
var newRedactor = files.NewRedactor

// Worker drives the extraction pipeline. Construct, then call Run from
// the leader-elected worker block.
type Worker struct {
	Pool *pgxpool.Pool
	S3   files.S3Mirror
	// Router picks the extraction mechanism per format: local parsers
	// for xlsx/docx, a model for PDFs. The worker never chooses — see
	// router.go for why that decision lives in one place.
	Router *Router
	// Fetchers maps events.source to its byte fetcher. Only "slack"
	// is wired today.
	Fetchers map[string]files.Fetcher
	Gate     GateChecker
	Logger   *slog.Logger
	Registry prometheus.Registerer
	Config   Config

	metrics  *workerMetrics
	redactor files.Redactor
}

// log supports callers that drive a Worker directly without Run.
func (w *Worker) log() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// staleClaimAge must exceed perCallTimeout so live work is not reclaimed.
const staleClaimAge = 30 * time.Minute

// maxTransientRetries bounds how long one row can hold a worker slot
// while its transient errors fail to clear.
const maxTransientRetries = 5

// Run launches the extraction workers.
func (w *Worker) Run(ctx context.Context) error {
	w.Config.fillDefaults()
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Registry == nil {
		w.Registry = prometheus.NewRegistry()
	}
	w.metrics = newWorkerMetrics(w.Registry)

	if w.Pool == nil {
		return errors.New("docs: Pool is required")
	}
	if w.S3 == nil {
		return errors.New("docs: S3 is required")
	}
	if w.Router == nil {
		return errors.New("docs: Router is required")
	}
	if len(w.Fetchers) == 0 {
		return errors.New("docs: at least one Fetcher required")
	}
	if w.Gate == nil {
		return errors.New("docs: Gate is required (opt-in is not optional)")
	}

	// A worker that can't redact secrets must not extract: what it wrote
	// would be indexed and embedded unredacted.
	redactor, err := newRedactor()
	if err != nil {
		return fmt.Errorf("docs: secret redactor: %w", err)
	}
	w.redactor = redactor

	var wg sync.WaitGroup
	for i := 0; i < w.Config.Concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.runLoop(ctx, id)
		}(i)
	}

	wg.Wait()
	return ctx.Err()
}

func (w *Worker) runLoop(ctx context.Context, id int) {
	backoff := w.Config.MinBackoff
	claimFormats := w.Router.ClaimFormats()
	for {
		if ctx.Err() != nil {
			return
		}
		w.updateGauges(ctx)

		row, err := w.claimPending(ctx, claimFormats)
		if err != nil {
			w.log().Warn("docs: claim failed", "worker", id, "err", err)
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			backoff = nextBackoff(backoff, w.Config.MaxBackoff)
			continue
		}
		if row == nil {
			if sleepCtx(ctx, w.Config.PollInterval) != nil {
				return
			}
			continue
		}
		backoff = w.Config.MinBackoff

		transients := 0
		for {
			err := w.extractOne(ctx, row)
			if err == nil {
				w.metrics.success.Inc()
				break
			}
			if files.IsTransient(err) {
				transients++
				if transients > maxTransientRetries {
					w.log().Warn("docs: transient retries exhausted; bumping + releasing",
						"worker", id, "document_id", row.documentID,
						"transients", transients, "err", err)
					w.metrics.transient.Inc()
					// Persistently transient rows must eventually leave the queue.
					if uerr := w.recordExhaustion(ctx, row.documentID, err); uerr != nil {
						w.log().Error("docs: exhaustion bump failed",
							"document_id", row.documentID, "err", uerr)
					}
					break
				}
				w.log().Warn("docs: transient; retrying same claim",
					"worker", id, "document_id", row.documentID,
					"transients", transients, "err", err)
				w.metrics.transient.Inc()
				if sleepCtx(ctx, backoff) != nil {
					return
				}
				backoff = nextBackoff(backoff, w.Config.MaxBackoff)
				if uerr := w.refreshClaim(ctx, row.documentID); uerr != nil {
					w.log().Warn("docs: claim refresh failed; releasing",
						"document_id", row.documentID, "err", uerr)
					break
				}
				continue
			}
			w.log().Warn("docs: extraction failed",
				"worker", id, "document_id", row.documentID, "err", err)
			if uerr := w.bumpFailure(ctx, row.documentID, err); uerr != nil {
				w.log().Error("docs: bump failure failed",
					"document_id", row.documentID, "err", uerr)
			}
			w.metrics.failures.Inc()
			break
		}
	}
}

// claimRow is the per-iteration unit of work.
type claimRow struct {
	documentID   int64
	eventID      int64
	source       string
	sourceFileID string
	filename     string
	mediaType    string
	docFormat    string
	storageKey   string
	channelID    string
}

// claimPending atomically claims the newest matching row.
// Empty formats match any format; an empty queue returns (nil, nil).
func (w *Worker) claimPending(ctx context.Context, formats []string) (*claimRow, error) {
	sources := make([]string, 0, len(w.Fetchers))
	for source := range w.Fetchers {
		sources = append(sources, source)
	}
	var r claimRow
	var storage, docFormat, channelID *string
	err := w.Pool.QueryRow(ctx, `
		UPDATE event_documents
		   SET claimed_at = now()
		 WHERE id = (
		   SELECT ed.id
		     FROM event_documents ed
		    WHERE ed.extracted_at IS NULL
		      AND ed.failure_count < $1
		      AND EXISTS (SELECT 1 FROM events e WHERE e.id = ed.event_id AND e.source = ANY($4::text[]) AND NOT e.deleted)
		      AND (ed.claimed_at IS NULL OR ed.claimed_at < now() - make_interval(secs => $2))
		      AND ($3::text[] IS NULL OR ed.doc_format = ANY($3::text[]))
		      AND `+files.SlackChannelNotBlocked("ed.event_id")+`
		    ORDER BY ed.occurred_at DESC, ed.id
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		 )
		RETURNING id, event_id,
		          (SELECT source FROM events WHERE id = event_documents.event_id),
		          source_file_id, filename, media_type, doc_format, storage_key,
		          (SELECT channel_id FROM slack_message_details
		            WHERE event_id = event_documents.event_id)
	`, events.MaxFailures, staleClaimAge.Seconds(), nullIfNoFormats(formats), sources).Scan(
		&r.documentID, &r.eventID, &r.source,
		&r.sourceFileID, &r.filename, &r.mediaType, &docFormat, &storage, &channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if storage != nil {
		r.storageKey = *storage
	}
	if docFormat != nil {
		r.docFormat = *docFormat
	}
	if channelID != nil {
		r.channelID = *channelID
	}
	return &r, nil
}

// nullIfNoFormats makes an empty filter mean "any format" in SQL.
func nullIfNoFormats(formats []string) []string {
	if len(formats) == 0 {
		return nil
	}
	return formats
}

// extractOne runs the full pipeline for one document.
func (w *Worker) extractOne(ctx context.Context, row *claimRow) error {
	timer := w.metrics.startTimer()

	// Recheck the gate before doing work; it may have changed since enqueue.
	enabled, err := w.gateAllows(ctx, row.channelID)
	if err != nil {
		timer.done("gate_err")
		return &files.TransientError{Underlying: fmt.Errorf("gate check: %w", err)}
	}
	if !enabled {
		w.log().Info("docs: channel not opted in; settling without extraction",
			"document_id", row.documentID, "channel_id", row.channelID)
		timer.done("gate_closed")
		return w.settleRejected(ctx, row.documentID, "gate_closed", nil)
	}

	docFormat := row.docFormat
	if docFormat == "" {
		docFormat = FormatForMediaType(row.mediaType, row.filename)
	}
	if docFormat == "" {
		timer.done("unsupported_format")
		return &files.PermanentError{Underlying: fmt.Errorf(
			"unsupported media type %q for %s", row.mediaType, row.filename)}
	}

	// Step 1: bytes. Prefer our own S3 mirror when a prior pass stored
	// them: it's content-addressed and immutable, the read is far
	// cheaper than a Slack round trip, and decisively — Slack's
	// 12-month file retention may have deleted the upstream copy while
	// our mirror still has the bytes. Fall back to the source fetcher
	// when there's no key or the read fails.
	var (
		data       []byte
		ct         string
		fromMirror bool
	)
	if row.storageKey != "" {
		start := time.Now()
		b, mct, gerr := w.S3.Get(ctx, row.storageKey)
		if gerr == nil {
			data, ct, fromMirror = b, mct, true
			w.metrics.observeStage("fetch", "ok", start)
		} else {
			w.metrics.observeStage("fetch", "err", start)
			w.log().Warn("docs: mirror read failed; falling back to source",
				"document_id", row.documentID, "storage_key", row.storageKey, "err", gerr)
		}
	}
	if !fromMirror {
		fetcher, ok := w.Fetchers[row.source]
		if !ok {
			timer.done("no_fetcher")
			return fmt.Errorf("no fetcher for source %q", row.source)
		}
		start := time.Now()
		var ferr error
		data, ct, ferr = fetcher.Fetch(ctx, row.sourceFileID)
		if ferr != nil {
			w.metrics.observeStage("fetch", "err", start)
			timer.done("fetch_err")
			return fmt.Errorf("fetch %s/%s: %w", row.source, row.sourceFileID, ferr)
		}
		w.metrics.observeStage("fetch", "ok", start)
	}
	if ct == "" {
		ct = row.mediaType
	}
	if len(data) == 0 {
		timer.done("empty_bytes")
		return &files.PermanentError{Underlying: errors.New("source returned zero bytes")}
	}
	// Size guard before the S3 write and well before the model call: bytes
	// are held in memory through base64 encoding, and an oversized file is
	// a guaranteed rejection by the model provider.
	if len(data) > MaxDocumentBytes {
		w.log().Info("docs: oversized document; settling",
			"document_id", row.documentID, "bytes", len(data))
		timer.done("too_large")
		return w.settleRejected(ctx, row.documentID, "too_large", nil)
	}

	// Step 2: mirror to S3 at the content-addressed key.
	hash := files.SHA256Hex(data)
	key := row.storageKey
	if !fromMirror {
		key = files.StorageKeyFor(row.source, hash, ExtensionFor(docFormat))
		start := time.Now()
		if err := w.S3.Put(ctx, key, ct, data); err != nil {
			w.metrics.observeStage("s3_put", "err", start)
			timer.done("s3_put_err")
			return fmt.Errorf("s3 put %s: %w", key, files.ClassifyAWSError(err))
		}
		w.metrics.observeStage("s3_put", "ok", start)
	}

	// Step 3: extract.
	extractor, err := w.Router.For(docFormat)
	if err != nil {
		timer.done("unroutable")
		return fmt.Errorf("route %s: %w", row.sourceFileID, err)
	}
	start := time.Now()
	var result *Result
	if resumable, ok := extractor.(CheckpointingExtractor); ok {
		result, err = resumable.ExtractWithCheckpoints(
			ctx, docFormat, row.filename, data,
			&postgresPageCheckpointStore{
				pool:         w.Pool,
				documentID:   row.documentID,
				eventID:      row.eventID,
				sourceSHA256: hash,
				redactor:     w.redactor,
				logger:       w.log(),
				metrics:      w.metrics,
			},
		)
	} else {
		result, err = extractor.Extract(ctx, docFormat, row.filename, data)
	}
	if err != nil {
		w.metrics.observeStage("extract", "err", start)
		timer.done("extract_err")
		return fmt.Errorf("extract %s: %w", row.sourceFileID, err)
	}
	w.metrics.observeStage("extract", "ok", start)

	// Step 3b: pictures stored inside the document join the image caption
	// queue as images of the same event, before the document settles: a
	// crash after this reads the document again and finds them recorded,
	// where the other order would lose them for good.
	if err := w.storePictures(ctx, row, result.Pictures); err != nil {
		timer.done("pictures_err")
		return err
	}

	// Step 4: settle deterministic bad output without retrying.
	if reason := Reject(result.Markdown); reason != "" {
		w.log().Warn("docs: extraction rejected",
			"document_id", row.documentID, "reason", reason,
			"runes", len([]rune(result.Markdown)))
		w.metrics.rejected.WithLabelValues(string(reason)).Inc()
		timer.done("rejected_" + string(reason))
		return w.settleRejected(ctx, row.documentID, string(reason), result)
	}

	// Step 5: redact before chunking, so a secret can't be split across
	// a chunk boundary and survive, and so what lands in event_chunks
	// (and gets embedded) is already clean.
	//
	// The nil guard is not defensive noise: Redactor is an interface, so
	// an unset field is a nil interface value and calling through it
	// panics rather than no-op'ing (unlike the nil *secretRedactor
	// receiver inside package files, which is nil-safe). Run() always
	// sets it, but a worker driven directly — tests, or any future
	// caller that skips Run — would otherwise crash mid-document.
	text := result.Markdown
	if w.redactor != nil {
		var redactions []string
		text, redactions = w.redactor.Redact(text)
		if len(redactions) > 0 {
			w.log().Warn("docs: redacted secrets from extracted text",
				"document_id", row.documentID, "event_id", row.eventID,
				"rule_ids", redactions, "count", len(redactions))
			w.metrics.redactions.Add(float64(len(redactions)))
		}
	}

	chunks := Split(text)
	if len(chunks) == 0 {
		timer.done("no_chunks")
		return w.settleRejected(ctx, row.documentID, "no_chunks", result)
	}

	// Step 6: commit.
	if err := w.commit(ctx, row, commitInput{
		hash:       hash,
		size:       len(data),
		storageKey: key,
		docFormat:  docFormat,
		result:     result,
		chunks:     chunks,
	}); err != nil {
		timer.done("commit_err")
		return err
	}
	timer.done("ok")
	w.metrics.chunksEmitted.Add(float64(len(chunks)))
	w.log().Info("docs: extracted",
		"document_id", row.documentID, "event_id", row.eventID,
		"filename", row.filename, "format", docFormat,
		"model", result.ModelID, "pages", result.PageCount,
		"runes", len([]rune(text)), "chunks", len(chunks),
		"truncated", result.Truncated)
	return nil
}

// storePictures mirrors a document's pictures and records them as images
// of its event (events.ReconcileDocumentImages). The caption worker
// describes each from the mirror, so a picture costs one caption call and
// nothing is fetched for it again.
func (w *Worker) storePictures(ctx context.Context, row *claimRow, pictures []Picture) error {
	images := make([]events.DocumentImage, 0, len(pictures))
	for _, picture := range pictures {
		hash := files.SHA256Hex(picture.Data)
		key := files.StorageKeyFor(row.source, hash, path.Ext(picture.Name))
		if err := w.S3.Put(ctx, key, picture.MediaType, picture.Data); err != nil {
			return fmt.Errorf("s3 put %s: %w", key, files.ClassifyAWSError(err))
		}
		images = append(images, events.DocumentImage{
			SourceImageID: row.sourceFileID + "/" + picture.Name,
			Mimetype:      picture.MediaType, StorageKey: key, SHA256Hex: hash, Bytes: int64(len(picture.Data)),
		})
	}
	// A picture is sent to the caption model as soon as it is recorded, so
	// the channel's opt-in is checked in the transaction that records it:
	// the check at the start of the read may be seconds old. A channel that
	// has opted out gets nothing queued, and the settle that follows makes
	// the same check and says so.
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return &files.TransientError{Underlying: fmt.Errorf("record document pictures: %w", err)}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	closed, found, err := documentGateClosed(ctx, tx, row.documentID)
	if err != nil {
		return err
	}
	if !found || closed {
		return nil
	}
	if err := events.ReconcileDocumentImagesTx(ctx, tx, row.eventID, row.documentID, images); err != nil {
		return &files.TransientError{Underlying: fmt.Errorf("record document pictures: %w", err)}
	}
	if err := tx.Commit(ctx); err != nil {
		return &files.TransientError{Underlying: fmt.Errorf("record document pictures: %w", err)}
	}
	if len(images) > 0 {
		w.log().Info("docs: queued document pictures for captions",
			"document_id", row.documentID, "event_id", row.eventID, "pictures", len(images))
	}
	return nil
}

// documentGateClosed reports, inside tx, whether the channel a document was
// posted in has opted out of document reading, and locks the document's
// event against a concurrent delete. found is false when the document row is
// gone. It reads the tables directly, in the caller's transaction, for the
// reasons commit gives; an event that is no Slack message has no channel to
// opt out, and a channel with no setting has not opted in.
func documentGateClosed(ctx context.Context, tx pgx.Tx, documentID int64) (closed, found bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
		         SELECT 1 FROM slack_message_details d
		           LEFT JOIN slack_channel_config c ON c.channel_id = d.channel_id
		          WHERE d.event_id = e.id AND NOT COALESCE(c.doc_ingest_enabled, false))
		  FROM events e JOIN event_documents ed ON ed.event_id = e.id
		 WHERE ed.id = $1
		   FOR UPDATE OF e
	`, documentID).Scan(&closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, &files.TransientError{Underlying: fmt.Errorf("gate recheck: %w", err)}
	}
	return closed, true, nil
}

// gateAllows reports whether the channel is opted in. A row with no
// channel (non-slack source) is allowed: no other source has a
// per-channel opt-in concept, and such rows can only exist if some
// future connector creates them deliberately.
func (w *Worker) gateAllows(ctx context.Context, channelID string) (bool, error) {
	if channelID == "" {
		return true, nil
	}
	return w.Gate.DocIngestEnabled(ctx, channelID)
}

type commitInput struct {
	hash       string
	size       int
	storageKey string
	docFormat  string
	result     *Result
	chunks     []Chunk
}

// commit atomically replaces one document's chunks and extraction state.
// The in-transaction gate check prevents publishing after channel opt-out.
func (w *Worker) commit(ctx context.Context, row *claimRow, in commitInput) error {
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		ingestedAt time.Time
		deleted    bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT ingested_at, deleted FROM events WHERE id = $1 FOR UPDATE
	`, row.eventID).Scan(&ingestedAt, &deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("parent event vanished mid-extraction")
		}
		return fmt.Errorf("lock event: %w", err)
	}
	// A soft delete that landed after the claim already removed the
	// event's chunks, and the embedder skips deleted events; chunks
	// written now would never embed. The claim no longer returns this
	// row, so the error is terminal.
	if deleted {
		return errors.New("parent event deleted mid-extraction")
	}

	// Gate check #3, in-transaction. Read directly rather than through
	// the Gate interface: the interface may hold its own pool
	// connection, and a read on a different connection would not be part
	// of this transaction's snapshot — which is the entire point of
	// doing it here. A channel with no config row reads as false
	// (default-deny), matching every other gate site.
	if row.channelID != "" {
		var enabled bool
		err := tx.QueryRow(ctx, `
			SELECT doc_ingest_enabled FROM slack_channel_config WHERE channel_id = $1
		`, row.channelID).Scan(&enabled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			enabled = false
		case err != nil:
			return &files.TransientError{Underlying: fmt.Errorf("gate recheck: %w", err)}
		}
		if !enabled {
			w.log().Info("docs: channel opted out during extraction; discarding",
				"document_id", row.documentID, "channel_id", row.channelID)
			// Roll back the (so far empty) tx and settle outside it, so
			// the settle isn't discarded along with the rollback.
			_ = tx.Rollback(ctx)
			return w.settleRejected(ctx, row.documentID, "gate_closed", in.result)
		}
	}

	// Replace this document's chunks only.
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_chunks WHERE event_document_id = $1
	`, row.documentID); err != nil {
		return fmt.Errorf("delete prior chunks: %w", err)
	}

	// Build the context prefix from data we already have on the row.
	sharer, channelName, dateStr := w.chunkContext(ctx, tx, row)
	for i, c := range in.chunks {
		prefix := ContextPrefix(row.filename, sharer, channelName, dateStr, c)
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_chunks
				(event_id, kind, chunk_idx, content, context_prefix, event_document_id, occurred_at)
			VALUES ($1, 'document_text', $2, $3, $4, $5,
			        (SELECT occurred_at FROM events WHERE id = $1))
		`, row.eventID, i, c.Content, prefix, row.documentID); err != nil {
			return fmt.Errorf("insert chunk %d: %w", i, err)
		}
	}

	// Prefer the most specific partial-extraction reason.
	var incompleteReason *string
	switch {
	case in.result.Truncated:
		reason := "output_truncated"
		incompleteReason = &reason
	case in.result.CostLimited:
		reason := "cost_limit"
		incompleteReason = &reason
	case in.result.PageCount > 0 && in.result.PagesExtracted < in.result.PageCount:
		reason := "missing_pages"
		incompleteReason = &reason
	}

	var costUSD *float64
	if !isLocalMechanism(in.result.ModelID) {
		cost := in.result.CostUSD
		costUSD = &cost
	}

	tag, err := tx.Exec(ctx, `
		UPDATE event_documents
		   SET storage_key       = $2,
		       sha256            = decode($3, 'hex'),
		       bytes             = $4,
		       doc_format        = $5,
		       extract_model     = $6,
		       extracted_at      = now(),
		       page_count        = $7,
		       incomplete_reason = $8,
		       pages_extracted   = $9,
		       rejected_reason   = NULL,
		       extract_cost_usd  = $10,
		       failure_count     = 0,
		       last_failure      = NULL,
		       last_failed_at    = NULL
		 WHERE id = $1
	`, row.documentID, in.storageKey, in.hash, int64(in.size), in.docFormat,
		in.result.ModelID, in.result.PageCount, incompleteReason,
		in.result.PagesExtracted, costUSD)
	if err != nil {
		return fmt.Errorf("update event_documents: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The row was deleted between claim and commit (attachment
		// removed via an edit). Roll back so the chunks we just inserted
		// don't commit as orphans.
		return errors.New("event_documents row vanished mid-extraction")
	}
	if incompleteReason == nil {
		if _, err := tx.Exec(ctx, `
			DELETE FROM event_document_extract_parts WHERE event_document_id = $1
		`, row.documentID); err != nil {
			return fmt.Errorf("delete completed extraction checkpoints: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	lag := time.Since(ingestedAt).Seconds()
	if lag < 0 {
		lag = 0
	}
	w.metrics.ingestedToExtracted.WithLabelValues(row.source).Observe(lag)
	return nil
}

// chunkContext resolves the human-readable bits of the context prefix.
// Best-effort: a missing actor or channel name degrades the prefix but
// must never fail the extraction, so errors are swallowed.
func (w *Worker) chunkContext(ctx context.Context, tx pgx.Tx, row *claimRow) (sharer, channel, date string) {
	var actor, chanName *string
	var occurred time.Time
	err := tx.QueryRow(ctx, `
		SELECT e.actor, e.occurred_at,
		       (SELECT ci.name FROM slack_message_details d
		          JOIN slack_channel_info ci ON ci.channel_id = d.channel_id
		         WHERE d.event_id = e.id)
		  FROM events e WHERE e.id = $1
	`, row.eventID).Scan(&actor, &occurred, &chanName)
	if err != nil {
		return "", "", ""
	}
	if actor != nil {
		sharer = *actor
	}
	if chanName != nil {
		channel = *chanName
	}
	if !occurred.IsZero() {
		date = occurred.UTC().Format("2006-01-02")
	}
	return sharer, channel, date
}

// settleRejected records a terminal rejection and removes stale chunks.
// A non-nil result still records any model usage it incurred.
func (w *Worker) settleRejected(
	ctx context.Context, documentID int64, reason string, result *Result,
) error {
	var mechanism string
	var costUSD *float64
	if result != nil {
		mechanism = result.ModelID
		if !isLocalMechanism(result.ModelID) {
			cost := result.CostUSD
			costUSD = &cost
		}
	}

	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The last gate check, as commit makes for a document with text. A
	// document that is only pictures settles here without reaching commit,
	// after its pictures were recorded; if its channel opted out meanwhile
	// they must not stay.
	gateClosed, found, err := documentGateClosed(ctx, tx, documentID)
	if err != nil {
		return err
	}
	if !found {
		// The attachment was removed while it was being read.
		return nil
	}
	if gateClosed {
		reason = "gate_closed"
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM event_chunks WHERE event_document_id = $1
	`, documentID); err != nil {
		return fmt.Errorf("delete prior chunks: %w", err)
	}
	// A channel that opted out gives up what was read from its documents,
	// their pictures included. A document settled for another reason keeps
	// its pictures: one with no text may be nothing but pictures.
	if reason == "gate_closed" {
		if err := events.DeleteDocumentImages(ctx, tx, documentID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE event_documents
		   SET extracted_at      = now(),
		       rejected_reason   = $2,
		       extract_model     = COALESCE(extract_model, $3),
		       incomplete_reason = NULL,
		       extract_cost_usd  = COALESCE($4, extract_cost_usd)
		 WHERE id = $1
	`, documentID, reason, mechanism, costUSD); err != nil {
		return fmt.Errorf("update event_documents: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_document_extract_parts WHERE event_document_id = $1
	`, documentID); err != nil {
		return fmt.Errorf("delete rejected extraction checkpoints: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (w *Worker) refreshClaim(ctx context.Context, documentID int64) error {
	_, err := w.Pool.Exec(ctx, `
		UPDATE event_documents SET claimed_at = now() WHERE id = $1
	`, documentID)
	return err
}

// bumpFailure records a per-row failure. Known-permanent errors jump
// straight to the cap so the worker doesn't spend four more claim
// cycles on a guaranteed re-failure.
func (w *Worker) bumpFailure(ctx context.Context, documentID int64, cause error) error {
	if files.IsTransient(cause) {
		return nil
	}
	msg := truncateRunes(cause.Error(), 512)
	target := "failure_count + 1"
	if isPermanent(cause) {
		target = fmt.Sprintf("%d", events.MaxFailures)
	}
	_, err := w.Pool.Exec(ctx, fmt.Sprintf(`
		UPDATE event_documents
		   SET failure_count  = %s,
		       last_failure   = $2,
		       last_failed_at = now()
		 WHERE id = $1
	`, target), documentID, msg)
	return err
}

// isLocalMechanism reports whether a model id is actually a local-parser
// mechanism tag (SpreadsheetMechanism, DocumentMechanism) rather than a
// billed model — the "local:" prefix is the two local parsers' own
// naming convention (router.go), reused here as the signal for "this
// document cost nothing and made no gateway call."
func isLocalMechanism(modelID string) bool {
	return strings.HasPrefix(modelID, "local:")
}

func isPermanent(err error) bool {
	if err == nil {
		return false
	}
	var pe *files.PermanentError
	if errors.As(err, &pe) {
		return true
	}
	// Slack file deleted from the workspace; never coming back.
	msg := err.Error()
	return strings.Contains(msg, "file_not_found") || strings.Contains(msg, "file_deleted")
}

// recordExhaustion is the bumpFailure variant for transient-retries-
// exhausted. Bumps the count despite the transient classification (the
// point is that retrying isn't working right now) but leaves claimed_at
// set, so staleClaimAge is the natural backoff before re-claim.
func (w *Worker) recordExhaustion(ctx context.Context, documentID int64, cause error) error {
	_, err := w.Pool.Exec(ctx, `
		UPDATE event_documents
		   SET failure_count  = failure_count + 1,
		       last_failure   = $2,
		       last_failed_at = now()
		 WHERE id = $1
	`, documentID, truncateRunes(cause.Error(), 512))
	return err
}

// updateGauges piggy-backs queue-depth metrics onto the loop, throttled
// so the COUNT(*)s don't fire every iteration.
func (w *Worker) updateGauges(ctx context.Context) {
	if !w.metrics.shouldUpdateGauges() {
		return
	}
	var pending, stranded, partial int64
	_ = w.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM event_documents
		    WHERE extracted_at IS NULL AND failure_count < $1),
		  (SELECT count(*) FROM event_documents
		    WHERE extracted_at IS NULL AND failure_count >= $1),
		  (SELECT count(*) FROM event_documents
		    WHERE incomplete_reason IS NOT NULL)
	`, events.MaxFailures).Scan(&pending, &stranded, &partial)
	w.metrics.pending.Set(float64(pending))
	w.metrics.stranded.Set(float64(stranded))
	w.metrics.partial.Set(float64(partial))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// truncateRunes is rune-aware so multi-byte codepoints aren't split
// mid-character (Postgres rejects invalid UTF-8 in text columns).
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
