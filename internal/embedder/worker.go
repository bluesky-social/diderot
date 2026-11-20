package embedder

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

// WorkerConfig controls the polling embedder worker. Docs/design.md
// § Embedder worker: "Single fetcher + N embedder goroutines ... vLLM
// call is the rate-determining step; DB write becomes trivial once
// the HNSW index is out of Postgres."
type WorkerConfig struct {
	// Concurrency is the number of goroutines that call vLLM in
	// parallel. Each consumes batches from a shared channel filled by
	// one fetcher goroutine. Default 1.
	//
	// The fetcher tracks handed-off ids in memory so concurrent workers
	// receive distinct batches without holding database row locks across
	// the embedding HTTP call.
	Concurrency int
	// BatchSize is the fixed number of rows per fetcher SELECT + per
	// vLLM embed request. Tuned empirically: past ~64 rows/batch
	// vLLM throughput collapses. Default 32.
	BatchSize int
	// PollInterval is the fixed sleep while the queue is empty.
	// Default 2s.
	PollInterval time.Duration
	// MinBackoff / MaxBackoff govern fetch-error backoff. Default
	// 5s / 60s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (c *WorkerConfig) fillDefaults() {
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.BatchSize == 0 {
		c.BatchSize = 32
	}
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.MinBackoff == 0 {
		c.MinBackoff = 5 * time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 60 * time.Second
	}
}

// Worker polls event_chunks for rows with NULL embedded_at (and below
// the failure cap), sends them through an Embedder, upserts the vectors
// into qdrant, then stamps embedded_at + embedder_id in Postgres.
// Leader-only: a single replica runs this at a time so one GPU's
// worth of compute doesn't get paid twice.
//
// Shape: one fetcher goroutine selects unembedded rows (no locking)
// and pushes batches onto a bounded channel. N embedder goroutines
// pop batches, call vLLM, write to qdrant, UPDATE Postgres. The
// qdrant-first order matters — a crash between steps leaves the
// Postgres row still NULL, so the next run picks it up. Reversed
// order would mark a chunk done in Postgres before its vector was
// visible in qdrant, permanently losing that chunk to search.
type Worker struct {
	Pool     *pgxpool.Pool
	Embedder Embedder
	Store    pointStore
	Logger   *slog.Logger
	// Registry is the prometheus registerer the worker's metrics
	// attach to. nil is fine — the worker registers into a
	// throwaway registry, metrics still exist but aren't scraped.
	// Production sets this to prometheus.DefaultRegisterer.
	Registry prometheus.Registerer
	Config   WorkerConfig

	metrics *workerMetrics
}

type pointStore interface {
	Upsert(context.Context, []qdrantstore.Point) error
	Dimensions() int
	// Collection names the collection the points go to, which each chunk
	// is stamped with.
	Collection() string
}

// maxEmbedFailures matches the literal claim predicate and the partial
// index `WHERE failure_count < 5`. The SQL must keep the literal rather than
// parameterize this value: a PostgreSQL generic prepared plan cannot
// prove that a parameter implies the partial-index predicate. Raising
// this requires rebuilding the index and updating the claim literal.
const maxEmbedFailures = 5

var qdrantWriteTimeout = 30 * time.Second

// Run blocks until ctx is cancelled. Spawns one fetcher +
// Concurrency embedder goroutines; returns once all have exited.
func (w *Worker) Run(ctx context.Context) error {
	w.Config.fillDefaults()
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Registry == nil {
		w.Registry = prometheus.NewRegistry()
	}
	w.metrics = newWorkerMetrics(w.Registry)
	// Dim must match the collection's configured vector size.
	if d, want := w.Embedder.Dimensions(), w.Store.Dimensions(); d != want {
		return fmt.Errorf("embedder dim %d != qdrant collection dim %d", d, want)
	}

	ch := make(chan chunkBatch, 2*w.Config.Concurrency)
	inFlight := newInFlightSet()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.fetch(ctx, ch, inFlight)
	}()
	for i := 0; i < w.Config.Concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.embed(ctx, id, ch, inFlight)
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// chunkRow carries the row + its parent event's qdrant-payload
// fields. One big SELECT with a JOIN is cheaper than a second round
// trip per chunk to fetch the metadata.
type chunkRow struct {
	id             int64
	content        string
	contextPrefix  string // "" = no prefix (NULL in the table)
	eventID        int64
	source         string
	actorID        string
	actorCanonical string // events.actor_canonical: email or 'slack:<user_id>'
	threadID       string
	occurredAt     time.Time
	scope          string // events.scope: visibility scope for the qdrant payload
}

// embedInput builds the string passed to vLLM: prefix + "\n\n" +
// content when a prefix exists, content alone otherwise. Kept as a
// method so the per-chunk and per-batch paths agree on the format.
func (r *chunkRow) embedInput() string {
	if r.contextPrefix == "" {
		return r.content
	}
	return r.contextPrefix + "\n\n" + r.content
}

type chunkBatch struct {
	rows []chunkRow
}

// inFlightSet tracks chunks handed to the bounded worker channel but
// not yet finished. It replaces the old monotonic queue cursor: the
// fetcher can always start at the newest row while excluding work
// already assigned to another goroutine.
type inFlightSet struct {
	mu  sync.Mutex
	ids map[int64]struct{}
}

func newInFlightSet() *inFlightSet {
	return &inFlightSet{ids: make(map[int64]struct{})}
}

func (s *inFlightSet) snapshot() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	return ids
}

func (s *inFlightSet) add(rows []chunkRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		s.ids[row.id] = struct{}{}
	}
}

func (s *inFlightSet) release(rows []chunkRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		delete(s.ids, row.id)
	}
}

// fetch runs the single SELECT loop, always selecting the top
// pending batch (newest event recency first). It does NOT keep a
// monotonic cursor through chunk ids: a cursor that advances past ids
// is invalid once the queue is recency-ordered, because a new
// high-priority chunk arriving at the front has a larger id and would
// be skipped by an "id > cursor" filter. See the design note at
// fetchBatch for why the recency rework and the cursor removal are
// coupled.
//
// Instead of a cursor, the fetcher excludes exactly the ids it has
// already handed to the channel and whose embedded_at isn't stamped
// yet (the in-flight set, populated on push and drained by release()
// when an embedder finishes). That prevents the fetcher from
// re-selecting the same top rows while an embedder is mid-embed —
// "always top batch" WITHOUT a re-selection busy loop or duplicate
// embedding — while remaining correct under recency ordering: an
// in-flight id is excluded only while being processed, so a brand-new
// higher-priority row that is not in flight is selected immediately.
// When every pending row is in flight the SELECT returns empty and we
// poll, so the loop has a natural idle path too.
func (w *Worker) fetch(ctx context.Context, ch chan<- chunkBatch, inFlight *inFlightSet) {
	defer close(ch)
	backoff := w.Config.MinBackoff
	batchSize := w.Config.BatchSize

	// Gauge-snapshot cadence. The fetcher queries the DB once per
	// iteration anyway; piggy-back the pending/stranded counts so
	// we get free updates. Cheap partial-index COUNT(*).
	var lastGaugeUpdate time.Time
	updateGauges := func() {
		if time.Since(lastGaugeUpdate) < 5*time.Second {
			return
		}
		lastGaugeUpdate = time.Now()
		var pending, stranded int64
		_ = w.Pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM event_chunks
			    WHERE embedded_at IS NULL AND failure_count < 5),
			  (SELECT count(*) FROM event_chunks
			    WHERE failure_count >= 5)
		`).Scan(&pending, &stranded)
		w.metrics.queuePending.Set(float64(pending))
		w.metrics.queueStranded.Set(float64(stranded))
	}

	for {
		if ctx.Err() != nil {
			return
		}
		updateGauges()

		// Snapshot the in-flight ids to exclude from this SELECT.
		rows, err := w.fetchBatch(ctx, batchSize, inFlight.snapshot())
		if err != nil {
			// A fetch cut short by the worker stopping is not a failure.
			if ctx.Err() != nil {
				return
			}
			w.Logger.Error("embedder: fetch", "err", err)
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			backoff = nextBackoff(backoff, w.Config.MaxBackoff)
			continue
		}
		backoff = w.Config.MinBackoff

		if len(rows) == 0 {
			// Nothing pending outside the in-flight set (or the queue
			// is genuinely empty). Poll at a fixed short interval;
			// fetch errors retain exponential backoff.
			if sleepCtx(ctx, w.Config.PollInterval) != nil {
				return
			}
			continue
		}

		// Mark the batch in flight before it's observably handed off,
		// so a fast loop can't re-select it between push and embed.
		inFlight.add(rows)

		select {
		case <-ctx.Done():
			return
		case ch <- chunkBatch{rows: rows}:
		}
	}
}

// Design note (embedder): recency order and the cursor rework are
// coupled. The queue used to be FIFO by chunk id — `ORDER BY c.id`
// with a `c.id > $cursor` pager — and that monotonic cursor was
// correct because "next" was always "later-inserted", so advancing
// past ids could never skip work. Ordering by event recency
// (occurred_at DESC, id DESC) breaks that invariant: a fresh
// interactive chunk inserted NOW has the largest id and sorts FIRST,
// so an id-cursor that jumped past old ids would skip it — the
// head-of-line starvation recency order exists to prevent. There is no
// monotonic cursor compatible with a recency
// queue, so the cursor is replaced by an in-flight exclusion set:
// always select the top pending batch modulo the ids currently being
// embedded. New higher-priority rows are never excluded and win the
// next claim; the embedded_at marker (rows leave the pending
// predicate once written) plus the bounded channel and the empty-queue
// poll prevent any busy loop; and the exclusion set additionally
// avoids re-embedding a batch that is already mid-flight.
//
// fetchBatch pulls the next n unembedded chunks — newest event
// recency first — excluding ids currently in flight, joined to their
// parent event for the qdrant payload. deleted=true events are
// excluded — their chunks are physically gone anyway (the events
// sink deletes chunk rows on tombstone) but the JOIN is a
// belt-and-suspenders defense against a chunk row leaking past a
// partial-failure window.
func (w *Worker) fetchBatch(ctx context.Context, n int, excluded []int64) ([]chunkRow, error) {
	// nil encodes as SQL NULL through pgx, which would make
	// `NOT (c.id = ANY(NULL))` evaluate to NULL/unknown and drop
	// every row. The in-flight set is always non-nil, but callers
	// can pass nil; normalize to an empty array so the ANY predicate
	// is "false for all ids".
	if excluded == nil {
		excluded = []int64{}
	}
	// Interpolate the compile-time integer so PostgreSQL sees a literal
	// predicate even after pgx switches this statement to a generic plan.
	query := fmt.Sprintf(`
		SELECT c.id, c.content, COALESCE(c.context_prefix, ''),
		       e.id, e.source, COALESCE(e.actor_id, ''),
		       e.actor_canonical,
		       COALESCE(e.thread_id, ''), e.occurred_at, e.scope
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE c.embedded_at IS NULL
		   AND c.failure_count < %d
		   AND e.deleted = false
		   AND NOT (c.id = ANY($2::bigint[]))
		 ORDER BY c.occurred_at DESC, c.id DESC
		 LIMIT $1
	`, maxEmbedFailures)
	rows, err := w.Pool.Query(ctx, query, n, excluded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []chunkRow
	for rows.Next() {
		var r chunkRow
		if err := rows.Scan(
			&r.id, &r.content, &r.contextPrefix,
			&r.eventID, &r.source, &r.actorID, &r.actorCanonical,
			&r.threadID, &r.occurredAt, &r.scope,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// embed is the consumer loop. Pops batches, processes each, then
// releases the batch's ids from the fetcher's in-flight set so they
// can be re-selected if they failed (or stay gone if embedded). No
// tx across the HTTP call.
func (w *Worker) embed(ctx context.Context, id int, ch <-chan chunkBatch, inFlight *inFlightSet) {
	for batch := range ch {
		if ctx.Err() != nil {
			return
		}
		err := w.embedOne(ctx, id, batch.rows)
		inFlight.release(batch.rows)
		if err != nil {
			w.Logger.Error("embedder: embed", "worker", id, "err", err)
			if sleepCtx(ctx, w.Config.MinBackoff) != nil {
				return
			}
		}
	}
}

// embedOne processes a single batch. Two failure modes:
//
//  1. Transient (vLLM 5xx, TCP refused, timeout) — sleep + retry the
//     whole batch, indefinitely, without touching any row's
//     failure_count. The service is having a moment; it's not that
//     this input is bad.
//
//  2. Terminal (4xx non-429, embedding-count mismatch, dim mismatch)
//     — fall through to per-row retry so one malformed row doesn't
//     drop its healthy batch-mates.
//
// Without the TransientError check, an embedding server that answers
// 503 while its pods restart would strand every row in the batch
// across 5 retry cycles, because per-row retry bumps failure_count
// for each 503.
func (w *Worker) embedOne(ctx context.Context, id int, rows []chunkRow) error {
	timer := w.metrics.startBatch()
	w.metrics.batchSize.Observe(float64(len(rows)))
	start := time.Now()
	contents := make([]string, len(rows))
	for i, r := range rows {
		contents[i] = r.embedInput()
	}

	vecs, err := w.batchEmbedWithTransientRetry(ctx, id, contents)
	if err != nil {
		w.Logger.Warn("embedder: batch failed, retrying per-row",
			"worker", id, "batch_size", len(rows), "err", err)
		// Per-row path records its own outcome via bumpFailure; the
		// overall batch here records terminal_err since we fell out
		// of the batch-level transient retry loop.
		timer.done("terminal_err")
		return w.retryIndividually(ctx, rows)
	}
	if err := w.writeBack(ctx, rows, vecs); err != nil {
		timer.done("terminal_err")
		return fmt.Errorf("write back: %w", err)
	}
	w.metrics.chunksEmbedded.Add(float64(len(rows)))
	timer.done("ok")
	w.Logger.Info("embedder: batch done",
		"worker", id, "rows", len(rows),
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// batchEmbedWithTransientRetry retries the whole batch when the
// Embedder returns a TransientError, with exponential backoff
// capped at MaxBackoff. Returns the non-transient error (or nil)
// directly — no retry for terminal errors.
func (w *Worker) batchEmbedWithTransientRetry(ctx context.Context, id int, contents []string) ([][]float32, error) {
	backoff := w.Config.MinBackoff
	for {
		vecs, err := w.Embedder.Embed(ctx, contents)
		if err == nil {
			return vecs, nil
		}
		if !IsTransient(err) {
			return nil, err
		}
		w.Logger.Warn("embedder: transient batch error; sleeping + retrying",
			"worker", id, "batch_size", len(contents), "backoff", backoff, "err", err)
		if sleepErr := sleepCtx(ctx, backoff); sleepErr != nil {
			return nil, sleepErr
		}
		backoff = nextBackoff(backoff, w.Config.MaxBackoff)
	}
}

// retryIndividually embeds rows one at a time. Each row's embed
// goes through the same transient-retry dance as the batch path, so
// a service hiccup in the middle of per-row doesn't poison every
// remaining row. True poison (repeated non-transient failure on a
// single row — vLLM 400 on malformed input, e.g.) bumps
// failure_count + records last_failure; the partial index drops
// them after maxEmbedFailures.
func (w *Worker) retryIndividually(ctx context.Context, rows []chunkRow) error {
	for _, r := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		vecs, err := w.batchEmbedWithTransientRetry(ctx, -1, []string{r.embedInput()})
		if err != nil {
			// Non-transient per-row error. Actual poison.
			w.Logger.Warn("embedder: poison row",
				"chunk_id", r.id, "content_len", len(r.content), "err", err)
			w.metrics.poisonRows.Inc()
			if uerr := w.bumpFailure(ctx, r.id, err); uerr != nil {
				return fmt.Errorf("bump failure for %d: %w", r.id, uerr)
			}
			continue
		}
		if err := w.writeBack(ctx, []chunkRow{r}, vecs); err != nil {
			return fmt.Errorf("write back chunk %d: %w", r.id, err)
		}
		w.metrics.chunksEmbedded.Add(1)
	}
	return nil
}

// bumpFailure records a per-row embed failure. Truncates the stored
// error message to a schema-friendly size — pathological embedders
// (long backend stack traces) can easily produce 10KB errors that
// bloat the table with redundant debug detail. 512 runes is plenty
// to identify a class of failure.
//
// Truncation is rune-aware: a byte-level slice can split a multi-byte
// codepoint in half, and Postgres rejects invalid UTF-8 in text
// columns. A rejected UPDATE would prevent failure_count from ever
// incrementing, leaving the poison chunk stuck in the embedder queue
// indefinitely.
func (w *Worker) bumpFailure(ctx context.Context, chunkID int64, cause error) error {
	msg := truncateRunes(cause.Error(), 512)
	_, err := w.Pool.Exec(ctx, `
		UPDATE event_chunks
		   SET failure_count  = failure_count + 1,
		       last_failure   = $2,
		       last_failed_at = now()
		 WHERE id = $1
	`, chunkID, msg)
	return err
}

// truncateRunes returns s unchanged if it is at most n runes long;
// otherwise it returns the first n runes. Byte-level slicing would
// risk splitting a multi-byte codepoint.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		// Fast path: ASCII can't exceed n runes if byte length is <= n.
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// writeBack locks only the chunk rows that survived vector generation, then
// upserts and acknowledges them within one short transaction. The row locks
// serialize the final Qdrant write against deletion, preventing a finished
// delete from being resurrected by an embedder that started earlier.
func (w *Worker) writeBack(ctx context.Context, rows []chunkRow, vecs [][]float32) error {
	if len(rows) != len(vecs) {
		return fmt.Errorf("chunk/vec length mismatch: %d vs %d", len(rows), len(vecs))
	}
	if len(rows) == 0 {
		return nil
	}

	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin writeback: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	chunkIDs := make([]int64, len(rows))
	rowByID := make(map[int64]int, len(rows))
	for i, r := range rows {
		chunkIDs[i] = r.id
		rowByID[r.id] = i
	}
	locked, err := tx.Query(ctx, `
		SELECT id
		  FROM event_chunks
		 WHERE id = ANY($1::bigint[])
		 ORDER BY id
		 FOR UPDATE
	`, chunkIDs)
	if err != nil {
		return fmt.Errorf("lock surviving chunks: %w", err)
	}
	var surviving []int64
	for locked.Next() {
		var id int64
		if err := locked.Scan(&id); err != nil {
			locked.Close()
			return fmt.Errorf("scan surviving chunk: %w", err)
		}
		surviving = append(surviving, id)
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return fmt.Errorf("read surviving chunks: %w", err)
	}
	if len(surviving) == 0 {
		return tx.Commit(ctx)
	}

	points := make([]qdrantstore.Point, 0, len(surviving))
	writtenScopes := make([]string, 0, len(surviving))
	writtenTimes := make([]time.Time, 0, len(surviving))
	for _, id := range surviving {
		i := rowByID[id]
		r := rows[i]
		points = append(points, qdrantstore.Point{
			ID:             uint64(r.id),
			Vector:         vecs[i],
			EventID:        r.eventID,
			Source:         r.source,
			ActorID:        r.actorID,
			ActorCanonical: r.actorCanonical,
			ThreadID:       r.threadID,
			OccurredAt:     r.occurredAt,
			Scope:          r.scope,
		})
		writtenScopes = append(writtenScopes, r.scope)
		writtenTimes = append(writtenTimes, r.occurredAt)
	}
	qdrantCtx, cancel := context.WithTimeout(ctx, qdrantWriteTimeout)
	err = w.Store.Upsert(qdrantCtx, points)
	cancel()
	if err != nil {
		return fmt.Errorf("qdrant upsert: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		WITH sent AS (
			SELECT * FROM unnest($2::bigint[], $3::text[], $4::timestamptz[]) AS x(id, scope, occurred_at)
		)
		UPDATE event_chunks c
		   SET embedded_at = clock_timestamp(),
		       embedder_id = $1,
		       embedded_collection = $5,
		       scope_sync_needed = e.scope IS DISTINCT FROM sent.scope OR c.occurred_at IS DISTINCT FROM sent.occurred_at,
		       scope_sync_attempts = CASE WHEN e.scope = sent.scope AND c.occurred_at = sent.occurred_at THEN 0 ELSE c.scope_sync_attempts END,
		       scope_sync_next_attempt_at = CASE WHEN e.scope IS DISTINCT FROM sent.scope OR c.occurred_at IS DISTINCT FROM sent.occurred_at THEN now() ELSE c.scope_sync_next_attempt_at END,
		       scope_sync_last_error = CASE WHEN e.scope = sent.scope AND c.occurred_at = sent.occurred_at THEN NULL ELSE c.scope_sync_last_error END
		  FROM sent, events e
		 WHERE c.id = sent.id
		   AND e.id = c.event_id
	`, w.Embedder.ID(), surviving, writtenScopes, writtenTimes, w.Store.Collection())
	if err != nil {
		return fmt.Errorf("update chunks: %w", err)
	}
	if got, want := tag.RowsAffected(), int64(len(surviving)); got != want {
		return fmt.Errorf("acknowledged %d surviving chunks, want %d", got, want)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit writeback: %w", err)
	}
	return nil
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
