package prompts_test

import (
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/stretchr/testify/require"
)

// exampleSHA256 is the bundle hash of the example rendered for an
// organization named Acme, with {{SOURCES}} left as written (cmd/diderot's source surface golden pins what
// it renders to). An edit here changes what a fresh install seeds: update
// the hash in the same commit, on purpose.
const exampleSHA256 = "8c62089b07dd9e5fd6992c45d0d491a2b010f253524b04591212a51b50c7b19b"

func TestExampleRendersPinnedBundle(t *testing.T) {
	bundle, err := promptcatalog.Embedded()
	require.NoError(t, err)
	rendered := bundle.Render(map[string]string{promptcatalog.VarOrgName: "Acme"})
	require.Equal(t, exampleSHA256, rendered.SHA256,
		"the example prompts changed; if that is intended, update exampleSHA256")
}

func TestExampleRendersWithoutAnOrganization(t *testing.T) {
	rendered := promptcatalog.MustEmbedded().Render(promptcatalog.Values())
	for _, prompt := range rendered.Prompts {
		require.NotContains(t, prompt.Body, "{{", prompt.Key)
	}
	base, err := rendered.Text(promptcatalog.AskBase)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(base, "You are Diderot. You answer questions about what the organization has written down"))
}
