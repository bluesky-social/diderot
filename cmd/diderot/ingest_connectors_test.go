package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	linearcx "github.com/bluesky-social/diderot/internal/connector/linear"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	zendeskcx "github.com/bluesky-social/diderot/internal/connector/zendesk"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/bluesky-social/diderot/internal/worker"
)

// fakeUpstream records the paths a connector's workers request and answers
// each with route's reply.
type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  map[string]int
}

func newFakeUpstream(t *testing.T, route func(r *http.Request) (int, any)) *fakeUpstream {
	t.Helper()
	fake := &fakeUpstream{paths: map[string]int{}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.paths[r.URL.Path]++
		fake.mu.Unlock()
		status, body := route(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeUpstream) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[path] > 0
}

func ingestTestPool(t *testing.T, key string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, key))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// testDeps is what a source's Run is handed in these tests: its image
// fetcher slot is images, and its document worker logs to logger.
func testDeps(pool *pgxpool.Pool, eventSource string, logger *slog.Logger, images *lentImages) source.Deps {
	return source.Deps{
		Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()), Logger: logger, Metrics: settings.Metrics{},
		Files: &sourceFiles{
			eventSource: eventSource, images: images, cfg: &config.Config{}, models: &inferenceModels{}, pool: pool, logger: logger,
		},
	}
}

// runSource starts src.Run, waits for started to report its workers
// running, then cancels it and requires Run to return promptly.
func runSource(t *testing.T, src source.Source, deps source.Deps, resolved settings.Resolved, started func() bool, whileRunning func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx, deps, resolved) }()
	require.Eventually(t, started, 15*time.Second, 10*time.Millisecond, "the connector's workers never reached the source")
	if whileRunning != nil {
		whileRunning()
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// TestNotionSourceRun: Run starts the sync worker against the configured
// secret, lends the image fetcher while it runs, and stops on cancel.
func TestNotionSourceRun(t *testing.T) {
	pool := ingestTestPool(t, "cmd_notion_connector")
	fake := newFakeUpstream(t, func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "Bearer notion-fixture" {
			return http.StatusUnauthorized, map[string]any{}
		}
		switch r.URL.Path {
		case "/users/me":
			return http.StatusOK, map[string]any{"object": "user", "id": "bot-1", "type": "bot"}
		case "/search":
			return http.StatusOK, map[string]any{"object": "list", "results": []any{}, "has_more": false}
		}
		return http.StatusNotFound, map[string]any{}
	})
	images := &lentImages{}
	resolved := settings.NewResolved(notioncx.ConnectorName, nil, map[string]string{notioncx.FieldToken: "notion-fixture"})
	runSource(t, notioncx.Connector{BaseURL: fake.server.URL}, testDeps(pool, "notion", discardLogger(), images), resolved,
		func() bool { return fake.requested("/search") }, func() {
			require.True(t, images.Available(), "the image fetcher is lent while Notion runs")
		})
	require.True(t, fake.requested("/users/me"))
	require.False(t, images.Available(), "the image fetcher is taken back when Notion stops")
}

// redirectTransport sends every request to target, for clients whose API
// origin is fixed.
type redirectTransport struct{ target *url.URL }

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = t.target.Scheme, t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func redirectedClient(t *testing.T, target string) *http.Client {
	t.Helper()
	parsed, err := url.Parse(target)
	require.NoError(t, err)
	return &http.Client{Transport: redirectTransport{target: parsed}}
}

// TestLinearSourceRun: Run starts the sync worker with the configured key
// and stops on cancel; settings without usable credentials don't start it.
func TestLinearSourceRun(t *testing.T) {
	pool := ingestTestPool(t, "cmd_linear_connector")
	var issuesAsked atomic.Bool
	fake := newFakeUpstream(t, func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "linear-fixture" {
			return http.StatusUnauthorized, map[string]any{}
		}
		var request struct{ Query string }
		if json.NewDecoder(r.Body).Decode(&request) == nil && strings.Contains(request.Query, "issues(") {
			issuesAsked.Store(true)
		}
		return http.StatusOK, map[string]any{"data": map[string]any{
			"organization": map[string]any{"id": "org-1"},
			"issues":       map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
		}}
	})
	connector := linearcx.Connector{HTTP: redirectedClient(t, fake.server.URL)}
	deps := testDeps(pool, "linear", discardLogger(), &lentImages{}).For(linearcx.ConnectorName)
	resolved := settings.NewResolved(linearcx.ConnectorName,
		settings.Values{linearcx.FieldExcludeTeams: []string{"SUP"}}, map[string]string{linearcx.FieldToken: "linear-fixture"})
	runSource(t, connector, deps, resolved, issuesAsked.Load, nil)

	err := connector.Run(context.Background(), deps, settings.NewResolved(linearcx.ConnectorName, nil, nil))
	require.Error(t, err, "no credentials")
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestZendeskSourceRun: Run signs in and starts the sync worker, lends
// the attachment fetcher while it runs, and starts a document worker exactly
// when Zendesk's own Index documents setting is on.
func TestZendeskSourceRun(t *testing.T) {
	pool := ingestTestPool(t, "cmd_zendesk_connector")
	for _, docIngest := range []bool{false, true} {
		fake := newFakeUpstream(t, func(r *http.Request) (int, any) {
			switch r.URL.Path {
			case "/oauth/tokens":
				return http.StatusOK, map[string]any{"access_token": "granted", "token_type": "bearer", "scope": "read", "expires_in": 1800}
			case "/api/v2/groups.json":
				return http.StatusOK, map[string]any{"groups": []map[string]any{{"id": 7}}, "meta": map[string]any{"has_more": false}}
			case "/api/v2/search/export.json":
				return http.StatusOK, map[string]any{"results": []any{}, "meta": map[string]any{"has_more": false}}
			}
			return http.StatusNotFound, map[string]any{}
		})
		var logs lockedBuffer
		images := &lentImages{}
		deps := testDeps(pool, "zendesk", slog.New(slog.NewTextHandler(&logs, nil)), images).For(zendeskcx.ConnectorName)
		resolved := settings.NewResolved(zendeskcx.ConnectorName, settings.Values{
			zendeskcx.FieldSubdomain: "example", zendeskcx.FieldOAuthClientID: "diderot-ingest",
			zendeskcx.FieldIndexedGroupIDs: []string{"7"}, zendeskcx.FieldDocIngest: docIngest,
		}, map[string]string{zendeskcx.FieldOAuthSecret: "zendesk-fixture"})
		runSource(t, zendeskcx.Connector{Origin: fake.server.URL}, deps, resolved, func() bool { return fake.requested("/api/v2/groups.json") }, func() {
			require.True(t, images.Available(), "the attachment fetcher is lent while Zendesk runs")
		})
		require.True(t, fake.requested("/oauth/tokens"))
		require.False(t, images.Available())
		// Without object storage the document worker says so as it starts.
		require.Equal(t, docIngest, strings.Contains(logs.String(), "document ingest is on"),
			"doc_ingest=%v: the document worker follows Zendesk's own setting", docIngest)
	}
}

func testSessionIngest(t *testing.T, pool *pgxpool.Pool, cfg *config.Config) *sessionIngest {
	t.Helper()
	return &sessionIngest{
		cfg: cfg, models: &inferenceModels{}, pool: pool,
		control: sessioningest.NewSummaryControl(sessioningest.NewStore(pool), nil), logger: discardLogger(),
	}
}

// TestSessionFeedSourceRun: Run reads the feed with the configured URL and
// token and stops on cancel. With push ingest it never reads the feed.
func TestSessionFeedSourceRun(t *testing.T) {
	pool := ingestTestPool(t, "cmd_session_feed_connector")
	newFeed := func() *fakeUpstream {
		return newFakeUpstream(t, func(r *http.Request) (int, any) {
			if r.Header.Get("Authorization") != "Bearer feed-fixture" {
				return http.StatusUnauthorized, map[string]any{}
			}
			return http.StatusOK, map[string]any{"changes": []any{}, "next_cursor": 0}
		})
	}
	resolve := func(feed *fakeUpstream) settings.Resolved {
		return settings.NewResolved(sessionfeed.ConnectorName, settings.Values{sessionfeed.FieldURL: feed.server.URL},
			map[string]string{sessionfeed.FieldToken: "feed-fixture"})
	}
	cfg := &config.Config{SessionIngestTick: 30 * time.Second, SessionIngestMaxRevisionBytes: 1 << 20}

	sourceWithIngest := func(ingest *sessionIngest) *sessionFeedSource {
		term := &settings.Live[leaderTerm]{}
		term.Set(leaderTerm{ingest: ingest})
		return &sessionFeedSource{term: term}
	}
	deps := testDeps(pool, "agent_session", discardLogger(), &lentImages{})

	feed := newFeed()
	runSource(t, sourceWithIngest(testSessionIngest(t, pool, cfg)), deps, resolve(feed),
		func() bool { return feed.requested("/api/v1/session-changes") }, nil)

	// Push ingest runs without the feed connector, so the connector reads
	// nothing.
	feed = newFeed()
	push := *cfg
	push.SessionPush.Enabled = true
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, sourceWithIngest(testSessionIngest(t, pool, &push)).Run(ctx, deps, resolve(feed)))

	require.ErrorIs(t, (&sessionFeedSource{term: &settings.Live[leaderTerm]{}}).Run(ctx, deps, resolve(feed)), errNoLeaderTerm,
		"outside a leader term there is no ingest to run")
	require.False(t, feed.requested("/api/v1/session-changes"))
}

// TestSessionIngestRegisterFailsClosed: a feed without a revision size cap,
// or no source at all, registers no worker.
func TestSessionIngestRegisterFailsClosed(t *testing.T) {
	ingest := &sessionIngest{cfg: &config.Config{SessionIngestTick: time.Minute}, logger: discardLogger()}
	feed := sessioningest.NewClientFeed(sessionfeed.NewClient("http://feed.invalid", "x", nil))
	require.ErrorContains(t, ingest.register(worker.NewSupervisor(nil, nil), nil, feed, ""), "DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES")
	require.ErrorContains(t, ingest.register(worker.NewSupervisor(nil, nil), nil, nil, ""), "no source")
}

// TestLiveSessionFeed: request paths read the session feed connector from the
// settings cache: nothing while it is off, its client and viewer once it is
// enabled.
func TestLiveSessionFeed(t *testing.T) {
	ctx := context.Background()
	pool := ingestTestPool(t, "cmd_live_session_feed")
	store := settings.NewStore(pool, discardLogger()).WithConnectors(settings.NewRegistry(sessionfeed.Connector{}), settings.Secrets{
		Env: map[string]string{settings.EnvName(sessionfeed.ConnectorName, sessionfeed.FieldToken): "feed-fixture"},
	})
	require.NoError(t, store.Load(ctx))
	live := &liveSessionFeed{store: store}
	require.Nil(t, live.Client())
	require.Empty(t, live.ViewerBaseURL())
	usage, err := live.GetWorkUsage(ctx, sessionfeed.DiderotAskWorkNamespace, "id")
	require.NoError(t, err)
	require.Nil(t, usage)

	_, err = pool.Exec(ctx, `UPDATE connector_settings SET enabled = true, version = version + 1,
		settings = '{"url": "http://feed.example", "viewer_base_url": "https://sessions.example"}'
		WHERE connector = $1`, sessionfeed.ConnectorName)
	require.NoError(t, err)
	require.NoError(t, store.Load(ctx))
	client := live.Client()
	require.NotNil(t, client)
	require.Same(t, client, live.Client(), "the client is reused until the settings change")
	require.Equal(t, "https://sessions.example", live.ViewerBaseURL())
}
