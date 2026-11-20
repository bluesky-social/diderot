package adminapi

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This file pins panic recovery in handleAsk's detached notification
// goroutine (the `go func() { ... n.NotifyAskInvocation(...) }()` guarding
// the "ask: invocation DM failed" log). It reaches the same
// Consumer.sendAskNotification as the MCP ("mcp-notify-invocation") and
// Slack ("slack-notify-mention") call sites, and like them must recover: an
// unrecovered panic in that shared formatter or sender would crash the whole
// replica -- every other in-flight /v1/ask request and MCP task, plus
// Slack/Discord turns if that replica is the leader.
//
// panicAskInvocationNotifier panics unconditionally, standing in for a
// bug deep in the shared notification path (a directory lookup, DM
// building).
type panicAskInvocationNotifier struct{}

func (panicAskInvocationNotifier) NotifyAskInvocation(context.Context, string, string, string) error {
	panic("simulated panic in the ask-invocation notifier")
}

func (panicAskInvocationNotifier) NotifyMCPInvocation(context.Context, string, string, string, string) error {
	return nil
}

// TestHTTPNotifyInvocationPanicDoesNotCrashProcess: a panic in the
// detached notification goroutine POST /v1/ask spawns after accepting a
// turn must not bring down the whole replica -- it must be recovered,
// logged, and counted like every other serving-path goroutine.
//
// The repro runs the exact HTTP call in a subprocess: an unrecovered
// panic on a background goroutine kills the process it occurs in, so
// there is no way to observe "did this crash the process" from inside
// the same process.
func TestHTTPNotifyInvocationPanicDoesNotCrashProcess(t *testing.T) {
	const sentinel = "DIDEROT_TEST_HTTP_NOTIFY_PANIC_CHILD"
	if os.Getenv(sentinel) == "1" {
		runHTTPNotifyInvocationPanicChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPNotifyInvocationPanicDoesNotCrashProcess$", "-test.v")
	cmd.Env = append(os.Environ(), sentinel+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child process output:\n%s", out)

	if err != nil {
		t.Fatalf("a panic in the detached ask-invocation notifier must not crash the whole replica "+
			"process, but the subprocess crashed: %v\nsubprocess output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "recovered panic") || !strings.Contains(string(out), "http-notify-invocation") {
		t.Fatalf("expected a recovered-panic log line naming the http-notify-invocation site; got:\n%s", out)
	}
}

func runHTTPNotifyInvocationPanicChild(t *testing.T) {
	pool := askSessionPool(t)
	s, _ := newTestServerAs(t, pool, "alice@example.com")
	s.SetAskInvocationNotifier(panicAskInvocationNotifier{})

	rr := postAsk(t, s, `{"question": "trigger the notifier panic"}`)
	if rr.Code != 200 {
		t.Fatalf("ask: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// handleAsk returns as soon as ExecuteTurn does; the notifier panics on
	// its own detached goroutine afterward. Give it time to run (and,
	// before the fix, to crash the process) before this process would
	// otherwise exit cleanly: a background goroutine's panic has no effect
	// on a process that has already returned from main.
	time.Sleep(time.Second)
	os.Exit(0) // only reached if the panic did not crash the process
}
