package adminaction

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/connector/discord"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// DiscordChannels is the Discord connector's channel selection
// (discord.Channels).
type DiscordChannels interface {
	Add(ctx context.Context, channelID, visibility, policyName string, confirmed bool, actor string) (discord.Selection, error)
	Remove(ctx context.Context, channelID string) (bool, error)
	Reread(ctx context.Context, channelID string) (bool, error)
}

// ChooseDiscordChannel indexes a Discord channel with a visibility, or
// changes the visibility of one already chosen. A channel private in
// Discord needs confirmed.
func ChooseDiscordChannel(ctx context.Context, a Audit, channels DiscordChannels, channelID, visibility, policyName string, confirmed bool) (discord.Selection, error) {
	var selected discord.Selection
	details := map[string]any{"visibility": visibility, "policy_scope": policyName, "confirmed_private": confirmed}
	err := a.Run(ctx, "sources.discord.channel.choose", channelID, details, func() error {
		var err error
		selected, err = channels.Add(ctx, channelID, visibility, policyName, confirmed, a.actor())
		return err
	})
	return selected, err
}

// RemoveDiscordChannel takes a Discord channel out of the index, and reports
// whether it was chosen.
func RemoveDiscordChannel(ctx context.Context, a Audit, channels DiscordChannels, channelID string) (bool, error) {
	var removed bool
	err := a.Run(ctx, "sources.discord.channel.remove", channelID, map[string]any{}, func() error {
		var err error
		removed, err = channels.Remove(ctx, channelID)
		return err
	})
	return removed, err
}

// RereadDiscordChannel asks for a chosen Discord channel's whole history to
// be read again, and reports whether it is chosen.
func RereadDiscordChannel(ctx context.Context, a Audit, channels DiscordChannels, channelID string) (bool, error) {
	var requested bool
	err := a.Run(ctx, "sources.discord.channel.reread", channelID, map[string]any{}, func() error {
		var err error
		requested, err = channels.Reread(ctx, channelID)
		return err
	})
	return requested, err
}

// DiscordAsk is where Ask answers in Discord (discord.Channels).
type DiscordAsk interface {
	SetAskInGuild(ctx context.Context, guildID string, on bool) error
	SetAskInChannel(ctx context.Context, channelID string, on bool) error
	SetAskDirectMessages(ctx context.Context, on bool) error
	SetTrustedGuild(ctx context.Context, guildID string, trusted bool) error
}

// SetDiscordAskInGuild turns Ask on or off in a Discord server.
func SetDiscordAskInGuild(ctx context.Context, a Audit, ask DiscordAsk, guildID string, on bool) error {
	return a.Run(ctx, "sources.discord.ask.guild", guildID, map[string]any{"on": on}, func() error {
		return ask.SetAskInGuild(ctx, guildID, on)
	})
}

// SetDiscordAskInChannel turns Ask on or off in one Discord channel.
func SetDiscordAskInChannel(ctx context.Context, a Audit, ask DiscordAsk, channelID string, on bool) error {
	return a.Run(ctx, "sources.discord.ask.channel", channelID, map[string]any{"on": on}, func() error {
		return ask.SetAskInChannel(ctx, channelID, on)
	})
}

// SetDiscordTrustedGuild marks a Discord server as one whose members are the
// organisation's own people, so Ask there may answer from internal content,
// or no longer.
func SetDiscordTrustedGuild(ctx context.Context, a Audit, ask DiscordAsk, guildID string, trusted bool) error {
	return a.Run(ctx, "sources.discord.ask.trusted", guildID, map[string]any{"trusted": trusted}, func() error {
		return ask.SetTrustedGuild(ctx, guildID, trusted)
	})
}

// SetDiscordAskDirectMessages turns Ask's answers to Discord direct
// messages on or off.
func SetDiscordAskDirectMessages(ctx context.Context, a Audit, ask DiscordAsk, on bool) error {
	return a.Run(ctx, "sources.discord.ask.direct_messages", "discord", map[string]any{"on": on}, func() error {
		return ask.SetAskDirectMessages(ctx, on)
	})
}

// RequestSourceBackfill records a registered source's backfill with args, a
// trimmed string for each of its fields, and returns when it was recorded.
func RequestSourceBackfill(ctx context.Context, a Audit, backfiller source.Backfiller, deps source.Deps, name string, args settings.Values) (time.Time, error) {
	var requestedAt time.Time
	err := a.Run(ctx, "sources."+name+".backfill", name, args, func() error {
		var err error
		requestedAt, err = backfiller.RequestBackfill(ctx, deps.For(name), args)
		return err
	})
	return requestedAt, err
}

// NotionRoots is the Notion connector's managed roots (*notion.RootsStore).
type NotionRoots interface {
	Add(ctx context.Context, input, label, addedBy string) (string, error)
	Remove(ctx context.Context, input string) (bool, error)
}

// AddNotionRoot adds a Notion page or database, by URL or UUID, as a root to
// index, and returns its object ID.
func AddNotionRoot(ctx context.Context, a Audit, roots NotionRoots, input, label string) (string, error) {
	input, label = strings.TrimSpace(input), strings.TrimSpace(label)
	var objectID string
	err := a.Run(ctx, "sources.notion.root.add", input, map[string]any{"label": label}, func() error {
		if roots == nil {
			return errors.New("notion root store unavailable")
		}
		var err error
		objectID, err = roots.Add(ctx, input, label, a.actor())
		return err
	})
	return objectID, err
}

// RemoveNotionRoot removes a root, and reports whether there was one.
func RemoveNotionRoot(ctx context.Context, a Audit, roots NotionRoots, input string) (bool, error) {
	input = strings.TrimSpace(input)
	var removed bool
	err := a.Run(ctx, "sources.notion.root.remove", input, map[string]any{}, func() error {
		if roots == nil {
			return errors.New("notion root store unavailable")
		}
		var err error
		removed, err = roots.Remove(ctx, input)
		return err
	})
	return removed, err
}
