package slack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
)

func viewing(t *testing.T, entities string) *slackevents.AppContext {
	t.Helper()
	var context slackevents.AppContext
	require.NoError(t, json.Unmarshal([]byte(`{"entities":`+entities+`}`), &context))
	return &context
}

// A direct message names the channel its sender has open only when the
// asker's own scopes already reach it and Diderot ingests it. The turn's
// scopes never change.
func TestViewedChannelNamesOnlyAReadableIngestedChannel(t *testing.T) {
	pool := getConsumerPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `TRUNCATE slack_channel_sync, slack_ingest_blacklist RESTART IDENTITY`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_sync (channel_id, ingest_enabled) VALUES
		('C_PUBLIC', true), ('C_PRIVATE', true), ('C_PAUSED', false), ('C_OTHERS', true), ('C_BLOCKED', true)`)
	require.NoError(t, err)
	c := newTestConsumer(t, pool)
	c.blacklist = NewIngestBlacklist(pool, nil)
	_, err = c.blacklist.Add(ctx, "C_BLOCKED", "blocked", "test", "operator")
	require.NoError(t, err)

	// The asker reads public channels and the private channels they share
	// with the bot; C_OTHERS is a private channel they are not in.
	scopes := []string{events.ScopePublic, events.ScopeInternal,
		events.ScopeChannel("C_PUBLIC"), events.ScopeChannel("C_PRIVATE"), events.ScopeChannel("C_PAUSED"),
		events.ScopeChannel("C_BLOCKED"), events.ScopeChannel("C_UNSYNCED"), events.ScopeOwner("asker@example.com")}
	channel := func(id string) *slackevents.AppContext {
		return viewing(t, `[{"type":"slack#/types/channel_id","value":"`+id+`"}]`)
	}

	assert.Equal(t, &ask.ViewedChannel{ID: "C_PUBLIC"}, c.viewedChannel(ctx, channel("C_PUBLIC"), scopes))
	assert.Equal(t, &ask.ViewedChannel{ID: "C_PRIVATE"}, c.viewedChannel(ctx, channel("C_PRIVATE"), scopes),
		"a private channel the asker shares with the bot")
	assert.Nil(t, c.viewedChannel(ctx, channel("C_OTHERS"), scopes), "a channel the asker cannot read")
	assert.Nil(t, c.viewedChannel(ctx, channel("C_PAUSED"), scopes), "a channel Diderot does not ingest")
	assert.Nil(t, c.viewedChannel(ctx, channel("C_UNSYNCED"), scopes), "a channel Diderot has never synced")
	assert.Nil(t, c.viewedChannel(ctx, channel("C_BLOCKED"), scopes), "a blocked channel")
	assert.Nil(t, c.viewedChannel(ctx, channel("D_SOMEONE"), scopes), "another direct message")
	assert.Nil(t, c.viewedChannel(ctx, nil, scopes))

	thread := viewing(t, `[{"type":"slack#/types/canvas_id","value":"F1"},
		{"type":"slack#/types/message_context","value":{"channel_id":"C_PRIVATE","message_ts":"1.000100"}}]`)
	assert.Equal(t, &ask.ViewedChannel{ID: "C_PRIVATE"}, c.viewedChannel(ctx, thread, scopes),
		"an open thread names its channel; a canvas names none")
	assert.Nil(t, c.viewedChannel(ctx, viewing(t, `[{"type":"slack#/types/list_id","value":"F2"}]`), scopes))
}

// The app_context Slack adds to a direct message informs that turn only: it
// is not stored with the message.
func TestDirectMessageIsStoredWithoutItsAppContext(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, `{
		"type": "message", "channel": "D_ASKER", "channel_type": "im", "user": "U_ASKER",
		"text": "what changed here?", "ts": "1700000201.000100",
		"app_context": {"entities": [{"type": "slack#/types/channel_id", "value": "C_PRIVATE"}]}
	}`))
	var raw string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT raw::text FROM events WHERE source = 'slack' AND external_id = 'D_ASKER:1700000201.000100'`).Scan(&raw))
	assert.NotContains(t, raw, "app_context")
	assert.NotContains(t, raw, "C_PRIVATE")
}
