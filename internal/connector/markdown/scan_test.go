package markdown

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTree writes files, by slash-separated path under dir, creating their
// directories.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, text := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(text), 0o644))
	}
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// TestListFindsOnlyMarkdownFilesInTheFolder: a scan reads regular .md and
// .markdown files under the folder, and nothing reached through a symlink,
// hidden, or of another kind.
func TestListFindsOnlyMarkdownFilesInTheFolder(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.md": "# Outside", "docs/leak.md": "# Leak"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"README.md":               "# Readme",
		"guide/setup.markdown":    "# Setup",
		"guide/deep/NOTES.MD":     "# Notes",
		"guide/image.png":         "png",
		"notes.txt":               "text",
		".hidden.md":              "# Hidden",
		".git/HEAD.md":            "# In a hidden directory",
		"guide/.drafts/draft.md":  "# Draft",
		"real/inside.md":          "# Inside",
		"markdown.md.bak":         "backup",
		"guide/deep/plain.md.txt": "text",
	})
	for link, target := range map[string]string{
		"linked-inside.md":  filepath.Join(dir, "README.md"),
		"relative.md":       "README.md",
		"linked-outside.md": filepath.Join(outside, "secret.md"),
		"escape.md":         "../" + filepath.Base(outside) + "/secret.md",
		"linked-dir":        filepath.Join(dir, "real"),
		"outside-dir":       filepath.Join(outside, "docs"),
		"dangling.md":       filepath.Join(dir, "missing.md"),
	} {
		require.NoError(t, os.Symlink(target, filepath.Join(dir, link)))
	}

	found, err := list(t.Context(), openRoot(t, dir), maxFiles)
	require.NoError(t, err)
	require.Equal(t, []string{"README.md", "guide/deep/NOTES.MD", "guide/setup.markdown", "real/inside.md"}, found.paths)
	require.Zero(t, found.badNames)
}

// TestReadStaysInTheFolder: a path that climbs out of the folder, or a
// symlink that leads out of it, is never read.
func TestReadStaysInTheFolder(t *testing.T) {
	parent := t.TempDir()
	writeTree(t, parent, map[string]string{"outside.md": "# Outside", "folder/inside.md": "# Inside"})
	dir := filepath.Join(parent, "folder")
	require.NoError(t, os.Symlink(filepath.Join(parent, "outside.md"), filepath.Join(dir, "link.md")))
	root := openRoot(t, dir)

	for _, p := range []string{"../outside.md", "sub/../../outside.md", "link.md", "/outside.md"} {
		_, err := read(root, p)
		require.Error(t, err, p)
	}
	got, err := read(root, "inside.md")
	require.NoError(t, err)
	require.Equal(t, "# Inside", got.text)
}

// TestReadRefusesAFileReplacedByASymlink: a file the walk listed and that
// became a symlink before it was read is not read through the link, even to
// a file in the folder.
func TestReadRefusesAFileReplacedByASymlink(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".private/secret.txt": "not for the index"})
	require.NoError(t, os.Symlink(".private/secret.txt", filepath.Join(dir, "a.md")))
	_, err := read(openRoot(t, dir), "a.md")
	require.ErrorIs(t, err, errNotRegular)
}

// TestReadSkipsWhatItCannotIndex: a file over 1 MiB, not UTF-8 text, or
// empty is skipped; a file of exactly 1 MiB is read.
func TestReadSkipsWhatItCannotIndex(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"exact.md":   strings.Repeat("a", maxFileBytes),
		"large.md":   strings.Repeat("a", maxFileBytes+1),
		"latin1.md":  "caf\xe9",
		"nul.md":     "# A\x00B",
		"empty.md":   "",
		"spaces.md":  " \n\t\n",
		"unicode.md": "# Café ☕",
	})
	root := openRoot(t, dir)
	for _, p := range []string{"large.md", "latin1.md", "nul.md", "empty.md", "spaces.md"} {
		_, err := read(root, p)
		require.ErrorIs(t, err, errSkip, p)
	}
	for _, p := range []string{"exact.md", "unicode.md"} {
		_, err := read(root, p)
		require.NoError(t, err, p)
	}
}

// TestListStopsPastTheLimit: a folder with more Markdown files than the
// limit is an error, not part of the folder; one at the limit is not.
func TestListStopsPastTheLimit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a", "b.md": "b", "c/d.md": "d", "c/e.txt": "e"})
	root := openRoot(t, dir)

	found, err := list(t.Context(), root, 3)
	require.NoError(t, err)
	require.Len(t, found.paths, 3)
	_, err = list(t.Context(), root, 2)
	require.ErrorIs(t, err, errTooMany)
}

func TestListStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := list(ctx, openRoot(t, dir), maxFiles)
	require.ErrorIs(t, err, context.Canceled)
}

func TestTitle(t *testing.T) {
	for _, tt := range []struct {
		name, path, text, want string
	}{
		{"first heading", "a.md", "intro\n# First\n# Second\n", "First"},
		{"heading after a fence", "a.md", "```sh\n# a comment\n```\n# Real\n", "Real"},
		{"tilde fence", "a.md", "~~~\n# a comment\n~~~\n", "a"},
		{"only deeper headings", "guide/setup.markdown", "## Setup\ntext", "setup"},
		{"empty heading", "notes.md", "#\n# \ntext", "notes"},
		{"crlf", "a.md", "# Windows line\r\nbody", "Windows line"},
		{"no extension left", "dir/Release.Notes.md", "text", "Release.Notes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, title(tt.path, tt.text))
		})
	}
}

func TestLink(t *testing.T) {
	for _, tt := range []struct {
		base, path, want string
	}{
		{"", "a.md", ""},
		{"https://example.com/docs", "a.md", "https://example.com/docs/a.md"},
		{"https://example.com/docs/", "guide/a.md", "https://example.com/docs/guide/a.md"},
		{"https://example.com/", "my notes/100% #1?.md", "https://example.com/my%20notes/100%25%20%231%3F.md"},
		{"https://example.com/#/", "a.md", "https://example.com/#/a.md"},
	} {
		require.Equal(t, tt.want, link(tt.base, tt.path), tt.path)
	}
}
