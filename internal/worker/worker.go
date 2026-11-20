// Package worker provides a small supervision layer for diderot's
// leader-only background workers.
//
// Two shapes of worker exist in the codebase:
//
//   - Periodic loops: "tick on an interval, do a unit of work, back
//     off on error." Before this package every one of these
//     (scopesync sweep, the slack-channel reconcile loop, etc.) was
//     a hand-rolled `go func(){ run(); ticker := ...; for { select
//     ctx.Done / ticker.C } }`. Runner collapses that into one
//     well-tested implementation.
//   - Long-running funcs: workers that own their own internal loop
//     (the embedder fetcher/embedder fan-out, the slack consumer's
//     socket-mode read loop, the caption worker). These just need
//     Run(ctx) called once and supervised.
//
// Supervisor runs a mix of both under one waitgroup, propagates ctx
// cancellation to every worker, and blocks until all have drained.
//
// # Error policy (load-bearing — do not change without thought)
//
// A worker that returns or errors must NOT kill the pod, and must
// NOT cancel its sibling workers. This preserves diderot's existing
// behavior: before this package, every leader worker was a bare
// `go func()` whose error was logged-and-swallowed. The historical
// failure mode that motivated this package is the *opposite* of a
// crash — a dead embedder used to leave a healthy-looking pod that
// silently stopped indexing. The fix is visibility, not a crash:
//
//   - Every run (success or failure) is logged and recorded in a
//     per-worker prometheus metric.
//   - A `last_success_timestamp` gauge lets Grafana alert "worker X
//     hasn't succeeded in Y" — the durable replacement for noticing
//     a stalled worker by eye.
//   - A panic in any worker is recovered, logged, and counted, then
//     treated as an error (periodic Runners retry next tick); it
//     never unwinds into the pod's main goroutine.
//
// We are adding observability, NOT changing crash semantics. If a
// future change wants a worker death to actually take the pod down,
// that is a deliberate policy reversal and belongs in its own commit
// with its own rationale.
//
// safeRun's recover only protects the single goroutine it calls a
// registered worker's Run on -- not a further goroutine that Run, or a
// handler it dispatches to, spawns itself (every per-event and per-turn
// goroutine on a serving path: a Slack/Discord event handler or Ask
// turn, an MCP ask task). Recover (panic.go) is the same policy applied
// at those spawn points, since recover() cannot cross a goroutine
// boundary for safeRun to do it on their behalf.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Func is a long-running worker that owns its own loop. It should
// block until ctx is cancelled and then return promptly. A non-nil
// return (including a panic, which Supervisor converts to an error)
// is logged + metered but does not stop sibling workers.
type Func func(ctx context.Context) error

// RunFunc is the per-tick callback of a Runner. It does one unit of
// work and returns. A nil return marks the tick a success (advances
// the last-success gauge, resets backoff); a non-nil return is
// logged + metered and the loop backs off before the next attempt.
// context.Canceled returns are treated as a clean shutdown, not a
// failure.
type RunFunc func(ctx context.Context) error

// Runner is a periodic worker: it invokes Run on an interval, with
// optional immediate boot-tick and exponential error backoff, so no
// periodic worker hand-rolls its own ticker, backoff and cancellation.
type Runner struct {
	// Name labels this worker in logs + metrics. Required.
	Name string
	// Interval between ticks once the loop is in steady state.
	// Required (must be > 0).
	Interval time.Duration
	// BootTick runs Run once immediately on start, before the first interval
	// elapses. Scope queue drains and Slack reconciliation use it to recover
	// work accumulated while the leader was down.
	BootTick bool
	// MinBackoff / MaxBackoff bound the exponential backoff applied
	// after an errored tick. Backoff replaces the normal Interval
	// wait for the *next* tick only; a subsequent success resets it.
	// Zero values default to MinBackoff=Interval, MaxBackoff=Interval
	// (i.e. no extra backoff, just retry next interval) — callers
	// that want real backoff set both.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Run is the per-tick work. Required.
	Run RunFunc
	// Logger is the worker's logger. nil defaults to slog.Default().
	Logger *slog.Logger
}

// run drives the periodic loop until ctx is cancelled. It is the
// single implementation of the boot-tick + ticker + select-ctx.Done
// + error-backoff pattern. It never returns an error: a Runner's
// per-tick errors are handled in-loop (log + metric + backoff) per
// the package error policy, so the loop itself never reports a
// failure up to the Supervisor.
func (r Runner) run(ctx context.Context, m *metrics) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	minBackoff := r.MinBackoff
	if minBackoff <= 0 {
		minBackoff = r.Interval
	}
	maxBackoff := r.MaxBackoff
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}

	// tick runs Run once with panic recovery, records the outcome,
	// and returns the wait before the next tick (Interval on success,
	// the current backoff on error).
	backoff := minBackoff
	tick := func() (wait time.Duration) {
		err := safeRun(r.Run, ctx, logger, r.Name)
		switch {
		case err == nil:
			m.recordSuccess(r.Name)
			backoff = minBackoff
			return r.Interval
		case ctx.Err() != nil:
			// Cancellation during the tick is a clean shutdown, not a
			// failure. Don't pollute the error counter; the loop is
			// about to exit on the ctx.Done select anyway.
			return r.Interval
		default:
			m.recordError(r.Name)
			logger.Error("worker tick failed",
				"worker", r.Name, "backoff", backoff, "err", err)
			w := backoff
			backoff = nextBackoff(backoff, maxBackoff)
			return w
		}
	}

	wait := r.Interval
	if r.BootTick {
		if ctx.Err() != nil {
			return
		}
		// Honor tick's returned wait: a failed boot tick retries on
		// the error backoff, not the full interval — same contract as
		// the steady-state loop below.
		wait = tick()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			wait = tick()
			timer.Reset(wait)
		}
	}
}

// safeRun invokes fn with panic recovery, converting a panic into an
// error so the caller's normal error path (log + metric + backoff)
// handles it. Without this a panic in any single tick would unwind
// through the goroutine and crash the pod — exactly the crash
// behavior the package error policy forbids.
//
// The panic is logged HERE, at the recovery point, rather than relying
// on the caller's error handling to surface it. Both callers suppress
// errors when ctx.Err() != nil (a cancellation during teardown is a
// clean shutdown, not a failure worth counting) — but safeRun turns a
// panic INTO an error, so a panic that races with shutdown (nil deref
// or send-on-closed-channel during teardown) would otherwise be
// swallowed by that suppression and never logged. The package error
// policy promises panics are "recovered, logged, and counted"
// unconditionally; logging at recovery is what makes the "logged" part
// hold regardless of the cancellation state. (Counting still defers to
// the caller, which deliberately skips the counter on cancellation.)
func safeRun(fn func(context.Context) error, ctx context.Context, logger *slog.Logger, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("worker panic: %v\n%s", r, debug.Stack())
			logger.Error("worker panicked", "worker", name, "err", err)
		}
	}()
	return fn(ctx)
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// Supervisor runs a set of Runners and long-running Funcs under one
// waitgroup, with shared ctx cancellation and per-worker liveness
// metrics. Construct with NewSupervisor; register with AddRunner /
// AddFunc; call Run to start everything and block until ctx is
// cancelled and every worker has drained.
type Supervisor struct {
	logger  *slog.Logger
	metrics *metrics

	mu      sync.Mutex
	runners []Runner
	funcs   []namedFunc
}

type namedFunc struct {
	name string
	fn   Func
}

// NewSupervisor builds a Supervisor. reg may be nil (tests): metrics
// then register into a throwaway registry so they still function but
// aren't scraped — matching the embedder.Worker convention. When reg is
// also an Observer, it is told every run's outcome. logger nil defaults
// to slog.Default().
func NewSupervisor(logger *slog.Logger, reg prometheusRegisterer) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		logger:  logger,
		metrics: newMetrics(reg),
	}
}

// AddRunner registers a periodic Runner. Panics if the Runner is
// misconfigured (empty name, non-positive interval, nil Run) — these
// are programmer errors caught at wiring time, not runtime
// conditions.
func (s *Supervisor) AddRunner(r Runner) {
	if r.Name == "" {
		panic("worker: Runner.Name is required")
	}
	if r.Interval <= 0 {
		panic(fmt.Sprintf("worker: Runner %q needs a positive Interval", r.Name))
	}
	if r.Run == nil {
		panic(fmt.Sprintf("worker: Runner %q needs a Run func", r.Name))
	}
	if r.Logger == nil {
		r.Logger = s.logger
	}
	s.mu.Lock()
	s.runners = append(s.runners, r)
	s.mu.Unlock()
}

// AddFunc registers a long-running worker that owns its own loop
// (slack consumer, embedder, caption worker). name labels it in logs
// + metrics. Panics on empty name or nil fn (wiring-time errors).
func (s *Supervisor) AddFunc(name string, fn Func) {
	if name == "" {
		panic("worker: AddFunc name is required")
	}
	if fn == nil {
		panic(fmt.Sprintf("worker: AddFunc %q needs a non-nil func", name))
	}
	s.mu.Lock()
	s.funcs = append(s.funcs, namedFunc{name: name, fn: fn})
	s.mu.Unlock()
}

// Run starts every registered worker and blocks until ctx is
// cancelled and all workers have returned. Mirrors the leader-fn
// contract: ctx cancellation (leadership loss or SIGTERM) propagates
// to every worker, and Run only returns once they've all drained, so
// the caller can rely on Run returning == "all leader work stopped."
//
// Per the package error policy a worker error never short-circuits
// Run; long-running Funcs that return early (error or nil) are
// logged + metered, and Run still waits for the remaining workers.
func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	runners := s.runners
	funcs := s.funcs
	s.mu.Unlock()

	var wg sync.WaitGroup

	for _, r := range runners {
		wg.Add(1)
		go func(r Runner) {
			defer wg.Done()
			r.Logger.Info("worker starting",
				"worker", r.Name, "interval", r.Interval, "boot_tick", r.BootTick)
			r.run(ctx, s.metrics)
			r.Logger.Info("worker stopped", "worker", r.Name)
		}(r)
	}

	for _, nf := range funcs {
		wg.Add(1)
		go func(nf namedFunc) {
			defer wg.Done()
			s.logger.Info("worker starting", "worker", nf.name)
			err := safeRun(nf.fn, ctx, s.logger, nf.name)
			switch {
			case err == nil || ctx.Err() != nil:
				// Clean exit, or exit because ctx was cancelled
				// (shutdown). A nil return before shutdown still counts
				// as a "stopped" event worth surfacing — a long-running
				// worker that returns nil while ctx is live has quietly
				// given up, which is the silent-stall failure mode.
				if err == nil && ctx.Err() == nil {
					s.metrics.recordError(nf.name)
					s.logger.Error("worker returned before shutdown",
						"worker", nf.name)
				}
			default:
				s.metrics.recordError(nf.name)
				s.logger.Error("worker exited with error",
					"worker", nf.name, "err", err)
			}
			s.logger.Info("worker stopped", "worker", nf.name)
		}(nf)
	}

	wg.Wait()
}
