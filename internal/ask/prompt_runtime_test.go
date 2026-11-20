package ask

import (
	"context"
	"testing"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/stretchr/testify/require"
)

type unusedReleaseReader struct{}

func (unusedReleaseReader) Current(context.Context) (*promptcatalog.Release, error) {
	panic("not called")
}

func (unusedReleaseReader) Get(context.Context, int64) (*promptcatalog.Release, error) {
	panic("not called")
}

func TestPromptRuntimeBuildsAgentsPerRenderedBundle(t *testing.T) {
	ctx := context.Background()
	runtime, err := NewPromptRuntime(unusedReleaseReader{}, newAskAgentsTestModel(t, ""), nil, 3)
	require.NoError(t, err)
	release := promptcatalog.MustEmbedded()
	render := func(org string) promptcatalog.Bundle {
		return release.Render(map[string]string{promptcatalog.VarOrgName: org})
	}

	first, err := runtime.Agents(ctx, render("First Co"))
	require.NoError(t, err)
	again, err := runtime.Agents(ctx, render("First Co"))
	require.NoError(t, err)
	require.Same(t, first, again, "one release rendered the same way reuses its runners")
	renamed, err := runtime.Agents(ctx, render("Second Co"))
	require.NoError(t, err)
	require.NotSame(t, first, renamed, "a changed variable builds runners with the new text")
}
