package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// responsesStep writes one scripted model response. The Nth request to the
// fake gateway is answered by the Nth step; a request past the script fails
// the test, which is how "the child must not make another call" is asserted.
type responsesStep func(*testing.T, http.ResponseWriter)

func newScriptedResponsesServer(t *testing.T, steps ...responsesStep) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "undecodable request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		index := len(bodies) - 1
		mu.Unlock()
		if index >= len(steps) {
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		steps[index](t, w)
	}))
	t.Cleanup(server.Close)
	return server, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

func newScriptedResponsesServerWithHeaders(t *testing.T, steps ...responsesStep) (*httptest.Server, func() []map[string]any, func() []http.Header) {
	t.Helper()
	var (
		mu      sync.Mutex
		bodies  []map[string]any
		headers []http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "undecodable request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		index := len(bodies) - 1
		mu.Unlock()
		if index >= len(steps) {
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		steps[index](t, w)
	}))
	t.Cleanup(server.Close)
	return server, func() []map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return append([]map[string]any(nil), bodies...)
		}, func() []http.Header {
			mu.Lock()
			defer mu.Unlock()
			return append([]http.Header(nil), headers...)
		}
}

func toolCallStep(callID, name, arguments string) responsesStep {
	return func(t *testing.T, w http.ResponseWriter) {
		writeResponsesFixture(t, w, map[string]any{
			"id": "resp_" + callID, "object": "response", "created_at": 1,
			"status": "completed", "model": "test", "parallel_tool_calls": true,
			"output": []any{map[string]any{
				"type": "function_call", "id": "fc_" + callID, "call_id": callID,
				"name": name, "arguments": arguments, "status": "completed",
			}},
			"usage": responsesUsage(5, 2, 0),
		})
	}
}

func textStep(id, text string) responsesStep {
	return func(t *testing.T, w http.ResponseWriter) {
		writeAskAgentsFixture(t, w, id, text)
	}
}

func fakeResearchTool(t *testing.T, name string, run func(context.Context) (string, error)) RegisteredTool {
	t.Helper()
	base, err := utils.InferTool(name, "fake "+name, func(ctx context.Context, _ struct{}) (string, error) {
		return run(ctx)
	})
	require.NoError(t, err)
	return RegisteredTool{
		Tool:         base,
		Presentation: ToolPresentation{Status: func(json.RawMessage) StatusLine { return StatusLine{Emoji: "mag", Verb: "querying"} }},
	}
}

const investigateReportFixture = "## Observations\ngrafana_query_prometheus over 2026-08-13T00:00Z..01:00Z showed a flat series\n\n## Inference\nno regression\n\n## Remaining uncertainty\nlogs not checked\n\n## Artifacts\nnone"

type investigateHarness struct {
	tool    RegisteredTool
	metrics *Metrics
	bodies  func() []map[string]any
}

func newInvestigateHarness(t *testing.T, tools []RegisteredTool, tune func(*InvestigateToolOptions), steps ...responsesStep) *investigateHarness {
	t.Helper()
	server, bodies := newScriptedResponsesServer(t, steps...)
	metrics := NewMetrics(prometheus.NewRegistry())
	opts := InvestigateToolOptions{
		Model:   newAskAgentsTestModel(t, server.URL+"/v1"),
		Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, tools...)},
		Metrics: metrics,
	}
	if tune != nil {
		tune(&opts)
	}
	registered, err := NewEinoInvestigateTool(opts)
	require.NoError(t, err)
	return &investigateHarness{tool: registered, metrics: metrics, bodies: bodies}
}

func (h *investigateHarness) invoke(ctx context.Context, t *testing.T, arguments string) (string, error) {
	t.Helper()
	invokable, ok := h.tool.Tool.(tool.InvokableTool)
	require.True(t, ok, "the delegation tool must be invokable")
	return invokable.InvokableRun(ctx, arguments)
}

func (h *investigateHarness) outcome(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(h.metrics.investigateRuns.WithLabelValues(outcome))
}

func requestToolNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["tools"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		declared, ok := entry.(map[string]any)
		require.True(t, ok)
		name, _ := declared["name"].(string)
		names = append(names, name)
	}
	return names
}

func requestSystemPrompt(t *testing.T, body map[string]any) string {
	t.Helper()
	input, ok := body["input"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, input)
	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "system", first["role"])
	content, ok := first["content"].(string)
	require.True(t, ok)
	return content
}

func requestContainsText(t *testing.T, body map[string]any, want string) bool {
	t.Helper()
	input, ok := body["input"].([]any)
	require.True(t, ok)
	return requestInputContainsText(input, want)
}

func TestInvestigateToolRunsChildAndReturnsReport(t *testing.T) {
	t.Parallel()

	harness := newInvestigateHarness(t,
		[]RegisteredTool{
			fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{"series":[]}`, nil }),
			fakeResearchTool(t, "grafana_query_loki_logs", func(context.Context) (string, error) { return `{"lines":[]}`, nil }),
		}, nil,
		toolCallStep("call_metrics", "grafana_query_prometheus", `{}`),
		toolCallStep("call_logs", "grafana_query_loki_logs", `{}`),
		textStep("resp_report", investigateReportFixture),
	)

	report, err := harness.invoke(context.Background(), t, `{"request":"why did uploader upload error rate rise at 01:00 UTC","time_window_utc":{"from":"2026-08-13T00:00:00Z","to":"2026-08-13T02:00:00Z"}}`)
	require.NoError(t, err)
	require.Equal(t, investigateReportFixture, report)

	bodies := harness.bodies()
	require.Len(t, bodies, 3, "the child ran two research rounds and one report round")

	names := requestToolNames(t, bodies[0])
	require.ElementsMatch(t, []string{"grafana_query_prometheus", "grafana_query_loki_logs"}, names)
	for _, forbidden := range []string{InvestigateToolName, "retrieve", "list", "expand", "code_search", "web_search"} {
		require.NotContains(t, names, forbidden)
	}

	instruction := requestSystemPrompt(t, bodies[0])
	require.Contains(t, instruction, "## Observations")
	require.Contains(t, instruction, "## How to investigate")
	require.Contains(t, instruction, "## Investigating an incident")
	require.NotContains(t, instruction, "respond tool", "the child has no terminal answer tool to mention")
	require.NotContains(t, instruction, "four research tool rounds")
	require.True(t, requestContainsText(t, bodies[0], `why did uploader upload error rate rise at 01:00 UTC

Explicit UTC time window: from "2026-08-13T00:00:00Z" to "2026-08-13T02:00:00Z".`))

	require.Equal(t, float64(1), harness.outcome(t, "reported"))
	require.Equal(t, float64(2), testutil.ToFloat64(harness.metrics.investigateToolRounds))
	require.Equal(t, 1, testutil.CollectAndCount(harness.metrics.investigateDuration))
	require.Equal(t, 1, testutil.CollectAndCount(harness.metrics.investigateReportBytes))
}

// TestInvestigateToolLeavesRootRunStateUntouched is the load-bearing isolation
// property: the child inherits the turn context, so the root's recorder and
// tool budget must come back exactly as they went in — including through the
// unknown-tool path, whose recording variant would write into the recorder.
func TestInvestigateToolLeavesRootRunStateUntouched(t *testing.T) {
	t.Parallel()

	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{}`, nil })}, nil,
		toolCallStep("call_metrics", "grafana_query_prometheus", `{}`),
		toolCallStep("call_ghost", "grafana_not_a_registered_tool", `{}`),
		textStep("resp_report", investigateReportFixture),
	)

	recorder, appender, _ := newRecorderTest(t, 4)
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	ctx := withAgenticRunRecorder(context.Background(), recorder)
	ctx = withAgenticToolBudget(ctx, budget)

	report, err := harness.invoke(ctx, t, `{"request":"check the uploader error rate"}`)
	require.NoError(t, err)
	require.Equal(t, investigateReportFixture, report)

	attempts, success := appender.snapshot()
	require.Empty(t, attempts, "a child run must never write through the root recorder")
	require.Empty(t, success)
	require.Nil(t, recorder.PendingAssistantSnapshot(), "the root's pending assistant boundary must stay empty")
	require.Equal(t, 4, recorder.NextTurnToCommit())
	require.False(t, recorder.Finished())

	completed, maxRounds, forced := budget.state()
	require.Equal(t, 0, completed, "child rounds must not spend the root tool budget")
	require.Equal(t, 1, maxRounds)
	require.False(t, forced)
}

// softDeadlineFlipClock makes the forced-report boundary deterministic. A
// real 1ms-style soft deadline is a coin flip: a warm loopback model round can
// finish in well under a millisecond, so whether the second round gets forced
// depends on machine load rather than the code under test. Instead the tool's
// injected clock reads as "before the soft deadline" until flip is called
// (from the first scripted response), and "long past it" afterwards — the
// first model round is always unrestricted and the second is always forced,
// on any machine. The child's hard context deadline still uses the real
// clock, so Grace stays generous to keep it out of the picture.
func softDeadlineFlipClock() (now func() time.Time, flip func()) {
	anchor := time.Now()
	var flipped atomic.Bool
	return func() time.Time {
			if flipped.Load() {
				return anchor.Add(time.Hour)
			}
			return anchor
		}, func() {
			flipped.Store(true)
		}
}

// flipThen serves the wrapped step and then flips the clock, so every model
// call scripted after this one observes a passed soft deadline.
func flipThen(flip func(), step responsesStep) responsesStep {
	return func(t *testing.T, w http.ResponseWriter) {
		step(t, w)
		flip()
	}
}

func TestInvestigateToolForcedReportAtSoftDeadline(t *testing.T) {
	t.Parallel()

	tune := func(now func() time.Time) func(*InvestigateToolOptions) {
		return func(opts *InvestigateToolOptions) {
			opts.SoftDeadline = time.Minute
			opts.Grace = 30 * time.Second
			opts.Now = now
		}
	}

	t.Run("model reports", func(t *testing.T) {
		t.Parallel()
		now, flip := softDeadlineFlipClock()
		harness := newInvestigateHarness(t,
			[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{}`, nil })}, tune(now),
			flipThen(flip, toolCallStep("call_metrics", "grafana_query_prometheus", `{}`)),
			textStep("resp_report", investigateReportFixture),
		)
		report, err := harness.invoke(context.Background(), t, `{"request":"check the uploader error rate"}`)
		require.NoError(t, err)
		require.Equal(t, investigateReportFixture, report)

		bodies := harness.bodies()
		require.Len(t, bodies, 2)
		require.Nil(t, bodies[0]["tool_choice"], "the first round is unrestricted")
		require.True(t, requestContainsText(t, bodies[1], childForcedReportInstr))
		require.Equal(t, "none", bodies[1]["tool_choice"], "the forced round forbids further tool calls")
		require.NotEmpty(t, requestToolNames(t, bodies[1]), "declared schemas stay for history validity")
		require.Equal(t, float64(1), harness.outcome(t, "forced_report"))
	})

	t.Run("model keeps calling tools", func(t *testing.T) {
		t.Parallel()
		now, flip := softDeadlineFlipClock()
		harness := newInvestigateHarness(t,
			[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{}`, nil })}, tune(now),
			flipThen(flip, toolCallStep("call_metrics", "grafana_query_prometheus", `{}`)),
			toolCallStep("call_more", "grafana_query_prometheus", `{}`),
		)
		report, err := harness.invoke(context.Background(), t, `{"request":"check the uploader error rate"}`)
		require.NoError(t, err)
		require.Equal(t, childPartialReportText, report,
			"correctness must not depend on the gateway honoring tool_choice")
		require.Len(t, harness.bodies(), 2, "the forced round is terminal even when the model ignores it")
		require.Equal(t, float64(1), harness.outcome(t, "forced_report"))
	})
}

func TestInvestigateToolHardDeadlineIsRecoverable(t *testing.T) {
	t.Parallel()

	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})},
		func(opts *InvestigateToolOptions) {
			opts.SoftDeadline = 100 * time.Millisecond
			opts.Grace = 100 * time.Millisecond
		},
		toolCallStep("call_metrics", "grafana_query_prometheus", `{}`),
	)

	ctx := context.Background()
	report, err := harness.invoke(ctx, t, `{"request":"check the uploader error rate"}`)
	require.Empty(t, report)
	require.ErrorContains(t, err, "investigation deadline expired")
	// The tool error policy aborts the whole turn on a deadline sentinel, so
	// this error must not carry one: the root can still answer without it.
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	require.False(t, errors.Is(err, context.Canceled))
	require.NoError(t, ctx.Err(), "the parent turn stays live")
	require.Equal(t, float64(1), harness.outcome(t, "deadline_expired"))
}

func TestInvestigateToolParentCancellationAborts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(toolCtx context.Context) (string, error) {
			close(started)
			<-toolCtx.Done()
			return "", toolCtx.Err()
		})},
		func(opts *InvestigateToolOptions) { opts.SoftDeadline = time.Minute },
		toolCallStep("call_metrics", "grafana_query_prometheus", `{}`),
	)

	go func() {
		<-started
		cancel()
	}()
	report, err := harness.invoke(ctx, t, `{"request":"check the uploader error rate"}`)
	require.Empty(t, report)
	require.ErrorIs(t, err, context.Canceled)
}

func TestInvestigateToolAdmissionRejectsShortTurns(t *testing.T) {
	t.Parallel()

	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) {
			t.Error("no research tool may run when the turn cannot afford an investigation")
			return "", nil
		})}, nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, err := harness.invoke(ctx, t, `{"request":"check the uploader error rate"}`)
	require.Empty(t, report)
	require.ErrorContains(t, err, "insufficient time remaining")
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	require.Empty(t, harness.bodies(), "no child agent may be constructed or called")
	require.Equal(t, float64(1), harness.outcome(t, "insufficient_time"))
}

func TestInvestigateToolAdmitClampsSoftDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	subject := &investigateTool{
		softDeadline:     defaultInvestigateSoftDeadline,
		grace:            defaultInvestigateGrace,
		synthesisReserve: defaultInvestigateSynthesisReserve,
		minChildBudget:   defaultInvestigateMinChildBudget,
		now:              func() time.Time { return now },
	}

	soft, err := subject.admit(context.Background(), now)
	require.NoError(t, err)
	require.Equal(t, now.Add(defaultInvestigateSoftDeadline), soft, "an unbounded turn uses the configured soft deadline")

	generous, cancel := context.WithDeadline(context.Background(), now.Add(20*time.Minute))
	defer cancel()
	soft, err = subject.admit(generous, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(defaultInvestigateSoftDeadline), soft)

	tight, cancelTight := context.WithDeadline(context.Background(), now.Add(5*time.Minute))
	defer cancelTight()
	soft, err = subject.admit(tight, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(5*time.Minute-defaultInvestigateSynthesisReserve-defaultInvestigateGrace), soft,
		"the reserve and the grace window are held back from research")

	// Exactly at the threshold the child still gets its minimum window.
	threshold := defaultInvestigateSynthesisReserve + defaultInvestigateMinChildBudget + defaultInvestigateGrace
	atThreshold, cancelAt := context.WithDeadline(context.Background(), now.Add(threshold))
	defer cancelAt()
	soft, err = subject.admit(atThreshold, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(defaultInvestigateMinChildBudget), soft)

	below, cancelBelow := context.WithDeadline(context.Background(), now.Add(threshold-time.Second))
	defer cancelBelow()
	_, err = subject.admit(below, now)
	require.ErrorContains(t, err, "insufficient time remaining")
}

func TestBuildDelegatedSteeringRoutesAndDefaultsToIncident(t *testing.T) {
	t.Parallel()

	alert := buildDelegatedSteering("which alert rules are firing for uploader right now", investigateCapabilities{observability: true})
	require.True(t, alert.Active)
	require.Equal(t, "alert", alert.Intent)
	require.Contains(t, alert.CardIDs, "workflow:alert")
	require.Contains(t, alert.Text, "## Investigating an alert")
	require.NotContains(t, alert.Text, "AVAILABLE OBSERVABILITY TOOLS", "the child holds the whole read-only toolset")

	// No router intent token appears here, but the delegation itself settles
	// the question, so the child still gets a workflow.
	plain := BuildObservabilitySteering("why did the uploader upload path start returning 500s")
	require.False(t, plain.Active, "the shared router would switch steering off for this text")
	defaulted := buildDelegatedSteering("why did the uploader upload path start returning 500s", investigateCapabilities{observability: true})
	require.True(t, defaulted.Active)
	require.Equal(t, "incident", defaulted.Intent)
	require.Equal(t, []string{"workflow:incident", "syntax:promql", "syntax:logql", "syntax:traceql"}, defaulted.CardIDs)
	require.Contains(t, defaulted.Text, "## Investigating an incident")
	require.Contains(t, defaulted.Text, "## PromQL")
	require.Contains(t, defaulted.Text, "## LogQL")
	require.Contains(t, defaulted.Text, "## TraceQL")
	require.NotEqual(t, plain.RouterHash, defaulted.RouterHash, "provenance must record the delegated routing decision")
	require.NotEqual(t, plain.BundleHash, defaulted.BundleHash)
	require.Equal(t, hashText(ObservabilityBundleVersion+"\n"+defaulted.Text), defaulted.BundleHash,
		"the bundle hash must cover the text the child was actually given")

	panels := buildDelegatedSteering("show me the uploader dashboard panel for the last hour", investigateCapabilities{observability: true})
	require.True(t, panels.PanelRequested)

	sentry := buildDelegatedSteering("inspect this Sentry issue from the last hour", investigateCapabilities{sentry: true, observability: true})
	require.True(t, sentry.Active)
	require.Equal(t, "incident", sentry.Intent, "the persisted intent remains within the existing schema enum")
	require.Equal(t, []string{"workflow:sentry"}, sentry.CardIDs)
	require.Contains(t, sentry.Text, "## Investigating in Sentry")
	require.NotContains(t, sentry.Text, "## PromQL")

	sentryOnly := buildDelegatedSteering("why did the app start failing", investigateCapabilities{sentry: true})
	require.Equal(t, []string{"workflow:sentry"}, sentryOnly.CardIDs)
	require.Contains(t, sentryOnly.Text, "do not run several equivalent Sentry searches")
}

func TestDelegatedSteeringUsesParentPromptBundle(t *testing.T) {
	bundle := promptcatalog.MustEmbedded()
	const marker = "PARENT-RELEASE-ALERT-WORKFLOW"
	for i := range bundle.Prompts {
		if bundle.Prompts[i].Key == promptcatalog.AskObservabilityAlert {
			bundle.Prompts[i].Body = marker
		}
	}
	capabilities := investigateCapabilities{observability: true}
	steering := buildDelegatedSteeringWith(bundle, "which alerts are firing", capabilities)
	require.Contains(t, steering.Text, marker)
	require.NotContains(t, steering.Text, "## Investigating an alert")
	require.Contains(t, investigateChildInstructionWith(bundle, steering, capabilities), marker)
}

func TestInvestigateToolRecordsSteeringProvenance(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	store := NewArtifactStore(pool, &memoryArtifactBlob{})
	sessionID := newAgenticSession(t, mgr)

	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{}`, nil })},
		func(opts *InvestigateToolOptions) { opts.Artifacts = store },
		textStep("resp_report", investigateReportFixture),
	)

	recorder, _, _ := newRecorderTest(t, 6)
	ctx := withArtifactSession(context.Background(), sessionID)
	ctx = withAgenticRunRecorder(ctx, recorder)
	report, err := harness.invoke(ctx, t, `{"request":"which alert rules are firing for uploader"}`)
	require.NoError(t, err)
	require.Equal(t, investigateReportFixture, report)

	var (
		intent  string
		cardIDs []string
	)
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT intent, card_ids FROM ask_observability_steering
		 WHERE session_id=$1 AND user_turn=6`, sessionID).Scan(&intent, &cardIDs))
	require.Equal(t, "alert", intent)
	require.Contains(t, cardIDs, "workflow:alert")
}

// A panel the child renders reaches the root only through the report text. That
// is the whole channel by design: there is no ambient state that promotes a
// render into an attachment behind the root model's back, so the root has to
// name the artifact in final artifacts to publish it.
func TestInvestigateToolReportsPanelArtifactIDs(t *testing.T) {
	t.Parallel()

	artifactID := uuid.New()
	panelInfo := &schema.ToolInfo{
		Name:        PanelImageToolName,
		Desc:        "render a stored panel",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(mustParseTestSchema(t, `{"type":"object","additionalProperties":false,"properties":{}}`)),
	}
	panelTool := utils.NewEnhancedTool(panelInfo, func(ctx context.Context, _ struct{}) (*schema.ToolResult, error) {
		return &schema.ToolResult{Parts: []schema.ToolOutputPart{{
			Type: schema.ToolPartTypeText,
			Text: `{"schema":"ask-panel-artifact-v1","artifact_id":"` + artifactID.String() + `"}`,
		}}}, nil
	})

	harness := newInvestigateHarness(t,
		[]RegisteredTool{{
			Tool:         panelTool,
			Presentation: ToolPresentation{Status: func(json.RawMessage) StatusLine { return StatusLine{Emoji: "bar_chart", Verb: "rendering"} }},
		}}, nil,
		toolCallStep("call_panel", PanelImageToolName, `{}`),
		textStep("resp_report", "## Observations\nrendered\n\n## Inference\nnone\n\n## Remaining uncertainty\nnone\n\n## Artifacts\n"+artifactID.String()+" Grafana panel 7"),
	)

	report, err := harness.invoke(context.Background(), t, `{"request":"show the uploader dashboard panel"}`)
	require.NoError(t, err)
	require.Contains(t, report, artifactID.String(),
		"the artifact id is the root's only handle on a panel the child rendered")
}

func TestNewEinoInvestigateToolValidation(t *testing.T) {
	t.Parallel()

	research := fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return "", nil })
	mdl := newAskAgentsTestModel(t, "")
	recursive := RegisteredTool{Tool: mustInvestigateTool(t, mdl, research).Tool, Presentation: research.Presentation}

	cases := []struct {
		name string
		opts InvestigateToolOptions
		want string
	}{
		{name: "model", opts: InvestigateToolOptions{Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, research)}}, want: "model is required"},
		{name: "tools", opts: InvestigateToolOptions{Model: mdl}, want: "at least one research tool"},
		{name: "empty server", opts: InvestigateToolOptions{Model: mdl, Servers: []*MCPToolServer{testMCPServer(MCPServerGrafana)}}, want: "at least one research tool"},
		{name: "nil tool", opts: InvestigateToolOptions{Model: mdl, Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, RegisteredTool{})}}, want: "is nil"},
		{name: "recursion", opts: InvestigateToolOptions{Model: mdl, Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, recursive)}}, want: `"investigate_observability" must not be a child research tool`},
		{name: "negative grace", opts: InvestigateToolOptions{Model: mdl, Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, research)}, Grace: -time.Second}, want: "Grace must be positive"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			registered, err := NewEinoInvestigateTool(tt.opts)
			require.ErrorContains(t, err, tt.want)
			require.Nil(t, registered.Tool)
		})
	}
}

func mustInvestigateTool(t *testing.T, mdl model.AgenticModel, research RegisteredTool) RegisteredTool {
	t.Helper()
	registered, err := NewEinoInvestigateTool(InvestigateToolOptions{Model: mdl, Servers: []*MCPToolServer{testMCPServer(MCPServerGrafana, research)}})
	require.NoError(t, err)
	return registered
}

// testMCPServer stands in for a connected server of kind serving tools.
func testMCPServer(kind MCPServerKind, tools ...RegisteredTool) *MCPToolServer {
	return &MCPToolServer{name: string(kind), kind: kind, tools: tools}
}

func TestInvestigateToolRegistersWithPresentationAndSchema(t *testing.T) {
	t.Parallel()

	research := fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return "", nil })
	registered := mustInvestigateTool(t, newAskAgentsTestModel(t, ""), research)
	tools, presentations, err := NewToolRegistry(context.Background(), []RegisteredTool{registered})
	require.NoError(t, err)
	require.Len(t, tools, 1)

	info, err := tools[0].Info(context.Background())
	require.NoError(t, err)
	require.Equal(t, InvestigateToolName, info.Name)
	require.Contains(t, info.Desc, "Grafana")
	require.NotContains(t, info.Desc, "Sentry", "the description must reflect only configured child backends")
	require.Contains(t, info.Desc, "final artifacts field")
	raw, err := json.Marshal(mustToolJSONSchema(t, info))
	require.NoError(t, err)
	require.Contains(t, string(raw), "the investigator sees nothing of this conversation")
	require.Contains(t, string(raw), "time_window_utc")

	line := presentations[InvestigateToolName].Status(json.RawMessage(`{"request":"check uploader"}`))
	require.Equal(t, "investigating live telemetry", line.Verb)
	require.Equal(t, "check uploader", line.Detail)

	// The child's research tools contribute no presentations to the root: the
	// child emits no status, so the transcript shows one investigate card rather
	// than one card per Grafana call.
	require.NotContains(t, presentations, "grafana_query_prometheus")
}

func mustParseTestSchema(t *testing.T, raw string) *jsonschema.Schema {
	t.Helper()
	js, err := parseToolJSONSchema(json.RawMessage(raw))
	require.NoError(t, err)
	return js
}

func mustToolJSONSchema(t *testing.T, info *schema.ToolInfo) any {
	t.Helper()
	converted, err := info.ToJSONSchema()
	require.NoError(t, err)
	return converted
}

func TestInvestigateToolRejectsEmptyRequest(t *testing.T) {
	t.Parallel()

	harness := newInvestigateHarness(t,
		[]RegisteredTool{fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return "", nil })}, nil)
	report, err := harness.invoke(context.Background(), t, `{"request":"   "}`)
	require.Empty(t, report)
	require.ErrorContains(t, err, "request is required")
	require.Empty(t, harness.bodies())
}

func TestInvestigateChildInstructionComposition(t *testing.T) {
	t.Parallel()

	steering := buildDelegatedSteering("which alert rules are firing for checkout", investigateCapabilities{observability: true})
	instruction := investigateChildInstruction(steering, investigateCapabilities{sentry: true, observability: true})
	require.True(t, strings.HasPrefix(instruction, investigateChildRole))
	require.Contains(t, instruction, investigateChildDiscipline)
	require.Contains(t, instruction, investigateSentryDiscipline)
	require.Contains(t, instruction, investigateGrafanaDiscipline)
	require.Contains(t, instruction, "search_issues")
	require.Contains(t, instruction, "Never say you changed anything in Sentry.")
	require.NotContains(t, instruction, "organization and project", "the connection's URL sets the scope; the prompt does not repeat it")
	require.Contains(t, instruction, "## Investigating an alert")
	require.True(t, strings.HasSuffix(instruction, investigateChildReportContract))

	sentryOnly := investigateChildInstruction(steering, investigateCapabilities{sentry: true})
	require.Contains(t, sentryOnly, investigateSentryDiscipline)
	require.NotContains(t, sentryOnly, investigateGrafanaDiscipline)
	require.NotContains(t, sentryOnly, "datasource UIDs")
	require.NotContains(t, sentryOnly, "grafana_get_panel_image")

	grafanaOnly := investigateChildInstruction(steering, investigateCapabilities{observability: true})
	require.Contains(t, grafanaOnly, investigateGrafanaDiscipline)
	require.NotContains(t, grafanaOnly, investigateSentryDiscipline)
	require.NotContains(t, grafanaOnly, "Never say you changed anything in Sentry.")
}

func TestInvestigateChildInstructionDocumentsChangeCorrelation(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name         string
		capabilities investigateCapabilities
	}{
		{name: "grafana", capabilities: investigateCapabilities{observability: true}},
		{name: "sentry", capabilities: investigateCapabilities{sentry: true}},
		{name: "both", capabilities: investigateCapabilities{observability: true, sentry: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			steering := buildDelegatedSteering("investigate the incident", testCase.capabilities)
			instruction := investigateChildInstruction(steering, testCase.capabilities)
			for _, want := range []string{
				"When a deployment-history tool is available",
				"around the time the symptom began as measured",
				"not only around the time an alert fired",
				"Include changes to dependencies",
				"Reuse the change records the request supplies",
				"Query only the windows or scopes they do not cover",
				"A change near the onset is a hypothesis",
				"A change after the onset did not cause the onset",
				"Report relevant changes with their links",
				"Say whether a lookup found nothing or failed",
				"the record may not cover every system",
				"If there is no deployment-history tool",
			} {
				require.Contains(t, instruction, want)
			}
		})
	}
}

func TestInvestigateToolDescriptionMatchesConfiguredBackends(t *testing.T) {
	t.Parallel()

	mdl := newAskAgentsTestModel(t, "")
	sentry := fakeResearchTool(t, "search_issues", func(context.Context) (string, error) { return "", nil })
	grafana := fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return "", nil })

	// A grafana server's allowlist may name tools outside the reviewed set;
	// the child's capabilities follow the server's kind, not tool names.
	custom := fakeResearchTool(t, "metrics_query", func(context.Context) (string, error) { return "", nil })

	for _, tc := range []struct {
		name     string
		servers  []*MCPToolServer
		contains []string
		excludes []string
	}{
		{name: "sentry only", servers: []*MCPToolServer{testMCPServer(MCPServerSentry, sentry)}, contains: []string{"Sentry", "Sentry issues and events"}, excludes: []string{"Grafana", "rendered panels"}},
		{name: "grafana only", servers: []*MCPToolServer{testMCPServer(MCPServerGrafana, grafana)}, contains: []string{"Grafana", "metrics, logs, traces"}, excludes: []string{"Sentry"}},
		{name: "both", servers: []*MCPToolServer{testMCPServer(MCPServerSentry, sentry), testMCPServer(MCPServerGrafana, grafana)}, contains: []string{"Sentry", "Grafana"}},
		{name: "custom grafana allowlist", servers: []*MCPToolServer{testMCPServer(MCPServerGrafana, custom)}, contains: []string{"Grafana", "metrics, logs, traces"}, excludes: []string{"Sentry"}},
		{name: "generic server", servers: []*MCPToolServer{testMCPServer(MCPServerGeneric, grafana)}, contains: []string{"configured observability"}, excludes: []string{"Grafana", "Sentry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registered, err := NewEinoInvestigateTool(InvestigateToolOptions{Model: mdl, Servers: tc.servers})
			require.NoError(t, err)
			info, err := registered.Tool.Info(t.Context())
			require.NoError(t, err)
			for _, want := range tc.contains {
				require.Contains(t, info.Desc, want)
			}
			for _, unwanted := range tc.excludes {
				require.NotContains(t, info.Desc, unwanted)
			}
		})
	}
}

// TestExecuteTurnDelegatesToInvestigateToolAndAnswersFromItsReport is the
// end-to-end-ish delegation test: it drives the full root path (AskAgents'
// real adk tool-calling loop via AskService.ExecuteTurn) rather than calling
// the investigate tool's InvokableRun directly the way the harness above
// does. It proves the delegation tool registered by NewEinoInvestigateTool
// works when the root model actually calls it as a tool, and that the
// child's own model calls happen against a separate, independent server —
// pinning that root and child never share a model round-trip.
func TestExecuteTurnDelegatesToInvestigateToolAndAnswersFromItsReport(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	childServer, childBodies, childHeaders := newScriptedResponsesServerWithHeaders(t,
		toolCallStep("call_sentry", "search_issues", `{"query":"is:unresolved"}`),
		textStep("resp_child_report", investigateReportFixture),
	)
	investigateRegistration, err := NewEinoInvestigateTool(InvestigateToolOptions{
		Model: newAskAgentsTestModel(t, childServer.URL+"/v1"),
		Servers: []*MCPToolServer{testMCPServer(MCPServerGeneric,
			fakeResearchTool(t, "grafana_query_prometheus", func(context.Context) (string, error) { return `{"series":[]}`, nil }),
			fakeResearchTool(t, "search_issues", func(context.Context) (string, error) { return `{"issues":[]}`, nil }),
		)},
	})
	require.NoError(t, err)

	const investigateArgs = `{"request":"why did uploader upload error rate rise at 01:00 UTC"}`
	rootServer, rootBodies := newScriptedResponsesServer(t,
		toolCallStep("call_investigate", InvestigateToolName, investigateArgs),
		func(t *testing.T, w http.ResponseWriter) {
			writeRespondFixture(t, w, "resp_root_final", "The rise was a flat series with no regression per the investigation.")
		},
	)
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, rootServer.URL+"/v1"),
		[]tool.BaseTool{investigateRegistration.Tool}, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Now: time.Now, LinearTeams: testLinearTeams,
		Model: registryModel(t, "gpt-5.6-sol:direct"), ServiceTier: "fast",
		Limits: func() Limits { return Limits{ReasoningEffort: "high"} },
	})
	require.NoError(t, err)

	effort, tier := "medium", "fast"
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "did uploader uploads regress?", Surface: AskRunSurfaceHTTP,
		AllowedScopes: []string{"public", "internal"},
		ModelOverrides: ModelOverrides{
			ReasoningEffort: &effort,
			ServiceTier:     &tier,
			ReasoningEffortSchedule: &ReasoningEffortSchedule{
				AfterToolRounds: 1,
				Effort:          "high",
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "The rise was a flat series with no regression per the investigation.", result.Reply)

	require.Len(t, rootBodies(), 2, "root: one call that delegates, one that synthesizes the final answer")
	require.Equal(t, "medium", rootBodies()[0]["reasoning"].(map[string]any)["effort"])
	require.Equal(t, "high", rootBodies()[1]["reasoning"].(map[string]any)["effort"])
	require.Equal(t, []string{InvestigateToolName}, requestToolNames(t, rootBodies()[0]),
		"the root sees only the bounded delegation tool, never raw Sentry search")
	require.Len(t, childBodies(), 2, "the child made its own independent search and report calls")
	for _, body := range childBodies() {
		require.Equal(t, "medium", body["reasoning"].(map[string]any)["effort"])
		require.Equal(t, "fast", body["service_tier"])
	}
	for _, headers := range childHeaders() {
		require.Equal(t, sessionID.String(), headers.Get("x-agent-parent-session-id"))
		require.Empty(t, headers.Get("x-bedrock-metadata"))
	}
	require.Contains(t, requestToolNames(t, childBodies()[0]), "search_issues")
	require.NotContains(t, requestToolNames(t, childBodies()[0]), InvestigateToolName)
	require.True(t, requestContainsText(t, childBodies()[0], "why did uploader upload error rate rise at 01:00 UTC"),
		"the child's request must carry the root's delegated question, not the root's chat history")

	runs, err := mgr.InspectRunTelemetry(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	var rootModels, childModels, rootTools, childTools int
	for _, call := range runs[0].InferenceCalls {
		if call.Workload == TelemetryWorkloadObservabilityChild {
			childModels++
		} else {
			rootModels++
		}
	}
	for _, call := range runs[0].ToolCalls {
		if call.Workload == TelemetryWorkloadObservabilityChild {
			childTools++
		} else {
			rootTools++
		}
	}
	require.Equal(t, 2, rootModels)
	require.Equal(t, 2, childModels)
	require.Equal(t, 1, rootTools)
	require.Equal(t, 1, childTools)
}
