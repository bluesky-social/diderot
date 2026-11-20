package adminui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/puddle/v2"
)

// overviewMaxAge is how old the corpus totals may be before a view starts a
// refresh. Counting each source's items scans every event, which takes
// seconds on a large corpus, so views are served the last count and it is
// refreshed in the background.
const overviewMaxAge = 10 * time.Minute

// overviewRefreshTimeout bounds one background refresh.
const overviewRefreshTimeout = 2 * time.Minute

// overviewEmptyMaxAge is how old the totals may be while they count nothing
// for a source that is on. That is a fresh install, or a source just
// connected, in the minutes its first items arrive, which is when an
// operator is watching the count.
const overviewEmptyMaxAge = 15 * time.Second

// overviewQuickCount is how long a count may have taken for a view to wait
// for the next one instead of being served the last.
const overviewQuickCount = time.Second

// corpusTotals is how many items each source has indexed, by event source.
type corpusTotals struct {
	At time.Time
	// Took is how long the count took.
	Took    time.Duration
	Sources map[string]corpusSource
}

type corpusSource struct {
	Items  int64
	Newest *time.Time
}

// overviewCache holds the last corpus totals.
type overviewCache struct {
	mu         sync.Mutex
	totals     *corpusTotals
	refreshing bool
	// loading is closed when the load of a cache with no totals yet ends,
	// so concurrent first views share it.
	loading chan struct{}
}

// cachedCorpus returns the corpus totals. With totals cached it returns
// them at once, starting one background refresh when they are stale;
// without, it loads them once, and concurrent requests wait for that load.
func (s *Server) cachedCorpus(ctx context.Context) (corpusTotals, error) {
	c := &s.overview
	for {
		c.mu.Lock()
		if c.totals != nil {
			if !c.refreshing && time.Since(c.totals.At) > overviewMaxAge {
				c.refreshing = true
				go s.refreshCorpus()
			}
			totals := *c.totals
			c.mu.Unlock()
			return totals, nil
		}
		wait := c.loading
		if wait == nil {
			break
		}
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return corpusTotals{}, ctx.Err()
		}
	}
	done := make(chan struct{})
	c.loading = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.loading = nil
		c.mu.Unlock()
		close(done)
	}()
	start := time.Now()
	totals, err := s.loadCorpus(ctx)
	if err != nil {
		return totals, err
	}
	s.storeCorpus(totals)
	s.logger.Info("admin corpus totals refreshed", "duration", time.Since(start).Round(time.Millisecond))
	return totals, nil
}

// corpusFor returns the corpus totals for a page that lists the sources
// that are on, by event source. Totals that count nothing for one of them
// are recounted once they are overviewEmptyMaxAge old: before the page is
// drawn when counting is quick, as it is on a corpus that small, and in the
// background otherwise.
func (s *Server) corpusFor(ctx context.Context, on []string) (corpusTotals, error) {
	totals, err := s.cachedCorpus(ctx)
	if err != nil {
		return totals, err
	}
	empty := false
	for _, eventSource := range on {
		empty = empty || totals.Sources[eventSource].Items == 0
	}
	if !empty || time.Since(totals.At) < overviewEmptyMaxAge {
		return totals, nil
	}
	// One recount at a time: a view that finds one under way is served the
	// last count.
	c := &s.overview
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return totals, nil
	}
	c.refreshing = true
	c.mu.Unlock()
	if totals.Took > overviewQuickCount {
		go s.refreshCorpus()
		return totals, nil
	}
	fresh, err := s.loadCorpus(ctx)
	if err != nil {
		s.logCountFailed(ctx, "admin corpus totals recount failed", err)
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
		return totals, nil
	}
	s.storeCorpus(fresh)
	return fresh, nil
}

// refreshCorpus recounts in the background. On failure the previous totals
// stay and the next view retries.
func (s *Server) refreshCorpus() {
	ctx, cancel := context.WithTimeout(context.Background(), overviewRefreshTimeout)
	defer cancel()
	start := time.Now()
	totals, err := s.loadCorpus(ctx)
	if err != nil {
		s.logCountFailed(ctx, "admin corpus totals refresh failed", err)
		s.overview.mu.Lock()
		s.overview.refreshing = false
		s.overview.mu.Unlock()
		return
	}
	s.storeCorpus(totals)
	s.logger.Info("admin corpus totals refreshed", "duration", time.Since(start).Round(time.Millisecond))
}

// logCountFailed logs a count that failed. One cut short because its view
// went away or the server is stopping, which closes the database pool under
// a background count, is not a failure.
func (s *Server) logCountFailed(ctx context.Context, message string, err error) {
	level := slog.LevelWarn
	if errors.Is(err, context.Canceled) || errors.Is(err, puddle.ErrClosedPool) {
		level = slog.LevelDebug
	}
	s.logger.Log(ctx, level, message, "err", err)
}

func (s *Server) storeCorpus(totals corpusTotals) {
	s.overview.mu.Lock()
	defer s.overview.mu.Unlock()
	s.overview.totals = &totals
	s.overview.refreshing = false
}

func (s *Server) loadCorpus(ctx context.Context) (corpusTotals, error) {
	totals := corpusTotals{At: time.Now(), Sources: map[string]corpusSource{}}
	rows, err := s.pool.Query(ctx, `
		SELECT source, count(*) FILTER (WHERE NOT deleted), max(occurred_at) FILTER (WHERE NOT deleted)
		  FROM events
		 GROUP BY source`)
	if err != nil {
		return totals, fmt.Errorf("corpus totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		var item corpusSource
		if err := rows.Scan(&source, &item.Items, &item.Newest); err != nil {
			return totals, fmt.Errorf("corpus totals scan: %w", err)
		}
		totals.Sources[source] = item
	}
	if err := rows.Err(); err != nil {
		return totals, fmt.Errorf("corpus totals rows: %w", err)
	}
	totals.Took = time.Since(totals.At)
	return totals, nil
}

// WarmOverview counts the corpus so the first view after a restart doesn't
// wait for it (or waits on this count instead of starting another).
func (s *Server) WarmOverview(ctx context.Context) {
	if _, err := s.cachedCorpus(ctx); err != nil && ctx.Err() == nil {
		s.logCountFailed(ctx, "admin corpus totals warm-up failed", err)
	}
}
