package notion

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/source"
)

// TestStatusCountsUncoveredPages: the page shows when search and the walk
// last ran, the walk requested, and the pages search found outside every
// root.
func TestStatusCountsUncoveredPages(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE notion_roots, notion_discovered_resources CASCADE`)
	require.NoError(t, err)
	searched := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `UPDATE notion_sync SET last_discovery_at = $1, last_reconcile_at = NULL,
		backfill_requested_at = NULL WHERE id = 1`, searched)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `UPDATE notion_sync SET last_discovery_at = NULL WHERE id = 1`) })
	_, err = pool.Exec(ctx, `
		INSERT INTO notion_roots (object_type, object_id, label, added_by)
		VALUES ('data_source', '11111111-1111-1111-1111-111111111111', 'Engineering', 'operator@example.com')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO notion_discovered_resources (object_type, object_id, title, url) VALUES
		('data_source', '11111111-1111-1111-1111-111111111111', 'Engineering', 'https://notion.so/engineering'),
		('data_source', '22222222-2222-2222-2222-222222222222', 'Projects', 'https://notion.so/projects')`)
	require.NoError(t, err)

	status, err := Connector{}.Status(ctx, source.Deps{Pool: pool})
	require.NoError(t, err)
	labels := make([]string, 0, len(status.Rows))
	for _, row := range status.Rows {
		labels = append(labels, row.Label)
	}
	require.Equal(t, []string{"Last search for shared pages", "Pages last read", "Last re-read requested", "Shared pages not indexed"}, labels)
	search, ok := status.Rows[0].Value.(*time.Time)
	require.True(t, ok)
	require.True(t, searched.Equal(*search))
	require.Nil(t, status.Rows[1].Value.(*time.Time))
	require.Equal(t, int64(1), status.Rows[3].Value, "the root itself is covered; the other page is not")
}
