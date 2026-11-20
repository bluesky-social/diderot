package ask

import (
	"context"
	"slices"
	"testing"
)

func TestAuthorizationSnapshotSurvivesDetachment(t *testing.T) {
	scopes := []string{"public", "internal", "corpus:zendesk"}
	ctx := detachedMCPContext(context.Background(), "alice@example.com", scopes)
	scopes[0] = "corpus:other"
	extracted := scopesFromContext(ctx)
	extracted[0] = "corpus:other"
	if !slices.Equal(scopesFromContext(ctx), []string{"public", "internal", "corpus:zendesk"}) {
		t.Fatal("caller mutated the frozen scope set")
	}
	if slices.Contains(scopesFromContext(context.Background()), "corpus:zendesk") {
		t.Fatal("missing snapshot granted access")
	}
}
