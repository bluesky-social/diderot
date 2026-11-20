package ask

import (
	"context"
	"errors"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const defaultMaxToolTurns = 8

const capSynthInstr = `You have used all the tool rounds this turn allows, so you cannot call tools again. Write the final answer now from the results above. Give what the evidence supports, say plainly what you could not establish or did not get to, and do not tell the asker only to ask again.`

const capCannedText = "(I hit the max tool-call budget for this turn; partial information from tool results above. Ask again to continue.)"

const capErrorCannedText = "(I ran into an error finishing this answer. Try again in a moment.)"

type agenticToolBudgetContextKey struct{}

// agenticToolBudget is one run's tool-round state. A round is one completed
// assistant tool-call batch, regardless of parallel fan-out.
type agenticToolBudget struct {
	mu              sync.Mutex
	maxRounds       int
	completedRounds int
	forced          bool
}

func newAgenticToolBudget(maxRounds int) (*agenticToolBudget, error) {
	if maxRounds <= 0 {
		return nil, errors.New("ask tool budget: max rounds must be positive")
	}
	return &agenticToolBudget{maxRounds: maxRounds}, nil
}

func withAgenticToolBudget(ctx context.Context, budget *agenticToolBudget) context.Context {
	return context.WithValue(ctx, agenticToolBudgetContextKey{}, budget)
}

func agenticToolBudgetFromContext(ctx context.Context) *agenticToolBudget {
	budget, _ := ctx.Value(agenticToolBudgetContextKey{}).(*agenticToolBudget)
	return budget
}

func (b *agenticToolBudget) recordCompletedRound() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.forced {
		return errors.New("ask tool budget: tool round completed after forced synthesis began")
	}
	if b.completedRounds >= b.maxRounds {
		return errors.New("ask tool budget: completed rounds exceeded maximum")
	}
	b.completedRounds++
	return nil
}

func (b *agenticToolBudget) state() (completed, max int, forced bool) {
	if b == nil {
		return 0, 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.completedRounds, b.maxRounds, b.forced
}

func (b *agenticToolBudget) beginForcedSynthesis() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.forced || b.completedRounds < b.maxRounds {
		return false
	}
	b.forced = true
	return true
}

func (b *agenticToolBudget) forcedSynthesisStarted() bool {
	_, _, forced := b.state()
	return forced
}

// agenticBudgetMiddleware uses Eino's dynamic ToolInfos state rather than a
// second agent loop. Once the completed-round cap is reached, the next model
// call sees the final tool results plus one synthesis instruction. It retains
// every declared tool for protocol-valid history while the model option allows
// no further tool calls.
type agenticBudgetMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newAgenticBudgetMiddleware() *agenticBudgetMiddleware {
	return &agenticBudgetMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

func (m *agenticBudgetMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	budget := agenticToolBudgetFromContext(ctx)
	completed, maxRounds, alreadyForced := budget.state()
	if budget == nil || completed < maxRounds {
		return m.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, errors.New("ask tool budget: forced synthesis has no message history")
	}

	next := *state
	next.Messages = append([]*schema.AgenticMessage(nil), state.Messages...)
	// Keep the complete tool declarations. The model gateway's Anthropic converter
	// may validate every historical function call against the tools declared on
	// the current Responses request. Removing research schemas here makes an
	// otherwise valid conversation fail before synthesis. WrapModel separately
	// forbids new calls.
	next.ToolInfos = append([]*schema.ToolInfo(nil), state.ToolInfos...)
	next.DeferredToolInfos = append([]*schema.ToolInfo(nil), state.DeferredToolInfos...)
	if !alreadyForced {
		last := state.Messages[len(state.Messages)-1]
		if last == nil || last.Role != schema.AgenticRoleTypeUser || len(AgenticMessageToolResults(last)) == 0 {
			return ctx, state, errors.New("ask tool budget: forced synthesis requires trailing tool results")
		}
		if !budget.beginForcedSynthesis() {
			return ctx, state, errors.New("ask tool budget: forced synthesis state changed concurrently")
		}
		// Keep the completed tool-result message intact. Anthropic rejects a
		// Responses history item that mixes tool results with ordinary user
		// text as "invalid Responses tool history". The synthesis instruction
		// is a distinct user message after the full result batch.
		next.Messages = append(next.Messages, NewAgenticUserText(capSynthInstr))
	}
	return ctx, &next, nil
}

func (m *agenticBudgetMiddleware) WrapModel(
	_ context.Context,
	mdl model.BaseModel[*schema.AgenticMessage],
	_ *adk.TypedModelContext[*schema.AgenticMessage],
) (model.BaseModel[*schema.AgenticMessage], error) {
	return forcedSynthesisModel{inner: mdl}, nil
}

type forcedSynthesisModel struct {
	inner model.BaseModel[*schema.AgenticMessage]
}

func (m forcedSynthesisModel) Generate(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.AgenticMessage, error) {
	return m.inner.Generate(ctx, input, appendBudgetToolRestriction(ctx, opts)...)
}

func (m forcedSynthesisModel) Stream(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return m.inner.Stream(ctx, input, appendBudgetToolRestriction(ctx, opts)...)
}

func appendBudgetToolRestriction(ctx context.Context, opts []model.Option) []model.Option {
	budget := agenticToolBudgetFromContext(ctx)
	if budget == nil || !budget.forcedSynthesisStarted() {
		return opts
	}
	choice := model.WithAgenticToolChoice(&schema.AgenticToolChoice{
		Type: schema.ToolChoiceForbidden,
	})
	return append(append([]model.Option(nil), opts...), choice)
}

func (m *agenticBudgetMiddleware) AfterModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	budget := agenticToolBudgetFromContext(ctx)
	if budget == nil || !budget.forcedSynthesisStarted() {
		return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
	}
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, errors.New("ask tool budget: forced synthesis produced no message")
	}
	latest := state.Messages[len(state.Messages)-1]
	if latest != nil && len(AgenticMessageToolCalls(latest)) == 0 && AgenticMessageText(latest) != "" {
		return ctx, state, nil
	}
	fallback := NewAgenticAssistantText(capCannedText)
	if latest != nil {
		if usage := AgenticMessageUsage(latest); usage != nil {
			fallback.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: usage}
		}
	}
	next := *state
	next.Messages = append([]*schema.AgenticMessage(nil), state.Messages...)
	next.Messages[len(next.Messages)-1] = fallback
	return ctx, &next, nil
}
