package slack

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/slack-go/slack/slackevents"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
)

// viewedChannel is the channel the sender of a direct message has open in
// Slack (the message's app_context, which Slack sends once the app
// subscribes to app_context_changed), when the turn may name it. A direct
// message's turn reads with the asker's own scopes (scopesForDM): the public
// channels, and the private channels the asker shares with the bot, as Slack
// says now. The channel is named only when:
//
//   - its scope is in that set, so the asker can already read it through
//     Diderot; and
//   - Diderot ingests it: its sync row is enabled and it is not blocked.
//
// The turn's scopes are not changed: naming the channel lets the model take
// "here" and "this channel" to mean it, and widens nothing it can read. A
// channel that fails either test, a canvas or list, or no context at all,
// names nothing.
func (c *Consumer) viewedChannel(ctx context.Context, viewing *slackevents.AppContext, allowedScopes []string) *ask.ViewedChannel {
	channelID := viewedChannelID(viewing)
	if channelID == "" || !slices.Contains(allowedScopes, events.ScopeChannel(channelID)) {
		return nil
	}
	if c.blacklist != nil {
		if _, blocked := c.blacklist.Match(channelID); blocked {
			return nil
		}
	}
	if c.pool == nil {
		return nil
	}
	var ingested bool
	err := c.pool.QueryRow(ctx,
		`SELECT ingest_enabled FROM slack_channel_sync WHERE channel_id = $1`, channelID).Scan(&ingested)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		c.logger.Warn("ask DM: viewed channel not checked; leaving it out", "channel", channelID, "err", err)
		return nil
	case !ingested:
		return nil
	}
	viewed := &ask.ViewedChannel{ID: channelID}
	if c.dir != nil {
		if name, err := c.dir.LookupChannel(ctx, channelID); err == nil {
			viewed.Name = name
		}
	}
	return viewed
}

// viewedChannelID is the most relevant channel in an app_context: a
// channel the sender has open, or the channel of a message or thread they
// have open. Canvases and lists name no channel.
func viewedChannelID(viewing *slackevents.AppContext) string {
	if viewing == nil {
		return ""
	}
	for _, entity := range viewing.Entities {
		switch {
		case entity.Type == slackevents.AppContextEntityChannel && entity.Value != "":
			return entity.Value
		case entity.Type == slackevents.AppContextEntityMessageContext && entity.Message != nil && entity.Message.ChannelID != "":
			return entity.Message.ChannelID
		}
	}
	return ""
}
