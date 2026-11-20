package adminapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/diderot/internal/adminaction"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
)

var (
	// ErrSlackOff is a Slack action asked for while the Slack connector is
	// off or not set up. Operators read both as written.
	ErrSlackOff = errors.New("Slack is off") //nolint:staticcheck // starts with a name
	// ErrNoSlackUserToken is an invite or removal of the bot asked for while
	// Slack has no user token to act as.
	ErrNoSlackUserToken = errors.New("Slack has no user token; set one at /admin/connectors/slack") //nolint:staticcheck // starts with a name
)

// SlackOn reports whether the Slack connector is on, which its channel
// controls need.
func (s *Server) SlackOn() bool {
	_, on := s.slackControls()
	return on
}

// SlackUserTokenSet reports whether Slack has the user token that inviting
// and removing the bot act as.
func (s *Server) SlackUserTokenSet() bool { return s.getChannelInviter() != nil }

// SetSlackChannelConfig turns one of a channel's switches on or off.
func (s *Server) SetSlackChannelConfig(ctx context.Context, channelID, field string, enabled bool, updatedBy string) error {
	controls, on := s.slackControls()
	if !on {
		return ErrSlackOff
	}
	store := controls.ChannelConfigs
	switch field {
	case adminaction.SlackConfigAsk:
		return store.SetAskEnabled(ctx, channelID, enabled, updatedBy)
	case adminaction.SlackConfigDocuments:
		return store.SetDocIngestEnabled(ctx, channelID, enabled, updatedBy)
	case adminaction.SlackConfigExternal:
		return store.SetAllowExternalAsk(ctx, channelID, enabled, updatedBy)
	}
	return fmt.Errorf("%w %q", adminaction.ErrUnknownSlackConfig, field)
}

// AddSlackBlacklist blacklists a channel, under its name from the Slack
// directory when channelName is empty.
func (s *Server) AddSlackBlacklist(ctx context.Context, channelID, channelName, reason, addedBy string) (slackcx.BlacklistEntry, error) {
	controls, on := s.slackControls()
	if !on {
		return slackcx.BlacklistEntry{}, ErrSlackOff
	}
	store := controls.Blacklist
	if pool := s.getStatusPool(); channelName == "" && pool != nil {
		_ = pool.QueryRow(ctx, `SELECT COALESCE(name, '') FROM slack_channel_info WHERE channel_id=$1`, channelID).Scan(&channelName)
	}
	return store.Add(ctx, channelID, channelName, strings.TrimSpace(reason), addedBy)
}

// RemoveSlackBlacklist reports whether the channel had a blacklist entry.
func (s *Server) RemoveSlackBlacklist(ctx context.Context, channelID string) (bool, error) {
	controls, on := s.slackControls()
	if !on {
		return false, ErrSlackOff
	}
	return controls.Blacklist.RemoveByID(ctx, channelID)
}

// SetSlackMembership invites the bot to a channel or removes it, and reports
// whether it already was, or wasn't, a member.
func (s *Server) SetSlackMembership(ctx context.Context, channelID string, joined bool) (bool, error) {
	controls, on := s.slackControls()
	if !on {
		return false, ErrSlackOff
	}
	inviter := controls.Inviter
	if inviter == nil {
		return false, ErrNoSlackUserToken
	}
	if joined {
		err := inviter.Invite(ctx, channelID)
		if isAlreadyInChannelErr(err) {
			return true, nil
		}
		return false, err
	}
	err := inviter.Kick(ctx, channelID)
	if isNotInChannelErr(err) {
		return true, nil
	}
	return false, err
}

// RequestSlackBackfill resets a channel's history walk once the bot's
// membership is confirmed, and reports whether the channel already had one.
// Without a Slack user token the membership can't be checked, and the walk
// is reset anyway.
func (s *Server) RequestSlackBackfill(ctx context.Context, channelID string) (bool, error) {
	controls, on := s.slackControls()
	if !on {
		return false, ErrSlackOff
	}
	if inviter := controls.Inviter; inviter != nil {
		ok, err := inviter.IsBotInChannel(ctx, channelID)
		switch {
		case errors.Is(err, slackcx.ErrChannelNotFound):
			return false, err
		case err != nil:
			s.logger.Warn("backfill: membership check failed", "channel", channelID, "err", err)
			return false, fmt.Errorf("membership check: %w", err)
		case !ok:
			return false, adminaction.ErrBotNotInChannel
		}
	} else {
		s.logger.Info("backfill: no inviter configured; skipping membership check", "channel", channelID)
	}
	wasReset, err := controls.SyncWorker.RequestBackfill(ctx, channelID)
	if err != nil {
		return false, fmt.Errorf("enqueue: %w", err)
	}
	return wasReset, nil
}

func (s *Server) RequestSlackDocumentBackfill(ctx context.Context, channelID, updatedBy string) error {
	controls, on := s.slackControls()
	if !on {
		return ErrSlackOff
	}
	store := controls.ChannelConfigs
	enabled, err := store.DocIngestEnabled(ctx, channelID)
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("document ingest is disabled for this channel; enable it before requesting a replay")
	}
	// Re-enabling an already-enabled channel deliberately resets the durable
	// cursor to zero; the leader's worker drains it in bounded restart-safe batches.
	return store.SetDocIngestEnabled(ctx, channelID, true, updatedBy)
}

// ResolveSlackChannel turns a channel's name, with or without its #, or its
// ID into the channel's ID and name, as Slack has them. It needs the Slack
// user token.
func (s *Server) ResolveSlackChannel(ctx context.Context, nameOrID string) (channelID, name string, err error) {
	controls, on := s.slackControls()
	if !on {
		return "", "", ErrSlackOff
	}
	if controls.Inviter == nil {
		return "", "", ErrNoSlackUserToken
	}
	return controls.Inviter.ResolveChannel(ctx, strings.TrimSpace(nameOrID))
}

// isAlreadyInChannelErr reports whether an invite failed because the bot is
// already a member. slack-go returns the API's error string, with no typed
// field for it.
func isAlreadyInChannelErr(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already_in_channel") || message == "user_is_in_channel"
}

// isNotInChannelErr reports whether a removal failed because the bot was not
// a member.
func isNotInChannelErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not_in_channel")
}
