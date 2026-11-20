package zendesk

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconciliationAppliesMissedDeletesAndArticleVisibility(t *testing.T) {
	ctx := context.Background()
	fixture := &articleFixture{public: true}
	calls := 0
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if strings.HasPrefix(request.URL.Path, "/api/v2/tickets/") {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		fixture.serve(t, writer, request)
	})
	worker := testWorker(t, client)
	require.NoError(t, worker.storeTicket(ctx, fixtureTicket(), fixtureComments(), nil))
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	fixture.public = false
	caughtUp, err := worker.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted AND (thread_id = 'ticket:12' OR scope = 'public')`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted AND scope = 'corpus:zendesk'`).Scan(&count))
	require.Equal(t, 1, count)
	previousCalls := calls
	caughtUp, err = worker.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, previousCalls, calls)
}

func TestReconciliationAndRetryPassesAreBoundedAndDurable(t *testing.T) {
	ctx := context.Background()
	calls := 0
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusForbidden)
	})
	worker := testWorker(t, client)
	for index := range 11 {
		ticket := fixtureTicket()
		ticket.ID = int64(index + 1)
		ticket.Subject = fmt.Sprintf("Ticket %d", ticket.ID)
		require.NoError(t, worker.storeTicket(ctx, ticket, nil, nil))
	}
	caughtUp, err := worker.Reconcile(ctx)
	require.NoError(t, err)
	require.False(t, caughtUp)
	require.Equal(t, 10, calls)
	resumed := &Worker{Pool: worker.Pool, Sink: worker.Sink, Client: client}
	caughtUp, err = resumed.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, 11, calls)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry`).Scan(&count))
	require.Equal(t, 11, count)
	_, err = worker.Pool.Exec(ctx, `UPDATE zendesk_retry SET not_before = now()`)
	require.NoError(t, err)
	require.NoError(t, resumed.RetryDeferred(ctx))
	require.Equal(t, 21, calls)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry WHERE attempts = 1 AND not_before > now()`).Scan(&count))
	require.Equal(t, 10, count)
}
