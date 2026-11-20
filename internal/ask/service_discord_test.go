package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/require"
)

// grantChannel grants a source's channel scope to readers holding public.
type grantChannel struct{ calls atomic.Int32 }

func (g *grantChannel) GrantScopes(_ context.Context, scopes []string) []string {
	g.calls.Add(1)
	if slices.Contains(scopes, "public") {
		return append(slices.Clone(scopes), "discord-channel:7")
	}
	return scopes
}

// TestAskServiceDiscordTurnRunsWithTheGrantedScopes: a Discord turn runs in
// its chat thread's session, and its tools read with the scopes the
// sources grant on top of the turn's own.
func TestAskServiceDiscordTurnRunsWithTheGrantedScopes(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID, err := mgr.LoadOrCreateForSlackThread(context.Background(), "220000000000000001", "s-0123456789abcdef", "")
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tool", "object": "response", "created_at": 1, "status": "completed", "model": "test",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc_lookup", "call_id": "call-lookup",
					"name": "lookup", "arguments": `{}`, "status": "completed",
				}},
				"usage": responsesUsage(1, 1, 0),
			})
			return
		}
		writeRespondFixture(t, w, "resp_final", "done")
	}))
	defer server.Close()
	var observed []string
	lookup, err := utils.InferTool("lookup", "lookup", func(ctx context.Context, _ struct{}) (map[string]any, error) {
		observed = slices.Clone(scopesFromContext(ctx))
		return map[string]any{"count": 0}, nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "surface prompt", 4)
	require.NoError(t, err)
	grants := &grantChannel{}
	svc, err := NewAskService(AskServiceOptions{Manager: mgr, Agents: agents, LinearTeams: testLinearTeams, ScopeGrants: grants})
	require.NoError(t, err)
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "what changed?", Surface: AskRunSurfaceDiscord,
		AllowedScopes: []string{"public"},
	})
	require.NoError(t, err)
	require.Equal(t, "done", result.Reply)
	require.Equal(t, []string{"public", "discord-channel:7"}, observed)
	require.EqualValues(t, 1, grants.calls.Load())
}
