package zendesk

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientAcceptsEmptyTerminalPage(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		body       string
		wantCursor string
	}{
		{"null cursor", `{"tickets":[],"after_cursor":null,"end_of_stream":true}`, "saved"},
		{"missing cursor", `{"tickets":[],"end_of_stream":true}`, "saved"},
		{"empty cursor", `{"tickets":[],"after_cursor":"","end_of_stream":true}`, "saved"},
		{"unchanged cursor", `{"tickets":[],"after_cursor":"saved","end_of_stream":true}`, "saved"},
		{"new cursor", `{"tickets":[],"after_cursor":"next","end_of_stream":true}`, "next"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "saved", request.URL.Query().Get("cursor"))
				_, _ = writer.Write([]byte(testCase.body))
			})
			page, err := client.Tickets(context.Background(), "saved")
			require.NoError(t, err)
			require.NotNil(t, page.Tickets)
			require.Empty(t, page.Tickets)
			require.Equal(t, testCase.wantCursor, page.AfterCursor)
			require.NotNil(t, page.EndOfStream)
			require.True(t, *page.EndOfStream)
		})
	}
}

func TestClientRejectsEmptyTerminalPageWithoutSavedCursor(t *testing.T) {
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": nil, "end_of_stream": true})
	})
	_, err := client.Tickets(context.Background(), "")
	require.ErrorContains(t, err, "incomplete ticket export pagination")
}

func TestTicketSyncPreservesCheckpointAcrossEmptyTerminalPages(t *testing.T) {
	for _, stream := range []string{"tickets", "tickets-recent"} {
		t.Run(stream, func(t *testing.T) {
			ctx := context.Background()
			var exports int
			client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v2/incremental/tickets/cursor.json":
					require.Equal(t, "saved", request.URL.Query().Get("cursor"))
					require.Empty(t, request.URL.Query().Get("start_time"))
					exports++
					if exports <= 2 {
						respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": nil, "end_of_stream": true})
						return
					}
					respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "advanced", "end_of_stream": true})
				case "/api/v2/tickets/12/comments.json":
					respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
				default:
					t.Errorf("unexpected request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			})
			worker := testWorker(t, client)
			worker.RecentLookback = 30 * 24 * time.Hour
			watermark := time.Now().Add(-worker.RecentLookback).Unix()
			_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, cursor, watermark)
				VALUES ($1, $2, 'saved', $3)`, client.origin, stream, watermark)
			require.NoError(t, err)
			syncTickets := worker.SyncTickets
			if stream == "tickets-recent" {
				syncTickets = worker.SyncRecentTickets
			}
			for _, wantCursor := range []string{"saved", "saved", "advanced"} {
				caughtUp, err := syncTickets(ctx)
				require.NoError(t, err)
				require.True(t, caughtUp)
				var cursor string
				var savedWatermark int64
				require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT cursor, watermark FROM zendesk_sync
					WHERE tenant = $1 AND stream = $2`, client.origin, stream).Scan(&cursor, &savedWatermark))
				require.Equal(t, wantCursor, cursor)
				require.Equal(t, watermark, savedWatermark)
			}
			var eventCount int
			require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'zendesk'`).Scan(&eventCount))
			require.Equal(t, 3, eventCount)
		})
	}
}
