package ask

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// This file is a deterministic regression test for a confirmed bug:
// corpusTaskStore.create (internal/ask/corpus_mcp_tasks.go) ran an MCP ask
// task's turn on a bare goroutine with no recover, and the go-sdk itself
// dispatches every MCP method (retrieve, list, expand, attachment,
// tasks/*, and the ask task middleware's own pre-work) on its own
// goroutine with no recover either -- unlike /v1/ask, which runs on
// net/http's per-request, already-recovered handler goroutine. A panic
// anywhere in either path crashed the WHOLE replica process: every other
// request, Slack turn, and task it was serving, not just the one task or
// call that panicked.

// panicAskRunner's ExecuteTurn panics unconditionally. It stands in for
// any bug deep in the turn pipeline (a malformed model response, a nil
// tool result, a bad type assertion in a connector) reaching the MCP ask
// task path.
type panicAskRunner struct{}

func (panicAskRunner) CreateOwnedSession(context.Context, string) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (panicAskRunner) OwnsSession(context.Context, uuid.UUID, string) (bool, error) {
	return true, nil
}

func (panicAskRunner) ReadArtifact(context.Context, uuid.UUID, uuid.UUID) (ArtifactRecord, []byte, error) {
	return ArtifactRecord{}, nil, errors.New("not implemented")
}

func (panicAskRunner) ExecuteTurn(context.Context, TurnRequest) (TurnResult, error) {
	panic("ask turn panicked: simulated bug deep in the turn pipeline")
}

func (panicAskRunner) Hold() (func(), bool) { return func() {}, true }

// TestMCPAskTaskPanicEndsAsFailedTaskNotProcessCrash: a turn panicking
// inside the MCP ask task's background goroutine must not crash the
// replica serving every other request, Slack turn, and task -- it must
// instead finish that one task as "failed", observable by any client
// polling tasks/get.
//
// The repro runs the exact RPC calls in a child process, since an
// unrecovered panic would otherwise kill this test binary along with it.
func TestMCPAskTaskPanicEndsAsFailedTaskNotProcessCrash(t *testing.T) {
	const sentinel = "DIDEROT_TEST_MCP_TASK_PANIC_CHILD"
	if os.Getenv(sentinel) == "1" {
		runMCPAskTaskPanicChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPAskTaskPanicEndsAsFailedTaskNotProcessCrash$", "-test.v")
	cmd.Env = append(os.Environ(), sentinel+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child process output:\n%s", out)

	require.NoError(t, err,
		"an MCP ask task's turn panicking must not crash the whole replica process; "+
			"child process output:\n%s", out)
	require.Contains(t, string(out), "task finished as failed",
		"the panicking task must reach a terminal \"failed\" status instead of staying \"working\" "+
			"until its TTL")
}

func runMCPAskTaskPanicChild(t *testing.T) {
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, panicAskRunner{})
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "trigger the panic"},
	}, true))
	require.Equal(t, "working", create["status"])
	taskID := create["taskId"].(string)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := resultObject(t, callRPC(t, server, "alex@example.com", "tasks/get", taskID,
			map[string]any{"taskId": taskID}, true))
		if got["status"] == "failed" {
			_, _ = os.Stderr.WriteString("task finished as failed\n")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task never reached a terminal status")
}

// panicRetriever's Retrieve and List panic unconditionally, standing in for
// a bug reached by a synchronous MCP tool call outside the ask task path.
type panicRetriever struct{}

func (panicRetriever) Retrieve(context.Context, retrieve.Request) (*retrieve.Result, error) {
	panic("retrieve panicked: simulated bug in a synchronous MCP tool handler")
}

func (panicRetriever) List(context.Context, retrieve.ListRequest) (*retrieve.Result, error) {
	panic("list panicked: simulated bug in a synchronous MCP tool handler")
}

// TestMCPRetrievePanicDoesNotCrashProcess: the go-sdk dispatches every MCP
// method, including a plain synchronous tools/call retrieve with no task
// involved, on its own goroutine with no recover of its own. A panic
// there must not crash the whole replica either.
func TestMCPRetrievePanicDoesNotCrashProcess(t *testing.T) {
	const sentinel = "DIDEROT_TEST_MCP_RETRIEVE_PANIC_CHILD"
	if os.Getenv(sentinel) == "1" {
		runMCPRetrievePanicChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPRetrievePanicDoesNotCrashProcess$", "-test.v")
	cmd.Env = append(os.Environ(), sentinel+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child process output:\n%s", out)

	require.NoError(t, err,
		"a panic in a synchronous MCP tool handler must not crash the whole replica process; "+
			"child process output:\n%s", out)
	require.Contains(t, string(out), "retrieve call returned an error, not a crash")
}

func runMCPRetrievePanicChild(t *testing.T) {
	server := newCorpusMCPHTTPServer(t, panicRetriever{}, panicAskRunner{})
	resp := callRPC(t, server, "alex@example.com", "tools/call", "retrieve", map[string]any{
		"name": "retrieve", "arguments": map[string]any{"question": "trigger the panic"},
	}, true)
	// Either an MCP-level protocol error or an isError tool result is an
	// acceptable "contained" outcome; what matters is that a response came
	// back at all instead of the connection dying with the process.
	if resp.Error == nil {
		result := resultObject(t, resp)
		require.Equal(t, true, result["isError"])
	}
	_, _ = os.Stderr.WriteString("retrieve call returned an error, not a crash\n")
}

// panickingReferenceResolver panics instead of resolving an explicit
// corpus handle, standing in for a bug deep in AskService's own turn
// pipeline -- the code every surface (HTTP, Slack, Discord, and the MCP
// ask task) shares through AskService.ExecuteTurn.
type panickingReferenceResolver struct{}

func (panickingReferenceResolver) PrepareReferences(context.Context, []CorpusReference, []string) (ReferencePrerequisiteState, error) {
	panic("simulated bug deep in the turn pipeline")
}

// panickingScopeGranter panics instead of widening a turn's scopes,
// standing in for a bug in ExecuteTurn's own preamble -- before
// runTurnLocked, and so outside its recover.
type panickingScopeGranter struct{}

func (panickingScopeGranter) GrantScopes(context.Context, []string) []string {
	panic("simulated bug in ExecuteTurn's preamble, before runTurnLocked")
}

// TestAskServiceExecuteTurnPreamblePanicEndsAsOrdinaryFailure is the
// companion to TestAskServiceTurnPanicEndsAsOrdinaryFailureNotProcessCrash:
// it panics in ExecuteTurn's own preamble (scopeGrants.GrantScopes), which
// runs before runTurnLocked and so is outside that function's own
// recovery. ExecuteTurn needs its own outer recover for the shared-turn
// contract to hold on every path through it, not only the inner one.
func TestAskServiceExecuteTurnPreamblePanicEndsAsOrdinaryFailure(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the model must never be called once the turn panicked before reaching it")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	svc.scopeGrants = panickingScopeGranter{}

	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "hello",
		Surface: AskRunSurfaceHTTP, AllowedScopes: []string{"slack-channel:C_SAME"},
	})
	require.Error(t, err, "a panic in ExecuteTurn's own preamble must surface as an ordinary error, "+
		"not crash the process serving this turn")
	require.Contains(t, err.Error(), "panicked")
	require.Empty(t, result.Reply)
}

// TestAskServiceTurnPanicEndsAsOrdinaryFailureNotProcessCrash is the
// shared-code counterpart of the two MCP tests above: AskService.
// ExecuteTurn (internal/ask/service.go, runTurnLocked) is the ONE place
// every surface -- HTTP, Slack, Discord, and the MCP ask task -- runs a
// turn through. A panic anywhere in it must come back as an ordinary
// error (which each surface already turns into its own failure path: an
// HTTP 500, a Slack/Discord reply, a failed MCP task), not as an
// unrecovered panic that would crash whichever process is serving it.
//
// Unlike the MCP tests above, this does not need a subprocess: the fix
// recovers the panic before it can unwind out of ExecuteTurn, so calling
// it directly is safe once fixed. Before the fix, running this single
// test (in isolation; a stray panic would otherwise take any other test
// sharing the process down with it) crashes the test binary instead of
// reporting a clean failure.
func TestAskServiceTurnPanicEndsAsOrdinaryFailureNotProcessCrash(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the model must never be called once the turn panicked before reaching it")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	svc.references = panickingReferenceResolver{}

	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "summarize event_id:42",
		Surface: AskRunSurfaceHTTP, AllowedScopes: []string{"slack-channel:C_SAME"},
	})
	require.Error(t, err, "a panic deep in the turn pipeline must surface as an ordinary error, "+
		"not crash the process serving this turn")
	require.Contains(t, err.Error(), "panicked")
	require.Empty(t, result.Reply)
}

// panicDeleteBackend wraps the in-memory backend and panics in Delete,
// standing in for a bug in the backend call corpusTaskStore.expire makes
// (today, Postgres; in principle any corpusTaskBackend implementation).
type panicDeleteBackend struct {
	*memoryCorpusTaskBackend
}

func (panicDeleteBackend) Delete(context.Context, string) error {
	panic("simulated panic from the MCP task's TTL expire callback")
}

// TestMCPTaskExpirePanicDoesNotCrashProcess: corpusTaskStore.create
// schedules `time.AfterFunc(taskTTL, func() { s.expireRecovered(...) })`
// once per MCP ask task, on every replica. Unlike the task's own turn
// goroutine ("mcp-ask-task") and its cancellation watcher
// ("mcp-task-cancellation-watch"), this callback ran with no recover of
// its own, so a panic in it -- reached once per task, an hour after every
// task is created, whether or not the turn itself ever ran into trouble
// -- still crashed the whole replica.
//
// The repro runs expireRecovered directly (bypassing the real taskTTL,
// which is an hour) in a subprocess: an unrecovered panic on a background
// goroutine kills the process it occurs in, so there is no way to
// observe "did this crash the process" from inside the same process.
func TestMCPTaskExpirePanicDoesNotCrashProcess(t *testing.T) {
	const sentinel = "DIDEROT_TEST_MCP_TASK_EXPIRE_PANIC_CHILD"
	if os.Getenv(sentinel) == "1" {
		store := newCorpusTaskStore(panicDeleteBackend{newMemoryCorpusTaskBackend()},
			func() CorpusAskLease { return CorpusAskLease{} }, context.Background())
		go store.expireRecovered("some-task-id")
		time.Sleep(time.Second)
		os.Exit(0) // only reached if the panic did not crash the process
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPTaskExpirePanicDoesNotCrashProcess$", "-test.v")
	cmd.Env = append(os.Environ(), sentinel+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child process output:\n%s", out)

	require.NoError(t, err,
		"a panic in the MCP ask task's TTL expire callback must not crash the whole replica "+
			"process; child process output:\n%s", out)
	require.Contains(t, string(out), "recovered panic")
	require.Contains(t, string(out), "mcp-task-expire")
}

// TestSessionLeaseRenewalPanicDoesNotCrashProcess: withSessionLease
// (internal/ask/session_lease.go) spawns Manager.renewSessionLease on its
// own bare goroutine for every turn, on every surface (HTTP, Slack,
// Discord, the MCP ask task) -- exactly the same per-turn-renewal-loop
// shape as askchat's workingTurn.renew ("ask-working-renew"), and it
// needs a recover of its own. A panic here is outside the reach of
// ExecuteTurn's own "ask-turn-executor" recover, since recover() never
// crosses a goroutine boundary: unrecovered, it would crash the whole
// process serving this turn (and every other
// request, Slack turn, and task it is serving) instead of just ending
// this one turn.
//
// time.NewTicker panics on a non-positive interval: it stands in for any
// bug in this function (a future change to the renew interval, a bad
// config value) the same way the other probes in this file use an
// unconditional panic to stand in for a latent bug elsewhere in their
// call tree. The repro calls renewSessionLease directly, bypassing
// takeSessionLease's real database round trip (irrelevant here, and
// already covered by ExecuteTurn's own synchronous recover), in a
// subprocess: an unrecovered panic on a background goroutine kills the
// process it occurs in, so there is no way to observe "did this crash
// the process" from inside the same process.
func TestSessionLeaseRenewalPanicDoesNotCrashProcess(t *testing.T) {
	const sentinel = "DIDEROT_TEST_SESSION_LEASE_RENEW_PANIC_CHILD"
	if os.Getenv(sentinel) == "1" {
		m := &Manager{logger: slog.Default(), lease: sessionLeaseTiming{renew: 0}}
		lost := make(chan error, 1)
		go m.renewSessionLease(context.Background(), uuid.New(), uuid.New(),
			func(cause error) { lost <- cause })
		select {
		case cause := <-lost:
			if !errors.Is(cause, errSessionLeaseLost) {
				_, _ = os.Stderr.WriteString("lost() called with the wrong cause: " + cause.Error() + "\n")
				os.Exit(4)
			}
			_, _ = os.Stderr.WriteString("lost() called with errSessionLeaseLost\n")
		case <-time.After(5 * time.Second):
			os.Exit(5) // lost() was never called -- the turn would hang forever
		}
		time.Sleep(time.Second)
		os.Exit(0) // only reached if the panic did not crash the process
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionLeaseRenewalPanicDoesNotCrashProcess$", "-test.v")
	cmd.Env = append(os.Environ(), sentinel+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child process output:\n%s", out)

	require.NoError(t, err,
		"a panic in the session-lease renewal goroutine must not crash the whole process serving "+
			"this turn; child process output:\n%s", out)
	require.Contains(t, string(out), "recovered panic")
	require.Contains(t, string(out), "ask-session-lease-renew")
	require.Contains(t, string(out), "lost() called with errSessionLeaseLost")
}
