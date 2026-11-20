package worker

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// panicCount reads diderot_handler_panics_total{site} from the process's
// default registry (Recover always registers there; see panic.go). Tests
// read a delta around their own call, since the counter is shared process
// state other tests in this binary also increment.
func panicCount(t *testing.T, site string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "diderot_handler_panics_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelMatch(m, "site", site) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestRecoverAbsorbsPanicLogsAndCounts is the contract every serving-path
// goroutine launch site relies on: deferring Recover at the top of a
// goroutine stops a panic from propagating (so the process survives),
// logs it at error level with its stack and the site, and increments
// diderot_handler_panics_total{site} once.
func TestRecoverAbsorbsPanicLogsAndCounts(t *testing.T) {
	const site = "test-site-absorbs"
	before := panicCount(t, site)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	survived := false
	func() {
		defer func() {
			// Runs after Recover's own defer (registered first here, so it
			// runs last): proves Recover actually stopped the panic rather
			// than merely observing it on its way through.
			if r := recover(); r != nil {
				t.Fatalf("panic should have been absorbed by Recover, but reached this frame too: %v", r)
			}
			survived = true
		}()
		// Recover must be deferred directly -- not wrapped in another
		// closure -- or Go's recover() will not see the panic; see its
		// doc comment.
		defer Recover(logger, site)
		panic("boom: simulated handler bug")
	}()

	if !survived {
		t.Fatal("the panic should not have unwound past Recover")
	}
	if !strings.Contains(buf.String(), "recovered panic") || !strings.Contains(buf.String(), site) ||
		!strings.Contains(buf.String(), "boom: simulated handler bug") {
		t.Fatalf("expected an error log naming the site and the panic value, got: %s", buf.String())
	}
	if got := panicCount(t, site) - before; got != 1 {
		t.Fatalf("diderot_handler_panics_total{site=%q} should have incremented by 1, got %v", site, got)
	}
}

// TestRecoverNoPanicIsNoop proves Recover does not report, log, or count
// anything when the deferred function it guards returns normally.
func TestRecoverNoPanicIsNoop(t *testing.T) {
	const site = "test-site-noop"
	before := panicCount(t, site)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	func() {
		defer Recover(logger, site)
	}()

	if buf.Len() != 0 {
		t.Fatalf("expected no log output, got: %s", buf.String())
	}
	if got := panicCount(t, site) - before; got != 0 {
		t.Fatalf("metric should not have moved, got delta %v", got)
	}
}

// TestRecoverProtectsAGoroutineItDidNotCreate shows Recover works exactly
// where every production call site needs it: deferred at the top of a
// bare `go func() { ... }()`, guarding a panic raised by code the goroutine
// calls into (not inlined at the panic site itself) -- the same shape as
// consumerRunState.acceptHandler, asker.spawn, and corpusTaskStore.create.
func TestRecoverProtectsAGoroutineItDidNotCreate(t *testing.T) {
	const site = "test-site-goroutine"
	before := panicCount(t, site)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	panicking := func() { panic("boom: deep bug") }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer Recover(logger, site)
		panicking()
	}()
	wg.Wait() // if the panic were not recovered, this goroutine would have
	// crashed the whole test binary before reaching wg.Done().

	if got := panicCount(t, site) - before; got != 1 {
		t.Fatalf("expected the metric to move once, got delta %v", got)
	}
	if !strings.Contains(buf.String(), site) {
		t.Fatalf("expected a log line naming the site, got: %s", buf.String())
	}
}

// TestRecoverRunsOnPanicCleanupOnlyWhenItRecovers is the pattern a site
// with its own bookkeeping to release uses (the MCP ask task runner
// finishing its task as failed, an Ask turn posting its ordinary failure
// reply): `defer worker.Recover(logger, site, cleanup)`, cleanup still
// deferred directly through Recover itself. cleanup must run when a panic
// was absorbed, and must not run otherwise.
func TestRecoverRunsOnPanicCleanupOnlyWhenItRecovers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	t.Run("panicked", func(t *testing.T) {
		ran := false
		func() {
			defer Recover(logger, "test-site-cleanup-panicked", func() { ran = true })
			panic("boom")
		}()
		if !ran {
			t.Fatal("cleanup should have run after an absorbed panic")
		}
	})

	t.Run("no panic", func(t *testing.T) {
		ran := false
		func() {
			defer Recover(logger, "test-site-cleanup-clean", func() { ran = true })
		}()
		if ran {
			t.Fatal("cleanup should not run when nothing panicked")
		}
	})
}
