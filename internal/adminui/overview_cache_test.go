package adminui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestCorpusTotalsAreCachedAndRefreshedWhenStale(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_corpus"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	addEvent := func(id string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw)
			VALUES ('slack', $1, now(), 'body', '{}')`, id)
		require.NoError(t, err)
	}

	first, err := s.cachedCorpus(ctx)
	require.NoError(t, err)
	require.Empty(t, first.Sources)

	addEvent("one")
	cached, err := s.cachedCorpus(ctx)
	require.NoError(t, err)
	require.Empty(t, cached.Sources, "fresh totals are served without counting")

	s.overview.mu.Lock()
	s.overview.totals.At = time.Now().Add(-overviewMaxAge - time.Second)
	s.overview.mu.Unlock()
	stale, err := s.cachedCorpus(ctx)
	require.NoError(t, err)
	require.Empty(t, stale.Sources, "stale totals are still served at once")
	require.Eventually(t, func() bool {
		got, err := s.cachedCorpus(ctx)
		return err == nil && got.Sources["slack"].Items == 1
	}, 30*time.Second, 50*time.Millisecond, "the stale view recounts in the background")
}

func TestWarmOverviewCountsTheCorpus(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_corpus"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	s.WarmOverview(ctx)
	s.overview.mu.Lock()
	warmed := s.overview.totals != nil
	s.overview.mu.Unlock()
	require.True(t, warmed)
}

func TestCorpusFirstViewsShareOneCount(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_corpus"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Hold a count in flight, as a first view would, and check that a
	// second view waits for it instead of counting.
	done := make(chan struct{})
	s.overview.mu.Lock()
	s.overview.loading = done
	s.overview.mu.Unlock()
	got := make(chan corpusTotals, 1)
	go func() {
		totals, err := s.cachedCorpus(ctx)
		if err == nil {
			got <- totals
		}
	}()
	select {
	case <-got:
		t.Fatal("a second view counted while a count was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	shared := corpusTotals{At: time.Now(), Sources: map[string]corpusSource{"shared": {Items: 7}}}
	s.storeCorpus(shared)
	s.overview.mu.Lock()
	s.overview.loading = nil
	s.overview.mu.Unlock()
	close(done)
	select {
	case totals := <-got:
		require.Equal(t, int64(7), totals.Sources["shared"].Items)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting view never returned")
	}
}

func TestCorpusCountCutShortAsTheServerStopsIsNotAWarning(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "adminui_corpus"))
	require.NoError(t, err)
	pool.Close()
	var logs bytes.Buffer
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	s.overview.refreshing = true

	s.refreshCorpus()
	require.Contains(t, logs.String(), `level=DEBUG msg="admin corpus totals refresh failed" err="corpus totals: closed pool"`)
	require.NotContains(t, logs.String(), "level=WARN")
	require.False(t, s.overview.refreshing, "the next view may count again")

	logs.Reset()
	s.logCountFailed(context.Background(), "admin corpus totals refresh failed", errors.New("corpus totals: relation does not exist"))
	require.Contains(t, logs.String(), "level=WARN", "any other failure is still a warning")
}
