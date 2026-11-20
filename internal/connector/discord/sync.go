package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

// recentWindow is how far back the daily re-read reaches.
const recentWindow = 7 * 24 * time.Hour

// readRetryAfter is how long a channel whose read of its history failed
// waits before it is read again. A variable so tests can shorten it.
var readRetryAfter = 5 * time.Minute

// threadInfo is what the reader knows of a thread.
type threadInfo struct {
	GuildID, ParentID, Name string
	Type                    int
}

// reader is the leader's whole view of Discord for one run: the directory,
// the chosen channels and how reading them stands, and the threads it has
// seen. Every write of a channel's messages runs under that channel's lock.
type reader struct {
	client *Client
	deps   source.Deps
	logger *slog.Logger
	// historyFrom is the oldest a first read goes (zero: all history).
	historyFrom time.Time

	mu         sync.Mutex
	dir        Directory
	selections map[string]Selection
	progress   map[string]Progress
	threads    map[string]threadInfo
	locks      map[string]*sync.Mutex
	// deleted are messages deleted live this run, and edited the edit
	// time last written of each message, so a slower read can't undo a
	// newer change.
	deleted map[string]bool
	edited  map[string]time.Time
	// users are the users seen or looked up this run, for mentions a
	// message does not name itself.
	users map[string]user
	// rereadAll is the last whole re-read an administrator asked for.
	rereadAll time.Time
	// self is the bot's user ID: its posts are not indexed.
	self string
}

func newReader(client *Client, deps source.Deps, historyDays int64) *reader {
	r := &reader{
		client: client, deps: deps, logger: deps.Logger,
		selections: map[string]Selection{}, progress: map[string]Progress{},
		threads: map[string]threadInfo{}, locks: map[string]*sync.Mutex{},
		deleted: map[string]bool{}, edited: map[string]time.Time{}, users: map[string]user{},
	}
	if historyDays > 0 {
		r.historyFrom = time.Now().Add(-time.Duration(historyDays) * 24 * time.Hour).UTC()
	}
	return r
}

// load reads the state the run starts from.
func (r *reader) load(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.deps.State.Get(ctx, directoryKey, &r.dir); err != nil {
		return err
	}
	if _, err := r.deps.State.Get(ctx, progressKey, &r.progress); err != nil {
		return err
	}
	if r.progress == nil {
		r.progress = map[string]Progress{}
	}
	for channelID, p := range r.progress {
		g, _, _ := r.dir.channel(channelID)
		for id, name := range p.Threads {
			r.threads[id] = threadInfo{GuildID: g.ID, ParentID: channelID, Name: name, Type: channelPublicThread}
		}
	}
	_, err := r.deps.State.Get(ctx, selectionKey, &r.selections)
	return err
}

// refreshDirectory reads the directory again and saves it.
func (r *reader) refreshDirectory(ctx context.Context) error {
	dir, err := readDirectory(ctx, r.client)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		failed := r.dir
		r.mu.Unlock()
		failed.Error = describeErr(err)
		if setErr := r.deps.State.Set(ctx, directoryKey, failed); setErr != nil {
			return errors.Join(err, setErr)
		}
		return fmt.Errorf("discord directory: %w", err)
	}
	r.mu.Lock()
	r.dir = dir
	r.mu.Unlock()
	return r.deps.State.Set(ctx, directoryKey, dir)
}

func (r *reader) lock(channelID string) func() {
	r.mu.Lock()
	l, ok := r.locks[channelID]
	if !ok {
		l = &sync.Mutex{}
		r.locks[channelID] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// placeOf is where a message in channelID (a channel or a thread) belongs,
// when it is in a chosen channel. It reads an unknown
// channel from Discord to learn whether it is a thread of one.
func (r *reader) placeOf(ctx context.Context, channelID string) (place, bool, error) {
	r.mu.Lock()
	info, known := r.threads[channelID]
	r.mu.Unlock()
	if !known {
		if _, selected := r.selection(channelID); !selected {
			_, _, inDirectory := r.directory().channel(channelID)
			if inDirectory {
				return place{}, false, nil
			}
			ch, err := r.client.channel(ctx, channelID)
			if err != nil {
				if isStatus(err, 403, 404) {
					return place{}, false, nil
				}
				return place{}, false, err
			}
			if !isThread(ch.Type) {
				return place{}, false, nil
			}
			info = r.rememberThread(ch)
			known = true
		}
	}
	top, threadID, threadName := channelID, "", ""
	if known {
		if info.Type == channelPrivateThread {
			return place{}, false, nil
		}
		top, threadID, threadName = info.ParentID, channelID, info.Name
	}
	selected, ok := r.selection(top)
	if !ok {
		return place{}, false, nil
	}
	return r.place(selected.GuildID, top, threadID, threadName), true, nil
}

func (r *reader) selection(channelID string) (Selection, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.selections[channelID]
	return s, ok && !s.Removing
}

func (r *reader) directory() Directory {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dir
}

// place builds the place of a message in a channel or one of its threads.
func (r *reader) place(guildID, channelID, threadID, threadName string) place {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := place{GuildID: guildID, ChannelID: channelID, ThreadID: threadID, ThreadName: threadName,
		Channels: map[string]string{}}
	for _, g := range r.dir.Guilds {
		if g.ID != guildID {
			continue
		}
		p.GuildName, p.Roles = g.Name, g.Roles
		for _, c := range g.Channels {
			p.Channels[c.ID] = c.Name
		}
	}
	for id, t := range r.threads {
		if t.GuildID == guildID {
			p.Channels[id] = t.Name
		}
	}
	p.ChannelName = p.Channels[channelID]
	if p.ChannelName == "" {
		p.ChannelName = r.selections[channelID].Name
	}
	return p
}

func (r *reader) rememberThread(ch channel) threadInfo {
	info := threadInfo{GuildID: ch.GuildID, ParentID: ch.ParentID, Name: ch.Name, Type: ch.Type}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.threads[ch.ID] = info
	if info.Type != channelPrivateThread {
		if p, ok := r.progress[ch.ParentID]; ok {
			if p.Threads == nil {
				p.Threads = map[string]string{}
			}
			p.Threads[ch.ID] = ch.Name
			r.progress[ch.ParentID] = p
		}
	}
	return info
}

// write indexes one message at p with its channel's scope, unless the
// channel is no longer chosen, the message was deleted live, or a newer edit
// of it was already written. A message with nothing to index takes its
// event out. The selection is read under the channel's lock, which a
// removal holds, so a read that began before it can't bring a removed
// channel back.
func (r *reader) write(ctx context.Context, m message, p place) error {
	unlock := r.lock(p.ChannelID)
	defer unlock()
	if _, chosen := r.selection(p.ChannelID); !chosen {
		return nil
	}
	scope := ChannelScope(p.ChannelID)
	r.mu.Lock()
	deleted := r.deleted[m.ID]
	last, seen := r.edited[m.ID]
	r.mu.Unlock()
	edit := m.Timestamp
	if m.EditedTimestamp != nil {
		edit = *m.EditedTimestamp
	}
	if deleted || (seen && edit.Before(last)) {
		return nil
	}
	m.Mentions = r.mentioned(ctx, m)
	out, ok := render(m, p, scope)
	if !ok || (r.self != "" && m.Author.ID == r.self) {
		r.mu.Lock()
		r.edited[m.ID] = edit
		r.mu.Unlock()
		return r.remove(ctx, externalID(p, m.ID))
	}
	result, err := r.deps.Sink.Upsert(ctx, out.Event)
	if err != nil {
		return err
	}
	if !result.NoChange || len(out.Images) > 0 || len(out.Documents) > 0 {
		if err := events.ReconcileImages(ctx, r.deps.Pool, result.EventID, out.Images); err != nil {
			return err
		}
		if err := events.ReconcileDocuments(ctx, r.deps.Pool, result.EventID, out.Documents); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.edited[m.ID] = edit
	r.mu.Unlock()
	return nil
}

// mentioned is the users a message's text mentions, with their names: the
// ones the message lists, and others (a mention that did not notify, as in a
// message sent with mentions turned off) from the users seen this run or
// read from Discord.
func (r *reader) mentioned(ctx context.Context, m message) []user {
	known := map[string]bool{}
	r.mu.Lock()
	for _, u := range append(slices.Clone(m.Mentions), m.Author) {
		if u.ID != "" {
			r.users[u.ID] = u
		}
	}
	r.mu.Unlock()
	out := slices.Clone(m.Mentions)
	for _, u := range out {
		known[u.ID] = true
	}
	texts := []string{m.Content}
	if m.ReferencedMessage != nil {
		texts = append(texts, m.ReferencedMessage.Content)
		out = append(out, m.ReferencedMessage.Mentions...)
		for _, u := range m.ReferencedMessage.Mentions {
			known[u.ID] = true
		}
	}
	for _, s := range m.MessageSnapshots {
		texts = append(texts, s.Message.Content)
	}
	for _, text := range texts {
		for _, match := range userMention.FindAllStringSubmatch(text, -1) {
			id := match[1]
			if known[id] {
				continue
			}
			known[id] = true
			r.mu.Lock()
			u, ok := r.users[id]
			r.mu.Unlock()
			if !ok && r.client != nil {
				fetched, err := r.client.user(ctx, id)
				if err != nil {
					continue
				}
				u = fetched
				r.mu.Lock()
				r.users[id] = u
				r.mu.Unlock()
			}
			if u.ID != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// deleteLive takes out a message Discord says was deleted, and remembers
// it for the run.
func (r *reader) deleteLive(ctx context.Context, p place, messageID string) error {
	unlock := r.lock(p.ChannelID)
	defer unlock()
	r.mu.Lock()
	r.deleted[messageID] = true
	r.mu.Unlock()
	return r.remove(ctx, externalID(p, messageID))
}

// remove deletes one message's event with its attachments. The caller
// holds the channel's lock.
func (r *reader) remove(ctx context.Context, externalID string) error {
	result, err := r.deps.Sink.Delete(ctx, ConnectorName, externalID)
	if err != nil || !result.Existed {
		return err
	}
	if err := events.ReconcileImages(ctx, r.deps.Pool, result.EventID, nil); err != nil {
		return err
	}
	return events.ReconcileDocuments(ctx, r.deps.Pool, result.EventID, nil)
}

// stream is one channel or thread whose messages are read.
type stream struct {
	ID         string
	ThreadID   string
	ThreadName string
}

// streams lists what to read of a chosen channel: its own history unless it
// is a forum, and its public threads, active and archived since from.
func (r *reader) streams(ctx context.Context, guildID string, ch ChannelEntry, from time.Time) ([]stream, error) {
	var out []stream
	if ch.Type != channelForum && ch.Type != channelMedia {
		out = append(out, stream{ID: ch.ID})
	}
	active, err := r.client.activeThreads(ctx, guildID)
	if err != nil {
		return nil, fmt.Errorf("active threads: %w", err)
	}
	seen := map[string]bool{}
	add := func(t channel) {
		if t.ParentID != ch.ID || t.Type == channelPrivateThread || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		t.GuildID = guildID
		r.rememberThread(t)
		out = append(out, stream{ID: t.ID, ThreadID: t.ID, ThreadName: t.Name})
	}
	for _, t := range active {
		add(t)
	}
	err = r.client.archivedPublicThreads(ctx, ch.ID, func(page []channel) (bool, error) {
		for _, t := range page {
			if !from.IsZero() && t.ThreadMetadata != nil {
				if archived, err := time.Parse(time.RFC3339, t.ThreadMetadata.ArchiveTimestamp); err == nil && archived.Before(from) {
					// Threads come most recently archived first, and a
					// thread archived before from has nothing newer.
					return false, nil
				}
			}
			add(t)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("archived threads: %w", err)
	}
	return out, nil
}

// readChannel reads a chosen channel's messages from from (zero: all of
// them) to now, writing each. With a complete read it deletes the indexed
// messages in that window it did not see, and it returns the cursors of the
// streams it read.
func (r *reader) readChannel(ctx context.Context, channelID string, from time.Time) (map[string]string, error) {
	selected, ok := r.selection(channelID)
	if !ok {
		return nil, nil
	}
	_, ch, ok := r.directory().channel(channelID)
	if !ok {
		return nil, errors.New("the bot can no longer see this channel")
	}
	if !ch.Readable {
		return nil, errors.New("the bot is not allowed to read this channel's history (it needs View Channel and Read Message History)")
	}
	started := time.Now().UTC()
	streams, err := r.streams(ctx, selected.GuildID, ch, from)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	cursors := map[string]string{}
	for _, s := range streams {
		p := r.place(selected.GuildID, channelID, s.ThreadID, s.ThreadName)
		err := r.client.messagesBefore(ctx, s.ID, "", func(page []message) (bool, error) {
			for _, m := range page {
				if !from.IsZero() && snowflakeTime(m.ID).Before(from) {
					return false, nil
				}
				if laterID(m.ID, cursors[s.ID]) {
					cursors[s.ID] = m.ID
				}
				seen[externalID(p, m.ID)] = true
				m.GuildID = selected.GuildID
				if err := r.write(ctx, m, p); err != nil {
					return false, err
				}
			}
			return true, nil
		})
		if err != nil {
			if s.ThreadID != "" && isStatus(err, 404) {
				// A thread deleted mid-read is not a failure of the
				// channel's read: its messages are gone too. One the bot
				// can no longer read fails the read, so the sweep can't
				// delete what it did not see.
				continue
			}
			return nil, fmt.Errorf("read %s: %w", s.ID, err)
		}
	}
	if err := r.sweep(ctx, selected.GuildID, channelID, from, started, seen); err != nil {
		return nil, err
	}
	return cursors, nil
}

// sweep deletes the indexed messages of a channel created in [from,
// before) that a complete read did not see.
func (r *reader) sweep(ctx context.Context, guildID, channelID string, from, before time.Time, seen map[string]bool) error {
	indexed, err := r.indexed(ctx, guildID, channelID)
	if err != nil {
		return err
	}
	unlock := r.lock(channelID)
	defer unlock()
	for _, id := range indexed {
		if seen[id] {
			continue
		}
		created := snowflakeTime(id[strings.LastIndexByte(id, '/')+1:])
		if created.IsZero() || created.Before(from) || !created.Before(before) {
			continue
		}
		if err := r.remove(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// indexed lists the external IDs of a channel's indexed messages.
func (r *reader) indexed(ctx context.Context, guildID, channelID string) ([]string, error) {
	rows, err := r.deps.Pool.Query(ctx, `
		SELECT external_id FROM events
		 WHERE source = $1 AND starts_with(external_id, $2) AND NOT deleted`, ConnectorName, prefix(guildID, channelID))
	if err != nil {
		return nil, fmt.Errorf("discord: list indexed messages: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// count is how many messages of a channel are indexed.
func (r *reader) count(ctx context.Context, guildID, channelID string) (int64, error) {
	var n int64
	err := r.deps.Pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source = $1 AND starts_with(external_id, $2) AND NOT deleted`, ConnectorName, prefix(guildID, channelID)).Scan(&n)
	return n, err
}

// tail reads each stream of a chosen channel after its cursor, and the
// active threads it has not read yet.
func (r *reader) tail(ctx context.Context, channelID string) error {
	selected, ok := r.selection(channelID)
	if !ok {
		return nil
	}
	_, ch, ok := r.directory().channel(channelID)
	if !ok || !ch.Readable {
		return nil
	}
	r.mu.Lock()
	cursors := map[string]string{}
	for id, c := range r.progress[channelID].Cursors {
		cursors[id] = c
	}
	r.mu.Unlock()
	streams := []stream{}
	if ch.Type != channelForum && ch.Type != channelMedia {
		streams = append(streams, stream{ID: channelID})
	}
	active, err := r.client.activeThreads(ctx, selected.GuildID)
	if err != nil {
		return fmt.Errorf("active threads: %w", err)
	}
	for _, t := range active {
		if t.ParentID == channelID && t.Type != channelPrivateThread {
			t.GuildID = selected.GuildID
			r.rememberThread(t)
			streams = append(streams, stream{ID: t.ID, ThreadID: t.ID, ThreadName: t.Name})
		}
	}
	for _, s := range streams {
		p := r.place(selected.GuildID, channelID, s.ThreadID, s.ThreadName)
		after := cursors[s.ID]
		if after == "" {
			// A stream not read before: a thread started since the last
			// read. Read it whole, as far as the history limit.
			after = snowflakeAt(r.historyFrom)
		}
		err := r.client.messagesAfter(ctx, s.ID, after, func(page []message) error {
			for _, m := range page {
				m.GuildID = selected.GuildID
				if err := r.write(ctx, m, p); err != nil {
					return err
				}
				if laterID(m.ID, cursors[s.ID]) {
					cursors[s.ID] = m.ID
				}
			}
			return nil
		})
		if err != nil {
			if s.ThreadID != "" && isStatus(err, 403, 404) {
				continue
			}
			return fmt.Errorf("read %s: %w", s.ID, err)
		}
	}
	r.mu.Lock()
	p := r.progress[channelID]
	if p.Cursors == nil {
		p.Cursors = map[string]string{}
	}
	for id, c := range cursors {
		if laterID(c, p.Cursors[id]) {
			p.Cursors[id] = c
		}
	}
	now := time.Now().UTC()
	p.TailAt = &now
	p.TailError = ""
	r.progress[channelID] = p
	r.mu.Unlock()
	return nil
}

// saveProgress writes the progress for every replica's page.
func (r *reader) saveProgress(ctx context.Context) error {
	r.mu.Lock()
	snapshot := make(map[string]Progress, len(r.progress))
	for id, p := range r.progress {
		p.Cursors, p.Threads = maps.Clone(p.Cursors), maps.Clone(p.Threads)
		snapshot[id] = p
	}
	r.mu.Unlock()
	return r.deps.State.Set(ctx, progressKey, snapshot)
}

// control does what the administrators' changes ask: it takes out removed
// channels, re-stamps a channel whose visibility changed, and reads a
// channel's whole history when it is new or a re-read was asked for, one
// channel a pass.
func (r *reader) control(ctx context.Context) error {
	var selections map[string]Selection
	if _, err := r.deps.State.Get(ctx, selectionKey, &selections); err != nil {
		return err
	}
	var rereadAll time.Time
	if _, err := r.deps.State.Get(ctx, rereadAllKey, &rereadAll); err != nil {
		return err
	}
	r.mu.Lock()
	r.selections = selections
	r.rereadAll = rereadAll
	r.mu.Unlock()

	var errs []error
	for id, s := range selections {
		if !s.Removing {
			continue
		}
		if err := r.drop(ctx, s.GuildID, id); err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range selectedIDs(selections) {
		if err := r.rescope(ctx, id, selections[id]); err != nil {
			errs = append(errs, err)
		}
	}
	if next := r.nextFullRead(selections, rereadAll); next != "" {
		errs = append(errs, r.fullRead(ctx, next))
	}
	if err := r.saveProgress(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// nextFullRead is a chosen channel whose whole history is due to be read:
// one never read, or read before a re-read was asked for.
func (r *reader) nextFullRead(selections map[string]Selection, rereadAll time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range selectedIDs(selections) {
		p, ok := r.progress[id]
		if p.ErrorAt != nil && time.Since(*p.ErrorAt) < readRetryAfter {
			continue
		}
		if !ok || p.FullReadAt == nil {
			return id
		}
		requested := rereadAll
		if at := selections[id].RereadAt; at != nil && at.After(requested) {
			requested = *at
		}
		if requested.After(*p.FullReadAt) {
			return id
		}
	}
	return ""
}

// fullRead reads one channel's whole history to the history limit.
func (r *reader) fullRead(ctx context.Context, channelID string) error {
	selected, _ := r.selection(channelID)
	r.logger.Info("discord: reading channel history", "channel", channelID, "from", r.historyFrom)
	cursors, err := r.readChannel(ctx, channelID, r.historyFrom)
	return r.record(ctx, channelID, selected, cursors, err, true)
}

// recentRead re-reads a channel's last week.
func (r *reader) recentRead(ctx context.Context, channelID string) error {
	selected, _ := r.selection(channelID)
	from := time.Now().Add(-recentWindow).UTC()
	if from.Before(r.historyFrom) {
		from = r.historyFrom
	}
	cursors, err := r.readChannel(ctx, channelID, from)
	return r.record(ctx, channelID, selected, cursors, err, false)
}

// record notes how a read of a channel went.
func (r *reader) record(ctx context.Context, channelID string, selected Selection, cursors map[string]string, readErr error, full bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	count, countErr := r.count(ctx, selected.GuildID, channelID)
	now := time.Now().UTC()
	r.mu.Lock()
	p := r.progress[channelID]
	if p.Scope == "" {
		p.Scope = ChannelScope(channelID)
	}
	if countErr == nil {
		p.Messages = count
	}
	if readErr != nil {
		p.Error, p.ErrorAt = describeErr(readErr), &now
	} else {
		p.Error, p.ErrorAt = "", nil
		if p.Cursors == nil {
			p.Cursors = map[string]string{}
		}
		for id, c := range cursors {
			if laterID(c, p.Cursors[id]) {
				p.Cursors[id] = c
			}
		}
		p.RecentReadAt = &now
		if full {
			p.FullReadAt = &now
			from := r.historyFrom
			if !from.IsZero() {
				p.FullReadFrom = &from
			}
		}
	}
	r.progress[channelID] = p
	r.mu.Unlock()
	if readErr != nil {
		return fmt.Errorf("discord channel %s: %w", channelID, readErr)
	}
	return nil
}

// drop deletes a removed channel's messages, then its selection.
func (r *reader) drop(ctx context.Context, guildID, channelID string) error {
	unlock := r.lock(channelID)
	indexed, err := r.indexed(ctx, guildID, channelID)
	if err == nil {
		for _, id := range indexed {
			if err = r.remove(ctx, id); err != nil {
				break
			}
		}
	}
	unlock()
	if err != nil {
		return fmt.Errorf("discord: remove channel %s: %w", channelID, err)
	}
	r.mu.Lock()
	delete(r.progress, channelID)
	r.mu.Unlock()
	if err := r.saveProgress(ctx); err != nil {
		return err
	}
	r.logger.Info("discord: removed channel from the index", "channel", channelID, "messages", len(indexed))
	return forget(ctx, r.deps.State, channelID)
}

// rescope gives a channel's indexed messages the channel's own scope, in
// place: messages indexed when a channel's events carried its visibility
// scope instead.
func (r *reader) rescope(ctx context.Context, channelID string, selected Selection) error {
	scope := ChannelScope(channelID)
	r.mu.Lock()
	p, ok := r.progress[channelID]
	r.mu.Unlock()
	if !ok || p.Scope == scope || p.Scope == "" {
		return nil
	}
	unlock := r.lock(channelID)
	changed, err := r.deps.Sink.Rescope(ctx, ConnectorName, prefix(selected.GuildID, channelID), scope)
	unlock()
	if err != nil {
		return fmt.Errorf("discord: scope channel %s: %w", channelID, err)
	}
	r.mu.Lock()
	p = r.progress[channelID]
	p.Scope = scope
	r.progress[channelID] = p
	r.mu.Unlock()
	r.logger.Info("discord: scoped channel messages to the channel", "channel", channelID, "messages", changed)
	return nil
}

// tailAll tails every chosen channel that has been read whole, and gives a
// channel whose last re-read is a day old its daily re-read of the last
// week.
func (r *reader) tailAll(ctx context.Context) error {
	r.mu.Lock()
	var ready, recent []string
	for _, id := range selectedIDs(r.selections) {
		p := r.progress[id]
		if p.FullReadAt == nil {
			continue
		}
		ready = append(ready, id)
		if p.RecentReadAt == nil || time.Since(*p.RecentReadAt) > 24*time.Hour {
			recent = append(recent, id)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, id := range ready {
		err := r.tail(ctx, id)
		if err == nil && slices.Contains(recent, id) {
			// After the tail has read from each stream's cursor, so a gap
			// older than the re-read's week is not skipped.
			errs = append(errs, r.recentRead(ctx, id))
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("discord channel %s: %w", id, err))
			r.mu.Lock()
			p := r.progress[id]
			p.TailError = describeErr(err)
			r.progress[id] = p
			r.mu.Unlock()
		}
	}
	errs = append(errs, r.saveProgress(ctx))
	return errors.Join(errs...)
}
