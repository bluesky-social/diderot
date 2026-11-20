package linear

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestStatusReadsTheSyncState: the page shows when each loop last ran and
// the backfill waiting, with its project.
func TestStatusReadsTheSyncState(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "linear_status"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = $1, last_reconcile_at = NULL,
		visibility_swept_at = $1, backfill_requested_at = $1, backfill_project = 'Roadmap' WHERE id = 1`, at)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `UPDATE linear_sync SET last_sync_at = NULL, visibility_swept_at = NULL,
			backfill_requested_at = NULL, backfill_project = NULL WHERE id = 1`)
	})

	status, err := Connector{}.Status(ctx, source.Deps{Pool: pool})
	require.NoError(t, err)
	require.Len(t, status.Rows, 4)
	when := func(row settings.StatusRow) *time.Time {
		t.Helper()
		value, ok := row.Value.(*time.Time)
		require.True(t, ok, "%s is a time", row.Label)
		return value
	}
	require.Equal(t, "Last sync", status.Rows[0].Label)
	require.True(t, at.Equal(*when(status.Rows[0])))
	require.Equal(t, "Last reconcile", status.Rows[1].Label)
	require.Nil(t, when(status.Rows[1]))
	require.Equal(t, "Private-team sweep", status.Rows[2].Label)
	require.Equal(t, settings.StatusRow{Label: "Backfill requested", Value: when(status.Rows[3]), Detail: "Roadmap"}, status.Rows[3])
	require.True(t, at.Equal(*when(status.Rows[3])))
}
