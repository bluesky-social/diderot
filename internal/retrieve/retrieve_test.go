package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// deterministicEmbedder produces a fixed-dim vector that captures
// per-character distribution of the input. Same content → same
// vector; similar content → similar cosine. Distinguishes
// real-world-sized strings enough that cosine-search ranks an
// exact match above an unrelated one.
//
// Approach: fold each rune's value into a slot chosen by
// (rune % dim). This gives a 1024-dim "character histogram" that
// exact-match scores 1.0 on and unrelated content scores poorly on.
type deterministicEmbedder struct {
	dim int
	id  string
}

func (d *deterministicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, d.dim)
		for _, r := range t {
			slot := int(uint32(r)) % d.dim
			v[slot] += 1.0
		}
		out[i] = v
	}
	return out, nil
}
func (d *deterministicEmbedder) Dimensions() int { return d.dim }
func (d *deterministicEmbedder) ID() string      { return d.id }

func testDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "retrieve")
}

func testQdrantAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set")
	}
	return addr
}

// setupWorld wires the full pipeline against a real pool + a fresh
// qdrant collection. Also TRUNCATEs events so each test starts clean.
func setupWorld(t *testing.T) (*pgxpool.Pool, *qdrantstore.Store, *deterministicEmbedder) {
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

	coll := fmt.Sprintf("diderot_test_retrieve_%d", time.Now().UnixNano())
	store, err := qdrantstore.New(qdrantstore.Config{
		Addr: testQdrantAddr(t), Collection: coll, Dimensions: 1024,
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
		_ = store.Drop(context.Background())
		_ = store.Close()
	})

	emb := &deterministicEmbedder{dim: store.Dimensions(), id: "test:fake:v1"}
	return pool, store, emb
}

// seedEvent ingests one event via the sink + pushes its
// embedding(s) to qdrant manually (short-circuiting the embedder
// worker so tests are fast + deterministic). Defaults Scope to
// public (Postgres row AND qdrant payload, matching the embedder
// worker's behavior); scope-filtering tests set it explicitly.
func seedEvent(t *testing.T, pool *pgxpool.Pool, store *qdrantstore.Store, emb *deterministicEmbedder, ev events.Event) int64 {
	t.Helper()
	ctx := context.Background()
	if ev.Scope == "" {
		ev.Scope = events.ScopePublic
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	res, err := sink.Upsert(ctx, ev)
	if err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	// Embed + push each chunk of this event.
	rows, err := pool.Query(ctx,
		`SELECT id, content FROM event_chunks WHERE event_id = $1 ORDER BY chunk_idx`,
		res.EventID)
	if err != nil {
		t.Fatal(err)
	}
	var points []qdrantstore.Point
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			t.Fatal(err)
		}
		vecs, err := emb.Embed(ctx, []string{content})
		if err != nil {
			t.Fatal(err)
		}
		points = append(points, qdrantstore.Point{
			ID: uint64(id), Vector: vecs[0],
			EventID: res.EventID, Source: ev.Source, ActorID: ev.ActorID,
			ThreadID: ev.ThreadID, OccurredAt: ev.OccurredAt,
			Scope: ev.Scope,
		})
	}
	rows.Close()
	if err := store.Upsert(ctx, points); err != nil {
		t.Fatalf("seed upsert qdrant: %v", err)
	}
	// Mark as embedded so the dereference's deleted=false check
	// isn't the only path exercising real data.
	if _, err := pool.Exec(ctx,
		`UPDATE event_chunks SET embedded_at=now(), embedder_id=$1 WHERE event_id=$2`,
		emb.id, res.EventID); err != nil {
		t.Fatal(err)
	}
	return res.EventID
}

func TestRetrieveHappyPath(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)

	// Three events. The question matches the first exactly, so it
	// should rank highest under the deterministic embedder.
	idA := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		ThreadID: "T1", URL: "https://x/1", Title: "#ops",
		Body: "k8s cluster is angry and alerts are firing",
		Raw:  json.RawMessage(`{}`),
	})
	seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "C1:2.2",
		OccurredAt: time.Unix(1700000100, 0), Actor: "bob", ActorID: "U2",
		Body: "totally unrelated message about groceries", Raw: json.RawMessage(`{}`),
	})
	seedEvent(t, pool, store, emb, events.Event{
		Source: "ssh", ExternalID: "sess-1",
		OccurredAt: time.Unix(1700000200, 0), Actor: "alice", ActorID: "U1",
		Body: "ran `systemctl restart foo` and it worked", Raw: json.RawMessage(`{}`),
	})

	res, err := r.Retrieve(context.Background(), Request{
		Question: "k8s cluster is angry and alerts are firing",
		Filters:  Filters{AllowedScopes: []string{"public", "internal"}},
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected hits, got none")
	}
	if res.Hits[0].EventID != idA {
		t.Errorf("top hit should be the exact-match event id=%d; got %d (%q)",
			idA, res.Hits[0].EventID, res.Hits[0].Content)
	}
	if res.EmbedModelID != emb.id {
		t.Errorf("embed_model_id = %q, want %q", res.EmbedModelID, emb.id)
	}
	// Latency fields populated. Use Total instead of asserting on
	// stage timers — on fast CI hardware, individual stages can
	// complete in <500µs and all round to 0ms, even though the
	// timers ran. TotalMS is `time.Since(total).Milliseconds()`
	// which floor-rounds; we just want a non-negative value plus
	// a non-empty embed model id (sanity-check the Result was
	// populated, not that we hit any particular wall-clock floor).
	if res.Latency.TotalMS < 0 {
		t.Errorf("Total latency negative: %d", res.Latency.TotalMS)
	}
}

func TestRetrieveDedupsByEventID(t *testing.T) {
	// An event with a long body gets split into multiple chunks;
	// they all end up in qdrant, the top-N returns several of
	// them. Dedup must keep only one hit per event_id.
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)

	longBody := strings.Repeat("cluster alerts k8s angry ", 200) // ~5000 chars -> multi-chunk
	idA := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: longBody,
		Raw: json.RawMessage(`{}`),
	})

	res, err := r.Retrieve(context.Background(), Request{
		Question: "cluster alerts k8s angry",
		Filters:  Filters{AllowedScopes: []string{"public", "internal"}},
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Errorf("multi-chunk event should dedup to 1 hit; got %d", len(res.Hits))
		for _, h := range res.Hits {
			t.Logf("  hit event_id=%d chunk_id=%d", h.EventID, h.ChunkID)
		}
	}
	if res.Hits[0].EventID != idA {
		t.Errorf("got event_id=%d, want %d", res.Hits[0].EventID, idA)
	}
	if res.Hits[0].ChunkCount < 2 {
		t.Errorf("multi-chunk event should report ChunkCount >= 2; got %d", res.Hits[0].ChunkCount)
	}
}

func TestRetrieveFiltersBySources(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)

	// Same-ish content from two sources; filter picks one.
	seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "deployment failure on prod",
		Raw: json.RawMessage(`{}`),
	})
	idSSH := seedEvent(t, pool, store, emb, events.Event{
		Source: "ssh", ExternalID: "sess-1",
		OccurredAt: time.Unix(1700000100, 0), Body: "deployment failure on prod",
		Raw: json.RawMessage(`{}`),
	})

	res, err := r.Retrieve(context.Background(), Request{
		Question: "deployment failure on prod",
		Filters:  Filters{Sources: []string{"ssh"}, AllowedScopes: []string{"public", "internal"}},
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("ssh-filter should return 1 hit; got %d", len(res.Hits))
	}
	if res.Hits[0].EventID != idSSH {
		t.Errorf("filter leaked non-ssh hit: got event_id=%d, want %d",
			res.Hits[0].EventID, idSSH)
	}
}

func TestRetrieveDroppedDeletedEvent(t *testing.T) {
	// An event that got tombstoned after ingest (deleted=true).
	// Its qdrant points still exist until the reconciler cleans
	// them up, but the dereference SQL has `AND e.deleted = false`.
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)

	id := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "secret content",
		Raw: json.RawMessage(`{}`),
	})

	// Simulate the sink's Delete path on the events row but leave
	// the qdrant point intact (mimicking the reconciler-lag window).
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE events SET deleted=true, body='', title=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	res, err := r.Retrieve(ctx, Request{
		Question: "secret content", TopK: 10,
		Filters: Filters{AllowedScopes: []string{"public", "internal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("tombstoned event must not surface; got %d hits", len(res.Hits))
	}
}

func TestRetrieveEmptyQuestionErrors(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	_, err := r.Retrieve(context.Background(), Request{Question: ""})
	if err == nil {
		t.Error("empty question should error")
	}
}

func TestRetrieveEmptyCorpus(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	// No seeds: qdrant + events both empty.
	res, err := r.Retrieve(context.Background(), Request{Question: "anything"})
	if err != nil {
		t.Fatalf("empty corpus should not error; got %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("empty corpus got %d hits", len(res.Hits))
	}
}
