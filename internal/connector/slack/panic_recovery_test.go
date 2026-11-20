package slack

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests pin that a panic in a Slack event handler or an Ask turn
// does not crash the process. consumerRunState.acceptHandler and
// consumerRunState.startAsk each run their work on a new goroutine, and
// recover() never crosses a goroutine boundary, so without a recover of
// their own a panic anywhere in that call tree (ingest, directory
// resolution, Ask routing, a tool call, answer delivery) would be fatal
// to the WHOLE diderot process -- every other Slack thread in flight,
// the HTTP API, the MCP server, every other connector -- rather than
// failing just the one event or turn, contrary to
// internal/worker/worker.go's documented error policy.
//
// Both tests run the real run_state.go code in a subprocess: an
// unrecovered panic in a background goroutine kills the process it occurs
// in, so there is no way to observe "did this crash the process" from
// inside the same process. The panic must be recovered, logged, and
// counted, and the process must survive.

// runInSlackPanicSubprocess re-execs the current test binary with the
// given marker set, so the marked branch runs in a fresh process whose
// crash (if any) does not take this test binary down with it.
func runInSlackPanicSubprocess(t *testing.T, testName, marker string) (combinedOutput string, err error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	cmd.Env = append(os.Environ(), "DIDEROT_TEST_PANIC_HELPER="+marker)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// TestHandlerPanicDoesNotCrashProcess: a panic inside a Slack event
// handler accepted via consumerRunState.acceptHandler/startHandler -- the
// path every live MessageEvent, ReactionAddedEvent, AppMentionEvent,
// AgentSessionStoppedEvent, MemberLeftChannelEvent, and InteractionCallback
// goes through (RunWithProcessContext's dispatch loop in slack.go) -- must
// not bring down the whole process.
func TestHandlerPanicDoesNotCrashProcess(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_PANIC_HELPER") == "handler" {
		state := newConsumerRunState(context.Background(), nil)
		if !state.startHandler(func(context.Context) {
			panic("simulated panic from a Slack event handler")
		}) {
			os.Exit(3) // handler rejected -- shouldn't happen against a fresh state
		}
		// Give the spawned goroutine time to actually panic before this
		// process would otherwise exit cleanly.
		time.Sleep(time.Second)
		os.Exit(0) // only reached if the panic did not crash the process
		return
	}

	out, err := runInSlackPanicSubprocess(t, "TestHandlerPanicDoesNotCrashProcess", "handler")
	if err != nil {
		t.Fatalf("a panic inside an accepted Slack event handler must not crash the whole diderot "+
			"process, but the subprocess crashed: %v\nsubprocess output:\n%s", err, out)
	}
	if !strings.Contains(out, "recovered panic") || !strings.Contains(out, "slack-handler") {
		t.Fatalf("expected a recovered-panic log line naming the slack-handler site; got:\n%s", out)
	}
}

// TestAskTurnPanicDoesNotCrashProcess covers the other
// bare goroutine in run_state.go: consumerRunState.startAsk, which runs
// every Ask turn (reached from any @-mention or DM once past the ask
// gates). A panic anywhere in that turn must not take down the process
// serving every other Slack thread, the HTTP API, and the MCP server.
func TestAskTurnPanicDoesNotCrashProcess(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_PANIC_HELPER") == "ask" {
		state := newConsumerRunState(context.Background(), nil)
		if !state.startAsk(func(context.Context) {
			panic("simulated panic from an Ask turn")
		}) {
			os.Exit(3)
		}
		time.Sleep(time.Second)
		os.Exit(0)
		return
	}

	out, err := runInSlackPanicSubprocess(t, "TestAskTurnPanicDoesNotCrashProcess", "ask")
	if err != nil {
		t.Fatalf("a panic inside an Ask turn must not crash the whole diderot process, but the "+
			"subprocess running consumerRunState.startAsk crashed: %v\nsubprocess output:\n%s", err, out)
	}
	if !strings.Contains(out, "recovered panic") || !strings.Contains(out, "slack-ask") {
		t.Fatalf("expected a recovered-panic log line naming the slack-ask site; got:\n%s", out)
	}
}

// TestAcceptedHandlerPanicStillReleasesItsWaitGroupSlot: a recovered panic
// must not leave acceptHandler's accounting stuck -- the shutdown drain
// (consumerRunState.drain) waits on s.handlers, and a handler that never
// calls Done would make a graceful shutdown hang for the full drain
// timeout on every deploy that happens to panic mid-handler.
func TestAcceptedHandlerPanicStillReleasesItsWaitGroupSlot(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	if !state.startHandler(func(context.Context) {
		panic("simulated panic from a Slack event handler")
	}) {
		t.Fatal("handler rejected against a fresh state")
	}
	done := make(chan struct{})
	go func() {
		state.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handlers WaitGroup never drained: a panicking handler left its slot held")
	}
}

// TestAskTurnPanicStillReleasesItsWaitGroupSlot is the same bookkeeping
// check for the Ask-turn path.
func TestAskTurnPanicStillReleasesItsWaitGroupSlot(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	if !state.startAsk(func(context.Context) {
		panic("simulated panic from an Ask turn")
	}) {
		t.Fatal("ask rejected against a fresh state")
	}
	done := make(chan struct{})
	go func() {
		state.asks.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("asks WaitGroup never drained: a panicking turn left its slot held")
	}
}

// panicProgressTransport's SetStatus panics unconditionally, standing in
// for a bug in the status transport call or its metric recording -- the
// only code slackProgress.runStatus runs on every Slack Ask turn.
type panicProgressTransport struct{}

func (panicProgressTransport) SetStatus(context.Context, sessionStatus) progressCallResult {
	panic("simulated panic from the Slack agent-session status transport")
}

// TestSlackAskProgressPanicDoesNotCrashProcess: slackProgress.Start runs
// runStatus on its own goroutine ("go p.runStatus()", progress.go) for
// every Slack Ask turn. Its Discord twin (asksurface.go's progress.Start,
// site "discord-ask-progress") is recovered; this one was not, so a panic
// in the status transport call still crashed the whole process, exactly
// the mechanism this group's fix otherwise closes.
func TestSlackAskProgressPanicDoesNotCrashProcess(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_PANIC_HELPER") == "progress" {
		p := testProgress(panicProgressTransport{})
		p.Start()
		<-p.statusDone
		time.Sleep(time.Second)
		os.Exit(0) // only reached if the panic did not crash the process
		return
	}

	out, err := runInSlackPanicSubprocess(t, "TestSlackAskProgressPanicDoesNotCrashProcess", "progress")
	if err != nil {
		t.Fatalf("a panic inside the Slack ask-progress status goroutine must not crash the whole "+
			"diderot process, but the subprocess crashed: %v\nsubprocess output:\n%s", err, out)
	}
	if !strings.Contains(out, "recovered panic") || !strings.Contains(out, "slack-ask-progress") {
		t.Fatalf("expected a recovered-panic log line naming the slack-ask-progress site; got:\n%s", out)
	}
}
