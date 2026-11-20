package zendesk

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTicketPageWorkersWaitBeforeCheckpointing(t *testing.T) {
	started := make(chan int64, 4)
	release := make(chan struct{})
	var active, peak atomic.Int32
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental/tickets") {
			var tickets []Ticket
			for identifier := int64(12); identifier < 16; identifier++ {
				ticket := fixtureTicket()
				ticket.ID = identifier
				tickets = append(tickets, ticket)
			}
			respond(t, writer, map[string]any{"tickets": tickets, "users": []User{{ID: 10, Name: "original"}}, "after_cursor": "next", "end_of_stream": true})
			return
		}
		identifier, err := strconv.ParseInt(strings.Split(request.URL.Path, "/")[4], 10, 64)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for prior := peak.Load(); current > prior && !peak.CompareAndSwap(prior, current); prior = peak.Load() {
		}
		started <- identifier
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		comment := fixtureComments()[0]
		comment.ID = 1000 + identifier
		respond(t, writer, map[string]any{
			"comments": []Comment{comment}, "users": []User{{ID: 10, Email: fmt.Sprintf("ticket%d@example.test", identifier)}},
			"meta": map[string]any{"has_more": false},
		})
	})
	worker := testWorker(t, client)
	worker.TicketConcurrency = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	finished := make(chan error, 1)
	go func() {
		_, err := worker.SyncTickets(ctx)
		finished <- err
	}()
	t.Cleanup(func() { cancel(); <-finished }) //nolint:errcheck // shutdown error is irrelevant after cancel
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("two ticket workers did not overlap")
		}
	}
	select {
	case <-started:
		t.Fatal("ticket concurrency exceeded its configured bound")
	case <-time.After(25 * time.Millisecond):
	}
	var checkpoints int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_sync`).Scan(&checkpoints))
	require.Zero(t, checkpoints)
	close(release)
	err := <-finished
	finished <- err
	require.NoError(t, err)
	require.EqualValues(t, 2, peak.Load())
	var cursor string
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT cursor FROM zendesk_sync WHERE stream='tickets'`).Scan(&cursor))
	require.Equal(t, "next", cursor)
	for identifier := int64(12); identifier < 16; identifier++ {
		var actor string
		require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT actor_canonical FROM events WHERE external_id=$1`, ticketKey(identifier)).Scan(&actor))
		require.Equal(t, fmt.Sprintf("ticket%d@example.test", identifier), actor)
	}
}

func TestConcurrentPageFailureKeepsCursor(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v2/incremental/tickets/cursor.json":
					other := fixtureTicket()
					other.ID = 13
					respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket(), other}, "after_cursor": "next", "end_of_stream": true})
				case "/api/v2/tickets/13/comments.json":
					if deferred {
						writer.WriteHeader(http.StatusForbidden)
					} else {
						writer.Header().Set("Content-Length", "100")
						_, _ = writer.Write([]byte("{}"))
					}
				default:
					respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
				}
			})
			worker := testWorker(t, client)
			ctx := context.Background()
			_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant,stream,cursor) VALUES ($1,'tickets','original')`, client.origin)
			require.NoError(t, err)
			_, err = worker.SyncTickets(ctx)
			var cursor string
			if deferred {
				require.NoError(t, err)
				var retries int
				require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry WHERE source_id=13`).Scan(&retries))
				require.Equal(t, 1, retries)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT cursor FROM zendesk_sync WHERE stream='tickets'`).Scan(&cursor))
			if deferred {
				require.Equal(t, "next", cursor)
			} else {
				require.Equal(t, "original", cursor)
			}
		})
	}
}

func TestDuplicateTicketsAreProcessedOnceUsingCurrentState(t *testing.T) {
	var comments atomic.Int32
	client := testLookupClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "show_many.json") {
			require.Equal(t, "12", request.URL.Query().Get("ids"))
			current := fixtureTicket()
			current.Subject = "Current state"
			respond(t, writer, map[string]any{"tickets": []Ticket{current}})
			return
		}
		if strings.Contains(request.URL.Path, "incremental/tickets") {
			older := fixtureTicket()
			older.UpdatedAt = older.CreatedAt
			newer := older
			newer.UpdatedAt = older.UpdatedAt.Add(time.Hour)
			newer.Subject = "Newest snapshot"
			respond(t, writer, map[string]any{"tickets": []Ticket{newer, older}, "after_cursor": "next", "end_of_stream": true})
			return
		}
		comments.Add(1)
		respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
	})
	worker := testWorker(t, client)
	_, err := worker.SyncTickets(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, comments.Load())
	var title string
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT title FROM events WHERE external_id='ticket:12'`).Scan(&title))
	require.Equal(t, "Current state", title)
}

func TestCanceledParallelPageDoesNotAdvanceCursor(t *testing.T) {
	started := make(chan struct{}, 2)
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental/tickets") {
			other := fixtureTicket()
			other.ID = 13
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket(), other}, "after_cursor": "next", "end_of_stream": true})
			return
		}
		started <- struct{}{}
		<-request.Context().Done()
	})
	worker := testWorker(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant,stream,cursor) VALUES ($1,'tickets','original')`, client.origin)
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { _, err := worker.SyncTickets(ctx); finished <- err }()
	t.Cleanup(func() { cancel(); <-finished }) //nolint:errcheck // shutdown error is irrelevant after cancel
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("ticket workers did not start")
		}
	}
	cancel()
	err = <-finished
	finished <- err
	require.ErrorIs(t, err, context.Canceled)
	var cursor string
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT cursor FROM zendesk_sync WHERE stream='tickets'`).Scan(&cursor))
	require.Equal(t, "original", cursor)
}
