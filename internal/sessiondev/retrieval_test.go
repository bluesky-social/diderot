package sessiondev

import (
	"context"
	"testing"

	"github.com/bluesky-social/diderot/internal/events"
)

func TestDeterministicEmbedderAndScopes(t *testing.T) {
	embedder := DeterministicEmbedder{}
	vectors, err := embedder.Embed(context.Background(), []string{"same", "same", "different"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || len(vectors[0]) != VectorDim {
		t.Fatalf("vector shape = %d x %d", len(vectors), len(vectors[0]))
	}
	for dimension := range vectors[0] {
		if vectors[0][dimension] != vectors[1][dimension] {
			t.Fatal("equal input produced different vectors")
		}
	}
	identical := true
	for dimension := range vectors[0] {
		if vectors[0][dimension] != vectors[2][dimension] {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("different input produced an identical vector")
	}
	scopes := (ScopeResolver{}).ScopesForEmail(context.Background(), " Owner@Example.Invalid ")
	if len(scopes) != 3 || scopes[2] != events.ScopeOwner("owner@example.invalid") {
		t.Fatalf("scopes = %v", scopes)
	}
}
