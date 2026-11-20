package adminapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAskInvocationNotifier captures the args it was called with and
// signals done so tests can await the detached goroutine handleAsk
// fires it in, instead of sleeping.
type fakeAskInvocationNotifier struct {
	done chan struct{}

	ownerEmail string
	sessionID  string
	question   string
}

func newFakeAskInvocationNotifier() *fakeAskInvocationNotifier {
	return &fakeAskInvocationNotifier{done: make(chan struct{}, 1)}
}

func (f *fakeAskInvocationNotifier) NotifyAskInvocation(_ context.Context, ownerEmail, sessionID, question string) error {
	f.ownerEmail = ownerEmail
	f.sessionID = sessionID
	f.question = question
	f.done <- struct{}{}
	return nil
}

func (f *fakeAskInvocationNotifier) NotifyMCPInvocation(_ context.Context, _, _, _, _ string) error {
	return nil
}

// TestSetAskInvocationNotifierRoundTrip exercises the getter/setter
// pair in isolation (mirrors SetAskRunner/getAsk).
func TestSetAskInvocationNotifierRoundTrip(t *testing.T) {
	s := newTestServer("tok", nil)
	if got := s.getAskInvocationNotifier(); got != nil {
		t.Fatalf("expected nil notifier before Set, got %v", got)
	}
	n := newFakeAskInvocationNotifier()
	s.SetAskInvocationNotifier(n)
	if got := s.getAskInvocationNotifier(); got != n {
		t.Fatalf("getAskInvocationNotifier() = %v, want %v", got, n)
	}
	s.SetAskInvocationNotifier(nil)
	if got := s.getAskInvocationNotifier(); got != nil {
		t.Fatalf("expected nil notifier after clearing, got %v", got)
	}
}

// TestHandleAskFiresNotifier verifies accepted asks include the caller,
// session, and question in the detached notification.
func TestHandleAskFiresNotifier(t *testing.T) {
	pool := askSessionPool(t)
	s, _ := newTestServerAs(t, pool, "Alice@Example.com")
	n := newFakeAskInvocationNotifier()
	s.SetAskInvocationNotifier(n)

	rr := postAsk(t, s, `{"question": "hi"}`)
	if rr.Code != 200 {
		t.Fatalf("ask: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-n.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notifier to fire")
	}

	if n.ownerEmail != "alice@example.com" {
		t.Errorf("ownerEmail = %q, want lowercased caller email", n.ownerEmail)
	}
	if n.sessionID == "" {
		t.Error("sessionID = \"\", want the created session id")
	}
	if n.question != "hi" {
		t.Errorf("question = %q, want %q", n.question, "hi")
	}
}

func TestHandleAskFiresNotifierWhenTurnFails(t *testing.T) {
	pool := askSessionPool(t)
	s, runner := newTestServerAs(t, pool, "alice@example.com")
	runner.runErr = errors.New("turn failed")
	n := newFakeAskInvocationNotifier()
	s.SetAskInvocationNotifier(n)

	rr := postAsk(t, s, `{"question": "hard question"}`)
	if rr.Code != 500 {
		t.Fatalf("ask: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-n.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notifier to fire")
	}
	if n.question != "hard question" {
		t.Errorf("question = %q, want %q", n.question, "hard question")
	}
}

// TestHandleAskNoNotifierConfigured confirms handleAsk doesn't panic
// or block when no notifier is wired (the default/no-op path — most
// deployments won't have Slack notify users configured).
func TestHandleAskNoNotifierConfigured(t *testing.T) {
	pool := askSessionPool(t)
	s, _ := newTestServerAs(t, pool, "bob@example.com")

	rr := postAsk(t, s, `{"question": "hi"}`)
	if rr.Code != 200 {
		t.Fatalf("ask: status = %d; body=%s", rr.Code, rr.Body.String())
	}
}
