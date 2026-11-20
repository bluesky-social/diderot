package zendesk

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecentPassResumesWithoutChangingHistoryCheckpoint(t *testing.T) {
	var requests atomic.Int32
	var firstStart int64
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "incremental/tickets") {
			respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
			return
		}
		switch requests.Add(1) {
		case 1:
			var err error
			firstStart, err = strconv.ParseInt(request.URL.Query().Get("start_time"), 10, 64)
			require.NoError(t, err)
			require.InDelta(t, time.Now().Add(-30*24*time.Hour).Unix(), firstStart, 5)
			require.Empty(t, request.URL.Query().Get("cursor"))
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "recent-next", "end_of_stream": false})
		case 2:
			require.Equal(t, "recent-next", request.URL.Query().Get("cursor"))
			require.Empty(t, request.URL.Query().Get("start_time"))
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "recent-end", "end_of_stream": true})
		case 3:
			require.Equal(t, "historical-checkpoint", request.URL.Query().Get("cursor"))
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "historical-next", "end_of_stream": false})
		default:
			t.Error("unexpected ticket request")
			writer.WriteHeader(http.StatusBadRequest)
		}
	})
	worker := testWorker(t, client)
	worker.RecentLookback = 30 * 24 * time.Hour
	ctx := context.Background()
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant,stream,cursor) VALUES ($1,'tickets','historical-checkpoint')`, client.origin)
	require.NoError(t, err)
	caughtUp, err := worker.SyncRecentTickets(ctx)
	require.NoError(t, err)
	require.False(t, caughtUp)
	restarted := &Worker{Client: client, Pool: worker.Pool, Sink: worker.Sink, RecentLookback: time.Hour}
	caughtUp, err = restarted.SyncRecentTickets(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	var cursor string
	var watermark int64
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT cursor,watermark FROM zendesk_sync WHERE stream='tickets-recent'`).Scan(&cursor, &watermark))
	require.Equal(t, "recent-end", cursor)
	require.Equal(t, firstStart, watermark)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT cursor FROM zendesk_sync WHERE stream='tickets'`).Scan(&cursor))
	require.Equal(t, "historical-checkpoint", cursor)
	caughtUp, err = restarted.SyncTickets(ctx)
	require.NoError(t, err)
	require.False(t, caughtUp)
	var roots int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source='zendesk' AND external_id=thread_id`).Scan(&roots))
	require.Equal(t, 1, roots)
}

func TestRecentInitialCutoffSurvivesFailedFirstPage(t *testing.T) {
	var starts []string
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		starts = append(starts, request.URL.Query().Get("start_time"))
		writer.WriteHeader(http.StatusForbidden)
	})
	worker := testWorker(t, client)
	worker.RecentLookback = 30 * 24 * time.Hour
	_, err := worker.SyncRecentTickets(context.Background())
	require.Error(t, err)
	worker.RecentLookback = time.Hour
	_, err = worker.SyncRecentTickets(context.Background())
	require.Error(t, err)
	require.Len(t, starts, 2)
	require.Equal(t, starts[0], starts[1])
	var cursor string
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT cursor FROM zendesk_sync WHERE stream='tickets-recent'`).Scan(&cursor))
	require.Empty(t, cursor)
}

func TestRunPrioritizesRecentPagesThenResumesHistory(t *testing.T) {
	streams := make(chan string, 10)
	var articleCalls, activeComments, peakComments atomic.Int32
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/incremental/tickets/cursor.json":
			cursor := request.URL.Query().Get("cursor")
			stream, next, caughtUp := "history", "historical-next", false
			if request.URL.Query().Get("start_time") != "" && request.URL.Query().Get("start_time") != "0" {
				stream, next = "recent", "recent-next"
			} else if strings.HasPrefix(cursor, "recent") {
				stream, next, caughtUp = "recent", "recent-end", true
			} else if cursor == "historical-next" {
				next = "historical-end"
			}
			streams <- stream
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": next, "end_of_stream": caughtUp})
		case "/api/v2/tickets/12/comments.json":
			active := activeComments.Add(1)
			defer activeComments.Add(-1)
			for peak := peakComments.Load(); active > peak && !peakComments.CompareAndSwap(peak, active); peak = peakComments.Load() {
			}
			respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
		case "/api/v2/help_center/incremental/articles.json":
			articleCalls.Add(1)
			respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 0})
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)
	worker.RecentLookback = 30 * 24 * time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	finished := make(chan error, 1)
	go func() { finished <- worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-finished) })
	for _, expected := range []string{"recent", "recent", "history", "history"} {
		select {
		case stream := <-streams:
			require.Equal(t, expected, stream)
		case <-ctx.Done():
			t.Fatal("recent and historical streams did not make progress")
		}
	}
	require.EqualValues(t, 1, peakComments.Load())
	require.EqualValues(t, 1, articleCalls.Load())
}

func TestRecentPollingDoesNotWaitForHistoricalIdleInterval(t *testing.T) {
	var exports atomic.Int32
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental/tickets") {
			exports.Add(1)
			cursor := request.URL.Query().Get("cursor")
			if cursor == "" {
				if request.URL.Query().Get("start_time") == "0" {
					cursor = "history"
				} else {
					cursor = "recent"
				}
			}
			respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": cursor, "end_of_stream": true})
			return
		}
		respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 0})
	})
	worker := testWorker(t, client)
	worker.RecentLookback = time.Hour
	worker.SyncInterval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	require.NoError(t, worker.Run(ctx))
	require.GreaterOrEqual(t, exports.Load(), int32(4))
}
