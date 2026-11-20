package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// leaderWorkers are the leader term's workers under serveLifecycleEnv: Ask,
// push ingest, and object storage are off, so the term runs the embedder,
// the connector supervisor, and the scope and Qdrant-delete workers.
var leaderWorkers = []string{"embedder", "connectors", "scope-reconcile", "scope-delta", "scope-metrics", "qdrant-delete"}

// TestServeStartsAndStopsInOrder runs serve against the test Postgres and
// Qdrant and checks the order it starts and stops in, from its log: the
// database and Qdrant are ready and every route is mounted before any
// listener opens; a changed model choice stops every worker of the leader
// term before the next term starts one; and on the shutdown signal the
// leader work drains before the listeners shut down and serve returns.
func TestServeStartsAndStopsInOrder(t *testing.T) {
	ports := serveLifecycleEnv(t)
	logs := newCapturedLog(t, "serve")

	ctx, stop := context.WithCancel(t.Context())
	served, returned := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(returned)
		served <- runServe(ctx, logs)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-returned:
		case <-time.After(time.Minute):
		}
	})

	base := "http://" + ports.public
	waitFor(t, served, 2*time.Minute, "/readyz to answer 200", func() bool {
		return httpStatus(base+"/readyz") == http.StatusOK
	})
	assert.Equal(t, http.StatusOK, httpStatus(base+"/healthz"))
	waitFor(t, served, time.Minute, "the first leader term's workers to start", func() bool {
		return logs.count("worker starting", leaderWorkers...) == len(leaderWorkers)
	})

	// A saved model change ends the leader term and starts another.
	saveModel(t, base, os.Getenv("DIDEROT_PUBLIC_URL"), "thread_summaries", "gpt-5.4:main")
	waitFor(t, served, time.Minute, "the second leader term's workers to start", func() bool {
		return logs.count("worker starting", leaderWorkers...) == 2*len(leaderWorkers)
	})

	stop()
	select {
	case err := <-served:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatal("serve did not return after the shutdown signal")
	}
	_, err := net.DialTimeout("tcp", ports.public, time.Second)
	require.Error(t, err, "the public listener is closed once serve returns")

	lines := logs.lines()
	at := func(msg string) int {
		t.Helper()
		i := slices.IndexFunc(lines, func(l logLine) bool { return l.Msg == msg })
		require.NotEqual(t, -1, i, "logged %q", msg)
		return i
	}
	all := func(msg string, workers ...string) []int {
		var at []int
		for i, l := range lines {
			if l.Msg == msg && (len(workers) == 0 || slices.Contains(workers, l.Worker)) {
				at = append(at, i)
			}
		}
		return at
	}

	// Migrations and the stores come before the routes, and the routes
	// before any listener. Search opens the embedding model's collection
	// beside them, once the settings name it.
	order := []int{at("diderot starting"), at("db connected"), at("prompt release store ready"), at("API mounted")}
	at("qdrant collection ready")
	assert.True(t, slices.IsSorted(order), "startup milestones in order: %v", order)
	listening := all("http server listening")
	require.Len(t, listening, len(listenerNames))
	assert.Less(t, at("API mounted"), listening[0], "every route is mounted before a listener opens")

	// The first term stops all its workers before the second starts any.
	starting, stopped := all("worker starting", leaderWorkers...), all("worker stopped", leaderWorkers...)
	require.Len(t, starting, 2*len(leaderWorkers))
	require.Len(t, stopped, 2*len(leaderWorkers))
	restart := at("the saved models changed; restarting with them")
	firstStopped, secondStarting := stopped[:len(leaderWorkers)], starting[len(leaderWorkers):]
	assert.Less(t, restart, firstStopped[0])
	assert.Less(t, firstStopped[len(firstStopped)-1], secondStarting[0],
		"every worker of the first term stops before the second term starts one")

	// On the signal, the leader work drains before the listeners shut down.
	// The workers see the signal as serve does, so some may stop before
	// serve logs it.
	signal, complete := at("shutdown signal received"), at("shutdown complete")
	lastStopped := stopped[len(leaderWorkers):]
	assert.Less(t, signal, complete)
	assert.Less(t, lastStopped[len(lastStopped)-1], complete, "the leader work drains before shutdown completes")

	// A worker cut off mid-call by its term's end may log the cancellation.
	for _, l := range lines {
		if l.Err != context.Canceled.Error() {
			assert.NotEqual(t, "ERROR", l.Level, "unexpected error logged: %s", l.raw)
		}
	}
}

type lifecyclePorts struct{ public, internal, metrics string }

// serveLifecycleLockKey is the advisory lock serve lifecycle tests take
// turns under.
const serveLifecycleLockKey = 0x5e7e11fe

// serveLifecycleEnv points serve at the test Postgres and Qdrant, in none
// mode on loopback ports, without leader election, Ask, push ingest, or
// object storage. It skips without the test databases.
func serveLifecycleEnv(t *testing.T) lifecyclePorts {
	t.Helper()
	serveTestEnv(t, "cmd_serve_lifecycle")
	// The provider answers the test a saved model change runs.
	seedModelSettings(t, os.Getenv("DIDEROT_DB_URL"), newFakeAskModel(t).server.URL+"/v1", settings.Models{})
	// The PagerDuty webhook listener's port is fixed, so this test in every
	// checkout sharing the test Postgres takes its turn under a session
	// lock, which closing the connection releases.
	lock, err := pgx.Connect(t.Context(), os.Getenv("DIDEROT_TEST_DB_URL"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close(context.Background()) })
	_, err = lock.Exec(t.Context(), "SELECT pg_advisory_lock($1)", serveLifecycleLockKey)
	require.NoError(t, err)
	webhook, err := net.Listen("tcp", "127.0.0.1:9093")
	if err != nil {
		t.Skipf("the PagerDuty webhook port is in use: %v", err)
	}
	require.NoError(t, webhook.Close())

	ports := lifecyclePorts{public: freeLoopbackAddr(t), internal: freeLoopbackAddr(t), metrics: freeLoopbackAddr(t)}
	for key, value := range map[string]string{
		"DIDEROT_LEADER_ELECTION":      "false",
		"DIDEROT_LISTEN_ADDR":          ports.public,
		"DIDEROT_INTERNAL_LISTEN_ADDR": ports.internal,
		"DIDEROT_METRICS_LISTEN_ADDR":  ports.metrics,
		"DIDEROT_PUBLIC_URL":           "http://" + ports.public,
	} {
		t.Setenv(key, value)
	}
	return ports
}

// serveTestEnv points serve at the test Postgres, in a database of its own
// named db, in none mode as a standalone deployment, with Ask, push ingest,
// and object storage off and nothing from the developer's shell. Its
// settings have a provider, gateway, that nothing answers at, whose
// embedding model is chosen, in a Qdrant collection of its own. It skips
// without the test databases. Callers set the listen addresses, and
// askServeEnv a model provider that answers.
func serveTestEnv(t *testing.T, db string) {
	t.Helper()
	qdrantAddr := os.Getenv("DIDEROT_QDRANT_TEST_ADDR")
	if qdrantAddr == "" {
		t.Skip("DIDEROT_QDRANT_TEST_ADDR not set; skipping serve test")
	}
	dbURL := testdb.URL(t, db)

	// Nothing else from the developer's shell reaches config.Load.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "DIDEROT_") && !strings.HasPrefix(key, "DIDEROT_TEST_") &&
			key != "DIDEROT_ATLAS_BINARY" && key != "DIDEROT_QDRANT_TEST_ADDR" {
			t.Setenv(key, "")
		}
	}
	serveEnv(t)
	collection := fmt.Sprintf("diderot_test_%s_%d", db, time.Now().UnixNano())
	for key, value := range map[string]string{
		"DIDEROT_DB_URL":               dbURL,
		"DIDEROT_QDRANT_ADDR":          qdrantAddr,
		"DIDEROT_PROVIDER_GATEWAY_KEY": "test-key",
		"DIDEROT_DEPLOYMENT":           "standalone",
		"DIDEROT_AUTH_MODE":            "none",
		"DIDEROT_AUTH_NONE_EMAIL":      "admin@example.com",
		"DIDEROT_OIDC_ISSUER":          "",
		"DIDEROT_OIDC_AUDIENCE":        "",
		"DIDEROT_GATEWAY_API_KEY":      "test-key",
	} {
		t.Setenv(key, value)
	}
	seedModelSettings(t, dbURL, "http://127.0.0.1:1/v1", settings.Models{
		Embedding: &settings.Embedding{Provider: "gateway", Model: "embed", Dimensions: 8, Collection: collection},
	})
	t.Cleanup(func() {
		store, err := qdrantstore.New(qdrantstore.Config{Addr: qdrantAddr, Collection: collection, Dimensions: 8}, prometheus.NewRegistry())
		if err != nil {
			return
		}
		defer func() { _ = store.Close() }()
		_ = store.Drop(context.Background())
	})
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

// waitFor polls ready until it holds, failing the test if serve returns
// first or timeout passes.
func waitFor(t *testing.T, served <-chan error, timeout time.Duration, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ready() {
		select {
		case err := <-served:
			t.Fatalf("serve returned while waiting for %s: %v", what, err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func httpStatus(url string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// saveModel chooses a role's model the way an administrator does: the
// Models page's form, posted to the server at base as its none-mode
// administrator. origin is the server's DIDEROT_PUBLIC_URL.
func saveModel(t *testing.T, base, origin, role, model string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DIDEROT_DB_URL"))
	require.NoError(t, err)
	defer pool.Close()
	saved, err := settings.NewStore(pool, nil).ReadModels(ctx)
	require.NoError(t, err)

	const csrf = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	form := url.Values{"csrf_token": {csrf}, "version": {strconv.FormatInt(saved.Version, 10)}, "role." + role: {"gateway/" + model}}
	req, err := http.NewRequest(http.MethodPost, base+"/admin/settings/models", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(&http.Cookie{Name: "diderot_admin_csrf", Value: csrf})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "save %s: %s", role, got)
	require.Contains(t, resp.Header.Get("Location"), "notice=", "save %s: %s", role, resp.Header.Get("Location"))
}

// logLine is one line serve logged.
type logLine struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Worker string `json:"worker"`
	Err    string `json:"err"`
	raw    string
}

// capturedLog is serve's JSON log, which runServe writes here instead of to
// standard output.
type capturedLog struct {
	mu      sync.Mutex
	partial []byte
	all     []logLine
}

// newCapturedLog is the log of one serve, which the test prints, each line
// after name, if it fails.
func newCapturedLog(t *testing.T, name string) *capturedLog {
	t.Helper()
	logs := &capturedLog{}
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range logs.lines() {
				t.Log(name + ": " + line.raw)
			}
		}
	})
	return logs
}

// Write takes the log as serve writes it, one JSON object per line.
func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial = append(c.partial, p...)
	for {
		end := bytes.IndexByte(c.partial, '\n')
		if end < 0 {
			return len(p), nil
		}
		raw := bytes.Clone(c.partial[:end])
		c.partial = c.partial[end+1:]
		var line logLine
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		line.raw = string(raw)
		c.all = append(c.all, line)
	}
}

func (c *capturedLog) lines() []logLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.all)
}

// count is how many lines carry msg and one of workers.
func (c *capturedLog) count(msg string, workers ...string) int {
	n := 0
	for _, l := range c.lines() {
		if l.Msg == msg && slices.Contains(workers, l.Worker) {
			n++
		}
	}
	return n
}

// seedModelSettings saves, before serve starts, an enabled provider named
// gateway at baseURL, which offers gatewayEntries' models and whose key
// DIDEROT_PROVIDER_GATEWAY_KEY sets, and
// models as the models row, as an installer's Models page would; models
// without an embedding keeps the one saved.
func seedModelSettings(t *testing.T, dbURL, baseURL string, models settings.Models) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	offered := []string{}
	for _, entry := range gatewayEntries {
		offered = append(offered, entry.ID)
	}
	provider, err := json.Marshal(map[string]any{"kind": "openai", "base_url": baseURL, "models": offered})
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `
		INSERT INTO connector_settings (connector, enabled, settings, secrets, version, updated_by, updated_at)
		VALUES ('provider:gateway', true, $1, '{}', 1, 'test', now())
		ON CONFLICT (connector) DO UPDATE SET enabled = true, settings = EXCLUDED.settings, version = connector_settings.version + 1`, provider)
	require.NoError(t, err)
	if models.Embedding == nil {
		var saved settings.Models
		var raw []byte
		if conn.QueryRow(ctx, `SELECT settings FROM connector_settings WHERE connector = 'models'`).Scan(&raw) == nil {
			require.NoError(t, json.Unmarshal(raw, &saved))
			models.Embedding = saved.Embedding
		}
	}
	doc, err := json.Marshal(models)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `
		INSERT INTO connector_settings (connector, enabled, settings, secrets, version, updated_by, updated_at)
		VALUES ('models', false, $1, '{}', 1, 'test', now())
		ON CONFLICT (connector) DO UPDATE SET settings = EXCLUDED.settings, version = connector_settings.version + 1`, doc)
	require.NoError(t, err)
}
