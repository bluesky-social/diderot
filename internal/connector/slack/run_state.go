package slack

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/worker"
)

// gracefulDrainTimeout leaves 30 seconds of the pod's 15-minute termination
// grace for the HTTP server and process-level cleanup after accepted Slack
// work has drained.
const gracefulDrainTimeout = 14*time.Minute + 30*time.Second

// consumerRunState owns the asynchronous work accepted by one socket-mode
// run. The acceptance flags make WaitGroup.Add and Wait mutually exclusive:
// stopHandlers closes handler admission before waiting, and ask admission is
// closed only after every accepted handler has returned.
type consumerRunState struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger

	mu                sync.Mutex
	acceptingHandlers bool
	acceptingAsks     bool
	handlers          sync.WaitGroup
	asks              sync.WaitGroup
}

func newConsumerRunState(parent context.Context, logger *slog.Logger) *consumerRunState {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	if logger == nil {
		logger = slog.Default()
	}
	return &consumerRunState{
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		acceptingHandlers: true,
		acceptingAsks:     true,
	}
}

// acceptHandler reserves one handler slot and returns a starter that launches
// it. Socket Mode uses the split operation to reserve before acknowledging the
// envelope, then launch immediately after the ack. If admission is closed the
// caller must not ack, so Slack can redeliver the event to the new leader.
func (s *consumerRunState) acceptHandler(fn func(context.Context)) (func(), bool) {
	s.mu.Lock()
	if !s.acceptingHandlers {
		s.mu.Unlock()
		return nil, false
	}
	s.handlers.Add(1)
	s.mu.Unlock()

	return func() {
		go func() {
			defer s.handlers.Done()
			// A panic anywhere in fn's call tree (ingest, directory
			// resolution, Ask routing...) must fail only this one event,
			// not the whole process: see worker.Recover.
			defer worker.Recover(s.logger, "slack-handler")
			fn(s.ctx)
		}()
	}, true

}

func (s *consumerRunState) startHandler(fn func(context.Context)) bool {
	start, accepted := s.acceptHandler(fn)
	if accepted {
		start()
	}
	return accepted
}

func (s *consumerRunState) startAsk(fn func(context.Context)) bool {
	s.mu.Lock()
	if !s.acceptingAsks {
		s.mu.Unlock()
		return false
	}
	s.asks.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.asks.Done()
		// A panic anywhere in the turn (the askchat engine, a tool call,
		// answer delivery) must fail only this one turn, not the whole
		// process: see worker.Recover. askchat.Engine.RunTurn already
		// recovers panics from the turn it runs (ask.AskService.ExecuteTurn
		// and its own delivery/cleanup code), so by the time a panic would
		// reach here it is something that escaped that -- still contained,
		// never silently swallowed.
		defer worker.Recover(s.logger, "slack-ask")
		fn(s.ctx)
	}()
	return true
}

func (s *consumerRunState) stopHandlers() {
	s.mu.Lock()
	s.acceptingHandlers = false
	s.mu.Unlock()
}

func (s *consumerRunState) stopAsks() {
	s.mu.Lock()
	s.acceptingAsks = false
	s.mu.Unlock()
}

func (s *consumerRunState) cancelWork() {
	s.cancel()
}

func waitGroupDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// drain keeps accepted work alive until it finishes or timeout expires. It
// first waits for already-acked event handlers, since those handlers may still
// route an ask, then closes ask admission and waits for the turns themselves.
// The bool reports whether all work completed inside the grace window.
func (s *consumerRunState) drain(timeout time.Duration) bool {
	s.stopHandlers()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-waitGroupDone(&s.handlers):
	case <-timer.C:
		s.stopAsks()
		s.cancel()
		// Cancellation is cooperative. A Slack or DB call that ignores its
		// context must not turn the bounded pod drain into an unbounded wait;
		// process termination will reap anything still stuck after cleanup.
		return false
	}

	s.stopAsks()
	select {
	case <-waitGroupDone(&s.asks):
		s.cancel()
		return true
	case <-timer.C:
		s.cancel()
		return false
	}
}

// cancelAndWait is used for leadership loss and socket failure. In those
// cases continuing an ask could let two leaders answer concurrently.
func (s *consumerRunState) cancelAndWait() {
	s.cancel()
	s.stopHandlers()
	s.handlers.Wait()
	s.stopAsks()
	s.asks.Wait()
}

// consumerRunTracker publishes the state belonging to the current Run call.
// Tests that invoke routing helpers directly have no installed run and retain
// the historical fire-and-forget behavior.
type consumerRunTracker struct {
	mu     sync.Mutex
	state  *consumerRunState
	closed bool
}

func (t *consumerRunTracker) install(state *consumerRunState) {
	t.mu.Lock()
	t.state = state
	t.closed = false
	t.mu.Unlock()
}

func (t *consumerRunTracker) uninstall(state *consumerRunState) {
	t.mu.Lock()
	if t.state == state {
		t.state = nil
		t.closed = true
	}
	t.mu.Unlock()
}

func (t *consumerRunTracker) startAsk(fallback context.Context, fn func(context.Context)) bool {
	t.mu.Lock()
	state, closed := t.state, t.closed
	if state != nil {
		started := state.startAsk(fn)
		t.mu.Unlock()
		return started
	}
	if closed {
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	go func() {
		// No installed run means this is the historical fire-and-forget
		// path tests use when they call routing helpers directly; the
		// same recovery as the installed-run case still applies so a
		// panic there fails a test instead of crashing the whole binary.
		defer worker.Recover(slog.Default(), "slack-ask-fallback")
		fn(fallback)
	}()
	return true
}
