package slack

import (
	"bytes"
	"context"
	"errors"

	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
)

// slackPostChunkBytes is the per-message budget for generated
// chat.postMessage bodies. 3500 UTF-8 bytes matches
// ChatGPT-in-Slack's SLACK_INTERIM_MESSAGE_BYTE_LIMIT — comfortably
// below Slack's documented 4000-char `text` field msg_too_long
// threshold and the 40000-char hard truncation cap, with headroom
// for Slack's native Markdown-to-rich-text translation.
//
// Slack's native markdown block permits a larger body, but the message also
// carries a complete non-mrkdwn accessibility fallback in `text`. Slack's
// limits on that field are documented in chars, not bytes. Picking bytes for
// our budget means a multi-byte-heavy answer (lots of emoji, CJK) isn't
// accidentally allowed past the char threshold. 3500 bytes ≈ 3500 ASCII
// chars, which sits 12% under the 4000-char cap.
const slackPostChunkBytes = 3500

// Slack is the first surface of the shared Ask-over-chat engine
// (internal/askchat): the consumer routes mentions and DMs into the engine,
// and slackSurface is what the engine does in Slack.
type slackSurface struct{ c *Consumer }

// engine is the shared Ask-over-chat engine driving this consumer's turns.
func (c *Consumer) engine() *askchat.Engine {
	e := &askchat.Engine{
		Surface: slackSurface{c}, Ask: chatRouter(c.ask), Turns: &c.turns, Logger: c.logger,
	}
	if store := c.workingThreads(); store != nil {
		e.Working = store
	}
	if c.pool != nil {
		e.Answers = &askchat.AnswerStore{Pool: c.pool}
	}
	return e
}

func (slackSurface) Name() ask.AskRunSurface { return ask.AskRunSurfaceSlack }

func (s slackSurface) Asker(ctx context.Context, askerID string) *ask.AskerInfo {
	return s.c.buildAskerInfo(ctx, askerID)
}

func (s slackSurface) MarkQuestion(ctx context.Context, channelID, messageTS string, mark askchat.Mark) {
	c := s.c
	switch mark {
	case askchat.MarkSeen:
		c.addPreAckReaction(ctx, channelID, messageTS)
	case askchat.MarkStopped:
		c.removeEyes(ctx, channelID, messageTS)
	case askchat.MarkAnswered:
		c.removeEyes(ctx, channelID, messageTS)
		err := WithSlackRetry(ctx, c.limiter, methodReactionsAdd, func() error {
			return c.api.AddReactionContext(ctx, "white_check_mark",
				slack.NewRefToMessage(channelID, messageTS))
		})
		if err != nil {
			c.logger.Warn("ask: add check_mark failed",
				"channel", channelID, "message_ts", messageTS, "err", err)
		}
	}
}

// Progress is the thread's Slack agent session: "processing" while the turn
// runs, set back to "active" once it has said what it had to.
func (s slackSurface) Progress(q askchat.Question) askchat.Progress {
	c := s.c
	transport := c.progress
	if transport == nil {
		transport = &slackProgressTransport{api: c.api, limiter: c.limiter}
	}
	return slackTurnProgress{
		p: newSlackProgress(transport, c.metrics, c.logger, q.Place, q.Thread, q.Title, q.AskerID, defaultProgressTimings),
		c: c,
	}
}

type slackTurnProgress struct {
	p *slackProgress
	c *Consumer
}

func (p slackTurnProgress) Start() { p.p.Start() }
func (p slackTurnProgress) Stop()  { p.p.Stop() }

// Finish sets the session back to active, only once the processing call is
// known to have landed: a clear racing it could be overtaken.
func (p slackTurnProgress) Finish(ctx context.Context) bool {
	if p.p.Join(ctx) != statusJoinConfirmed {
		return false
	}
	if err := p.p.Clear(ctx); err != nil {
		p.c.logger.Warn("ask: agent session clear failed",
			"channel", p.p.start.ChannelID, "thread_ts", p.p.start.ThreadTS, "err", err)
		return false
	}
	return true
}

func (s slackSurface) ClearProgress(ctx context.Context, conv askchat.Conversation) (bool, error) {
	c := s.c
	if c.progress == nil {
		return false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, progressRequestTimeout)
	defer cancel()
	result := c.progress.SetStatus(requestCtx, sessionStatus{
		ChannelID: conv.Place, ThreadTS: conv.Thread, Status: slack.AgentSessionStatusActive,
	})
	return result.err == nil, result.err
}

func (s slackSurface) PostReply(ctx context.Context, conv askchat.Conversation, text string) error {
	c := s.c
	if c.api == nil {
		return nil
	}
	return WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		_, _, apiErr := c.api.PostMessageContext(ctx, conv.Place,
			slack.MsgOptionTS(conv.Thread),
			slack.MsgOptionText(sanitizeSlackMarkdown(text), false),
		)
		return apiErr
	})
}

func (slackSurface) Sanitize(text string) string { return sanitizeSlackMarkdown(text) }

func (slackSurface) ChunkLimit() int { return slackPostChunkBytes }

// PostChunk posts one chunk as a native Markdown block in the thread; the
// last carries the feedback buttons.
func (s slackSurface) PostChunk(ctx context.Context, conv askchat.Conversation, chunk string, opts askchat.ChunkOptions) (string, error) {
	c := s.c
	if c.api == nil {
		return "", errors.New("slack api not configured")
	}
	var msgTS string
	err := WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		var apiErr error
		var trailingBlocks []slack.Block
		if opts.Last {
			trailingBlocks = append(trailingBlocks, feedbackButtonsBlock())
		}
		msgOpts := generatedMarkdownMessageOptions(chunk, trailingBlocks...)
		msgOpts = append(msgOpts, slack.MsgOptionTS(conv.Thread))
		if opts.Marked {
			msgOpts = append(msgOpts, slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl())
		}
		_, msgTS, apiErr = c.api.PostMessageContext(ctx, conv.Place, msgOpts...)
		return apiErr
	})
	return msgTS, err
}

func (s slackSurface) DeliveredChunks(ctx context.Context, conv askchat.Conversation, marker string) (map[string]string, error) {
	if s.c.api == nil {
		return nil, errors.New("slack api not configured")
	}
	return s.c.deliveredAnswerChunks(ctx, conv.Place, conv.Thread, marker)
}

// Upload attaches a panel to the thread as a Slack file.
func (s slackSurface) Upload(ctx context.Context, conv askchat.Conversation, file askchat.File) (string, error) {
	c := s.c
	if c.api == nil {
		return "", errors.New("slack api not configured")
	}
	uploaded, err := c.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Reader:          bytes.NewReader(file.Data),
		FileSize:        len(file.Data),
		Filename:        file.Name,
		Title:           file.Title,
		AltTxt:          file.AltText,
		Channel:         conv.Place,
		ThreadTimestamp: conv.Thread,
	})
	if err != nil || uploaded == nil {
		return "", err
	}
	return uploaded.ID, nil
}

// chatRouter is the consumer's Ask as the engine's Router.
func chatRouter(r AskRouter) askchat.Router {
	if r == nil {
		return nil
	}
	return askchat.NewRouter(r)
}
