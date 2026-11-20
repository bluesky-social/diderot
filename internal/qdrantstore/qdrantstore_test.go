package qdrantstore

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/qdrant/go-client/qdrant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAddr(t *testing.T) {
	tests := []struct {
		addr     string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"localhost:6334", "localhost", 6334, false},
		{"qdrant.example.com:6334", "qdrant.example.com", 6334, false},
		{"[::1]:6334", "::1", 6334, false},
		{"::1", "", 0, true}, // IPv6 without brackets must fail — previously mis-parsed as host="::" port="1"
		{"missingport", "", 0, true},
		{":6334", "", 0, true},
		{"host:notaport", "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			host, port, err := ParseAddr(tc.addr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("got (%q, %d), want (%q, %d)", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// testVectorDim is the collection size these tests run with.
const testVectorDim = 1024

//nolint:staticcheck // Fixtures mirror the v1.16 protobuf fields.
func validCollectionInfo(shards, replicas uint32) *qdrant.CollectionInfo {
	onDisk := true
	hnswOnDisk := false
	alwaysRAM := true
	return &qdrant.CollectionInfo{
		Config: &qdrant.CollectionConfig{
			Params: &qdrant.CollectionParams{
				ShardNumber:            shards,
				ReplicationFactor:      qdrant.PtrOf(replicas),
				WriteConsistencyFactor: qdrant.PtrOf(writeConsistencyFactor),
				OnDiskPayload:          true,
				VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
					Size: testVectorDim, Distance: qdrant.Distance_Cosine, OnDisk: &onDisk,
				}),
			},
			HnswConfig: &qdrant.HnswConfigDiff{
				M: qdrant.PtrOf(hnswM), EfConstruct: qdrant.PtrOf(hnswEFConstruct), OnDisk: &hnswOnDisk,
			},
			OptimizerConfig: &qdrant.OptimizersConfigDiff{
				MaxOptimizationThreads: qdrant.NewMaxOptimizationThreads(maxOptimizationThreads),
			},
			QuantizationConfig: qdrant.NewQuantizationScalar(&qdrant.ScalarQuantization{
				Type: qdrant.QuantizationType_Int8, AlwaysRam: &alwaysRAM,
			}),
		},
		PayloadSchema: map[string]*qdrant.PayloadSchemaInfo{
			"source":          {DataType: qdrant.PayloadSchemaType_Keyword},
			"actor_id":        {DataType: qdrant.PayloadSchemaType_Keyword},
			"actor_canonical": {DataType: qdrant.PayloadSchemaType_Keyword},
			"occurred_at":     {DataType: qdrant.PayloadSchemaType_Datetime},
			"scope":           {DataType: qdrant.PayloadSchemaType_Keyword},
			"thread_id":       {DataType: qdrant.PayloadSchemaType_Keyword}, // permitted legacy extra
		},
	}
}

func TestValidateCollection(t *testing.T) {
	store := &Store{collection: "test", dimensions: testVectorDim, shardNumber: 5, replicationFactor: 2}
	require.NoError(t, store.validateCollection(validCollectionInfo(5, 2)))

	t.Run("vector size drift", func(t *testing.T) {
		resized := &Store{collection: "test", dimensions: 768, shardNumber: 5, replicationFactor: 2}
		err := resized.validateCollection(validCollectionInfo(5, 2))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vector size=1024 want=768")
	})

	t.Run("topology drift", func(t *testing.T) {
		err := store.validateCollection(validCollectionInfo(4, 1))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shards=4 want=5")
		assert.Contains(t, err.Error(), "replication=1 want=2")
	})

	t.Run("wrong index type", func(t *testing.T) {
		info := validCollectionInfo(5, 2)
		info.PayloadSchema["scope"].DataType = qdrant.PayloadSchemaType_Integer
		err := store.validateCollection(info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload index "scope"=Integer want=Keyword`)
	})

	t.Run("missing index is created by ensure", func(t *testing.T) {
		info := validCollectionInfo(5, 2)
		delete(info.PayloadSchema, "scope")
		require.NoError(t, store.validateCollection(info))
	})
}

// TestZeroDimensionsRejectsVectorOperations: payload-only tools may omit
// Dimensions, but then nothing may create, write, or search vectors.
func TestZeroDimensionsRejectsVectorOperations(t *testing.T) {
	store, err := New(Config{Addr: "localhost:1", Collection: "payload_only"}, prometheus.NewRegistry())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	require.ErrorContains(t, store.EnsureCollection(ctx), "Dimensions is required")
	require.ErrorContains(t, store.Upsert(ctx, []Point{{ID: 1}}), "Dimensions is required")
	_, err = store.Search(ctx, nil, SearchFilters{}, 1)
	require.ErrorContains(t, err, "Dimensions is required")
}

func TestBuildFilter(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if f := buildFilter(SearchFilters{}); f == nil || len(f.Must) != 1 || len(f.Must[0].GetField().GetMatch().GetKeywords().GetStrings()) != 0 {
			t.Fatalf("expected empty scope match for zero-value filters, got %+v", f)
		}
	})
	t.Run("sources-only", func(t *testing.T) {
		f := buildFilter(SearchFilters{Sources: []string{"slack"}})
		if f == nil || len(f.Must) != 2 {
			t.Fatalf("expected source and scope conditions, got %+v", f)
		}
	})
	t.Run("all-dimensions", func(t *testing.T) {
		f := buildFilter(SearchFilters{
			Sources:        []string{"slack", "notion"},
			Actors:         []string{"U123"},
			OccurredAfter:  time.Unix(1700000000, 0),
			OccurredBefore: time.Unix(1710000000, 0),
		})
		if f == nil || len(f.Must) != 4 {
			t.Fatalf("expected 4 conditions (sources, actors, datetime range, scope), got %d: %+v",
				len(f.Must), f)
		}
	})
	t.Run("only-after", func(t *testing.T) {
		f := buildFilter(SearchFilters{OccurredAfter: time.Unix(1700000000, 0)})
		if f == nil || len(f.Must) != 2 {
			t.Fatalf("expected date and scope conditions, got %+v", f)
		}
	})
}

// TestIntegration runs end-to-end against a real qdrant. Skipped unless
// DIDEROT_QDRANT_TEST_ADDR is set — CI does not run this; local dev
// runs it against the compose-provided qdrant.
func TestIntegration(t *testing.T) {
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Unique collection name per test run so parallel runs and reruns
	// don't collide. Qdrant has no transactional rollback; we DROP in
	// cleanup instead.
	coll := "diderot_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	store, err := New(Config{
		Addr:       addr,
		Collection: coll,
		Dimensions: testVectorDim,
	}, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = store.Close() }()
	defer func() {
		// Best-effort cleanup; ignore error on teardown.
		_ = store.cli.DeleteCollection(context.Background(), coll)
	}()

	if exists, err := store.CollectionExists(ctx); err != nil || exists {
		t.Fatalf("CollectionExists before EnsureCollection = %t, %v", exists, err)
	}
	if err := store.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if exists, err := store.CollectionExists(ctx); err != nil || !exists {
		t.Fatalf("CollectionExists after EnsureCollection = %t, %v", exists, err)
	}

	// Re-running EnsureCollection must be a no-op (idempotency is how
	// the deploy-path safely calls it every startup).
	if err := store.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection (2nd call): %v", err)
	}

	vec := make([]float32, testVectorDim)
	for i := range vec {
		vec[i] = float32(i%7) * 0.01
	}
	points := []Point{
		{
			ID:         1,
			Vector:     vec,
			EventID:    1001,
			Source:     "slack",
			ActorID:    "U123",
			ThreadID:   "T1",
			OccurredAt: time.Unix(1700000000, 0).UTC(),
			Scope:      "public",
		},
		{
			ID:         2,
			Vector:     vec,
			EventID:    1002,
			Source:     "notion",
			ActorID:    "notion-user-abc",
			OccurredAt: time.Unix(1705000000, 0).UTC(),
			Scope:      "internal",
		},
	}
	if err := store.Upsert(ctx, points); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Sanity: count reflects both points.
	if n, err := store.CountPoints(ctx); err != nil {
		t.Fatalf("CountPoints: %v", err)
	} else if n != 2 {
		t.Fatalf("CountPoints: got %d, want 2", n)
	}

	// Search for the same vector — both points should come back.
	hits, err := store.Search(ctx, vec, SearchFilters{AllowedScopes: []string{"public", "internal"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search: got %d hits, want 2", len(hits))
	}
	for _, h := range hits {
		if h.EventID == 0 {
			t.Errorf("hit %d has no event_id in payload", h.ChunkID)
		}
	}

	// Filter by source = slack → only point 1.
	hits, err = store.Search(ctx, vec, SearchFilters{Sources: []string{"slack"}, AllowedScopes: []string{"public", "internal"}}, 10)
	if err != nil {
		t.Fatalf("filtered Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("filtered Search: got %+v, want [1]", hits)
	}

	// ScrollIDs enumerates all ids.
	seen := map[uint64]bool{}
	if err := store.ScrollIDs(ctx, 100, func(ids []uint64) error {
		for _, id := range ids {
			seen[id] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("ScrollIDs: %v", err)
	}
	if !seen[1] || !seen[2] || len(seen) != 2 {
		t.Fatalf("ScrollIDs: got %v, want {1,2}", seen)
	}

	// ScrollScopes requests only the scope payload and keeps ids paired.
	scopes := map[uint64]string{}
	if err := store.ScrollScopes(ctx, 1, func(ids []uint64, values []string) error {
		if len(ids) != len(values) {
			t.Fatalf("ids=%d scopes=%d", len(ids), len(values))
		}
		for i, id := range ids {
			scopes[id] = values[i]
		}
		return nil
	}); err != nil {
		t.Fatalf("ScrollScopes: %v", err)
	}
	if scopes[1] != "public" || scopes[2] != "internal" {
		t.Fatalf("ScrollScopes: got %v", scopes)
	}

	// DeleteIDs removes point 1; scroll now sees only point 2.
	if err := store.DeleteIDs(ctx, []uint64{1}); err != nil {
		t.Fatalf("DeleteIDs: %v", err)
	}
	if n, err := store.CountPoints(ctx); err != nil {
		t.Fatalf("CountPoints post-delete: %v", err)
	} else if n != 1 {
		t.Fatalf("CountPoints post-delete: got %d, want 1", n)
	}

	// Deleting a non-existent id is a no-op (idempotent).
	if err := store.DeleteIDs(ctx, []uint64{9999}); err != nil {
		t.Fatalf("DeleteIDs(missing): %v", err)
	}

	// Re-Upsert overwrites the vector for an existing id without
	// failing — this is what the embedder relies on for crash-safety.
	if err := store.Upsert(ctx, []Point{points[1]}); err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}
}

func TestUpsertWrongDim(t *testing.T) {
	// Validation fires pre-gRPC, so no qdrant needed.
	store := &Store{collection: "unused", metrics: newMetrics(prometheus.NewRegistry(), "unused")}
	err := store.Upsert(context.Background(), []Point{{
		ID:     1,
		Vector: []float32{0.1, 0.2}, // wrong dim
	}})
	if err == nil {
		t.Fatal("expected error on wrong-dim vector, got nil")
	}
}

func TestSearchWrongDim(t *testing.T) {
	store := &Store{collection: "unused", metrics: newMetrics(prometheus.NewRegistry(), "unused")}
	_, err := store.Search(context.Background(), []float32{0.1}, SearchFilters{}, 10)
	if err == nil {
		t.Fatal("expected error on wrong-dim query vector, got nil")
	}
}
