package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/require"
)

func TestAgenticStatusMiddlewareValidation(t *testing.T) {
	middleware := newAgenticStatusMiddleware()
	validEndpoint := func(context.Context, string, ...tool.Option) (string, error) { return "", nil }

	wrapped, err := middleware.WrapInvokableToolCall(context.Background(), nil, &adk.ToolContext{Name: "lookup", CallID: "call"})
	require.Nil(t, wrapped)
	require.ErrorContains(t, err, "nil tool endpoint")
	for _, toolContext := range []*adk.ToolContext{nil, {}, {Name: "lookup"}, {CallID: "call"}} {
		wrapped, err = middleware.WrapInvokableToolCall(context.Background(), validEndpoint, toolContext)
		require.Nil(t, wrapped)
		require.ErrorContains(t, err, "incomplete tool context")
	}
}

func TestAgenticStatusMiddlewareSuccessLifecycle(t *testing.T) {
	middleware := newAgenticStatusMiddleware()
	wrapped, err := middleware.WrapInvokableToolCall(context.Background(),
		func(_ context.Context, arguments string, _ ...tool.Option) (string, error) {
			require.JSONEq(t, `{"query":"status"}`, arguments)
			return `{"count":2}`, nil
		},
		&adk.ToolContext{Name: "lookup", CallID: "call-1"},
	)
	require.NoError(t, err)

	var updates []StatusUpdate
	ctx := withToolStatus(context.Background(), func(update StatusUpdate) {
		updates = append(updates, update)
	}, map[string]ToolPresentation{
		"lookup": {
			Status: func(input json.RawMessage) StatusLine {
				require.JSONEq(t, `{"query":"status"}`, string(input))
				return StatusLine{Emoji: "mag", Verb: "searching", Detail: "status"}
			},
			StatusOutput: func(output json.RawMessage) string {
				require.JSONEq(t, `{"count":2}`, string(output))
				return "2 results"
			},
			StatusSources: func(output json.RawMessage) []StatusSource {
				require.JSONEq(t, `{"count":2}`, string(output))
				return []StatusSource{{URL: "https://example.com", Label: "result"}}
			},
		},
	})
	result, err := wrapped(ctx, `{"query":"status"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"count":2}`, result)
	require.Equal(t, []StatusUpdate{
		{Phase: "tool_use", ToolName: "lookup", TaskID: "call-1", State: StatusPending, Emoji: "mag", Verb: "searching", Summary: "status"},
		{Phase: "tool_use", ToolName: "lookup", TaskID: "call-1", State: StatusInProgress, Emoji: "mag", Verb: "searching", Summary: "status"},
		{Phase: "tool_use", ToolName: "lookup", TaskID: "call-1", State: StatusComplete, Emoji: "mag", Verb: "searching", Summary: "status", Output: "2 results", Sources: []StatusSource{{URL: "https://example.com", Label: "result"}}},
	}, updates)
}

func TestAgenticStatusMiddlewareErrorPreservesResult(t *testing.T) {
	wantErr := errors.New("fatal")
	middleware := newAgenticStatusMiddleware()
	wrapped, err := middleware.WrapInvokableToolCall(context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) { return "partial", wantErr },
		&adk.ToolContext{Name: "lookup", CallID: "call-error"},
	)
	require.NoError(t, err)

	var outputCalled atomic.Bool
	var updates []StatusUpdate
	ctx := withToolStatus(context.Background(), func(update StatusUpdate) { updates = append(updates, update) }, map[string]ToolPresentation{
		"lookup": {
			StatusOutput: func(json.RawMessage) string { outputCalled.Store(true); return "must not run" },
			StatusSources: func(json.RawMessage) []StatusSource {
				outputCalled.Store(true)
				return nil
			},
		},
	})
	result, err := wrapped(ctx, `{}`)
	require.Equal(t, "partial", result)
	require.ErrorIs(t, err, wantErr)
	require.False(t, outputCalled.Load())
	require.Equal(t, []StatusState{StatusPending, StatusInProgress, StatusError}, statusStates(updates))
}

func TestAgenticStatusMiddlewareRecoverableErrorIsStatusError(t *testing.T) {
	base := newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		return toolErrorOutput{}, errors.New("dependency unavailable")
	})
	recoverable := wrapOneTool(t, base)
	middleware := newAgenticStatusMiddleware()
	wrapped, err := middleware.WrapInvokableToolCall(context.Background(), recoverable.InvokableRun,
		&adk.ToolContext{Name: "lookup", CallID: "call-recoverable"})
	require.NoError(t, err)

	var outputCalled atomic.Bool
	var updates []StatusUpdate
	ctx := withToolStatus(context.Background(), func(update StatusUpdate) { updates = append(updates, update) }, map[string]ToolPresentation{
		"lookup": {
			StatusOutput: func(json.RawMessage) string { outputCalled.Store(true); return "wrong" },
			StatusSources: func(json.RawMessage) []StatusSource {
				outputCalled.Store(true)
				return nil
			},
		},
	})
	result, err := wrapped(ctx, `{"query":"q"}`)
	require.NoError(t, err, "recoverable failure must remain model-visible data")
	require.JSONEq(t, `{"error":"dependency unavailable"}`, result)
	require.False(t, outputCalled.Load())
	require.Equal(t, []StatusState{StatusPending, StatusInProgress, StatusError}, statusStates(updates))
}

func TestRecoverableToolErrorPayloadRecognitionIsClosed(t *testing.T) {
	for _, result := range []string{
		``, `null`, `[]`, `{}`, `{"error":null}`, `{"error":""}`,
		`{"error":"warning","ok":true}`, `{"error":{"message":"bad"}}`,
	} {
		require.False(t, IsRecoverableToolErrorPayload(result), result)
	}
	require.True(t, IsRecoverableToolErrorPayload(`{"error":"bad"}`))
}

func TestAgenticStatusMiddlewareRunsThroughTypedAgent(t *testing.T) {
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch modelCalls.Add(1) {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tool", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_status", "call_id": "call-status",
					"name": "lookup", "arguments": `{"query":"status"}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
		case 2:
			writeAskAgentsFixture(t, w, "resp_final", "done")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	type lookupInput struct {
		Query string `json:"query"`
	}
	lookup, err := utils.InferTool("lookup", "lookup", func(_ context.Context, input lookupInput) (map[string]int, error) {
		require.Equal(t, "status", input.Query)
		return map[string]int{"count": 1}, nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "status test", 3)
	require.NoError(t, err)

	var updates []StatusUpdate
	ctx := withToolStatus(context.Background(), func(update StatusUpdate) { updates = append(updates, update) }, map[string]ToolPresentation{
		"lookup": {Status: func(json.RawMessage) StatusLine { return StatusLine{Verb: "looking"} }},
	})
	iter := agents.HTTP.Query(ctx, "use lookup")
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, event.Err)
	}
	require.Equal(t, int32(2), modelCalls.Load())
	require.Equal(t, []StatusState{StatusPending, StatusInProgress, StatusComplete}, statusStates(updates))
	for _, update := range updates {
		require.Equal(t, "lookup", update.ToolName)
		require.Equal(t, "call-status", update.TaskID)
		require.Equal(t, "looking", update.Verb)
	}
}

func TestAgenticStatusMiddlewareParallelRunsSerializeEmitter(t *testing.T) {
	middleware := newAgenticStatusMiddleware()
	var (
		updatesMu  sync.Mutex
		updates    []StatusUpdate
		active     atomic.Int32
		concurrent atomic.Bool
	)
	emitter := func(update StatusUpdate) {
		if active.Add(1) != 1 {
			concurrent.Store(true)
		}
		time.Sleep(time.Millisecond)
		updatesMu.Lock()
		updates = append(updates, update)
		updatesMu.Unlock()
		active.Add(-1)
	}
	ctx := withToolStatus(context.Background(), emitter, nil)

	var wg sync.WaitGroup
	for _, id := range []string{"call-a", "call-b"} {
		wrapped, err := middleware.WrapInvokableToolCall(context.Background(),
			func(context.Context, string, ...tool.Option) (string, error) { return `{}`, nil },
			&adk.ToolContext{Name: "lookup", CallID: id},
		)
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, callErr := wrapped(ctx, `{}`)
			require.NoError(t, callErr)
		}()
	}
	wg.Wait()
	require.False(t, concurrent.Load(), "per-run emitter must never be entered concurrently")
	updatesMu.Lock()
	captured := append([]StatusUpdate(nil), updates...)
	updatesMu.Unlock()
	require.Len(t, captured, 6)
	for _, id := range []string{"call-a", "call-b"} {
		var perCall []StatusUpdate
		for _, update := range captured {
			if update.TaskID == id {
				perCall = append(perCall, update)
			}
		}
		require.Equal(t, []StatusState{StatusPending, StatusInProgress, StatusComplete}, statusStates(perCall))
	}

	// A separate run without status context must not inherit another run's
	// emitter or presentation registry.
	wrapped, err := middleware.WrapInvokableToolCall(context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) { return `{}`, nil },
		&adk.ToolContext{Name: "lookup", CallID: "call-no-status"},
	)
	require.NoError(t, err)
	_, err = wrapped(context.Background(), `{}`)
	require.NoError(t, err)
	updatesMu.Lock()
	require.Len(t, updates, 6)
	updatesMu.Unlock()
}

func statusStates(updates []StatusUpdate) []StatusState {
	states := make([]StatusState, 0, len(updates))
	for _, update := range updates {
		states = append(states, update.State)
	}
	return states
}
