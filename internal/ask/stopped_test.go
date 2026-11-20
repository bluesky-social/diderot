package ask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A turn its asker stops records the outcome "stopped" with no error: it is
// neither an answer nor a failure.
func TestAskServiceStoppedTurnIsRecordedAsStopped(t *testing.T) {
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

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	ctx, stop := context.WithCancelCause(context.Background())
	go func() {
		<-requested
		stop(ErrTurnStopped)
	}()
	_, err := executeHTTPTestTurn(ctx, svc, sessionID, "a long question")
	require.Error(t, err)
	require.True(t, TurnStopped(ctx))

	runs, err := mgr.InspectRunTelemetry(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "stopped", runs[0].Outcome)
	require.Empty(t, runs[0].Error)
}

func TestTurnStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, TurnStopped(ctx), "an ordinary cancellation is not a stop")
	stopped, stop := context.WithCancelCause(context.Background())
	stop(ErrTurnStopped)
	child, done := context.WithTimeout(stopped, time.Minute)
	defer done()
	require.True(t, TurnStopped(child), "a stop reaches the turn's child contexts")
}
