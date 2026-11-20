package slack

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/askchat"
	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// reactionsAPI is the subset of the Slack client that the feedback
// sweep needs. Isolated as an interface so tests can inject a fake
// without constructing a real *slack.Client.
type reactionsAPI interface {
	GetReactionsContext(ctx context.Context, item slackgo.ItemRef, params slackgo.GetReactionsParameters) (slackgo.ReactedItem, error)
}

// feedbackSweepWindow is how far back ask_answer_messages rows are
// scanned during the reconcile sweep. 7 days covers all plausible
// live-reaction windows without growing unbounded.
const feedbackSweepWindow = 7 * 24 * time.Hour

// FeedbackSweepInterval is the cadence for the reconcile sweep that
// catches reactions missed during socket-mode disconnect windows.
// Exported so cmd/diderot's Slack source (slack_connector.go) can wire
// the worker.Runner without importing the constant inline.
const FeedbackSweepInterval = time.Hour

const (
	feedbackActionID      = "diderot_ask_feedback"
	feedbackPositiveValue = "positive_feedback"
	feedbackNegativeValue = "negative_feedback"
	// Colons cannot occur in Slack's raw reaction names, so this sentinel
	// cannot collide with a real custom emoji in the shared DB column.
	feedbackButtonsRowKey  = "button:feedback"
	feedbackButtonsBlockID = "diderot_ask_feedback_block"
)

// feedbackButtonsBlock renders Slack's purpose-built feedback control in a
// context_actions block. Slack supplies the icons; button text is the hover
// label.
func feedbackButtonsBlock() *slackgo.ContextActionsBlock {
	positive := slackgo.NewFeedbackButton(
		slackgo.NewTextBlockObject(slackgo.PlainTextType, "Mark this answer as helpful", true, false),
		feedbackPositiveValue,
	)
	negative := slackgo.NewFeedbackButton(
		slackgo.NewTextBlockObject(slackgo.PlainTextType, "Mark this answer as not helpful", true, false),
		feedbackNegativeValue,
	)
	buttons := slackgo.NewFeedbackButtonsBlockElement(feedbackActionID, positive, negative)
	return slackgo.NewContextActionsBlock(feedbackButtonsBlockID, buttons)
}

// verdictForEmoji maps a raw Slack reaction name to a feedback
// verdict. A skin-tone variant's name carries its tone as a suffix
// ("+1::skin-tone-3"), and is the same thumb. +1 and -1 are Slack's
// aliases for thumbsup/thumbsdown.
func verdictForEmoji(name string) string {
	name, _, _ = strings.Cut(name, "::skin-tone-")
	switch name {
	case "+1", "thumbsup":
		return "up"
	case "-1", "thumbsdown":
		return "down"
	default:
		return "other"
	}
}

// insertAnswerMessage registers a posted answer chunk in the shared answer
// store, so reaction events resolve back to the originating session.
func insertAnswerMessage(ctx context.Context, pool *pgxpool.Pool, channelID, messageTS, sessionID string, turn, chunkIndex int) error {
	return (&askchat.AnswerStore{Pool: pool}).Insert(ctx, channelID, messageTS, sessionID, turn, chunkIndex)
}

// upsertFeedback records a rating of an answer chunk on reaction_added, a
// button click, or the sweep seeing a live reaction
// (askchat.AnswerStore.UpsertFeedback).
func upsertFeedback(ctx context.Context, pool *pgxpool.Pool, sessionID string, turn int, channelID, messageTS, reactor, emoji, verdict string) error {
	return (&askchat.AnswerStore{Pool: pool}).UpsertFeedback(ctx, sessionID, turn, channelID, messageTS, reactor, emoji, verdict)
}

// stampFeedbackRemoved marks a rating retracted.
func stampFeedbackRemoved(ctx context.Context, pool *pgxpool.Pool, sessionID string, turn int, messageTS, reactor, emoji string) error {
	return (&askchat.AnswerStore{Pool: pool}).StampFeedbackRemoved(ctx, sessionID, turn, messageTS, reactor, emoji)
}

// lookupAnswerMessage resolves a message to the session and turn whose
// answer it is; found is false for any other message.
func lookupAnswerMessage(ctx context.Context, pool *pgxpool.Pool, channelID, messageTS string) (sessionID string, turn int, found bool, err error) {
	return (&askchat.AnswerStore{Pool: pool}).Lookup(ctx, channelID, messageTS)
}

// handleReactionAdded processes a reaction_added event. It resolves
// the reaction target to an answer message, then upserts a feedback
// row. Always returns true (ack) — reactions must never wedge the
// event loop.
//
// Reactions on non-answer messages (the common case — most Slack
// messages are not diderot answers) are dropped after a single cheap
// PK lookup.
//
// NOTE: the reaction_added and reaction_removed event subscriptions
// require the reactions:read scope on the Slack app manifest.
func (c *Consumer) handleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	if ev.Item.Type != "message" {
		return
	}
	// Ignore reactions from the bot itself — we add/remove :eyes: and
	// :white_check_mark: on user messages as part of the ask UX.
	if self := c.SelfUserID(); self != "" && ev.User == self {
		return
	}

	sessionID, turn, found, err := lookupAnswerMessage(ctx, c.pool, ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		c.logger.Warn("ask feedback: lookup answer message failed",
			"channel", ev.Item.Channel, "ts", ev.Item.Timestamp, "err", err)
		return
	}
	if !found {
		return // reaction on a non-answer message — common case, drop
	}

	verdict := verdictForEmoji(ev.Reaction)
	if err := upsertFeedback(ctx, c.pool, sessionID, turn, ev.Item.Channel, ev.Item.Timestamp, ev.User, ev.Reaction, verdict); err != nil {
		c.logger.Warn("ask feedback: upsert failed",
			"session_id", sessionID, "turn", turn, "reactor", ev.User, "emoji", ev.Reaction, "err", err)
		return
	}
	c.metrics.askFeedback.WithLabelValues(verdict).Inc()
}

// handleReactionRemoved processes a reaction_removed event. Sets
// removed_at on the matching feedback row. Always acks.
//
// NOTE: same reactions:read scope requirement as handleReactionAdded.
func (c *Consumer) handleReactionRemoved(ctx context.Context, ev *slackevents.ReactionRemovedEvent) {
	if ev.Item.Type != "message" {
		return
	}
	// Self-reactions are never written as feedback, so no row to stamp.
	if self := c.SelfUserID(); self != "" && ev.User == self {
		return
	}

	sessionID, turn, found, err := lookupAnswerMessage(ctx, c.pool, ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		c.logger.Warn("ask feedback: lookup answer message for removal failed",
			"channel", ev.Item.Channel, "ts", ev.Item.Timestamp, "err", err)
		return
	}
	if !found {
		return
	}

	if err := stampFeedbackRemoved(ctx, c.pool, sessionID, turn, ev.Item.Timestamp, ev.User, ev.Reaction); err != nil {
		c.logger.Warn("ask feedback: stamp removed_at failed",
			"session_id", sessionID, "turn", turn, "reactor", ev.User, "emoji", ev.Reaction, "err", err)
	}
}

// handleFeedbackButtonClick stores a native feedback-buttons selection against
// the answer message. Button votes use one stable row key per user/message, so
// changing from positive to negative updates the existing vote instead of
// leaving both active. Reaction feedback remains supported independently.
func (c *Consumer) handleFeedbackButtonClick(ctx context.Context, cb slackgo.InteractionCallback, value string) {
	verdict := ""
	switch value {
	case feedbackPositiveValue:
		verdict = "up"
	case feedbackNegativeValue:
		verdict = "down"
	default:
		return
	}

	channelID := cb.Container.ChannelID
	if channelID == "" {
		channelID = cb.Channel.ID
	}
	messageTS := cb.Container.MessageTs
	if messageTS == "" {
		messageTS = cb.MessageTs
	}
	clicker := cb.User.ID
	if channelID == "" || messageTS == "" || clicker == "" {
		c.logger.Warn("ask feedback button: incomplete interaction payload",
			"channel", channelID, "msg_ts", messageTS, "user", clicker)
		return
	}

	sessionID, turn, found, err := lookupAnswerMessage(ctx, c.pool, channelID, messageTS)
	if err != nil {
		c.logger.Warn("ask feedback button: lookup answer message failed",
			"channel", channelID, "ts", messageTS, "err", err)
		return
	}
	if !found {
		return
	}
	if err := upsertFeedback(ctx, c.pool, sessionID, turn, channelID, messageTS, clicker, feedbackButtonsRowKey, verdict); err != nil {
		c.logger.Warn("ask feedback button: upsert failed",
			"session_id", sessionID, "turn", turn, "user", clicker, "verdict", verdict, "err", err)
		return
	}
	c.metrics.askFeedback.WithLabelValues(verdict).Inc()
}

// sweepAnswerMessageRow holds the minimal data needed for one
// reactions.get call during the reconcile sweep.
type sweepAnswerMessageRow struct {
	channelID string
	messageTS string
	sessionID string
	turn      int
}

// SweepFeedback reconciles ask_feedback against live Slack reactions
// for all ask_answer_messages rows created in the last
// feedbackSweepWindow. This backstop catches reactions that arrived
// during socket-mode disconnect windows (at-most-once past ~30s).
//
// Volume at current load: ~15 answers/week, so a 7-day window holds
// ~15 rows. Each row costs one reactions.get call (Tier 3, ~50/min);
// the sweep easily fits within rate limits.
//
// c.reactions must be set (Run sets it to c.api; tests inject a
// fake via withReactionsAPI). When nil, SweepFeedback is a no-op.
func (c *Consumer) SweepFeedback(ctx context.Context) error {
	if c.reactions == nil {
		return nil // no-op when consumer isn't connected
	}

	// Load all answer messages from the sweep window.
	rows, err := c.pool.Query(ctx, `
		SELECT channel_id, message_ts, session_id::text, turn
		  FROM ask_answer_messages
		 WHERE created_at > now() - $1::interval
		 ORDER BY created_at DESC
	`, intervalString(feedbackSweepWindow))
	if err != nil {
		return err
	}
	var msgs []sweepAnswerMessageRow
	for rows.Next() {
		var r sweepAnswerMessageRow
		if err := rows.Scan(&r.channelID, &r.messageTS, &r.sessionID, &r.turn); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, msg := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.reconcileMessageReactions(ctx, msg); err != nil {
			// Log per-message errors and continue; one bad message
			// shouldn't abort the sweep for the rest.
			c.logger.Warn("ask feedback sweep: reconcile message failed",
				"channel", msg.channelID, "msg_ts", msg.messageTS, "err", err)
		}
	}
	return nil
}

// reconcileMessageReactions calls reactions.get for one answer
// message and reconciles the live reaction set against ask_feedback.
// Present reactions absent from DB → upserted. DB rows for this
// specific message_ts whose (reactor, emoji) is absent from the live
// message → removed_at stamped.
//
// Scoping to message_ts is load-bearing: a multi-chunk answer has one
// ask_answer_messages row per chunk. Without scoping, sweeping chunk 2
// would stamp removed_at on chunk 1's reactions (which live on a
// different message_ts and thus aren't visible in chunk 2's
// reactions.get response).
func (c *Consumer) reconcileMessageReactions(ctx context.Context, msg sweepAnswerMessageRow) error {
	// Capture the snapshot time from the DB clock BEFORE calling
	// reactions.get. Using the DB clock (not time.Now()) eliminates
	// skew between the Go process and the Postgres server, since
	// added_at is set by DB now() on upsert. Any ask_feedback row
	// with added_at > snapshotAt was inserted by handleReactionAdded
	// after our snapshot — it's live by definition and must not be
	// stamped removed even if it's absent from liveSet.
	var snapshotAt time.Time
	if err := c.pool.QueryRow(ctx, `SELECT now()`).Scan(&snapshotAt); err != nil {
		return err
	}

	var liveItem slackgo.ReactedItem
	err := WithSlackRetry(ctx, c.limiter, methodReactionsGet, func() error {
		var apiErr error
		liveItem, apiErr = c.reactions.GetReactionsContext(ctx,
			slackgo.NewRefToMessage(msg.channelID, msg.messageTS),
			slackgo.GetReactionsParameters{Full: true},
		)
		return apiErr
	})
	if err != nil {
		return err
	}

	// Build a set of (reactor, emoji) pairs present on the live message.
	type reactionKey struct{ reactor, emoji string }
	liveSet := make(map[reactionKey]struct{})
	for _, r := range liveItem.Reactions {
		for _, user := range r.Users {
			if self := c.SelfUserID(); self != "" && user == self {
				continue // skip bot's own reactions
			}
			key := reactionKey{user, r.Name}
			liveSet[key] = struct{}{}
			verdict := verdictForEmoji(r.Name)
			if err := upsertFeedback(ctx, c.pool, msg.sessionID, msg.turn, msg.channelID, msg.messageTS, user, r.Name, verdict); err != nil {
				c.logger.Warn("ask feedback sweep: upsert failed",
					"session_id", msg.sessionID, "reactor", user, "emoji", r.Name, "err", err)
			}
		}
	}

	// Stamp removed_at for DB rows scoped to this message_ts whose
	// reaction is no longer live. Two guards:
	//   message_ts: scopes to this chunk only (prevents chunk 2's
	//     sweep from touching chunk 1's rows).
	//   added_at <= snapshotAt: excludes rows inserted by
	//     handleReactionAdded AFTER our reactions.get call — those
	//     rows are live by definition; they were absent from liveSet
	//     only because the reaction arrived after our snapshot.
	dbRows, err := c.pool.Query(ctx, `
		SELECT reactor, emoji
		  FROM ask_feedback
		 WHERE session_id = $1::uuid
		   AND turn = $2
		   AND message_ts = $3
		   AND removed_at IS NULL
		   AND added_at <= $4
		   AND emoji <> $5
	`, msg.sessionID, msg.turn, msg.messageTS, snapshotAt, feedbackButtonsRowKey)
	if err != nil {
		return err
	}
	var toRemove []reactionKey
	for dbRows.Next() {
		var k reactionKey
		if scanErr := dbRows.Scan(&k.reactor, &k.emoji); scanErr != nil {
			dbRows.Close()
			return scanErr
		}
		if _, present := liveSet[k]; !present {
			toRemove = append(toRemove, k)
		}
	}
	dbRows.Close()
	if err := dbRows.Err(); err != nil {
		return err
	}

	for _, k := range toRemove {
		if err := stampFeedbackRemoved(ctx, c.pool, msg.sessionID, msg.turn, msg.messageTS, k.reactor, k.emoji); err != nil {
			c.logger.Warn("ask feedback sweep: stamp removed_at failed",
				"session_id", msg.sessionID, "reactor", k.reactor, "emoji", k.emoji, "err", err)
		}
	}
	return nil
}
