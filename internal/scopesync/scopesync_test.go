package scopesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/testdb"
)

type fakeStore struct {
	occurredCalls  [][]uint64
	occurredValues []time.Time
	setOccurredAt  func([]uint64, time.Time) error
	scopeCalls     [][]uint64
	scopeValues    []string
	setScope       func([]uint64, string) error
	deleteCalls    [][]uint64
	delete         func([]uint64) error
	auditIDs       []uint64
	auditScopes    []string
	auditHook      func()
}

func (f *fakeStore) SetOccurredAt(_ context.Context, ids []uint64, occurredAt time.Time) error {
	f.occurredCalls = append(f.occurredCalls, append([]uint64(nil), ids...))
	f.occurredValues = append(f.occurredValues, occurredAt)
	if f.setOccurredAt != nil {
		return f.setOccurredAt(ids, occurredAt)
	}
	return nil
}

func (f *fakeStore) SetScope(_ context.Context, ids []uint64, scope string) error {
	f.scopeCalls = append(f.scopeCalls, append([]uint64(nil), ids...))
	f.scopeValues = append(f.scopeValues, scope)
	if f.setScope != nil {
		return f.setScope(ids, scope)
	}
	return nil
}

func (f *fakeStore) DeleteIDs(_ context.Context, ids []uint64) error {
	f.deleteCalls = append(f.deleteCalls, append([]uint64(nil), ids...))
	if f.delete != nil {
		return f.delete(ids)
	}
	return nil
}

func (f *fakeStore) ScrollScopes(_ context.Context, pageSize uint32, visit func([]uint64, []string) error) error {
	if f.auditHook != nil {
		f.auditHook()
	}
	for start := 0; start < len(f.auditIDs); start += int(pageSize) {
		end := min(start+int(pageSize), len(f.auditIDs))
		if err := visit(f.auditIDs[start:end], f.auditScopes[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func setupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "scopesync"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, table := range []string{"events", "slack_channel_info", "qdrant_delete_queue"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`TRUNCATE %s RESTART IDENTITY CASCADE`, table)); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool
}

func seed(t *testing.T, pool *pgxpool.Pool, channel string, private bool, scope string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	res, err := sink.Upsert(ctx, events.Event{
		Source: "slack", ExternalID: channel + ":1", OccurredAt: time.Now(),
		Body: "message", Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, occurred_at)
		VALUES ($1, $2, now())
	`, res.EventID, channel); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
		VALUES ($1, $2, $3, false, now())
	`, channel, channel, private); err != nil {
		t.Fatal(err)
	}
	var chunkID int64
	if err := pool.QueryRow(ctx, `
		UPDATE event_chunks SET embedded_at = now(), embedder_id = 'test'
		 WHERE event_id = $1 RETURNING id
	`, res.EventID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	return res.EventID, chunkID
}

func service(pool *pgxpool.Pool, store Store) *Service {
	return &Service{Pool: pool, Store: store, Logger: slog.Default(), Metrics: NewMetrics(nil)}
}

func TestDrainScopeRefreshesRecencyWithoutReembedding(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CTIME", false, events.ScopePublic)
	wantTime := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET occurred_at=$2,scope_sync_needed=true WHERE id=$1`, chunkID, wantTime); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 || len(fake.occurredCalls) != 1 || len(fake.occurredCalls[0]) != 1 || fake.occurredCalls[0][0] != uint64(chunkID) || !fake.occurredValues[0].Equal(wantTime) {
		t.Errorf("recency payload was not patched: applied=%d ids=%v times=%v", applied, fake.occurredCalls, fake.occurredValues)
	}
	var embedded bool
	if err := pool.QueryRow(context.Background(), `SELECT embedded_at IS NOT NULL FROM event_chunks WHERE id=$1`, chunkID).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if !embedded || len(fake.deleteCalls) != 0 {
		t.Errorf("recency refresh invalidated existing embedding")
	}
}

func TestDrainScopeRecencyChangeDuringWriteRemainsPending(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CTIMERACE", false, events.ScopePublic)
	firstTime := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET occurred_at=$2,scope_sync_needed=true WHERE id=$1`, chunkID, firstTime); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{setOccurredAt: func(_ []uint64, _ time.Time) error {
		_, err := pool.Exec(context.Background(), `UPDATE event_chunks SET occurred_at=$2,scope_sync_needed=true WHERE id=$1`, chunkID, firstTime.Add(time.Hour))
		return err
	}}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := pool.QueryRow(context.Background(), `SELECT scope_sync_needed FROM event_chunks WHERE id=$1`, chunkID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if applied != 0 || !pending {
		t.Errorf("stale payload write cleared newer recency: applied=%d pending=%v", applied, pending)
	}
}

func TestDrainScopeRecencyFailureRetainsPendingWork(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CTIMEFAIL", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{setOccurredAt: func(_ []uint64, _ time.Time) error { return errors.New("payload failure") }}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err == nil || applied != 0 {
		t.Errorf("failed recency write was acknowledged: applied=%d err=%v", applied, err)
	}
	var pending bool
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT scope_sync_needed,scope_sync_attempts FROM event_chunks WHERE id=$1`, chunkID).Scan(&pending, &attempts); err != nil {
		t.Fatal(err)
	}
	if !pending || attempts != 1 {
		t.Errorf("failed recency write lost retry state: pending=%v attempts=%d", pending, attempts)
	}
}

func TestDrainScopeRecencyUpdatesRealQdrantWithoutReplacingVector(t *testing.T) {
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set")
	}
	pool := setupDB(t)
	ctx := context.Background()
	eventID, chunkID := seed(t, pool, "CQDRANTTIME", false, events.ScopePublic)
	store, err := qdrantstore.New(qdrantstore.Config{Addr: addr, Collection: fmt.Sprintf("recency_test_%d", time.Now().UnixNano()), Dimensions: 1024}, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(func() { _ = store.Drop(context.Background()) })
	if err := store.EnsureCollection(ctx); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, 1024)
	vector[0] = 1
	firstTime := time.Date(2026, 8, 3, 12, 0, 0, 123456000, time.UTC)
	if err := store.Upsert(ctx, []qdrantstore.Point{{ID: uint64(chunkID), EventID: eventID, Vector: vector, Source: "slack", Scope: events.ScopePublic, OccurredAt: firstTime}}); err != nil {
		t.Fatal(err)
	}
	assertTime := func(t *testing.T, want time.Time) {
		t.Helper()
		hits, err := store.Search(ctx, vector, qdrantstore.SearchFilters{AllowedScopes: []string{events.ScopePublic}, OccurredAfter: want.Add(-time.Microsecond), OccurredBefore: want.Add(time.Microsecond)}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ChunkID != uint64(chunkID) || hits[0].Score < 0.99 {
			t.Errorf("timestamp %s did not match unchanged vector %d: hits=%+v", want, chunkID, hits)
		}
	}
	t.Run("initial_timestamp_precision", func(t *testing.T) { assertTime(t, firstTime) })
	newTime := firstTime.Add(time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE event_chunks SET occurred_at=$2,scope_sync_needed=true WHERE id=$1`, chunkID, newTime); err != nil {
		t.Fatal(err)
	}
	if applied, err := service(pool, store).DrainScope(ctx); err != nil || applied != 1 {
		t.Fatalf("drain applied=%d err=%v", applied, err)
	}
	t.Run("corrected_timestamp", func(t *testing.T) { assertTime(t, newTime) })
	if count, err := store.CountPoints(ctx); err != nil || count != 1 {
		t.Errorf("payload correction changed point count: count=%d err=%v", count, err)
	}
}

func TestReconcileQueuesOnlyChangedChunks(t *testing.T) {
	pool := setupDB(t)
	changedEvent, changedChunk := seed(t, pool, "CPRIV", true, events.ScopePublic)
	_, stableChunk := seed(t, pool, "CPUB", false, events.ScopeChannel("CPUB"))
	dmEvent, dmChunk := seed(t, pool, "DUSER", false, events.ScopeOwner("owner@example.com"))
	fake := &fakeStore{}
	s := service(pool, fake)

	result, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Chunks != 1 || result.Pending != 1 {
		t.Fatalf("result = %+v, want 1 changed event/chunk/pending", result)
	}
	if len(fake.scopeCalls) != 0 {
		t.Fatalf("reconcile made %d Qdrant calls", len(fake.scopeCalls))
	}
	var scope string
	if err := pool.QueryRow(context.Background(), `SELECT scope FROM events WHERE id=$1`, changedEvent).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeChannel("CPRIV") {
		t.Fatalf("scope = %q", scope)
	}
	var changed, stable, dmPending bool
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT scope_sync_needed FROM event_chunks WHERE id=$1),
		       (SELECT scope_sync_needed FROM event_chunks WHERE id=$2),
		       (SELECT scope_sync_needed FROM event_chunks WHERE id=$3)
	`, changedChunk, stableChunk, dmChunk).Scan(&changed, &stable, &dmPending); err != nil {
		t.Fatal(err)
	}
	if !changed || stable || dmPending {
		t.Fatalf("dirty flags changed=%v stable=%v dm=%v", changed, stable, dmPending)
	}
	if err := pool.QueryRow(context.Background(), `SELECT scope FROM events WHERE id=$1`, dmEvent).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeOwner("owner@example.com") {
		t.Fatalf("DM scope = %q", scope)
	}

	result, err = s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || result.Chunks != 0 {
		t.Fatalf("no-op result = %+v", result)
	}
}

func TestReconcileSkipsNegativeCache(t *testing.T) {
	pool := setupDB(t)
	eventID, _ := seed(t, pool, "CGONE", false, events.ScopeChannel("CGONE"))
	if _, err := pool.Exec(context.Background(), `UPDATE slack_channel_info SET name='' WHERE channel_id='CGONE'`); err != nil {
		t.Fatal(err)
	}
	result, err := service(pool, &fakeStore{}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 {
		t.Fatalf("changed %d events", result.Events)
	}
	var scope string
	_ = pool.QueryRow(context.Background(), `SELECT scope FROM events WHERE id=$1`, eventID).Scan(&scope)
	if scope != events.ScopeChannel("CGONE") {
		t.Fatalf("scope = %q", scope)
	}
}

func TestDrainScopeSuccessAndRacingChange(t *testing.T) {
	pool := setupDB(t)
	eventID, chunkID := seed(t, pool, "CRACE", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{}
	fake.setScope = func(_ []uint64, _ string) error {
		_, err := pool.Exec(context.Background(), `UPDATE events SET scope='slack-channel:CRACE' WHERE id=$1`, eventID)
		return err
	}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 || len(fake.scopeCalls) != 1 {
		t.Fatalf("applied=%d calls=%d", applied, len(fake.scopeCalls))
	}
	var pending bool
	_ = pool.QueryRow(context.Background(), `SELECT scope_sync_needed FROM event_chunks WHERE id=$1`, chunkID).Scan(&pending)
	if !pending {
		t.Fatal("racing scope change was incorrectly acknowledged")
	}

	fake.setScope = nil
	applied, err = service(pool, fake).DrainScope(context.Background())
	if err != nil || applied != 1 {
		t.Fatalf("second drain applied=%d err=%v", applied, err)
	}
}

func TestDrainScopeFailureRetainsWholePassWithoutFallback(t *testing.T) {
	pool := setupDB(t)
	_, id1 := seed(t, pool, "C1", false, events.ScopePublic)
	_, id2 := seed(t, pool, "C2", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true WHERE id=ANY($1)`, []int64{id1, id2}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{setScope: func([]uint64, string) error { return errors.New("qdrant unavailable") }}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err == nil || applied != 0 {
		t.Fatalf("applied=%d err=%v", applied, err)
	}
	if len(fake.scopeCalls) != 1 || len(fake.scopeCalls[0]) != 2 {
		t.Fatalf("calls = %#v, want one two-point call", fake.scopeCalls)
	}
	var pending, attempted int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE scope_sync_needed),
		       count(*) FILTER (WHERE scope_sync_attempts=1 AND scope_sync_next_attempt_at > now())
		  FROM event_chunks WHERE id=ANY($1)
	`, []int64{id1, id2}).Scan(&pending, &attempted); err != nil {
		t.Fatal(err)
	}
	if pending != 2 || attempted != 2 {
		t.Fatalf("pending=%d attempted=%d", pending, attempted)
	}
}

func TestDrainScopeBoundedToOneThousand(t *testing.T) {
	pool := setupDB(t)
	eventID, _ := seed(t, pool, "CBIG", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at, embedded_at, embedder_id, scope_sync_needed)
		SELECT $1, 'text', g, 'extra', now(), now(), 'test', true
		  FROM generate_series(1, 1000) g
	`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{}
	applied, err := service(pool, fake).DrainScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if applied != batchSize || len(fake.scopeCalls) != 1 || len(fake.scopeCalls[0]) != batchSize {
		t.Fatalf("applied=%d calls=%d batch=%d", applied, len(fake.scopeCalls), len(fake.scopeCalls[0]))
	}
	var pending int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM event_chunks WHERE scope_sync_needed`).Scan(&pending)
	if pending != 1 {
		t.Fatalf("pending=%d, want 1", pending)
	}
}

func TestDeleteTriggerAndWorker(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CDELETE", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	var queued int64
	if err := pool.QueryRow(context.Background(), `SELECT point_id FROM qdrant_delete_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != chunkID {
		t.Fatalf("queued=%d want %d", queued, chunkID)
	}
	fake := &fakeStore{}
	applied, err := service(pool, fake).DrainDeletes(context.Background())
	if err != nil || applied != 1 {
		t.Fatalf("applied=%d err=%v", applied, err)
	}
	var remaining int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM qdrant_delete_queue`).Scan(&remaining)
	if remaining != 0 || len(fake.deleteCalls) != 1 {
		t.Fatalf("remaining=%d calls=%d", remaining, len(fake.deleteCalls))
	}
}

func TestDeleteFailureRetriesDurably(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CFAIL", false, events.ScopePublic)
	_, _ = pool.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, chunkID)
	fake := &fakeStore{delete: func([]uint64) error { return errors.New("down") }}
	if _, err := service(pool, fake).DrainDeletes(context.Background()); err == nil {
		t.Fatal("expected delete failure")
	}
	var attempts int
	var future bool
	if err := pool.QueryRow(context.Background(), `SELECT attempts, next_attempt_at > now() FROM qdrant_delete_queue WHERE point_id=$1`, chunkID).Scan(&attempts, &future); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !future {
		t.Fatalf("attempts=%d future=%v", attempts, future)
	}
}

func TestDeleteTriggerRollsBackWithTransaction(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CROLLBACK", false, events.ScopePublic)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var chunks, queued int
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM event_chunks WHERE id=$1),
		       (SELECT count(*) FROM qdrant_delete_queue WHERE point_id=$1)
	`, chunkID).Scan(&chunks, &queued); err != nil {
		t.Fatal(err)
	}
	if chunks != 1 || queued != 0 {
		t.Fatalf("chunks=%d queued=%d", chunks, queued)
	}
}

func TestDeleteTriggerRunsForCascade(t *testing.T) {
	pool := setupDB(t)
	eventID, chunkID := seed(t, pool, "CCASCADE", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `DELETE FROM events WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	var queued bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM qdrant_delete_queue WHERE point_id=$1)`, chunkID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if !queued {
		t.Fatal("cascading chunk delete was not queued")
	}
}

func TestDeleteQueueInvisibleBeforeCommit(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CUNCOMMITTED", false, events.ScopePublic)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `DELETE FROM event_chunks WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM qdrant_delete_queue WHERE point_id=$1)`, chunkID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("queue row was visible before delete transaction committed")
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM qdrant_delete_queue WHERE point_id=$1)`, chunkID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if !visible {
		t.Fatal("queue row missing after commit")
	}
}

func TestDrainDeletesBoundedToOneThousand(t *testing.T) {
	pool := setupDB(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO qdrant_delete_queue (point_id)
		SELECT g FROM generate_series(1, 1001) g
	`); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{}
	applied, err := service(pool, fake).DrainDeletes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if applied != batchSize || len(fake.deleteCalls) != 1 || len(fake.deleteCalls[0]) != batchSize {
		t.Fatalf("applied=%d calls=%d batch=%d", applied, len(fake.deleteCalls), len(fake.deleteCalls[0]))
	}
	var remaining int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM qdrant_delete_queue`).Scan(&remaining)
	if remaining != 1 {
		t.Fatalf("remaining=%d", remaining)
	}
}

func TestAuditQueuesAllRepairTypes(t *testing.T) {
	pool := setupDB(t)
	_, mismatchID := seed(t, pool, "CMISMATCH", false, events.ScopePublic)
	_, missingID := seed(t, pool, "CMISSING", false, events.ScopePublic)
	const orphanID = uint64(999999)
	fake := &fakeStore{
		auditIDs:    []uint64{uint64(mismatchID), orphanID},
		auditScopes: []string{events.ScopeChannel("WRONG"), events.ScopePublic},
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE event_chunks SET queued_at = now() - interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	result, err := service(pool, fake).Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ScopeMismatches != 1 || result.OrphanPoints != 1 || result.MissingPoints != 1 {
		t.Fatalf("result = %+v", result)
	}
	// The chunk put back to be embedded waits from now; the one whose
	// vector only needs its scope patched was not queued again.
	var missingQueuedNow, mismatchQueuedNow bool
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT queued_at > now() - interval '1 minute' FROM event_chunks WHERE id=$1),
		       (SELECT queued_at > now() - interval '1 minute' FROM event_chunks WHERE id=$2)
	`, missingID, mismatchID).Scan(&missingQueuedNow, &mismatchQueuedNow); err != nil {
		t.Fatal(err)
	}
	if !missingQueuedNow || mismatchQueuedNow {
		t.Fatalf("queued just now: missing=%v mismatch=%v", missingQueuedNow, mismatchQueuedNow)
	}
	var mismatchPending bool
	var missingEmbedded bool
	var orphanQueued bool
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT scope_sync_needed FROM event_chunks WHERE id=$1),
		       (SELECT embedded_at IS NOT NULL FROM event_chunks WHERE id=$2),
		       EXISTS(SELECT 1 FROM qdrant_delete_queue WHERE point_id=$3)
	`, mismatchID, missingID, orphanID).Scan(&mismatchPending, &missingEmbedded, &orphanQueued); err != nil {
		t.Fatal(err)
	}
	if !mismatchPending || missingEmbedded || !orphanQueued {
		t.Fatalf("mismatch=%v missing_embedded=%v orphan=%v", mismatchPending, missingEmbedded, orphanQueued)
	}
	if len(fake.scopeCalls) != 0 || len(fake.deleteCalls) != 0 {
		t.Fatal("audit mutated Qdrant")
	}
}

func TestAuditDoesNotRequeueConcurrentlyEmbeddedChunk(t *testing.T) {
	pool := setupDB(t)
	_, chunkID := seed(t, pool, "CCONCURRENT", false, events.ScopePublic)
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET embedded_at=NULL, embedder_id=NULL WHERE id=$1`, chunkID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{auditHook: func() {
		if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET embedded_at=clock_timestamp(), embedder_id='test' WHERE id=$1`, chunkID); err != nil {
			t.Error(err)
		}
	}}
	result, err := service(pool, fake).Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.MissingPoints != 0 {
		t.Fatalf("missing=%d", result.MissingPoints)
	}
	var embedded bool
	if err := pool.QueryRow(context.Background(), `SELECT embedded_at IS NOT NULL FROM event_chunks WHERE id=$1`, chunkID).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if !embedded {
		t.Fatal("audit requeued a chunk embedded after its cutoff")
	}
}
