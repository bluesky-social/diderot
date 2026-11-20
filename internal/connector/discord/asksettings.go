package discord

import (
	"context"
	"slices"
)

// askKey is the state key of Ask's settings, which administrators write
// through Channels on any replica.
const askKey = "ask"

// AskSettings say where Ask answers in Discord. It is off until turned on.
type AskSettings struct {
	// Guilds are the servers where Ask answers mentions and commands.
	Guilds map[string]bool `json:"guilds,omitempty"`
	// ChannelsOff are channels of those servers where it does not; a
	// channel's threads follow it.
	ChannelsOff map[string]bool `json:"channels_off,omitempty"`
	// DirectMessages says Ask answers direct messages.
	DirectMessages bool `json:"direct_messages,omitempty"`
	// Trusted are the servers whose members are the organisation's own
	// people: Ask there may read what every Diderot user can (internal).
	Trusted map[string]bool `json:"trusted,omitempty"`
}

// answersIn reports whether Ask answers in a guild's channel.
func (s AskSettings) answersIn(guildID, channelID string) bool {
	return s.Guilds[guildID] && !s.ChannelsOff[channelID]
}

// guilds is the servers where Ask is on, in a stable order.
func (s AskSettings) guilds() []string {
	var ids []string
	for id, on := range s.Guilds {
		if on {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// AskSettings reads Ask's settings.
func (c Channels) AskSettings(ctx context.Context) (AskSettings, error) {
	var s AskSettings
	_, err := c.State.Get(ctx, askKey, &s)
	return s, err
}

// SetAskInGuild turns Ask on or off in a server the directory lists.
func (c Channels) SetAskInGuild(ctx context.Context, guildID string, on bool) error {
	var directory Directory
	if _, err := c.State.Get(ctx, directoryKey, &directory); err != nil {
		return err
	}
	// Turning off works for a server the bot has left, so a setting can't
	// come back to life if it rejoins.
	if on && !slices.ContainsFunc(directory.Guilds, func(g GuildEntry) bool { return g.ID == guildID }) {
		return &ChoiceError{"the bot is not in that server"}
	}
	var s AskSettings
	return c.State.Update(ctx, askKey, &s, func(bool) error {
		if s.Guilds == nil {
			s.Guilds = map[string]bool{}
		}
		if on {
			s.Guilds[guildID] = true
		} else {
			delete(s.Guilds, guildID)
		}
		return nil
	})
}

// SetTrustedGuild marks a server the directory lists as one whose members
// are the organisation's own people, or no longer.
func (c Channels) SetTrustedGuild(ctx context.Context, guildID string, trusted bool) error {
	var directory Directory
	if _, err := c.State.Get(ctx, directoryKey, &directory); err != nil {
		return err
	}
	// Untrusting works for a server the bot has left, so trust can't come
	// back to life if it rejoins.
	if trusted && !slices.ContainsFunc(directory.Guilds, func(g GuildEntry) bool { return g.ID == guildID }) {
		return &ChoiceError{"the bot is not in that server"}
	}
	var s AskSettings
	return c.State.Update(ctx, askKey, &s, func(bool) error {
		if s.Trusted == nil {
			s.Trusted = map[string]bool{}
		}
		if trusted {
			s.Trusted[guildID] = true
		} else {
			delete(s.Trusted, guildID)
		}
		return nil
	})
}

// SetAskInChannel turns Ask on or off in one channel of a server where it
// is on.
func (c Channels) SetAskInChannel(ctx context.Context, channelID string, on bool) error {
	var directory Directory
	if _, err := c.State.Get(ctx, directoryKey, &directory); err != nil {
		return err
	}
	if _, _, ok := directory.channel(channelID); !ok {
		return &ChoiceError{"the bot can't see that channel"}
	}
	var s AskSettings
	return c.State.Update(ctx, askKey, &s, func(bool) error {
		if s.ChannelsOff == nil {
			s.ChannelsOff = map[string]bool{}
		}
		if on {
			delete(s.ChannelsOff, channelID)
		} else {
			s.ChannelsOff[channelID] = true
		}
		return nil
	})
}

// SetAskDirectMessages turns Ask's answers to direct messages on or off.
func (c Channels) SetAskDirectMessages(ctx context.Context, on bool) error {
	var s AskSettings
	return c.State.Update(ctx, askKey, &s, func(bool) error {
		s.DirectMessages = on
		return nil
	})
}
