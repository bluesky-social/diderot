package ask

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/require"
)

func TestAskServiceStagesReasoningEffortAfterCompletedToolRounds(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	server, bodies := newScriptedResponsesServer(t,
		toolCallStep("call_1", "lookup", `{}`),
		toolCallStep("call_2", "lookup", `{}`),
		toolCallStep("call_3", "lookup", `{}`),
		func(t *testing.T, w http.ResponseWriter) {
			writeRespondFixture(t, w, "resp_final", "staged answer")
		},
	)
	lookup, err := utils.InferTool("lookup", "lookup", func(context.Context, struct{}) (string, error) {
		return "tool result", nil
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "staged effort prompt", 6)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Now: time.Now, LinearTeams: testLinearTeams,
		Model: registryModel(t, "gpt-5.6-sol:direct"), ServiceTier: "fast",
		Limits: func() Limits { return Limits{ReasoningEffort: "medium", MaxToolTurns: 4} },
	})
	require.NoError(t, err)

	medium, fast := "medium", "fast"
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "research this", Surface: AskRunSurfaceHTTP,
		AllowedScopes: []string{"public"},
		ModelOverrides: ModelOverrides{
			ReasoningEffort: &medium,
			ServiceTier:     &fast,
			ReasoningEffortSchedule: &ReasoningEffortSchedule{
				AfterToolRounds: 2,
				Effort:          "high",
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "staged answer", result.Reply)

	recorded := bodies()
	require.Len(t, recorded, 4)
	var efforts []string
	for _, body := range recorded {
		reasoning, ok := body["reasoning"].(map[string]any)
		require.True(t, ok)
		efforts = append(efforts, reasoning["effort"].(string))
	}
	require.Equal(t, []string{"medium", "medium", "high", "high"}, efforts)

	runs, err := mgr.InspectRunTelemetry(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	var requested []string
	for _, call := range runs[0].InferenceCalls {
		if call.Workload == TelemetryWorkloadRoot {
			requested = append(requested, call.RequestedEffort)
		}
	}
	require.Equal(t, efforts, requested)
}
