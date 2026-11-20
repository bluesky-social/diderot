//go:build unix

package markdown

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNamedPipeIsSkippedWithoutBlocking: a named pipe called .md is not a
// Markdown file. Opening one for reading would wait for a writer; neither
// the walk nor a read of one that replaced a file since does.
func TestNamedPipeIsSkippedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A"})
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o644))
	root := openRoot(t, dir)

	var (
		found            folder
		listErr, readErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		found, listErr = list(t.Context(), root, maxFiles)
		_, readErr = read(root, "pipe.md")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading the folder blocked on a named pipe")
	}
	require.NoError(t, listErr)
	require.Equal(t, []string{"a.md"}, found.paths)
	require.ErrorIs(t, readErr, errNotRegular)
}
