package discord

import (
	"fmt"
	"mime"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
)

// ConnectorName is the source's connector_settings row and its events'
// source.
const ConnectorName = "discord"

// place is where a message was posted: its guild, its top-level channel, and
// the thread it is in, if any.
type place struct {
	GuildID, GuildName     string
	ChannelID, ChannelName string
	ThreadID, ThreadName   string
	// Roles and Channels name the IDs a message can mention.
	Roles    map[string]string
	Channels map[string]string
}

// streamID is the channel the message is actually in: the thread, or the
// channel.
func (p place) streamID() string {
	if p.ThreadID != "" {
		return p.ThreadID
	}
	return p.ChannelID
}

// title is how the place reads in a result: "#channel", or
// "#channel › thread".
func (p place) title() string {
	title := "#" + p.ChannelName
	if p.ThreadID != "" {
		title += " › " + p.ThreadName
	}
	return title
}

// rendered is a message as an event, with its attachments for the image and
// document workers.
type rendered struct {
	Event     events.Event
	Images    []events.ImageRef
	Documents []events.DocumentRef
}

// externalID is a message's event: its guild, top-level channel, and ID.
func externalID(p place, messageID string) string { return prefix(p.GuildID, p.ChannelID) + messageID }

// messageURL is the link Discord opens the message at.
func messageURL(guildID, channelID, messageID string) string {
	return "https://discord.com/channels/" + guildID + "/" + channelID + "/" + messageID
}

// render makes the event of a message posted at p, with scope. It reports
// false for a message with nothing to index: a notice, or an empty one.
func render(m message, p place, scope string) (rendered, bool) {
	if !indexedMessageType(m.Type) {
		return rendered{}, false
	}
	body := renderBody(m, p)
	if strings.TrimSpace(events.SanitizeText(body)) == "" {
		// A body that is only a NUL byte (or other content the sink
		// would sanitize away entirely) has nothing to index, same as
		// a genuinely empty one. Checking the sanitized form here,
		// not the raw TrimSpace of body, is what makes this case a
		// skip (write's caller takes the event out via r.remove)
		// instead of a Sink.Upsert call doomed to fail validate()'s
		// "Body is required" check forever.
		return rendered{}, false
	}
	id := externalID(p, m.ID)
	var threadID string
	switch {
	case p.ThreadID != "":
		threadID = externalID(p, p.ThreadID)
	case m.Thread != nil && m.Thread.ID == m.ID && m.Thread.Type != channelPrivateThread:
		// The message started a thread, which has its ID: it is the
		// thread's root.
		threadID = id
	}
	occurred := m.Timestamp
	if occurred.IsZero() {
		occurred = snowflakeTime(m.ID)
	}
	out := rendered{Event: events.Event{
		Source: ConnectorName, ExternalID: id, OccurredAt: occurred.UTC(),
		Actor: m.Author.displayName(), ActorID: m.Author.ID, ActorCanonical: "discord:" + m.Author.ID,
		ThreadID: threadID, Title: p.title(), Body: body,
		URL:           messageURL(p.GuildID, p.streamID(), m.ID),
		Raw:           rawMessage(m),
		ContextPrefix: "Discord " + p.title() + " in " + p.GuildName,
		Scope:         scope,
	}}
	for _, a := range m.Attachments {
		mediaType := mediaTypeOf(a.ContentType)
		fileID := attachmentFileID(p.streamID(), m.ID, a.ID)
		if files.CaptionableImage(mediaType) {
			out.Images = append(out.Images, events.ImageRef{SourceImageID: fileID, Mimetype: mediaType})
		}
		if format := events.DocFormatForMediaType(mediaType, a.Filename); format != "" {
			out.Documents = append(out.Documents, events.DocumentRef{SourceFileID: fileID, Filename: a.Filename, Mimetype: mediaType, DocFormat: format})
		}
	}
	for i, e := range m.Embeds {
		mediaType := imageTypeOf(embedImage(e))
		if !files.CaptionableImage(mediaType) {
			continue
		}
		out.Images = append(out.Images, events.ImageRef{SourceImageID: embedFileID(p.streamID(), m.ID, i), Mimetype: mediaType})
	}
	return out, true
}

// imageTypeOf is the media type an image URL's extension names, or "".
func imageTypeOf(location string) string {
	u, err := url.Parse(location)
	if err != nil || location == "" {
		return ""
	}
	return mediaTypeOf(mime.TypeByExtension(strings.ToLower(path.Ext(u.Path))))
}

func mediaTypeOf(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
}

// attachmentFileID names an attachment for the attachment workers: the
// channel and message it is on, and its own ID.
func attachmentFileID(channelID, messageID, attachmentID string) string {
	return channelID + "/" + messageID + "/" + attachmentID
}

// embedFileID names the image of a message's index'th embed.
func embedFileID(channelID, messageID string, index int) string {
	return channelID + "/" + messageID + "/embed" + strconv.Itoa(index)
}

// embedImage is the proxied image of an image embed, or of a link preview's
// image, when Discord serves one.
func embedImage(e embed) string {
	for _, media := range []*embedMedia{e.Image, e.Thumbnail} {
		if media != nil && media.ProxyURL != "" {
			return media.ProxyURL
		}
	}
	return ""
}

// renderBody is a message as one block of text: who it replies to, its
// words with mentions as names, what it forwards, its embeds, attachments,
// stickers, poll, and reactions.
func renderBody(m message, p place) string {
	var b strings.Builder
	line := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	if m.MessageReference != nil && m.MessageReference.Type == 0 && m.ReferencedMessage != nil {
		ref := m.ReferencedMessage
		line(fmt.Sprintf("Replying to %s: %q", ref.Author.displayName(), clip(resolveMentions(ref.Content, *ref, p), 200)))
	}
	line(resolveMentions(m.Content, m, p))
	for _, s := range m.MessageSnapshots {
		line("Forwarded message: " + resolveMentions(s.Message.Content, m, p))
		for _, a := range s.Message.Attachments {
			line("Forwarded attachment: " + a.Filename)
		}
	}
	for _, e := range m.Embeds {
		switch {
		case e.Title != "" && e.Description != "":
			line("Embed: " + e.Title + " — " + e.Description)
		case e.Title != "":
			line("Embed: " + e.Title)
		case e.Description != "":
			line("Embed: " + e.Description)
		}
	}
	if len(m.Attachments) > 0 {
		names := make([]string, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			name := a.Filename
			if a.Description != "" {
				name += " (" + a.Description + ")"
			}
			names = append(names, name)
		}
		line("Attachments: " + strings.Join(names, ", "))
	}
	for _, s := range m.StickerItems {
		line("Sticker: " + s.Name)
	}
	if m.Poll != nil {
		line("Poll: " + m.Poll.Question.Text)
	}
	if len(m.Reactions) > 0 {
		parts := make([]string, 0, len(m.Reactions))
		for _, r := range m.Reactions {
			name := r.Emoji.Name
			if r.Emoji.ID != "" {
				name = ":" + name + ":"
			}
			parts = append(parts, fmt.Sprintf("%s %d", name, r.Count))
		}
		line("Reactions: " + strings.Join(parts, " · "))
	}
	return b.String()
}

var (
	userMention    = regexp.MustCompile(`<@!?(\d+)>`)
	roleMention    = regexp.MustCompile(`<@&(\d+)>`)
	channelMention = regexp.MustCompile(`<#(\d+)>`)
	customEmoji    = regexp.MustCompile(`<a?:(\w+):\d+>`)
	timestampTag   = regexp.MustCompile(`<t:(-?\d+)(?::[tTdDfFR])?>`)
)

// resolveMentions writes a message's mentions as Discord shows them: users
// and roles by name, channels by name, custom emoji by name, and timestamps
// as dates. A mention it can't name keeps a plain form, never the raw tag.
func resolveMentions(text string, m message, p place) string {
	users := map[string]string{}
	for _, u := range m.Mentions {
		users[u.ID] = u.displayName()
	}
	text = userMention.ReplaceAllStringFunc(text, func(tag string) string {
		if name, ok := users[userMention.FindStringSubmatch(tag)[1]]; ok {
			return "@" + name
		}
		return "@unknown-user"
	})
	text = roleMention.ReplaceAllStringFunc(text, func(tag string) string {
		if name, ok := p.Roles[roleMention.FindStringSubmatch(tag)[1]]; ok {
			return "@" + name
		}
		return "@unknown-role"
	})
	text = channelMention.ReplaceAllStringFunc(text, func(tag string) string {
		if name, ok := p.Channels[channelMention.FindStringSubmatch(tag)[1]]; ok {
			return "#" + name
		}
		return "#unknown-channel"
	})
	text = customEmoji.ReplaceAllString(text, ":$1:")
	text = timestampTag.ReplaceAllStringFunc(text, func(tag string) string {
		seconds, err := strconv.ParseInt(timestampTag.FindStringSubmatch(tag)[1], 10, 64)
		if err != nil {
			return tag
		}
		return time.Unix(seconds, 0).UTC().Format("2006-01-02 15:04 UTC")
	})
	return text
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
