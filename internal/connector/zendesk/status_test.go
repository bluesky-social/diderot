package zendesk

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/source"
)

// TestStatusReadsTheSyncCheckpoints: the page shows when tickets and
// articles were last read and when the reconciliation last moved.
func TestStatusReadsTheSyncCheckpoints(t *testing.T) {
	ctx := t.Context()
	worker := testWorker(t, nil)
	tickets := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, updated_at) VALUES
		('https://example.zendesk.com', 'tickets', $1),
		('https://example.zendesk.com', 'tickets-recent', $1 - interval '1 hour'),
		('https://example.zendesk.com', 'articles', $1 - interval '2 hours')`, tickets)
	require.NoError(t, err)

	status, err := Connector{}.Status(ctx, source.Deps{Pool: worker.Pool}.For(ConnectorName))
	require.NoError(t, err)
	require.Len(t, status.Rows, 4)
	require.Equal(t, "Last ticket sync", status.Rows[0].Label)
	require.True(t, tickets.Equal(*status.Rows[0].Value.(*time.Time)), "the later of the two ticket streams")
	require.Equal(t, "Last article sync", status.Rows[1].Label)
	require.True(t, tickets.Add(-2*time.Hour).Equal(*status.Rows[1].Value.(*time.Time)))
	require.Equal(t, "Last reconcile", status.Rows[2].Label)
	require.Nil(t, status.Rows[2].Value.(*time.Time), "never reconciled")
}
