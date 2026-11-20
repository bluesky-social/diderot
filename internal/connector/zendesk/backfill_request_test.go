package zendesk

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/source"
)

// TestBackfillRereadsTicketsAndArticles: a requested backfill waits until
// the sync worker's next pass, which forgets where the ticket and article
// streams had read to, and starts only once per request.
func TestBackfillRereadsTicketsAndArticles(t *testing.T) {
	ctx := t.Context()
	worker := testWorker(t, testClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, cursor, watermark) VALUES
		($1, 'tickets', 'ticket-cursor', 0), ($1, 'tickets-recent', 'recent-cursor', 7),
		($1, 'articles', '', 99), ($1, 'reconcile', '12', 0)`, worker.Client.origin)
	require.NoError(t, err)
	deps := source.Deps{Pool: worker.Pool}.For(ConnectorName)
	backfillRow := func() *time.Time {
		t.Helper()
		status, err := Connector{}.Status(ctx, deps)
		require.NoError(t, err)
		row := status.Rows[len(status.Rows)-1]
		require.Equal(t, "Backfill requested", row.Label)
		return row.Value.(*time.Time)
	}
	streams := func() []string {
		t.Helper()
		rows, err := worker.Pool.Query(ctx, `SELECT stream FROM zendesk_sync ORDER BY stream`)
		require.NoError(t, err)
		var names []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		return names
	}

	started, err := worker.startBackfill(ctx)
	require.NoError(t, err)
	require.False(t, started, "nothing was requested")
	require.Nil(t, backfillRow())

	requestedAt, err := Connector{}.RequestBackfill(ctx, deps, nil)
	require.NoError(t, err)
	require.True(t, requestedAt.Equal(*backfillRow()))
	require.Len(t, streams(), 4, "the request waits for the sync worker")

	started, err = worker.startBackfill(ctx)
	require.NoError(t, err)
	require.True(t, started)
	require.Equal(t, []string{"reconcile"}, streams(), "tickets and articles are read from the start again")
	require.Nil(t, backfillRow())

	started, err = worker.startBackfill(ctx)
	require.NoError(t, err)
	require.False(t, started, "one request starts one backfill")

	_, err = Connector{}.RequestBackfill(ctx, deps, nil)
	require.NoError(t, err)
	require.NotNil(t, backfillRow(), "a later request waits again")
}
