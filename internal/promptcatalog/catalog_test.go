package promptcatalog

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestEmbeddedCatalog(t *testing.T) {
	bundle, err := Embedded()
	require.NoError(t, err)
	require.NoError(t, ValidateWire(bundle))
	require.Len(t, bundle.SHA256, 64)
	_, err = bundle.Text("ask.pagerduty")
	require.Error(t, err)
	for _, key := range requiredKeys() {
		text, err := bundle.Text(key)
		require.NoError(t, err, key)
		require.NotEmpty(t, text)
	}
}

func TestValidateWireAcceptsReleaseBeforeAskTitles(t *testing.T) {
	bundle := MustEmbedded()
	var prompts []Prompt
	for _, prompt := range bundle.Prompts {
		if prompt.Key != AskTitle {
			prompts = append(prompts, prompt)
		}
	}
	require.Len(t, prompts, len(bundle.Prompts)-1)
	bundle.Prompts = prompts
	bundle.SHA256 = bundleHash(bundle)
	require.NoError(t, ValidateWire(bundle))
	text, err := bundle.Text(AskBase)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	_, err = bundle.Text(AskTitle)
	require.Error(t, err)
}

func TestLoadRejectsUnknownManifestFields(t *testing.T) {
	_, err := Load(fstest.MapFS{
		"manifest.yaml": {Data: []byte("schema_version: 1\nunknown: true\nprompts: []\n")},
	})
	require.ErrorContains(t, err, "field unknown not found")
}

func TestValidateWireRejectsMutation(t *testing.T) {
	bundle := MustEmbedded()
	bundle.Prompts[0].Body += " changed"
	require.ErrorContains(t, ValidateWire(bundle), "content hash mismatch")
}

func TestLoadRejectsUnlistedMarkdown(t *testing.T) {
	_, err := Load(fstest.MapFS{
		"manifest.yaml": {Data: []byte("schema_version: 1\nprompts:\n  - key: one\n    path: one.md\n")},
		"one.md":        {Data: []byte("listed")},
		"forgotten.md":  {Data: []byte("not listed")},
	})
	require.ErrorContains(t, err, "not listed in manifest")
}
