package slack

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
)

type fakeProgressTransport struct {
	mu       sync.Mutex
	calls    []sessionStatus
	statusFn func(context.Context, string) progressCallResult
}

func (f *fakeProgressTransport) SetStatus(ctx context.Context, status sessionStatus) progressCallResult {
	f.mu.Lock()
	f.calls = append(f.calls, status)
	fn := f.statusFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, status.Status)
	}
	return progressCallResult{}
}

func (f *fakeProgressTransport) statusValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	statuses := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		statuses = append(statuses, call.Status)
	}
	return statuses
}

func (f *fakeProgressTransport) statusCalls() []sessionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionStatus(nil), f.calls...)
}

func testProgress(transport progressTransport) *slackProgress {
	return newSlackProgress(transport, nil, slog.Default(), "C_TEST", "100.000", "what changed", "U_TEST", progressTimings{
		requestTimeout: 200 * time.Millisecond,
		joinGrace:      200 * time.Millisecond,
	})
}

// The turn sets the session processing once, named for the question and its
// asker, and back to active once it has answered: Slack keeps a session
// processing for an hour and does not clear it when the answer is posted.
func TestSlackProgressStatusStopJoinThenClear(t *testing.T) {
	transport := &fakeProgressTransport{}
	p := testProgress(transport)
	p.Start()
	waitForLen(t, func() int { return len(transport.statusValues()) }, 1, "status set")
	p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := p.Join(ctx); got != statusJoinConfirmed {
		t.Fatalf("Join = %d, want confirmed", got)
	}
	if err := p.Clear(ctx); err != nil {
		t.Fatalf("Clear = %v", err)
	}
	calls := transport.statusCalls()
	want := []sessionStatus{
		{ChannelID: "C_TEST", ThreadTS: "100.000", Status: slackgo.AgentSessionStatusProcessing, Title: "what changed", Initiator: "U_TEST"},
		{ChannelID: "C_TEST", ThreadTS: "100.000", Status: slackgo.AgentSessionStatusActive},
	}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("status calls = %#v, want %#v", calls, want)
	}
}

func TestSlackProgressStatusJoinBeforeStartReturnsImmediately(t *testing.T) {
	transport := &fakeProgressTransport{}
	p := testProgress(transport)
	p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if got := p.Join(ctx); got != statusJoinConfirmed {
		t.Fatalf("Join = %d, want confirmed", got)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("Join without Start blocked for %s", elapsed)
	}
	if err := p.Clear(ctx); err != nil {
		t.Fatalf("Clear without Start = %v", err)
	}
	if got := len(transport.statusValues()); got != 0 {
		t.Fatalf("status calls = %d, want 0", got)
	}
	p.Start()
	time.Sleep(20 * time.Millisecond)
	if got := len(transport.statusValues()); got != 0 {
		t.Fatalf("late Start made %d calls after finalized join", got)
	}
}

func TestSlackProgressAmbiguousStatusJoinSkipsClear(t *testing.T) {
	transport := &fakeProgressTransport{
		statusFn: func(context.Context, string) progressCallResult {
			return progressCallResult{err: context.DeadlineExceeded, ambiguous: true}
		},
	}
	p := testProgress(transport)
	p.Start()
	waitForLen(t, func() int { return len(transport.statusValues()) }, 1, "ambiguous status")
	p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := p.Join(ctx); got != statusJoinAmbiguous {
		t.Fatalf("Join = %d, want ambiguous", got)
	}
	if err := p.Clear(ctx); err == nil {
		t.Fatal("Clear = nil after ambiguous join")
	}
	for _, status := range transport.statusValues() {
		if status == slackgo.AgentSessionStatusActive {
			t.Fatal("explicit clear was issued after ambiguous join")
		}
	}
}

func TestSlackProgressStatusStartupIsAsyncAndCancellationExits(t *testing.T) {
	transport := &fakeProgressTransport{}
	started := make(chan struct{})
	transport.statusFn = func(ctx context.Context, _ string) progressCallResult {
		close(started)
		<-ctx.Done()
		return progressCallResult{err: ctx.Err(), ambiguous: true}
	}
	p := testProgress(transport)
	begin := time.Now()
	p.Start()
	if elapsed := time.Since(begin); elapsed > 20*time.Millisecond {
		t.Fatalf("Start blocked for %s", elapsed)
	}
	<-started
	p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := p.Join(ctx); got != statusJoinTimeout {
		t.Fatalf("Join = %d, want timeout", got)
	}
	select {
	case <-p.statusDone:
	case <-time.After(time.Second):
		t.Fatal("status call did not exit after cancellation")
	}
}

type capturingProgressAPI struct {
	statuses []slackgo.AgentSessionSetStatusParameters
}

func (c *capturingProgressAPI) SetAgentSessionStatusContext(_ context.Context, params slackgo.AgentSessionSetStatusParameters) (*slackgo.AgentSessionSetStatusResponse, error) {
	c.statuses = append(c.statuses, params)
	return &slackgo.AgentSessionSetStatusResponse{}, nil
}

func TestSlackProgressAdapterCallsAgentSessions(t *testing.T) {
	api := &capturingProgressAPI{}
	transport := &slackProgressTransport{api: api}

	start := sessionStatus{ChannelID: "C_TEST", ThreadTS: "100.000", Status: slackgo.AgentSessionStatusProcessing, Title: "what changed", Initiator: "U_TEST"}
	if result := transport.SetStatus(context.Background(), start); result.err != nil {
		t.Fatalf("set status: %v", result.err)
	}
	if result := transport.SetStatus(context.Background(), sessionStatus{ChannelID: "C_TEST", ThreadTS: "100.000", Status: slackgo.AgentSessionStatusActive}); result.err != nil {
		t.Fatalf("clear status: %v", result.err)
	}
	want := []slackgo.AgentSessionSetStatusParameters{
		{ChannelID: "C_TEST", ThreadTS: "100.000", Status: "processing", Title: "what changed", InitiatorUserID: "U_TEST"},
		{ChannelID: "C_TEST", ThreadTS: "100.000", Status: "active"},
	}
	if len(api.statuses) != 2 || api.statuses[0] != want[0] || api.statuses[1] != want[1] {
		t.Fatalf("status calls = %#v, want %#v", api.statuses, want)
	}
}

type blockingProgressAPI struct {
	started chan struct{}
}

func (b *blockingProgressAPI) SetAgentSessionStatusContext(ctx context.Context, _ slackgo.AgentSessionSetStatusParameters) (*slackgo.AgentSessionSetStatusResponse, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSlackProgressAdapterBlockedHTTPCallReturnsOnContextCancellation(t *testing.T) {
	api := &blockingProgressAPI{started: make(chan struct{})}
	transport := &slackProgressTransport{api: api}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan progressCallResult, 1)
	go func() {
		done <- transport.SetStatus(ctx, sessionStatus{ChannelID: "C_TEST", ThreadTS: "100.000", Status: slackgo.AgentSessionStatusProcessing})
	}()
	<-api.started
	cancel()
	select {
	case result := <-done:
		if !result.ambiguous || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not return after context cancellation")
	}
}

func TestSessionTitle(t *testing.T) {
	cases := []struct {
		question string
		mention  bool
		want     string
	}{
		{"@diderot   what changed\nin the deploy?", true, "what changed in the deploy?"},
		{"@alice said the deploy broke", false, "@alice said the deploy broke"},
		{"@diderot", true, "@diderot"},
		{strings.Repeat("word ", 40), false, strings.TrimSpace(strings.Repeat("word ", 16)) + "…"},
	}
	for _, tc := range cases {
		got := sessionTitle(tc.question, tc.mention)
		if got != tc.want {
			t.Errorf("sessionTitle(%q, %t) = %q, want %q", tc.question, tc.mention, got, tc.want)
		}
		if n := len([]rune(got)); n > sessionTitleRunes {
			t.Errorf("title of %q is %d runes", tc.question, n)
		}
	}
}
