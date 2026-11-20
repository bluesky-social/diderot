package ask

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// TestCorpusMCPAskBlockedDuringSetup: a request the setup gate refused gets
// the notice from the ask tool and starts nothing, while the corpus tools
// keep working.
func TestCorpusMCPAskBlockedDuringSetup(t *testing.T) {
	const notice = "Diderot is still being set up."
	runner := newFakeCorpusAskRunner()
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Content: "answer"}}}}
	srv, err := newCorpusMCPServer(retriever, testSources, &fakeExpander{}, &fakeScopeLookup{}, &fakeAttachmentReader{}, &fakeExactReferenceResolver{}, testLinearTeams,
		func() CorpusAskLease { return CorpusAskLease{Runner: runner, Context: t.Context()} },
		newMemoryCorpusTaskBackend(), t.Context(), "test")
	require.NoError(t, err)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithMCPCaller(r.Context(), r.Header.Get("X-Test-User"), []string{"public", "internal"})
		transport.ServeHTTP(w, r.WithContext(WithMCPAskBlocked(ctx, notice)))
	}))
	t.Cleanup(server.Close)

	result := resultObject(t, callRPC(t, server, "user@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "what changed?"},
	}, true))
	require.Equal(t, true, result["isError"])
	require.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], notice)
	select {
	case <-runner.started:
		t.Fatal("a refused ask started a run")
	default:
	}

	result = resultObject(t, callRPC(t, server, "user@example.com", "tools/call", "retrieve", map[string]any{
		"name": "retrieve", "arguments": map[string]any{"question": "what changed?"},
	}, true))
	require.NotEqual(t, true, result["isError"], "retrieve is not gated")
}
