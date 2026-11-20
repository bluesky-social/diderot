package ask

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

func expiredChildState() *investigateChildState {
	return newInvestigateChildState(time.Now().Add(-time.Second), time.Now)
}

func TestInvestigateReportMiddlewareForcesReportAfterSoftDeadline(t *testing.T) {
	t.Parallel()

	child := expiredChildState()
	ctx := withInvestigateChildState(context.Background(), child)
	toolInfo := &schema.ToolInfo{Name: "grafana_query_prometheus", Desc: "metrics"}
	deferred := &schema.ToolInfo{Name: "tempo_get-trace", Desc: "trace"}
	original := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages:          []*schema.AgenticMessage{NewAgenticUserText("investigate"), newAgenticToolResult("call-1", "grafana_query_prometheus", `{"ok":true}`)},
		ToolInfos:         []*schema.ToolInfo{toolInfo},
		DeferredToolInfos: []*schema.ToolInfo{deferred},
	}
	middleware := newInvestigateReportMiddleware()

	_, forced, err := middleware.BeforeModelRewriteState(ctx, original, nil)
	require.NoError(t, err)
	require.NotSame(t, original, forced)
	require.Len(t, original.Messages, 2, "input state must not mutate")
	require.Len(t, forced.Messages, 3)
	require.Equal(t, childForcedReportInstr, AgenticMessageText(forced.Messages[2]))
	require.Len(t, forced.Messages[1].ContentBlocks, 1, "tool-result message stays protocol-pure")
	require.Equal(t, []*schema.ToolInfo{toolInfo}, forced.ToolInfos, "declared schemas survive for history validity")
	require.Equal(t, []*schema.ToolInfo{deferred}, forced.DeferredToolInfos)
	require.True(t, child.forcedReportStarted())

	// A second pass must not append the instruction twice.
	_, second, err := middleware.BeforeModelRewriteState(ctx, forced, nil)
	require.NoError(t, err)
	require.Same(t, forced, second)
}

func TestInvestigateReportMiddlewareSkipsForceWithoutEvidence(t *testing.T) {
	t.Parallel()

	child := expiredChildState()
	ctx := withInvestigateChildState(context.Background(), child)
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages: []*schema.AgenticMessage{NewAgenticUserText("investigate")},
	}
	_, got, err := newInvestigateReportMiddleware().BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.Same(t, state, got, "the first call has nothing to summarize; the hard deadline still bounds it")
	require.False(t, child.forcedReportStarted())

	// Before the soft deadline nothing is rewritten either.
	live := newInvestigateChildState(time.Now().Add(time.Hour), time.Now)
	liveCtx := withInvestigateChildState(context.Background(), live)
	evidence := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages: []*schema.AgenticMessage{newAgenticToolResult("call-1", "grafana_query_prometheus", `{}`)},
	}
	_, got, err = newInvestigateReportMiddleware().BeforeModelRewriteState(liveCtx, evidence, nil)
	require.NoError(t, err)
	require.Same(t, evidence, got)
	require.False(t, live.forcedReportStarted())
}

func TestInvestigateReportMiddlewareCapturesTerminalTextAndCountsRounds(t *testing.T) {
	t.Parallel()

	child := newInvestigateChildState(time.Now().Add(time.Hour), time.Now)
	ctx := withInvestigateChildState(context.Background(), child)
	middleware := newInvestigateReportMiddleware()

	toolRound := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages: []*schema.AgenticMessage{assistantCallMsg(call("call-1", "grafana_query_prometheus"))},
	}
	_, got, err := middleware.AfterModelRewriteState(ctx, toolRound, nil)
	require.NoError(t, err)
	require.Same(t, toolRound, got)
	require.Equal(t, 1, child.toolRoundCount())
	require.Empty(t, child.reportText())

	report := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages: []*schema.AgenticMessage{NewAgenticAssistantText("## Observations\nthe series was flat")},
	}
	_, got, err = middleware.AfterModelRewriteState(ctx, report, nil)
	require.NoError(t, err)
	require.Same(t, report, got)
	require.Equal(t, "## Observations\nthe series was flat", child.reportText())
	require.Equal(t, 1, child.toolRoundCount())
}

func TestInvestigateReportMiddlewareReplacesForcedToolCallsWithPartialReport(t *testing.T) {
	t.Parallel()

	child := expiredChildState()
	ctx := withInvestigateChildState(context.Background(), child)
	middleware := newInvestigateReportMiddleware()
	boundary := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages: []*schema.AgenticMessage{newAgenticToolResult("call-1", "grafana_query_prometheus", `{}`)},
	}
	_, _, err := middleware.BeforeModelRewriteState(ctx, boundary, nil)
	require.NoError(t, err)

	stillCalling := assistantCallMsg(call("call-2", "grafana_query_loki_logs"))
	stillCalling.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{
		PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34,
	}}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{stillCalling}}
	_, rewritten, err := middleware.AfterModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.NotSame(t, state, rewritten)
	require.Len(t, AgenticMessageToolCalls(state.Messages[0]), 1, "model output must not mutate")
	require.Empty(t, AgenticMessageToolCalls(rewritten.Messages[0]))
	require.Equal(t, childPartialReportText, AgenticMessageText(rewritten.Messages[0]))
	require.Equal(t, childPartialReportText, child.reportText())
	require.Equal(t, &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34}, AgenticMessageUsage(rewritten.Messages[0]))
	require.NoError(t, ValidateAgenticMessage(rewritten.Messages[0]))
}

func TestAppendForcedReportToolChoiceOnlyAfterForcing(t *testing.T) {
	t.Parallel()

	require.Empty(t, appendForcedReportToolChoice(context.Background(), nil),
		"a context with no child state must not restrict tool calls")

	child := expiredChildState()
	ctx := withInvestigateChildState(context.Background(), child)
	require.Empty(t, appendForcedReportToolChoice(ctx, nil))
	require.True(t, child.beginForcedReport())
	require.Len(t, appendForcedReportToolChoice(ctx, nil), 1)
}

func TestNewInvestigateChildRunnerValidation(t *testing.T) {
	t.Parallel()

	research, err := utils.InferTool("grafana_query_prometheus", "metrics", func(context.Context, struct{}) (string, error) {
		return "", nil
	})
	require.NoError(t, err)
	mdl := newAskAgentsTestModel(t, "")

	cases := []struct {
		name string
		cfg  investigateChildConfig
		want string
	}{
		{name: "model", cfg: investigateChildConfig{instruction: "go", tools: []tool.BaseTool{research}}, want: "model is required"},
		{name: "instruction", cfg: investigateChildConfig{model: mdl, tools: []tool.BaseTool{research}}, want: "instruction is required"},
		{name: "tools", cfg: investigateChildConfig{model: mdl, instruction: "go"}, want: "research tool is required"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			runner, err := newInvestigateChildRunner(context.Background(), tt.cfg)
			require.Nil(t, runner)
			require.ErrorContains(t, err, tt.want)
		})
	}

	runner, err := newInvestigateChildRunner(context.Background(), investigateChildConfig{
		model: mdl, instruction: "go", tools: []tool.BaseTool{research},
	})
	require.NoError(t, err)
	require.NotNil(t, runner)
}
