package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/bluesky-social/diderot/internal/worker"
)

// vectorHarness is a database with three chunks an earlier version embedded
// (they record no collection), and the collection their vectors are in.
type vectorHarness struct {
	pool       *pgxpool.Pool
	vectors    *vectorStores
	cfg        *config.Config
	collection string
	embeddedAt time.Time
}

func newVectorHarness(t *testing.T) *vectorHarness {
	t.Helper()
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_vectors"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE; DELETE FROM connector_settings WHERE connector = 'models'`)
	require.NoError(t, err)
	h := &vectorHarness{pool: pool, cfg: &config.Config{QdrantAddr: addr}, embeddedAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}
	_, err = pool.Exec(ctx, `
		WITH e AS (
		  INSERT INTO events (source, external_id, occurred_at, actor, body, raw)
		  VALUES ('slack', 'adopt-1', now(), 'u', 'body', '{}') RETURNING id)
		INSERT INTO event_chunks (event_id, chunk_idx, content, occurred_at, embedded_at, embedder_id)
		SELECT id, i, 'body', now(), $1, 'self:local-embed:v1' FROM e, generate_series(0, 2) i`, h.embeddedAt)
	require.NoError(t, err)
	h.vectors = newVectorStores(h.cfg, testLogger())
	t.Cleanup(h.vectors.close)
	h.collection = fmt.Sprintf("diderot_test_adopt_%d", time.Now().UnixNano())
	existing, err := h.vectors.open(ctx, settings.Embedding{Collection: h.collection, Dimensions: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = existing.Drop(context.Background()) })
	return h
}

// waiting counts the chunks the embedder would embed.
func (h *vectorHarness) waiting(t *testing.T) (count int) {
	t.Helper()
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT count(*) FROM event_chunks WHERE embedded_at IS NULL`).Scan(&count))
	return count
}

// TestAdoptingTheCollectionEmbedsNothingAgain is the upgrade's guarantee: a
// deployment whose embedding choice adopts the collection its vectors are
// already in opens that collection as it is, and no chunk loses its vector,
// at the first leader term or any later one.
func TestAdoptingTheCollectionEmbedsNothingAgain(t *testing.T) {
	h := newVectorHarness(t)
	ctx := context.Background()
	catalog := newModelCatalog(h.cfg, h.vectors)
	adopted := settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 4, Collection: h.collection}
	require.NoError(t, catalog.PrepareCollection(ctx, adopted, true))
	err := catalog.PrepareCollection(ctx, settings.Embedding{Collection: h.collection + "_missing", Dimensions: 4}, true)
	require.ErrorIs(t, err, settings.ErrInvalid, "adopting a collection that does not exist creates nothing")
	err = catalog.PrepareCollection(ctx, settings.Embedding{Collection: h.collection, Dimensions: 8}, true)
	require.ErrorIs(t, err, settings.ErrInvalid, "an adopted collection must hold vectors of the model's dimension")

	work := leaderWork{pool: h.pool, vectors: h.vectors}
	for range 2 {
		store := openTermVectors(ctx, work, adopted, false, testLogger())
		require.NotNil(t, store)
		assert.Equal(t, h.collection, store.Collection())
		assert.Zero(t, h.waiting(t), "nothing is embedded again")
		count, err := store.CountPoints(ctx)
		require.NoError(t, err)
		assert.Zero(t, count, "and nothing is written to the collection")
	}
	var stamped int
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT count(*) FROM event_chunks WHERE embedded_at = $1 AND embedder_id = 'self:local-embed:v1'`, h.embeddedAt).Scan(&stamped))
	assert.Equal(t, 3, stamped, "every chunk keeps the stamp it had")
}

// TestFillingMarksWhatTheNewCollectionLacks: the chunks a collection being
// filled still lacks are found by the collection each chunk records, not by
// the model that embedded it, so the same model in a new collection is
// filled too; and a chunk an earlier leader embedded elsewhere meanwhile is
// caught by the next term.
func TestFillingMarksWhatTheNewCollectionLacks(t *testing.T) {
	h := newVectorHarness(t)
	ctx := context.Background()
	work := leaderWork{pool: h.pool, vectors: h.vectors}
	// The same model, in a collection of its own.
	filling := settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 4, Collection: h.collection + "_new"}
	store := openTermVectors(ctx, work, filling, true, testLogger())
	require.NotNil(t, store)
	t.Cleanup(func() { _ = store.Drop(context.Background()) })
	assert.Equal(t, 3, h.waiting(t), "every chunk is embedded into the collection, though the same model embedded it before")

	// The embedder writes them, recording the collection.
	_, err := h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_at = now(), embedder_id = 'self:local-embed:v1', embedded_collection = $1`, filling.Collection)
	require.NoError(t, err)
	require.NotNil(t, openTermVectors(ctx, work, filling, true, testLogger()))
	assert.Zero(t, h.waiting(t), "a later term leaves what is already in the collection")

	// A leader that had not seen the change embedded one chunk into the old
	// collection: the next term marks it again.
	_, err = h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_collection = $1 WHERE chunk_idx = 0`, h.collection)
	require.NoError(t, err)
	require.NotNil(t, openTermVectors(ctx, work, filling, true, testLogger()))
	assert.Equal(t, 1, h.waiting(t))
}

// TestSearchMovesToAFilledCollectionOnly: the model being filled becomes the
// embedding model once its collection holds every chunk, and not before.
func TestSearchMovesToAFilledCollectionOnly(t *testing.T) {
	h := newVectorHarness(t)
	ctx := context.Background()
	store := settings.NewStore(h.pool, testLogger())
	require.NoError(t, store.Load(ctx))
	live := settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 4, Collection: h.collection}
	filling := settings.Embedding{Provider: "gateway", Model: "other", Dimensions: 4, Collection: h.collection + "_other"}
	_, err := store.UpdateModels(ctx, store.Models().Version, settings.Actor{Email: "op@example.com"}, "test", func(models *settings.Models) error {
		models.Embedding, models.Filling = &live, &filling
		return nil
	})
	require.NoError(t, err)
	finish := finishFilling(store, h.pool, filling, testLogger()).Run

	require.NoError(t, fenceFilling(ctx, h.pool, filling.Collection, testLogger()))
	require.NoError(t, finish(ctx))
	row, err := store.ReadModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, &live, row.Embedding, "search keeps the collection it has while chunks wait")
	assert.Equal(t, &filling, row.Filling)

	// Two of three embedded: still filling.
	_, err = h.pool.Exec(ctx, `UPDATE event_chunks SET embedded_at = now(), embedded_collection = $1 WHERE chunk_idx < 2`, filling.Collection)
	require.NoError(t, err)
	require.NoError(t, finish(ctx))
	row, err = store.ReadModels(ctx)
	require.NoError(t, err)
	assert.NotNil(t, row.Filling)

	// A chunk the embedder gave up on does not hold the change back.
	_, err = h.pool.Exec(ctx, `UPDATE event_chunks SET failure_count = 5 WHERE chunk_idx = 2`)
	require.NoError(t, err)
	require.NoError(t, finish(ctx))
	row, err = store.ReadModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, &filling, row.Embedding, "search uses the filled collection")
	assert.Nil(t, row.Filling, "and nothing is left to mark at the next leader term")
	var action string
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT action FROM admin_audit_log WHERE target = 'connector_settings:models' ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.embedding.filled", action)
}

// TestACollectionThatCannotBeOpenedStopsNeitherTheLeaderNorSilently: with an
// embedding model saved whose collection has another shape, the replica is
// not ready and says why, and the leader's other work still runs.
func TestACollectionThatCannotBeOpenedStopsNeitherTheLeaderNorSilently(t *testing.T) {
	h := newVectorHarness(t)
	// The collection holds vectors of 4 dimensions; the settings say 8.
	mismatched := settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 8, Collection: h.collection}
	models := &inferenceModels{embedding: mismatched}
	models.embed.ID = "local-embed"
	models.embed.endpoint.BaseURL = "http://gateway.invalid/v1"
	models.embed.endpoint.APIKey = "k"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	catalog := newModelCatalog(h.cfg, h.vectors)
	catalog.saved = func() settings.Models { return settings.Models{} }
	live := func() *inferenceModels { return models }
	readiness := httpserver.NewReadiness()
	retriever := retrieve.NewRetriever(h.pool, nil, nil, testLogger())
	go followEmbedding(ctx, testLogger(), catalog, live, h.vectors, retriever, readiness)
	require.Eventually(t, func() bool { return readiness.NotReady()[readySearch] != "" }, 10*time.Second, 20*time.Millisecond)
	reason := readiness.NotReady()[readySearch]
	assert.Contains(t, reason, h.collection)
	assert.Contains(t, reason, "vector size=4 want=8")
	require.NotNil(t, catalog.searchFault.Load(), "the Models page and the Overview say so too")

	// The leader's term: its vector work waits on the collection, and
	// everything else runs.
	sup := worker.NewSupervisor(testLogger(), prometheus.NewRegistry())
	addVectorWork(sup, leaderWork{cfg: h.cfg, pool: h.pool, vectors: h.vectors, logger: testLogger()}, models, testLogger())
	ran := make(chan struct{})
	sup.AddFunc("everything-else", func(ctx context.Context) error {
		close(ran)
		<-ctx.Done()
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sup.Run(ctx)
	}()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader's other work waited for the vector collection")
	}
	cancel()
	<-done
}
