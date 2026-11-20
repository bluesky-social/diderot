package discord

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gatewayHarness struct {
	f      *fakeDiscord
	events chan dispatch
	done   chan error
	cancel context.CancelFunc

	mu       sync.Mutex
	statuses []gatewayStatus
}

func startGateway(t *testing.T, f *fakeDiscord, token string) *gatewayHarness {
	t.Helper()
	h := &gatewayHarness{f: f, events: make(chan dispatch, 64), done: make(chan error, 1)}
	client := NewClient(f.apiBase(), "test-token")
	g := &gateway{
		token: token, intents: gatewayIntents, url: client.gatewayURL, logger: slog.New(slog.DiscardHandler),
		events: h.events,
		status: func(s gatewayStatus) { h.mu.Lock(); h.statuses = append(h.statuses, s); h.mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	go func() { h.done <- g.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	return h
}

func (h *gatewayHarness) next(t *testing.T) dispatch {
	t.Helper()
	select {
	case d := <-h.events:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no dispatch")
		return dispatch{}
	}
}

func TestGatewayIdentifiesDispatchesAndResumes(t *testing.T) {
	f := newFakeDiscord(t)
	h := startGateway(t, f, "test-token")
	conn := f.nextConn(t)
	assert.Equal(t, "READY", h.next(t).Type)
	conn.send("MESSAGE_CREATE", map[string]any{"id": "1"})
	conn.send("MESSAGE_CREATE", map[string]any{"id": "2"})
	assert.Equal(t, "MESSAGE_CREATE", h.next(t).Type)
	assert.Equal(t, "MESSAGE_CREATE", h.next(t).Type)

	// Heartbeats are sent and acknowledged; the connection stays up past
	// several intervals.
	time.Sleep(700 * time.Millisecond)

	// A drop with a resumable code resumes the session from the last
	// sequence: no new identify.
	conn.close(4000)
	conn = f.nextConn(t)
	assert.Equal(t, "RESUMED", h.next(t).Type)
	identifies, resumes := f.stats()
	assert.Equal(t, 1, identifies)
	assert.Equal(t, []int64{3}, resumes)

	// Discord asking for a reconnect resumes too.
	conn.op(opReconnect, nil)
	f.nextConn(t)
	assert.Equal(t, "RESUMED", h.next(t).Type)
	identifies, resumes = f.stats()
	assert.Equal(t, 1, identifies)
	assert.Len(t, resumes, 2)
}

// A dispatch sent while the connection is down arrives when the session
// resumes: the client resumes from its last sequence, and Discord replays.
func TestGatewayResumeReplaysWhatWasMissed(t *testing.T) {
	f := newFakeDiscord(t)
	h := startGateway(t, f, "test-token")
	conn := f.nextConn(t)
	assert.Equal(t, "READY", h.next(t).Type)
	// The event is recorded but written to no connection, before the
	// client can resume: only the resume's replay can deliver it.
	f.handoff.Lock()
	conn.close(4000)
	f.record("MESSAGE_CREATE", map[string]any{"id": "sent while down"})
	f.handoff.Unlock()
	f.nextConn(t)
	d := h.next(t)
	require.Equal(t, "MESSAGE_CREATE", d.Type)
	assert.Contains(t, string(d.Data), "sent while down")
	assert.Equal(t, "RESUMED", h.next(t).Type)
}

func TestGatewayInvalidSessionIdentifiesAgain(t *testing.T) {
	f := newFakeDiscord(t)
	h := startGateway(t, f, "test-token")
	conn := f.nextConn(t)
	assert.Equal(t, "READY", h.next(t).Type)
	conn.op(opInvalidSession, false)
	f.nextConn(t)
	assert.Equal(t, "READY", h.next(t).Type)
	identifies, resumes := f.stats()
	assert.Equal(t, 2, identifies)
	assert.Empty(t, resumes)
}

func TestGatewayFatalCloseStops(t *testing.T) {
	f := newFakeDiscord(t)
	h := startGateway(t, f, "wrong-token")
	select {
	case err := <-h.done:
		var fatal *FatalGatewayError
		require.True(t, errors.As(err, &fatal), "%v", err)
		assert.Equal(t, 4004, fatal.Code)
		h.done <- nil // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("a refused token did not stop the gateway")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.statuses)
	assert.Error(t, h.statuses[len(h.statuses)-1].Err)
}

func TestGatewayTarget(t *testing.T) {
	target, err := gatewayTarget("wss://gateway.discord.gg")
	require.NoError(t, err)
	assert.Equal(t, "wss://gateway.discord.gg/?encoding=json&v=10", target)
	_, err = gatewayTarget("https://example.com")
	assert.Error(t, err)
}
