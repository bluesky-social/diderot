package slack

import (
	"context"
	"encoding/json"

	"github.com/slack-go/slack/slackevents"
)

// handleAppMention handles Slack's app_mention event. Its use is the one
// mention no message event carries: someone mentions the bot in a channel it
// is not in and lets Slack add it, and the mention arrives only as an
// app_mention. Any other channel mention arrives as a message event too, so:
//
//   - a mention whose message event this process has already handled
//     (seen) is left to it;
//   - otherwise the mention goes through the message path as the message it
//     is, where ingest is idempotent and the mentions record routes it to
//     Ask once, whichever delivery reaches it first.
//
// Mentions in direct messages and edits never arrive as app_mention.
func (c *Consumer) handleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	if ev.Channel == "" || ev.TimeStamp == "" || ev.Edited != nil {
		return
	}
	if c.blacklist != nil {
		if _, ok := c.blacklist.Match(ev.Channel); ok {
			c.metrics.events.WithLabelValues("blacklisted").Inc()
			return
		}
	}
	if c.seen.Seen(ev.Channel, ev.TimeStamp) {
		return
	}
	message, err := appMentionMessage(ev)
	if err != nil {
		c.logger.Warn("slack: app_mention not readable as a message", "channel", ev.Channel, "err", err)
		return
	}
	// The bot is in the channel now: Slack added it for this mention, or it
	// was already there. Apply the join policy the reconcile loop would
	// apply when it finds the channel, so the mention is not refused as
	// not enabled in the minutes before it does. It never overrides a
	// setting someone chose.
	if c.channelConfig != nil {
		if _, err := c.channelConfig.DefaultAskEnabled(ctx, ev.Channel, "auto (joined channel)"); err != nil {
			c.logger.Warn("slack: app_mention: default ask-enable failed", "channel", ev.Channel, "err", err)
		}
	}
	c.processMessageEventAsync(ctx, message)
}

// appMentionMessage is the message event an app_mention stands for: the
// same message, with the fields the message path reads.
func appMentionMessage(ev *slackevents.AppMentionEvent) (*slackevents.MessageEvent, error) {
	raw, err := json.Marshal(struct {
		Type            string `json:"type"`
		Channel         string `json:"channel"`
		User            string `json:"user,omitempty"`
		BotID           string `json:"bot_id,omitempty"`
		Text            string `json:"text"`
		TimeStamp       string `json:"ts"`
		ThreadTimeStamp string `json:"thread_ts,omitempty"`
		EventTimeStamp  string `json:"event_ts,omitempty"`
		UserTeam        string `json:"user_team,omitempty"`
		SourceTeam      string `json:"source_team,omitempty"`
		Blocks          any    `json:"blocks,omitempty"`
		Attachments     any    `json:"attachments,omitempty"`
		Files           any    `json:"files,omitempty"`
		Upload          bool   `json:"upload,omitempty"`
	}{
		Type: "message", Channel: ev.Channel, User: ev.User, BotID: ev.BotID, Text: ev.Text,
		TimeStamp: ev.TimeStamp, ThreadTimeStamp: ev.ThreadTimeStamp, EventTimeStamp: ev.EventTimeStamp,
		UserTeam: ev.UserTeam, SourceTeam: ev.SourceTeam,
		Blocks: nonEmpty(len(ev.Blocks.BlockSet) > 0, ev.Blocks), Attachments: nonEmpty(len(ev.Attachments) > 0, ev.Attachments),
		Files: nonEmpty(len(ev.Files) > 0, ev.Files), Upload: ev.Upload,
	})
	if err != nil {
		return nil, err
	}
	var message slackevents.MessageEvent
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

// nonEmpty is v when present, and nil (left out of the JSON) otherwise.
func nonEmpty(present bool, v any) any {
	if !present {
		return nil
	}
	return v
}
