package ask

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticLinearTeams is a loaded, fixed LinearTeamProvider for tests.
type staticLinearTeams map[string]struct{}

func (s staticLinearTeams) KnownLinearTeams(context.Context) (map[string]struct{}, bool) {
	return s, true
}

// testLinearTeams is the default team set for Ask and MCP test harnesses.
var testLinearTeams = staticLinearTeams{"ENG": {}, "APP": {}}

type scriptedLinearTeamLoader struct {
	mu    sync.Mutex
	calls int
	keys  []string
	err   error
}

func (l *scriptedLinearTeamLoader) LinearTeamKeys(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return append([]string(nil), l.keys...), nil
}

func (l *scriptedLinearTeamLoader) set(keys []string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys, l.err = keys, err
}

func (l *scriptedLinearTeamLoader) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func newTestLinearTeamCache(t *testing.T, loader LinearTeamKeyLoader, clock *time.Time, logs *bytes.Buffer) *LinearTeamCache {
	t.Helper()
	cache, err := NewLinearTeamCache(loader, slog.New(slog.NewTextHandler(logs, nil)))
	require.NoError(t, err)
	cache.now = func() time.Time { return *clock }
	return cache
}

func TestLinearTeamCacheLoadsLazilyAndRefreshesAfterTTL(t *testing.T) {
	loader := &scriptedLinearTeamLoader{keys: []string{"ENG", "app"}}
	clock := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cache := newTestLinearTeamCache(t, loader, &clock, &bytes.Buffer{})
	require.Equal(t, 0, loader.callCount(), "construction performs no I/O")

	teams, ok := cache.KnownLinearTeams(t.Context())
	require.True(t, ok)
	require.Equal(t, map[string]struct{}{"ENG": {}, "APP": {}}, teams)
	require.Equal(t, 1, loader.callCount())

	loader.set([]string{"ENG", "OPS"}, nil)
	clock = clock.Add(linearTeamRefreshInterval - time.Second)
	teams, _ = cache.KnownLinearTeams(t.Context())
	require.Contains(t, teams, "APP", "a fresh set is served from cache")
	require.Equal(t, 1, loader.callCount())

	clock = clock.Add(time.Second)
	teams, ok = cache.KnownLinearTeams(t.Context())
	require.True(t, ok)
	require.Equal(t, map[string]struct{}{"ENG": {}, "OPS": {}}, teams)
	require.Equal(t, 2, loader.callCount())
}

func TestLinearTeamCacheKeepsLastGoodSetOnRefreshError(t *testing.T) {
	loader := &scriptedLinearTeamLoader{keys: []string{"ENG"}}
	clock := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	cache := newTestLinearTeamCache(t, loader, &clock, &logs)
	_, ok := cache.KnownLinearTeams(t.Context())
	require.True(t, ok)

	loader.set(nil, errors.New("database unavailable"))
	clock = clock.Add(linearTeamRefreshInterval)
	teams, ok := cache.KnownLinearTeams(t.Context())
	require.True(t, ok)
	require.Equal(t, map[string]struct{}{"ENG": {}}, teams)
	require.Equal(t, 2, loader.callCount())
	require.Contains(t, logs.String(), "linear team keys refresh failed")

	clock = clock.Add(linearTeamRetryInterval - time.Second)
	_, _ = cache.KnownLinearTeams(t.Context())
	require.Equal(t, 2, loader.callCount(), "a failed load is not retried before the retry interval")

	loader.set([]string{"OPS"}, nil)
	clock = clock.Add(time.Second)
	teams, ok = cache.KnownLinearTeams(t.Context())
	require.True(t, ok)
	require.Equal(t, map[string]struct{}{"OPS": {}}, teams)
	require.Equal(t, 3, loader.callCount())
}

func TestLinearTeamCacheFirstLoadFailureAdmitsNoBareKeys(t *testing.T) {
	loader := &scriptedLinearTeamLoader{err: errors.New("database unavailable")}
	clock := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	cache := newTestLinearTeamCache(t, loader, &clock, &logs)

	for range 3 {
		teams, ok := cache.KnownLinearTeams(t.Context())
		require.False(t, ok)
		require.Empty(t, teams)
		require.Empty(t, parseExplicitReferences(t.Context(), cache, "status of ENG-770?"))
	}
	require.Equal(t, 1, strings.Count(logs.String(), "linear team keys not loaded"), "the unloaded state is logged once")

	loader.set([]string{"ENG"}, nil)
	clock = clock.Add(linearTeamRetryInterval)
	refs := parseExplicitReferences(t.Context(), cache, "status of ENG-770?")
	require.Len(t, refs, 1)
	require.Equal(t, "ENG-770", refs[0].Value)
}

type blockingLinearTeamLoader struct {
	calls   atomic.Int32
	release chan struct{}
}

func (l *blockingLinearTeamLoader) LinearTeamKeys(context.Context) ([]string, error) {
	l.calls.Add(1)
	<-l.release
	return []string{"ENG"}, nil
}

func TestLinearTeamCacheSharesOneConcurrentLoad(t *testing.T) {
	loader := &blockingLinearTeamLoader{release: make(chan struct{})}
	cache, err := NewLinearTeamCache(loader, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			teams, ok := cache.KnownLinearTeams(t.Context())
			assert.True(t, ok)
			assert.Contains(t, teams, "ENG")
		})
	}
	require.Eventually(t, func() bool { return loader.calls.Load() == 1 }, time.Second, time.Millisecond)
	close(loader.release)
	wg.Wait()
	require.Equal(t, int32(1), loader.calls.Load())
}

func TestNewLinearTeamCacheRequiresLoader(t *testing.T) {
	_, err := NewLinearTeamCache(nil, nil)
	require.ErrorContains(t, err, "loader is required")
}
