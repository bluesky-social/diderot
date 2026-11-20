package promptcatalog

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func setVariables(t *testing.T, values map[string]string) {
	t.Helper()
	SetVariables(func() map[string]string { return values })
	t.Cleanup(func() { SetVariables(nil) })
}

func TestValuesFillEveryVariable(t *testing.T) {
	setVariables(t, nil)
	require.Equal(t, map[string]string{
		VarOrgName: DefaultOrgName, VarOrgDescription: "", VarMaxTitleRunes: "72", VarSources: "", VarSlackWorkspaceName: "",
		VarGrafanaDatasources: "",
	}, Values())

	setVariables(t, map[string]string{
		VarOrgName: " Example Co ", VarOrgDescription: "Makes examples.", VarSlackWorkspaceName: "Example",
		VarSources: "- `docs` — documents.\n- `chat` — messages.\n", "UNLISTED": "x",
		VarGrafanaDatasources: " Grafana datasources: Loki `logs`. ",
	})
	require.Equal(t, map[string]string{
		VarOrgName: "Example Co", VarOrgDescription: "Makes examples.", VarMaxTitleRunes: "72",
		VarSources: "- `docs` — documents.\n- `chat` — messages.", VarSlackWorkspaceName: "Example",
		VarGrafanaDatasources: "Grafana datasources: Loki `logs`.",
	}, Values())
}

func TestRenderIsOnePassAndLiteral(t *testing.T) {
	setVariables(t, map[string]string{
		VarOrgName:        "{{ORG_DESCRIPTION}} and $1 {{.}}",
		VarOrgDescription: "rescanned",
	})
	got := Render("What {{ORG_NAME}} knows; {{ORG_DESCRIPTION}}; {{ OTHER }}.", Values())
	require.Equal(t, "What {{ORG_DESCRIPTION}} and $1 {{.}} knows; rescanned; {{ OTHER }}.", got,
		"a substituted value is never scanned again, and unknown text is left alone")
}

func TestBundleRenderRehashes(t *testing.T) {
	bundle := MustEmbedded()
	rendered := bundle.Render(map[string]string{VarOrgName: "Example Co"})
	require.NotEqual(t, bundle.SHA256, rendered.SHA256)
	require.NoError(t, ValidateWire(rendered))
	text, err := rendered.Text(AskBase)
	require.NoError(t, err)
	require.Contains(t, text, "what Example Co has written down")
	original, err := bundle.Text(AskBase)
	require.NoError(t, err)
	require.Contains(t, original, "what {{ORG_NAME}} has written down", "Render does not modify its receiver")
}

func TestActiveTextRendersPerCall(t *testing.T) {
	setVariables(t, map[string]string{VarOrgName: "First Co"})
	require.Contains(t, ActiveText(AskBase), "what First Co has written down")
	setVariables(t, map[string]string{VarOrgName: "Second Co"})
	require.Contains(t, ActiveText(AskBase), "what Second Co has written down")
	require.Contains(t, ActiveText(SessionTitle), "at most 72 characters")
	text, err := Active().Text(AskBase)
	require.NoError(t, err)
	require.Contains(t, text, "what Second Co has written down")
}

func TestValidateTextRejectsUnknownPlaceholders(t *testing.T) {
	for _, body := range []string{
		"Hello {{ORG_NAME}} at {{MAX_TITLE_RUNES}} with {{ORG_DESCRIPTION}}.",
		"Helm writes {{ .Values.image }} and {{- end }}; braces like { } and {x} are text.",
	} {
		require.NoError(t, ValidateText("ask.base", body), body)
	}
	for _, body := range []string{
		"Hello {{ORG}}.",
		"Hello {{org_name}}.",
		"Hello {{ ORG_NAME }}.",
		"Hello {{ORG-NAME}}.",
	} {
		require.ErrorContains(t, ValidateText("ask.base", body), "ask.base uses unknown variable", body)
	}
}

func TestLoadRejectsUnknownPlaceholder(t *testing.T) {
	files := fstest.MapFS{}
	for _, prompt := range MustEmbedded().Prompts {
		files[prompt.Path] = &fstest.MapFile{Data: []byte(prompt.Body)}
	}
	manifest := "schema_version: 1\nprompts:\n"
	for _, prompt := range MustEmbedded().Prompts {
		manifest += "  - key: " + prompt.Key + "\n    path: " + prompt.Path + "\n"
	}
	files["manifest.yaml"] = &fstest.MapFile{Data: []byte(manifest)}
	_, err := Load(files)
	require.NoError(t, err)

	files["ask/base.md"] = &fstest.MapFile{Data: []byte("Answer for {{COMPANY}}.")}
	_, err = Load(files)
	require.ErrorContains(t, err, "ask.base uses unknown variable {{COMPANY}}")
}
