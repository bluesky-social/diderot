package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAskServiceToolBudgetForcesRespondOnlySynthesis(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var (
		calls     atomic.Int32
		requestMu sync.Mutex
		requests  []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requestMu.Lock()
		requests = append(requests, body)
		requestMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			writeBudgetToolCall(t, w, "call-1")
		case 2:
			writeRespondFixture(t, w, "resp_synth", "synthesized from results")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	svc := newBudgetServiceForTest(t, mgr, server.URL+"/v1")
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "use the tool")
	require.NoError(t, err)
	require.Equal(t, TurnResult{Reply: "synthesized from results", IncompleteReason: IncompleteToolBudget, AssistantTurn: 3}, result)
	require.Equal(t, int32(2), calls.Load())

	requestMu.Lock()
	recorded := append([]map[string]any(nil), requests...)
	requestMu.Unlock()
	require.Len(t, recorded, 2)
	require.True(t, requestInputContainsText(recorded[1]["input"].([]any), capSynthInstr))
	if tools, present := recorded[1]["tools"].([]any); present {
		require.Len(t, tools, 1, "forced synthesis request must preserve historical tool schemas")
	} else {
		t.Fatal("forced synthesis request must preserve tool schemas")
	}
	require.Equal(t, "none", recorded[1]["tool_choice"], "forced synthesis must forbid further tools")

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(history))
	require.Len(t, history[2].Message.ContentBlocks, 1, "synthesis instruction is in-memory only")
	require.Equal(t, "synthesized from results", AgenticMessageText(history[3].Message))
	require.Equal(t, IncompleteToolBudget, history[3].IncompleteReason)
}

func TestAskServiceToolBudgetRewritesRepeatedToolCallToCannedFinal(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			writeBudgetToolCall(t, w, "call-1")
		case 2:
			writeBudgetToolCall(t, w, "call-ignored")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	svc := newBudgetServiceForTest(t, mgr, server.URL+"/v1")
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "keep using tools")
	require.ErrorContains(t, err, "synthesis failed without a valid terminal response")
	require.Empty(t, result)
	require.Equal(t, int32(2), calls.Load(), "rewritten forced output must terminate without executing another tool")

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(history))
	require.Empty(t, AgenticMessageToolCalls(history[3].Message))
	require.Equal(t, capErrorCannedText, AgenticMessageText(history[3].Message))
	require.Equal(t, IncompleteSynthesisError, history[3].IncompleteReason)
}

func TestAskServiceToolBudgetPersistsCannedFinalOnSynthesisError(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			writeBudgetToolCall(t, w, "call-1")
			return
		}
		http.Error(w, "synthesis unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	svc := newBudgetServiceForTest(t, mgr, server.URL+"/v1")
	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "use the tool")
	require.ErrorContains(t, err, "synthesis failed without a valid terminal response")
	require.Empty(t, result)
	require.GreaterOrEqual(t, calls.Load(), int32(2))

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(history))
	require.Equal(t, capErrorCannedText, AgenticMessageText(history[3].Message))
	require.Equal(t, IncompleteSynthesisError, history[3].IncompleteReason)
}

func TestAskServiceToolBudgetCancellationDoesNotPersistFallback(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			writeBudgetToolCall(t, w, "call-1")
			return
		}
		close(secondStarted)
		select {
		case <-r.Context().Done():
		case <-releaseSecond:
			http.Error(w, "released after cancellation", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	svc := newBudgetServiceForTest(t, mgr, server.URL+"/v1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := executeHTTPTestTurn(ctx, svc, sessionID, "use the tool")
		done <- runErr
	}()
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("forced synthesis request did not start")
	}
	cancel()
	close(releaseSecond)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled turn did not return")
	}

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2}, agenticTurns(history))
}

// TestAskServiceReadsLimitsPerRun: the service reads its limits as each run
// starts, so a changed effort or tool-turn limit applies to the next question
// with no new service; an effort the model does not accept is its default.
func TestAskServiceReadsLimitsPerRun(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)

	var requestMu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requestMu.Lock()
		requests = append(requests, body)
		call := len(requests)
		requestMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Call tools until the budget forbids them.
		if body["tool_choice"] == "none" {
			writeRespondFixture(t, w, "resp_synth", "synthesized")
			return
		}
		writeBudgetToolCall(t, w, "call-"+strconv.Itoa(call))
	}))
	defer server.Close()
	drain := func() []map[string]any {
		requestMu.Lock()
		defer requestMu.Unlock()
		drained := requests
		requests = nil
		return drained
	}
	efforts := func(bodies []map[string]any) []any {
		var out []any
		for _, body := range bodies {
			reasoning, _ := body["reasoning"].(map[string]any)
			out = append(out, reasoning["effort"])
		}
		return out
	}

	lookup, err := utils.InferTool("lookup", "lookup", func(context.Context, struct{}) (string, error) {
		return "tool result", nil
	})
	require.NoError(t, err)
	mdl, err := NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL + "/v1", APIKey: "token", Model: registryModel(t, "gpt-6.1-sol:main"), MaxTokens: 256,
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), mdl, []tool.BaseTool{lookup}, "limits prompt", 10)
	require.NoError(t, err)
	var limitsMu sync.Mutex
	limits := Limits{ReasoningEffort: "low", MaxToolTurns: 1}
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Now: time.Now, LinearTeams: testLinearTeams, Model: registryModel(t, "gpt-6.1-sol:main"),
		Limits: func() Limits {
			limitsMu.Lock()
			defer limitsMu.Unlock()
			return limits
		},
	})
	require.NoError(t, err)
	setLimits := func(next Limits) {
		limitsMu.Lock()
		limits = next
		limitsMu.Unlock()
	}

	_, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "use the tool")
	require.NoError(t, err)
	assert.Equal(t, []any{"low", "low"}, efforts(drain()), "one tool round, then the forced answer")

	setLimits(Limits{ReasoningEffort: "high", MaxToolTurns: 3})
	_, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "use the tool")
	require.NoError(t, err)
	assert.Equal(t, []any{"high", "high", "high", "high"}, efforts(drain()))

	// gpt-6.1-sol lists no "none"; the model's default sends no effort.
	setLimits(Limits{ReasoningEffort: "none"})
	_, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "use the tool")
	require.NoError(t, err)
	got := drain()
	assert.Len(t, got, defaultMaxToolTurns+1, "zero turns is the default")
	assert.Equal(t, make([]any, len(got)), efforts(got))
}

func newBudgetServiceForTest(t *testing.T, mgr *Manager, baseURL string) *AskService {
	t.Helper()
	lookup, err := utils.InferTool("lookup", "lookup", func(context.Context, struct{}) (string, error) {
		return "tool result", nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, baseURL), []tool.BaseTool{lookup}, "budget prompt", 4)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Now: time.Now, LinearTeams: testLinearTeams,
		Limits: func() Limits { return Limits{MaxToolTurns: 1} },
	})
	require.NoError(t, err)
	return svc
}

func writeBudgetToolCall(t *testing.T, w http.ResponseWriter, callID string) {
	t.Helper()
	writeResponsesFixture(t, w, map[string]any{
		"id": "resp_" + callID, "object": "response", "created_at": 1,
		"status": "completed", "model": "test",
		"output": []any{map[string]any{
			"type": "function_call", "id": "fc_" + callID, "call_id": callID,
			"name": "lookup", "arguments": `{}`, "status": "completed",
		}},
		"usage": responsesUsage(1, 1, 0),
	})
}
