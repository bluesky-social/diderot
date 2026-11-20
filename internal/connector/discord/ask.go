package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// AskRouter is the leader term's Ask as Discord uses it: chat sessions keyed
// by a place and a thread, as Slack's are.
type AskRouter interface {
	askchat.ThreadSessions
	// SlackSessionState reports whether a conversation has a session, and
	// the newest of its place's messages the session has seen.
	SlackSessionState(ctx context.Context, channelID, threadTS string) (known bool, contextThrough time.Time, err error)
}

// Ask is what a Run needs to answer in Discord.
type Ask struct {
	Router AskRouter
	// Summarizer writes thread summaries; nil says no model is chosen for
	// them.
	Summarizer *askchat.Summarizer
}

// The commands Ask registers, globally.
const (
	commandAsk                = "ask"
	commandSummarize          = "Summarize thread"
	commandSummarizePrivately = "Summarize thread privately"
)

// interactionTurnTimeout bounds a turn answered through an interaction,
// whose token lasts 15 minutes, leaving time to deliver the answer.
const interactionTurnTimeout = 13 * time.Minute

// What Ask says when it won't answer.
const (
	offInChannel   = "Ask isn't on in this channel. An administrator can turn it on on Diderot's Discord page."
	offInDMs       = "I don't answer direct messages here. Mention me in a server channel where Ask is on."
	notAMember     = "I answer direct messages only from members of a server where Ask is on."
	noQuestion     = "Ask me a question after the mention."
	accessUnknown  = "I couldn't read who can see this channel, so I can't answer here right now. Please try again shortly."
	notAThread     = "Summarize a message in a thread, or the message a thread started from."
	noSummaries    = "Thread summaries aren't set up: an administrator chooses a model for them on Diderot's Models page."
	summaryPrivate = "Not everyone who can see this channel can read its history, so I can only summarize it privately."
	cannotRead     = "You can't read this channel's history, so I can't summarize it for you."
)

// asker answers in Discord for one Run: it routes mentions, direct
// messages, commands, Stop buttons and reactions into the shared engine.
type asker struct {
	client     *Client
	deps       source.Deps
	r          *reader
	router     AskRouter
	summarizer *askchat.Summarizer
	surface    *surface
	engine     *askchat.Engine
	answers    *askchat.AnswerStore
	self       user
	appID      string
	logger     *slog.Logger
	// run is the Run's context, which turns run under.
	run   context.Context
	turns sync.WaitGroup

	mu sync.Mutex
	// places are the places with a turn running or waiting, and how many.
	places map[string]*placeTurns
}

// placeTurns queues the turns of one place.
type placeTurns struct {
	sync.Mutex
	waiting int
}

// lockPlace waits for the turns before it in place to end, so that a
// place has one turn at a time: its progress, working record and context
// belong to that turn alone.
func (a *asker) lockPlace(place string) func() {
	a.mu.Lock()
	if a.places == nil {
		a.places = map[string]*placeTurns{}
	}
	p := a.places[place]
	if p == nil {
		p = &placeTurns{}
		a.places[place] = p
	}
	p.waiting++
	a.mu.Unlock()
	p.Lock()
	return func() {
		p.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		if p.waiting--; p.waiting == 0 {
			delete(a.places, place)
		}
	}
}

func newAsker(run context.Context, client *Client, deps source.Deps, r *reader, a *Ask) (*asker, error) {
	self, err := client.currentUser(run)
	if err != nil {
		return nil, fmt.Errorf("discord ask: read the bot: %w", err)
	}
	app, err := client.currentApplication(run)
	if err != nil {
		return nil, fmt.Errorf("discord ask: read the application: %w", err)
	}
	s := newSurface(client, app.ID, self.ID, deps.Logger)
	answers := &askchat.AnswerStore{Pool: deps.Pool}
	return &asker{
		client: client, deps: deps, r: r, router: a.Router, summarizer: a.Summarizer,
		surface: s, answers: answers, self: self, appID: app.ID, logger: deps.Logger, run: run,
		engine: &askchat.Engine{
			Surface: s, Ask: askchat.NewRouter(a.Router), Answers: answers,
			Working: askchat.PGWorkingThreads{Pool: deps.Pool, Surface: ask.AskRunSurfaceDiscord},
			Turns:   &askchat.Turns{}, Logger: deps.Logger,
		},
	}, nil
}

// commands are Ask's application commands: /ask in servers and in the
// bot's DMs, and the two message commands in servers.
func commands() []applicationCommand {
	return []applicationCommand{
		{Type: commandChatInput, Name: commandAsk, Description: "Ask Diderot a question",
			Contexts: []int{contextGuild, contextBotDM},
			Options: []commandOption{
				{Type: 3, Name: "question", Description: "What you want to know", Required: true},
				{Type: 5, Name: "private", Description: "Show the answer only to you"},
			}},
		{Type: commandMessage, Name: commandSummarize, Contexts: []int{contextGuild}},
		{Type: commandMessage, Name: commandSummarizePrivately, Contexts: []int{contextGuild}},
	}
}

// registerCommands puts Ask's commands in place.
func (a *asker) registerCommands(ctx context.Context) error {
	return a.client.setGlobalCommands(ctx, a.appID, commands())
}

// spawn runs fn off the live loop; the Run waits for it before returning.
func (a *asker) spawn(fn func()) {
	a.turns.Add(1)
	go func() {
		defer a.turns.Done()
		// A panic anywhere in fn (a mention, a DM, an interaction, the Ask
		// turn itself) must fail only this one unit of work, not crash the
		// whole process: see worker.Recover. askchat.Engine.RunTurn already
		// recovers panics from the turn it runs, so by the time one would
		// reach here it is something that escaped that -- still contained.
		defer worker.Recover(a.logger, "discord-ask")
		fn()
	}()
}

// wait waits for the turns and replies spawned.
func (a *asker) wait() { a.turns.Wait() }

// handle routes one gateway dispatch to Ask. It never blocks on a turn.
func (a *asker) handle(ctx context.Context, d dispatch) {
	switch d.Type {
	case "MESSAGE_CREATE":
		var m message
		if err := json.Unmarshal(d.Data, &m); err != nil {
			return
		}
		if m.Author.ID == "" || m.Author.Bot || m.Author.ID == a.self.ID || !indexedMessageType(m.Type) {
			return
		}
		switch {
		case m.GuildID == "":
			a.spawn(func() { a.onDirectMessage(m) })
		case m.mentions(a.self.ID):
			a.spawn(func() { a.onMention(m) })
		}
	case "INTERACTION_CREATE":
		var i interaction
		if err := json.Unmarshal(d.Data, &i); err != nil {
			return
		}
		a.spawn(func() { a.onInteraction(i) })
	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		a.onReaction(ctx, d)
	}
}

func (a *asker) settings(ctx context.Context) (AskSettings, error) {
	return ChannelsFor(a.deps).AskSettings(ctx)
}

// claim takes a question once, so a redelivered event is not answered
// twice; a failed claim answers anyway.
func (a *asker) claim(ctx context.Context, channelID, messageID string) bool {
	result, err := askchat.Claim(ctx, a.deps.Pool, channelID, messageID)
	if err != nil {
		a.logger.Warn("discord ask: claiming the question failed; answering anyway", "channel", channelID, "err", err)
	}
	return result != "duplicate"
}

// replyTo answers m with a short message.
func (a *asker) replyTo(ctx context.Context, m message, text string) {
	_, err := a.client.createMessage(ctx, m.ChannelID, outMessage{
		Content: text, AllowedMentions: noMentions, Flags: flagSuppressEmbeds,
		MessageReference: &messageReference{MessageID: m.ID},
	})
	if err != nil {
		a.logger.Warn("discord ask: reply failed", "channel", m.ChannelID, "err", err)
	}
}

// onMention answers a mention in a guild channel in a thread: the one it
// was asked in, or one started from the question.
func (a *asker) onMention(m message) {
	ctx := a.run
	settings, err := a.settings(ctx)
	if err != nil {
		a.logger.Warn("discord ask: reading the settings failed", "err", err)
		return
	}
	ch, err := a.client.channel(ctx, m.ChannelID)
	if err != nil {
		a.logger.Warn("discord ask: reading the channel failed", "channel", m.ChannelID, "err", err)
		return
	}
	inThread := isThread(ch.Type)
	parentID := m.ChannelID
	if inThread {
		parentID = ch.ParentID
	}
	if !settings.answersIn(m.GuildID, parentID) {
		a.replyTo(ctx, m, offInChannel)
		return
	}
	if !a.claim(ctx, m.ChannelID, m.ID) {
		return
	}
	question := a.questionText(m)
	if question == "" {
		a.replyTo(ctx, m, noQuestion)
		return
	}
	if !inThread && ch.Type != channelText && ch.Type != channelAnnouncement {
		a.replyTo(ctx, m, "I answer in text channels and their threads.")
		return
	}
	place := m.ChannelID
	if !inThread {
		thread, err := a.client.startThread(ctx, m.ChannelID, m.ID, threadName(question))
		if err != nil {
			a.logger.Warn("discord ask: starting a thread failed", "channel", m.ChannelID, "err", err)
			a.replyTo(ctx, m, "I couldn't start a thread for the answer: "+describeErr(err)+".")
			return
		}
		place = thread.ID
		a.surface.rememberParent(place, m.ChannelID)
		defer a.surface.forgetParent(place)
	}
	a.answer(ctx, m, question, place, parentID, inThread, a.channelAccess(m.GuildID, parentID))
}

// refusal is why a question goes unanswered, as the asker is told.
type refusal string

func (r refusal) Error() string { return string(r) }

// channelAccess reads, when the turn starts, the audience of an answer in a
// guild channel (or one of its threads).
func (a *asker) channelAccess(guildID, channelID string) func(context.Context) (audience, error) {
	return func(ctx context.Context) (audience, error) {
		settings, err := a.settings(ctx)
		if err != nil {
			return audience{}, err
		}
		if !settings.answersIn(guildID, channelID) {
			return audience{}, refusal(offInChannel)
		}
		return a.channelAudience(ctx, settings, guildID, channelID)
	}
}

// memberAccess reads, when the turn starts, what an answer only userID
// reads may read.
func (a *asker) memberAccess(userID string, dm bool) func(context.Context) (audience, error) {
	return func(ctx context.Context) (audience, error) {
		settings, err := a.settings(ctx)
		if err != nil {
			return audience{}, err
		}
		if dm && !settings.DirectMessages {
			return audience{}, refusal(offInDMs)
		}
		scopes, isMember := a.memberScopes(ctx, settings, userID)
		if !isMember {
			return audience{}, refusal(notAMember)
		}
		return audience{Scopes: scopes}, nil
	}
}

// onDirectMessage answers a direct message, with what the asker can read.
func (a *asker) onDirectMessage(m message) {
	ctx := a.run
	settings, err := a.settings(ctx)
	if err != nil {
		a.logger.Warn("discord ask: reading the settings failed", "err", err)
		return
	}
	if !settings.DirectMessages {
		a.replyTo(ctx, m, offInDMs)
		return
	}
	if !a.claim(ctx, m.ChannelID, m.ID) {
		return
	}
	question := a.questionText(m)
	if question == "" {
		return
	}
	a.answer(ctx, m, question, m.ChannelID, "", false, a.memberAccess(m.Author.ID, true))
}

// answer runs the turn for question m in place, a thread whose parent is
// parentID or a DM channel, for the audience access reads. The audience is
// read once the turn before it in the place has ended, so a question that
// waited runs with the access of when it runs, not of when it was asked.
// inThread says the place is a thread that may have messages of its own the
// session has not seen. They are read with the bot's access, outside the
// scopes, so they are the turn's context only when everyone who can view
// the channel can read its history, and whether they can is part of the
// session's key, as the scopes are.
func (a *asker) answer(ctx context.Context, m message, question, place, parentID string, inThread bool, access func(context.Context) (audience, error)) {
	// The question is marked seen while it waits for a turn before it.
	a.surface.MarkQuestion(ctx, place, m.ID, askchat.MarkSeen)
	defer a.lockPlace(place)()
	if ctx.Err() != nil {
		return
	}
	heard, err := access(ctx)
	if err != nil {
		var refused refusal
		text := accessUnknown
		if errors.As(err, &refused) {
			text = string(refused)
		} else {
			a.logger.Warn("discord ask: reading the answer's audience failed", "channel", place, "err", err)
		}
		a.surface.MarkQuestion(ctx, place, m.ID, askchat.MarkStopped)
		if _, err := a.surface.post(ctx, askchat.Conversation{Place: place}, text); err != nil {
			a.logger.Warn("discord ask: reply failed", "channel", place, "err", err)
		}
		return
	}
	scopes := heard.Scopes
	key := scopes
	if heard.ReadsHistory {
		key = append(slices.Clone(scopes), "thread-history")
	}
	withContext := inThread && heard.ReadsHistory
	conv := askchat.Conversation{Place: place, Thread: sessionKey(key)}
	_, through, err := a.router.SlackSessionState(ctx, conv.Place, conv.Thread)
	if err != nil {
		a.logger.Warn("discord ask: reading the session failed", "channel", place, "err", err)
		a.engine.ReplyError(ctx, conv, "", "I couldn't start this conversation; please try again shortly")
		return
	}
	contextThrough := m.Timestamp
	var threadContext string
	if withContext {
		tc, err := a.threadContext(ctx, m.GuildID, place, parentID, through, m.ID)
		if err != nil {
			// The answer goes on without it, and the next question
			// tries the missed messages again.
			a.logger.Warn("discord ask: reading the thread failed; answering without it", "channel", place, "err", err)
			contextThrough = time.Time{}
		}
		threadContext = tc
	}
	a.engine.RunTurn(ctx, askchat.Question{
		Conversation: conv, MessageID: m.ID, AskerID: m.Author.ID,
		Text: question, Title: question,
		ThreadContext: threadContext, ContextThrough: contextThrough,
		Asker: askerInfo(m.Author, m.Member), AllowedScopes: scopes,
	})
}

// selfMention finds a mention of a user, to take the bot's out of a
// question.
var selfMention = regexp.MustCompile(`<@!?(\d+)>`)

// questionText is m as the model reads it: without the mention of the bot,
// with other mentions as names, and with what it replies to and attaches.
func (a *asker) questionText(m message) string {
	m.Content = selfMention.ReplaceAllStringFunc(m.Content, func(tag string) string {
		if selfMention.FindStringSubmatch(tag)[1] == a.self.ID {
			return ""
		}
		return tag
	})
	return strings.TrimSpace(renderBody(m, a.r.place(m.GuildID, "", "", "")))
}

// threadName is a thread's name for question: its start, within Discord's
// 100 characters.
func threadName(question string) string {
	name := clip(question, 90)
	if name == "" {
		return "Question"
	}
	return name
}

func askerInfo(u user, m *member) *ask.AskerInfo {
	name := u.displayName()
	if m != nil && m.Nick != "" {
		name = m.Nick
	}
	return &ask.AskerInfo{DisplayName: name, Handle: u.Username, LocalTime: time.Now().UTC()}
}

// threadContext is the place's messages after through and before the
// question, which the session has not seen, newest ThreadContextMax of
// them; with no session yet, also the message the thread started from.
// The bot's own messages are left out.
func (a *asker) threadContext(ctx context.Context, guildID, threadID, parentID string, through time.Time, beforeID string) (string, error) {
	var msgs []message
	truncated := false
	err := a.client.messagesBefore(ctx, threadID, beforeID, func(page []message) (bool, error) {
		for _, m := range page {
			if !through.IsZero() && !m.Timestamp.After(through) {
				return false, nil
			}
			if m.Author.ID == a.self.ID || !indexedMessageType(m.Type) {
				continue
			}
			if len(msgs) == askchat.ThreadContextMax {
				truncated = true
				return false, nil
			}
			msgs = append(msgs, m)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	if through.IsZero() && !truncated && parentID != "" {
		if starter, err := a.client.message(ctx, parentID, threadID); err == nil && starter.Author.ID != a.self.ID {
			msgs = append(msgs, starter)
		}
	}
	p := a.r.place(guildID, "", "", "")
	out := make([]askchat.ContextMessage, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		out = append(out, askchat.ContextMessage{At: m.Timestamp, Actor: authorName(m), Body: renderBody(m, p)})
	}
	return askchat.RenderThreadContext(out, truncated), nil
}

func authorName(m message) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	return m.Author.displayName()
}

// onReaction records a 👍 or 👎 on an answer as its rating.
func (a *asker) onReaction(ctx context.Context, d dispatch) {
	var r struct {
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
		MessageID string `json:"message_id"`
		Emoji     emoji  `json:"emoji"`
	}
	if err := json.Unmarshal(d.Data, &r); err != nil || r.UserID == a.self.ID {
		return
	}
	verdict := verdictFor(r.Emoji.Name)
	if verdict == "" {
		return
	}
	sessionID, turn, found, err := a.answers.Lookup(ctx, r.ChannelID, r.MessageID)
	if err != nil || !found {
		return
	}
	if d.Type == "MESSAGE_REACTION_ADD" {
		err = a.answers.UpsertFeedback(ctx, sessionID, turn, r.ChannelID, r.MessageID, r.UserID, r.Emoji.Name, verdict)
	} else {
		err = a.answers.StampFeedbackRemoved(ctx, sessionID, turn, r.MessageID, r.UserID, r.Emoji.Name)
	}
	if err != nil {
		a.logger.Warn("discord ask: recording a rating failed", "channel", r.ChannelID, "err", err)
	}
}

// verdictFor is the rating a reaction gives: thumbs up or down, in any skin
// tone, or none.
func verdictFor(name string) string {
	switch {
	case strings.HasPrefix(name, "👍"):
		return "up"
	case strings.HasPrefix(name, "👎"):
		return "down"
	}
	return ""
}

func (s *surface) rememberParent(threadID, parentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parents[threadID] = parentID
}
