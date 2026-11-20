package adminaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
)

// The per-channel switches SetSlackChannelConfig turns on and off: Ask,
// document ingest, and Ask in a channel with external members.
const (
	SlackConfigAsk       = "ask"
	SlackConfigDocuments = "documents"
	SlackConfigExternal  = "external"
)

var (
	// ErrUnknownSlackConfig names a switch SetSlackChannelConfig does not have.
	ErrUnknownSlackConfig = errors.New("unknown channel configuration field")
	// ErrNotBlacklisted means there was no blacklist entry to remove.
	ErrNotBlacklisted = errors.New("channel is not blacklisted")
	// ErrBotNotInChannel means a history backfill was requested for a channel
	// the bot is not a member of.
	ErrBotNotInChannel = errors.New("bot is not in the channel; invite it first")
)

// Slack is what the Slack actions act through (*adminapi.Server). While
// Slack is off its methods return adminapi.ErrSlackOff, except
// StartSlackImageReplay, which works on stored events.
type Slack interface {
	SetSlackChannelConfig(ctx context.Context, channelID, field string, enabled bool, updatedBy string) error
	// AddSlackBlacklist looks the channel's name up when channelName is empty.
	AddSlackBlacklist(ctx context.Context, channelID, channelName, reason, addedBy string) (slackcx.BlacklistEntry, error)
	RemoveSlackBlacklist(ctx context.Context, channelID string) (bool, error)
	// SetSlackMembership reports whether the bot already was, or wasn't, in
	// the channel.
	SetSlackMembership(ctx context.Context, channelID string, joined bool) (already bool, err error)
	// RequestSlackBackfill resets the channel's history walk, after checking
	// the bot is in it, and reports whether the channel already had one.
	RequestSlackBackfill(ctx context.Context, channelID string) (wasReset bool, err error)
	RequestSlackDocumentBackfill(ctx context.Context, channelID, updatedBy string) error
	StartSlackImageReplay(ctx context.Context, requestedBy, channelID string) (int64, error)
}

// SetSlackChannelConfig turns one of a channel's switches on or off.
func SetSlackChannelConfig(ctx context.Context, a Audit, slack Slack, channelID, field string, enabled bool) error {
	switch field {
	case SlackConfigAsk, SlackConfigDocuments, SlackConfigExternal:
	default:
		return fmt.Errorf("%w %q", ErrUnknownSlackConfig, field)
	}
	return a.Run(ctx, "sources.slack.config."+field, channelID, map[string]any{"enabled": enabled}, func() error {
		if slack == nil {
			return errors.New("slack source controls unavailable")
		}
		return slack.SetSlackChannelConfig(ctx, channelID, field, enabled, a.actor())
	})
}

// BlacklistSlackChannel stops ingesting a channel. channelName is optional.
func BlacklistSlackChannel(ctx context.Context, a Audit, slack Slack, channelID, channelName, reason string) (slackcx.BlacklistEntry, error) {
	reason = strings.TrimSpace(reason)
	var entry slackcx.BlacklistEntry
	err := a.Run(ctx, "sources.slack.blacklist", channelID, map[string]any{"reason": reason}, func() error {
		if slack == nil {
			return errors.New("slack source controls unavailable")
		}
		var err error
		entry, err = slack.AddSlackBlacklist(ctx, channelID, channelName, reason, a.actor())
		return err
	})
	return entry, err
}

// UnblacklistSlackChannel removes a channel's blacklist entry. It fails with
// ErrNotBlacklisted when the channel has none.
func UnblacklistSlackChannel(ctx context.Context, a Audit, slack Slack, channelID string) error {
	return a.Run(ctx, "sources.slack.unblacklist", channelID, map[string]any{}, func() error {
		if slack == nil {
			return errors.New("slack source controls unavailable")
		}
		removed, err := slack.RemoveSlackBlacklist(ctx, channelID)
		if err == nil && !removed {
			err = ErrNotBlacklisted
		}
		return err
	})
}

// SetSlackMembership invites the bot to a channel or removes it, and reports
// whether it already was, or wasn't, a member.
func SetSlackMembership(ctx context.Context, a Audit, slack Slack, channelID string, joined bool) (bool, error) {
	action := "sources.slack.invite"
	if !joined {
		action = "sources.slack.leave"
	}
	var already bool
	err := a.Run(ctx, action, channelID, map[string]any{}, func() error {
		if slack == nil {
			return errors.New("slack membership control unavailable")
		}
		var err error
		already, err = slack.SetSlackMembership(ctx, channelID, joined)
		return err
	})
	return already, err
}

// RequestSlackBackfill re-walks a channel's message history, and reports
// whether the channel already had a history walk.
func RequestSlackBackfill(ctx context.Context, a Audit, slack Slack, channelID string) (bool, error) {
	var wasReset bool
	err := a.Run(ctx, "sources.slack.backfill", channelID, map[string]any{"kind": "messages"}, func() error {
		if slack == nil {
			return errors.New("slack backfill control unavailable")
		}
		var err error
		wasReset, err = slack.RequestSlackBackfill(ctx, channelID)
		return err
	})
	return wasReset, err
}

// RequestSlackDocumentBackfill restarts a channel's document reconciliation
// from its first message, for a channel with document ingest on.
func RequestSlackDocumentBackfill(ctx context.Context, a Audit, slack Slack, channelID string) error {
	return a.Run(ctx, "sources.slack.backfill", channelID, map[string]any{"kind": "documents"}, func() error {
		if slack == nil {
			return errors.New("slack backfill control unavailable")
		}
		return slack.RequestSlackDocumentBackfill(ctx, channelID, a.actor())
	})
}

// StartSlackImageReplay rebuilds the images of one channel's stored
// messages, or of every channel's when channelID is empty, as a durable job,
// and returns the job's ID.
func StartSlackImageReplay(ctx context.Context, a Audit, slack Slack, channelID string) (int64, error) {
	channelID = strings.TrimSpace(channelID)
	target := channelID
	if target == "" {
		target = "all"
	}
	var jobID int64
	err := a.Run(ctx, "sources.slack.images.enqueue", target, map[string]any{"kind": "images"}, func() error {
		if slack == nil {
			return errors.New("slack image replay is unavailable")
		}
		var err error
		jobID, err = slack.StartSlackImageReplay(ctx, a.actor(), channelID)
		return err
	})
	return jobID, err
}

// SlackDocuments rebuilds what Diderot read from Slack documents
// (*adminapi.Server).
type SlackDocuments interface {
	StartSlackDocumentRebuild(ctx context.Context, requestedBy string) (int64, error)
}

// RebuildSlackDocuments deletes what Diderot read from the documents of
// every channel with document indexing on and queues them to be read again,
// as a durable job, and returns the job's ID.
func RebuildSlackDocuments(ctx context.Context, a Audit, slack SlackDocuments) (int64, error) {
	var jobID int64
	err := a.Run(ctx, "sources.slack.documents.rebuild", "all", map[string]any{}, func() error {
		if slack == nil {
			return errors.New("slack document rebuild is unavailable")
		}
		var err error
		jobID, err = slack.StartSlackDocumentRebuild(ctx, a.actor())
		return err
	})
	return jobID, err
}
