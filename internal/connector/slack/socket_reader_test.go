package slack

import (
	"testing"
	"time"

	"github.com/slack-go/slack/socketmode"
)

func TestNextSocketEventStopsWhenEventsChannelRemainsOpen(t *testing.T) {
	events := make(chan socketmode.Event)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		if _, ok := nextSocketEvent(events, stop); ok {
			t.Error("event reader reported an event after stop")
		}
	}()

	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event reader waited for slack-go to close Events")
	}
}
