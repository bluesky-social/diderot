package discord

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// reactionDelay is how long a message with a reaction change waits before
// it is read again for its counts, so a burst of reactions costs one read.
const reactionDelay = 3 * time.Second

// live applies the gateway's dispatches to the index.
type live struct {
	r *reader
	// ask answers in Discord; nil when Ask is off.
	ask *asker
	// refreshDirectory asks for the directory to be read again soon.
	refreshDirectory func()

	mu sync.Mutex
	// reacted are the messages, by channel and ID, with reaction changes
	// waiting for a read.
	reacted map[[2]string]bool
	// lastEvent is when the last dispatch's time was saved.
	lastEvent time.Time
}

// handle applies one dispatch. An error is logged by the caller; the next
// tail or re-read repairs what it missed.
func (l *live) handle(ctx context.Context, d dispatch) error {
	if l.ask != nil {
		l.ask.handle(ctx, d)
	}
	switch d.Type {
	case "GUILD_CREATE":
		var g struct {
			ID      string    `json:"id"`
			Threads []channel `json:"threads"`
		}
		if err := json.Unmarshal(d.Data, &g); err != nil {
			return err
		}
		for _, t := range g.Threads {
			t.GuildID = g.ID
			l.r.rememberThread(t)
		}
		l.refreshDirectory()
	case "GUILD_DELETE", "GUILD_ROLE_CREATE", "GUILD_ROLE_UPDATE", "GUILD_ROLE_DELETE",
		"CHANNEL_CREATE", "CHANNEL_UPDATE", "CHANNEL_DELETE":
		l.refreshDirectory()
	case "THREAD_CREATE", "THREAD_UPDATE":
		var t channel
		if err := json.Unmarshal(d.Data, &t); err != nil {
			return err
		}
		l.r.rememberThread(t)
		if d.Type == "THREAD_CREATE" && t.Type != channelPrivateThread {
			// A thread started from a message has its ID; read that
			// message again so it becomes the thread's root.
			// A forum post's first message is in the thread itself and
			// arrives as its own create, so the parent not having it is
			// no deletion.
			return l.reread(ctx, t.ParentID, t.ID, false)
		}
	case "THREAD_LIST_SYNC":
		var sync struct {
			GuildID string    `json:"guild_id"`
			Threads []channel `json:"threads"`
		}
		if err := json.Unmarshal(d.Data, &sync); err != nil {
			return err
		}
		for _, t := range sync.Threads {
			t.GuildID = sync.GuildID
			l.r.rememberThread(t)
		}
	case "THREAD_DELETE":
		var t channel
		if err := json.Unmarshal(d.Data, &t); err != nil {
			return err
		}
		return l.deleteThread(ctx, t)
	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		var m message
		if err := json.Unmarshal(d.Data, &m); err != nil {
			return err
		}
		if m.GuildID == "" {
			return nil
		}
		p, ok, err := l.r.placeOf(ctx, m.ChannelID)
		if err != nil || !ok {
			return err
		}
		if m.Author.ID == "" || m.Timestamp.IsZero() {
			// A partial update: read the whole message.
			return l.reread(ctx, m.ChannelID, m.ID, true)
		}
		// The tail's cursors move only with what it reads itself: a live
		// message after a gap the gateway could not resume must not carry
		// the cursor past the messages of the gap.
		return l.r.write(ctx, m, p)
	case "MESSAGE_DELETE":
		var del struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
			GuildID   string `json:"guild_id"`
		}
		if err := json.Unmarshal(d.Data, &del); err != nil {
			return err
		}
		return l.delete(ctx, del.ChannelID, del.GuildID, del.ID)
	case "MESSAGE_DELETE_BULK":
		var del struct {
			IDs       []string `json:"ids"`
			ChannelID string   `json:"channel_id"`
			GuildID   string   `json:"guild_id"`
		}
		if err := json.Unmarshal(d.Data, &del); err != nil {
			return err
		}
		var errs []error
		for _, id := range del.IDs {
			errs = append(errs, l.delete(ctx, del.ChannelID, del.GuildID, id))
		}
		return errors.Join(errs...)
	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE", "MESSAGE_REACTION_REMOVE_ALL", "MESSAGE_REACTION_REMOVE_EMOJI":
		var reaction struct {
			ChannelID string `json:"channel_id"`
			MessageID string `json:"message_id"`
			GuildID   string `json:"guild_id"`
		}
		if err := json.Unmarshal(d.Data, &reaction); err != nil {
			return err
		}
		if reaction.GuildID == "" {
			return nil
		}
		l.mu.Lock()
		l.reacted[[2]string{reaction.ChannelID, reaction.MessageID}] = true
		l.mu.Unlock()
	}
	return nil
}

func (l *live) delete(ctx context.Context, channelID, guildID, messageID string) error {
	if guildID == "" {
		return nil
	}
	p, ok, err := l.r.placeOf(ctx, channelID)
	if err != nil || !ok {
		return err
	}
	return l.r.deleteLive(ctx, p, messageID)
}

// reread reads one message from Discord and writes it. A message Discord
// no longer has is deleted when missingIsDeleted.
func (l *live) reread(ctx context.Context, channelID, messageID string, missingIsDeleted bool) error {
	p, ok, err := l.r.placeOf(ctx, channelID)
	if err != nil || !ok {
		return err
	}
	m, err := l.r.client.message(ctx, channelID, messageID)
	if isStatus(err, 404) {
		if !missingIsDeleted {
			return nil
		}
		return l.r.deleteLive(ctx, p, messageID)
	}
	if err != nil {
		return err
	}
	m.GuildID = p.GuildID
	return l.r.write(ctx, m, p)
}

// deleteThread takes out the messages of a deleted thread.
func (l *live) deleteThread(ctx context.Context, t channel) error {
	p, ok, err := l.r.placeOf(ctx, t.ParentID)
	if err != nil || !ok {
		return err
	}
	threadID := externalID(p, t.ID)
	// A text channel's thread starts from a message that stays in the
	// channel; a forum or media post's first message is in the thread and
	// goes with it.
	keepRoot := true
	if _, parent, ok := l.r.directory().channel(p.ChannelID); ok && (parent.Type == channelForum || parent.Type == channelMedia) {
		keepRoot = false
	}
	rows, err := l.r.deps.Pool.Query(ctx, `
		SELECT external_id FROM events
		 WHERE source = $1 AND thread_id = $2 AND NOT (external_id = $2 AND $3) AND NOT deleted`, ConnectorName, threadID, keepRoot)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	unlock := l.r.lock(p.ChannelID)
	defer unlock()
	for _, id := range ids {
		if err := l.r.remove(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// flushReactions reads again each message whose reactions changed.
func (l *live) flushReactions(ctx context.Context) error {
	l.mu.Lock()
	pending := l.reacted
	l.reacted = map[[2]string]bool{}
	l.mu.Unlock()
	var errs []error
	for key := range pending {
		errs = append(errs, l.reread(ctx, key[0], key[1], true))
	}
	return errors.Join(errs...)
}

// run applies dispatches from events until ctx ends, reading reacted
// messages again every reactionDelay.
func (l *live) run(ctx context.Context, events <-chan dispatch) {
	ticker := time.NewTicker(reactionDelay)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.flushReactions(ctx); err != nil && ctx.Err() == nil {
				l.r.logger.Warn("discord: reading reacted messages failed", "err", err)
			}
		case d := <-events:
			if err := l.handle(ctx, d); err != nil && ctx.Err() == nil {
				l.r.logger.Warn("discord: applying a live event failed", "event", d.Type, "err", err)
			}
			l.noteEvent(ctx)
		}
	}
}

// noteEvent saves, at most every half minute, when the last live event
// arrived.
func (l *live) noteEvent(ctx context.Context) {
	now := time.Now().UTC()
	if now.Sub(l.lastEvent) < 30*time.Second {
		return
	}
	l.lastEvent = now
	var state GatewayState
	_ = l.r.deps.State.Update(ctx, gatewayKey, &state, func(bool) error {
		state.LastEvent = now
		return nil
	})
}
