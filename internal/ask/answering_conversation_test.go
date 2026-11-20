package ask

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// TestSessionConversationsNamesTheSlackThread: a session started in Slack is
// part of one corpus conversation, its thread, under the key the Slack
// connector gives that thread's events. A session started over the API or
// MCP has no records of its own in the corpus.
func TestSessionConversationsNamesTheSlackThread(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	inThread, err := mgr.LoadOrCreateForSlackThread(ctx, "C0EXAMPLE1", "1700000500.000100", "asker@example.com")
	require.NoError(t, err)
	// A second thread, so the lookup is seen to pick one session's.
	_, err = mgr.LoadOrCreateForSlackThread(ctx, "C0EXAMPLE1", "1690000000.000100", "asker@example.com")
	require.NoError(t, err)
	conversations, err := mgr.SessionConversations(ctx, inThread)
	require.NoError(t, err)
	require.Equal(t, []retrieve.Conversation{{Source: "slack", ThreadID: "C0EXAMPLE1:1700000500.000100"}}, conversations)

	overAPI, err := mgr.CreateOwned(ctx, "asker@example.com")
	require.NoError(t, err)
	conversations, err = mgr.SessionConversations(ctx, overAPI.ID)
	require.NoError(t, err)
	require.Empty(t, conversations)
}

// TestSearchToolsLeaveOutTheConversationBeingAnswered: the exclusion is put
// on every retrieve and list request by the server, from the run's context.
// It is not a tool parameter, so the model can neither set it nor lift it.
func TestSearchToolsLeaveOutTheConversationBeingAnswered(t *testing.T) {
	answering := []retrieve.Conversation{{Source: "slack", ThreadID: "C0EXAMPLE1:1700000500.000100"}}
	scoped := WithAllowedScopes(context.Background(), []string{"public"})
	inConversation := withConversationsBeingAnswered(scoped, answering)

	fake := &fakeCorpusRetriever{result: &retrieve.Result{}}
	retrieveTool := NewEinoRetrieveTool(fake, testSources).Tool.(tool.InvokableTool)
	listTool := NewEinoListTool(fake, testSources).Tool.(tool.InvokableTool)

	_, err := retrieveTool.InvokableRun(inConversation, `{"question":"when did it ship"}`)
	require.NoError(t, err)
	require.Equal(t, answering, fake.retrieveReq.Filters.ExcludeConversations)
	_, err = listTool.InvokableRun(inConversation, `{"sources":["slack"]}`)
	require.NoError(t, err)
	require.Equal(t, answering, fake.listReq.ExcludeConversations)

	// A run that is not part of any corpus conversation excludes nothing.
	_, err = retrieveTool.InvokableRun(scoped, `{"question":"when did it ship"}`)
	require.NoError(t, err)
	require.Empty(t, fake.retrieveReq.Filters.ExcludeConversations)
	_, err = listTool.InvokableRun(scoped, `{"sources":["slack"]}`)
	require.NoError(t, err)
	require.Empty(t, fake.listReq.ExcludeConversations)

	for _, registered := range []RegisteredTool{NewEinoRetrieveTool(fake, testSources), NewEinoListTool(fake, testSources)} {
		parameters := toolParameters(context.Background(), registered.Tool.Info)
		require.NotContains(t, parameters["properties"], "exclude_conversations")
	}
}
