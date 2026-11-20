package markdown

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// TestConnectorRegisters: the settings and source registries accept it,
// with no actor.
func TestConnectorRegisters(t *testing.T) {
	require.NotPanics(t, func() { settings.NewRegistry(Connector{}) })
	require.NotPanics(t, func() { source.NewRegistry(Connector{}) })
}

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval string
		baseURL  string
		errs     settings.FieldErrors
	}{
		{name: "defaults", interval: "5m0s"},
		{name: "the minimum", interval: "1m0s", baseURL: "https://example.com/docs/"},
		{name: "too often", interval: "30s", errs: settings.FieldErrors{FieldRescanInterval: "must be at least 1 minute"}},
		{name: "not a web address", interval: "5m0s", baseURL: "example.com/docs", errs: settings.FieldErrors{FieldBaseURL: "must be an http or https URL"}},
		{name: "another scheme", interval: "5m0s", baseURL: "file:///etc/", errs: settings.FieldErrors{FieldBaseURL: "must be an http or https URL"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Connector{}.Validate(settings.Values{FieldRescanInterval: tt.interval, FieldBaseURL: tt.baseURL})
			if tt.errs == nil {
				require.NoError(t, err)
				return
			}
			require.Equal(t, tt.errs, err)
		})
	}
}

// TestTestReportsTheFolder: Test fails without a folder, on one that does
// not open, on one with no Markdown files, and on one with too many, and
// otherwise says how many it found.
func TestTestReportsTheFolder(t *testing.T) {
	t.Setenv(DirVariable, "")
	report := Connector{}.Test(t.Context(), settings.Resolved{})
	require.False(t, report.Passed())
	require.Equal(t, "No folder is mounted for this deployment.", report.Checks[0].Detail)

	full := t.TempDir()
	writeTree(t, full, map[string]string{"a.md": "# A", "b/c.md": "# C", "d.txt": "d"})
	t.Setenv(DirVariable, full)
	report = Connector{}.Test(t.Context(), settings.Resolved{})
	require.True(t, report.Passed(), "%+v", report)
	require.Equal(t, "Found 2 Markdown files.", report.Checks[1].Detail)

	for name, tt := range map[string]struct {
		dir    string
		limit  int
		failed string
		detail string
	}{
		"missing":   {dir: filepath.Join(t.TempDir(), "missing"), limit: maxFiles, failed: "Folder"},
		"empty":     {dir: t.TempDir(), limit: maxFiles, failed: "Markdown files", detail: "The folder has no Markdown files."},
		"too many":  {dir: full, limit: 1, failed: "Markdown files", detail: "The folder has more than 10,000 Markdown files."},
		"at limit":  {dir: full, limit: 2},
		"one file":  {dir: filepath.Join(full, "b"), limit: maxFiles, detail: "Found 1 Markdown file."},
		"no folder": {dir: "", limit: maxFiles, failed: "Folder", detail: "No folder is mounted for this deployment."},
	} {
		t.Run(name, func(t *testing.T) {
			report := test(t.Context(), tt.dir, tt.limit)
			require.Len(t, report.Checks, 2)
			if tt.failed == "" {
				require.True(t, report.Passed(), "%+v", report)
			} else {
				require.False(t, report.Passed())
				for _, check := range report.Checks {
					if check.Name == tt.failed {
						require.False(t, check.Passed)
						require.NotEmpty(t, check.Remedy)
					}
				}
			}
			if tt.detail != "" {
				require.Contains(t, []string{report.Checks[0].Detail, report.Checks[1].Detail}, tt.detail)
			}
		})
	}
}
