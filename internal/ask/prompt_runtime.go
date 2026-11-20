package ask

import (
	"context"
	"fmt"
	"sync"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
)

type PromptReleaseReader interface {
	Current(context.Context) (*promptcatalog.Release, error)
	Get(context.Context, int64) (*promptcatalog.Release, error)
}

// PromptRuntime resolves immutable releases and lazily builds one reusable
// root runner pair per rendered release. Sessions pin the release ID before
// asking for runners, so activation never changes instructions
// mid-conversation; a changed prompt variable applies from the next turn.
type PromptRuntime struct {
	store         PromptReleaseReader
	model         model.AgenticModel
	tools         []tool.BaseTool
	maxIterations int

	mu sync.Mutex
	// cache is keyed by the rendered bundle's SHA256.
	cache map[string]*AskAgents
}

func NewPromptRuntime(store PromptReleaseReader, mdl model.AgenticModel, tools []tool.BaseTool, maxIterations int) (*PromptRuntime, error) {
	if store == nil {
		return nil, fmt.Errorf("ask prompt runtime: store is required")
	}
	if mdl == nil {
		return nil, fmt.Errorf("ask prompt runtime: model is required")
	}
	if maxIterations <= 0 {
		return nil, fmt.Errorf("ask prompt runtime: maxIterations must be positive")
	}
	return &PromptRuntime{store: store, model: mdl,
		tools: append([]tool.BaseTool(nil), tools...), maxIterations: maxIterations,
		cache: make(map[string]*AskAgents)}, nil
}

func (r *PromptRuntime) Current(ctx context.Context) (*promptcatalog.Release, error) {
	return r.store.Current(ctx)
}

func (r *PromptRuntime) Get(ctx context.Context, id int64) (*promptcatalog.Release, error) {
	return r.store.Get(ctx, id)
}

// Agents returns the root runners for a release bundle already rendered with
// this turn's prompt variables.
func (r *PromptRuntime) Agents(ctx context.Context, rendered promptcatalog.Bundle) (*AskAgents, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if agents := r.cache[rendered.SHA256]; agents != nil {
		return agents, nil
	}
	instructions, err := AskInstructionsFromBundle(rendered)
	if err != nil {
		return nil, fmt.Errorf("ask prompt runtime: bundle %s: %w", rendered.SHA256, err)
	}
	agents, err := NewAskAgentsWithInstructions(ctx, r.model, r.tools, instructions, r.maxIterations)
	if err != nil {
		return nil, fmt.Errorf("ask prompt runtime: build bundle %s agents: %w", rendered.SHA256, err)
	}
	r.cache[rendered.SHA256] = agents
	return agents, nil
}

type promptBundleContextKey struct{}

func withPromptBundle(ctx context.Context, bundle promptcatalog.Bundle) context.Context {
	return context.WithValue(ctx, promptBundleContextKey{}, bundle)
}

func promptBundleFromContext(ctx context.Context) promptcatalog.Bundle {
	if bundle, ok := ctx.Value(promptBundleContextKey{}).(promptcatalog.Bundle); ok {
		return bundle
	}
	return embeddedAskPrompts
}
