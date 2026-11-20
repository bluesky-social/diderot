package slack

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumerRunStateGracefulDrainWaitsForHandlerAndAsk(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	handlerRelease := make(chan struct{})
	askRelease := make(chan struct{})
	askStarted := make(chan struct{})

	if !state.startHandler(func(ctx context.Context) {
		<-handlerRelease
		if !state.startAsk(func(ctx context.Context) {
			close(askStarted)
			<-askRelease
		}) {
			t.Error("ask rejected while an accepted handler was still routing")
		}
	}) {
		t.Fatal("initial handler rejected")
	}

	drained := make(chan bool, 1)
	go func() { drained <- state.drain(time.Second) }()

	select {
	case got := <-drained:
		t.Fatalf("drain returned before handler completed: %v", got)
	case <-time.After(20 * time.Millisecond):
	}

	close(handlerRelease)
	select {
	case <-askStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted handler did not start ask")
	}

	select {
	case got := <-drained:
		t.Fatalf("drain returned before ask completed: %v", got)
	case <-time.After(20 * time.Millisecond):
	}

	close(askRelease)
	if got := <-drained; !got {
		t.Fatal("drain reported timeout after work completed")
	}
	if state.startHandler(func(context.Context) {}) {
		t.Fatal("handler accepted after drain")
	}
	if state.startAsk(func(context.Context) {}) {
		t.Fatal("ask accepted after drain")
	}
}

func TestConsumerRunStateLeadershipLossCancelsAcceptedWork(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	var cancelled atomic.Bool
	started := make(chan struct{})
	if !state.startAsk(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
	}) {
		t.Fatal("initial ask rejected")
	}
	<-started

	done := make(chan struct{})
	go func() {
		state.cancelAndWait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelAndWait did not return")
	}
	if !cancelled.Load() {
		t.Fatal("accepted ask did not observe cancellation")
	}
}

func TestConsumerRunStateRejectedHandlerIsNotStarted(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	state.stopHandlers()
	start, accepted := state.acceptHandler(func(context.Context) {
		t.Error("rejected handler ran")
	})
	if accepted || start != nil {
		t.Fatal("handler reserved after admission closed")
	}
	state.cancelAndWait()
}

func TestConsumerRunStateDrainTimeoutCancelsWork(t *testing.T) {
	state := newConsumerRunState(context.Background(), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	if !state.startAsk(func(context.Context) {
		close(started)
		<-release
	}) {
		t.Fatal("initial ask rejected")
	}
	<-started

	before := time.Now()
	if state.drain(10 * time.Millisecond) {
		t.Fatal("drain unexpectedly completed inside timeout")
	}
	if elapsed := time.Since(before); elapsed > 250*time.Millisecond {
		t.Fatalf("drain waited for stuck work after timeout: %v", elapsed)
	}
	close(release)
	state.asks.Wait()
}

func TestConsumerRunTrackerRejectsAskAfterRunUninstalled(t *testing.T) {
	var tracker consumerRunTracker
	state := newConsumerRunState(context.Background(), nil)
	tracker.install(state)
	tracker.uninstall(state)

	if tracker.startAsk(context.Background(), func(context.Context) {
		t.Error("ask started after run was uninstalled")
	}) {
		t.Fatal("ask accepted after run was uninstalled")
	}
}
