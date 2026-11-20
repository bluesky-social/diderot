package zendesk

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/ratelimit"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// testClient answers current-state lookups (show_many) with fixtureTicket
// under each requested ID (open, in indexedGroup), and the purge sweep's group list and searches
// with nothing to purge; testLookupClient leaves all of these to the handler.
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	return testLookupClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/groups.json":
			respond(t, writer, map[string]any{"groups": []map[string]any{{"id": indexedGroup}}, "meta": map[string]any{"has_more": false}})
			return
		case "/api/v2/search/export.json":
			respond(t, writer, map[string]any{"results": []any{}, "meta": map[string]any{"has_more": false}})
			return
		}
		if request.URL.Path == "/api/v2/tickets/show_many.json" {
			var tickets []Ticket
			for _, raw := range strings.Split(request.URL.Query().Get("ids"), ",") {
				id, err := strconv.ParseInt(raw, 10, 64)
				require.NoError(t, err)
				ticket := fixtureTicket()
				ticket.ID = id
				tickets = append(tickets, ticket)
			}
			respond(t, writer, map[string]any{"tickets": tickets})
			return
		}
		handler(writer, request)
	})
}

func testLookupClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	return testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/oauth/tokens" {
			require.Equal(t, http.MethodPost, request.Method)
			require.Empty(t, request.Header.Get("Authorization"))
			var payload map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
			require.Equal(t, map[string]any{
				"grant_type": "client_credentials", "client_id": "synthetic-client", "client_secret": "synthetic-secret",
				"scope": "read", "expires_in": float64(1800),
			}, payload)
			respond(t, writer, map[string]any{"access_token": "synthetic-token", "token_type": "bearer", "scope": "read", "expires_in": 1800})
			return
		}
		handler(writer, request)
	})
}

func testOAuthClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
	require.NoError(t, err)
	client.origin = server.URL
	client.limiter = ratelimit.New(rate.Inf, 1)
	client.exportLimiter = ratelimit.New(rate.Inf, 1)
	return client
}

func testWorker(t *testing.T, client *Client) *Worker {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "zendesk"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `TRUNCATE events, zendesk_sync, zendesk_retry CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM source_state WHERE source = $1`, ConnectorName)
	require.NoError(t, err)
	return &Worker{
		Client: client, Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()), IndexedGroups: []int64{indexedGroup},
		State: source.NewState(pool, ConnectorName),
	}
}

const indexedGroup = 1

func fixtureTicket() Ticket {
	return Ticket{ID: 12, Subject: "Synthetic issue", Status: "open", RequesterID: 10, GroupID: indexedGroup, CreatedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
}

func fixtureComments() []Comment {
	return []Comment{
		{ID: 21, AuthorID: 10, Body: "Synthetic user report", Public: true, CreatedAt: fixtureTicket().CreatedAt},
		{ID: 22, AuthorID: 11, Body: "Synthetic private note", Public: false, CreatedAt: fixtureTicket().CreatedAt.Add(time.Hour)},
	}
}

func respond(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(writer).Encode(value))
}

func TestTicketSyncResumesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	var cursors []string
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer synthetic-token", request.Header.Get("Authorization"))
		require.Equal(t, http.MethodGet, request.Method)
		switch request.URL.Path {
		case "/api/v2/incremental/tickets/cursor.json":
			cursors = append(cursors, request.URL.Query().Get("cursor"))
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "next", "end_of_stream": true})
		case "/api/v2/tickets/12/comments.json":
			comments := fixtureComments()
			if request.URL.Query().Get("page[after]") == "" {
				respond(t, writer, map[string]any{"comments": comments[:1], "meta": map[string]any{"has_more": true, "after_cursor": "comments-next"}})
			} else {
				require.Equal(t, "comments-next", request.URL.Query().Get("page[after]"))
				respond(t, writer, map[string]any{"comments": comments[1:], "meta": map[string]any{"has_more": false}, "users": []User{{ID: 11, Name: "Synthetic Agent", Email: "agent@example.test"}}})
			}
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)
	caughtUp, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	var count int
	var firstChunk int64
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*), min(id) FROM event_chunks`).Scan(&count, &firstChunk))
	require.Equal(t, 3, count)
	caughtUp, err = worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, []string{"", "next"}, cursors)
	var finalChunk int64
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*), min(id) FROM event_chunks`).Scan(&count, &finalChunk))
	require.Equal(t, 3, count)
	require.Equal(t, firstChunk, finalChunk)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE scope = 'corpus:zendesk'`).Scan(&count))
	require.Equal(t, 3, count)
	var author string
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT actor_canonical FROM events WHERE external_id = 'ticket:12:comment:22'`).Scan(&author))
	require.Equal(t, "agent@example.test", author)
}

func TestTicketSyncDurablyDefersUnreadableTicket(t *testing.T) {
	ctx := context.Background()
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental") {
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "must-not-save", "end_of_stream": true})
			return
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte("SENSITIVE UPSTREAM CONTENT"))
	})
	worker := testWorker(t, client)
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_sync`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry WHERE source_id = 12`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestTicketSyncAppliesEditsAndDeletion(t *testing.T) {
	ctx := context.Background()
	deleted := false
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		ticket := fixtureTicket()
		if deleted {
			ticket.Status = "deleted"
		}
		if strings.Contains(request.URL.Path, "incremental") {
			respond(t, writer, map[string]any{"tickets": []Ticket{ticket}, "after_cursor": "next", "end_of_stream": true})
			return
		}
		comments := fixtureComments()
		comments[1].Body = "Redacted"
		respond(t, writer, map[string]any{"comments": comments, "meta": map[string]any{"has_more": false}})
	})
	worker := testWorker(t, client)
	require.NoError(t, worker.storeTicket(ctx, fixtureTicket(), fixtureComments(), nil))
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	var body string
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT body FROM events WHERE external_id = 'ticket:12:comment:22'`).Scan(&body))
	require.Contains(t, body, "Redacted")
	require.NotContains(t, body, "Synthetic private note")
	deleted = true
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks`).Scan(&count))
	require.Zero(t, count)
}

func TestClientRejectsIncompleteOrRepeatingPagination(t *testing.T) {
	for _, body := range []string{
		`{"tickets":[{"id":12}],"end_of_stream":true}`,
		`{"tickets":[{"id":12}],"after_cursor":null,"end_of_stream":true}`,
		`{"tickets":[],"after_cursor":null,"end_of_stream":false}`,
		`{"tickets":[],"after_cursor":null}`,
		`{"tickets":null,"after_cursor":null,"end_of_stream":true}`,
		`{"after_cursor":null,"end_of_stream":true}`,
		`{"tickets":[],"after_cursor":"next"}`,
		`{"tickets":[],"after_cursor":"next","end_of_stream":false}`,
	} {
		client := testClient(t, func(writer http.ResponseWriter, request *http.Request) { _, _ = writer.Write([]byte(body)) })
		_, err := client.Tickets(context.Background(), "next")
		require.Error(t, err)
	}
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		respond(t, writer, map[string]any{"comments": []Comment{}, "meta": map[string]any{"has_more": true, "after_cursor": "repeat"}})
	})
	_, _, err := client.Comments(context.Background(), 12)
	require.ErrorContains(t, err, "did not advance")
}

func TestPoisonTicketDoesNotBlockLaterTicketsAndRetries(t *testing.T) {
	ctx := context.Background()
	readable := false
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental") {
			later := fixtureTicket()
			later.ID = 13
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket(), later}, "after_cursor": "next", "end_of_stream": true})
		} else if request.URL.Path == "/api/v2/tickets/12.json" {
			respond(t, writer, map[string]any{"ticket": fixtureTicket()})
		} else if request.URL.Path == "/api/v2/tickets/12/comments.json" && !readable {
			writer.WriteHeader(http.StatusForbidden)
		} else {
			respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
		}
	})
	worker := testWorker(t, client)
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = 'ticket:13' AND NOT deleted`).Scan(&count))
	require.Equal(t, 3, count)
	_, err = worker.Pool.Exec(ctx, `UPDATE zendesk_retry SET not_before = now()`)
	require.NoError(t, err)
	require.NoError(t, worker.RetryDeferred(ctx))
	var attempts int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT attempts FROM zendesk_retry WHERE source_id = 12`).Scan(&attempts))
	require.Equal(t, 1, attempts)
	readable = true
	_, err = worker.Pool.Exec(ctx, `UPDATE zendesk_retry SET not_before = now()`)
	require.NoError(t, err)
	require.NoError(t, worker.RetryDeferred(ctx))
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = 'ticket:12' AND NOT deleted`).Scan(&count))
	require.Equal(t, 3, count)
}

func TestFailedRetryPersistenceHoldsCursor(t *testing.T) {
	ctx := context.Background()
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "incremental") {
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "next", "end_of_stream": true})
		} else {
			writer.WriteHeader(http.StatusForbidden)
		}
	})
	worker := testWorker(t, client)
	_, err := worker.Pool.Exec(ctx, `ALTER TABLE zendesk_retry ADD CONSTRAINT reject_fixture CHECK (source_id <> 12)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = worker.Pool.Exec(ctx, `ALTER TABLE zendesk_retry DROP CONSTRAINT reject_fixture`)
	})
	_, err = worker.SyncTickets(ctx)
	require.Error(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_sync`).Scan(&count))
	require.Zero(t, count)
}

func TestClientDoesNotFollowRedirectsOrAuthenticateAnonymousReads(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { destinationCalls++ }))
	defer destination.Close()
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.Empty(t, request.Header.Get("Authorization"))
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	})
	var output any
	err := client.get(context.Background(), "/api/v2/help_center/en-us/articles/1.json", nil, false, &output)
	require.ErrorContains(t, err, "302")
	require.Zero(t, destinationCalls)
}

func TestCredentialsAndRetryDelay(t *testing.T) {
	for _, subdomain := range []string{"", "https://synthetic.zendesk.com", "synthetic/evil", "synthetic.example", "-bad"} {
		_, err := NewClient(subdomain, "synthetic-client", "synthetic-secret", 300)
		require.Error(t, err)
	}
	for _, clientID := range []string{"", "client\nidentifier", " client "} {
		require.Error(t, ValidateCredentials("synthetic", clientID, "synthetic-secret"))
	}
	for _, secret := range []string{"", " secret ", "secret\nvalue"} {
		require.Error(t, ValidateCredentials("synthetic", "synthetic-client", secret))
	}
	require.Equal(t, 12*time.Second, retryDelay("12"))
	require.Zero(t, retryDelay("not a duration"))
}

func TestOAuthExchangeFailureHoldsIngestionCheckpoints(t *testing.T) {
	client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/oauth/tokens", request.URL.Path)
		writer.WriteHeader(http.StatusUnauthorized)
	})
	worker := testWorker(t, client)
	ctx := context.Background()
	_, err := worker.SyncTickets(ctx)
	require.Error(t, err)
	_, err = worker.SyncArticles(ctx)
	require.Error(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_sync`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry`).Scan(&count))
	require.Zero(t, count)
}

func TestUnindexedGroupTicketsAreTombstonedAndPurged(t *testing.T) {
	ctx := context.Background()
	currentGroup := int64(indexedGroup)
	commentFetches, ticketFetches := 0, 0
	var searches []string
	client := testLookupClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/incremental/tickets/cursor.json":
			// The snapshot still shows ticket 12 in the indexed group; 13 has no group.
			ungrouped := fixtureTicket()
			ungrouped.ID, ungrouped.GroupID = 13, 0
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket(), ungrouped}, "after_cursor": "next", "end_of_stream": true})
		case "/api/v2/tickets/show_many.json":
			current, ungrouped := fixtureTicket(), fixtureTicket()
			current.GroupID = currentGroup
			ungrouped.ID, ungrouped.GroupID = 13, 0
			respond(t, writer, map[string]any{"tickets": []Ticket{current, ungrouped}})
		case "/api/v2/tickets/12.json":
			ticketFetches++
			current := fixtureTicket()
			current.GroupID = currentGroup
			respond(t, writer, map[string]any{"ticket": current})
		case "/api/v2/tickets/12/comments.json":
			commentFetches++
			respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
		case "/api/v2/groups.json":
			respond(t, writer, map[string]any{"groups": []map[string]any{{"id": indexedGroup}, {"id": 7}}, "meta": map[string]any{"has_more": false}})
		case "/api/v2/search/export.json":
			query, after := request.URL.Query().Get("query"), request.URL.Query().Get("page[after]")
			require.Equal(t, "ticket", request.URL.Query().Get("filter[type]"))
			searches = append(searches, query+"|"+after)
			switch {
			case query == "type:ticket group:none":
				respond(t, writer, map[string]any{"results": []map[string]any{{"id": 13}}, "meta": map[string]any{"has_more": false}})
			case query == "type:ticket group:7" && after == "":
				respond(t, writer, map[string]any{"results": []map[string]any{{"id": 12}}, "meta": map[string]any{"has_more": true, "after_cursor": "search-next"}})
			case query == "type:ticket group:7":
				respond(t, writer, map[string]any{"results": []map[string]any{{"id": 99}}, "meta": map[string]any{"has_more": false}})
			default:
				t.Errorf("unexpected search: %s", query)
			}
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)
	live := func() int {
		var count int
		require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'zendesk' AND NOT deleted`).Scan(&count))
		return count
	}

	// Only the ticket in an indexed group is stored; the ungrouped one is never read.
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, live())
	require.Equal(t, 1, commentFetches)

	// It moves out of the indexed groups: the sweep finds and tombstones it.
	currentGroup = 7
	require.NoError(t, worker.PurgeUnindexedGroups(ctx))
	require.Equal(t, 0, live())
	require.Equal(t, []string{"type:ticket group:none|", "type:ticket group:7|", "type:ticket group:7|search-next"}, searches)
	require.Equal(t, 1, ticketFetches) // only the still-indexed hit (12) is re-read, not 13 or 99
	require.NoError(t, worker.PurgeUnindexedGroups(ctx))
	require.Equal(t, 1, ticketFetches) // already tombstoned: no further reads
	var chunks int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks`).Scan(&chunks))
	require.Zero(t, chunks)

	// A stale export snapshot can't resurrect it: sync judges the current group.
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, live())
	require.Equal(t, 1, commentFetches)
}

func TestTicketsFailClosedWhenUnreadable(t *testing.T) {
	ctx := context.Background()
	readable := true
	client := testLookupClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/incremental/tickets/cursor.json":
			respond(t, writer, map[string]any{"tickets": []Ticket{fixtureTicket()}, "after_cursor": "next", "end_of_stream": true})
		case "/api/v2/tickets/show_many.json":
			tickets := []Ticket{}
			if readable {
				tickets = append(tickets, fixtureTicket())
			}
			respond(t, writer, map[string]any{"tickets": tickets})
		case "/api/v2/tickets/12.json":
			writer.WriteHeader(http.StatusForbidden)
		case "/api/v2/tickets/12/comments.json":
			respond(t, writer, map[string]any{"comments": fixtureComments(), "meta": map[string]any{"has_more": false}})
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)
	count := func(query string) int {
		var n int
		require.NoError(t, worker.Pool.QueryRow(ctx, query).Scan(&n))
		return n
	}
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count(`SELECT count(*) FROM events WHERE source = 'zendesk' AND NOT deleted`))

	// The lookup no longer returns it: the indexed copy goes and a retry is queued.
	readable = false
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)
	require.Zero(t, count(`SELECT count(*) FROM events WHERE source = 'zendesk' AND NOT deleted`))
	require.Equal(t, 1, count(`SELECT count(*) FROM zendesk_retry`))

	// A retry that is still forbidden keeps its retry record and backs off.
	_, err = worker.Pool.Exec(ctx, `UPDATE zendesk_retry SET not_before = now() - interval '1 second'`)
	require.NoError(t, err)
	require.NoError(t, worker.RetryDeferred(ctx))
	require.Equal(t, 1, count(`SELECT attempts FROM zendesk_retry WHERE source_id = 12`))
}

func TestRunDoesNotReportStepsCutShortByStoppingAsErrors(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	worker := testWorker(t, testLookupClient(t, func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	logs := &lockedBuffer{}
	worker.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	require.NoError(t, <-done)
	require.Contains(t, logs.String(), "level=DEBUG", "the interrupted step is still traceable")
	require.Contains(t, logs.String(), "context canceled")
	require.NotContains(t, logs.String(), "level=ERROR")
	require.NotContains(t, logs.String(), "level=WARN")
}

// lockedBuffer is a log destination goroutines can share.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
