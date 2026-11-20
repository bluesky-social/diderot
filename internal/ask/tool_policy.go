package ask

import (
	"context"
	"slices"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type corpusOnlyContextKey struct{}

func withCorpusOnly(ctx context.Context, restricted bool) context.Context {
	return context.WithValue(ctx, corpusOnlyContextKey{}, restricted)
}

func corpusOnlyFromContext(ctx context.Context) bool {
	restricted, _ := ctx.Value(corpusOnlyContextKey{}).(bool)
	return restricted
}

func corpusToolAllowed(name string) bool {
	switch name {
	case "retrieve", "list", "expand":
		return true
	default:
		return false
	}
}

type agenticToolPolicyMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newAgenticToolPolicyMiddleware() *agenticToolPolicyMiddleware {
	return &agenticToolPolicyMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

// BeforeAgent assembles the run's tools: it leaves out every optional tool
// that is unavailable as the run starts, so the model is never told about it
// and a call to it finds no such tool.
func (middleware *agenticToolPolicyMiddleware) BeforeAgent(
	ctx context.Context, runCtx *adk.ChatModelAgentContext,
) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	next := *runCtx
	next.Tools = slices.DeleteFunc(slices.Clone(runCtx.Tools), func(candidate tool.BaseTool) bool {
		optional, ok := candidate.(OptionalTool)
		return ok && !optional.Available()
	})
	return ctx, &next, nil
}

func (middleware *agenticToolPolicyMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if !corpusOnlyFromContext(ctx) || state == nil {
		return middleware.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	filter := func(infos []*schema.ToolInfo) []*schema.ToolInfo {
		filtered := make([]*schema.ToolInfo, 0, len(infos))
		for _, info := range infos {
			if info != nil && corpusToolAllowed(info.Name) {
				filtered = append(filtered, info)
			}
		}
		return filtered
	}
	next := *state
	next.ToolInfos = filter(state.ToolInfos)
	next.DeferredToolInfos = filter(state.DeferredToolInfos)
	return ctx, &next, nil
}

const toolAccessDenied = `{"error":"tool is not available for this audience; use the advertised corpus tools"}`

func (middleware *agenticToolPolicyMiddleware) WrapInvokableToolCall(
	_ context.Context, endpoint adk.InvokableToolCallEndpoint, toolContext *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, arguments string, options ...tool.Option) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if corpusOnlyFromContext(ctx) && (toolContext == nil || !corpusToolAllowed(toolContext.Name)) {
			return toolAccessDenied, nil
		}
		return endpoint(ctx, arguments, options...)
	}, nil
}

func (middleware *agenticToolPolicyMiddleware) WrapEnhancedInvokableToolCall(
	_ context.Context, endpoint adk.EnhancedInvokableToolCallEndpoint, toolContext *adk.ToolContext,
) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, argument *schema.ToolArgument, options ...tool.Option) (*schema.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if corpusOnlyFromContext(ctx) && (toolContext == nil || !corpusToolAllowed(toolContext.Name)) {
			return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: toolAccessDenied}}}, nil
		}
		return endpoint(ctx, argument, options...)
	}, nil
}
