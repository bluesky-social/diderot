package slack

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
)

// A Slack turn runs on its socket run's context, not on the context of the
// event that asked: the event's handler ending, or its context being
// cancelled, leaves the turn running. Only the run ending (the process
// stopping, leadership lost) or the stop button ends it.
func TestAskTurnOutlivesTheEventThatAskedIt(t *testing.T) {
	c, _ := mentionConsumer(t)
	router := newBlockingRouter()
	c.ask = router
	run := newConsumerRunState(context.Background(), nil)
	c.run.install(run)
	defer c.run.uninstall(run)

	event, endEvent := context.WithCancel(context.Background())
	c.handleEventsAPI(event, wrapAppMention(t, `{
		"type": "app_mention", "channel": "C_NEW", "user": "U_AVERY",
		"text": "`+mentionText+`", "ts": "1700000201.000100", "event_ts": "1700000201.000100"
	}`))
	select {
	case <-router.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the mention did not reach Ask")
	}
	turn := <-router.turnCtx

	endEvent()
	select {
	case <-turn.Done():
		t.Fatal("cancelling the event's context ended the turn")
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, turn.Err())

	ended := make(chan struct{})
	go func() {
		run.cancelAndWait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the run's end did not end the turn")
	}
	assert.ErrorIs(t, turn.Err(), context.Canceled)
	assert.False(t, ask.TurnStopped(turn), "the run ending is not a stop")
	assert.False(t, ask.TurnCancelled(turn), "nor a caller's cancellation")
}
