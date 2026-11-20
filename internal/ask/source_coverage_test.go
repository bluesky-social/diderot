package ask

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// TestRetrieveResultNamesTheSourcesNotYetSearched: whether a source gets
// searched must not depend on the model reaching it in a list. Each result
// of a search limited to some sources names the ones no search of the turn
// has covered, in the registry's order, until a search of everything or the
// last of them clears it.
func TestRetrieveResultNamesTheSourcesNotYetSearched(t *testing.T) {
	all := testSources.EventSources()
	require.GreaterOrEqual(t, len(all), 3, "the test needs a few sources")
	fake := &fakeCorpusRetriever{result: &retrieve.Result{}}
	retrieveTool := NewEinoRetrieveTool(fake, testSources).Tool.(tool.InvokableTool)

	notSearched := func(ctx context.Context, arguments string) []string {
		t.Helper()
		raw, err := retrieveTool.InvokableRun(ctx, arguments)
		require.NoError(t, err)
		var out struct {
			NotSearched []string `json:"not_searched"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &out))
		return out.NotSearched
	}
	limitedTo := func(sources ...string) string {
		arguments, err := json.Marshal(map[string]any{"question": "when did it ship", "sources": sources})
		require.NoError(t, err)
		return string(arguments)
	}
	turn := func() context.Context {
		return withSourceCoverage(WithAllowedScopes(context.Background(), []string{"public"}))
	}

	// One source at a time, as the model used to search.
	ctx := turn()
	require.Equal(t, all[1:], notSearched(ctx, limitedTo(all[0])))
	require.Equal(t, all[2:], notSearched(ctx, limitedTo(all[1])), "what earlier searches covered stays covered")
	for _, name := range all[2:] {
		notSearched(ctx, limitedTo(name))
	}
	require.Empty(t, notSearched(ctx, limitedTo(all[0])), "every source searched by name: nothing is left")

	// A search of everything covers everything, whatever came before.
	ctx = turn()
	require.Equal(t, all[1:], notSearched(ctx, limitedTo(all[0])))
	require.Empty(t, notSearched(ctx, `{"question":"when did it ship"}`))
	require.Empty(t, notSearched(ctx, limitedTo(all[0])), "and it stays covered for the rest of the turn")

	// The next turn starts again.
	require.Equal(t, all[1:], notSearched(turn(), limitedTo(all[0])))

	// Outside an Ask turn there is no record and nothing is claimed.
	require.Empty(t, notSearched(WithAllowedScopes(context.Background(), []string{"public"}), limitedTo(all[0])))
}
