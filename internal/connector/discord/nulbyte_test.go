package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNulByteMessageDoesNotStallChannelHistory pins that a message Postgres
// cannot store as given does not stall a Discord channel's history read.
//
// Trigger: a message in a channel's history contains a literal NUL byte
// (U+0000) in its content. Postgres text columns reject that byte
// (SQLSTATE 22021), and jsonb rejects the \u0000 escape (SQLSTATE 22P05)
// that any message field containing it would be marshalled into for the
// Raw column.
//
// Contract: internal/connector/slack/sync_worker.go states the shape
// every connector's sync loop is meant to have — "Per-message failure:
// count + continue... the cursor advances regardless." Discord's
// reader.write (sync.go) returns the sink's error unchanged, the page
// callback aborts the whole historyPage walk on it, and reader.record
// withholds progress.Cursors/FullReadAt on that path — so if the sink
// rejected one poisoned message, the channel would never finish its full
// history read, on any retry, because Discord message history never
// changes and the read restarts from "now" every time.
//
// The guarantee is at the sink (internal/events), not in Discord's
// retry/error-handling code: DBSink.Upsert sanitizes NUL bytes and invalid
// UTF-8 out of every text field and the raw JSON document before writing,
// so the poisoned message is stored successfully instead of erroring at
// all, and Discord's page walk simply keeps going.
func TestNulByteMessageDoesNotStallChannelHistory(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	deps := testDeps(t)

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	oldest := msg(fakeID(base, 1), alice, base, "the oldest message, valid")
	poison := msg(fakeID(base.Add(time.Minute), 1), alice, base.Add(time.Minute), "poisoned\x00message")
	newest := msg(fakeID(base.Add(2*time.Minute), 1), alice, base.Add(2*time.Minute), "the newest message, valid")
	f.addMessage(fakeText, oldest)
	f.addMessage(fakeText, poison)
	f.addMessage(fakeText, newest)

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

	// The newer message is written first (the page callback walks
	// newest-first), with content unchanged — it has no dirty bytes.
	eventually(t, "the newer message", func() bool {
		return indexedEvents(t, deps)[newest.ID].Body == "the newest message, valid"
	})

	// The poisoned message itself is stored, with the NUL byte stripped:
	// the sink made it storable instead of the connector skipping it.
	eventually(t, "the poisoned message, sanitized", func() bool {
		return indexedEvents(t, deps)[poison.ID].Body == "poisonedmessage"
	})

	// The actual contract: the message strictly OLDER than the poisoned
	// one — already fetched from Discord in the same page as the two
	// above — gets indexed too. Before the fix this never happened: the
	// page callback aborted on the poisoned message before reaching it.
	eventually(t, "the older message, despite a later poison", func() bool {
		return indexedEvents(t, deps)[oldest.ID].Body == "the oldest message, valid"
	})
}

// TestNulByteOnlyContentMessageIsSkippedNotFailed pins that a message
// whose entire content is a NUL byte is skipped. Such content is not
// whitespace per strings.TrimSpace, so a bare
// `strings.TrimSpace(body) == ""` check treats it as "something to
// index." Sink.Upsert then sanitizes Body to "" and fails validate()'s
// "Body is required" check on every retry — forever, since the message's
// content never changes — which stalls the channel's full-history read
// exactly as an unsanitized sink would, just one layer further down
// (render's guard, not the sink itself).
//
// render checks strings.TrimSpace(events.SanitizeText(body)) instead, so
// a NUL-only message is recognized as nothing to index and
// taken out via r.remove — same as a message with genuinely empty
// content — instead of ever reaching Sink.Upsert.
func TestNulByteOnlyContentMessageIsSkippedNotFailed(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	deps := testDeps(t)

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	oldest := msg(fakeID(base, 1), alice, base, "the oldest message, valid")
	poison := msg(fakeID(base.Add(time.Minute), 1), alice, base.Add(time.Minute), "\x00")
	newest := msg(fakeID(base.Add(2*time.Minute), 1), alice, base.Add(2*time.Minute), "the newest message, valid")
	f.addMessage(fakeText, oldest)
	f.addMessage(fakeText, poison)
	f.addMessage(fakeText, newest)

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

	eventually(t, "the newer message", func() bool {
		return indexedEvents(t, deps)[newest.ID].Body == "the newest message, valid"
	})

	// The actual contract: the strictly older message, in the same
	// already-fetched page as the NUL-only message, still gets indexed.
	// Before the fix this timed out: the NUL-only message made every
	// read attempt fail validate() on every retry.
	eventually(t, "the older message, despite a later NUL-only message", func() bool {
		return indexedEvents(t, deps)[oldest.ID].Body == "the oldest message, valid"
	})

	// The NUL-only message itself must not appear as an event: it was
	// skipped (nothing to index), not written with an empty body.
	time.Sleep(200 * time.Millisecond)
	if _, ok := indexedEvents(t, deps)[poison.ID]; ok {
		t.Errorf("NUL-only message %s was indexed; want it skipped (render has nothing to index)", poison.ID)
	}
}

// TestNulByteAttachmentFilenameDoesNotStallChannelHistory pins that
// ReconcileDocuments sanitizes DocumentRef.Filename before writing
// event_documents.filename. reader.write (sync.go) calls
// ReconcileDocuments right after Sink.Upsert and returns its error
// unchanged, which the page callback treats as fatal — so an unsanitized
// attachment filename with a NUL byte would stall the channel exactly as
// an unsanitized message body would.
func TestNulByteAttachmentFilenameDoesNotStallChannelHistory(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	deps := testDeps(t)

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	oldest := msg(fakeID(base, 1), alice, base, "the oldest message, valid")
	poison := msg(fakeID(base.Add(time.Minute), 1), alice, base.Add(time.Minute), "a message with a poisoned attachment")
	poison.Attachments = []attachment{{
		ID: "900000000000000001", Filename: "report\x00.pdf", ContentType: "application/pdf", URL: "https://example.com/report.pdf",
	}}
	newest := msg(fakeID(base.Add(2*time.Minute), 1), alice, base.Add(2*time.Minute), "the newest message, valid")
	f.addMessage(fakeText, oldest)
	f.addMessage(fakeText, poison)
	f.addMessage(fakeText, newest)

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

	eventually(t, "the newer message", func() bool {
		return indexedEvents(t, deps)[newest.ID].Body == "the newest message, valid"
	})

	eventually(t, "the poisoned-attachment message", func() bool {
		return indexedEvents(t, deps)[poison.ID].Body == "a message with a poisoned attachment\nAttachments: report.pdf"
	})

	// The actual contract: the strictly older message, in the same
	// already-fetched page as the poisoned attachment, still gets
	// indexed. Before the fix this timed out: ReconcileDocuments failed
	// on the attachment filename's NUL byte on every retry.
	eventually(t, "the older message, despite a later poisoned attachment", func() bool {
		return indexedEvents(t, deps)[oldest.ID].Body == "the oldest message, valid"
	})

	// The document row exists, with the filename sanitized.
	var filename string
	eventually(t, "the attachment's filename, sanitized", func() bool {
		return deps.Pool.QueryRow(t.Context(), `
			SELECT d.filename FROM event_documents d JOIN events e ON e.id = d.event_id
			 WHERE e.source = 'discord' AND e.external_id LIKE '%`+poison.ID+`'
		`).Scan(&filename) == nil && filename == "report.pdf"
	})
}

// TestNulByteThreadNameDoesNotStallSavedProgressOrOtherChannelRemoval pins
// that a Discord-supplied name (here a thread's) is sanitized on its way to
// source_state.value (jsonb) through State.Set/Update — a path the sink's
// own sanitizing (which covers events.DBSink and
// ReconcileDocuments/ReconcileImages) does not reach.
//
// rememberThread (sync.go) copies a thread's name, verbatim, into
// Progress.Threads, and saveProgress persists every selected channel's
// Progress as ONE jsonb value (source_state, key "progress"). A NUL byte in
// any one channel's remembered thread name therefore made every saveProgress
// of the WHOLE map fail (SQLSTATE 22P05), for every selected channel,
// forever — not just the thread's own channel. Two symptoms, both
// reproduced here:
//
//   - The poisoned channel's FullReadAt (and its thread's name) never reach
//     source_state, where Overview — every replica's page, and tailAll's own
//     eligibility check — reads them: the admin page shows a channel stuck
//     "reading" forever even once Discord's history is fully walked in
//     memory.
//   - A DIFFERENT, clean channel's removal can never complete. drop()
//     (sync.go) removes the channel's indexed messages, then calls
//     saveProgress, and only calls forget() (which drops its Selection) if
//     that succeeds. Since saveProgress writes the combined map, the other
//     channel's poisoned thread name blocks it too, so the removed channel's
//     Selection stays Removing=true forever.
func TestNulByteThreadNameDoesNotStallSavedProgressOrOtherChannelRemoval(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	deps := testDeps(t)

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	f.addMessage(fakeText, msg(fakeID(base, 1), alice, base, "pin the toolchain version"))
	threadID := fakeID(base.Add(time.Minute), 1)
	f.addThread(channel{ID: threadID, Type: channelPublicThread, GuildID: fakeGuild, ParentID: fakeText, Name: "tool\x00chain"})
	f.addMessage(threadID, msg(fakeID(base.Add(2*time.Minute), 1), bob, base.Add(2*time.Minute), "yes, pin it to 1.27"))
	f.addMessage(fakePrivate, msg(fakeID(base.Add(3*time.Minute), 1), alice, base.Add(3*time.Minute), "staff note"))

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
	_, err = channels.Add(t.Context(), fakePrivate, VisibilityEveryone, "", true, "admin@example.com")
	require.NoError(t, err)

	// The messages (and the thread reply) are indexed in Postgres
	// regardless of the poisoned name: writing events.raw never depended
	// on source_state.
	eventually(t, "the thread reply", func() bool {
		return indexedEvents(t, deps)[fakeID(base.Add(2*time.Minute), 1)].Body == "yes, pin it to 1.27"
	})

	// The actual contract: the poisoned thread name must not stop the
	// channel's own progress — FullReadAt and the thread name itself —
	// from ever reaching source_state, where Overview (every replica's
	// page) reads them.
	eventually(t, "the channel's FullReadAt, saved", func() bool {
		o, err := channels.Overview(t.Context())
		return err == nil && o.Progress[fakeText].FullReadAt != nil
	})
	// The thread name is only remembered into Progress.Threads once the
	// channel has a Progress entry (rememberThread requires one to already
	// exist), so this is populated by a later tail/recentRead pass, not
	// the first full read itself. Poll for it rather than reading Overview
	// once: before the fix, this saveProgress failed on every later tick
	// too, so this would time out instead of ever showing the sanitized
	// name.
	eventually(t, "the saved thread name, sanitized", func() bool {
		o, err := channels.Overview(t.Context())
		return err == nil && o.Progress[fakeText].Threads[threadID] == "toolchain"
	})

	// The actual contract, part two: a DIFFERENT channel's removal must
	// still complete. Before the fix, every saveProgress — including the
	// one drop() makes before it ever calls forget() — failed on
	// fakeText's poisoned thread name, so fakePrivate's selection stayed
	// Removing=true forever.
	removed, err := channels.Remove(t.Context(), fakePrivate)
	require.NoError(t, err)
	require.True(t, removed)
	eventually(t, "the other channel's removal completing", func() bool {
		o, err := channels.Overview(t.Context())
		if err != nil {
			return false
		}
		_, stillThere := o.Selections[fakePrivate]
		return !stillThere
	})
}

// TestNulByteCategoryNameDoesNotStallDirectoryOrChannelSelection pins that
// refreshDirectory (sync.go) sanitizes names: an unsanitized NUL byte in
// any guild, role, category or channel name makes every refreshDirectory
// fail the same way (SQLSTATE 22P05, source_state key "directory"), so the
// directory Overview shows is never updated again. On
// a fresh install this means no channel can ever be chosen, because
// Channels.Add (state.go) looks the channel up in that same saved
// directory.
func TestNulByteCategoryNameDoesNotStallDirectoryOrChannelSelection(t *testing.T) {
	shortIntervals(t)
	f := newFakeDiscord(t)
	deps := testDeps(t)

	const poisonedCategory, newChannel = "200000000000000006", "200000000000000007"
	f.mu.Lock()
	f.channels = append(f.channels,
		channel{ID: poisonedCategory, Type: channelCategory, Name: "Proj\x00ects"},
		channel{ID: newChannel, Type: channelText, Name: "incidents", ParentID: poisonedCategory, Position: 4},
	)
	f.mu.Unlock()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	incidentID := fakeID(base, 1)
	f.addMessage(newChannel, msg(incidentID, alice, base, "a new incident"))

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
	// The actual contract: the directory must still reach source_state
	// despite the poisoned category name, and must list the channel under
	// it. Before the fix, every refreshDirectory failed on the category's
	// NUL byte, so the saved directory stayed empty forever.
	eventually(t, "the directory, including the channel under the poisoned category", func() bool {
		o, err := channels.Overview(t.Context())
		if err != nil || len(o.Directory.Guilds) != 1 {
			return false
		}
		_, ch, ok := o.Directory.channel(newChannel)
		return ok && ch.Name == "incidents"
	})

	// The category name itself is saved, sanitized.
	o, err := channels.Overview(t.Context())
	require.NoError(t, err)
	_, ch, ok := o.Directory.channel(newChannel)
	require.True(t, ok)
	if ch.Category != "Projects" {
		t.Errorf("category = %q, want %q (NUL stripped)", ch.Category, "Projects")
	}

	// The second half of the contract: a channel under the poisoned
	// category can actually be chosen. Before the fix, Channels.Add always
	// failed with "the bot can't see that channel" because the directory
	// it reads was never saved.
	_, err = channels.Add(t.Context(), newChannel, VisibilityEveryone, "", false, "admin@example.com")
	require.NoError(t, err)
	eventually(t, "the new channel's message", func() bool {
		return indexedEvents(t, deps)[incidentID].Body == "a new incident"
	})
}
