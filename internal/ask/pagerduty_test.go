package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAskAgentsNeedNoPagerDutyPrompt(t *testing.T) {
	instructions := defaultAskInstructions("base")
	agents, err := NewAskAgentsWithInstructions(context.Background(), newAskAgentsTestModel(t, ""), nil, instructions, 4)
	require.NoError(t, err)
	require.NotNil(t, agents.HTTP)
	require.NotNil(t, agents.Slack)
}

func TestAskServicePagerDutySurfacePersistsProvenanceAndTelemetry(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	var modelRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.NoError(t, json.NewDecoder(request.Body).Decode(&modelRequest))
		writer.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, writer, "resp_pagerduty", "Root cause not established.")
	}))
	defer server.Close()
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "investigate this page", Surface: AskRunSurfacePagerDuty, AllowedScopes: []string{"public"},
	})
	require.NoError(t, err)
	require.Equal(t, "Root cause not established.", result.Reply)
	require.True(t, requestInputContainsText(modelRequest["input"].([]any), agentInstruction("fixed service prompt", SurfaceSlack, nil)))
	require.False(t, requestInputContainsText(modelRequest["input"].([]any), "responding autonomously to a PagerDuty page"))
	var surface string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT surface FROM ask_sessions WHERE id=$1`, sessionID).Scan(&surface))
	require.Equal(t, "pagerduty", surface)
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT surface FROM ask_runs WHERE session_id=$1`, sessionID).Scan(&surface))
	require.Equal(t, "pagerduty", surface)
}
