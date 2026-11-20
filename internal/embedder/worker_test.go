package embedder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// fakeEmbedder produces deterministic fixed-dim vectors based on the
// chunk id embedded in the text. Lets us verify which rows made it
// to qdrant without actually running vLLM.
type fakeEmbedder struct {
	dim   int
	id    string
	mu    sync.Mutex
	calls int

	// failContent, when non-empty, causes Embed to error on any
	// batch whose texts contain that substring. Used to exercise
	// the retry-individually + bump-failure path.
	failContent string

	// transientFailuresRemaining, when >0, causes Embed to return
	// a TransientError until the counter drains. Used to exercise
	// the "sleep + retry batch" path without burning failure_count.
	transientFailuresRemaining int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	if f.transientFailuresRemaining > 0 {
		f.transientFailuresRemaining--
		f.mu.Unlock()
		return nil, &TransientError{Underlying: errors.New("fake: vllm 503")}
	}
	f.mu.Unlock()

	if f.failContent != "" {
		for _, t := range texts {
			if strings.Contains(t, f.failContent) {
				return nil, errors.New("fake: bad content")
			}
		}
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		// Hash-lite: sum of runes, mixed into each dim slot. Deterministic
		// per text, different per text, not-all-zeros. Good enough for
		// a cosine-distance non-pathology test.
		s := float32(0)
		for _, r := range t {
			s += float32(r) * 0.0001
		}
		for j := range v {
			v[j] = s + float32(j)*0.0001
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Dimensions() int { return f.dim }
func (f *fakeEmbedder) ID() string      { return f.id }

func testDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "embedder")
}

func testQdrantAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set; skipping qdrant integration test")
	}
	return addr
}

// setupWorld wires up a fresh pool + a fresh qdrant collection and
// returns both plus a cleanup func. Each test gets its own collection
// (time-derived name) so parallel runs don't collide.
func setupWorld(t *testing.T) (*pgxpool.Pool, *qdrantstore.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	coll := fmt.Sprintf("diderot_test_embed_%d", time.Now().UnixNano())
	store, err := qdrantstore.New(qdrantstore.Config{
		Addr:       testQdrantAddr(t),
		Collection: coll,
		Dimensions: testVectorDim,
	}, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("qdrant New: %v", err)
	}
	if err := store.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	t.Cleanup(func() {
		// Drop before close: leaked per-test collections accumulate
		// in the dev qdrant volume until the container OOMs.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = store.Drop(cleanupCtx)
		_ = store.Close()
	})
	return pool, store
}

func TestWorkerHappyPath(t *testing.T) {
	pool, store := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// Ingest a few events via the real sink so chunks land in the
	// queue properly.
	for i := 0; i < 5; i++ {
		if _, err := sink.Upsert(ctx, events.Event{
			Scope:      events.ScopePublic,
			Source:     "slack",
			ExternalID: fmt.Sprintf("C1:%d", i),
			OccurredAt: time.Unix(1700000000+int64(i), 0),
			Actor:      "alice",
			ActorID:    "U1",
			ThreadID:   "T1",
			Title:      "#ops",
			Body:       fmt.Sprintf("message number %d", i),
		}); err != nil {
			t.Fatalf("seed Upsert %d: %v", i, err)
		}
	}

	// Count chunks in the queue.
	var initial int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM event_chunks WHERE embedded_at IS NULL AND failure_count < 5`,
	).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	if initial != 5 {
		t.Fatalf("queue depth: got %d, want 5", initial)
	}

	emb := &fakeEmbedder{dim: testVectorDim, id: "test:fake:v1"}
	w := &Worker{
		Pool:     pool,
		Embedder: emb,
		Store:    store,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Config: WorkerConfig{
			Concurrency: 2,
			BatchSize:   3,
			MinBackoff:  50 * time.Millisecond,
			MaxBackoff:  100 * time.Millisecond,
		},
	}

	// Qdrant is shared by concurrently running integration-test packages in
	// CI. Keep the worker bounded, but leave enough time for both waited
	// upserts to finish under runner contention.
	runCtx, runCancel := context.WithTimeout(ctx, 15*time.Second)
	defer runCancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// Do not cancel the workers until Postgres proves every Qdrant-first
	// write completed. The old two-second poll deadline fell through without
	// asserting that condition, then canceled in-flight Qdrant upserts.
	observeCtx, stopObserving := context.WithTimeout(ctx, 14*time.Second)
	defer stopObserving()
	remaining := initial
	var observeErr error
	for remaining > 0 {
		if err := pool.QueryRow(observeCtx,
			`SELECT count(*) FROM event_chunks WHERE embedded_at IS NULL AND failure_count < 5`,
		).Scan(&remaining); err != nil {
			observeErr = err
			break
		}
		if remaining == 0 {
			break
		}
		if sleepCtx(observeCtx, 50*time.Millisecond) != nil {
			break
		}
	}
	runCancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("worker Run returned %v, want a context error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not shut down within deadline")
	}
	if observeErr != nil {
		t.Fatalf("observe embed queue: %v", observeErr)
	}
	if remaining != 0 {
		t.Fatalf("embed queue did not drain before deadline: %d rows remain", remaining)
	}
	if emb.calls != 2 {
		t.Fatalf("embed calls: got %d, want 2 distinct batches (3 + 2 rows)", emb.calls)
	}

	// All 5 chunks should now have embedded_at + embedder_id populated.
	var embedded int
	var sampleEmbedderID string
	err := pool.QueryRow(ctx, `
		SELECT count(*), MIN(embedder_id)
		  FROM event_chunks
		 WHERE embedded_at IS NOT NULL
	`).Scan(&embedded, &sampleEmbedderID)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != 5 {
		t.Fatalf("embedded rows: got %d, want 5", embedded)
	}
	if sampleEmbedderID != "test:fake:v1" {
		t.Errorf("embedder_id mismatch: got %q", sampleEmbedderID)
	}
	// Each chunk records the collection its vector went to, which is how a
	// later change of collection knows what the new one lacks.
	var elsewhere int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM event_chunks
		 WHERE embedded_at IS NOT NULL AND embedded_collection IS DISTINCT FROM $1`, store.Collection()).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	if elsewhere != 0 {
		t.Errorf("%d embedded chunks do not record the collection %s", elsewhere, store.Collection())
	}

	// Qdrant should have 5 points with the right payload shape.
	countCtx, countCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer countCancel()
	n, err := store.CountPoints(countCtx)
	if err != nil {
		t.Fatalf("CountPoints: %v", err)
	}
	if n != 5 {
		t.Errorf("qdrant count: got %d, want 5", n)
	}

	// Search for anything; we should see hits tagged source=slack.
	vec := make([]float32, testVectorDim)
	hits, err := store.Search(countCtx, vec, qdrantstore.SearchFilters{Sources: []string{"slack"}, AllowedScopes: []string{events.ScopePublic}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 5 {
		t.Errorf("Search hits: got %d, want 5", len(hits))
	}
	for _, h := range hits {
		if h.EventID == 0 {
			t.Errorf("hit %d missing event_id payload", h.ChunkID)
		}
	}
}

func TestWorkerPoisonRowBumpsFailure(t *testing.T) {
	pool, store := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// Two events. One contains the magic failContent so the fake
	// embedder refuses to process batches containing it.
	okBody := "normal text"
	poisonBody := "POISON: reject me"
	if _, err := sink.Upsert(ctx, events.Event{
		Scope:  events.ScopePublic,
		Source: "slack", ExternalID: "ok",
		OccurredAt: time.Unix(1, 0), Body: okBody,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Upsert(ctx, events.Event{
		Scope:  events.ScopePublic,
		Source: "slack", ExternalID: "bad",
		OccurredAt: time.Unix(2, 0), Body: poisonBody,
	}); err != nil {
		t.Fatal(err)
	}

	w := &Worker{
		Pool:     pool,
		Embedder: &fakeEmbedder{dim: testVectorDim, id: "test:fake:v1", failContent: "POISON"},
		Store:    store,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Config: WorkerConfig{
			Concurrency: 1,
			BatchSize:   10, // both rows in one batch; poison triggers per-row retry
			MinBackoff:  50 * time.Millisecond,
			MaxBackoff:  100 * time.Millisecond,
		},
	}

	runCtx, runCancel := context.WithTimeout(ctx, 5*time.Second)
	defer runCancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// Wait until BOTH the good row is embedded AND the poison row's
	// last_failure has been recorded. Only checking "good is
	// embedded" was racy — the per-row retry that bumps last_failure
	// can lag behind the good row's commit by enough that the
	// 1500ms deadline expires before the bump lands. We need both
	// signals before runCancel.
	deadline := time.Now().Add(4500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var goodDone int
		var poisonHasFailure bool
		_ = pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM event_chunks c
			     JOIN events e ON e.id = c.event_id
			    WHERE e.external_id = 'ok' AND c.embedded_at IS NOT NULL),
			  (SELECT last_failure IS NOT NULL FROM event_chunks c
			     JOIN events e ON e.id = c.event_id
			    WHERE e.external_id = 'bad')
		`).Scan(&goodDone, &poisonHasFailure)
		if goodDone >= 1 && poisonHasFailure {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	runCancel()
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("worker Run returned %v, want a context error", err)
	}

	// Good chunk embedded_at != NULL; poison chunk failure_count >= 1
	// with last_failure set.
	var okEmbedded, poisonFailures int
	var lastFailure *string
	err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM event_chunks c
			   JOIN events e ON e.id = c.event_id
			  WHERE e.external_id = 'ok' AND c.embedded_at IS NOT NULL),
			(SELECT failure_count FROM event_chunks c
			   JOIN events e ON e.id = c.event_id
			  WHERE e.external_id = 'bad'),
			(SELECT last_failure FROM event_chunks c
			   JOIN events e ON e.id = c.event_id
			  WHERE e.external_id = 'bad')
	`).Scan(&okEmbedded, &poisonFailures, &lastFailure)
	if err != nil {
		t.Fatal(err)
	}
	if okEmbedded != 1 {
		t.Errorf("good chunk not embedded; okEmbedded=%d", okEmbedded)
	}
	if poisonFailures < 1 {
		t.Errorf("poison failure_count not bumped: %d", poisonFailures)
	}
	if lastFailure == nil || !strings.Contains(*lastFailure, "bad content") {
		t.Errorf("last_failure should capture the error: %v", lastFailure)
	}

	// The failure is timed by the database's clock, which the Failures page
	// dates it by; a chunk that never failed has no failure time.
	var poisonTimed, okTimed bool
	err = pool.QueryRow(ctx, `
		SELECT
			(SELECT COALESCE(c.last_failed_at BETWEEN now() - interval '1 minute' AND now(), false)
			   FROM event_chunks c JOIN events e ON e.id = c.event_id
			  WHERE e.external_id = 'bad'),
			(SELECT c.last_failed_at IS NOT NULL
			   FROM event_chunks c JOIN events e ON e.id = c.event_id
			  WHERE e.external_id = 'ok')
	`).Scan(&poisonTimed, &okTimed)
	if err != nil {
		t.Fatal(err)
	}
	if !poisonTimed {
		t.Error("last_failed_at should be when the poison chunk failed, within the last minute")
	}
	if okTimed {
		t.Error("a chunk that embedded without failing should have no last_failed_at")
	}
}

func TestWorkerTransientRetryPreservesFailureCount(t *testing.T) {
	// While an embedding server answers 503 (its pods restarting, say),
	// a per-row retry that bumped failure_count on every attempt would
	// strand rows at the cap. TransientError bypasses failure_count: the
	// batch retries until the service is back.
	pool, store := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	if _, err := sink.Upsert(ctx, events.Event{
		Scope:  events.ScopePublic,
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "hello",
	}); err != nil {
		t.Fatal(err)
	}

	// Embedder fakes 3 transient failures, then succeeds.
	emb := &fakeEmbedder{
		dim: testVectorDim, id: "test:fake:v1",
		transientFailuresRemaining: 3,
	}
	w := &Worker{
		Pool: pool, Embedder: emb, Store: store,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Config: embedderWorkerConfigForTest(),
	}
	// Qdrant is shared by concurrently running integration-test packages in
	// CI. Keep this bounded, but allow a successful upsert to finish under
	// runner contention after the deliberately injected retry backoffs.
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// Wait for the chunk to end up embedded_at != NULL, leaving a small
	// margin for worker shutdown before the bounded run context expires.
	observeCtx, stopObserving := context.WithTimeout(ctx, 14*time.Second)
	defer stopObserving()
	for {
		var n int
		err := pool.QueryRow(observeCtx, `SELECT count(*) FROM event_chunks WHERE embedded_at IS NOT NULL`).Scan(&n)
		if err != nil {
			if observeCtx.Err() != nil {
				break
			}
			t.Fatalf("observe embedded chunk: %v", err)
		}
		if n >= 1 {
			break
		}
		if sleepCtx(observeCtx, 50*time.Millisecond) != nil {
			break
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("worker Run returned %v, want a context error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not shut down within deadline")
	}

	// Verify: chunk embedded, failure_count still 0.
	var (
		failureCount int
		lastFailure  *string
		embeddedAt   *time.Time
	)
	verifyCtx, stopVerifying := context.WithTimeout(ctx, 5*time.Second)
	defer stopVerifying()
	err := pool.QueryRow(verifyCtx,
		`SELECT failure_count, last_failure, embedded_at FROM event_chunks WHERE event_id = 1`,
	).Scan(&failureCount, &lastFailure, &embeddedAt)
	if err != nil {
		t.Fatal(err)
	}
	if failureCount != 0 {
		t.Errorf("failure_count should stay 0 on transient retry; got %d", failureCount)
	}
	if lastFailure != nil {
		t.Errorf("last_failure should stay NULL on transient retry; got %q", *lastFailure)
	}
	if embeddedAt == nil {
		t.Error("chunk should have eventually embedded")
	}
	if emb.calls < 4 {
		// 3 transient failures + at least 1 success
		t.Errorf("expected >=4 Embed calls (3 retries + 1 success), got %d", emb.calls)
	}
}

// embedderWorkerConfigForTest returns a WorkerConfig with tight
// backoffs so the TransientError retry test converges fast.
func embedderWorkerConfigForTest() WorkerConfig {
	return WorkerConfig{
		Concurrency: 1,
		BatchSize:   5,
		MinBackoff:  50 * time.Millisecond,
		MaxBackoff:  200 * time.Millisecond,
	}
}

func TestWorkerDimMismatchFailsFast(t *testing.T) {
	// No DB / qdrant needed — Run should fail pre-loop when the
	// embedder's dim doesn't match the collection's configured size.
	w := &Worker{
		Embedder: &fakeEmbedder{dim: 512, id: "test:wrongdim:v1"}, // 512 != 1024
		Store:    &fakePointStore{},
	}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatal("expected dim mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "dim") {
		t.Errorf("error should mention dim: %v", err)
	}
}

// chunkIDForExternal returns the (single) chunk id for an event
// looked up by (source, external_id). Fixtures ingesting a one-body
// event get exactly one chunk.
func chunkIDForExternal(t *testing.T, pool *pgxpool.Pool, source, externalID string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `
		SELECT c.id
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE e.source = $1 AND e.external_id = $2
	`, source, externalID).Scan(&id); err != nil {
		t.Fatalf("chunk id for %s:%s: %v", source, externalID, err)
	}
	return id
}

// TestWorkerClaim_RecencyFirst is the regression test for the
// embedder claim queue: it must claim the most recently-occurred
// event work first, NOT the earliest-inserted (smallest-id) row.
//
// The old code claimed `ORDER BY c.id ASC` with a monotonic id
// cursor, so historically-inserted backlog (small id) always came
// first and a live message inserted "now" (largest id) starved
// behind it. This fixture seeds old work FIRST / smaller id and
// recent work SECOND / larger id, then asserts fetchBatch selects
// the RECENT row first — a negative control against the old
// id-ordered FIFO.
func TestWorkerClaim_RecencyFirst(t *testing.T) {
	pool, _ := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// Old work first: smaller id, older source recency.
	if _, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopePublic, Source: "slack", ExternalID: "C1:old",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Title: "#ops", Body: "historical backfill message",
	}); err != nil {
		t.Fatal(err)
	}
	oldID := chunkIDForExternal(t, pool, "slack", "C1:old")

	// Recent work second: larger id, newer source recency.
	if _, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopePublic, Source: "slack", ExternalID: "C1:recent",
		OccurredAt: time.Unix(1700000100, 0), Actor: "bob", ActorID: "U2",
		Title: "#ops", Body: "live interactive message",
	}); err != nil {
		t.Fatal(err)
	}
	recentID := chunkIDForExternal(t, pool, "slack", "C1:recent")
	if recentID <= oldID {
		t.Fatalf("fixture broken: recent id %d should be > old id %d", recentID, oldID)
	}

	w := &Worker{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	batch, err := w.fetchBatch(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) < 2 {
		t.Fatalf("expected >=2 pending chunks, got %d", len(batch))
	}
	if batch[0].id != recentID {
		t.Errorf("recency-first: claimed id %d first, want recent %d (old id-ordered FIFO would claim %d first)",
			batch[0].id, recentID, oldID)
	}
}

// TestWorkerClaim_RecencyTiebreakDesc asserts the deterministic
// tiebreaker: two chunks with the SAME source occurred_at resolve by
// id DESC (the more recently inserted one wins). This only passes
// once ordering is `occurred_at DESC, id DESC`.
func TestWorkerClaim_RecencyTiebreakDesc(t *testing.T) {
	pool, _ := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	sameInstant := time.Unix(1700000000, 0)
	for i, ext := range []string{"C1:o1", "C1:o2"} {
		if _, err := sink.Upsert(ctx, events.Event{
			Scope: events.ScopePublic, Source: "slack", ExternalID: ext,
			OccurredAt: sameInstant, Actor: "alice", ActorID: "U1",
			Title: "#ops", Body: fmt.Sprintf("same instant %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := chunkIDForExternal(t, pool, "slack", "C1:o1")
	second := chunkIDForExternal(t, pool, "slack", "C1:o2")
	if second <= first {
		t.Fatalf("fixture broken: second id %d should be > first id %d", second, first)
	}

	w := &Worker{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	batch, err := w.fetchBatch(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) < 2 {
		t.Fatalf("expected >=2 pending chunks, got %d", len(batch))
	}
	if batch[0].id != second {
		t.Errorf("tiebreaker id DESC: claimed id %d first, want %d", batch[0].id, second)
	}
}

// TestChunkOccurredAt_SourceRecencyAcrossReprocessing proves the
// denormalized event_chunks.occurred_at is populated from the parent
// event's SOURCE recency at every chunk (re)creation, and that
// reprocessing (a re-ingest edit that deletes+recreates the text
// chunks) does NOT rewrite it to a processing timestamp.
func TestChunkOccurredAt_SourceRecencyAcrossReprocessing(t *testing.T) {
	pool, _ := setupWorld(t)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	sourceRecency := time.Unix(1700000000, 0).UTC()
	if _, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopePublic, Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: sourceRecency, Body: "v1 text",
	}); err != nil {
		t.Fatal(err)
	}
	var firstOccurred time.Time
	if err := pool.QueryRow(ctx, `
		SELECT occurred_at FROM event_chunks
		  WHERE event_id = (SELECT id FROM events WHERE external_id = 'C1:1.1')
	`).Scan(&firstOccurred); err != nil {
		t.Fatalf("first chunk occurred_at: %v", err)
	}
	if !firstOccurred.Equal(sourceRecency) {
		t.Errorf("first chunk occurred_at: got %v, want source recency %v", firstOccurred, sourceRecency)
	}

	// Reprocess: re-ingest the same logical event with a new body but
	// the SAME source occurred_at (Slack edits retain the message ts).
	// The old text chunk is deleted and a fresh one inserted; that
	// fresh chunk must still carry the SOURCE recency, not now().
	if res, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopePublic, Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: sourceRecency, Body: "v2 text with more words",
	}); err != nil {
		t.Fatal(err)
	} else if !res.WasUpdate {
		t.Fatal("second Upsert should be an update (reprocessing)")
	}

	var reprocessed time.Time
	if err := pool.QueryRow(ctx, `
		SELECT occurred_at FROM event_chunks
		  WHERE event_id = (SELECT id FROM events WHERE external_id = 'C1:1.1')
	`).Scan(&reprocessed); err != nil {
		t.Fatalf("reprocessed chunk occurred_at: %v", err)
	}
	if !reprocessed.Equal(sourceRecency) {
		t.Errorf("reprocessed chunk occurred_at: got %v, want source recency %v (must not be a processing timestamp)", reprocessed, sourceRecency)
	}
}

// testVectorDim is the collection size these tests run with.
const testVectorDim = 1024

type fakePointStore struct {
	upsert func(context.Context, []qdrantstore.Point) error
}

func (f *fakePointStore) Dimensions() int { return testVectorDim }

// testCollection is the collection the fake store stands for.
const testCollection = "test_collection"

func (f *fakePointStore) Collection() string { return testCollection }

func (f *fakePointStore) Upsert(ctx context.Context, points []qdrantstore.Point) error {
	if f.upsert != nil {
		return f.upsert(ctx, points)
	}
	return nil
}

func setupWritebackDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE qdrant_delete_queue`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedWritebackEvent(t *testing.T, pool *pgxpool.Pool, externalID, scope string) int64 {
	t.Helper()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	result, err := sink.Upsert(context.Background(), events.Event{
		Source: "slack", ExternalID: externalID, OccurredAt: time.Now(),
		Body: "writeback race " + externalID, Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.EventID
}

func writebackRows(t *testing.T, pool *pgxpool.Pool) []chunkRow {
	t.Helper()
	w := &Worker{Pool: pool}
	rows, err := w.fetchBatch(context.Background(), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func vectorsFor(rows []chunkRow) [][]float32 {
	vectors := make([][]float32, len(rows))
	for i := range vectors {
		vectors[i] = make([]float32, testVectorDim)
		vectors[i][0] = 1
	}
	return vectors
}

func TestWriteBackPerIDScopeAcknowledgement(t *testing.T) {
	pool := setupWritebackDB(t)
	changedEvent := seedWritebackEvent(t, pool, "changed", events.ScopePublic)
	seedWritebackEvent(t, pool, "stable", events.ScopeChannel("CSTABLE"))
	rows := writebackRows(t, pool)
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE events SET scope='slack-channel:CCHANGED' WHERE id=$1
	`, changedEvent); err != nil {
		t.Fatal(err)
	}
	var written []qdrantstore.Point
	w := &Worker{
		Pool: pool, Embedder: &fakeEmbedder{id: "test", dim: testVectorDim},
		Store: &fakePointStore{upsert: func(_ context.Context, points []qdrantstore.Point) error {
			written = append(written, points...)
			return nil
		}},
	}
	if err := w.writeBack(context.Background(), rows, vectorsFor(rows)); err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("written=%d", len(written))
	}
	var changedPending, stablePending, bothEmbedded bool
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT c.scope_sync_needed FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id='changed'),
		  (SELECT c.scope_sync_needed FROM event_chunks c JOIN events e ON e.id=c.event_id WHERE e.external_id='stable'),
		  (SELECT bool_and(c.embedded_at IS NOT NULL) FROM event_chunks c)
	`).Scan(&changedPending, &stablePending, &bothEmbedded); err != nil {
		t.Fatal(err)
	}
	if !changedPending || stablePending || !bothEmbedded {
		t.Fatalf("changed_pending=%v stable_pending=%v embedded=%v", changedPending, stablePending, bothEmbedded)
	}
}

func TestWriteBackRecencyChangedDuringInferenceRemainsPending(t *testing.T) {
	pool := setupWritebackDB(t)
	eventID := seedWritebackEvent(t, pool, "recency-race", events.ScopePublic)
	rows := writebackRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	newTime := rows[0].occurredAt.Add(time.Hour)
	if _, err := pool.Exec(context.Background(), `UPDATE events SET occurred_at=$2 WHERE id=$1`, eventID, newTime); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET occurred_at=$2,scope_sync_needed=true WHERE event_id=$1`, eventID, newTime); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: pool, Embedder: &fakeEmbedder{id: "test", dim: testVectorDim}, Store: &fakePointStore{}}
	if err := w.writeBack(context.Background(), rows, vectorsFor(rows)); err != nil {
		t.Fatal(err)
	}
	var pending, embedded bool
	if err := pool.QueryRow(context.Background(), `SELECT scope_sync_needed,embedded_at IS NOT NULL FROM event_chunks WHERE id=$1`, rows[0].id).Scan(&pending, &embedded); err != nil {
		t.Fatal(err)
	}
	if !pending || !embedded {
		t.Errorf("in-flight embedding lost recency correction: pending=%v embedded=%v", pending, embedded)
	}
}

func TestWriteBackSkipsChunkDeletedBeforeLock(t *testing.T) {
	pool := setupWritebackDB(t)
	seedWritebackEvent(t, pool, "deleted", events.ScopePublic)
	rows := writebackRows(t, pool)
	if _, err := pool.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, rows[0].id); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := &Worker{
		Pool: pool, Embedder: &fakeEmbedder{id: "test", dim: testVectorDim},
		Store: &fakePointStore{upsert: func(context.Context, []qdrantstore.Point) error {
			calls++
			return nil
		}},
	}
	if err := w.writeBack(context.Background(), rows, vectorsFor(rows)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("upsert calls=%d", calls)
	}
	var queued bool
	_ = pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM qdrant_delete_queue WHERE point_id=$1)`, rows[0].id).Scan(&queued)
	if !queued {
		t.Fatal("delete was not durably queued")
	}
}

func TestWriteBackLockOrdersDeleteAfterUpsert(t *testing.T) {
	pool := setupWritebackDB(t)
	seedWritebackEvent(t, pool, "locked", events.ScopePublic)
	rows := writebackRows(t, pool)
	entered := make(chan struct{})
	release := make(chan struct{})
	w := &Worker{
		Pool: pool, Embedder: &fakeEmbedder{id: "test", dim: testVectorDim},
		Store: &fakePointStore{upsert: func(context.Context, []qdrantstore.Point) error {
			close(entered)
			<-release
			return nil
		}},
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- w.writeBack(context.Background(), rows, vectorsFor(rows)) }()
	<-entered
	deleteDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, rows[0].id)
		deleteDone <- err
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("delete did not wait for writeback lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	var chunks, queued int
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM event_chunks WHERE id=$1),
		       (SELECT count(*) FROM qdrant_delete_queue WHERE point_id=$1)
	`, rows[0].id).Scan(&chunks, &queued); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 || queued != 1 {
		t.Fatalf("chunks=%d queued=%d", chunks, queued)
	}
}

func TestWriteBackTimeoutRollsBackAndReleasesLock(t *testing.T) {
	pool := setupWritebackDB(t)
	seedWritebackEvent(t, pool, "timeout", events.ScopePublic)
	rows := writebackRows(t, pool)
	previous := qdrantWriteTimeout
	qdrantWriteTimeout = 20 * time.Millisecond
	t.Cleanup(func() { qdrantWriteTimeout = previous })
	w := &Worker{
		Pool: pool, Embedder: &fakeEmbedder{id: "test", dim: testVectorDim},
		Store: &fakePointStore{upsert: func(ctx context.Context, _ []qdrantstore.Point) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	}
	if err := w.writeBack(context.Background(), rows, vectorsFor(rows)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	var embedded bool
	_ = pool.QueryRow(context.Background(), `SELECT embedded_at IS NOT NULL FROM event_chunks WHERE id=$1`, rows[0].id).Scan(&embedded)
	if embedded {
		t.Fatal("timed out upsert was acknowledged")
	}
	deleteCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := pool.Exec(deleteCtx, `DELETE FROM event_chunks WHERE id=$1`, rows[0].id); err != nil {
		t.Fatalf("delete remained blocked after timeout: %v", err)
	}
}
