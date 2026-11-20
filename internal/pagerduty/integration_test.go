package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/require"
)

// noLinearTeams is a loaded, empty team set: no bare issue key is a reference.
type noLinearTeams struct{}

func (noLinearTeams) KnownLinearTeams(context.Context) (map[string]struct{}, bool) {
	return map[string]struct{}{}, true
}

func TestSignedPageToDurableInvestigationAndLateSlackThread(t *testing.T) {
	store, pool := testStore(t)
	var modelCalls, deployReads, notePosts, investigations atomic.Int32
	noteContent := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/incidents/P123":
			_, _ = writer.Write([]byte(`{"incident":{"id":"P123","title":"API failures","status":"triggered"}}`))
		case "/incidents/P123/alerts":
			_, _ = writer.Write([]byte(`{"alerts":[{"body":{"details":{"query":"api_errors_total"}}}],"more":false}`))
		case "/incidents":
			_, _ = writer.Write([]byte(`{"incidents":[{"id":"POTHER","title":"Related upstream errors","status":"triggered"}],"more":false}`))
		case "/incidents/P123/notes":
			if request.Method == http.MethodGet {
				notes := []map[string]string{}
				if noteContent != "" {
					notes = append(notes, map[string]string{"id": "NOTE1", "content": noteContent})
				}
				require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"notes": notes}))
			} else {
				require.Equal(t, http.MethodPost, request.Method)
				notePosts.Add(1)
				var payload struct {
					Note struct {
						Content string `json:"content"`
					} `json:"note"`
				}
				require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
				noteContent = payload.Note.Content
				_, _ = writer.Write([]byte(`{"note":{"id":"NOTE1"}}`))
			}
		case "/v1/responses":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
			serialized, err := json.Marshal(payload)
			require.NoError(t, err)
			require.Contains(t, string(serialized), "## Answering in Slack")
			require.NotContains(t, string(serialized), "responding autonomously to a PagerDuty page")
			require.Contains(t, string(serialized), "Investigate this alert")
			require.Contains(t, string(serialized), "Related upstream errors")
			var output []any
			if modelCalls.Add(1) == 1 {
				output = []any{
					map[string]any{"type": "function_call", "id": "fc_obs", "call_id": "call_obs", "name": ask.InvestigateToolName, "arguments": `{"request":"Investigate api errors around 2026-10-14T19:30:00Z"}`, "status": "completed"},
					map[string]any{"type": "function_call", "id": "fc_deploy", "call_id": "call_deploy", "name": "list_deployments", "arguments": `{"start_after":"2026-10-14T18:30:00Z","start_before":"2026-10-14T20:00:00Z","offset":0}`, "status": "completed"},
				}
			} else {
				require.Contains(t, string(serialized), "revision abc123")
				require.Contains(t, string(serialized), "errors began 19:25Z")
				answer, err := json.Marshal(ask.FinalResponse{Answer: "Hypothesis: api rollout preceded errors. Correlation is not proven causation. Inspect revision abc123.", Artifacts: []ask.ArtifactSelection{}})
				require.NoError(t, err)
				output = []any{map[string]any{"type": "message", "id": "msg_final", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": string(answer), "annotations": []any{}}}}}
			}
			require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{
				"id": fmt.Sprintf("resp_%d", modelCalls.Load()), "object": "response", "created_at": 1,
				"status": "completed", "model": "test", "output": output,
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20,
					"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}},
			}))
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(404)
		}
	}))
	defer server.Close()
	model, err := ask.NewResponsesModel(context.Background(), ask.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "test", ModelID: "test", Timeout: time.Minute, MaxRetries: ask.NoResponsesRetries,
	})
	require.NoError(t, err)
	observability, err := utils.InferTool(ask.InvestigateToolName, "scripted read-only telemetry investigation",
		func(_ context.Context, input struct {
			Request string `json:"request"`
		}) (map[string]string, error) {
			investigations.Add(1)
			require.Contains(t, input.Request, "2026-10-14T19:30:00Z")
			return map[string]string{"observation": "errors began 19:25Z"}, nil
		})
	require.NoError(t, err)
	// A deployment-history tool such as an operator's MCP server serves.
	deployments, err := utils.InferTool("list_deployments", "scripted read-only change timeline",
		func(_ context.Context, input struct {
			StartAfter  string `json:"start_after"`
			StartBefore string `json:"start_before"`
			Offset      int    `json:"offset"`
		}) (map[string]any, error) {
			deployReads.Add(1)
			require.Equal(t, "2026-10-14T18:30:00Z", input.StartAfter)
			require.Equal(t, "2026-10-14T20:00:00Z", input.StartBefore)
			return map[string]any{"events": []map[string]string{{"id": "deploy1", "timestamp": "2026-10-14T19:20:00Z",
				"description": "api rollout", "long_description": "revision abc123"}}}, nil
		})
	require.NoError(t, err)
	agents, err := ask.NewAskAgents(context.Background(), model, []tool.BaseTool{observability, deployments}, "You are Diderot.", 5)
	require.NoError(t, err)
	service, err := ask.NewAskService(ask.AskServiceOptions{
		Manager: ask.NewManager(pool, nil), Agents: agents, LinearTeams: noLinearTeams{},
		Limits: func() ask.Limits { return ask.Limits{MaxToolTurns: 4} },
	})
	require.NoError(t, err)
	pager := NewClient("token", "bot@example.com")
	pager.baseURL = server.URL
	slack := &slackFixture{}
	runner := &Runner{Queue: store, Agent: service, Pager: pager, Slack: newSlackFixture(t, store, slack), PublicURL: "https://diderot.example", AllowedScopes: []string{"public"}}
	webhook := &Webhook{Secrets: secretsOn("signing-secret"), Inbox: store}
	body, err := json.Marshal(map[string]any{"event": testEvent()})
	require.NoError(t, err)
	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
		request.Header.Set("X-PagerDuty-Signature", signature(body, "signing-secret"))
		response := httptest.NewRecorder()
		webhook.ServeHTTP(response, request)
		require.Equal(t, 202, response.Code)
		if index == 0 {
			require.ErrorIs(t, runner.Tick(context.Background()), ErrSlackThreadNotFound)
			require.Equal(t, int32(1), notePosts.Load())
			require.Equal(t, int32(2), modelCalls.Load())
			slack.mu.Lock()
			require.Zero(t, slack.posts)
			slack.rootVisible = true
			slack.mu.Unlock()
			_, err := pool.Exec(t.Context(), `UPDATE pagerduty_investigations SET next_attempt_at=now()-interval '1 second'`)
			require.NoError(t, err)
		}
		require.NoError(t, runner.Tick(context.Background()))
	}
	require.Equal(t, int32(2), modelCalls.Load())
	require.Equal(t, int32(1), investigations.Load())
	require.Equal(t, int32(1), deployReads.Load())
	require.Equal(t, int32(1), notePosts.Load())
	slack.mu.Lock()
	require.Equal(t, 1, slack.posts)
	require.Contains(t, slack.posted, "revision abc123")
	require.Contains(t, slack.posted, "/admin/ask/sessions/")
	slack.mu.Unlock()
	require.Contains(t, noteContent, "revision abc123")
	require.Contains(t, noteContent, "/admin/ask/sessions/")
	var state, surface string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT jobs.state, sessions.surface FROM pagerduty_investigations jobs JOIN ask_sessions sessions ON sessions.id=jobs.session_id`).Scan(&state, &surface))
	require.Equal(t, "done", state)
	require.Equal(t, "pagerduty", surface)
}
