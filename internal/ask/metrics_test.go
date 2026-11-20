package ask

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	componenttool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestMetricsEinoModelCallbacks(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	handler := metrics.CallbackHandler(AskRunSurfaceSlack, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}

	ctx := handler.OnStart(context.Background(), info, &model.AgenticCallbackInput{})
	msg := assistantCallMsg(schema.FunctionToolCall{CallID: "call-1", Name: "retrieve", Arguments: `{}`})
	handler.OnEnd(ctx, info, &model.AgenticCallbackOutput{
		Message: msg,
		TokenUsage: &model.TokenUsage{
			PromptTokens:     20,
			CompletionTokens: 5,
			PromptTokenDetails: model.PromptTokenDetails{
				CachedTokens: 12,
			},
		},
	})

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("slack", "ok")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRounds.WithLabelValues("slack", "true")))
	require.Equal(t, float64(20), testutil.ToFloat64(metrics.tokens.WithLabelValues("slack", "input")))
	require.Equal(t, float64(12), testutil.ToFloat64(metrics.tokens.WithLabelValues("slack", "cached_input")))
	require.Equal(t, float64(5), testutil.ToFloat64(metrics.tokens.WithLabelValues("slack", "output")))
	require.Equal(t, 1, testutil.CollectAndCount(metrics.modelRequestDuration))
}

func TestMetricsFallsBackToAgenticMessageUsage(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}
	msg := NewAgenticAssistantText("done")
	msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{
		PromptTokens:     7,
		CompletionTokens: 3,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 4,
		},
	}}

	ctx := handler.OnStart(context.Background(), info, []*schema.AgenticMessage{})
	handler.OnEnd(ctx, info, msg)

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRounds.WithLabelValues("http", "false")))
	require.Equal(t, float64(7), testutil.ToFloat64(metrics.tokens.WithLabelValues("http", "input")))
	require.Equal(t, float64(4), testutil.ToFloat64(metrics.tokens.WithLabelValues("http", "cached_input")))
	require.Equal(t, float64(3), testutil.ToFloat64(metrics.tokens.WithLabelValues("http", "output")))
}

func TestMetricsToolCallbacksBoundLabelsAndClassifyErrors(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceScheduled, map[string]ToolPresentation{
		"retrieve": {},
	})

	registered := &callbacks.RunInfo{Name: "retrieve", Component: components.ComponentOfTool}
	ctx := handler.OnStart(context.Background(), registered, &componenttool.CallbackInput{})
	handler.OnEnd(ctx, registered, &componenttool.CallbackOutput{Response: `{"error":"dependency unavailable"}`})

	unknown := &callbacks.RunInfo{Name: "model_invented_name", Component: components.ComponentOfTool}
	ctx = handler.OnStart(context.Background(), unknown, &componenttool.CallbackInput{})
	handler.OnError(ctx, unknown, errors.New("unknown tool"))

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.toolCalls.WithLabelValues("scheduled", "retrieve", "error")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.toolCalls.WithLabelValues("scheduled", unknownToolMetricLabel, "error")))
	require.Equal(t, 2, testutil.CollectAndCount(metrics.toolCallDuration))
}

func TestMetricsErrorWithoutStartSkipsDurationSample(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}

	handler.OnError(context.Background(), info, errors.New("rejected before start"))

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "error")))
	require.Equal(t, 0, testutil.CollectAndCount(metrics.modelRequestDuration))
}

func TestMetricsUnrecognizedToolOutputIsNotSuccess(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, map[string]ToolPresentation{"retrieve": {}})
	info := &callbacks.RunInfo{Name: "retrieve", Component: components.ComponentOfTool}

	ctx := handler.OnStart(context.Background(), info, &componenttool.CallbackInput{})
	handler.OnEnd(ctx, info, struct{ Unexpected string }{Unexpected: "shape"})

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.toolCalls.WithLabelValues("http", "retrieve", "unknown")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.toolCalls.WithLabelValues("http", "retrieve", "ok")))
}

func TestMetricsUnrecognizedModelOutputIsNotSuccess(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}

	ctx := handler.OnStart(context.Background(), info, &model.AgenticCallbackInput{})
	handler.OnEnd(ctx, info, struct{ Unexpected string }{Unexpected: "shape"})

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "unknown")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "ok")))
}

func TestMetricsNilModelMessageIsNotSuccess(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}

	ctx := handler.OnStart(context.Background(), info, &model.AgenticCallbackInput{})
	handler.OnEnd(ctx, info, &model.AgenticCallbackOutput{})

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "unknown")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "ok")))
	require.Equal(t, 0, testutil.CollectAndCount(metrics.modelRounds))
}

func TestMetricsApplicationOutcomesAndDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := NewMetrics(reg)
	second := NewMetrics(reg)

	second.observeTurn(AskRunSurfaceHTTP, "answered", time.Millisecond)
	second.observeForcedSynthesis(AskRunSurfaceHTTP, "fallback")
	second.observePersistenceFailure(AskRunSurfaceHTTP, "final_answer")

	require.Equal(t, float64(1), testutil.ToFloat64(first.turns.WithLabelValues("http", "answered")))
	require.Equal(t, float64(1), testutil.ToFloat64(first.forcedSynthesis.WithLabelValues("http", "fallback")))
	require.Equal(t, float64(1), testutil.ToFloat64(first.persistenceFailures.WithLabelValues("http", "final_answer")))
}

func TestAskServiceEmitsMetricsThroughRealEinoRun(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_metrics", "measured answer")
	}))
	defer server.Close()

	metrics := NewMetrics(prometheus.NewRegistry())
	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	svc.metrics = metrics

	result, err := executeHTTPTestTurn(context.Background(), svc, sessionID, "measure this")
	require.NoError(t, err)
	require.Equal(t, "measured answer", result.Reply)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.turns.WithLabelValues("http", "answered")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "ok")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRounds.WithLabelValues("http", "false")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.tokens.WithLabelValues("http", "input")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.tokens.WithLabelValues("http", "output")))
	require.Equal(t, 1, testutil.CollectAndCount(metrics.modelRequestDuration))
}

func TestNilMetricsCallbackIsSafe(t *testing.T) {
	var metrics *Metrics
	handler := metrics.CallbackHandler(AskRunSurfaceHTTP, nil)
	info := &callbacks.RunInfo{Component: components.ComponentOfAgenticModel}
	ctx := handler.OnStart(context.Background(), info, nil)
	require.NotPanics(t, func() {
		handler.OnError(ctx, info, errors.New("boom"))
	})
}
