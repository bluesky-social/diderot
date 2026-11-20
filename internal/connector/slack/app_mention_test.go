package slack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wrapAppMention(t *testing.T, raw string) slackevents.EventsAPIEvent {
	t.Helper()
	var inner slackevents.AppMentionEvent
	require.NoError(t, json.Unmarshal([]byte(raw), &inner))
	return slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "app_mention", Data: &inner},
	}
}

func mentionConsumer(t *testing.T) (*Consumer, *fakeAskRouter) {
	t.Helper()
	c := newTestConsumer(t, getConsumerPool(t))
	c.selfUserID.Store("U_DIDEROT")
	c.seen = newSeenStore(0)
	c.mentions = newSeenStore(0)
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	return c, router
}

func createCalls(router *fakeAskRouter) int {
	router.mu.Lock()
	defer router.mu.Unlock()
	return len(router.createCalls)
}

const mentionText = "<@U_DIDEROT> what did we decide about the rollout?"

// Someone mentions the bot in a channel it is not in and lets Slack add it:
// the mention arrives only as an app_mention, and is ingested and answered.
func TestAppMentionAloneIsAnswered(t *testing.T) {
	c, router := mentionConsumer(t)
	c.handleEventsAPI(context.Background(), wrapAppMention(t, `{
		"type": "app_mention", "channel": "C_NEW", "user": "U_AVERY",
		"text": "`+mentionText+`", "ts": "1700000101.000100", "event_ts": "1700000101.000100"
	}`))
	waitForLen(t, func() int { return createCalls(router) }, 1, "the app_mention reaches Ask")

	var body string
	require.NoError(t, c.pool.QueryRow(context.Background(),
		`SELECT body FROM events WHERE source = 'slack' AND external_id = 'C_NEW:1700000101.000100'`).Scan(&body))
	assert.Contains(t, body, "what did we decide about the rollout?", "the mention is ingested as the message it is")
}

// A mention in a channel the bot is in arrives twice. Whichever delivery
// comes first is answered; the other is dropped without the redelivery
// error the message path logs for a true duplicate.
func TestMentionDeliveredTwiceIsAnsweredOnce(t *testing.T) {
	for _, appMentionFirst := range []bool{true, false} {
		c, router := mentionConsumer(t)
		ts := "1700000102.000100"
		if appMentionFirst {
			ts = "1700000103.000100"
		}
		message := wrapMessageEvent(t, `{"type": "message", "channel": "C_OPS", "user": "U_AVERY",
			"text": "`+mentionText+`", "ts": "`+ts+`"}`)
		mention := wrapAppMention(t, `{"type": "app_mention", "channel": "C_OPS", "user": "U_AVERY",
			"text": "`+mentionText+`", "ts": "`+ts+`", "event_ts": "`+ts+`"}`)
		first, second := message, mention
		if appMentionFirst {
			first, second = mention, message
		}
		c.handleEventsAPI(context.Background(), first)
		c.handleEventsAPI(context.Background(), second)
		waitForLen(t, func() int { return createCalls(router) }, 1, "the mention reaches Ask")
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 1, createCalls(router), "app_mention first: %t", appMentionFirst)
		assert.Zero(t, testutil.ToFloat64(c.metrics.redeliveries.WithLabelValues("no_retry")),
			"no redelivery error (app_mention first: %t)", appMentionFirst)
	}
}

func TestAppMentionMessageKeepsTheMessage(t *testing.T) {
	var ev slackevents.AppMentionEvent
	require.NoError(t, json.Unmarshal([]byte(`{
		"type": "app_mention", "channel": "C1", "user": "U1", "text": "<@U2> look",
		"ts": "1.000200", "thread_ts": "1.000100", "event_ts": "1.000200",
		"files": [{"id": "F1", "name": "graph.png", "mimetype": "image/png"}]
	}`), &ev))
	message, err := appMentionMessage(&ev)
	require.NoError(t, err)
	assert.Equal(t, "message", message.Type)
	assert.Equal(t, "C1", message.Channel)
	assert.Equal(t, "1.000100", message.ThreadTimeStamp)
	require.NotNil(t, message.Message)
	require.Len(t, message.Message.Files, 1)
	assert.Equal(t, "F1", message.Message.Files[0].ID)
}
