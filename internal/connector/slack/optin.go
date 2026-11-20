package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/safehttp"
	"github.com/bluesky-social/diderot/internal/worker"
)

// askGateOutcome labels the decision maybeRouteAsk reached for one
// @-mention. Every outcome increments the
// diderot_slack_ask_gate_total counter and is forwarded to the
// operator ping so blocked attempts are as visible as routed ones.
type askGateOutcome string

const (
	gateAllowed                askGateOutcome = "allowed"
	gateBlockedNotEnabled      askGateOutcome = "blocked_not_enabled"
	gateBlockedExternalMembers askGateOutcome = "blocked_external_members"
	// gateBlockedExternalAsker is set when the asker is an external
	// user (guest or other-org Connect member) in a channel that has
	// NOT been whitelisted for external engagement
	// (allow_external_ask=false). External askers in whitelisted
	// channels pass through to the remaining gates and may receive
	// gateBlockedNotEnabled or gateAllowed instead.
	gateBlockedExternalAsker askGateOutcome = "blocked_external_asker"
	gateBlockedError         askGateOutcome = "blocked_gate_error"
)

// actionEnableAsk is the Block Kit action_id on the self-serve
// opt-in button. The interactive handler dispatches on it; the
// button's value carries the channel id being opted in.
const actionEnableAsk = "diderot_enable_ask"

// postGateEphemeral tells the invoker (and only the invoker — it's
// an ephemeral, visible to one user) why their @-mention didn't get
// an answer. For the not-enabled gate the message carries the
// self-serve opt-in button, UNLESS the asker is external: external
// clickers are refused by handleEnableAskClick, so a button they
// can't use is worse than no button; they get text-only pointing at
// internal members instead. For the external-members gate it names
// the external members (so the asker knows exactly who'd see the
// answers) and points at the operator for the opt-in (that gate is
// deliberately not self-serve). Best-effort with a 15s ctx, same
// posture as the other notify paths.
//
// externals carries the external member ids for the
// external-members outcome; ignored otherwise.
//
// threadTS is the original message's thread_ts and is empty for top-level
// mentions; when set, the ephemeral is posted into the thread so it
// lands where the user is looking.
//
// askerIsExternal should be true when the invoker is a Slack guest
// or other-org member; used only for the gateBlockedNotEnabled case.
func (c *Consumer) postGateEphemeral(parent context.Context, channelID, userID, threadTS string, outcome askGateOutcome, externals []string, askerIsExternal bool) {
	if c.api == nil || userID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	opts := []slack.MsgOption{slack.MsgOptionDisableLinkUnfurl()}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	switch outcome {
	case gateBlockedNotEnabled:
		if askerIsExternal {
			// External askers in a whitelisted channel: no button.
			// handleEnableAskClick refuses external clickers, so
			// presenting a button that always fails on click is worse
			// than directing them to an internal member who can
			// actually flip the flag.
			opts = append(opts, slack.MsgOptionText(c.externalNotEnabledMessage(channelID), false))
		} else {
			text := fmt.Sprintf(
				"Hi! I saw your question, but I'm not enabled to answer in <#%s> yet — answering is opt-in per channel. Anyone in the channel can turn me on:",
				channelID,
			)
			opts = append(opts, slack.MsgOptionBlocks(
				slack.NewSectionBlock(
					slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
					nil, nil,
				),
				slack.NewActionBlock("diderot_enable_ask_block",
					slack.NewButtonBlockElement(actionEnableAsk, channelID,
						slack.NewTextBlockObject(slack.PlainTextType, "Enable diderot in this channel", false, false),
					),
				),
			))
			// Fallback text for notifications / clients that don't
			// render blocks.
			opts = append(opts, slack.MsgOptionText(text, false))
		}
	case gateBlockedExternalMembers:
		msg := fmt.Sprintf(
			"Hi! I saw your question, but <#%s> contains external members (%s), and I only answer in those channels when the operator has explicitly opted it in. If you'd like that, ping %s — or remove the external members and ask again.",
			channelID, c.describeExternals(ctx, externals), c.operatorRef(ctx),
		)
		opts = append(opts, slack.MsgOptionText(msg, false))
	case gateBlockedError:
		opts = append(opts, slack.MsgOptionText(
			"Hi! I saw your question, but I couldn't check this channel's permissions just now (transient hiccup on my side). Try again in a minute.",
			false,
		))
	default:
		return
	}

	err := WithSlackRetry(ctx, c.limiter, methodPostEphemeral, func() error {
		_, apiErr := c.api.PostEphemeralContext(ctx, channelID, userID, opts...)
		return apiErr
	})
	if err != nil {
		c.logger.Warn("ask gate: ephemeral post failed",
			"channel", channelID, "user", userID, "outcome", string(outcome), "err", err)
	}
}

// describeExternals renders the external-member id list for the
// gate ephemeral: display names with email when the directory
// knows them, the raw <@Uxxx> mention as fallback. Capped at 10
// names so a guest-heavy channel doesn't produce a wall of text.
// Empty input (stale cfg row predating the external_members
// column, or a scan that failed mid-flight) degrades to the old
// generic phrase.
func (c *Consumer) describeExternals(ctx context.Context, externals []string) string {
	if len(externals) == 0 {
		return "guests or other-org users"
	}
	const maxNamed = 10
	names := make([]string, 0, len(externals))
	for i, uid := range externals {
		if i == maxNamed {
			names = append(names, fmt.Sprintf("and %d more", len(externals)-maxNamed))
			break
		}
		label := "<@" + uid + ">"
		if c.dir != nil {
			if ui, err := c.dir.LookupUserInfo(ctx, uid); err == nil {
				switch {
				case ui.Email != "" && pickName(ui.DisplayName, ui.Name) != "":
					label = fmt.Sprintf("%s — %s",
						sanitizeSlackMarkdown(pickName(ui.DisplayName, ui.Name)),
						sanitizeSlackMarkdown(ui.Email))
				case ui.Email != "":
					label = sanitizeSlackMarkdown(ui.Email)
				case pickName(ui.DisplayName, ui.Name) != "":
					label = sanitizeSlackMarkdown(pickName(ui.DisplayName, ui.Name))
				}
			}
		}
		names = append(names, label)
	}
	return strings.Join(names, ", ")
}

// HandleNewChannel is the join policy, fired by the
// SyncWorker reconcile loop exactly once per newly-discovered
// channel (public or private — discovery means the bot is a member):
//
//  1. Default ask_enabled=true so diderot answers @-mentions without
//     the per-channel opt-in dance. Only when no human has ever
//     touched the config row (DefaultAskEnabled's updated_by guard),
//     so an operator's explicit disable is never overridden. The
//     external-members gate still applies at mention time, so
//     auto-enabling never bypasses the guest protection.
//  2. For PRIVATE channels, DM the operator(s): private-channel
//     access is the sensitive grant (its history becomes
//     retrievable in-channel under the scope model), so joins
//     should be visible without watching the audit table.
func (c *Consumer) HandleNewChannel(parent context.Context, ch slack.Channel) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	enabled := false
	if c.channelConfig != nil {
		var err error
		enabled, err = c.channelConfig.DefaultAskEnabled(ctx, ch.ID, "auto (joined channel)")
		if err != nil {
			c.logger.Warn("new channel: default ask-enable failed",
				"channel", ch.ID, "err", err)
		} else if enabled {
			c.logger.Info("new channel: ask enabled by default",
				"channel", ch.ID, "name", ch.Name, "private", ch.IsPrivate)
		}
	}

	if !ch.IsPrivate {
		return
	}
	label := sanitizeSlackMarkdown(ch.Name)
	if label == "" {
		label = ch.ID
	}
	askNote := "ask is enabled by default"
	if !enabled {
		askNote = "ask config was already set and is unchanged"
	}
	if len(c.notifyEmails) == 0 || !c.notifyChannels {
		return
	}
	// Resolving the operators and their DMs run detached: this hook is
	// called inline from the reconcile loop, and a slow/many-operator
	// DM fan-out must not stall channel discovery. Detached from parent
	// too — reconcile finishing shouldn't cancel an in-flight
	// notification.
	go func() {
		defer worker.Recover(c.logger, "slack-notify-new-channel")
		dmCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, uid := range c.operatorIDs(dmCtx) {
			if err := c.dmUser(dmCtx, uid, fmt.Sprintf(
				"diderot was added to private channel #%s (<#%s>) — its history is now indexed and retrievable in-channel; %s",
				label, ch.ID, askNote,
			)); err != nil {
				c.logger.Warn("new channel: operator DM failed",
					"channel", ch.ID, "user_id", uid, "err", err)
			}
		}
	}()
}

// operatorRef renders the first operator to notify who is a workspace
// member as a clickable Slack mention, falling back to a generic phrase
// when there is none.
func (c *Consumer) operatorRef(ctx context.Context) string {
	if ids := c.operatorIDs(ctx); len(ids) > 0 {
		return "<@" + ids[0] + ">"
	}
	return "your diderot operator"
}

// handleInteractive dispatches socket-mode interactive payloads. Unknown
// action_ids are ignored so future controls don't break old pods mid-deploy.
func (c *Consumer) handleInteractive(ctx context.Context, cb slack.InteractionCallback) {
	// Message shortcuts (the three-dots menu on a message) — currently
	// only the thread summarizer. Dispatched before the block-actions
	// handling below.
	if cb.Type == slack.InteractionTypeMessageAction {
		c.handleMessageAction(ctx, cb)
		return
	}
	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	for _, action := range cb.ActionCallback.BlockActions {
		switch action.ActionID {
		case actionEnableAsk:
			c.handleEnableAskClick(ctx, cb, action.Value)
		case feedbackActionID:
			c.handleFeedbackButtonClick(ctx, cb, action.Value)
		}
	}
}

// handleEnableAskClick is the self-serve path for the ask_enabled
// gate: an internal user clicked "Enable diderot in this channel"
// on the blocked-mention ephemeral.
//
// The click re-runs the gate checks rather than trusting the
// ephemeral's context (TOCTOU: an external member may have joined,
// or the clicker may differ from the original asker):
//
//  1. Clicker must be internal. Fail-closed on directory errors.
//  2. The channel must not be external-membered-and-unopted —
//     allow_external_ask stays operator-only, so the click refuses
//     and points at the operator instead of enabling.
//
// On success, flips ask_enabled=true with the clicker recorded in
// the audit column, replaces the ephemeral via response_url, and
// DMs the operator(s) so self-serve enables are visible.
func (c *Consumer) handleEnableAskClick(parent context.Context, cb slack.InteractionCallback, channelID string) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	if channelID == "" {
		channelID = cb.Channel.ID
	}
	clicker := cb.User.ID
	if channelID == "" || clicker == "" || c.channelConfig == nil {
		c.metrics.askOptin.WithLabelValues("error").Inc()
		c.respondInteraction(ctx, cb.ResponseURL,
			"Something went wrong handling that click — try mentioning me again.")
		return
	}

	if c.dir != nil {
		ui, err := c.dir.LookupUserInfo(ctx, clicker)
		if err == nil && ui.IsNegativeCache {
			err = errors.New("user unknown to Slack")
		}
		if err != nil {
			c.logger.Warn("ask opt-in: clicker lookup failed; refusing",
				"channel", channelID, "user", clicker, "err", err)
			c.metrics.askOptin.WithLabelValues("error").Inc()
			c.respondInteraction(ctx, cb.ResponseURL,
				"I couldn't verify your account just now — try again in a minute.")
			return
		}
		if ui.IsExternal() {
			c.logger.Info("ask opt-in: external clicker refused",
				"channel", channelID, "user", clicker)
			c.metrics.askOptin.WithLabelValues("refused_external_clicker").Inc()
			c.respondInteraction(ctx, cb.ResponseURL,
				"This action isn't available to guest accounts.")
			return
		}
	}

	cfg, err := c.channelConfig.EnsureExternalMembersFresh(ctx, channelID)
	if err != nil {
		c.logger.Warn("ask opt-in: external-members scan failed; refusing",
			"channel", channelID, "user", clicker, "err", err)
		c.metrics.askOptin.WithLabelValues("error").Inc()
		c.respondInteraction(ctx, cb.ResponseURL,
			"I couldn't verify this channel's membership just now — try again in a minute.")
		return
	}
	if cfg.HasExternalMembers && !cfg.AllowExternalAsk {
		c.logger.Info("ask opt-in: channel has external members; refusing self-serve",
			"channel", channelID, "user", clicker)
		c.metrics.askOptin.WithLabelValues("refused_external_members").Inc()
		c.respondInteraction(ctx, cb.ResponseURL, fmt.Sprintf(
			"This channel contains external members, so enabling me here needs the operator — ping %s.",
			c.operatorRef(ctx),
		))
		return
	}

	updatedBy := clicker
	if c.dir != nil {
		if name, err := c.dir.LookupUser(ctx, clicker); err == nil && name != "" {
			updatedBy = name
		}
	}
	updatedBy += " (slack opt-in)"
	if err := c.channelConfig.SetAskEnabled(ctx, channelID, true, updatedBy); err != nil {
		c.logger.Error("ask opt-in: SetAskEnabled failed",
			"channel", channelID, "user", clicker, "err", err)
		c.metrics.askOptin.WithLabelValues("error").Inc()
		c.respondInteraction(ctx, cb.ResponseURL,
			"Something went wrong flipping the switch — try again in a minute.")
		return
	}

	c.logger.Info("ask opt-in: channel enabled via self-serve",
		"channel", channelID, "user", clicker)
	c.metrics.askOptin.WithLabelValues("enabled").Inc()
	c.respondInteraction(ctx, cb.ResponseURL,
		"Done — I'll answer @-mentions in this channel now. Ask your question again and I'll pick it up.")

	clickerLabel := clicker
	if c.dir != nil {
		if name, err := c.dir.LookupUser(ctx, clicker); err == nil && name != "" {
			clickerLabel = sanitizeSlackMarkdown(name)
		}
	}
	if !c.notifyChannels {
		return
	}
	for _, uid := range c.operatorIDs(ctx) {
		if uid == clicker {
			continue
		}
		if err := c.dmUser(ctx, uid, fmt.Sprintf(
			"%s enabled ask in <#%s> (self-serve opt-in)", clickerLabel, channelID,
		)); err != nil {
			c.logger.Warn("ask opt-in: operator DM failed", "user_id", uid, "err", err)
		}
	}
}

// respondInteraction replaces the clicked ephemeral via the
// interaction's response_url. Best-effort: a failed replacement
// leaves the stale button visible, which is cosmetic (re-clicks
// re-run the idempotent handler).
func (c *Consumer) respondInteraction(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}
	err := slack.PostWebhookCustomHTTPContext(ctx, responseURL, safehttp.NewClient(safehttp.Options{}), &slack.WebhookMessage{
		Text:            text,
		ReplaceOriginal: true,
		ResponseType:    "ephemeral",
	})
	if err != nil {
		c.logger.Warn("ask opt-in: response_url post failed", "err", err)
	}
}
