package discord

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

// The source's state keys. The leader writes the directory, the progress
// and the gateway's status; administrators write the selection, through
// Channels on any replica.
const (
	directoryKey = "directory"
	selectionKey = "channels"
	progressKey  = "progress"
	gatewayKey   = "gateway"
	rereadAllKey = "reread_all"
)

// Directory is every guild the bot is in and their channels, as the leader
// last read them.
type Directory struct {
	ReadAt time.Time `json:"read_at"`
	Error  string    `json:"error,omitempty"`
	Bot    string    `json:"bot,omitempty"`
	// ApplicationID is the bot's application, which its invite link names.
	ApplicationID string       `json:"application_id,omitempty"`
	Guilds        []GuildEntry `json:"guilds"`
}

// InviteURL is the link that adds the bot to a server with the permissions
// indexing and Ask need, and its commands; "" before the directory is
// read.
func (d Directory) InviteURL() string {
	if d.ApplicationID == "" {
		return ""
	}
	return "https://discord.com/oauth2/authorize?client_id=" + d.ApplicationID +
		"&scope=bot+applications.commands&permissions=" + strconv.FormatInt(AskPermissions, 10)
}

// GuildEntry is one guild in the directory.
type GuildEntry struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Roles    map[string]string `json:"roles,omitempty"`
	Channels []ChannelEntry    `json:"channels"`
}

// ChannelEntry is one channel the connector could index.
type ChannelEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	Category string `json:"category,omitempty"`
	Position int    `json:"position"`
	// Private says @everyone cannot view the channel in Discord.
	Private bool `json:"private,omitempty"`
	// Readable says the bot can view the channel and read its history.
	Readable bool `json:"readable"`
}

// Kind is what the channel is called on the page.
func (c ChannelEntry) Kind() string {
	switch c.Type {
	case channelAnnouncement:
		return "announcements"
	case channelForum:
		return "forum"
	case channelMedia:
		return "media"
	}
	return "text"
}

// channel finds a channel in the directory, with its guild.
func (d Directory) channel(id string) (GuildEntry, ChannelEntry, bool) {
	for _, g := range d.Guilds {
		for _, c := range g.Channels {
			if c.ID == id {
				return g, c, true
			}
		}
	}
	return GuildEntry{}, ChannelEntry{}, false
}

// ChannelScope is the scope of a channel's messages: one per channel, as
// Slack's are, so that an answer posted in Discord can be limited to the
// channels its audience can read. Diderot's readers get it through the
// channel's visibility (Connector.GrantScopes).
func ChannelScope(channelID string) string { return "discord-channel:" + channelID }

// Selection is a channel an administrator chose to index.
type Selection struct {
	GuildID string `json:"guild_id"`
	// Name is the channel's name when it was chosen, shown while the
	// directory no longer lists it.
	Name string `json:"name"`
	// Scope is who may read the channel's messages in Diderot: the scope
	// whose readers are granted the channel's own (internal, for everyone
	// who can use Diderot, or corpus:<name>).
	Scope   string    `json:"scope"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
	// RereadAt is when an administrator last asked for its whole history
	// to be read again.
	RereadAt *time.Time `json:"reread_at,omitempty"`
	// Removing says an administrator removed it: the leader deletes its
	// messages from the index, then the selection.
	Removing bool `json:"removing,omitempty"`
}

// Progress is how the leader's reading of one channel stands.
type Progress struct {
	// Scope is the scope its indexed messages carry.
	Scope string `json:"scope"`
	// Cursors are the newest message read in each stream: the channel and
	// each of its threads, by ID.
	Cursors map[string]string `json:"cursors,omitempty"`
	// Threads are the names of its threads, by ID.
	Threads map[string]string `json:"threads,omitempty"`
	// FullReadAt is when the last read of its whole history (to the
	// history limit) finished, and RecentReadAt the last daily re-read.
	FullReadAt   *time.Time `json:"full_read_at,omitempty"`
	FullReadFrom *time.Time `json:"full_read_from,omitempty"`
	RecentReadAt *time.Time `json:"recent_read_at,omitempty"`
	TailAt       *time.Time `json:"tail_at,omitempty"`
	Messages     int64      `json:"messages"`
	// Error is why the last read of the channel's history (whole, or the
	// daily week) failed, and ErrorAt when; a read that succeeds clears
	// them, and a channel with one waits readRetryAfter before its next
	// read. TailError is why the last tail failed, which the next good tail
	// clears.
	Error     string     `json:"error,omitempty"`
	ErrorAt   *time.Time `json:"error_at,omitempty"`
	TailError string     `json:"tail_error,omitempty"`
}

// Problem is what the page says is wrong with reading the channel, or "".
func (p Progress) Problem() string {
	if p.Error != "" {
		return p.Error
	}
	return p.TailError
}

// GatewayState is the live connection as the leader last reported it.
type GatewayState struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`
	LastEvent time.Time `json:"last_event,omitempty"`
	Error     string    `json:"error,omitempty"`
	Fatal     bool      `json:"fatal,omitempty"`
}

// Visibility choices. A channel's scope is either everyone who can use
// Diderot, or the readers the authorization policy grants one corpus scope.
const (
	VisibilityEveryone = "everyone"
	VisibilityPolicy   = "policy"
)

var policyScopeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ScopeFor is the scope of a visibility choice: events.ScopeInternal for
// everyone, or corpus:<name> for a policy scope.
func ScopeFor(visibility, name string) (string, error) {
	switch visibility {
	case VisibilityEveryone:
		return events.ScopeInternal, nil
	case VisibilityPolicy:
		name = strings.TrimPrefix(strings.TrimSpace(name), "corpus:")
		if !policyScopeName.MatchString(name) {
			return "", &ChoiceError{"name the policy scope with lower-case letters, digits, hyphens and underscores, such as discord-mods"}
		}
		return "corpus:" + name, nil
	}
	return "", &ChoiceError{"choose who may read the channel"}
}

// ChoiceError is a channel choice the page refuses, with what to change.
type ChoiceError struct{ Message string }

func (e *ChoiceError) Error() string { return e.Message }

// ErrPrivateNeedsChoice refuses the default visibility for a channel that
// is private in Discord.
var ErrPrivateNeedsChoice = &ChoiceError{"this channel is private in Discord: confirm who may read it here"}

// Channels is the administrator's side of channel selection: what the page
// shows and the changes it makes. Every method works on any replica.
type Channels struct {
	State source.State
}

// ChannelsFor returns the channel selection of the source whose deps these
// are.
func ChannelsFor(deps source.Deps) Channels { return Channels{State: deps.State} }

// Overview is everything the connector's channel section shows.
type Overview struct {
	Directory  Directory
	Selections map[string]Selection
	Progress   map[string]Progress
	Gateway    GatewayState
	HasGateway bool
	Ask        AskSettings
}

// Overview reads the directory, the selection and the progress.
func (c Channels) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	if _, err := c.State.Get(ctx, directoryKey, &o.Directory); err != nil {
		return Overview{}, err
	}
	if _, err := c.State.Get(ctx, selectionKey, &o.Selections); err != nil {
		return Overview{}, err
	}
	if _, err := c.State.Get(ctx, progressKey, &o.Progress); err != nil {
		return Overview{}, err
	}
	found, err := c.State.Get(ctx, gatewayKey, &o.Gateway)
	if err != nil {
		return Overview{}, err
	}
	o.HasGateway = found
	if _, err := c.State.Get(ctx, askKey, &o.Ask); err != nil {
		return Overview{}, err
	}
	return o, nil
}

// Add chooses a channel the directory lists, with a visibility; a private
// channel needs confirmed set. Adding a chosen channel changes its
// visibility.
func (c Channels) Add(ctx context.Context, channelID, visibility, policyName string, confirmed bool, actor string) (Selection, error) {
	scope, err := ScopeFor(visibility, policyName)
	if err != nil {
		return Selection{}, err
	}
	var directory Directory
	if _, err := c.State.Get(ctx, directoryKey, &directory); err != nil {
		return Selection{}, err
	}
	g, ch, ok := directory.channel(channelID)
	if !ok {
		return Selection{}, &ChoiceError{"the bot can't see that channel; check its permissions in Discord, then wait for the next read of the servers"}
	}
	if ch.Private && !confirmed {
		return Selection{}, ErrPrivateNeedsChoice
	}
	var selected Selection
	var selections map[string]Selection
	err = c.State.Update(ctx, selectionKey, &selections, func(bool) error {
		if selections == nil {
			selections = map[string]Selection{}
		}
		selected = selections[channelID]
		if selected.AddedAt.IsZero() || selected.Removing {
			selected = Selection{AddedBy: actor, AddedAt: time.Now().UTC()}
		}
		selected.GuildID, selected.Name, selected.Scope = g.ID, ch.Name, scope
		selections[channelID] = selected
		return nil
	})
	return selected, err
}

// Remove takes a channel out of the index.
func (c Channels) Remove(ctx context.Context, channelID string) (bool, error) {
	var removed bool
	var selections map[string]Selection
	err := c.State.Update(ctx, selectionKey, &selections, func(bool) error {
		selected, ok := selections[channelID]
		if !ok || selected.Removing {
			return nil
		}
		selected.Removing, removed = true, true
		selections[channelID] = selected
		return nil
	})
	return removed, err
}

// Reread asks for a chosen channel's whole history to be read again.
func (c Channels) Reread(ctx context.Context, channelID string) (bool, error) {
	var requested bool
	var selections map[string]Selection
	err := c.State.Update(ctx, selectionKey, &selections, func(bool) error {
		selected, ok := selections[channelID]
		if !ok || selected.Removing {
			return nil
		}
		now := time.Now().UTC()
		selected.RereadAt, requested = &now, true
		selections[channelID] = selected
		return nil
	})
	return requested, err
}

// selectedIDs is the chosen channels, not being removed, in a stable order.
func selectedIDs(selections map[string]Selection) []string {
	var ids []string
	for id, s := range selections {
		if !s.Removing {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// forget drops a removed channel's selection once its messages are gone.
func forget(ctx context.Context, state source.State, channelID string) error {
	var selections map[string]Selection
	return state.Update(ctx, selectionKey, &selections, func(bool) error {
		if s, ok := selections[channelID]; ok && s.Removing {
			delete(selections, channelID)
		}
		return nil
	})
}

// prefix is the external-ID prefix every message of a channel (and its
// threads) shares.
func prefix(guildID, channelID string) string { return guildID + "/" + channelID + "/" }

func describeErr(err error) string {
	switch {
	case isStatus(err, 401):
		return "Discord refused the bot token"
	case isStatus(err, 403):
		return "the bot is not allowed to read it (it needs View Channel and Read Message History)"
	case isStatus(err, 404):
		return "Discord no longer has it"
	}
	return fmt.Sprint(err)
}
