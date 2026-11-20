package settings

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Supervisor runs each enabled connector's Run on the leader. When a
// connector's version changes or it is disabled, the supervisor cancels its
// context, waits for its workers to return, and starts it again with the new
// settings. It reads the store's cache, which the store's poll keeps
// current, so it never queries the database itself.
type Supervisor struct {
	store      *Store
	connectors []Connector
	logger     *slog.Logger

	// Interval is how often the supervisor compares the cache with what
	// runs.
	Interval time.Duration
	// RetryAfter is how long a connector whose Run ended on its own, or
	// whose secrets couldn't be read, waits before it is started again.
	RetryAfter time.Duration
}

// NewSupervisor returns a supervisor for connectors.
func NewSupervisor(store *Store, logger *slog.Logger, connectors ...Connector) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		store: store, connectors: connectors, logger: logger,
		Interval: time.Second, RetryAfter: time.Minute,
	}
}

type connectorRun struct {
	version int64
	cancel  context.CancelFunc
	done    chan struct{}
	endedAt time.Time
}

func (r *connectorRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *connectorRun) stop() {
	r.cancel()
	<-r.done
}

// Run supervises until ctx ends, then stops every connector and waits for
// it.
func (s *Supervisor) Run(ctx context.Context) {
	runs := map[string]*connectorRun{}
	defer func() {
		for _, run := range runs {
			run.stop()
		}
	}()
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		for _, connector := range s.connectors {
			s.reconcile(ctx, connector, runs)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) reconcile(ctx context.Context, connector Connector, runs map[string]*connectorRun) {
	name := connector.Name()
	entry, ok := s.store.cached(name)
	want := ok && entry.row.Enabled
	if run := runs[name]; run != nil {
		switch {
		case !want || run.version != entry.row.Version:
			s.logger.Info("connector stopping", "connector", name, "version", run.version)
			run.stop()
			delete(runs, name)
		case !run.ended():
			return
		case time.Since(run.endedAt) < s.RetryAfter:
			return
		default:
			delete(runs, name)
		}
	}
	if !want || ctx.Err() != nil {
		return
	}
	run := &connectorRun{version: entry.row.Version, cancel: func() {}, done: make(chan struct{})}
	runs[name] = run
	resolved, err := entry.resolved, entry.err
	if err != nil {
		s.logger.Error("connector not started: its settings can't be resolved",
			"connector", name, "version", run.version, "err", err, "retry_after", s.RetryAfter)
		run.endedAt = time.Now()
		close(run.done)
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	run.cancel = cancel
	s.logger.Info("connector starting", "connector", name, "version", run.version)
	go func() {
		defer close(run.done)
		err := runRecovered(runCtx, connector, resolved)
		run.endedAt = time.Now()
		if runCtx.Err() == nil {
			s.logger.Error("connector stopped on its own; restarting after a delay",
				"connector", name, "version", run.version, "err", err, "retry_after", s.RetryAfter)
		}
	}()
}

// runRecovered turns a panic in a connector's Run, which runs on its own
// goroutine outside any worker supervisor, into an error the supervisor
// retries, instead of a process crash.
func runRecovered(ctx context.Context, connector Connector, resolved Resolved) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("connector panicked: %v", recovered)
		}
	}()
	return connector.Run(ctx, resolved)
}
