package ask

import (
	"context"
	"errors"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"
)

type AskRunSurface string

const (
	AskRunSurfaceHTTP      AskRunSurface = "http"
	AskRunSurfaceSlack     AskRunSurface = "slack"
	AskRunSurfaceDiscord   AskRunSurface = "discord"
	AskRunSurfaceScheduled AskRunSurface = "scheduled"
	AskRunSurfacePagerDuty AskRunSurface = "pagerduty"
)

// AskRunMetadata is the bounded, non-sensitive correlation data attached to
// every model gateway request made for one Ask turn: the canonical Ask
// session UUID only. It rides the x-agent-parent-session-id header as the
// trusted work join; there is no provider-invocation attribution header.
type AskRunMetadata struct {
	SessionID uuid.UUID
}

type askRunMetadataContextKey struct{}

func withAskRunMetadataContext(ctx context.Context, meta AskRunMetadata) context.Context {
	return context.WithValue(ctx, askRunMetadataContextKey{}, meta)
}

func askRunMetadataFromContext(ctx context.Context) (AskRunMetadata, bool) {
	meta, ok := ctx.Value(askRunMetadataContextKey{}).(AskRunMetadata)
	return meta, ok
}

func withAskRunMetadata(meta AskRunMetadata, extra ...model.Option) (adk.AgentRunOption, error) {
	options, err := askRunModelOptions(meta, extra...)
	if err != nil {
		return adk.AgentRunOption{}, err
	}
	return adk.WithChatModelOptions(options), nil
}

func askRunModelOptions(meta AskRunMetadata, extra ...model.Option) ([]model.Option, error) {
	if meta.SessionID == uuid.Nil {
		return nil, errors.New("ask metadata: session ID is required")
	}

	options := make([]model.Option, 0, len(extra)+1)
	options = append(options, extra...)
	// Keep the session-only header last so another provider-specific option
	// cannot silently erase the trusted work join through last-writer-wins.
	options = append(options, agenticopenai.WithCustomHeaders(map[string]string{
		"x-agent-parent-session-id": meta.SessionID.String(),
	}))
	return options, nil
}
