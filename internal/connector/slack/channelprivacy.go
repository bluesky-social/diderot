package slack

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var privacyChannelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

type channelInfoWriter interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func saveChannelInfo(ctx context.Context, writer channelInfoWriter, info *slack.Channel, observedAt time.Time) error {
	_, err := writer.Exec(ctx, `
		INSERT INTO slack_channel_info
			(channel_id, name, is_private, is_archived, is_ext_shared, is_pending_ext_shared, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (channel_id) DO UPDATE SET
			name=EXCLUDED.name, is_private=EXCLUDED.is_private, is_archived=EXCLUDED.is_archived,
			is_ext_shared=EXCLUDED.is_ext_shared, is_pending_ext_shared=EXCLUDED.is_pending_ext_shared,
			fetched_at=EXCLUDED.fetched_at
		WHERE slack_channel_info.fetched_at <= EXCLUDED.fetched_at`,
		info.ID, info.Name, info.IsPrivate, info.IsArchived, info.IsExtShared, info.IsPendingExtShared, observedAt)
	return err
}

func (d *Directory) refreshChannel(ctx context.Context, channelID string, keepPrivate bool) (*slack.Channel, error) {
	var observedAt time.Time
	if err := d.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedAt); err != nil {
		return nil, err
	}
	var info *slack.Channel
	if err := d.limiterCall(ctx, methodConvInfo, func() error {
		var err error
		info, err = d.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
		return err
	}); err != nil {
		return nil, err
	}
	if info == nil || info.ID != channelID || info.Name == "" {
		return nil, fmt.Errorf("channel %s refresh returned incomplete metadata", channelID)
	}
	snapshot := *info
	if keepPrivate {
		snapshot.IsPrivate = true
	}
	if err := saveChannelInfo(ctx, d.pool, &snapshot, observedAt); err != nil {
		return &snapshot, fmt.Errorf("store channel %s metadata: %w", channelID, err)
	}
	return &snapshot, nil
}

func (c *Consumer) handleChannelPrivacyChange(ctx context.Context, event *slackevents.MessageEvent) {
	if !privacyChannelID.MatchString(event.Channel) {
		c.logger.WarnContext(ctx, "slack: invalid privacy change channel", "channel", event.Channel)
		return
	}
	invalidate := func() {
		if c.channelAuthorizer != nil {
			c.channelAuthorizer.PublicScopes.Invalidate()
		}
	}
	if event.SubType == "channel_convert_to_private" {
		_, err := c.pool.Exec(ctx, `
			INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
			VALUES ($1, '', true, false, clock_timestamp())
			ON CONFLICT (channel_id) DO UPDATE SET is_private=true, fetched_at=clock_timestamp()`, event.Channel)
		if err != nil {
			c.logger.ErrorContext(ctx, "slack: privacy revocation failed", "channel", event.Channel, "err", err)
			return
		}
		invalidate()
	}
	if c.dir == nil {
		c.logger.WarnContext(ctx, "slack: privacy refresh unavailable", "channel", event.Channel)
		return
	}
	info, err := c.dir.refreshChannel(ctx, event.Channel, event.SubType == "channel_convert_to_private")
	if err != nil {
		c.logger.WarnContext(ctx, "slack: privacy refresh failed", "channel", event.Channel, "subtype", event.SubType, "err", err)
		return
	}
	invalidate()
	c.logger.InfoContext(ctx, "slack: channel privacy refreshed", "channel", event.Channel,
		"subtype", event.SubType, "is_private", info.IsPrivate)
}
