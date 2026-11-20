package discord

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/attachments/docs"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/bluesky-social/diderot/internal/worker"
)

// lentFiles records what Run lends the attachment workers.
type lentFiles struct {
	mu      sync.Mutex
	fetcher files.SourceFetcher
}

func (l *lentFiles) LendImages(_ context.Context, fetcher files.SourceFetcher) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fetcher = fetcher
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.fetcher = nil
	}
}

func (*lentFiles) AddDocumentWorker(*worker.Supervisor, string, docs.GateChecker, files.Fetcher) func() {
	return func() {}
}

func testDeps(t *testing.T) source.Deps {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.URL(t, "discord"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `TRUNCATE events, source_state RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return source.Deps{
		Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()),
		Logger: slog.New(slog.DiscardHandler),
	}.For(ConnectorName)
}

type indexedEvent struct {
	ExternalID, ThreadID, Title, Body, Scope, URL, Actor string
}

func indexedEvents(t *testing.T, deps source.Deps) map[string]indexedEvent {
	t.Helper()
	rows, err := deps.Pool.Query(t.Context(), `
		SELECT external_id, COALESCE(thread_id, ''), COALESCE(title, ''), body, scope, COALESCE(url, ''), COALESCE(actor, '')
		  FROM events WHERE source = 'discord' AND NOT deleted`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]indexedEvent{}
	for rows.Next() {
		var e indexedEvent
		require.NoError(t, rows.Scan(&e.ExternalID, &e.ThreadID, &e.Title, &e.Body, &e.Scope, &e.URL, &e.Actor))
		out[e.ExternalID[strings.LastIndexByte(e.ExternalID, '/')+1:]] = e
	}
	require.NoError(t, rows.Err())
	return out
}

func shortIntervals(t *testing.T) {
	control, tail, directory, retry := controlInterval, tailInterval, directoryInterval, readRetryAfter
	controlInterval, tailInterval, directoryInterval, readRetryAfter = 100*time.Millisecond, 300*time.Millisecond, time.Hour, time.Second
	t.Cleanup(func() {
		controlInterval, tailInterval, directoryInterval, readRetryAfter = control, tail, directory, retry
	})
}

// seedServer gives the fake a channel with a reply, an image and a thread;
// a forum with an archived post; a private channel; and a notice to skip.
type seeded struct {
	first, reply, image, starter, threadReply, forumPost, forumReply, staff string
	threadID                                                                string
}

func seedServer(f *fakeDiscord) seeded {
	base := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Millisecond)
	s := seeded{
		first: fakeID(base, 1), reply: fakeID(base.Add(time.Minute), 1), image: fakeID(base.Add(2*time.Minute), 1),
		starter: fakeID(base.Add(3*time.Minute), 1), threadReply: fakeID(base.Add(4*time.Minute), 1),
		forumPost: fakeID(base.Add(5*time.Minute), 1), forumReply: fakeID(base.Add(6*time.Minute), 1),
		staff: fakeID(base.Add(7*time.Minute), 1),
	}
	s.threadID = s.starter
	first := msg(s.first, alice, base, "the build cache is cold after deploys")
	f.addMessage(fakeText, first)
	reply := msg(s.reply, bob, base.Add(time.Minute), "warm it with the prefetch job")
	reply.Type = 19
	reply.MessageReference = &messageReference{MessageID: s.first, ChannelID: fakeText}
	reply.ReferencedMessage = &first
	f.addMessage(fakeText, reply)
	image := msg(s.image, alice, base.Add(2*time.Minute), "graph attached")
	image.Attachments = []attachment{{ID: "600000000000000001", Filename: "graph.png", ContentType: "image/png", Size: 10}}
	f.addMessage(fakeText, image)
	starter := msg(s.starter, bob, base.Add(3*time.Minute), "should we pin the toolchain?")
	starter.Thread = &channel{ID: s.threadID, Type: channelPublicThread, Name: "toolchain"}
	f.addMessage(fakeText, starter)
	notice := msg(fakeID(base.Add(3*time.Minute), 2), bob, base.Add(3*time.Minute), "")
	notice.Type = 18
	f.addMessage(fakeText, notice)
	f.addThread(channel{ID: s.threadID, Type: channelPublicThread, GuildID: fakeGuild, ParentID: fakeText, Name: "toolchain"})
	f.addMessage(s.threadID, msg(s.threadReply, alice, base.Add(4*time.Minute), "yes, pin it to 1.27"))

	postID := s.forumPost
	f.addThread(channel{ID: postID, Type: channelPublicThread, GuildID: fakeGuild, ParentID: fakeForum, Name: "login broken",
		ThreadMetadata: &threadMetadata{Archived: true, ArchiveTimestamp: base.Add(time.Hour).Format(time.RFC3339)}})
	f.addMessage(postID, msg(s.forumPost, alice, base.Add(5*time.Minute), "I can't sign in since Tuesday"))
	f.addMessage(postID, msg(s.forumReply, bob, base.Add(6*time.Minute), "fixed by clearing cookies"))
	f.addThread(channel{ID: "200000000000000077", Type: channelPrivateThread, GuildID: fakeGuild, ParentID: fakeText, Name: "secret"})
	f.addMessage("200000000000000077", msg(fakeID(base.Add(8*time.Minute), 1), alice, base, "private thread words"))
	f.addMessage(fakePrivate, msg(s.staff, alice, base.Add(7*time.Minute), "staff only"))
	return s
}

func TestRunIndexesChosenChannelsAndFollowsLiveEvents(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	s := seedServer(f)
	deps := testDeps(t)
	lent := &lentFiles{}
	deps.Files = lent
	c := Connector{APIBase: f.apiBase()}
	resolved := settings.NewResolved(ConnectorName, settings.Values{FieldHistoryDays: int64(0), FieldDocIngest: false},
		map[string]string{FieldBotToken: "test-token"})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, deps, resolved) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})

	channels := ChannelsFor(deps)
	eventually(t, "the directory", func() bool {
		o, err := channels.Overview(t.Context())
		return err == nil && len(o.Directory.Guilds) == 1
	})
	_, err := channels.Add(t.Context(), fakeText, VisibilityEveryone, "", false, "admin@example.com")
	require.NoError(t, err)
	_, err = channels.Add(t.Context(), fakeForum, VisibilityPolicy, "helpers", false, "admin@example.com")
	require.NoError(t, err)
	_, err = channels.Add(t.Context(), fakePrivate, VisibilityEveryone, "", false, "admin@example.com")
	require.ErrorIs(t, err, ErrPrivateNeedsChoice)
	_, err = channels.Add(t.Context(), fakePrivate, VisibilityPolicy, "staff", true, "admin@example.com")
	require.NoError(t, err)
	_, err = channels.Add(t.Context(), fakeVoice, VisibilityEveryone, "", false, "admin@example.com")
	require.Error(t, err, "a voice channel is not listed")

	eventually(t, "the backfill", func() bool { return len(indexedEvents(t, deps)) == 8 })
	got := indexedEvents(t, deps)
	assert.Equal(t, ChannelScope(fakeText), got[s.first].Scope)
	assert.Equal(t, "Alice Example", got[s.first].Actor)
	assert.Contains(t, got[s.reply].Body, `Replying to Alice Example: "the build cache is cold after deploys"`)
	assert.Equal(t, got[s.starter].ExternalID, got[s.starter].ThreadID, "the thread's starter is its root")
	assert.Equal(t, got[s.starter].ExternalID, got[s.threadReply].ThreadID)
	assert.Equal(t, "#general › toolchain", got[s.threadReply].Title)
	assert.Equal(t, ChannelScope(fakeForum), got[s.forumPost].Scope)
	assert.Equal(t, got[s.forumPost].ExternalID, got[s.forumReply].ThreadID)
	assert.Equal(t, "#help › login broken", got[s.forumPost].Title)
	assert.Equal(t, ChannelScope(fakePrivate), got[s.staff].Scope)
	// A channel's visibility grants its scope to the readers it names.
	granted, err := c.GrantScopes(t.Context(), deps, []string{events.ScopeInternal, "corpus:staff"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ChannelScope(fakeText), ChannelScope(fakePrivate)}, granted)
	for _, e := range got {
		assert.NotContains(t, e.Body, "private thread words")
	}
	var images int
	require.NoError(t, deps.Pool.QueryRow(t.Context(), `SELECT count(*) FROM event_images WHERE source_image_id = $1`,
		fakeText+"/"+s.image+"/600000000000000001").Scan(&images))
	assert.Equal(t, 1, images)

	// The lent fetcher reads the message again for a fresh link.
	lent.mu.Lock()
	fetcher := lent.fetcher.(*FileFetcher)
	lent.mu.Unlock()
	fetcher.allowed = func(u *url.URL) bool { return u.Host == strings.TrimPrefix(f.server.URL, "http://") }
	fetcher.http = http.DefaultClient
	data, mediaType, err := fetcher.FetchImage(t.Context(), fakeText+"/"+s.image+"/600000000000000001", "image/png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", mediaType)
	assert.Contains(t, string(data), "fake /files/600000000000000001")
	_, _, err = fetcher.FetchImage(t.Context(), fakeText+"/"+s.image+"/600000000000000099", "image/png")
	var permanent *files.PermanentError
	assert.ErrorAs(t, err, &permanent)
	fetcher.allowed = discordFileHost
	_, _, err = fetcher.FetchImage(t.Context(), fakeText+"/"+s.image+"/600000000000000001", "image/png")
	assert.ErrorAs(t, err, &permanent, "a link off Discord's hosts is refused")

	// Live events.
	f.nextConn(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	live := msg(fakeID(now, 1), bob, now, "deploy finished")
	f.addMessage(fakeText, live)
	live.GuildID = fakeGuild
	f.dispatch("MESSAGE_CREATE", live)
	eventually(t, "a live message", func() bool { return indexedEvents(t, deps)[live.ID].Body == "deploy finished" })

	edited := f.editMessage(fakeText, live.ID, func(m *message) {
		m.Content = "deploy finished at noon"
		at := now.Add(time.Minute)
		m.EditedTimestamp = &at
	})
	edited.GuildID = fakeGuild
	f.dispatch("MESSAGE_UPDATE", edited)
	eventually(t, "an edit", func() bool { return indexedEvents(t, deps)[live.ID].Body == "deploy finished at noon" })

	f.editMessage(fakeText, s.first, func(m *message) { m.Reactions = []reaction{{Count: 2, Emoji: emoji{Name: "🔥"}}} })
	f.dispatch("MESSAGE_REACTION_ADD", map[string]any{"guild_id": fakeGuild, "channel_id": fakeText, "message_id": s.first, "user_id": fakeBob, "emoji": map[string]any{"name": "🔥"}})
	eventually(t, "a reaction count", func() bool { return strings.Contains(indexedEvents(t, deps)[s.first].Body, "Reactions: 🔥 2") })

	newThread := fakeID(now.Add(2*time.Second), 1)
	f.addThread(channel{ID: newThread, Type: channelPublicThread, GuildID: fakeGuild, ParentID: fakeText, Name: "follow-up"})
	threadMessage := msg(fakeID(now.Add(3*time.Second), 1), alice, now, "in the new thread")
	threadMessage.GuildID = fakeGuild
	threadMessage.ChannelID = newThread
	f.addMessage(newThread, threadMessage)
	f.dispatch("THREAD_CREATE", channel{ID: newThread, Type: channelPublicThread, GuildID: fakeGuild, ParentID: fakeText, Name: "follow-up"})
	f.dispatch("MESSAGE_CREATE", threadMessage)
	eventually(t, "a message in a new thread", func() bool {
		e := indexedEvents(t, deps)[threadMessage.ID]
		return e.Title == "#general › follow-up" && strings.HasSuffix(e.ThreadID, "/"+newThread)
	})

	f.deleteMessage(fakeText, live.ID)
	f.dispatch("MESSAGE_DELETE", map[string]any{"id": live.ID, "channel_id": fakeText, "guild_id": fakeGuild})
	eventually(t, "a delete", func() bool { _, ok := indexedEvents(t, deps)[live.ID]; return !ok })

	// A message posted while the gateway missed it arrives with the tail.
	missed := msg(fakeID(time.Now().UTC(), 3), alice, time.Now().UTC(), "posted during a gap")
	f.addMessage(fakeText, missed)
	eventually(t, "the tail", func() bool { return indexedEvents(t, deps)[missed.ID].Body == "posted during a gap" })

	// Changing a visibility changes who is granted the channel's scope,
	// at once; removing takes the messages out.
	_, err = channels.Add(t.Context(), fakeForum, VisibilityEveryone, "", false, "admin@example.com")
	require.NoError(t, err)
	granted, err = c.GrantScopes(t.Context(), deps, []string{events.ScopeInternal})
	require.NoError(t, err)
	assert.Contains(t, granted, ChannelScope(fakeForum))
	assert.Equal(t, ChannelScope(fakeForum), indexedEvents(t, deps)[s.forumReply].Scope)
	_, err = channels.Remove(t.Context(), fakePrivate)
	require.NoError(t, err)
	eventually(t, "a removal", func() bool { _, ok := indexedEvents(t, deps)[s.staff]; return !ok })
	eventually(t, "the removed selection", func() bool {
		o, err := channels.Overview(t.Context())
		_, still := o.Selections[fakePrivate]
		return err == nil && !still
	})

	// A failed re-read deletes nothing; a complete one deletes what is gone.
	f.deleteMessage(fakeText, s.reply)
	f.failHistory(fakeText, 403)
	_, err = channels.Reread(t.Context(), fakeText)
	require.NoError(t, err)
	eventually(t, "the failed re-read", func() bool {
		o, _ := channels.Overview(t.Context())
		return o.Progress[fakeText].Error != ""
	})
	_, ok := indexedEvents(t, deps)[s.reply]
	assert.True(t, ok, "a failed read deletes nothing")
	o, err := channels.Overview(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, o.Progress[fakeText].Error, "a good tail does not hide a failed read")
	eventually(t, "the next re-read", func() bool { _, ok := indexedEvents(t, deps)[s.reply]; return !ok })

	// A deleted forum post takes its first message with it; a text
	// channel's thread starter stays in its channel.
	f.dispatch("THREAD_DELETE", map[string]any{"id": s.forumPost, "guild_id": fakeGuild, "parent_id": fakeForum, "type": channelPublicThread})
	f.dispatch("THREAD_DELETE", map[string]any{"id": s.threadID, "guild_id": fakeGuild, "parent_id": fakeText, "type": channelPublicThread})
	eventually(t, "deleted threads", func() bool {
		e := indexedEvents(t, deps)
		_, post := e[s.forumPost]
		_, postReply := e[s.forumReply]
		_, threadReply := e[s.threadReply]
		return !post && !postReply && !threadReply
	})
	_, starter := indexedEvents(t, deps)[s.starter]
	assert.True(t, starter)

	// Status and channel finding read the state any replica sees.
	status, err := c.Status(t.Context(), deps)
	require.NoError(t, err)
	assert.Empty(t, status.Waiting)
	assert.Equal(t, "connected", status.Rows[0].Detail)
	for _, entry := range []string{"general", "#General", "https://discord.com/channels/" + fakeGuild + "/" + fakeText, s.threadID,
		"https://discord.com/channels/" + fakeGuild + "/" + s.threadID + "/" + s.threadReply} {
		found, err := c.FindChannels(t.Context(), deps, entry)
		require.NoError(t, err)
		assert.Equal(t, []source.Channel{{ExternalIDPrefix: prefix(fakeGuild, fakeText), Scope: ChannelScope(fakeText)}}, found, entry)
	}
	found, err := c.FindChannels(t.Context(), deps, "staff")
	require.NoError(t, err)
	assert.Empty(t, found, "a removed channel is not found")
}

// choose makes a reader's channel chosen with scope, as control does.
func choose(r *reader, channelID, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selections[channelID] = Selection{GuildID: fakeGuild, Scope: scope}
}

// TestReaderWritesWithTheChannelScope: a write takes the channel's own
// scope, whatever its visibility, and a channel no longer chosen when the
// write lands is not written.
func TestReaderWritesWithTheChannelScope(t *testing.T) {
	deps := testDeps(t)
	r := newReader(nil, deps, 0)
	p := place{GuildID: fakeGuild, ChannelID: fakeText, ChannelName: "general"}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	choose(r, fakeText, "corpus:mods")
	m := msg(fakeID(at, 1), alice, at, "words")
	require.NoError(t, r.write(t.Context(), m, p))
	assert.Equal(t, ChannelScope(fakeText), indexedEvents(t, deps)[m.ID].Scope)
	r.mu.Lock()
	r.selections[fakeText] = Selection{GuildID: fakeGuild, Scope: "corpus:mods", Removing: true}
	r.mu.Unlock()
	late := msg(fakeID(at, 2), alice, at, "a read that finished after the removal")
	require.NoError(t, r.write(t.Context(), late, p))
	_, written := indexedEvents(t, deps)[late.ID]
	assert.False(t, written)
}

func TestExpand(t *testing.T) {
	deps := testDeps(t)
	r := newReader(nil, deps, 0)
	choose(r, fakeText, events.ScopeInternal)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	p := place{GuildID: fakeGuild, GuildName: "Example Server", ChannelID: fakeText, ChannelName: "general"}
	var ids []string
	for i := range 40 {
		m := msg(fakeID(base.Add(time.Duration(i)*time.Minute), 1), alice, base.Add(time.Duration(i)*time.Minute), "channel message "+string(rune('A'+i%26)))
		require.NoError(t, r.write(t.Context(), m, p))
		ids = append(ids, m.ID)
	}
	threadID := ids[10]
	tp := p
	tp.ThreadID, tp.ThreadName = threadID, "a thread"
	reply := msg(fakeID(base.Add(5*time.Hour), 1), bob, base.Add(5*time.Hour), "a reply in the thread")
	require.NoError(t, r.write(t.Context(), reply, tp))
	hidden := msg(fakeID(base.Add(6*time.Hour), 1), bob, base.Add(6*time.Hour), "a reply nobody here may read")
	require.NoError(t, r.write(t.Context(), hidden, tp))
	_, err := deps.Pool.Exec(t.Context(), `UPDATE events SET scope = 'corpus:staff' WHERE external_id = $1`, prefix(fakeGuild, fakeText)+hidden.ID)
	require.NoError(t, err)
	// The thread's starter becomes its root once it knows its thread.
	starter := msg(threadID, alice, base.Add(10*time.Minute), "channel message K")
	starter.Thread = &channel{ID: threadID, Type: channelPublicThread}
	require.NoError(t, r.write(t.Context(), starter, p))

	eventID := func(id string) int64 {
		var n int64
		require.NoError(t, deps.Pool.QueryRow(t.Context(), `SELECT id FROM events WHERE external_id = $1`, prefix(fakeGuild, fakeText)+id).Scan(&n))
		return n
	}
	c := Connector{}
	out, err := c.Expand(t.Context(), deps, eventID(reply.ID), []string{ChannelScope(fakeText)})
	require.NoError(t, err)
	assert.Contains(t, out.Content, "The thread:")
	assert.Contains(t, out.Content, "channel message K")
	assert.Contains(t, out.Content, "a reply in the thread")
	assert.NotContains(t, out.Content, "nobody here may read")

	out, err = c.Expand(t.Context(), deps, eventID(ids[30]), []string{ChannelScope(fakeText)})
	require.NoError(t, err)
	assert.Contains(t, out.Content, "» [2026-11-01 12:30] Alice Example: channel message E")
	assert.Equal(t, 25, strings.Count(out.Content, "\n"), "a heading, 15 messages before it, and the 9 after it there are")
	assert.True(t, out.Truncated)

	_, err = c.Expand(t.Context(), deps, 999999, []string{events.ScopeInternal})
	assert.ErrorIs(t, err, events.ErrEventNotFound)
}

func TestReaderDropsStaleWrites(t *testing.T) {
	deps := testDeps(t)
	r := newReader(nil, deps, 0)
	choose(r, fakeText, events.ScopeInternal)
	p := place{GuildID: fakeGuild, ChannelID: fakeText, ChannelName: "general"}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := msg(fakeID(at, 1), alice, at, "first words")
	newer := m
	newer.Content = "edited words"
	edit := at.Add(time.Minute)
	newer.EditedTimestamp = &edit
	require.NoError(t, r.write(t.Context(), newer, p))
	require.NoError(t, r.write(t.Context(), m, p), "an older read arrives late")
	assert.Equal(t, "edited words", indexedEvents(t, deps)[m.ID].Body)

	require.NoError(t, r.deleteLive(t.Context(), p, m.ID))
	require.NoError(t, r.write(t.Context(), newer, p), "a slower read of a deleted message")
	_, ok := indexedEvents(t, deps)[m.ID]
	assert.False(t, ok)
}

// TestReaderNamesMentionsAMessageDoesNotList: a mention that did not
// notify (Discord leaves it out of the message's mentions) is named from
// Discord's user, and one Discord does not know stays unnamed.
func TestReaderNamesMentionsAMessageDoesNotList(t *testing.T) {
	f := newFakeDiscord(t)
	deps := testDeps(t)
	r := newReader(NewClient(f.apiBase(), "test-token"), deps, 0)
	choose(r, fakeText, events.ScopeInternal)
	p := place{GuildID: fakeGuild, ChannelID: fakeText, ChannelName: "general"}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := msg(fakeID(at, 1), alice, at, "ping <@"+fakeBob+"> and <@300000000000000099>")
	require.NoError(t, r.write(t.Context(), m, p))
	assert.Equal(t, "ping @bob and @unknown-user", indexedEvents(t, deps)[m.ID].Body)
	require.NoError(t, r.write(t.Context(), msg(fakeID(at, 2), alice, at, "again <@"+fakeBob+">"), p))
	assert.Equal(t, 1, f.requestCount("/users/"+fakeBob), "a user is read once a run")
}

// TestNextFullReadSkipsAChannelThatJustFailed: a channel whose read just
// failed waits its turn, so the channels after it get their first read or
// their requested re-read.
func TestNextFullReadSkipsAChannelThatJustFailed(t *testing.T) {
	r := newReader(nil, source.Deps{}, 0)
	read := time.Now().Add(-time.Hour)
	failed := time.Now()
	r.progress["200000000000000001"] = Progress{FullReadAt: &read, Error: "no access", ErrorAt: &failed}
	selections := map[string]Selection{
		"200000000000000001": {GuildID: fakeGuild, Scope: events.ScopeInternal},
		"200000000000000002": {GuildID: fakeGuild, Scope: events.ScopeInternal},
	}
	assert.Equal(t, "200000000000000002", r.nextFullRead(selections, time.Now()))
	earlier := time.Now().Add(-10 * time.Minute)
	r.progress["200000000000000001"] = Progress{FullReadAt: &read, Error: "no access", ErrorAt: &earlier}
	assert.Equal(t, "200000000000000001", r.nextFullRead(selections, time.Now()), "it is tried again after a while")
}
