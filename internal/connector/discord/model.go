package discord

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"
)

// The Discord objects the connector reads, with only the fields it uses.
// Discord's documentation names each: https://docs.discord.com/developers.

// Channel types (https://docs.discord.com/developers/resources/channel).
const (
	channelText             = 0
	channelCategory         = 4
	channelAnnouncement     = 5
	channelAnnouncementPost = 10
	channelPublicThread     = 11
	channelPrivateThread    = 12
	channelForum            = 15
	channelMedia            = 16
)

// indexable reports whether a top-level channel of type t holds messages
// (or forum posts) the connector can index.
func indexable(t int) bool {
	switch t {
	case channelText, channelAnnouncement, channelForum, channelMedia:
		return true
	}
	return false
}

// isThread reports whether a channel of type t is a thread.
func isThread(t int) bool {
	return t == channelAnnouncementPost || t == channelPublicThread || t == channelPrivateThread
}

// Message types the connector indexes: ordinary messages, replies, and the
// messages of slash and context-menu commands. Joins, pins, boosts, thread
// notices and the rest are Discord's own notices, not anyone's words.
func indexedMessageType(t int) bool {
	switch t {
	case 0, 19, 20, 23:
		return true
	}
	return false
}

// Permission bits (https://docs.discord.com/developers/topics/permissions).
const (
	permAdministrator        int64 = 1 << 3
	permAddReactions         int64 = 1 << 6
	permViewChannel          int64 = 1 << 10
	permSendMessages         int64 = 1 << 11
	permEmbedLinks           int64 = 1 << 14
	permAttachFiles          int64 = 1 << 15
	permReadMessageHistory   int64 = 1 << 16
	permUseCommands          int64 = 1 << 31
	permCreatePublicThreads  int64 = 1 << 35
	permSendMessagesInThread int64 = 1 << 38
)

// AskPermissions are the permissions the bot needs to answer: to read and
// post in a channel and its threads, start a thread from a question, react,
// and attach panels.
const AskPermissions = permViewChannel | permReadMessageHistory | permSendMessages | permSendMessagesInThread |
	permCreatePublicThreads | permAddReactions | permEmbedLinks | permAttachFiles | permUseCommands

// Gateway intents the connector identifies with: guilds and channels,
// guild messages, their reactions, message content (privileged), and,
// for Ask, direct messages and their reactions.
const (
	intentGuilds                 = 1 << 0
	intentGuildMessages          = 1 << 9
	intentGuildMessageReactions  = 1 << 10
	intentDirectMessages         = 1 << 12
	intentDirectMessageReactions = 1 << 13
	intentMessageContent         = 1 << 15
	gatewayIntents               = intentGuilds | intentGuildMessages | intentGuildMessageReactions |
		intentDirectMessages | intentDirectMessageReactions | intentMessageContent
)

// Application flags that say the Message Content intent is on.
const (
	appFlagMessageContent        = 1 << 18
	appFlagMessageContentLimited = 1 << 19
)

// discordEpoch is the start of snowflake time, in Unix milliseconds.
const discordEpoch = 1420070400000

// snowflakeTime is when the object a snowflake names was created.
func snowflakeTime(id string) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + discordEpoch).UTC()
}

// snowflakeAt is the smallest snowflake of an object created at t: a
// message ID cursor for "after t".
func snowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// laterID reports whether snowflake a is newer than b ("" is oldest).
func laterID(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

// displayName is what Discord shows for the user outside any guild.
func (u user) displayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

type member struct {
	Nick  string   `json:"nick"`
	Roles []string `json:"roles"`
	User  *user    `json:"user"`
	// Pending says the member has not passed the server's Membership
	// Screening yet.
	Pending bool  `json:"pending"`
	Flags   int64 `json:"flags"`
}

// memberIsGuest marks a guest, who can only reach the voice channel they
// were invited to.
const memberIsGuest = 1 << 4

// full reports whether m is a member in full: past Membership Screening and
// not a guest.
func (m member) full() bool { return !m.Pending && m.Flags&memberIsGuest == 0 }

type role struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Permissions string `json:"permissions"`
}

type overwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"`
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

type threadMetadata struct {
	Archived         bool   `json:"archived"`
	ArchiveTimestamp string `json:"archive_timestamp"`
}

type channel struct {
	ID                   string          `json:"id"`
	Type                 int             `json:"type"`
	GuildID              string          `json:"guild_id"`
	Name                 string          `json:"name"`
	ParentID             string          `json:"parent_id"`
	Position             int             `json:"position"`
	PermissionOverwrites []overwrite     `json:"permission_overwrites"`
	ThreadMetadata       *threadMetadata `json:"thread_metadata"`
	LastMessageID        string          `json:"last_message_id"`
}

type guild struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Features []string `json:"features,omitempty"`
}

// previewable reports whether people who are not members can read the
// guild's channels: from the server directory, or before joining.
func (g guild) previewable() bool {
	return slices.Contains(g.Features, "PREVIEW_ENABLED") || slices.Contains(g.Features, "DISCOVERABLE")
}

type attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
	Description string `json:"description"`
}

type embedMedia struct {
	URL      string `json:"url"`
	ProxyURL string `json:"proxy_url"`
}

type embed struct {
	Type        string      `json:"type"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	URL         string      `json:"url"`
	Image       *embedMedia `json:"image"`
	Thumbnail   *embedMedia `json:"thumbnail"`
}

type emoji struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type reaction struct {
	Count int   `json:"count"`
	Emoji emoji `json:"emoji"`
}

type messageReference struct {
	Type      int    `json:"type"`
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
}

type snapshot struct {
	Message struct {
		Content     string       `json:"content"`
		Attachments []attachment `json:"attachments"`
		Embeds      []embed      `json:"embeds"`
	} `json:"message"`
}

type sticker struct {
	Name string `json:"name"`
}

type poll struct {
	Question struct {
		Text string `json:"text"`
	} `json:"question"`
}

type message struct {
	ID                string            `json:"id"`
	ChannelID         string            `json:"channel_id"`
	GuildID           string            `json:"guild_id"`
	Type              int               `json:"type"`
	Author            user              `json:"author"`
	Member            *member           `json:"member"`
	Content           string            `json:"content"`
	Timestamp         time.Time         `json:"timestamp"`
	EditedTimestamp   *time.Time        `json:"edited_timestamp"`
	Mentions          []user            `json:"mentions"`
	MentionRoles      []string          `json:"mention_roles"`
	Attachments       []attachment      `json:"attachments"`
	Embeds            []embed           `json:"embeds"`
	Reactions         []reaction        `json:"reactions"`
	MessageReference  *messageReference `json:"message_reference"`
	ReferencedMessage *message          `json:"referenced_message"`
	MessageSnapshots  []snapshot        `json:"message_snapshots"`
	Thread            *channel          `json:"thread"`
	StickerItems      []sticker         `json:"sticker_items"`
	Poll              *poll             `json:"poll"`
	Pinned            bool              `json:"pinned"`
	Components        []component       `json:"components,omitempty"`
}

// mentions reports whether m mentions the user userID.
func (m message) mentions(userID string) bool {
	for _, u := range m.Mentions {
		if u.ID == userID {
			return true
		}
	}
	return false
}

// interaction is a command or a component click, from the gateway.
type interaction struct {
	ID            string   `json:"id"`
	ApplicationID string   `json:"application_id"`
	Type          int      `json:"type"`
	Token         string   `json:"token"`
	GuildID       string   `json:"guild_id"`
	ChannelID     string   `json:"channel_id"`
	Channel       *channel `json:"channel"`
	// Member is the clicker in a guild, User in a DM.
	Member  *member  `json:"member"`
	User    *user    `json:"user"`
	Message *message `json:"message"`
	Data    struct {
		Name     string `json:"name"`
		Type     int    `json:"type"`
		CustomID string `json:"custom_id"`
		TargetID string `json:"target_id"`
		Options  []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"options"`
		Resolved struct {
			Messages map[string]message `json:"messages"`
		} `json:"resolved"`
	} `json:"data"`
}

// user is whoever made the interaction.
func (i interaction) user() user {
	if i.Member != nil && i.Member.User != nil {
		return *i.Member.User
	}
	if i.User != nil {
		return *i.User
	}
	return user{}
}

// option is a command option's value as text, and a bool option's as
// "true" or "false".
func (i interaction) option(name string) string {
	for _, o := range i.Data.Options {
		if o.Name != name {
			continue
		}
		var s string
		if json.Unmarshal(o.Value, &s) == nil {
			return s
		}
		return string(o.Value)
	}
	return ""
}

type application struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Flags int64  `json:"flags"`
}

type archivedThreads struct {
	Threads []channel `json:"threads"`
	HasMore bool      `json:"has_more"`
}

type activeThreads struct {
	Threads []channel `json:"threads"`
}

// rawMessage is a message as stored in an event's raw column: what the
// connector read, less the referenced message (stored as its own event).
func rawMessage(m message) json.RawMessage {
	m.ReferencedMessage = nil
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}
