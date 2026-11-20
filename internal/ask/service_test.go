package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func executeHTTPTestTurn(ctx context.Context, svc *AskService, sessionID uuid.UUID, userInput string) (TurnResult, error) {
	return svc.ExecuteTurn(ctx, TurnRequest{
		SessionID:     sessionID,
		UserInput:     userInput,
		Surface:       AskRunSurfaceHTTP,
		AllowedScopes: []string{"public", "internal"},
	})
}

func TestNewAskServiceValidation(t *testing.T) {
	mgr := &Manager{}
	agents := &AskAgents{}

	tests := []struct {
		name string
		opts AskServiceOptions
		want string
	}{
		{name: "manager", opts: AskServiceOptions{Agents: agents}, want: "manager is required"},
		{name: "agents", opts: AskServiceOptions{Manager: mgr}, want: "agents are required"},
		{name: "HTTP runner", opts: AskServiceOptions{Manager: mgr, Agents: agents}, want: "HTTP agent runner is required"},
		{name: "linear teams", opts: AskServiceOptions{Manager: mgr, PromptRuntime: &PromptRuntime{}}, want: "linear team provider is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := NewAskService(tt.opts)
			require.Nil(t, svc)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestAskServiceCreateSession(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("CreateSession must not call the model")
	}))
	defer server.Close()
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())

	sessionID, err := svc.CreateSession(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, sessionID)

	sess, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Empty(t, history)
}

func TestAskServiceFinalOnlyPersistsExactTurn(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	now := time.Date(2026, 8, 4, 19, 30, 0, 0, time.FixedZone("ignored", -7*60*60))

	var request map[string]any
	var requestHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		requestHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_final", "final answer")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, now)
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "what changed?")
	require.NoError(t, err)
	require.Equal(t, TurnResult{Reply: "final answer", AssistantTurn: 1}, result)

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(history))
	require.Equal(t, formatContextEnvelope(nil, now, "", "what changed?"), AgenticMessageText(history[0].Message))
	require.Equal(t, "final answer", AgenticMessageText(history[1].Message))

	require.NotContains(t, request, "metadata")
	requireFinalResponseFormat(t, request)
	require.Equal(t, sessionID.String(), requestHeaders.Get("x-agent-parent-session-id"))
	require.Empty(t, requestHeaders.Get("x-bedrock-metadata"), "provider invocation attribution header must be absent")
}

func TestAskServiceExecuteTurnPersistsAuthenticatedAsker(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	now := time.Date(2026, 8, 11, 20, 22, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_identity", "answer for Morgan")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, now)
	asker := &AskerInfo{DisplayName: "Morgan Lee", Email: "morgan@example.com"}
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID:     sessionID,
		UserInput:     "did I ask this already?",
		Surface:       AskRunSurfaceHTTP,
		Asker:         asker,
		AllowedScopes: []string{"public", "internal"},
	})
	require.NoError(t, err)
	require.Equal(t, "answer for Morgan", result.Reply)

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Contains(t, AgenticMessageText(history[0].Message),
		`"asker":{"display_name":"Morgan Lee","email":"morgan@example.com"}`)
}

func TestAskServicePersistsResponsesIncompleteReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   IncompleteReason
	}{
		{name: "max output tokens", reason: "max_output_tokens", want: IncompleteMaxTokens},
		{name: "content filter", reason: "content_filter", want: IncompleteContentFiltered},
		{name: "unknown", reason: "future_reason", want: IncompleteUnknownStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			mgr := NewManager(pool, nil)
			sessionID := newAgenticSession(t, mgr)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeResponsesFixture(t, w, map[string]any{
					"id": "resp_incomplete", "object": "response", "created_at": 1,
					"status": "incomplete", "incomplete_details": map[string]any{"reason": tt.reason},
					"model": "test",
					"output": []any{map[string]any{
						"type": "message", "id": "msg_partial", "status": "incomplete", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "partial answer", "annotations": []any{}}},
					}},
					"usage": responsesUsage(1, 1, 0),
				})
			}))
			defer server.Close()

			svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
			result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "question")
			require.ErrorContains(t, err, "model response incomplete")
			require.Empty(t, result)

			_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
			require.NoError(t, err)
			require.Len(t, history, 2)
			require.Equal(t, tt.want, history[1].IncompleteReason)
		})
	}
}

func TestAskServiceParallelToolBatchThenFinal(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch modelCalls.Add(1) {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tools", "object": "response", "created_at": 1,
				"status": "completed", "model": "test", "parallel_tool_calls": true,
				"output": []any{
					map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "lookup_a", "arguments": `{"value":"a"}`, "status": "completed"},
					map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "lookup_b", "arguments": `{"value":"b"}`, "status": "completed"},
				},
				"usage": responsesUsage(10, 3, 1),
			})
		case 2:
			writeRespondFixture(t, w, "resp_final", "both done")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	type lookupInput struct {
		Value string `json:"value"`
	}
	type lookupOutput struct {
		Value string `json:"value"`
	}
	allStarted := make(chan struct{})
	var started atomic.Int32
	var startOnce sync.Once
	newLookup := func(name string) tool.BaseTool {
		base, err := utils.InferTool(name, "parallel lookup", func(ctx context.Context, in lookupInput) (lookupOutput, error) {
			if started.Add(1) == 2 {
				startOnce.Do(func() { close(allStarted) })
			}
			select {
			case <-allStarted:
			case <-ctx.Done():
				return lookupOutput{}, ctx.Err()
			}
			return lookupOutput{Value: in.Value + "-done"}, nil
		})
		require.NoError(t, err)
		return base
	}

	svc := newServiceForTest(t, mgr, server.URL+"/v1", []tool.BaseTool{
		newLookup("lookup_a"), newLookup("lookup_b"),
	}, time.Now())
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "look up both")
	require.NoError(t, err)
	require.Equal(t, TurnResult{Reply: "both done", AssistantTurn: 4}, result)
	require.Equal(t, int32(2), started.Load())
	require.Equal(t, int32(2), modelCalls.Load())

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3, 4}, agenticTurns(history))
	require.Equal(t, []schema.FunctionToolCall{
		{CallID: "call_a", Name: "lookup_a", Arguments: `{"value":"a"}`},
		{CallID: "call_b", Name: "lookup_b", Arguments: `{"value":"b"}`},
	}, AgenticMessageToolCalls(history[1].Message))
	require.Equal(t, "lookup_a", AgenticMessageToolResults(history[2].Message)[0].Name)
	require.Equal(t, "lookup_b", AgenticMessageToolResults(history[3].Message)[0].Name)
	require.Equal(t, "both done", AgenticMessageText(history[4].Message))
}

func TestAskServiceContinuationReplaysPersistedHistory(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var (
		mu             sync.Mutex
		requests       []map[string]any
		requestHeaders []http.Header
		calls          atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		requests = append(requests, body)
		requestHeaders = append(requestHeaders, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		call := calls.Add(1)
		writeRespondFixture(t, w, "resp", map[int32]string{1: "first answer", 2: "second answer"}[call])
	}))
	defer server.Close()

	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, now)
	first, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "first question")
	require.NoError(t, err)
	require.Equal(t, 1, first.AssistantTurn)
	second, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "second question")
	require.NoError(t, err)
	require.Equal(t, TurnResult{Reply: "second answer", AssistantTurn: 3}, second)

	mu.Lock()
	recorded := append([]map[string]any(nil), requests...)
	recordedHeaders := append([]http.Header(nil), requestHeaders...)
	mu.Unlock()
	require.Len(t, recorded, 2)
	require.Empty(t, recordedHeaders[1].Get("x-bedrock-metadata"))
	items := recorded[1]["input"].([]any)
	require.GreaterOrEqual(t, len(items), 4)
	require.True(t, requestInputContainsText(items, formatContextEnvelope(nil, now, "", "first question")))
	require.True(t, requestInputContainsText(items, "first answer"))
	require.True(t, requestInputContainsText(items, formatContextEnvelope(nil, now, "", "second question")))

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(history))
}

func TestAskServiceLaterModelFailureKeepsCompletedToolBatch(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tools", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "lookup", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
			return
		}
		http.Error(w, "model unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	lookup, err := utils.InferTool("lookup", "lookup", func(context.Context, struct{}) (string, error) {
		return "tool result", nil
	})
	require.NoError(t, err)
	svc := newServiceForTest(t, mgr, server.URL+"/v1", []tool.BaseTool{lookup}, time.Now())
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "use the tool")
	require.Error(t, err)
	require.Empty(t, result)
	require.GreaterOrEqual(t, calls.Load(), int32(2))

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2}, agenticTurns(history))
	require.Len(t, AgenticMessageToolCalls(history[1].Message), 1)
	require.Equal(t, "tool result", toolResultText(t, AgenticMessageToolResults(history[2].Message)[0]))
}

func TestAskServiceSerializesSameSessionAndCancelsWaiter(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			http.Error(w, "same-session waiter reached model before lock release", http.StatusInternalServerError)
			return
		}
		close(requestStarted)
		select {
		case <-releaseRequest:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_first", "first answer")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	type turnOutcome struct {
		result TurnResult
		err    error
	}
	firstDone := make(chan turnOutcome, 1)
	go func() {
		result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "first")
		firstDone <- turnOutcome{result: result, err: err}
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not reach model")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	second, err := executeHTTPTestTurn(waitCtx, svc, sessionID, "second")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, second)
	require.Equal(t, int32(1), calls.Load())

	close(releaseRequest)
	select {
	case outcome := <-firstDone:
		require.NoError(t, outcome.err)
		require.Equal(t, TurnResult{Reply: "first answer", AssistantTurn: 1}, outcome.result)
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not finish after model release")
	}

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(history))
}

func TestAskServiceExecuteTurnAppliesTurnDeadline(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	// The handler's own delay (1s) bounds the worst case: even if the
	// client's context cancellation takes longer than expected to tear
	// down the in-flight request, the handler still returns on its own
	// and the test cannot hang. An earlier version of this test tried to
	// observe cancellation directly (blocking forever on
	// <-r.Context().Done(), or asserting r.Context().Deadline() from the
	// handler) and both hung or failed: an HTTP server has no visibility
	// into the client's context deadline — that timer is enforced purely
	// on the client side and is never transmitted over the wire — so the
	// only thing worth asserting here is wall-clock: the turn deadline
	// must cut the call short well before the handler would have replied.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_deadline", "answer")
	}))
	defer server.Close()

	svc := newServiceForTestWithDeadline(t, mgr, server.URL+"/v1", 150*time.Millisecond, time.Now())

	start := time.Now()
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "question")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Empty(t, result)
	require.Less(t, elapsed, 700*time.Millisecond,
		"a 150ms turn deadline should cut the call short well before the handler's 1s delay elapses")
}

func TestAskServiceExecuteTurnPreservesEarlierCallerDeadline(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_deadline", "answer")
	}))
	defer server.Close()

	// Default TurnDeadline (10m, far longer than the caller's deadline
	// below). context.WithTimeout(ctx, s.turnDeadline) must keep the
	// caller's earlier deadline rather than extending it.
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())

	callerCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := executeHTTPTestTurn(callerCtx, svc, sessionID, "question")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Empty(t, result)
	require.Less(t, elapsed, 700*time.Millisecond,
		"the caller's 150ms deadline should win over the much longer service default")
}

// Rendering a panel is not a decision to publish it. An empty
// final artifacts must yield an empty attachment list, because the model uses
// that emptiness deliberately: a fallback that attached every rendered panel
// would post blank images under a turn that explained it had no usable
// panels.
func TestExecuteTurnDoesNotAttachUnselectedRenderedArtifacts(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	store := NewArtifactStore(pool, &memoryArtifactBlob{})
	sessionID := newAgenticSession(t, mgr)

	_, err := store.PersistPanelImage(context.Background(), sessionID, "call-panel", testPanelPNG(t))
	require.NoError(t, err)

	renderPanel, err := utils.InferTool("render_panel", "render a panel", func(ctx context.Context, _ struct{}) (string, error) {
		return "rendered", nil
	})
	require.NoError(t, err)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_render", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_render", "call_id": "call-panel",
					"name": "render_panel", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
		case 2:
			// writeRespondFixture emits an empty artifacts array, which is exactly
			// the unselected-artifact shape this test exercises.
			writeRespondFixture(t, w, "resp_final", "Here's the panel.")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{renderPanel}, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{Manager: mgr, Agents: agents, Artifacts: store, Now: time.Now, LinearTeams: testLinearTeams})
	require.NoError(t, err)

	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "show me the panel")
	require.NoError(t, err)
	require.Equal(t, "Here's the panel.", result.Reply)
	require.Empty(t, result.Artifacts,
		"a panel the model did not select in final artifacts must not be attached")
}

func newServiceForTest(t *testing.T, mgr *Manager, baseURL string, tools []tool.BaseTool, now time.Time) *AskService {
	t.Helper()
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, baseURL), tools, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{Manager: mgr, Agents: agents, Now: func() time.Time { return now }, LinearTeams: testLinearTeams})
	require.NoError(t, err)
	return svc
}

func newServiceForTestWithDeadline(t *testing.T, mgr *Manager, baseURL string, turnDeadline time.Duration, now time.Time) *AskService {
	t.Helper()
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, baseURL), nil, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager:      mgr,
		Agents:       agents,
		Now:          func() time.Time { return now },
		TurnDeadline: turnDeadline,
		LinearTeams:  testLinearTeams,
	})
	require.NoError(t, err)
	return svc
}

func requestInputContainsText(items []any, want string) bool {
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			if content == want {
				return true
			}
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if ok && part["text"] == want {
					return true
				}
			}
		}
	}
	return false
}

func toolResultText(t *testing.T, result schema.FunctionToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	require.NotNil(t, result.Content[0].Text)
	return result.Content[0].Text.Text
}

// TestAskServiceRendersOrgNamePerTurn proves a saved organization name
// reaches the model's tool descriptions on the next turn, without rebuilding
// the agents.
func TestAskServiceRendersOrgNamePerTurn(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)

	var mu sync.Mutex
	var descriptions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Description string `json:"description"`
			} `json:"tools"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		for _, tool := range body.Tools {
			descriptions = append(descriptions, tool.Description)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_final", "done")
	}))
	defer server.Close()

	lookup, err := utils.InferTool("lookup", "what "+OrgNamePlaceholder+" decided", func(context.Context, struct{}) (string, error) {
		return "", nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "fixed service prompt", 5)
	require.NoError(t, err)
	var name atomic.Value
	name.Store("Example Corp")
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Now: time.Now, LinearTeams: testLinearTeams,
		OrgName: func() string { return name.Load().(string) },
	})
	require.NoError(t, err)

	_, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "first")
	require.NoError(t, err)
	name.Store("")
	_, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "second")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, descriptions, "what Example Corp decided")
	require.Contains(t, descriptions, "what "+DefaultOrgName+" decided")
	for _, description := range descriptions {
		require.NotContains(t, description, OrgNamePlaceholder)
	}
}
