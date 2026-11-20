package askchat

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
)

// memWorking is the working records in memory: just enough for a stop
// request to reach a renewing turn.
type memWorking struct {
	mu   sync.Mutex
	stop map[Conversation]bool
}

func (m *memWorking) Begin(context.Context, string, string, uuid.UUID) error { return nil }

func (m *memWorking) Renew(_ context.Context, channelID, threadTS string, _ uuid.UUID) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop[Conversation{channelID, threadTS}], true, nil
}

func (m *memWorking) Finish(context.Context, string, string, uuid.UUID, bool, bool) error { return nil }

func (m *memWorking) RequestStop(_ context.Context, channelID, threadTS string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		m.stop = map[Conversation]bool{}
	}
	m.stop[Conversation{channelID, threadTS}] = true
	return nil
}

func (m *memWorking) Expired(context.Context) ([]WorkingThread, error) { return nil, nil }

func (m *memWorking) Claim(context.Context, WorkingThread) (bool, bool, error) {
	return false, false, nil
}

func (m *memWorking) Release(context.Context, WorkingThread) error { return nil }

// A stop pressed while the turn runs on another replica (one still finishing
// after leadership moved) reaches it through the record the turn renews.
func TestStopRequestReachesATurnThroughItsRecord(t *testing.T) {
	previous := WorkingRenew
	WorkingRenew = 10 * time.Millisecond
	t.Cleanup(func() { WorkingRenew = previous })

	store := &memWorking{}
	turnCtx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	working, err := beginWorking(context.Background(), store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"C_TEST", "100.000", uuid.New(), stop)
	require.NoError(t, err)
	require.NoError(t, store.RequestStop(context.Background(), "C_TEST", "100.000"))
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("the turn did not see the stop request")
	}
	assert.True(t, ask.TurnStopped(turnCtx))
	working.finish(context.Background(), true, true)
}
