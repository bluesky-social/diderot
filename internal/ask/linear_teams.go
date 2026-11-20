package ask

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LinearTeamProvider supplies the Linear team keys that make a bare issue key
// such as ENG-770 an explicit corpus reference. ok is false until a set has
// loaded; until then no bare key is a reference. The returned map is shared
// and must not be modified.
type LinearTeamProvider interface {
	KnownLinearTeams(ctx context.Context) (teams map[string]struct{}, ok bool)
}

// LinearTeamKeyLoader reads the current team keys, e.g. events.DBSink.
type LinearTeamKeyLoader interface {
	LinearTeamKeys(ctx context.Context) ([]string, error)
}

const (
	linearTeamRefreshInterval = 15 * time.Minute
	linearTeamRetryInterval   = time.Minute
	linearTeamLoadTimeout     = 5 * time.Second
)

// LinearTeamCache is the production LinearTeamProvider. It loads lazily on
// the first call and reloads once the set is older than
// linearTeamRefreshInterval. A failed load keeps the last good set and is
// retried after linearTeamRetryInterval. Loads run under the mutex, so
// concurrent callers share one in-flight load instead of stampeding the
// database; linearTeamLoadTimeout bounds how long they wait.
type LinearTeamCache struct {
	loader LinearTeamKeyLoader
	logger *slog.Logger
	now    func() time.Time

	mu             sync.Mutex
	teams          map[string]struct{}
	loaded         bool
	nextLoad       time.Time
	warnedUnloaded bool
}

func NewLinearTeamCache(loader LinearTeamKeyLoader, logger *slog.Logger) (*LinearTeamCache, error) {
	if loader == nil {
		return nil, errors.New("linear team cache: loader is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LinearTeamCache{loader: loader, logger: logger, now: time.Now}, nil
}

func (c *LinearTeamCache) KnownLinearTeams(ctx context.Context) (map[string]struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); !now.Before(c.nextLoad) {
		c.reload(ctx, now)
	}
	if !c.loaded && !c.warnedUnloaded {
		c.warnedUnloaded = true
		c.logger.Warn("linear team keys not loaded; bare issue keys are not treated as corpus references until they are")
	}
	return c.teams, c.loaded
}

func (c *LinearTeamCache) reload(ctx context.Context, now time.Time) {
	// One caller's cancellation must not fail the load every waiter shares.
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linearTeamLoadTimeout)
	defer cancel()
	keys, err := c.loader.LinearTeamKeys(loadCtx)
	if err != nil {
		c.nextLoad = now.Add(linearTeamRetryInterval)
		c.logger.Warn("linear team keys refresh failed; keeping last good set", "loaded", c.loaded, "err", err)
		return
	}
	teams := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		teams[strings.ToUpper(key)] = struct{}{}
	}
	c.teams, c.loaded = teams, true
	c.nextLoad = now.Add(linearTeamRefreshInterval)
}

// parseExplicitReferences is the one parsing entry point for every surface
// that resolves exact references (Ask turns and MCP retrieve), so the Linear
// team gate cannot drift between them.
func parseExplicitReferences(ctx context.Context, teams LinearTeamProvider, input string) []CorpusReference {
	known, _ := teams.KnownLinearTeams(ctx)
	return ParseReferences(input, known)
}

var _ LinearTeamProvider = (*LinearTeamCache)(nil)
