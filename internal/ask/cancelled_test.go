package ask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// A turn whose caller goes away while a tool runs starts no further model or
// tool call, and records the outcome "cancelled" with no error: it is neither
// an answer nor a failure, of the turn or of the call it cut short.
func TestAskServiceCallerCancelStopsTheTurnAndRecordsCancelled(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeResponsesFixture(t, w, map[string]any{
			"id": "resp_tools", "object": "response", "created_at": 1,
			"status": "completed", "model": "test",
			"output": []any{map[string]any{
				"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "lookup", "arguments": `{}`, "status": "completed",
			}},
			"usage": responsesUsage(1, 1, 0),
		})
	}))
	defer server.Close()

	var toolCalls atomic.Int32
	inTool := make(chan struct{}, 1)
	lookup, err := utils.InferTool("lookup", "lookup", func(ctx context.Context, _ struct{}) (string, error) {
		toolCalls.Add(1)
		select {
		case inTool <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	require.NoError(t, err)

	metrics := NewMetrics(prometheus.NewRegistry())
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), []tool.BaseTool{lookup}, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{
		Manager: mgr, Agents: agents, Metrics: metrics, LinearTeams: testLinearTeams,
		Presentations: map[string]ToolPresentation{"lookup": {}},
	})
	require.NoError(t, err)

	request, callerGoesAway := context.WithCancel(context.Background())
	defer callerGoesAway()
	ctx, release := WithCallerCancel(request)
	defer release()
	go func() {
		<-inTool
		callerGoesAway()
	}()
	_, err = executeHTTPTestTurn(ctx, svc, sessionID, "use the tool")
	require.Error(t, err)
	require.True(t, TurnCancelled(ctx))
	require.Equal(t, int32(1), modelCalls.Load(), "no model call after the caller went away")
	require.Equal(t, int32(1), toolCalls.Load(), "no tool call after the caller went away")

	runs, err := mgr.InspectRunTelemetry(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "cancelled", runs[0].Outcome)
	require.Empty(t, runs[0].Error)

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.turns.WithLabelValues("http", "cancelled")))
	require.Zero(t, testutil.ToFloat64(metrics.turns.WithLabelValues("http", "error")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.toolCalls.WithLabelValues("http", "lookup", "cancelled")))
	require.Zero(t, testutil.ToFloat64(metrics.toolCalls.WithLabelValues("http", "lookup", "error")))
	require.Zero(t, testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "error")))
}

// A model call the caller's cancellation cut short is counted as cancelled,
// not as a model failure.
func TestAskServiceCallerCancelDuringModelCallIsNotAModelFailure(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	requested := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	metrics := NewMetrics(prometheus.NewRegistry())
	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"), nil, "fixed service prompt", 5)
	require.NoError(t, err)
	svc, err := NewAskService(AskServiceOptions{Manager: mgr, Agents: agents, Metrics: metrics, LinearTeams: testLinearTeams})
	require.NoError(t, err)

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		<-requested
		cancel(ErrTurnCancelled)
	}()
	_, err = executeHTTPTestTurn(ctx, svc, sessionID, "a long question")
	require.Error(t, err)

	runs, err := mgr.InspectRunTelemetry(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "cancelled", runs[0].Outcome)
	require.Empty(t, runs[0].Error)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "cancelled")))
	require.Zero(t, testutil.ToFloat64(metrics.modelRequests.WithLabelValues("http", "error")))
}

func TestWithCallerCancel(t *testing.T) {
	type key struct{}
	request, callerGoesAway := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))
	ctx, release := WithCallerCancel(request)
	defer release()
	require.Equal(t, "kept", ctx.Value(key{}), "the request's values reach the turn")
	require.NoError(t, ctx.Err())
	callerGoesAway()
	<-ctx.Done()
	child, done := context.WithTimeout(ctx, time.Minute)
	defer done()
	require.True(t, TurnCancelled(child), "the caller's cancellation reaches the turn's child contexts")

	plain, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, TurnCancelled(plain), "an ordinary cancellation is not the caller's")

	timedOut, cancelTimedOut := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelTimedOut()
	late, releaseLate := WithCallerCancel(timedOut)
	defer releaseLate()
	<-late.Done()
	require.False(t, TurnCancelled(late), "a request that ran out of time was not cancelled by its caller")
	require.ErrorIs(t, context.Cause(late), context.DeadlineExceeded)

	stopped, stop := context.WithCancelCause(context.Background())
	stop(ErrTurnStopped)
	passed, releasePassed := WithCallerCancel(stopped)
	defer releasePassed()
	<-passed.Done()
	require.True(t, TurnStopped(passed), "a cause the request already carries is kept")

	released, releaseNow := WithCallerCancel(context.Background())
	releaseNow()
	require.False(t, TurnCancelled(released), "releasing the context is not the caller's cancellation")
}
