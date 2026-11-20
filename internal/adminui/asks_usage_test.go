package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// usageRecorder is a WorkUsageClient test double that records every requested
// (namespace, reference_id) pair and can be configured to return usage, an
// error, or to block until its context is cancelled (slow/unresponsive upstream).
type usageRecorder struct {
	mu    sync.Mutex
	refs  [][2]string
	usage *sessionfeed.Usage
	err   error
	block bool
}

func (c *usageRecorder) GetWorkUsage(ctx context.Context, namespace, referenceID string) (*sessionfeed.Usage, error) {
	c.mu.Lock()
	c.refs = append(c.refs, [2]string{namespace, referenceID})
	usage, err, block := c.usage, c.err, c.block
	c.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return usage, err
}

func (c *usageRecorder) calledWith() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	refs := make([]string, 0, len(c.refs))
	for _, ref := range c.refs {
		refs = append(refs, ref[1])
	}
	return refs
}

func (c *usageRecorder) calledNamespaces() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	namespaces := make([]string, 0, len(c.refs))
	for _, ref := range c.refs {
		namespaces = append(namespaces, ref[0])
	}
	return namespaces
}

// askUsageTestServer returns a Server against the integration DB with one
// persisted ask session and the given usage recorder wired in. Shared ask
// tables are truncated so tests are order-independent.
func askUsageTestServer(t *testing.T, rec *usageRecorder) (*Server, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions, ask_messages, ask_slack_sessions, slack_channel_info RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	sessionID := uuid.MustParse("1f1fab90-87e8-4b3e-9f7d-082fd73899f4")
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_sessions (id, created_at, last_turn_at)
		VALUES ($1, $2::timestamptz, $2::timestamptz + interval '1 minute')`,
		sessionID, time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return &Server{
		pool:          pool,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		slackHost:     func() string { return "example.slack.com" },
		viewerBaseURL: func() string { return "https://sessions.example.com" },
		usageClient:   rec,
	}, sessionID
}

func TestAskSessionUsageUsesAskUUIDAndRenders(t *testing.T) {
	rec := &usageRecorder{usage: &sessionfeed.Usage{
		MeteredCostUSD: "1.25", CoverageComplete: false, UnpricedMeteredAttemptCount: 2,
	}}
	s, sessionID := askUsageTestServer(t, rec)

	detail, err := s.loadAskSession(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{sessionID.String()}, rec.calledWith(),
		"usage must be keyed by the Diderot ask UUID")
	assert.Equal(t, []string{sessionfeed.DiderotAskWorkNamespace}, rec.calledNamespaces(),
		"ask usage must go through the diderot.ask work namespace, not the session endpoint")
	assert.Equal(t, "$1.25", detail.Cost)
	assert.Equal(t, "$1.25", detail.CostExact)
	assert.Equal(t, "incomplete: 2 calls without a price", detail.CostNote)
}

func TestAskSessionUsageUnavailableDegradesGracefully(t *testing.T) {
	rec := &usageRecorder{err: &sessionfeed.HTTPError{
		StatusCode: http.StatusNotFound,
		URL:        "http://feed/api/v1/work-usage/diderot.ask/x",
		Body:       "not found",
	}}
	s, sessionID := askUsageTestServer(t, rec)

	detail, err := s.loadAskSession(context.Background(), sessionID)
	require.NoError(t, err, "an unavailable usage endpoint must not fail the detail page")
	assert.Equal(t, []string{sessionID.String()}, rec.calledWith())
	assert.Empty(t, detail.Cost)
	assert.Empty(t, detail.CostNote)

	var out bytes.Buffer
	detail.pageData = pageData{Title: "Ask transcript"}
	require.NoError(t, askSessionPage.ExecuteTemplate(&out, "layout", detail))
	html := out.String()
	assert.NotContains(t, html, "<dt>Cost</dt>")
	assert.NotContains(t, html, "Metered cost")
}

func TestAskSessionUsageDetailOnly(t *testing.T) {
	rec := &usageRecorder{usage: &sessionfeed.Usage{MeteredCostUSD: "0.5", CoverageComplete: true}}
	s, sessionID := askUsageTestServer(t, rec)

	_, _, err := s.loadAskSessions(context.Background(), httptest.NewRequest("GET", "/admin/ask/sessions", nil))
	require.NoError(t, err)
	assert.Empty(t, rec.calledWith(), "the sessions list must never fetch per-session usage")

	_, err = s.loadAskSession(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{sessionID.String()}, rec.calledWith())
}

func TestAskSessionUsageTimeoutBounded(t *testing.T) {
	rec := &usageRecorder{block: true}
	s, sessionID := askUsageTestServer(t, rec)

	start := time.Now()
	detail, err := s.loadAskSession(context.Background(), sessionID)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Empty(t, detail.Cost)
	assert.Empty(t, detail.CostNote)
	assert.GreaterOrEqual(t, elapsed, askUsageTimeout, "a hung upstream must still be cut off by the per-call timeout")
	assert.Less(t, elapsed, 10*time.Second, "the detail page must not block far beyond the usage timeout")
}
