package ask

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestForcedReportToolChoiceThroughGateway is the one assertion in this
// package that can prove the forced-report boundary actually holds: an
// httptest fake only echoes what this file tells it to, so it can never
// discover that the model gateway's Responses-to-Anthropic converter rejects
// tool_choice: "none" or silently drops it. The offline tests cover the
// middleware mechanics; this covers the wire contract they depend on.
//
// Skipped unless a provider is named in the environment, so a plain
// `go test ./internal/ask/...` never reaches the network:
//
//	DIDEROT_TEST_PROVIDER_URL         base URL of an OpenAI-compatible provider or gateway, with any /v1
//	DIDEROT_TEST_PROVIDER_KEY         its API key
//	DIDEROT_TEST_FORCED_REPORT_MODEL  a model it serves on the Responses protocol
//
//	go test -v -count=1 ./internal/ask -run TestForcedReportToolChoiceThroughGateway
//
// The bounded partial-report fallback in investigateReportMiddleware exists
// precisely because this option might not be honored, so a failure here is a
// tuning signal, not a correctness break.
func TestForcedReportToolChoiceThroughGateway(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_URL"))
	apiKey := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_KEY"))
	modelID := strings.TrimSpace(os.Getenv("DIDEROT_TEST_FORCED_REPORT_MODEL"))
	if baseURL == "" || apiKey == "" || modelID == "" {
		t.Skip("set DIDEROT_TEST_PROVIDER_URL, DIDEROT_TEST_PROVIDER_KEY and DIDEROT_TEST_FORCED_REPORT_MODEL to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mdl, err := NewResponsesModel(ctx, ResponsesConfig{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		ModelID:    modelID,
		Timeout:    90 * time.Second,
		MaxTokens:  1024,
		MaxRetries: 1,
	})
	require.NoError(t, err)

	js, err := parseToolJSONSchema(json.RawMessage(`{
	  "type": "object",
	  "additionalProperties": false,
	  "properties": {"expr": {"type": "string", "description": "PromQL expression."}},
	  "required": ["expr"]
	}`))
	require.NoError(t, err)
	toolInfo := &schema.ToolInfo{
		Name:        "grafana_query_prometheus",
		Desc:        "Run a PromQL query against the Prometheus datasource.",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}

	// Exactly the history shape investigateReportMiddleware produces: a
	// research round, its complete result batch, then the forced-report
	// instruction as its own user message.
	history := []*schema.AgenticMessage{
		NewAgenticUserText("Investigate whether the uploader service error rate rose in the last hour. " + investigateChildReportContract),
		assistantCallMsg(schema.FunctionToolCall{
			CallID: "call_forced_report_probe", Name: toolInfo.Name,
			Arguments: `{"expr":"sum(rate(http_requests_total{job=\"uploader\",code=~\"5..\"}[5m]))"}`,
		}),
		newAgenticToolResult("call_forced_report_probe", toolInfo.Name, `{"result":[{"metric":{"job":"uploader"},"values":[[1786000000,"0.4"]]}]}`),
		NewAgenticUserText(childForcedReportInstr),
	}

	out, err := mdl.Generate(ctx, history,
		model.WithTools([]*schema.ToolInfo{toolInfo}),
		agenticopenai.WithExtraFields(map[string]any{"tool_choice": "none"}),
		agenticopenai.WithCustomHeaders(map[string]string{
			"x-agent-parent-session-id": uuid.NewString(),
		}),
	)
	require.NoError(t, err, "the gateway must accept tool_choice: \"none\" beside declared tools")
	require.NotNil(t, out)
	require.Empty(t, AgenticMessageToolCalls(out), "tool_choice: \"none\" must suppress further research calls")
	require.NotEmpty(t, strings.TrimSpace(AgenticMessageText(out)), "the forced round must return report text")
}
