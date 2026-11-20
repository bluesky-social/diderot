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

	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAskServiceSlackSurfaceWiresScopeStatusCitationsAndContext(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID, err := mgr.LoadOrCreateForSlackThread(context.Background(), "C1", "1700.1", "asker@example.com")
	require.NoError(t, err)

	const citedURL = "https://slack.example.com/archives/C1/p1700"
	var (
		modelCalls atomic.Int32
		requestMu  sync.Mutex
		requests   []map[string]any
		headers    []http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requestMu.Lock()
		requests = append(requests, body)
		headers = append(headers, r.Header.Clone())
		requestMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch modelCalls.Add(1) {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_slack_tool", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_lookup", "call_id": "call-lookup",
					"name": "lookup", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
		case 2:
			writeRespondFixture(t, w, "resp_slack_final", "See <"+citedURL+"|the thread>.")
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	var (
		observedScopes    []string
		observedAnswering []retrieve.Conversation
	)
	lookup, err := utils.InferTool("lookup", "lookup", func(ctx context.Context, _ struct{}) (map[string]any, error) {
		observedScopes = append([]string(nil), scopesFromContext(ctx)...)
		observedAnswering = conversationsBeingAnswered(ctx)
		citationsFromContext(ctx).add([]retrieve.Hit{{
			EventID: 99, ExternalID: "C1:1700.1", Source: "slack", URL: citedURL, Score: 0.9,
		}})
		return map[string]any{"url": citedURL, "count": 1}, nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "surface prompt", 4)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager:     mgr,
		Agents:      agents,
		LinearTeams: testLinearTeams,
		Presentations: map[string]ToolPresentation{
			"lookup": {
				Status:       func(json.RawMessage) StatusLine { return StatusLine{Emoji: "mag", Verb: "looking", Detail: "thread"} },
				StatusOutput: func(json.RawMessage) string { return "1 result" },
				StatusSources: func(json.RawMessage) []StatusSource {
					return []StatusSource{{URL: citedURL, Label: "thread"}}
				},
			},
		},
		Now: func() time.Time { return time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)

	var updates []StatusUpdate
	through := time.Date(2026, 8, 4, 19, 59, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Ada Lovelace", Handle: "ada", Email: "ada@example.com",
		SlackUserID: "UADA", LocalTime: through.In(time.FixedZone("PDT", -7*60*60)), TZLabel: "Pacific Daylight Time",
	}
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID:      sessionID,
		UserInput:      "what changed?",
		Surface:        AskRunSurfaceSlack,
		Asker:          asker,
		ThreadContext:  "[19:55] Grace: deploy finished",
		ContextThrough: through,
		AllowedScopes:  []string{"public", "slack:C1"},
		StatusEmitter:  func(update StatusUpdate) { updates = append(updates, update) },
	})
	require.NoError(t, err)
	require.Equal(t, "See <"+citedURL+"|the thread>.", result.Reply)
	require.Empty(t, result.IncompleteReason)
	require.Equal(t, 3, result.AssistantTurn)
	require.Equal(t, []string{"public", "slack:C1"}, observedScopes)
	require.Equal(t, []retrieve.Conversation{{Source: "slack", ThreadID: "C1:1700.1"}}, observedAnswering,
		"a turn in a Slack thread tells its search tools which thread it is answering")
	require.Len(t, updates, 4)
	require.Equal(t, []StatusState{StatusPending, StatusInProgress, StatusComplete}, statusStates(updates[:3]))
	require.Equal(t, "synthesizing", updates[3].Phase)
	for _, update := range updates[:3] {
		require.Equal(t, "call-lookup", update.TaskID)
	}
	require.Len(t, result.Citations, 1)
	require.Equal(t, citedURL, result.Citations[0].URL)

	known, gotThrough, err := mgr.SlackSessionState(context.Background(), "C1", "1700.1")
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, through.Equal(gotThrough), "context_through = %s, want instant %s", gotThrough, through)
	var storedCitation CitationRow
	err = pool.QueryRow(context.Background(),
		`SELECT event_id, url FROM ask_citations WHERE session_id = $1 AND turn = $2`, sessionID, result.AssistantTurn,
	).Scan(&storedCitation.EventID, &storedCitation.URL)
	require.NoError(t, err)
	require.Equal(t, CitationRow{EventID: 99, URL: citedURL}, storedCitation)

	requestMu.Lock()
	recorded := append([]map[string]any(nil), requests...)
	recordedHeaders := append([]http.Header(nil), headers...)
	requestMu.Unlock()
	require.Len(t, recorded, 2)
	require.Empty(t, recordedHeaders[0].Get("x-bedrock-metadata"))
	items := recorded[0]["input"].([]any)
	require.True(t, requestInputContainsText(items, agentInstruction("surface prompt", SurfaceSlack, nil)))
	require.True(t, requestInputContainsText(items, formatContextEnvelope(asker,
		time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC), "[19:55] Grace: deploy finished", "what changed?")))
}

func TestAskServiceScheduledSurfaceUsesSlackPromptAndScheduledMetadata(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var request map[string]any
	var requestHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		requestHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_scheduled", "scheduled report")
	}))
	defer server.Close()
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())

	_, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "report", Surface: AskRunSurfaceScheduled,
	})
	require.ErrorContains(t, err, "non-empty allowedScopes")
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "report", Surface: AskRunSurfaceScheduled,
		AllowedScopes: []string{"public"},
	})
	require.NoError(t, err)
	require.Equal(t, "scheduled report", result.Reply)
	require.Empty(t, requestHeaders.Get("x-bedrock-metadata"))
	require.True(t, requestInputContainsText(request["input"].([]any), agentInstruction("fixed service prompt", SurfaceSlack, nil)))
}

func TestExecuteTurnHTTPAndSlackPersistIdenticalUserContext(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_shared_context", "ok")
	}))
	defer server.Close()
	now := time.Date(2026, 8, 12, 22, 27, 51, 0, time.UTC)
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, now)
	asker := &AskerInfo{
		DisplayName: "Alex Rivera", Handle: "Alex",
		Email: "alex@example.com", SlackUserID: "U0EXAMPLE01",
		LocalTime: now.In(time.FixedZone("PDT", -7*60*60)),
		TZ:        "America/Los_Angeles", TZLabel: "Pacific Daylight Time",
	}

	var contexts []string
	for _, surface := range []AskRunSurface{AskRunSurfaceHTTP, AskRunSurfaceSlack} {
		sessionID := newAgenticSession(t, mgr)
		_, err := svc.ExecuteTurn(context.Background(), TurnRequest{
			SessionID: sessionID, UserInput: "whats the weather in seattle",
			Surface: surface, Asker: asker, AllowedScopes: []string{"public", "internal"},
		})
		require.NoError(t, err)
		_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
		require.NoError(t, err)
		contexts = append(contexts, AgenticMessageText(history[0].Message))
	}
	require.Equal(t, contexts[0], contexts[1])
}

func TestAskServiceSlackFailureDoesNotAdvanceContextThrough(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID, err := mgr.LoadOrCreateForSlackThread(context.Background(), "C2", "1800.1", "")
	require.NoError(t, err)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tool", "object": "response", "created_at": 1,
				"status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_lookup", "call_id": "call-lookup",
					"name": "lookup", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
			return
		}
		http.Error(w, "model unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()
	lookup, err := utils.InferTool("lookup", "lookup", func(context.Context, struct{}) (string, error) {
		return "completed tool result", nil
	})
	require.NoError(t, err)
	svc := newServiceForTest(t, mgr, server.URL+"/v1", []tool.BaseTool{lookup}, time.Now())
	through := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
	_, err = svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "question", Surface: AskRunSurfaceSlack,
		ThreadContext: "context", ContextThrough: through, AllowedScopes: []string{"public"},
	})
	require.Error(t, err)

	known, gotThrough, err := mgr.SlackSessionState(context.Background(), "C2", "1800.1")
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, gotThrough.IsZero())
	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2}, agenticTurns(history), "completed tool batch must remain durable")
}

func TestAskServiceSlackSurfaceValidation(t *testing.T) {
	svc := &AskService{}
	_, err := svc.ExecuteTurn(context.Background(), TurnRequest{UserInput: "q", Surface: AskRunSurfaceSlack, AllowedScopes: []string{"public"}})
	require.ErrorContains(t, err, "session ID")
	_, err = svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		UserInput: "q", Surface: AskRunSurfaceSlack,
	})
	require.ErrorContains(t, err, "non-empty allowedScopes")
	_, err = svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		UserInput: "q", Surface: AskRunSurfaceHTTP,
	})
	require.ErrorContains(t, err, "non-empty allowedScopes")
}
