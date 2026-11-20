package qdrantstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func BenchmarkChannelScopeSearch(b *testing.B) {
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		b.Skip("DIDEROT_QDRANT_TEST_ADDR is required")
	}
	ctx := context.Background()
	collection := fmt.Sprintf("diderot_scope_bench_%d", time.Now().UnixNano())
	store, err := New(Config{Addr: addr, Collection: collection, Dimensions: testVectorDim}, prometheus.NewRegistry())
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, store.cli.DeleteCollection(context.Background(), collection))
		require.NoError(b, store.Close())
	})
	require.NoError(b, store.EnsureCollection(ctx))
	vector := make([]float32, testVectorDim)
	vector[0] = 1
	scopes := make([]string, 2060)
	for start := 0; start < len(scopes); start += 100 {
		var points []Point
		for index := start; index < min(start+100, len(scopes)); index++ {
			scopes[index] = fmt.Sprintf("slack-channel:C%010d", index)
			points = append(points, Point{ID: uint64(index + 1), EventID: int64(index + 1), Vector: vector, Source: "slack", Scope: scopes[index], OccurredAt: time.Now()})
		}
		require.NoError(b, store.Upsert(ctx, points))
	}
	for _, count := range []int{20, 206, 2060} {
		b.Run(fmt.Sprintf("%d-scopes", count), func(b *testing.B) {
			filters := SearchFilters{AllowedScopes: scopes[:count]}
			b.ReportAllocs()
			for b.Loop() {
				hits, err := store.Search(ctx, vector, filters, 10)
				require.NoError(b, err)
				require.Len(b, hits, 10)
				for _, hit := range hits {
					require.LessOrEqual(b, hit.EventID, int64(count))
				}
			}
		})
	}
}
