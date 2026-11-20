package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
)

var renderAt = time.Date(2026, 11, 3, 15, 4, 5, 0, time.UTC)

func renderPlace() place {
	return place{
		GuildID: fakeGuild, GuildName: "Example Server", ChannelID: fakeText, ChannelName: "general",
		Roles:    map[string]string{fakeRole: "maintainers"},
		Channels: map[string]string{fakeText: "general", fakeForum: "help"},
	}
}

func TestRenderMessage(t *testing.T) {
	id := fakeID(renderAt, 1)
	m := msg(id, alice, renderAt, "hey <@"+fakeBob+"> and <@!"+fakeBob+">, <@&"+fakeRole+"> see <#"+fakeForum+"> <:party:700000000000000001> at <t:1762182245:R>")
	m.Mentions = []user{bob}
	edited := renderAt.Add(time.Minute)
	m.EditedTimestamp = &edited
	m.Reactions = []reaction{{Count: 3, Emoji: emoji{Name: "👍"}}, {Count: 1, Emoji: emoji{ID: "700000000000000002", Name: "shipit"}}}
	out, ok := render(m, renderPlace(), events.ScopeInternal)
	require.True(t, ok)
	e := out.Event
	assert.Equal(t, "discord", e.Source)
	assert.Equal(t, fakeGuild+"/"+fakeText+"/"+id, e.ExternalID)
	assert.Equal(t, "https://discord.com/channels/"+fakeGuild+"/"+fakeText+"/"+id, e.URL)
	assert.Equal(t, "Alice Example", e.Actor)
	assert.Equal(t, fakeAlice, e.ActorID)
	assert.Equal(t, "discord:"+fakeAlice, e.ActorCanonical)
	assert.Equal(t, "#general", e.Title)
	assert.Empty(t, e.ThreadID)
	assert.Equal(t, events.ScopeInternal, e.Scope)
	assert.Equal(t, renderAt, e.OccurredAt)
	assert.Equal(t, "hey @bob and @bob, @maintainers see #help :party: at 2025-11-03 15:04 UTC\nReactions: 👍 3 · :shipit: 1", e.Body)
	assert.Contains(t, e.ContextPrefix, "Example Server")
}

func TestRenderUnknownMentionsNeverLeakTags(t *testing.T) {
	m := msg(fakeID(renderAt, 1), alice, renderAt, "<@300000000000000099> <@&400000000000000099> <#200000000000000099>")
	out, ok := render(m, renderPlace(), events.ScopeInternal)
	require.True(t, ok)
	assert.Equal(t, "@unknown-user @unknown-role #unknown-channel", out.Event.Body)
}

func TestRenderReplyForwardEmbedsAttachmentsStickersPoll(t *testing.T) {
	parentID := fakeID(renderAt, 1)
	parent := msg(parentID, bob, renderAt, "the deploy is "+strings.Repeat("very ", 60)+"slow")
	m := msg(fakeID(renderAt.Add(time.Minute), 2), alice, renderAt.Add(time.Minute), "agreed")
	m.Type = 19
	m.MessageReference = &messageReference{Type: 0, MessageID: parentID, ChannelID: fakeText}
	m.ReferencedMessage = &parent
	m.MessageSnapshots = []snapshot{{}}
	m.MessageSnapshots[0].Message.Content = "forwarded words"
	m.Embeds = []embed{{Type: "rich", Title: "Release notes", Description: "v1.2"}, {Type: "image", Thumbnail: &embedMedia{ProxyURL: "https://media.discordapp.net/external/x/pic.png"}}}
	m.Attachments = []attachment{
		{ID: "600000000000000001", Filename: "graph.png", ContentType: "image/png", Description: "latency graph"},
		{ID: "600000000000000002", Filename: "spec.pdf", ContentType: "application/pdf"},
		{ID: "600000000000000003", Filename: "notes.txt", ContentType: "text/plain; charset=utf-8"},
	}
	m.StickerItems = []sticker{{Name: "wave"}}
	m.Poll = &poll{}
	m.Poll.Question.Text = "Ship Friday?"
	out, ok := render(m, renderPlace(), events.ScopeInternal)
	require.True(t, ok)
	lines := strings.Split(out.Event.Body, "\n")
	require.Len(t, lines, 7)
	assert.True(t, strings.HasPrefix(lines[0], `Replying to bob: "the deploy is very`), lines[0])
	assert.True(t, strings.HasSuffix(lines[0], `…"`), "a long reply is cut: %s", lines[0])
	assert.Equal(t, "agreed", lines[1])
	assert.Equal(t, "Forwarded message: forwarded words", lines[2])
	assert.Equal(t, "Embed: Release notes — v1.2", lines[3])
	assert.Equal(t, "Attachments: graph.png (latency graph), spec.pdf, notes.txt", lines[4])
	assert.Equal(t, "Sticker: wave", lines[5])
	assert.Equal(t, "Poll: Ship Friday?", lines[6])

	stream := fakeText + "/" + m.ID + "/"
	assert.Equal(t, []events.ImageRef{
		{SourceImageID: stream + "600000000000000001", Mimetype: "image/png"},
		{SourceImageID: stream + "embed1", Mimetype: "image/png"},
	}, out.Images)
	assert.Equal(t, []events.DocumentRef{{SourceFileID: stream + "600000000000000002", Filename: "spec.pdf", Mimetype: "application/pdf", DocFormat: "pdf"}}, out.Documents)
	assert.NotContains(t, string(out.Event.Raw), "referenced_message\":{", "the replied-to message is its own event")
}

func TestRenderThreads(t *testing.T) {
	threadID := fakeID(renderAt, 1)
	// The message that started a thread in a text channel is its root.
	starter := msg(threadID, alice, renderAt, "should we migrate?")
	starter.Thread = &channel{ID: threadID, Type: channelPublicThread, Name: "migration"}
	out, ok := render(starter, renderPlace(), events.ScopeInternal)
	require.True(t, ok)
	assert.Equal(t, out.Event.ExternalID, out.Event.ThreadID)

	// A reply in the thread: its external ID keeps the channel, its link
	// the thread.
	p := renderPlace()
	p.ThreadID, p.ThreadName = threadID, "migration"
	reply := msg(fakeID(renderAt.Add(time.Hour), 2), bob, renderAt.Add(time.Hour), "yes")
	out, ok = render(reply, p, events.ScopeInternal)
	require.True(t, ok)
	assert.Equal(t, fakeGuild+"/"+fakeText+"/"+reply.ID, out.Event.ExternalID)
	assert.Equal(t, fakeGuild+"/"+fakeText+"/"+threadID, out.Event.ThreadID)
	assert.Equal(t, "https://discord.com/channels/"+fakeGuild+"/"+threadID+"/"+reply.ID, out.Event.URL)
	assert.Equal(t, "#general › migration", out.Event.Title)

	// A forum post's first message has the thread's ID: the root.
	forum := place{GuildID: fakeGuild, ChannelID: fakeForum, ChannelName: "help", ThreadID: threadID, ThreadName: "login broken"}
	out, ok = render(msg(threadID, alice, renderAt, "I can't log in"), forum, events.ScopeInternal)
	require.True(t, ok)
	assert.Equal(t, out.Event.ExternalID, out.Event.ThreadID)
	assert.Equal(t, "#help › login broken", out.Event.Title)

	// A private thread's starter is no root: private threads are not
	// indexed.
	starter.Thread.Type = channelPrivateThread
	out, ok = render(starter, renderPlace(), events.ScopeInternal)
	require.True(t, ok)
	assert.Empty(t, out.Event.ThreadID)
}

func TestRenderSkipsNoticesAndEmptyMessages(t *testing.T) {
	join := msg(fakeID(renderAt, 1), alice, renderAt, "")
	join.Type = 7
	_, ok := render(join, renderPlace(), events.ScopeInternal)
	assert.False(t, ok)
	pinned := msg(fakeID(renderAt, 2), alice, renderAt, "pinned a message")
	pinned.Type = 6
	_, ok = render(pinned, renderPlace(), events.ScopeInternal)
	assert.False(t, ok)
	_, ok = render(msg(fakeID(renderAt, 3), alice, renderAt, "  "), renderPlace(), events.ScopeInternal)
	assert.False(t, ok)
}

func TestSnowflakes(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	id := fakeID(at, 7)
	assert.Equal(t, at, snowflakeTime(id))
	assert.True(t, laterID(id, snowflakeAt(at)) || id == snowflakeAt(at))
	assert.False(t, laterID(snowflakeAt(at), id))
	assert.True(t, laterID("1000", "999"))
	assert.True(t, laterID("1", ""))
	assert.True(t, snowflakeTime("not a snowflake").IsZero())
}

func TestScopeFor(t *testing.T) {
	scope, err := ScopeFor(VisibilityEveryone, "")
	require.NoError(t, err)
	assert.Equal(t, events.ScopeInternal, scope)
	scope, err = ScopeFor(VisibilityPolicy, " corpus:discord-mods ")
	require.NoError(t, err)
	assert.Equal(t, "corpus:discord-mods", scope)
	for _, bad := range []string{"", "Mods", "a b", "*"} {
		_, err = ScopeFor(VisibilityPolicy, bad)
		assert.Error(t, err, bad)
	}
	_, err = ScopeFor("", "")
	assert.Error(t, err)
}
