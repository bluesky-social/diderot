package ask

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openairesponses "github.com/openai/openai-go/v3/responses"
)

// stagedEffortMiddleware keeps the default model behavior unless a turn has
// an explicit schedule. Once enough root tool rounds have completed, it adds
// the scheduled effort to each subsequent model request. A nil tool budget is
// deliberate for child workloads and prevents the root schedule from leaking.
type stagedEffortMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newStagedEffortMiddleware() *stagedEffortMiddleware {
	return &stagedEffortMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

func (m *stagedEffortMiddleware) WrapModel(
	_ context.Context,
	mdl model.BaseModel[*schema.AgenticMessage],
	_ *adk.TypedModelContext[*schema.AgenticMessage],
) (model.BaseModel[*schema.AgenticMessage], error) {
	return stagedEffortModel{inner: mdl}, nil
}

type stagedEffortModel struct {
	inner model.BaseModel[*schema.AgenticMessage]
}

func (m stagedEffortModel) Generate(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.AgenticMessage, error) {
	return m.inner.Generate(ctx, input, appendScheduledEffort(ctx, opts)...)
}

func (m stagedEffortModel) Stream(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return m.inner.Stream(ctx, input, appendScheduledEffort(ctx, opts)...)
}

func appendScheduledEffort(ctx context.Context, opts []model.Option) []model.Option {
	schedule := modelOverridesFromContext(ctx).ReasoningEffortSchedule
	budget := agenticToolBudgetFromContext(ctx)
	if schedule == nil || budget == nil {
		return opts
	}
	completed, _, _ := budget.state()
	if completed < schedule.AfterToolRounds {
		return opts
	}
	option := agenticopenai.WithResponsesReasoning(&openairesponses.ReasoningParam{
		Effort: openairesponses.ReasoningEffort(schedule.Effort),
	})
	return append(append([]model.Option(nil), opts...), option)
}
