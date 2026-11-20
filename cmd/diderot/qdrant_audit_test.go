package main

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

// TestQdrantAuditBeforeTheCollectionExists: an audit run before the server
// has created the collection says so, and what creates it, instead of
// Qdrant's error.
func TestQdrantAuditBeforeTheCollectionExists(t *testing.T) {
	addr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if addr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set; skipping integration test")
	}
	collection := "diderot_test_missing_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	store, err := qdrantstore.New(qdrantstore.Config{Addr: addr, Collection: collection, Dimensions: 8}, prometheus.NewRegistry())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	err = requireQdrantCollection(t.Context(), store)
	require.EqualError(t, err, "the Qdrant collection "+collection+
		" doesn't exist: choose the embedding model again on the Models page to create it")
}
