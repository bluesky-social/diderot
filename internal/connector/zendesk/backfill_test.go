package zendesk

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/ratelimit"
)

type queryRecorder struct {
	mu      sync.Mutex
	queries []pgx.TraceQueryStartData
}

func (recorder *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.queries = append(recorder.queries, data)
	return ctx
}

func (*queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (recorder *queryRecorder) take() []pgx.TraceQueryStartData {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	queries := recorder.queries
	recorder.queries = nil
	return queries
}

func recordWorkerQueries(t *testing.T, worker *Worker) *queryRecorder {
	t.Helper()
	recorder := &queryRecorder{}
	config := worker.Pool.Config()
	config.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	worker.Pool = pool
	worker.Sink = events.NewDBSink(pool, prometheus.NewRegistry())
	return recorder
}

func TestTicketCleanupIsRestrictedToItsThread(t *testing.T) {
	ctx := context.Background()
	worker := testWorker(t, testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected API read") }))
	otherTicket := fixtureTicket()
	otherTicket.ID = 120
	require.NoError(t, worker.storeTicket(ctx, otherTicket, fixtureComments(), nil))
	require.NoError(t, worker.storeTicket(ctx, fixtureTicket(), fixtureComments(), nil))
	recorder := recordWorkerQueries(t, worker)
	require.NoError(t, worker.storeTicket(ctx, fixtureTicket(), fixtureComments()[:1], nil))
	foundCleanup := false
	for _, query := range recorder.take() {
		if strings.Contains(query.SQL, "starts_with(external_id") {
			foundCleanup = true
			require.Contains(t, query.SQL, "AND thread_id = $3")
			require.Equal(t, []any{"ticket:12:comment:", []string{"ticket:12:comment:21"}, "ticket:12"}, query.Args)
		}
	}
	require.True(t, foundCleanup)
	var active, deleted int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT deleted), count(*) FILTER (WHERE deleted) FROM events`).Scan(&active, &deleted))
	require.Equal(t, 5, active)
	require.Equal(t, 1, deleted)
	var removed bool
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT deleted FROM events WHERE external_id = 'ticket:12:comment:22'`).Scan(&removed))
	require.True(t, removed)
}

func TestNewAttachmentFreeEventSkipsReconciliation(t *testing.T) {
	ctx := context.Background()
	worker := testWorker(t, testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected API read") }))
	recorder := recordWorkerQueries(t, worker)
	event := events.Event{Source: "zendesk", ExternalID: "ticket:12:comment:21", Scope: events.ScopeZendesk, Body: "No attachments", OccurredAt: fixtureTicket().CreatedAt}
	require.NoError(t, worker.upsertWithAttachments(ctx, event, nil))
	transactions := 0
	for _, query := range recorder.take() {
		for _, table := range []string{"event_images", "event_documents", "zendesk_files"} {
			require.NotContains(t, query.SQL, table)
		}
		if query.SQL == "begin" {
			transactions++
		}
	}
	require.Equal(t, 1, transactions)
}

func TestExistingEventStillReconcilesRemovedAttachments(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			ctx := context.Background()
			worker := testWorker(t, testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected API read") }))
			event := events.Event{Source: "zendesk", ExternalID: "ticket:12:comment:21", Scope: events.ScopeZendesk, Body: "Attachment test", OccurredAt: fixtureTicket().CreatedAt}
			attachments := []Attachment{
				{ID: 40, Filename: "image.png", Mimetype: "image/png", ContentURL: "https://synthetic.zendesk.com/attachments/40", Size: 12},
				{ID: 41, Filename: "guide.pdf", Mimetype: "application/pdf", ContentURL: "https://synthetic.zendesk.com/attachments/41", Size: 25},
			}
			require.NoError(t, worker.upsertWithAttachments(ctx, event, attachments))
			for _, table := range []string{"event_images", "event_documents", "zendesk_files"} {
				var count int
				require.NoError(t, worker.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
				require.Positive(t, count)
			}
			if changed {
				event.Body = "Updated attachment test"
			}
			require.NoError(t, worker.upsertWithAttachments(ctx, event, nil))
			for _, table := range []string{"event_images", "event_documents", "zendesk_files"} {
				var count int
				require.NoError(t, worker.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
				require.Zero(t, count)
			}
			var images int
			require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT image_count FROM events WHERE external_id = $1`, event.ExternalID).Scan(&images))
			require.Zero(t, images)
		})
	}
}

func TestRunDrainsTicketBacklogWithoutPageDelay(t *testing.T) {
	for _, articlesPending := range []bool{false, true} {
		t.Run(fmt.Sprintf("articles_pending=%t", articlesPending), func(t *testing.T) {
			requests := make(chan time.Time, 10)
			var client *Client
			client = testClient(t, func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v2/incremental/tickets/cursor.json":
					requests <- time.Now()
					respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": request.URL.Query().Get("cursor") + "next", "end_of_stream": false})
				case "/api/v2/help_center/incremental/articles.json":
					nextPage := ""
					if articlesPending {
						nextPage = client.origin + request.URL.Path + "?start_time=0&page=" + request.URL.Query().Get("page") + "next"
					}
					respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 0, "next_page": nextPage})
				default:
					t.Errorf("unexpected request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			})
			const exportInterval = 100 * time.Millisecond
			client.exportLimiter = ratelimit.New(rate.Every(exportInterval), 1)
			worker := testWorker(t, client)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			finished := make(chan error, 1)
			go func() { finished <- worker.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				require.NoError(t, <-finished)
			})
			var times []time.Time
			for len(times) < 2 {
				select {
				case timestamp := <-requests:
					times = append(times, timestamp)
				case <-ctx.Done():
					t.Fatal("ticket backlog did not advance without a page delay")
				}
			}
			require.GreaterOrEqual(t, times[1].Sub(times[0]), exportInterval-10*time.Millisecond)
		})
	}
}

func TestRunPreservesIdleAndErrorDelays(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		retryAfter string
		interval   time.Duration
	}{
		{name: "idle", interval: time.Second},
		{name: "error", status: http.StatusForbidden, interval: time.Second},
		{name: "retry_after", status: http.StatusTooManyRequests, retryAfter: "120", interval: time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan struct{}, 10)
			client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if strings.Contains(request.URL.Path, "incremental/tickets") {
					requests <- struct{}{}
					if test.status != 0 {
						writer.Header().Set("Retry-After", test.retryAfter)
						writer.WriteHeader(test.status)
						return
					}
					respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": "next", "end_of_stream": true})
					return
				}
				respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 0})
			})
			worker := testWorker(t, client)
			worker.SyncInterval = test.interval
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			finished := make(chan error, 1)
			go func() { finished <- worker.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				require.NoError(t, <-finished)
			})
			select {
			case <-requests:
			case <-ctx.Done():
				t.Fatal("initial ticket request did not arrive")
			}
			select {
			case <-requests:
				t.Fatal("worker retried before the idle/error delay elapsed")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}
