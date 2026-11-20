package ask

import (
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/stretchr/testify/require"
)

// TestOrganizationNoteFollowsRoleAndResearch: an install's note is appended
// right after Role and research, never to the observability investigator,
// and a release without it adds nothing.
func TestOrganizationNoteFollowsRoleAndResearch(t *testing.T) {
	t.Parallel()
	bundle := promptcatalog.MustEmbedded()
	bundle.Prompts = append(bundle.Prompts, promptcatalog.Prompt{Key: promptcatalog.AskOrganization, Body: "ASK-NOTE"})
	slices.SortFunc(bundle.Prompts, func(a, b promptcatalog.Prompt) int { return strings.Compare(a.Key, b.Key) })

	instructions, err := AskInstructionsFromBundle(bundle)
	require.NoError(t, err)
	root := agentInstructionWith(instructions, SurfaceHTTP, nil)
	require.True(t, strings.HasPrefix(root, bundle.MustText(promptcatalog.AskBase)+"\n\nASK-NOTE\n\n"+bundle.MustText(promptcatalog.AskAnswerDiscipline)))
	require.NotContains(t, investigateChildInstructionWith(bundle, ObservabilitySteering{}, investigateCapabilities{sentry: true}), "ASK-NOTE")

	plain := promptcatalog.MustEmbedded()
	plainInstructions, err := AskInstructionsFromBundle(plain)
	require.NoError(t, err)
	require.Empty(t, plainInstructions.Organization)
	require.True(t, strings.HasPrefix(agentInstructionWith(plainInstructions, SurfaceHTTP, nil),
		plain.MustText(promptcatalog.AskBase)+"\n\n"+plain.MustText(promptcatalog.AskAnswerDiscipline)))
}
