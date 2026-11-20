package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

type testToolInput struct {
	X string `json:"x"`
}

func newAskAgentsPinnedTool(t *testing.T) tool.BaseTool {
	t.Helper()
	toolInfo, err := utils.InferTool("pinned_tool", "Test tool", func(ctx context.Context, in testToolInput) (string, error) {
		return "", nil
	})
	require.NoError(t, err)
	return toolInfo
}

// TestNewAskAgentsValidation pins the validation contract: nil model, blank
// base prompt, and non-positive iteration limits are rejected before any
// request can be issued.
func TestNewAskAgentsValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mdl := newAskAgentsTestModel(t, "")
	pinned := newAskAgentsPinnedTool(t)

	cases := []struct {
		name       string
		model      model.AgenticModel
		basePrompt string
		maxIter    int
		wantErr    string
	}{
		{name: "nil model", model: nil, basePrompt: "base", maxIter: 5, wantErr: "model is required"},
		{name: "blank prompt", model: mdl, basePrompt: "", maxIter: 5, wantErr: "basePrompt is required"},
		{name: "whitespace prompt", model: mdl, basePrompt: "   \n\t ", maxIter: 5, wantErr: "basePrompt is required"},
		{name: "zero iterations", model: mdl, basePrompt: "base", maxIter: 0, wantErr: "maxIterations must be positive"},
		{name: "negative iterations", model: mdl, basePrompt: "base", maxIter: -3, wantErr: "maxIterations must be positive"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			agents, err := NewAskAgents(ctx, tt.model, []tool.BaseTool{pinned}, tt.basePrompt, tt.maxIter)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Nil(t, agents)
		})
	}
}

// TestNewAskAgentsConstructionMakesNoRequests verifies that construction
// performs no network I/O: the httptest handler fails the test if contacted.
func TestNewAskAgentsConstructionMakesNoRequests(t *testing.T) {
	t.Parallel()

	var hit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		t.Errorf("construction must not send a request to %s", r.URL)
	}))
	defer server.Close()

	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"),
		[]tool.BaseTool{newAskAgentsPinnedTool(t)}, "You are the pinned base prompt.", 5)
	require.NoError(t, err)
	require.NotNil(t, agents.HTTP)
	require.NotNil(t, agents.Slack)
	if hit.Load() {
		t.Error("httptest server was contacted during construction")
	}
}

// TestAskAgentsRunsFinalAnswerThroughBothSurfaces drives one no-tool
// final-answer call through each runner against a shared httptest Responses
// server. It asserts the first input item is exactly the surface-specific
// fixed system prompt (basePrompt + surface addendum, kept distinct between
// surfaces), that the server sees both surface prompts, and that each runner
// yields the fixture's final message.
func TestAskAgentsRunsFinalAnswerThroughBothSurfaces(t *testing.T) {
	t.Parallel()

	const basePrompt = "You are diderot, the pinned test assistant."
	type recordedRequest struct {
		Body map[string]any
	}
	var (
		requestMu sync.Mutex
		requests  []recordedRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requestMu.Lock()
		requests = append(requests, recordedRequest{Body: body})
		requestMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		writeAskAgentsFixture(t, w, "resp_final", "final answer text")
	}))
	defer server.Close()

	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"),
		[]tool.BaseTool{newAskAgentsPinnedTool(t)}, basePrompt, 5)
	require.NoError(t, err)

	for name, runner := range map[string]*adk.TypedRunner[*schema.AgenticMessage]{
		"http":  agents.HTTP,
		"slack": agents.Slack,
	} {
		runner := runner
		t.Run(name, func(t *testing.T) {
			iter := runner.Query(context.Background(), "what time is it?")
			var final *schema.AgenticMessage
			for {
				event, ok := iter.Next()
				if !ok {
					break
				}
				require.NoError(t, event.Err)
				if event.Output == nil || event.Output.MessageOutput == nil {
					continue
				}
				msg, getErr := event.Output.MessageOutput.GetMessage()
				require.NoError(t, getErr)
				if msg != nil {
					final = msg
				}
			}
			require.NotNil(t, final)
			require.Equal(t, "final answer text", stage0AgenticMessageText(final))
		})
	}

	requestMu.Lock()
	recorded := append([]recordedRequest(nil), requests...)
	requestMu.Unlock()
	require.Len(t, recorded, 2, "each surface runner must make exactly one no-tool final-answer call")

	wantSystemPrompts := map[string]bool{
		agentInstruction(basePrompt, SurfaceHTTP, nil):  false,
		agentInstruction(basePrompt, SurfaceSlack, nil): false,
	}
	for _, req := range recorded {
		input, ok := req.Body["input"].([]any)
		require.True(t, ok, "request body must carry an input item list")
		require.NotEmpty(t, input, "request must carry at least the system prompt")
		first, ok := input[0].(map[string]any)
		require.True(t, ok, "first input item must be an object")
		require.Equal(t, "system", first["role"])
		content, ok := first["content"].(string)
		require.True(t, ok, "system item content must be a string")
		require.Contains(t, wantSystemPrompts, content, "first input item must be the exact fixed system prompt")
		wantSystemPrompts[content] = true
	}
	for prompt, seen := range wantSystemPrompts {
		require.True(t, seen, "surface prompt must appear as the first input item: %q", prompt)
	}
	httpSys := agentInstruction(basePrompt, SurfaceHTTP, nil)
	slackSys := agentInstruction(basePrompt, SurfaceSlack, nil)
	require.NotEqual(t, httpSys, slackSys, "the two surface prompts must stay distinct")
}

func requireFinalResponseFormat(t *testing.T, body map[string]any) {
	t.Helper()
	textConfig, ok := body["text"].(map[string]any)
	require.True(t, ok, "root service requests must carry a text configuration")
	format, ok := textConfig["format"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "json_schema", format["type"])
	require.Equal(t, finalResponseSchemaName, format["name"])
	require.Equal(t, true, format["strict"])
	schemaValue, ok := format["schema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, schemaValue["additionalProperties"])
}

func TestAnswerDisciplinePinsFreshVerbatimConfiguration(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"Give the form requested",
		"Reproduce a command or a configuration only as it appears, whole, in a tool result of this conversation",
		"Never assemble one from fragments of different results",
	} {
		require.Contains(t, AnswerDisciplineInstructions(), want)
	}
	// Freshness is an evidence rule, stated with the others in the base prompt.
	require.Contains(t, DefaultPrompt(), "look for the most recent evidence and say how recent it is")
}

// TestAgentInstructionAppendsObservabilityDelegationOnlyWhenToolPresent pins
// the root delegation contract: the addendum telling the root about
// investigate_observability must appear exactly when that tool is present in
// the agent's tool list, since a deploy without the observability MCP
// attached must not tell the model it can delegate to a tool that doesn't
// exist.
func TestAgentInstructionAppendsObservabilityDelegationOnlyWhenToolPresent(t *testing.T) {
	t.Parallel()

	const basePrompt = "base prompt"
	plain := newAskAgentsPinnedTool(t)
	investigate, err := utils.InferTool(InvestigateToolName, "delegate to the child investigator", func(context.Context, testToolInput) (string, error) {
		return "", nil
	})
	require.NoError(t, err)

	without := agentInstruction(basePrompt, SurfaceHTTP, []tool.BaseTool{plain})
	require.NotContains(t, without, observabilityDelegationAddendum,
		"the addendum must not appear when investigate_observability is absent")

	with := agentInstruction(basePrompt, SurfaceHTTP, []tool.BaseTool{plain, investigate})
	require.Contains(t, with, observabilityDelegationAddendum)
	require.True(t, strings.HasSuffix(with, terminalOutputAddendum),
		"the terminal contract is appended last so it supersedes stale stored prompts")

	require.False(t, toolsIncludeInvestigate(nil))
	require.False(t, toolsIncludeInvestigate([]tool.BaseTool{nil, plain}), "nil and non-matching tools must not false-positive")
	require.True(t, toolsIncludeInvestigate([]tool.BaseTool{plain, investigate}))
}

func TestObservabilityDelegationInstructionsExposesAddendum(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"investigate_observability",
		"Its description lists the systems it can reach",
		"URL or ID, copied exactly",
		"It sees nothing of this conversation",
		"compare recorded changes with the time the symptom began",
		"do not query the same window yourself in parallel",
		"time bounds, filters, links and whether you read every page",
		"so the investigator checks only what is missing",
		"naming them in `artifacts`",
	} {
		require.Contains(t, ObservabilityDelegationInstructions(), want)
	}
}

func TestTerminalOutputInstructionsSupersedeStoredRespondPrompt(t *testing.T) {
	instruction := agentInstruction("Always call respond.", SurfaceHTTP, nil)
	require.True(t, strings.HasSuffix(instruction, TerminalOutputInstructions()))
	require.Contains(t, TerminalOutputInstructions(), "a `respond` tool delivering the answer. That tool no longer exists")
	require.Contains(t, TerminalOutputInstructions(), "in a fixed structure the API enforces")
}

// newAskAgentsTestModel builds a real Responses model pointed at the given
// server URL (or a defunct URL when serverURL is empty, which is only ever
// used for validation tests that never run the runners).
func newAskAgentsTestModel(t *testing.T, serverURL string) *agenticopenai.ResponsesModel {
	t.Helper()
	if serverURL == "" {
		serverURL = "http://127.0.0.1:1"
	}
	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: serverURL,
		APIKey:  "test",
		ModelID: "test",
		// A generous timeout and disabled retries keep scripted-server tests
		// deterministic: under full-suite parallel load a 5s localhost round
		// trip is not guaranteed, and a transparent retry would consume the
		// next script step and shift every subsequent assertion.
		Timeout:    time.Minute,
		MaxRetries: NoResponsesRetries,
	})
	require.NoError(t, err)
	return mdl
}

// writeAskAgentsFixture writes a minimal completed final-answer Responses
// fixture, mirroring the shape of the existing responsesUsage fixtures.
func writeAskAgentsFixture(t *testing.T, w http.ResponseWriter, id, text string) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"id": id, "object": "response", "created_at": 1,
		"status": "completed", "model": "test",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_final", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"usage": responsesUsage(1, 1, 0),
	}))
}

func writeRespondFixture(t *testing.T, w http.ResponseWriter, id, answer string) {
	t.Helper()
	writeAskAgentsFixture(t, w, id, mustJSON(t, FinalResponse{
		Answer:    answer,
		Artifacts: []ArtifactSelection{},
	}))
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}
