package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// quietLogger discards output so panic-recovery tests don't spam.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func counterValue(t *testing.T, reg *prometheus.Registry, name, worker, outcome string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelMatch(m, "worker", worker) && labelMatch(m, "outcome", outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name, worker string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelMatch(m, "worker", worker) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelMatch(m *dto.Metric, key, want string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == key {
			return l.GetValue() == want
		}
	}
	return false
}

// TestRunnerBootTick verifies the boot tick fires immediately, before
// the first interval elapses — load-bearing for scopesync's
// post-migration backfill and the reconcile loop's down-recovery.
func TestRunnerBootTick(t *testing.T) {
	var ticks atomic.Int64
	first := make(chan struct{})
	r := Runner{
		Name:     "boot",
		Interval: time.Hour, // long enough that only the boot tick fires
		BootTick: true,
		Logger:   quietLogger(),
		Run: func(context.Context) error {
			if ticks.Add(1) == 1 {
				close(first)
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.run(ctx, newMetrics(nil))

	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("boot tick did not fire promptly")
	}
	cancel()
	if got := ticks.Load(); got != 1 {
		t.Fatalf("expected exactly 1 tick (boot only), got %d", got)
	}
}

// TestRunnerBootTickFailureUsesBackoff verifies a FAILED boot tick
// schedules the first retry on the error backoff, not the full
// interval — a worker with Interval=1h, MinBackoff=10ms must retry
// in ~10ms, not stall an hour.
func TestRunnerBootTickFailureUsesBackoff(t *testing.T) {
	var ticks atomic.Int64
	second := make(chan struct{})
	r := Runner{
		Name:       "boot-fail",
		Interval:   time.Hour, // a discarded backoff would stall here
		BootTick:   true,
		MinBackoff: 10 * time.Millisecond,
		Logger:     quietLogger(),
		Run: func(context.Context) error {
			if ticks.Add(1) == 2 {
				close(second)
			}
			return errors.New("boom")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.run(ctx, newMetrics(nil))

	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("retry after failed boot tick did not fire on backoff; first retry is stalling on the full interval")
	}
}

// TestRunnerNoBootTick verifies that without BootTick the first run
// waits for the interval.
func TestRunnerNoBootTick(t *testing.T) {
	var ticks atomic.Int64
	r := Runner{
		Name:     "nobootick",
		Interval: 20 * time.Millisecond,
		Logger:   quietLogger(),
		Run: func(context.Context) error {
			ticks.Add(1)
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.run(ctx, newMetrics(nil))

	// Immediately after start (well under one interval) no tick yet.
	time.Sleep(5 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("expected 0 ticks before first interval, got %d", got)
	}
	// After a few intervals, ticks accumulate.
	time.Sleep(80 * time.Millisecond)
	if got := ticks.Load(); got < 2 {
		t.Fatalf("expected >=2 ticks after several intervals, got %d", got)
	}
}

// TestRunnerPeriodic verifies steady-state ticking on the interval.
func TestRunnerPeriodic(t *testing.T) {
	var ticks atomic.Int64
	r := Runner{
		Name:     "periodic",
		Interval: 10 * time.Millisecond,
		BootTick: true,
		Logger:   quietLogger(),
		Run: func(context.Context) error {
			ticks.Add(1)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	r.run(ctx, newMetrics(nil))
	// boot tick + ~11 interval ticks; allow slack for scheduler jitter.
	if got := ticks.Load(); got < 4 {
		t.Fatalf("expected multiple periodic ticks, got %d", got)
	}
}

// TestRunnerErrorBackoff verifies an errored tick does not kill the
// loop, increments the error counter, and the loop keeps retrying.
func TestRunnerErrorBackoff(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	var attempts atomic.Int64
	r := Runner{
		Name:       "flaky",
		Interval:   10 * time.Millisecond,
		BootTick:   true,
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
		Logger:     quietLogger(),
		Run: func(context.Context) error {
			n := attempts.Add(1)
			if n <= 3 {
				return errors.New("boom")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.run(ctx, m)

	if got := attempts.Load(); got < 4 {
		t.Fatalf("loop should have survived errors and reached success, attempts=%d", got)
	}
	if errs := counterValue(t, reg, "diderot_worker_runs_total", "flaky", "error"); errs < 3 {
		t.Fatalf("expected >=3 error runs recorded, got %v", errs)
	}
	if ok := counterValue(t, reg, "diderot_worker_runs_total", "flaky", "success"); ok < 1 {
		t.Fatalf("expected >=1 success run recorded, got %v", ok)
	}
	if _, present := gaugeValue(t, reg, "diderot_worker_last_success_timestamp_seconds", "flaky"); !present {
		t.Fatal("expected last_success gauge to be set after a successful tick")
	}
}

// TestRunnerPanicRecovered verifies a panicking tick is recovered,
// counted as an error, and the loop keeps running (does not crash the
// process) — the core of the "never kill the pod" policy.
func TestRunnerPanicRecovered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	var attempts atomic.Int64
	r := Runner{
		Name:       "panicky",
		Interval:   10 * time.Millisecond,
		BootTick:   true,
		MinBackoff: 5 * time.Millisecond,
		Logger:     quietLogger(),
		Run: func(context.Context) error {
			if attempts.Add(1) == 1 {
				panic("kaboom")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r.run(ctx, m) // must not propagate the panic

	if got := attempts.Load(); got < 2 {
		t.Fatalf("loop should have continued past the panic, attempts=%d", got)
	}
	if errs := counterValue(t, reg, "diderot_worker_runs_total", "panicky", "error"); errs < 1 {
		t.Fatalf("expected the panic recorded as an error run, got %v", errs)
	}
}

// TestSafeRunLogsPanicDuringCancellation pins the "panics are always
// logged" half of the error policy: a panic that races with shutdown
// (ctx already cancelled) must still be logged, even though both
// callers deliberately suppress the resulting error from the metrics
// counter on cancellation. Logging at the recovery point in safeRun
// is what keeps the panic stack trace from vanishing in that window.
func TestSafeRunLogsPanicDuringCancellation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the teardown-race window

	err := safeRun(func(context.Context) error {
		panic("teardown nil deref")
	}, ctx, logger, "shutting-down-worker")

	if err == nil {
		t.Fatal("panic should have been converted to an error")
	}
	logged := buf.String()
	if !strings.Contains(logged, "worker panicked") ||
		!strings.Contains(logged, "shutting-down-worker") ||
		!strings.Contains(logged, "teardown nil deref") {
		t.Fatalf("panic during cancellation was not logged with context; got: %q", logged)
	}
}

// TestRunnerStopsOnCancel verifies the loop returns promptly when ctx
// is cancelled.
func TestRunnerStopsOnCancel(t *testing.T) {
	r := Runner{
		Name:     "stopper",
		Interval: 10 * time.Millisecond,
		Logger:   quietLogger(),
		Run:      func(context.Context) error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.run(ctx, newMetrics(nil))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop promptly on cancel")
	}
}

// TestSupervisorWaitsForAll verifies Run blocks until every worker —
// Runner and Func alike — has drained after ctx cancellation.
func TestSupervisorWaitsForAll(t *testing.T) {
	sup := NewSupervisor(quietLogger(), nil)

	var runnerStopped, funcStopped atomic.Bool
	sup.AddRunner(Runner{
		Name:     "r1",
		Interval: 5 * time.Millisecond,
		BootTick: true,
		Run:      func(context.Context) error { return nil },
	})
	// A long-running func that exits on ctx and sets a flag, with a
	// short cleanup delay to prove Run actually waits for it.
	sup.AddFunc("f1", func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		funcStopped.Store(true)
		return nil
	})
	// Wrap the runner's stop detection via a second runner that flips
	// a flag when it sees cancellation through its own ctx.
	sup.AddFunc("f2", func(ctx context.Context) error {
		<-ctx.Done()
		runnerStopped.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor Run did not return after cancel")
	}
	if !funcStopped.Load() || !runnerStopped.Load() {
		t.Fatal("supervisor returned before all workers drained")
	}
}

// TestSupervisorOneWorkerErrorDoesNotStopOthers verifies the error
// policy: a Func that returns/errors early does not cancel siblings
// or stop the supervisor; the rest run until ctx cancellation.
func TestSupervisorOneWorkerErrorDoesNotStopOthers(t *testing.T) {
	reg := prometheus.NewRegistry()
	sup := NewSupervisor(quietLogger(), reg)

	var sibling atomic.Int64
	// Worker that dies immediately with an error.
	sup.AddFunc("dies", func(context.Context) error {
		return errors.New("early death")
	})
	// Sibling Runner that should keep ticking regardless.
	sup.AddRunner(Runner{
		Name:     "survivor",
		Interval: 10 * time.Millisecond,
		BootTick: true,
		Run: func(context.Context) error {
			sibling.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sup.Run(ctx)
	}()
	wg.Wait()

	if got := sibling.Load(); got < 3 {
		t.Fatalf("sibling should have kept ticking after the other worker died, ticks=%d", got)
	}
	if errs := counterValue(t, reg, "diderot_worker_runs_total", "dies", "error"); errs < 1 {
		t.Fatalf("expected the dead worker's error recorded, got %v", errs)
	}
}

// TestSupervisorFuncPanicRecovered verifies a panic in a long-running
// Func is recovered + recorded, and does not crash the process or
// stop siblings.
func TestSupervisorFuncPanicRecovered(t *testing.T) {
	reg := prometheus.NewRegistry()
	sup := NewSupervisor(quietLogger(), reg)

	var sibling atomic.Int64
	sup.AddFunc("panicfunc", func(context.Context) error {
		panic("func kaboom")
	})
	sup.AddRunner(Runner{
		Name:     "survivor2",
		Interval: 10 * time.Millisecond,
		BootTick: true,
		Run: func(context.Context) error {
			sibling.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sup.Run(ctx) // must not propagate the panic

	if errs := counterValue(t, reg, "diderot_worker_runs_total", "panicfunc", "error"); errs < 1 {
		t.Fatalf("expected the func panic recorded as an error, got %v", errs)
	}
	if got := sibling.Load(); got < 1 {
		t.Fatalf("sibling should have run despite the panicking func, ticks=%d", got)
	}
}

// TestAddRunnerValidation verifies wiring-time validation panics.
func TestAddRunnerValidation(t *testing.T) {
	sup := NewSupervisor(quietLogger(), nil)
	cases := []Runner{
		{Interval: time.Second, Run: func(context.Context) error { return nil }}, // no name
		{Name: "x", Run: func(context.Context) error { return nil }},             // no interval
		{Name: "x", Interval: time.Second},                                       // no Run
	}
	for i, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("case %d: expected AddRunner to panic on misconfiguration", i)
				}
			}()
			sup.AddRunner(c)
		}()
	}
}

// observingRegistry is a registerer that is also an Observer, as a
// connector's metrics are.
type observingRegistry struct {
	*prometheus.Registry
	succeeded, failed atomic.Int64
}

func (o *observingRegistry) Succeeded() { o.succeeded.Add(1) }
func (o *observingRegistry) Failed()    { o.failed.Add(1) }

// TestSupervisorTellsItsObserver: a supervisor built with a registerer that
// is an Observer tells it each run's outcome.
func TestSupervisorTellsItsObserver(t *testing.T) {
	reg := &observingRegistry{Registry: prometheus.NewRegistry()}
	sup := NewSupervisor(quietLogger(), reg)
	var ticks atomic.Int64
	sup.AddRunner(Runner{
		Name: "flaky", Interval: time.Millisecond, BootTick: true, MinBackoff: time.Millisecond, Logger: quietLogger(),
		Run: func(context.Context) error {
			if ticks.Add(1)%2 == 0 {
				return errors.New("boom")
			}
			return nil
		},
	})
	sup.AddFunc("quits", func(context.Context) error { return errors.New("gave up") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for reg.succeeded.Load() < 2 || reg.failed.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("observer saw %d successes and %d failures", reg.succeeded.Load(), reg.failed.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
