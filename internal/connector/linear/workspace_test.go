package linear

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestAnotherWorkspaceStartsTheSyncOver: the sync state is the workspace's
// the key belonged to, so a key for another workspace clears the watermark
// and the backfill in progress, and the same workspace keeps them.
func TestAnotherWorkspaceStartsTheSyncOver(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "linear_workspace"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM source_state WHERE source = $1`, ConnectorName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `UPDATE linear_sync SET last_sync_at = NULL, backfill_requested_at = NULL,
			backfill_cursor = NULL, backfill_project = NULL, backfill_project_id = NULL WHERE id = 1`)
	})

	var workspace atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(mustMarshal(map[string]any{"data": map[string]any{
			"organization": map[string]any{"id": workspace.Load()},
		}}))
	}))
	t.Cleanup(server.Close)
	client := NewClient(NewStaticTokenProvider(syntheticToken))
	client.httpClient = server.Client()
	client.httpClient.Transport = &rewriteTransport{base: server.Client().Transport, target: server.URL}
	state := source.NewState(pool, ConnectorName)

	watermark := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	setSyncState := func() {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = $1, backfill_requested_at = $1,
			backfill_cursor = 'page-7', backfill_project = 'Roadmap', backfill_project_id = 'proj-1' WHERE id = 1`, watermark)
		require.NoError(t, err)
	}
	syncState := func() (lastSync, requested *time.Time, cursor *string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT last_sync_at, backfill_requested_at, backfill_cursor
			FROM linear_sync WHERE id = 1`).Scan(&lastSync, &requested, &cursor))
		return lastSync, requested, cursor
	}

	setSyncState()
	workspace.Store("org-a")
	reset, err := startWorkspace(ctx, pool, state, client)
	require.NoError(t, err)
	require.False(t, reset, "the first workspace seen is the one the state was kept for")
	lastSync, requested, cursor := syncState()
	require.NotNil(t, lastSync)
	require.NotNil(t, requested)
	require.NotNil(t, cursor)

	reset, err = startWorkspace(ctx, pool, state, client)
	require.NoError(t, err)
	require.False(t, reset, "the same workspace keeps its state")
	lastSync, _, _ = syncState()
	require.True(t, watermark.Equal(*lastSync))

	workspace.Store("org-b")
	reset, err = startWorkspace(ctx, pool, state, client)
	require.NoError(t, err)
	require.True(t, reset)
	lastSync, requested, cursor = syncState()
	require.Nil(t, lastSync, "another workspace's watermark is not this one's")
	require.Nil(t, requested, "nor is its backfill")
	require.Nil(t, cursor)

	setSyncState()
	reset, err = startWorkspace(ctx, pool, state, client)
	require.NoError(t, err)
	require.False(t, reset, "the new workspace is now the recorded one")
}
