package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/slack-go/slack"
)

// Message-shortcut callback IDs for the thread summarizer. These must
// match the shortcut entries in slack-app-manifest.yaml (features:
// shortcuts, type: message). Message shortcuts, not slash commands —
// slash commands cannot be invoked inside threads, and this feature is
// thread-only by design.
const (
	summarizeCallbackID        = "summarize_thread"
	summarizePrivateCallbackID = "summarize_thread_private"
)

// summarizeReplyTimeout bounds the full fetch→LLM→post pipeline for one
// summarize invocation. Socket-mode acks are immediate (the event loop
// acks before launching the handler), so this only governs how long the
// handler goroutine keeps working. Deliberately short relative to the
// 15m ask-turn budget: a summary is a nice-to-have, not a research task.
const summarizeReplyTimeout = 90 * time.Second

// pinReactionName is the Slack reaction that marks an action item.
const pinReactionName = "pushpin"

// summarizeConcurrency caps how many summarize pipelines may run at
// once. Each pipeline performs Slack reads, a gateway LLM call, and
// posts; without a bound, repeated shortcut invocations could fan out
// unbounded concurrent work (rate limits, resource pressure, amplified
// gateway spend). Rejections are surfaced to the invoker, not dropped.
const summarizeConcurrency = 4

// summarizeSlots is the admission semaphore for summarize pipelines.
// Acquired non-blockingly in handleMessageAction — a full semaphore
// rejects the request with ephemeral feedback instead of queueing it (a
// queued request would still be a stale summary by the time it ran) or
// holding an accepted handler slot.
var summarizeSlots = make(chan struct{}, summarizeConcurrency)

// The thread summary's shared parts live in askchat; these are Slack's names
// for them.
type (
	summarizeMessage = askchat.ThreadMessage
	// Summarizer is the thread summarizer, with Slack's instructions.
	Summarizer = askchat.Summarizer
	// SummarizerConfig configures it; SystemPrompt defaults to Slack's.
	SummarizerConfig = askchat.SummarizerConfig
)

const (
	summarizeMaxMessages        = askchat.SummaryMaxMessages
	summarizeMaxFetchedMessages = askchat.SummaryMaxFetchedMessages
	summarizeMaxPromptChars     = askchat.SummaryMaxPromptChars
	summarizeMaxMessageChars    = askchat.SummaryMaxMessageChars
	summarizeMaxOutputTokens    = askchat.SummaryMaxOutputTokens
)

// NewSummarizer validates cfg without making a network request; an empty
// SystemPrompt is Slack's.
func NewSummarizer(cfg SummarizerConfig) (*Summarizer, error) {
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = summarizeSystemPrompt
	}
	return askchat.NewSummarizer(cfg)
}

func keepNewestAndPinned(all []summarizeMessage, limit int) []summarizeMessage {
	return askchat.KeepNewestAndPinned(all, limit)
}

// renderTranscript formats a Slack thread for the summary model.
func renderTranscript(channelName string, msgs []summarizeMessage) (string, string) {
	return askchat.RenderTranscript("Slack thread in channel "+channelName, msgs)
}

// summarizeSystemPrompt is the instruction contract for the summarizer
// model. Output is plain text formatted for Slack (mrkdwn-lite); the
// consumer renders it verbatim after sanitizeSlackMarkdown.
const summarizeSystemPrompt = `You summarize Slack threads for a busy engineering team.

You will receive a Slack thread transcript. Messages are formatted as:
  [index] <display-name>: <text>
Messages marked PINNED were flagged by the team with a pushpin (📌)
reaction as action items.

Produce a summary with exactly these sections, in this order:

1. A "Summary" section: 2-5 sentences capturing what the thread is
   about, key decisions made, and where things ended up.
2. An "Action Items" section: one bullet per PINNED message, each
   prefixed with the pushpin emoji and the author, e.g.
   "• 📌 @alice: deploy the fix Friday". Preserve the author's intent;
   do not invent, merge, or drop pinned items. If there are no pinned
   messages, write "None — no messages were pinned with 📌."

Rules:
- Plain text only, formatted for Slack. Use *bold* for section headers
  and bullets for lists. No markdown code fences, no headings syntax
  other than bold labels.
- Be concise and factual. Do not speculate beyond the transcript.
- Ignore bot messages unless they carry decision-relevant content.`

// threadRef identifies a Slack thread to summarize: the channel and the
// thread-root message timestamp.
type threadRef struct {
	ChannelID string
	ThreadTS  string
}

// resolveThreadRef extracts the thread a message shortcut was invoked
// on, from the interaction payload itself. The callback's Message is
// the message the user ran the shortcut on:
//
//   - a REPLY carries Msg.ThreadTimestamp = the root's ts — use it;
//   - a message inside a thread also surfaces Container.ThreadTs;
//   - the thread ROOT carries no thread markers of its own — it IS the
//     root, but it only counts as a thread when it has replies.
//
// ok=false when the message is not in a thread (the feature is
// thread-only) or the payload is missing the fields we need.
func resolveThreadRef(cb *slack.InteractionCallback) (threadRef, bool) {
	channelID := cb.Channel.ID
	if channelID == "" || cb.Message.Timestamp == "" {
		return threadRef{}, false
	}
	switch {
	case cb.Message.ThreadTimestamp != "":
		return threadRef{ChannelID: channelID, ThreadTS: cb.Message.ThreadTimestamp}, true
	case cb.Container.ThreadTs != "":
		return threadRef{ChannelID: channelID, ThreadTS: cb.Container.ThreadTs}, true
	case cb.Message.ReplyCount > 0:
		return threadRef{ChannelID: channelID, ThreadTS: cb.Message.Timestamp}, true
	default:
		return threadRef{}, false
	}
}

// threadReader is the Slack read surface the summarizer needs. It
// exists so tests can drive the pipeline without a live Slack client;
// consumerAPIReader adapts the consumer's client (with rate limiting)
// to satisfy it.
type threadReader interface {
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetUserInfoContext(ctx context.Context, userID string) (*slack.User, error)
}

// consumerAPIReader adapts the consumer's slack.Client to threadReader,
// routing every call through the tiered limiter + reactive 429 retry so
// summarize traffic shares the workspace-wide budgets with ingest,
// backfill, and ask.
type consumerAPIReader struct {
	c *Consumer
}

func (r consumerAPIReader) GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	var (
		msgs    []slack.Message
		hasMore bool
		next    string
	)
	err := WithSlackRetry(ctx, r.c.limiter, methodReplies, func() error {
		var apiErr error
		msgs, hasMore, next, apiErr = r.c.api.GetConversationRepliesContext(ctx, params)
		return apiErr
	})
	if err != nil {
		return nil, false, "", err
	}
	return msgs, hasMore, next, nil
}

func (r consumerAPIReader) GetUserInfoContext(ctx context.Context, userID string) (*slack.User, error) {
	var ui *slack.User
	err := WithSlackRetry(ctx, r.c.limiter, methodUserInfo, func() error {
		var apiErr error
		ui, apiErr = r.c.api.GetUserInfoContext(ctx, userID)
		return apiErr
	})
	if err != nil {
		return nil, err
	}
	return ui, nil
}

// fetchThread reads the thread at ref via conversations.replies,
// following pagination cursors up to summarizeMaxFetchedMessages total
// messages fetched (a hard stop, not a retention cap). The root message
// is included (conversations.replies returns it first). Replies arrive
// in chronological order, so the retained slice keeps the newest
// summarizeMaxMessages messages PLUS every pinned message outside that
// window (the feature's contract: one action item per 📌), with gaps
// marked for the render layer. Bot messages are kept; the model is
// instructed to weigh them lightly.
func fetchThread(ctx context.Context, client threadReader, ref threadRef) ([]summarizeMessage, error) {
	var collected []summarizeMessage
	fetched := 0
	cursor := ""
	for {
		params := &slack.GetConversationRepliesParameters{
			ChannelID: ref.ChannelID,
			Timestamp: ref.ThreadTS,
			Limit:     200,
		}
		if cursor != "" {
			params.Cursor = cursor
		}
		msgs, hasMore, next, err := client.GetConversationRepliesContext(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("fetch thread: %w", err)
		}
		for _, m := range msgs {
			collected = append(collected, summarizeMessage{
				Timestamp: m.Timestamp,
				UserID:    m.User,
				Username:  m.Name,
				Text:      m.Text,
				Pinned:    hasPinReaction(m.Reactions),
			})
		}
		fetched += len(msgs)
		// Retention cap: keep the newest messages and every pin; gap
		// counts accumulate across applications (see
		// keepNewestAndPinned).
		if len(collected) > summarizeMaxMessages {
			collected = keepNewestAndPinned(collected, summarizeMaxMessages)
		}
		// Hard fetch stop: beyond this, more pages cost Slack calls and
		// deadline without meaningfully changing the retained tail.
		if fetched >= summarizeMaxFetchedMessages {
			return collected, nil
		}
		if !hasMore || next == "" {
			return collected, nil
		}
		cursor = next
	}
}

// hasPinReaction reports whether a pushpin reaction is present among the
// message's reactions.
func hasPinReaction(reactions []slack.ItemReaction) bool {
	for _, r := range reactions {
		if r.Name == pinReactionName && r.Count > 0 {
			return true
		}
	}
	return false
}

// resolveUsernames replaces raw user IDs in thread messages with display
// names via users.info, caching per invocation only (one lookup per
// distinct user per summarize run; tier-4 budget, and the limiter paces
// repeats across the workspace anyway).
func resolveUsernames(ctx context.Context, client threadReader, msgs []summarizeMessage) {
	seen := map[string]string{}
	for i := range msgs {
		if msgs[i].UserID == "" {
			continue
		}
		name, ok := seen[msgs[i].UserID]
		if !ok {
			name = msgs[i].UserID
			if ui, err := client.GetUserInfoContext(ctx, msgs[i].UserID); err == nil && ui.Profile.DisplayName != "" {
				name = ui.Profile.DisplayName
			} else if err == nil && ui.Name != "" {
				name = ui.Name
			}
			seen[msgs[i].UserID] = name
		}
		msgs[i].Username = name
	}
}

// ---------------------------------------------------------------------------
// Gateway client
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Consumer wiring and pipeline
// ---------------------------------------------------------------------------

// WithSummarizer wires the thread-summarizer message shortcuts. Optional —
// when nil, the shortcuts reply "not configured" and everything else
// (ingest, ask) runs unchanged.
func (c *Consumer) WithSummarizer(s *Summarizer) *Consumer {
	c.summarizer = s
	return c
}

// handleMessageAction dispatches message-shortcut interactions (the
// three-dots menu on a message). Called from handleInteractive, which
// the event loop invokes AFTER acking the envelope, in its own handler
// goroutine — so the 90s pipeline cannot head-of-line-block dispatch
// of subsequent Slack events, and the run-state drain already waits for
// accepted handlers through process shutdown (no separate WaitGroup
// bookkeeping is needed here, unlike a bot whose handlers run on the
// event loop).
//
// Unknown callback IDs are ignored (the ack already happened at the
// loop level) so future shortcuts don't break old pods mid-deploy.
func (c *Consumer) handleMessageAction(ctx context.Context, cb slack.InteractionCallback) {
	switch cb.CallbackID {
	case summarizeCallbackID, summarizePrivateCallbackID:
	default:
		c.logger.Debug("summarize: ignoring unknown message action", "callback_id", cb.CallbackID)
		return
	}

	// Non-blocking admission: a full semaphore rejects the request with
	// feedback instead of queueing it (a queued request would still be
	// a stale summary by the time it ran) or parking a handler slot.
	select {
	case summarizeSlots <- struct{}{}:
		defer func() {
			<-summarizeSlots
			// The slot must be released even on panic: a skipped release
			// would permanently shrink the semaphore, and an unrecovered
			// panic in a handler goroutine kills the whole process
			// (taking ingest and ask down with it).
			if r := recover(); r != nil {
				c.logger.Error("summarize: pipeline panic", "panic", r)
			}
		}()
		private := cb.CallbackID == summarizePrivateCallbackID
		c.runSummarize(ctx, cb, private)
	default:
		c.metrics.summarize.WithLabelValues("rejected_busy").Inc()
		c.summarizeFeedback(ctx, cb.Channel.ID, cb.User.ID,
			"Too many summaries are already running — try again in a bit.")
	}
}

// runSummarize is the shared summarize pipeline for both shortcuts.
// The public shortcut posts the result into the thread; the private one
// posts the identical result ephemerally, visible only to the invoker.
// The shortcut payload carries the target message, its channel, and the
// invoking user — no link parsing, and the invoker is by construction
// viewing the channel the thread lives in.
func (c *Consumer) runSummarize(parent context.Context, cb slack.InteractionCallback, private bool) {
	userID := cb.User.ID
	channelID := cb.Channel.ID
	channelName := cb.Channel.Name

	if c.summarizer == nil {
		c.metrics.summarize.WithLabelValues("not_configured").Inc()
		c.summarizeFeedback(parent, channelID, userID,
			"The thread summarizer is not configured on this deployment.")
		return
	}

	ref, ok := resolveThreadRef(&cb)
	if !ok {
		c.metrics.summarize.WithLabelValues("not_thread").Inc()
		c.summarizeFeedback(parent, channelID, userID,
			"That message isn't in a thread — the summarizer only works inside message threads. Run it on any reply in the thread (or the thread's first message, once it has replies).")
		return
	}

	ctx, cancel := context.WithTimeout(parent, summarizeReplyTimeout)
	defer cancel()

	reader := consumerAPIReader{c: c}
	msgs, err := fetchThread(ctx, reader, ref)
	if err != nil {
		c.metrics.summarize.WithLabelValues("fetch_error").Inc()
		c.logger.Error("summarize: failed to fetch thread", "err", err, "channel", ref.ChannelID)
		c.summarizeFeedback(parent, channelID, userID, "Could not read that thread — check whether diderot is a member of the channel.")
		return
	}
	if len(msgs) == 0 {
		c.metrics.summarize.WithLabelValues("empty").Inc()
		c.summarizeFeedback(parent, channelID, userID, "That thread appears to be empty.")
		return
	}

	resolveUsernames(ctx, reader, msgs)
	transcript, truncationNote := renderTranscript(channelName, msgs)

	summary, usage, err := c.summarizer.Summarize(ctx, transcript)
	if err != nil {
		c.metrics.summarize.WithLabelValues("llm_error").Inc()
		c.logger.Error("summarize: gateway call failed", "err", err)
		c.summarizeFeedback(parent, channelID, userID, "Summarization failed — the LLM gateway returned an error. Try again in a bit.")
		return
	}
	// A well-formed response can still carry empty or
	// whitespace-only content (malformed provider response, empty
	// completion). Without this guard the invoker receives only the
	// metadata footer and the invocation counts as posted.
	if strings.TrimSpace(summary) == "" {
		c.metrics.summarize.WithLabelValues("llm_empty").Inc()
		c.logger.Error("summarize: gateway returned an empty summary")
		c.summarizeFeedback(parent, channelID, userID, "Summarization returned no usable result — try again in a bit.")
		return
	}

	// sanitizeSlackMarkdown neutralizes Slack-interpreted mention forms
	// in model output: thread content is untrusted input to the model,
	// and a prompt-injected thread could otherwise make the summary emit
	// <!channel>/<@user>, which Slack would interpret as real mentions.
	out := strings.TrimSpace(sanitizeSlackMarkdown(summary))
	if truncationNote != "" {
		out += "\n\n_" + sanitizeSlackMarkdown(truncationNote) + "_"
	}
	out += fmt.Sprintf("\n\n_summary by diderot · %d messages · %s · %d in / %d out tokens_",
		len(msgs), c.summarizer.ModelID(), usage.PromptTokens, usage.CompletionTokens)

	// Deliver on a context derived from the LEADER-scoped parent, not
	// the 90s pipeline child (whose deadline may already be exhausted
	// by fetch + gateway — posting with it would silently drop the
	// finished summary) and not context.Background() (a detached
	// context escapes run.cancelWork() on lease loss, so
	// a deposed leader could still mutate Slack after the new leader
	// takes over — exactly what the run-state cancellation exists to
	// prevent). The parent carries neither the 90s deadline nor
	// process-shutdown cancellation (graceful drain keeps it live),
	// so this satisfies both constraints at once.
	postCtx, postCancel := context.WithTimeout(parent, 10*time.Second)
	defer postCancel()

	// Delivery errors are surfaced, not swallowed: the invoker waited
	// through fetch + LLM for this result, and a swallowed post failure
	// (rate-limit exhaustion, permission error, oversized text) would
	// vanish with no feedback while the posted metric lied.
	var postErr error
	if private {
		// Ephemeral to the invoker, anchored into the thread. The
		// invoker ran the shortcut on a message in this channel, so
		// they are viewing (and are a member of) it — the ephemeral
		// post cannot hit user_not_in_channel.
		postErr = c.summarizePostEphemeral(postCtx, ref.ChannelID, userID, out, slack.MsgOptionTS(ref.ThreadTS))
	} else {
		postErr = c.summarizePostMessage(postCtx, ref.ChannelID, ref.ThreadTS, out)
	}
	if postErr != nil {
		c.metrics.summarize.WithLabelValues("post_error").Inc()
		c.logger.Error("summarize: delivery failed",
			"channel", ref.ChannelID, "thread", ref.ThreadTS, "private", private, "err", postErr)
		c.summarizeFeedback(parent, channelID, userID, "I finished summarizing that thread, but couldn't post the result — try again in a bit.")
		return
	}
	c.metrics.summarize.WithLabelValues("posted").Inc()
	c.logger.Info("summarize: posted summary",
		"channel", ref.ChannelID, "thread", ref.ThreadTS, "messages", len(msgs),
		"private", private, "prompt_tokens", usage.PromptTokens, "completion_tokens", usage.CompletionTokens)
}

// summarizeFeedback sends an ephemeral error/status message to the
// invoker, derived from the LEADER-scoped parent rather than the 90s
// summarize deadline (the most common cause of these posts is exactly
// that the deadline expired, and posting with the canceled context
// would fail silently) and rather than context.Background() (a detached
// context would escape run.cancelWork() on lease loss). The parent
// stays live through both cases.
func (c *Consumer) summarizeFeedback(parent context.Context, channelID, userID, msg string) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := c.summarizePostEphemeral(ctx, channelID, userID, msg); err != nil {
		// Nothing more to do: the feedback post is itself the failure
		// surface, so there is no further channel to report through.
		c.logger.Warn("summarize: feedback ephemeral failed",
			"channel", channelID, "user", userID, "err", err)
	}
}

// summarizePostEphemeral posts an ephemeral message to userID in
// channelID through the tier-4 limiter. Returns the post error so the
// pipeline can classify delivery (posted vs post_error) and tell the
// invoker when the result was lost.
func (c *Consumer) summarizePostEphemeral(ctx context.Context, channelID, userID, msg string, opts ...slack.MsgOption) error {
	if c.api == nil {
		return errors.New("summarize: no slack client")
	}
	opts = append(opts, slack.MsgOptionPostEphemeral(userID), slack.MsgOptionText(msg, false))
	return WithSlackRetry(ctx, c.limiter, methodPostEphemeral, func() error {
		_, apiErr := c.api.PostEphemeralContext(ctx, channelID, userID, opts...)
		return apiErr
	})
}

// summarizePostMessage posts a message into a thread through the tier-4
// limiter (chat.postMessage maps to tier 4 — see methodTiers). Returns
// the post error so the pipeline can classify delivery.
func (c *Consumer) summarizePostMessage(ctx context.Context, channelID, threadTS, msg string) error {
	if c.api == nil {
		return errors.New("summarize: no slack client")
	}
	return WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		_, _, apiErr := c.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionTS(threadTS),
			slack.MsgOptionText(msg, false),
		)
		return apiErr
	})
}
