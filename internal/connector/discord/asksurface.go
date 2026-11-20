package discord

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Discord is the second surface of the shared Ask-over-chat engine
// (internal/askchat): surface is what the engine does in Discord.
//
// A conversation's Place is the channel the answer is posted in: a thread,
// or a DM channel, or for a private /ask the channel it was run in. Its
// Thread is the session's key in that place: "s-" and the scopes'
// fingerprint (sessionKey), or "i-" and the interaction for a private /ask,
// whose answer goes back through the interaction.

// answerChunkLimit leaves room under Discord's 2,000 characters; a
// character is at least a byte, so a chunk of this many bytes always fits.
const answerChunkLimit = 1900

// The question's marks.
const (
	emojiSeen     = "👀"
	emojiAnswered = "✅"
)

// stopPrefix starts a Stop button's custom ID; the rest is the
// conversation's Thread, and the click's channel its Place.
const stopPrefix = "ask-stop:"

// The progress placeholder's words.
const (
	workingText = "Working on it…"
	stoppedText = "Stopped."
)

// typingEvery renews the typing indicator, which lasts ten seconds.
var typingEvery = 8 * time.Second

type surface struct {
	client *Client
	appID  string
	self   string
	logger *slog.Logger

	mu sync.Mutex
	// placeholders are the progress messages of the turns running here.
	placeholders map[askchat.Conversation]*placeholder
	// private are the interactions private conversations answer through.
	private map[askchat.Conversation]*privateReply
	// parents are the channels the threads Ask started belong to: a
	// thread started from a question has the question's ID, and the
	// question is in the parent.
	parents map[string]string
}

func newSurface(client *Client, appID, self string, logger *slog.Logger) *surface {
	return &surface{
		client: client, appID: appID, self: self, logger: logger,
		placeholders: map[askchat.Conversation]*placeholder{},
		private:      map[askchat.Conversation]*privateReply{},
		parents:      map[string]string{},
	}
}

// privateReply is an interaction a private conversation answers through.
// Its first response is the placeholder until the first post replaces it.
type privateReply struct {
	token    string
	mu       sync.Mutex
	answered bool
}

// placeholder is a turn's "Working on it…" message with its Stop button.
type placeholder struct {
	mu        sync.Mutex
	messageID string
	// stopped says someone pressed Stop: the placeholder stays, saying so.
	stopped bool
}

func (s *surface) privateFor(conv askchat.Conversation) *privateReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.private[conv]
}

func (*surface) Name() ask.AskRunSurface { return ask.AskRunSurfaceDiscord }

// Asker is given with every question.
func (*surface) Asker(context.Context, string) *ask.AskerInfo { return nil }

func (s *surface) MarkQuestion(ctx context.Context, place, messageID string, mark askchat.Mark) {
	if messageID == "" {
		return
	}
	channelID := place
	s.mu.Lock()
	if parent, ok := s.parents[place]; ok && place == messageID {
		channelID = parent
	}
	s.mu.Unlock()
	var err error
	switch mark {
	case askchat.MarkSeen:
		err = s.client.addReaction(ctx, channelID, messageID, emojiSeen)
	case askchat.MarkStopped:
		err = s.client.removeOwnReaction(ctx, channelID, messageID, emojiSeen)
	case askchat.MarkAnswered:
		err = errors.Join(s.client.removeOwnReaction(ctx, channelID, messageID, emojiSeen),
			s.client.addReaction(ctx, channelID, messageID, emojiAnswered))
	}
	if err != nil {
		s.logger.Warn("discord ask: marking the question failed", "channel", channelID, "err", err)
	}
}

func (s *surface) Progress(q askchat.Question) askchat.Progress {
	return &progress{s: s, conv: q.Conversation, done: make(chan struct{})}
}

// progress shows a turn working: a placeholder with a Stop button (for a
// private conversation, the interaction's response), and the typing
// indicator while the turn runs.
type progress struct {
	s    *surface
	conv askchat.Conversation

	mu      sync.Mutex
	started bool
	ended   bool
	cancel  context.CancelFunc
	// done is closed once Start's posting has finished.
	done chan struct{}
}

func (p *progress) Start() {
	p.mu.Lock()
	if p.started || p.ended {
		p.mu.Unlock()
		return
	}
	p.started = true
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		// A panic here (posting the placeholder, the typing-indicator
		// loop) must not crash the whole process over a "Working on it…"
		// message; close(p.done) above still runs, so Finish does not
		// wait forever.
		defer worker.Recover(p.s.logger, "discord-ask-progress")
		p.s.showWorking(ctx, p.conv, p.running)
	}()
}

// running reports whether the turn has not ended yet.
func (p *progress) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.ended
}

func (p *progress) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = true
	if p.cancel != nil {
		p.cancel()
	}
}

// Finish takes the placeholder down once its posting has finished; a
// stopped turn's placeholder stays, saying it stopped.
func (p *progress) Finish(ctx context.Context) bool {
	p.Stop()
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return true
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return false
	}
	ok, err := p.s.ClearProgress(ctx, p.conv)
	if err != nil {
		p.s.logger.Warn("discord ask: taking the placeholder down failed", "channel", p.conv.Place, "err", err)
	}
	return ok
}

// showWorking posts the placeholder, unless the turn has already ended,
// then shows the bot typing until ctx ends. The post itself is not cut off
// when the turn ends, so that Finish knows whether it landed.
func (s *surface) showWorking(ctx context.Context, conv askchat.Conversation, running func() bool) {
	if !running() {
		return
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	stop := buttonRow("Stop", stopPrefix+conv.Thread)
	if reply := s.privateFor(conv); reply != nil {
		reply.mu.Lock()
		defer reply.mu.Unlock()
		if reply.answered {
			return
		}
		if _, err := s.client.editOriginal(postCtx, s.appID, reply.token, outMessage{Content: workingText, Components: &stop}); err != nil {
			s.logger.Warn("discord ask: showing progress failed", "channel", conv.Place, "err", err)
		}
		return
	}
	posted, err := s.client.createMessage(postCtx, conv.Place, outMessage{
		Content: workingText, Components: &stop, AllowedMentions: noMentions,
	})
	if err != nil {
		s.logger.Warn("discord ask: posting the placeholder failed", "channel", conv.Place, "err", err)
		return
	}
	s.mu.Lock()
	s.placeholders[conv] = &placeholder{messageID: posted.ID}
	s.mu.Unlock()
	for {
		if err := s.client.typing(ctx, conv.Place); err != nil && ctx.Err() == nil {
			s.logger.Debug("discord ask: typing failed", "channel", conv.Place, "err", err)
		}
		if sleep(ctx, typingEvery) != nil {
			return
		}
	}
}

// markStopped notes that someone pressed Stop on conv's placeholder, which
// then stays, saying so.
func (s *surface) markStopped(conv askchat.Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.placeholders[conv]; ok {
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
	}
}

// ClearProgress takes conv's placeholders down: the one a turn here posted,
// or, for a turn that died with its process, any the bot's recent messages
// in the place still show. A stopped turn's placeholder is edited to say
// so instead. A private conversation's placeholder is the interaction's,
// which only the turn that holds it can reach; Discord drops it in time.
func (s *surface) ClearProgress(ctx context.Context, conv askchat.Conversation) (bool, error) {
	if strings.HasPrefix(conv.Thread, "i-") {
		reply := s.privateFor(conv)
		if reply == nil {
			return true, nil
		}
		reply.mu.Lock()
		defer reply.mu.Unlock()
		if reply.answered {
			return true, nil
		}
		none := []component{}
		_, err := s.client.editOriginal(ctx, s.appID, reply.token, outMessage{Content: stoppedText, Components: &none})
		return err == nil, err
	}
	s.mu.Lock()
	p, ok := s.placeholders[conv]
	delete(s.placeholders, conv)
	s.mu.Unlock()
	if ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.stopped {
			none := []component{}
			err := s.client.editMessage(ctx, conv.Place, p.messageID, outMessage{Content: stoppedText, Components: &none})
			return err == nil, err
		}
		err := s.client.deleteMessage(ctx, conv.Place, p.messageID)
		if isStatus(err, 404) {
			err = nil
		}
		return err == nil, err
	}
	var found []string
	err := s.client.messagesBefore(ctx, conv.Place, "", func(page []message) (bool, error) {
		for _, m := range page {
			if m.Author.ID == s.self && hasButton(m, stopPrefix+conv.Thread) {
				found = append(found, m.ID)
			}
		}
		return false, nil
	})
	if isStatus(err, 403, 404) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, id := range found {
		if err := s.client.deleteMessage(ctx, conv.Place, id); err != nil && !isStatus(err, 404) {
			return false, err
		}
	}
	return true, nil
}

// hasButton reports whether m carries a button with customID.
func hasButton(m message, customID string) bool {
	var walk func([]component) bool
	walk = func(cs []component) bool {
		for _, c := range cs {
			if c.CustomID == customID || walk(c.Components) {
				return true
			}
		}
		return false
	}
	return walk(m.Components)
}

func (s *surface) PostReply(ctx context.Context, conv askchat.Conversation, text string) error {
	_, err := s.post(ctx, conv, s.Sanitize(text))
	return err
}

// post posts text into conv without pings or link previews: in the place,
// or through a private conversation's interaction, whose first post
// replaces the placeholder.
func (s *surface) post(ctx context.Context, conv askchat.Conversation, text string, files ...upload) (message, error) {
	if strings.HasPrefix(conv.Thread, "i-") {
		reply := s.privateFor(conv)
		if reply == nil {
			return message{}, errors.New("discord ask: the private answer's interaction has ended")
		}
		reply.mu.Lock()
		defer reply.mu.Unlock()
		out := outMessage{Content: text, AllowedMentions: noMentions, Flags: flagEphemeral | flagSuppressEmbeds}
		if !reply.answered && len(files) == 0 {
			none := []component{}
			out.Components, out.Flags = &none, 0
			posted, err := s.client.editOriginal(ctx, s.appID, reply.token, out)
			if err == nil {
				reply.answered = true
			}
			return posted, err
		}
		return s.client.followUp(ctx, s.appID, reply.token, out, files...)
	}
	return s.client.createMessage(ctx, conv.Place, outMessage{
		Content: text, AllowedMentions: noMentions, Flags: flagSuppressEmbeds,
	}, files...)
}

// mentionEveryone finds the mentions Discord turns into pings of a whole
// server or channel, matched case-insensitively as a defensive measure:
// nothing in this codebase or Discord's docs confirms whether its client
// treats "@EVERYONE" or "@Here" as live the same as the lowercase form,
// so this assumes the less safe answer either way. A single regexp
// pass, rather than sanitizeSlackMarkdown's old multi-pass design, so
// there is nothing for one match's replacement to recombine with into
// a new one; the replacement also only inserts a character inside the
// matched word, so it can never form a new "@everyone"/"@here" span
// out of adjacent text either.
var mentionEveryone = regexp.MustCompile(`(?i)@(everyone|here)`)

// Sanitize keeps an answer from even looking like it pings everyone; no
// post pings anyone regardless, since every post sets AllowedMentions to
// an empty parse list (noMentions), Discord's own server-side control
// that disables mention resolution independent of message text.
func (*surface) Sanitize(text string) string {
	return mentionEveryone.ReplaceAllStringFunc(text, func(m string) string {
		return "@\u200b" + m[1:]
	})
}

func (*surface) ChunkLimit() int { return answerChunkLimit }

func (s *surface) PostChunk(ctx context.Context, conv askchat.Conversation, chunk string, _ askchat.ChunkOptions) (string, error) {
	posted, err := s.post(ctx, conv, chunk)
	return posted.ID, err
}

// Upload attaches a panel as a PNG in its own message.
func (s *surface) Upload(ctx context.Context, conv askchat.Conversation, file askchat.File) (string, error) {
	posted, err := s.post(ctx, conv, s.Sanitize(file.Title), upload{
		Name: file.Name, Description: clip(file.AltText, 1000), ContentType: "image/png", Data: file.Data,
	})
	if err != nil {
		return "", err
	}
	if len(posted.Attachments) > 0 {
		return posted.Attachments[0].ID, nil
	}
	return posted.ID, nil
}
