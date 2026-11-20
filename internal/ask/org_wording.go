package ask

import (
	"context"
	"strings"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// OrgNamePlaceholder stands for the organization's name in built-in tool
// descriptions. Each run replaces it with the name saved on the
// Organization page, so a new name applies without a restart.
const OrgNamePlaceholder = "{{ORG_NAME}}"

// DefaultOrgName stands in for the organization when none is saved.
const DefaultOrgName = promptcatalog.DefaultOrgName

type orgNameContextKey struct{}

func withOrgName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, orgNameContextKey{}, name)
}

func orgNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(orgNameContextKey{}).(string)
	return name
}

// renderOrgName replaces the placeholder in one pass; the substituted name
// is never scanned again.
func renderOrgName(text, name string) string {
	if name = strings.TrimSpace(name); name == "" {
		name = DefaultOrgName
	}
	return strings.ReplaceAll(text, OrgNamePlaceholder, name)
}

// agenticOrgWordingMiddleware fills OrgNamePlaceholder in the tool
// descriptions of every model call from the run's organization name.
type agenticOrgWordingMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newAgenticOrgWordingMiddleware() *agenticOrgWordingMiddleware {
	return &agenticOrgWordingMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

func (middleware *agenticOrgWordingMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if state == nil {
		return middleware.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	name := orgNameFromContext(ctx)
	next := *state
	next.ToolInfos = renderToolInfos(state.ToolInfos, name)
	next.DeferredToolInfos = renderToolInfos(state.DeferredToolInfos, name)
	return ctx, &next, nil
}

// renderToolInfos copies every info whose description names the
// organization; the registered infos are shared across runs and never
// modified.
func renderToolInfos(infos []*schema.ToolInfo, name string) []*schema.ToolInfo {
	if infos == nil {
		return nil
	}
	out := make([]*schema.ToolInfo, len(infos))
	for i, info := range infos {
		if info != nil && strings.Contains(info.Desc, OrgNamePlaceholder) {
			rendered := *info
			rendered.Desc = renderOrgName(info.Desc, name)
			info = &rendered
		}
		out[i] = info
	}
	return out
}
