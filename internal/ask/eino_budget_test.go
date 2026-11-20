package ask

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

func TestAgenticToolBudgetValidationAndCounting(t *testing.T) {
	budget, err := newAgenticToolBudget(0)
	require.Nil(t, budget)
	require.ErrorContains(t, err, "positive")

	budget, err = newAgenticToolBudget(2)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	completed, maxRounds, forced := budget.state()
	require.Equal(t, 1, completed)
	require.Equal(t, 2, maxRounds)
	require.False(t, forced)
	require.NoError(t, budget.recordCompletedRound())
	require.ErrorContains(t, budget.recordCompletedRound(), "exceeded")
}

func TestAgenticBudgetMiddlewarePreservesSchemasForToolFreeSynthesis(t *testing.T) {
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	resultMessage := newAgenticToolResult("call-1", "lookup", `{"ok":true}`)
	toolInfo := &schema.ToolInfo{Name: "lookup", Desc: "lookup"}
	deferred := &schema.ToolInfo{Name: "search", Desc: "search"}
	original := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages:          []*schema.AgenticMessage{NewAgenticUserText("question"), resultMessage},
		ToolInfos:         []*schema.ToolInfo{toolInfo},
		DeferredToolInfos: []*schema.ToolInfo{deferred},
	}
	middleware := newAgenticBudgetMiddleware()

	_, forced, err := middleware.BeforeModelRewriteState(ctx, original, nil)
	require.NoError(t, err)
	require.NotSame(t, original, forced)
	require.Equal(t, []*schema.ToolInfo{toolInfo}, original.ToolInfos, "input state tools must not mutate")
	require.Equal(t, []*schema.ToolInfo{deferred}, original.DeferredToolInfos)
	require.Len(t, original.Messages[1].ContentBlocks, 1, "live tool-result message must not mutate")
	require.Equal(t, []*schema.ToolInfo{toolInfo}, forced.ToolInfos)
	require.NotNil(t, forced.DeferredToolInfos)
	require.Equal(t, []*schema.ToolInfo{deferred}, forced.DeferredToolInfos)
	require.Len(t, forced.Messages, 3)
	require.Len(t, forced.Messages[1].ContentBlocks, 1, "tool-result message remains protocol-pure")
	require.Equal(t, capSynthInstr, AgenticMessageText(forced.Messages[2]))
	require.True(t, budget.forcedSynthesisStarted())

	// A defensive second hook pass keeps synthesis forced but never appends the
	// instruction twice.
	_, second, err := middleware.BeforeModelRewriteState(ctx, forced, nil)
	require.NoError(t, err)
	require.Len(t, second.Messages, 3)
	require.Len(t, second.Messages[1].ContentBlocks, 1)
	require.Equal(t, capSynthInstr, AgenticMessageText(second.Messages[2]))
}

func TestAgenticBudgetMiddlewareBelowCapIsNoop(t *testing.T) {
	budget, err := newAgenticToolBudget(2)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages:  []*schema.AgenticMessage{NewAgenticUserText("question")},
		ToolInfos: []*schema.ToolInfo{{Name: "lookup"}},
	}
	_, got, err := newAgenticBudgetMiddleware().BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.Same(t, state, got)
	require.False(t, budget.forcedSynthesisStarted())
}

func TestAgenticBudgetMiddlewareRejectsInvalidForcedBoundaryWithoutStarting(t *testing.T) {
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{NewAgenticUserText("plain user")}}

	_, got, err := newAgenticBudgetMiddleware().BeforeModelRewriteState(ctx, state, nil)
	require.Same(t, state, got)
	require.ErrorContains(t, err, "trailing tool results")
	require.False(t, budget.forcedSynthesisStarted())
}

func TestAgenticBudgetMiddlewareRewritesForcedToolCallForRecorder(t *testing.T) {
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	middleware := newAgenticBudgetMiddleware()
	boundary := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		newAgenticToolResult("call-1", "lookup", `{}`),
	}}
	_, _, err = middleware.BeforeModelRewriteState(ctx, boundary, nil)
	require.NoError(t, err)

	toolCall := assistantCallMsg(call("call-again", "lookup"))
	toolCall.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
	}}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{toolCall}}
	_, rewritten, err := middleware.AfterModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.NotSame(t, state, rewritten)
	require.Len(t, AgenticMessageToolCalls(state.Messages[0]), 1, "model output must not mutate")
	require.Empty(t, AgenticMessageToolCalls(rewritten.Messages[0]))
	require.Equal(t, capCannedText, AgenticMessageText(rewritten.Messages[0]))
	require.Equal(t, &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}, AgenticMessageUsage(rewritten.Messages[0]))
	require.NoError(t, ValidateAgenticMessage(rewritten.Messages[0]))
}

func TestAgenticBudgetMiddlewareKeepsNormalForcedAnswer(t *testing.T) {
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	middleware := newAgenticBudgetMiddleware()
	boundary := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		newAgenticToolResult("call-1", "lookup", `{}`),
	}}
	_, _, err = middleware.BeforeModelRewriteState(ctx, boundary, nil)
	require.NoError(t, err)

	answer := NewAgenticAssistantText("synthesized")
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{answer}}
	_, got, err := middleware.AfterModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.Same(t, state, got)
}

func TestAgenticBudgetMiddlewareRewritesReasoningOnlyForcedAnswer(t *testing.T) {
	budget, err := newAgenticToolBudget(1)
	require.NoError(t, err)
	require.NoError(t, budget.recordCompletedRound())
	ctx := withAgenticToolBudget(context.Background(), budget)
	middleware := newAgenticBudgetMiddleware()
	boundary := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		newAgenticToolResult("call-1", "lookup", `{}`),
	}}
	_, _, err = middleware.BeforeModelRewriteState(ctx, boundary, nil)
	require.NoError(t, err)

	reasoningOnly := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.Reasoning{Text: "internal reasoning without an answer"}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{CompletionTokens: 4, TotalTokens: 4}},
	}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{reasoningOnly}}
	_, got, err := middleware.AfterModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.NotSame(t, state, got)
	require.Equal(t, capCannedText, AgenticMessageText(got.Messages[0]))
	require.Equal(t, 4, AgenticMessageUsage(got.Messages[0]).CompletionTokens)
	require.NoError(t, ValidateAgenticMessage(got.Messages[0]))
}
