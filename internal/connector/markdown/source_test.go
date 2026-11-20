package markdown

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// writes counts the events a sink inserts or changes.
type writes struct{ n atomic.Int64 }

func (w *writes) Wrote() { w.n.Add(1) }

// testDeps is what Run hands the source, over a database of its own, with
// a sink whose writes are counted.
func testDeps(t *testing.T) (source.Deps, *writes) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.URL(t, "markdown"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	counted := &writes{}
	return source.Deps{
		Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()).Observed(counted),
		Logger: slog.New(slog.DiscardHandler),
	}.For(ConnectorName), counted
}

// indexedEvent is one event of the source as stored.
type indexedEvent struct {
	Title, URL, Body, Scope string
	OccurredAt              time.Time
	Actor                   string
	Deleted                 bool
}

func indexed(t *testing.T, deps source.Deps) map[string]indexedEvent {
	t.Helper()
	rows, err := deps.Pool.Query(t.Context(), `SELECT external_id, COALESCE(title, ''), COALESCE(url, ''), body, scope,
		occurred_at, COALESCE(actor, ''), deleted FROM events WHERE source = 'markdown'`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]indexedEvent{}
	for rows.Next() {
		var id string
		var e indexedEvent
		require.NoError(t, rows.Scan(&id, &e.Title, &e.URL, &e.Body, &e.Scope, &e.OccurredAt, &e.Actor, &e.Deleted))
		got[id] = e
	}
	require.NoError(t, rows.Err())
	return got
}

// live is the external IDs of the source's events that are not deleted.
func live(t *testing.T, deps source.Deps) []string {
	t.Helper()
	var ids []string
	for id, e := range indexed(t, deps) {
		if !e.Deleted {
			ids = append(ids, id)
		}
	}
	return ids
}

var modified = time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)

// TestScanIndexesEachFile: each Markdown file is one internal event keyed
// by its path, titled by its first heading or its name, dated by its
// modification time, with no actor, and linked under the base URL.
func TestScanIndexesEachFile(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"README.md":           "# Diderot\n\nAnswers questions.",
		"guide/setup.md":      "Run it.\n",
		"my notes/100% #1.md": "## Notes\n",
	})
	for _, p := range []string{"README.md", "guide/setup.md", "my notes/100% #1.md"} {
		require.NoError(t, os.Chtimes(filepath.Join(dir, filepath.FromSlash(p)), modified, modified))
	}

	done, err := scan(t.Context(), deps, dir, "https://example.com/docs", maxFiles)
	require.NoError(t, err)
	require.Equal(t, counts{indexed: 3, changed: 3}, done)
	require.Equal(t, map[string]indexedEvent{
		"README.md": {Title: "Diderot", URL: "https://example.com/docs/README.md", Body: "# Diderot\n\nAnswers questions.",
			Scope: events.ScopeInternal, OccurredAt: modified},
		"guide/setup.md": {Title: "setup", URL: "https://example.com/docs/guide/setup.md", Body: "Run it.\n",
			Scope: events.ScopeInternal, OccurredAt: modified},
		"my notes/100% #1.md": {Title: "100% #1", URL: "https://example.com/docs/my%20notes/100%25%20%231.md", Body: "## Notes\n",
			Scope: events.ScopeInternal, OccurredAt: modified},
	}, utc(indexed(t, deps)))
}

func utc(got map[string]indexedEvent) map[string]indexedEvent {
	for id, e := range got {
		e.OccurredAt = e.OccurredAt.UTC()
		got[id] = e
	}
	return got
}

// TestRescanWritesOnlyChanges: a rescan of an unchanged folder writes
// nothing; an edited file is written again, and a new base URL re-stamps
// every link.
func TestRescanWritesOnlyChanges(t *testing.T) {
	deps, counted := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A", "b/c.md": "# C"})

	_, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.EqualValues(t, 2, counted.n.Load())
	done, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.Equal(t, counts{indexed: 2}, done)
	require.EqualValues(t, 2, counted.n.Load(), "an unchanged folder writes nothing")

	writeTree(t, dir, map[string]string{"a.md": "# A\n\nmore"})
	_, err = scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.EqualValues(t, 3, counted.n.Load())

	_, err = scan(t.Context(), deps, dir, "https://docs.example.com/", maxFiles)
	require.NoError(t, err)
	require.EqualValues(t, 5, counted.n.Load())
	got := indexed(t, deps)
	require.Equal(t, "https://docs.example.com/a.md", got["a.md"].URL)
	require.Equal(t, "https://docs.example.com/b/c.md", got["b/c.md"].URL)
}

// TestRescanRemovesFilesThatAreGone: after a scan of the whole folder, the
// event of a file that is not there is deleted.
func TestRescanRemovesFilesThatAreGone(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A", "b.md": "# B", "c.md": "# C"})
	_, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(dir, "b.md")))
	require.NoError(t, os.Rename(filepath.Join(dir, "c.md"), filepath.Join(dir, ".c.md")))
	done, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.Equal(t, 2, done.removed)
	require.ElementsMatch(t, []string{"a.md"}, live(t, deps))
	require.True(t, indexed(t, deps)["b.md"].Deleted)
}

// TestSkippedFileKeepsItsEvent: a file that grows past 1 MiB or stops being
// UTF-8 text is counted and skipped, and its last indexed text stays until
// it is fixed or removed.
func TestSkippedFileKeepsItsEvent(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A", "b.md": "# B", "c.md": "# C", "d.md": "# D"})
	_, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)

	writeTree(t, dir, map[string]string{"b.md": strings.Repeat("b", maxFileBytes+1), "c.md": "caf\xe9", "d.md": "# D\x00"})
	done, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.Equal(t, counts{indexed: 1, skipped: 3}, done)
	got := indexed(t, deps)
	require.Equal(t, "# B", got["b.md"].Body)
	require.Equal(t, "# C", got["c.md"].Body)
	require.Equal(t, "# D", got["d.md"].Body)
	require.ElementsMatch(t, []string{"a.md", "b.md", "c.md", "d.md"}, live(t, deps))

	require.NoError(t, os.Remove(filepath.Join(dir, "b.md")))
	_, err = scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a.md", "c.md", "d.md"}, live(t, deps))
}

// TestFailedScanDeletesNothing: a scan that stops part way, from an error,
// too many files, or cancellation, removes no event, even of a file that
// is gone.
func TestFailedScanDeletesNothing(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A", "gone.md": "# Gone", "z.md": "# Z"})
	_, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.md")))

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = scan(cancelled, deps, dir, "", maxFiles)
	require.ErrorIs(t, err, context.Canceled)
	_, err = scan(t.Context(), deps, dir, "", 1)
	require.ErrorIs(t, err, errTooMany)
	require.ElementsMatch(t, []string{"a.md", "gone.md", "z.md"}, live(t, deps))

	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its permissions")
	}
	writeTree(t, dir, map[string]string{"a.md": "# A, edited"})
	unreadable := filepath.Join(dir, "z.md")
	require.NoError(t, os.Chmod(unreadable, 0))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	_, err = scan(t.Context(), deps, dir, "", maxFiles)
	require.ErrorIs(t, err, os.ErrPermission)
	got := indexed(t, deps)
	require.Equal(t, "# A, edited", got["a.md"].Body, "the files before the failure are written")
	require.False(t, got["gone.md"].Deleted)
}

// TestEmptyFolderDeletesNothing: a folder with no Markdown files is far
// more often a mount that failed than every document removed, so the scan
// fails, removes nothing, and the page says why.
func TestEmptyFolderDeletesNothing(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	t.Setenv(DirVariable, dir)
	writeTree(t, dir, map[string]string{"a.md": "# A"})
	require.NoError(t, rescan(t.Context(), deps, dir, "", maxFiles))

	require.NoError(t, os.Remove(filepath.Join(dir, "a.md")))
	writeTree(t, dir, map[string]string{"notes.txt": "not Markdown"})
	err := rescan(t.Context(), deps, dir, "", maxFiles)
	require.ErrorIs(t, err, errNoFiles)
	require.ElementsMatch(t, []string{"a.md"}, live(t, deps))

	status, err := Connector{}.Status(t.Context(), deps)
	require.NoError(t, err)
	require.Equal(t, "The last scan failed: The folder has no Markdown files.", status.Waiting)
	require.Equal(t, "failed", status.Rows[0].Detail)
	require.Equal(t, settings.StatusRow{Label: "Files indexed", Value: int64(1)}, status.Rows[1], "the last complete scan's count")
}

// TestRunNeedsAFolder: with no folder mounted, Run fails at once and the
// page says why.
func TestRunNeedsAFolder(t *testing.T) {
	deps, _ := testDeps(t)
	t.Setenv(DirVariable, "")
	err := Connector{}.Run(t.Context(), deps, settings.NewResolved(ConnectorName, nil, nil))
	require.EqualError(t, err, "No folder is mounted for this deployment.")
	status, err := Connector{}.Status(t.Context(), deps)
	require.NoError(t, err)
	require.Equal(t, "No folder is mounted for this deployment.", status.Waiting)
	require.Nil(t, status.Rows[0].Value.(*time.Time), "no scan has run")
}

// TestRunScansAndReports: Run scans the mounted folder at once, its status
// shows the scan, and it stops when its context ends.
func TestRunScansAndReports(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A", "b.md": "", "c.md": "# C"})
	t.Setenv(DirVariable, dir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ran := make(chan error, 1)
	go func() {
		ran <- Connector{}.Run(ctx, deps, settings.NewResolved(ConnectorName, settings.Values{FieldRescanInterval: "5m0s"}, nil))
	}()
	var status settings.Status
	require.Eventually(t, func() bool {
		var err error
		status, err = Connector{}.Status(t.Context(), deps)
		require.NoError(t, err)
		return status.Rows[0].Value.(*time.Time) != nil
	}, 15*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-ran:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}

	require.Empty(t, status.Waiting)
	require.Empty(t, status.Note)
	require.Empty(t, status.Rows[0].Detail)
	require.Equal(t, []settings.StatusRow{
		{Label: "Files indexed", Value: int64(2)},
		{Label: "Files skipped", Value: int64(1), Detail: "too large, not UTF-8 text, or empty"},
	}, status.Rows[1:])
	require.ElementsMatch(t, []string{"a.md", "c.md"}, live(t, deps))
}

// TestExpandReturnsTheDocument: expand is the document's stored text, and
// another source's event is not one of its documents.
func TestExpandReturnsTheDocument(t *testing.T) {
	deps, _ := testDeps(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "# A\n\nThe whole document."})
	_, err := scan(t.Context(), deps, dir, "", maxFiles)
	require.NoError(t, err)
	var eventID int64
	require.NoError(t, deps.Pool.QueryRow(t.Context(), `SELECT id FROM events WHERE source = 'markdown'`).Scan(&eventID))

	expanded, err := Connector{}.Expand(t.Context(), deps, eventID, nil)
	require.NoError(t, err)
	require.Equal(t, source.Expanded{Content: "# A\n\nThe whole document."}, expanded)

	other, err := deps.Sink.Upsert(t.Context(), events.Event{Source: "notes", ExternalID: "a.md", Scope: events.ScopeInternal,
		OccurredAt: modified, Body: "a note"})
	require.NoError(t, err)
	_, err = Connector{}.Expand(t.Context(), deps, other.EventID, nil)
	require.ErrorIs(t, err, source.ErrNotExpandable)
	_, err = Connector{}.Expand(t.Context(), deps, 999999, nil)
	require.ErrorIs(t, err, events.ErrEventNotFound)
}
