package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Consumer runs the Slack Socket Mode event loop. Leader-only —
// call Run from a goroutine wrapped in leader.Run so only one
// replica holds the WebSocket connection to Slack.
//
// On each received message event:
//  1. Ack the socket-mode request, before any handler work, so
//     Slack's 3s ack deadline never waits on the database.
//  2. ParseMessage -> ParseResult (Event or Delete tuple)
//  3. For an Event: events.DBSink.Upsert, with the
//     slack_message_details side-table row in the same transaction.
//  4. For a Delete: events.DBSink.Delete.
//
// The ack carries no "durably handled" meaning: Slack does not
// redeliver an event whose handler fails after it (leader flip, DB
// hiccup). The SyncWorker tail sweep is the backstop: it re-reads
// recent history and repairs a missed upsert. It repairs a missed
// delete only where history still returns a tombstone (see
// ingestRawMessage).
type Consumer struct {
	pool          *pgxpool.Pool
	sink          *events.DBSink
	appToken      string // xapp-...
	botToken      string // xoxb-...
	workspaceHost string // e.g. "example.slack.com"
	logger        *slog.Logger
	metrics       *consumerMetrics

	// dir + prefix are created inside Run() once we have the
	// constructed slack.Client; handlers reach them via c.dir / c.prefix.
	dir    *Directory
	prefix *PrefixBuilder
	// threadContextFetcher is injectable only for tests that exercise the
	// fetch-failure watermark path. nil uses fetchThreadContextBetween.
	threadContextFetcher func(context.Context, *pgxpool.Pool, string, string, time.Time, time.Time) (string, error)

	// sync is the continuous-pull worker. When non-nil,
	// the consumer drives priority bumps on live MessageEvents and
	// disable-on-leave on member_left_channel. Optional so the
	// consumer can still run standalone for tests.
	sync *SyncWorker

	// ask routes user messages addressed to @diderot (or follow-ups
	// in an existing thread session), plus every eligible direct message,
	// to the agentic /ask runner.
	// Optional — when nil, the consumer is ingest-only.
	ask AskRouter
	// askGate closes Ask while the install is in setup.
	askGate AskGate

	// dmScopes resolves the private-channel grants for the verified Slack
	// identity in a direct message. Production shares one resolver with
	// scheduled reports so both paths use the same five-minute revocation
	// cache. nil degrades DM turns to public+internal+owner scope only.
	dmScopes DMScopeResolver

	// api is the slack.Client used to post replies for ask.
	// Captured in Run() once and shared with the ask goroutines so
	// they can reach Slack's web API without recreating clients.
	api *slack.Client
	// progress drives the thread's Slack agent session status. Production
	// binds it to api in Run; tests can inject a transport without a live
	// workspace.
	progress progressTransport
	// working records the threads a turn has set working, so a turn whose
	// process dies is cleared (agent_sessions.go). nil uses the database's
	// table when the consumer has a pool; tests inject a fake.
	working workingThreadStore
	// turns are the Ask turns running in this process, for the stop button.
	turns askchat.Turns

	// selfUserID is the bot's Slack user id (from auth.test).
	// Discovered in Run(). Used to filter member_joined_channel
	// events to "the bot joined a new channel" vs. "someone else
	// joined a channel the bot's already in", and to detect
	// `<@Uxxx>` mentions in raw event text for ask routing.
	//
	// atomic.Value because the value is written from Run() (events
	// goroutine) and read from both the same goroutine AND the
	// backfill / sync-worker goroutines (via SelfUserID() / the
	// thunk plumbed through bf.WithSelfUserIDFn). All reads MUST
	// go through SelfUserID(); direct field access from outside
	// the goroutine that writes Run() is racy.
	selfUserID atomic.Value // string

	// notifyEmails are the emails of the operators who get a DM whenever
	// someone @-mentions the bot. Operator-awareness feature — lets
	// the operator follow along when the bot is being asked
	// questions in channels they're not actively watching.
	// Empty means no notifications. The Slack connector's "Operators to
	// notify" setting, resolved to Slack users by operatorIDs.
	notifyEmails []string
	// notifyQuestions, notifySearches and notifyChannels choose which
	// events DM the operators (the connector's "DM operators about ..."
	// settings).
	notifyQuestions, notifySearches, notifyChannels bool
	// unresolvedOperators holds the notify emails already warned about as
	// unresolvable, so each warns once per run.
	unresolvedOperators sync.Map
	// notifyExcludeEmails holds lowercased asker emails whose queries
	// never produce those DMs (the connector's excluded askers).
	notifyExcludeEmails map[string]struct{}
	// askSessionViewerBaseURL is the validated public Diderot origin used
	// to deep-link operator notifications into the Ask flight recorder.
	askSessionViewerBaseURL string
	// orgName returns the organization's current name for replies to
	// external members; nil or empty uses neutral wording.
	orgName func() string

	// channelConfig gates ask routing for channels containing
	// external members. Optional — when nil the gate degrades to
	// "external askers blocked, but channel-membership gate skipped"
	// (the existing tests run without one). Production wires it via
	// WithChannelConfig in cmd/diderot's Slack source.
	channelConfig     *ChannelConfigStore
	channelAuthorizer *ChannelAuthorizer

	// seen is the in-memory dedupe for socket-mode redelivery (see
	// dedupe.go). Lazy-init in Run(); nil-safe at call sites for
	// tests that drive handleEventsAPI directly without going
	// through Run.
	seen *seenStore
	// mentions records the channel mentions routed to Ask, by (channel,
	// claim key). Slack delivers a mention in a channel the bot is in both as
	// a message event and as an app_mention; whichever arrives first is
	// routed and the other drops quietly (app_mention.go). Lazy-init in Run;
	// nil skips the check.
	mentions *seenStore

	// blacklist is the explicit "do not ingest" override list. When
	// set, the live MessageEvent path checks every inbound message
	// against the blacklist BEFORE sink.Upsert and silently drops
	// (with a metric) for blacklisted channels. Optional — nil is
	// "membership = ingest" with no exception list, which is the
	// pre-blacklist behavior for tests.
	blacklist *IngestBlacklist

	// limiter is the global Slack-API budget governor, shared with
	// directory / backfill / channel_config / sync_worker. Optional
	// — nil disables proactive pacing for write paths (tests). All
	// write-side slack-go calls (chat.postMessage, reactions.*,
	// chat.update, conversations.open, chat.getPermalink) go through
	// it via WithSlackRetry so 429s are observable per-method and
	// Retry-After is honored uniformly.
	limiter *Limiter

	// ingest emits per-path ingest counters + lag histogram at sink-
	// write time for the socket path. Optional. Production wires it
	// via WithIngestMetrics from cmd/diderot's Slack source.
	ingest *ingestMetrics

	// reactions is used by SweepFeedback to call reactions.get.
	// Production sets it to c.api in Run(); tests inject a fake via
	// withReactionsAPI so the sweep can run without a live Slack
	// connection. nil causes SweepFeedback to no-op (same guard as
	// c.api == nil).
	reactions reactionsAPI

	// disableCaptionInjection, when true, makes maybeRouteAsk skip the
	// caption barrier (no pre-ack/wait/notices) and the mention-image
	// caption append. It exists ONLY as a negative control: a
	// deliberately-broken toggle used to prove the Slack e2e test
	// (internal/e2e/slack) actually fails when the caption reaches
	// nothing. Set from DIDEROT_DISABLE_CAPTION_INJECTION in cmd/diderot's
	// Slack source; defaults false. Do NOT leave this enabled in a
	// deployment: it answers before a screenshot is captioned, on purpose.
	disableCaptionInjection bool

	// run tracks the asynchronous handlers and ask turns accepted by the
	// current Socket Mode connection. It lets process shutdown stop intake
	// immediately while preserving already-acked work through the pod grace.
	run consumerRunTracker

	// summarizer backs the "Summarize thread" message shortcuts (see
	// summarize.go). nil = shortcuts reply "not configured"; ingest and
	// ask run unchanged.
	summarizer *Summarizer
}

// AskRouter is the subset of internal/ask the consumer needs.
// Defined as an interface so we don't introduce a slack→ask import
// dependency (ask already depends on retrieve, which has nothing
// to do with slack). Implementation-side, *ask.Runner has these
// methods directly; cmd/diderot's Slack source wires slack to ask.
type AskRouter interface {
	// askerEmail is stamped as the session's owner_email, but only on
	// the call that creates the session — the original asker, not
	// whoever's asking on a given follow-up turn.
	LookupOrCreateSlackSession(ctx context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error)
	// SlackSessionState reports whether a session row exists for the
	// thread and returns the context_through high-water mark (zero
	// when NULL or when no session exists). Used to gate routing and
	// to determine which thread messages have not yet been delivered
	// into the session for context injection.
	SlackSessionState(ctx context.Context, channelID, threadTS string) (known bool, contextThrough time.Time, err error)
	// ExecuteTurn is the same transport-neutral state-machine entrypoint used
	// by HTTP. Slack fills thread and delivery context before calling it.
	// StatusEmitter may be nil to skip progress notifications. Citations are
	// deduped + capped by the runner. ThreadContext is a pre-rendered timestamped
	// transcript of thread messages the session has not yet seen
	// (non-empty when interleaved human messages exist since the last
	// turn). contextThrough is the occurred_at of the @-mention; the
	// runner advances the session's high-water mark to this value
	// after a successful persist. asker carries the per-turn identity
	// context the runner injects into the user message envelope
	// (name, email, slack id, asker's-local time); pass nil to fall
	// back to the minimal UTC-only envelope. allowedScopes is the
	// visibility boundary for every retrieval this turn — computed
	// from the channel audience for shared channels or the verified
	// user audience for DMs (see scopesForChannel and scopesForDM);
	// must be non-empty (the runner rejects an empty set). assistantTurn is the monotonic turn index of the
	// persisted assistant message; the consumer uses it when
	// registering answer chunks in ask_answer_messages.
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
}

// DMScopeResolver is the per-identity visibility resolver used by direct
// messages. UserScopeResolver implements it; the narrow interface keeps
// routing tests independent from Slack's users.conversations API.
type DMScopeResolver interface {
	ScopesForEmail(ctx context.Context, email string) []string
}

// WithSyncWorker wires the continuous-pull worker. The consumer
// uses it for two paths:
//   - PriorityBump on every live MessageEvent (channel sweep
//     order pulls forward when activity is observed).
//   - Disable on member_left_channel (the row stays for operator
//     visibility but ingest_enabled flips false).
//
// Optional only for tests — production must wire it.
func (c *Consumer) WithSyncWorker(w *SyncWorker) *Consumer {
	c.sync = w
	return c
}

// WithAsk wires the /ask runner so messages addressed to @diderot
// (or follow-ups in an existing thread session) get routed to the
// agentic loop in addition to being ingested. No-op when nil; the
// consumer remains ingest-only.
func (c *Consumer) WithAsk(r AskRouter) *Consumer {
	c.ask = r
	return c
}

// AskGate is the setup gate (*settings.Store): Ask is open to everyone once
// the install is live, and only to administrators before.
type AskGate interface {
	AskAllowed(admin bool) bool
}

// WithAskGate closes Ask in Slack while the install is in setup. A Slack
// user carries no administrator groups, so the gate refuses everyone then.
func (c *Consumer) WithAskGate(gate AskGate) *Consumer {
	c.askGate = gate
	return c
}

// WithDMScopeResolver wires audience-scoped private-channel grants for direct
// messages. The verified Slack profile supplies the email; this resolver adds
// only channels that user currently shares with diderot.
func (c *Consumer) WithDMScopeResolver(r DMScopeResolver) *Consumer {
	c.dmScopes = r
	return c
}

// WithNotifyEmails configures the operators, by email, who get
// operator-awareness DMs for meaningful Diderot queries across Slack,
// HTTP, and MCP. Empty list means no notifications.
func (c *Consumer) WithNotifyEmails(emails []string) *Consumer {
	c.notifyEmails = emails
	return c
}

// WithNotifyEvents chooses what the operators are DMed about: questions
// (Slack, API and MCP asks), agent searches (MCP retrieve and list), and
// channel changes (added to a private channel, Ask turned on).
func (c *Consumer) WithNotifyEvents(questions, searches, channels bool) *Consumer {
	c.notifyQuestions, c.notifySearches, c.notifyChannels = questions, searches, channels
	return c
}

// WithNotifyExcludeEmails configures askers whose queries never produce
// operator-awareness DMs, compared case-insensitively.
func (c *Consumer) WithNotifyExcludeEmails(emails []string) *Consumer {
	c.notifyExcludeEmails = make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			c.notifyExcludeEmails[email] = struct{}{}
		}
	}
	return c
}

// WithAskSessionViewerURL configures the validated browser-reachable Diderot
// origin. Configuration validation happens before the consumer is built.
func (c *Consumer) WithAskSessionViewerURL(baseURL string) *Consumer {
	c.askSessionViewerBaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return c
}

// WithOrgName reads the organization's name, per reply, for replies that
// tell external members what they can't do. An empty name keeps the wording
// neutral.
func (c *Consumer) WithOrgName(name func() string) *Consumer {
	c.orgName = name
	return c
}

func (c *Consumer) organization() string {
	if c.orgName == nil {
		return ""
	}
	return strings.TrimSpace(c.orgName())
}

// externalNotEnabledMessage tells an external asker that Ask is off in the
// channel and who can turn it on.
func (c *Consumer) externalNotEnabledMessage(channelID string) string {
	enabler := "an internal member"
	if org := c.organization(); org != "" {
		enabler = "someone from the " + org + " team"
	}
	return fmt.Sprintf("Hi! I saw your question, but I'm not enabled to answer in <#%s> yet — answering is opt-in per channel. Ask %s in this channel to enable me.",
		channelID, enabler)
}

// externalDMDeniedMessage tells an external member that DMs are for
// internal members only.
func (c *Consumer) externalDMDeniedMessage() string {
	members := "internal members"
	if org := c.organization(); org != "" {
		members = "internal " + org + " members"
	}
	return "sorry, Diderot DMs are only available to " + members + "."
}

// WithChannelConfig wires the channel-config store used by the
// external-membership gate. When set, ask turns are blocked for
// channels containing external members unless allow_external_ask
// is true. Without it, only the per-asker gate applies.
func (c *Consumer) WithChannelConfig(s *ChannelConfigStore) *Consumer {
	c.channelConfig = s
	return c
}

func (c *Consumer) WithChannelAuthorizer(authorizer *ChannelAuthorizer) *Consumer {
	c.channelAuthorizer = authorizer
	return c
}

// WithLimiter wires the global Slack-API budget governor. Without
// it, write-side slack-go calls bypass the limiter (the pre-1c
// behavior); with it, every PostMessage / Update / reaction /
// OpenConversation / Permalink goes through Wait + Record* and
// honors Retry-After on 429.
func (c *Consumer) WithLimiter(l *Limiter) *Consumer {
	c.limiter = l
	return c
}

// WithIngestMetrics wires the per-path ingest observability metrics
// for the socket-mode live path. The other ingest paths (sync,
// thread_sync, backfill) wire the same handle into Backfill.
func (c *Consumer) WithIngestMetrics(m *IngestMetrics) *Consumer {
	c.ingest = m
	return c
}

// WithDisableCaptionInjection sets the negative-control flag that makes
// maybeRouteAsk skip the caption barrier and the mention-image caption
// append. It exists ONLY to let the Slack e2e test be shown to fail
// against known-broken behavior; production wires it false (from an
// unset env var).
func (c *Consumer) WithDisableCaptionInjection(disable bool) *Consumer {
	c.disableCaptionInjection = disable
	return c
}

// WithIngestBlacklist wires the do-not-ingest override list. The
// live MessageEvent path consults it before sink.Upsert; matching
// channels are silently dropped (with a metric for ops visibility).
// Without it, every channel the bot is a member of gets ingested
// (membership = ingest, no exceptions).
func (c *Consumer) WithIngestBlacklist(b *IngestBlacklist) *Consumer {
	c.blacklist = b
	return c
}

// SelfUserID returns the bot's own slack user_id. Empty string
// before Run has executed auth.test (consumer hasn't started yet).
// Exposed so callers like backfill can plumb the lazy thunk
// (b.WithSelfUserIDFn(consumer.SelfUserID)) at wire-up time, then
// pick up the populated value on the next ingestMessageCore call
// after the consumer authenticates.
func (c *Consumer) SelfUserID() string {
	if v := c.selfUserID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// NewConsumer wires up a consumer. logger may be nil (falls back to
// slog.Default). reg may be nil for tests (drops metrics on the floor).
func NewConsumer(
	pool *pgxpool.Pool,
	sink *events.DBSink,
	appToken, botToken, workspaceHost string,
	logger *slog.Logger,
	reg prometheus.Registerer,
) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &Consumer{
		pool:          pool,
		sink:          sink,
		appToken:      appToken,
		botToken:      botToken,
		workspaceHost: workspaceHost,
		logger:        logger,
		metrics:       newConsumerMetrics(reg),
	}
}

// Run blocks until ctx is cancelled or the underlying socket-mode
// client returns a terminal error. Safe to restart (leader flip)
// — the socket-mode client cleanly closes its WebSocket on
// ctx.Done.
func (c *Consumer) Run(ctx context.Context) error {
	return c.RunWithProcessContext(ctx, ctx)
}

// RunWithProcessContext runs one leader-scoped Socket Mode connection while
// using processCtx to distinguish a pod shutdown from an ordinary leadership
// loss. SIGTERM drains already-acked handlers and their active asks; losing the
// lease cancels them immediately so two leaders cannot answer concurrently.
func (c *Consumer) RunWithProcessContext(ctx, processCtx context.Context) error {
	if c.appToken == "" || c.botToken == "" {
		return errors.New("slack: appToken and botToken are required")
	}
	run := newConsumerRunState(ctx, c.logger)
	c.run.install(run)
	defer c.run.uninstall(run)
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			// Close admission as soon as leadership ends, before the socket
			// reader can consume and acknowledge already-queued envelopes.
			run.stopHandlers()
			// The process context is the parent of the leader context in
			// production, so its Err is already set when SIGTERM propagates
			// here. A lease-only cancellation leaves it live and must stop
			// accepted work immediately to preserve single-leader answers.
			if processCtx == nil || processCtx.Err() == nil {
				run.cancelWork()
			}
		case <-watchStop:
		}
	}()

	api := c.api
	if api == nil {
		api = slack.New(c.botToken, slack.OptionAppLevelToken(c.appToken))
		c.api = api
	}
	client := socketmode.New(api)
	c.progress = &slackProgressTransport{api: api, limiter: c.limiter}
	c.reactions = api
	c.dir = NewDirectory(c.pool, api)
	c.prefix = NewPrefixBuilder(c.dir, c.pool)
	if c.seen == nil {
		c.seen = newSeenStore(0) // default 10m TTL
	}
	if c.mentions == nil {
		c.mentions = newSeenStore(0)
	}

	// Discover our own user id. Required for member_joined_channel
	// filtering; if the lookup fails we log + proceed without
	// auto-backfill, since the consumer's primary job (live
	// ingest) doesn't need it.
	if authResp, err := api.AuthTestContext(ctx); err != nil {
		c.logger.Warn("slack: auth.test failed; auto-backfill on member_joined disabled", "err", err)
	} else {
		c.selfUserID.Store(authResp.UserID)
		c.logger.Info("slack: bot identity discovered",
			"user_id", authResp.UserID, "team_id", authResp.TeamID)
	}

	// Consumer goroutine. slack-go does not close client.Events when
	// RunContext returns, so stop the reader explicitly afterward.
	readerStop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			evt, ok := nextSocketEvent(client.Events, readerStop)
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				payload, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				// Surface Slack-side retries. The socket-mode
				// envelope carries retry_attempt + retry_reason,
				// populated when Slack is redelivering an event we
				// previously failed to ack (in-connection 3s
				// timeout retry) OR when "Delayed Events" is on
				// and Slack is replaying events from the up-to-24h
				// recovery queue. The two cases share an envelope
				// shape but mean very different things; we log
				// both so we can tell from the stream what's
				// actually happening. Inspecting Reason +
				// timing-vs-original-ts disambiguates.
				retryAttempt := 0
				retryReason := ""
				envelopeID := ""
				if evt.Request != nil {
					retryAttempt = evt.Request.RetryAttempt
					retryReason = evt.Request.RetryReason
					envelopeID = evt.Request.EnvelopeID
				}
				if retryAttempt > 0 {
					reason := retryReason
					if reason == "" {
						reason = "unknown"
					}
					c.metrics.retries.WithLabelValues(reason).Inc()
					c.logger.Info("slack socket: retried event",
						"retry_attempt", retryAttempt,
						"retry_reason", reason,
						"event_type", payload.Type,
						"envelope_id", envelopeID)
				}
				// Reserve the handler before ack. Once cancellation closes
				// admission, queued envelopes must remain unacked so Slack can
				// redeliver them to the new leader. The returned starter still
				// launches only AFTER the ack, preserving the ack-first handler
				// contract and Slack's 3s deadline.
				startHandler, accepted := run.acceptHandler(func(workCtx context.Context) {
					c.handleEventsAPIWithRetry(workCtx, payload, retryAttempt, retryReason)
				})
				if !accepted {
					continue
				}
				// Every accepted EventsAPI envelope is acked the
				// instant we've done the retry-metrics bookkeeping,
				// BEFORE its handler runs. Slack's 3s ack deadline
				// must never gate handler work — not the message
				// pipeline, not the reaction DB round-trips, not the
				// member-left disable. Handlers are best-effort with
				// their own durable backstops (the SyncWorker tail
				// sweep + reconcile disable pass); the ack no longer
				// carries any "durably handled" meaning, so nothing
				// gates on a handler return value. See
				// handleEventsAPIWithRetry.
				if evt.Request != nil {
					if err := client.Ack(*evt.Request); err != nil {
						c.logger.Warn("slack socket: ack failed", "err", err)
					}
				}
				startHandler()
			case socketmode.EventTypeInteractive:
				// Button clicks (today: the self-serve ask opt-in).
				// Ack immediately — Slack's 3s deadline shouldn't
				// wait on our DB writes — then handle async.
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				startHandler, accepted := run.acceptHandler(func(workCtx context.Context) {
					c.handleInteractive(workCtx, cb)
				})
				if !accepted {
					continue
				}
				if evt.Request != nil {
					if err := client.Ack(*evt.Request); err != nil {
						c.logger.Warn("slack socket: ack failed", "err", err)
					}
				}
				startHandler()
			case socketmode.EventTypeConnected:
				c.logger.Info("slack socket connected")
			case socketmode.EventTypeDisconnect:
				c.logger.Warn("slack socket disconnected")
			case socketmode.EventTypeConnectionError:
				c.logger.Warn("slack socket connection error",
					"data", fmt.Sprintf("%v", evt.Data))
			}
		}
	}()

	// Clear the threads earlier turns left working: at once, for a previous
	// leader's turns that died with it, and then as rows expire. It stops
	// with the socket, whether the leader context ends or the client fails.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		c.runWorkingThreadSweep(sweepCtx)
	}()

	err := client.RunContext(ctx)
	stopSweep()
	close(readerStop)
	<-done
	<-sweepDone
	close(watchStop)
	<-watchDone
	if processCtx != nil && processCtx.Err() != nil {
		c.logger.Info("slack: draining accepted work for process shutdown",
			"timeout", gracefulDrainTimeout)
		if run.drain(gracefulDrainTimeout) {
			c.logger.Info("slack: accepted work drained")
		} else {
			c.logger.Warn("slack: graceful drain timed out; remaining work cancelled",
				"timeout", gracefulDrainTimeout)
		}
	} else {
		c.logger.Info("slack: cancelling accepted work after leadership loss or socket exit")
		run.cancelAndWait()
	}
	return err
}

func nextSocketEvent(events <-chan socketmode.Event, stop <-chan struct{}) (socketmode.Event, bool) {
	select {
	case evt, ok := <-events:
		return evt, ok
	case <-stop:
		return socketmode.Event{}, false
	}
}

// handleEventsAPI dispatches the Callback events we care about.
//
// Post-ack-first refactor it returns nothing: the event loop acks
// the envelope BEFORE invoking any handler, so there is no ack
// decision left to make here. Every path is best-effort — errors
// log + metric and lean on the durable backstops (the SyncWorker
// tail sweep for ingest, the reconcile disable pass for
// member-left). The message-path gates (blacklist, seenStore
// dedupe) stay exactly where they were; they prevent duplicate work,
// not late acks.
//
// handleEventsAPI is the 2-arg shim retained for tests that don't
// care about retry context. Production callers use the 4-arg
// handleEventsAPIWithRetry directly so the dedupe gate sees the
// real retry_attempt + retry_reason.
func (c *Consumer) handleEventsAPI(ctx context.Context, evt slackevents.EventsAPIEvent) {
	c.handleEventsAPIWithRetry(ctx, evt, 0, "")
}

func (c *Consumer) handleEventsAPIWithRetry(ctx context.Context, evt slackevents.EventsAPIEvent, retryAttempt int, retryReason string) {
	if evt.Type != slackevents.CallbackEvent {
		return
	}

	switch inner := evt.InnerEvent.Data.(type) {
	case *slackevents.ReactionAddedEvent:
		// NOTE: reaction_added events require the reactions:read scope
		// and the reaction_added event subscription in the Slack app
		// manifest — operator action, not code.
		c.handleReactionAdded(ctx, inner)
	case *slackevents.ReactionRemovedEvent:
		// NOTE: same reactions:read scope requirement as ReactionAddedEvent.
		c.handleReactionRemoved(ctx, inner)
	case *slackevents.AppMentionEvent:
		// A mention, possibly one in a channel the bot was not in until
		// the person who mentioned it let Slack add it.
		c.handleAppMention(ctx, inner)
	case *slackevents.AgentSessionStoppedEvent:
		// Someone pressed Slack's stop button while a turn was working.
		c.handleAgentSessionStopped(ctx, inner)
	case *slackevents.MemberJoinedChannelEvent:
		// Channel-discovery side: intentionally a no-op. The
		// periodic reconcile loop is the single source of truth
		// for "is the bot in this channel" — it polls
		// users.conversations every reconcileEvery and enqueues
		// anything new. Live member_joined_channel events are
		// best-effort under socket-mode's at-most-once-during-
		// disconnect semantics: a join that happens while the socket
		// is down is never delivered, so they cannot be load-bearing
		// for backfill triggering.
		//
		// External-members cache: DO bust it. A join may have
		// added an external user; expiring the scan timestamp
		// makes the next mention re-scan instead of trusting up
		// to 6h of stale "no externals". Best-effort accelerator
		// only — the TTL remains the correctness backstop for
		// missed events.
		c.expireExternalCheck(ctx, inner.Channel)
	case *slackevents.MemberLeftChannelEvent:
		// Mirror of the join case: a leave may have removed the
		// last external member, so bust the scan cache (the
		// confirm rescan in the gate is the backstop when this
		// event is missed). handleMemberLeft additionally handles
		// the bot-itself-left case.
		c.expireExternalCheck(ctx, inner.Channel)
		c.handleMemberLeft(ctx, inner)
	case *slackevents.MessageEvent:
		if inner.SubType == "channel_convert_to_private" || inner.SubType == "channel_convert_to_public" {
			c.handleChannelPrivacyChange(ctx, inner)
			return
		}
		// Blacklist gate: if this channel is on the do-not-ingest
		// list, drop. Done before any parse / DB work so blacklisted
		// channels are essentially free regardless of volume.
		//
		// Match is an O(1), in-memory lookup by stable channel ID.
		if c.blacklist != nil && inner.Channel != "" {
			if entry, ok := c.blacklist.Match(inner.Channel); ok {
				c.metrics.events.WithLabelValues("blacklisted").Inc()
				c.logger.Debug("slack: live event suppressed by blacklist",
					"channel", inner.Channel, "reason", entry.Reason)
				return
			}
		}
		// Dedupe gate. This gate is now purely a duplicate-work
		// guard, not an ack guard — the envelope was already acked
		// before this handler ran. It stops a redelivered (channel,
		// ts) from re-running the upsert/ask path. We dedupe on
		// (channel, ts) which is stable across redeliveries
		// (envelope_id changes per retry, so isn't a useful key). On
		// a hit we drop AND shout — a duplicate is always evidence of
		// either (a) we acked too late (unlikely now that ack is
		// first), or (b) a bug in our event handling path. Either way
		// it warrants investigation.
		if c.seen != nil && inner.Channel != "" && inner.TimeStamp != "" {
			if !c.seen.MarkOrSeen(inner.Channel, inner.TimeStamp) {
				reason := retryReason
				if reason == "" {
					reason = "no_retry"
				}
				c.metrics.redeliveries.WithLabelValues(reason).Inc()
				// ERROR-level log because this is always a bug:
				// either we acked late and Slack retried (fix our
				// pipeline latency or move ack earlier), or we
				// re-entered handleEventsAPI for a previously-
				// processed event (fix that bug).
				c.logger.Error("slack: redelivered event suppressed (BUG: investigate)",
					"channel", inner.Channel,
					"message_ts", inner.TimeStamp,
					"retry_attempt", retryAttempt,
					"retry_reason", retryReason,
					"event_subtype", inner.SubType,
					"event_user", inner.User,
				)
				return
			}
		}
		// Message-ingest path. The whole EventsAPI handler already
		// runs in a tracked per-event goroutine when dispatched by Run,
		// so this call is synchronous within that
		// goroutine — the slow work (parse + upsert + side-table +
		// ask routing) no longer gets its own extra goroutine. Slack's
		// 3s ack deadline can't gate it: the event loop acked before
		// dispatching us. The SyncWorker tail-sweep is the durable
		// backstop for any upsert error we log but no longer
		// redeliver on. Tests call handleEventsAPI directly, so this runs
		// synchronously on their caller goroutine and assertions about the
		// events table remain deterministic.
		c.processMessageEventAsync(ctx, inner)
	default:
	}
}

// processMessageEventAsync runs the message-ingest pipeline in
// the background after the event-loop has already acked. Errors
// log + metric but never propagate; durability is the
// SyncWorker's job. Detached from the caller's ctx? No — we use
// the same ctx so a Run-level shutdown still cancels in-flight
// work.
func (c *Consumer) processMessageEventAsync(ctx context.Context, inner *slackevents.MessageEvent) {
	// What the sender has open in Slack (a direct message's app_context)
	// informs this turn only. It is not part of the message, so it is not
	// stored with it.
	viewing := inner.AppContext
	if viewing != nil {
		message := *inner
		message.AppContext = nil
		inner = &message
	}
	raw, err := json.Marshal(inner)
	if err != nil {
		c.logger.Error("slack: remarshal event", "err", err)
		c.metrics.events.WithLabelValues("remarshal_err").Inc()
		return
	}

	// Delete handling here is a live-path shortcut: detect and dispatch
	// the tombstone directly. The shared ingest core also handles deletes
	// because history/sync can surface an unwrapped top-level
	// subtype="tombstone" message (conversations.history /
	// conversations.replies' snapshot shape for a deleted-but-still-
	// anchoring thread root — not the message_changed-wrapped shape the
	// live Events API sends); the non-delete case below hands the raw
	// bytes to that core, which re-parses (one cheap JSON unmarshal —
	// keeping parse owned by the core is worth the redundancy).
	res, err := ParseMessage(ctx, raw, c.workspaceHost, c.dir)
	if err != nil {
		c.logger.Warn("slack: parse failed", "err", err)
		c.metrics.events.WithLabelValues("parse_err").Inc()
		return
	}
	if res.Event == nil && !res.Delete {
		c.metrics.events.WithLabelValues("skipped").Inc()
		return
	}
	if res.Delete {
		if _, err := c.sink.Delete(ctx, res.Source, res.ExternalID); err != nil {
			// Pre-async this returned false to trigger Slack
			// redelivery. Now we log + bail; SyncWorker tail-sweep
			// repairs it on its next pass only for a thread root
			// with replies, which history returns as a top-level
			// subtype="tombstone". A message history no longer
			// returns at all stays stored: nothing reconciles
			// absence.
			c.logger.Error("slack: delete failed (async; SyncWorker is the backstop)",
				"external_id", res.ExternalID, "err", err)
			c.metrics.events.WithLabelValues("delete_err").Inc()
			return
		}
		c.metrics.events.WithLabelValues("delete_ok").Inc()
		return
	}

	// Non-delete ingest: the shared core does parse -> self-filter ->
	// actor resolve -> prefix -> scope -> upsert (with the
	// slack_message_details side write folded into the same tx) ->
	// image reconcile. See ingest.go for the full sequence + the
	// error-posture rationale.
	//
	// LIVE-PATH ERROR POSTURE: best-effort. The event-loop already
	// acked this event to Slack (ack-first dispatch), so any failure here is
	// log + metric + lean-on-the-sweep — we never propagate. A hard
	// error from the core (parse or the now-atomic event+details
	// upsert) maps to "upsert_err"/"parse_err": nothing committed, and
	// the SyncWorker tail sweep re-ingests on its next pass (~5 min).
	// The image-reconcile error is carried separately in
	// res.ImageErr — best-effort here (log + "attachments_err"), unlike
	// the backfill path which promotes it to a retry.
	ingRes, err := ingestRawMessage(ctx, c.ingestDeps(), raw)
	if err != nil {
		// A core error on the live path is always an upsert failure:
		// parse already succeeded on the same bytes in the delete-check
		// above, so the core's own re-parse can't fail. The folded-in
		// slack_message_details write rides this same path now — a
		// details failure rolls back the whole event and surfaces here
		// as upsert_err (it used to be a swallowed details_err with the
		// event still committed; atomicity is the intended change). We
		// log + metric + lean on the SyncWorker tail sweep (~5 min).
		c.logger.Error("slack: live ingest failed (async; SyncWorker is the backstop)",
			"err", err)
		c.metrics.events.WithLabelValues("upsert_err").Inc()
		return
	}

	// Translate the neutral disposition into the live metric
	// vocabulary (diderot_slack_events_total{outcome=...}). Names are
	// load-bearing for dashboards and alerts, so they stay caller-side.
	switch ingRes.Disposition {
	case ingestSkippedSelf:
		c.metrics.events.WithLabelValues("skipped_self").Inc()
		return
	case ingestSkippedSubtype:
		c.metrics.events.WithLabelValues("skipped").Inc()
		return
	case ingestUpdated:
		c.metrics.events.WithLabelValues("upsert_update").Inc()
	default: // ingestInserted, ingestNoChange
		c.metrics.events.WithLabelValues("upsert_insert").Inc()
	}

	// Image reconcile: best-effort on the live path. The core captured
	// any error rather than failing the upsert (image reconcile is
	// outside the event tx); a failure here leaves the image untracked
	// but the message body already landed, so we ack regardless.
	if ingRes.ImageErr != nil {
		c.logger.Warn("slack: image reconcile failed",
			"event_id", ingRes.EventID, "err", ingRes.ImageErr)
		c.metrics.events.WithLabelValues("attachments_err").Inc()
	}
	if ingRes.DocErr != nil {
		c.logger.Warn("slack: document reconcile failed",
			"event_id", ingRes.EventID, "err", ingRes.DocErr)
		c.metrics.events.WithLabelValues("documents_err").Inc()
	}

	// Priority bump: tell the SyncWorker that this channel is
	// active. last_synced_at gets pulled forward (LEAST(...,
	// now() - overlap)) so the next claim picks the channel up
	// sooner. Best-effort: a failed bump just means the channel
	// gets swept on its normal cadence rather than ahead. No-op
	// when sync isn't wired (consumer running standalone). This is
	// LIVE-PATH-ONLY — the workers are already sweeping.
	if c.sync != nil {
		details, _ := DetailsFrom(raw)
		channelID := details.ChannelID
		if channelID == "" {
			channelID = res.Event.ExternalID
			if i := strings.IndexByte(channelID, ':'); i > 0 {
				channelID = channelID[:i]
			}
		}
		if channelID != "" {
			if err := c.sync.PriorityBump(ctx, channelID); err != nil {
				c.logger.Warn("slack: priority bump failed",
					"channel", channelID, "err", err)
			}
		}
	}

	// Ask routing — happens AFTER ingestion succeeds so our own
	// replies + follow-up questions remain searchable on the next
	// turn. Ask is fire-and-forget from this goroutine's
	// perspective; errors surface as Slack replies to the user
	// from runAskTurn's own goroutine. LIVE-PATH-ONLY. We pass
	// res.Event (parsed above for the delete check) — it carries the
	// same data the core upserted; ask routing only reads it.
	c.maybeRouteAsk(ctx, raw, res.Event, viewing)
}

// ingestDeps assembles the shared-core dependency set from the
// Consumer's fields. The live path always uses ingestPathSocket as
// the per-path ingest-metrics label.
func (c *Consumer) ingestDeps() ingestDeps {
	return ingestDeps{
		pool:          c.pool,
		sink:          c.sink,
		dir:           c.dir,
		prefix:        c.prefix,
		logger:        c.logger,
		workspaceHost: c.workspaceHost,
		selfUserID:    c.SelfUserID,
		ingest:        c.ingest,
		path:          ingestPathSocket,
		// docGate: nil until WithChannelConfig is called, which means
		// document reconcile is off in test wiring that omits it.
		// Default-deny is the right posture for an un-wired gate.
		docGate: c.docGate(),
	}
}

// docGate adapts the channel-config store to the DocIngestGate
// interface, returning nil (deny) when the store isn't wired.
//
// Returning the typed nil directly would produce a non-nil interface
// holding a nil pointer, which the ingest path's `docGate == nil`
// check would not catch — hence the explicit nil return.
func (c *Consumer) docGate() DocIngestGate {
	if c.channelConfig == nil {
		return nil
	}
	return c.channelConfig
}

// maybeRouteAsk inspects an already-ingested message and, if it
// should reach the ask runner, spawns a goroutine that runs the
// turn + posts the reply. Decision rules:
//
//  1. ask must be wired (WithAsk passed a non-nil AskRouter).
//  2. Author must not be the bot itself — we don't reply to our own
//     messages.
//  3. One-to-one DMs route every eligible human message without an
//     @-mention. The verified Slack identity determines the single-reader
//     retrieval scopes; guests and unresolvable identities fail closed.
//  4. Channel messages route iff the text explicitly @-mentions
//     the bot. This rule applies regardless of whether the thread
//     already has an ask session. Earlier behavior auto-routed
//     any message in a known-session thread, but that conflated
//     "humans discussing the bot's prior answer" with "follow-up
//     question" — coworkers chatting about a diderot reply would
//     each trigger an uninvited turn. Mention-required matches
//     user expectations for slack bots and makes the engagement
//     contract explicit.
//  5. Whether the thread has an existing session is still
//     consulted, but only for downstream UX choices (eyes-reaction
//     skipped on follow-ups, session_id continuity in
//     LookupOrCreateSlackSession).
func (c *Consumer) maybeRouteAsk(ctx context.Context, raw []byte, ev *events.Event, viewing *slackevents.AppContext) {
	if c.ask == nil || ev == nil {
		return
	}
	details, _ := DetailsFrom(raw)
	isDM := isDirectMessage(details.ChannelID)
	// Resolving an app-relayed author costs a directory lookup, and only
	// messages with no human author on the envelope need one. Most of those are
	// ordinary bot traffic — alerts, CI, status posts — that never addresses
	// diderot, so check the cheap mention condition first and leave that volume
	// on the same immediate-return path it has always taken. Human-authored
	// messages skip the lookup entirely.
	if details.UserID == "" && !isDM {
		rawText, _ := rawMessageText(raw)
		if !mentionsSelf(rawText, c.SelfUserID()) {
			return
		}
	}
	askerID := c.routingAuthorID(ctx, raw, details)
	if askerID == "" {
		return
	}
	if details.IsEdit {
		// A `message_changed` subevent. Ingest already updated the body;
		// now consider routing the edit through the ask loop IFF:
		//   (a) the mention check below confirms the EDITED text has an
		//       @-mention, AND
		//   (b) the original message is fresh enough to be worth answering.
		//
		// Staleness guard: reject edits whose original message is older than
		// editRouteWindow (1h). Editing a dormant thread message is almost
		// never "oops, forgot the @" — it's a typo fix in a dead thread.
		// OccurredAt on the parsed event is the original post time
		// (ParseMessage sets it from the original ts, not the edit envelope
		// ts), which is the right anchor for freshness.
		//
		// If the edit IS fresh, fall through: the mention check and gate
		// evaluation below run on the edit's text exactly as for a plain
		// message. The composite claim key (built further down) ensures
		// distinct edits each get a fresh claim slot while redeliveries of
		// the same edit are deduped.
		if time.Since(ev.OccurredAt) > editRouteWindow {
			c.logger.Debug("ask: edit too old to route; skipping",
				"channel", details.ChannelID,
				"orig_occurred_at", ev.OccurredAt,
				"age", time.Since(ev.OccurredAt).Round(time.Second))
			return
		}
	}
	if self := c.SelfUserID(); self != "" && askerID == self {
		return // no self-replies
	}

	// messageTS is the original message ts. For edits this is the same ts
	// the message had before the edit — it's the Slack-stable identity of
	// the message. It is used as the reaction target (:eyes: goes on the
	// original message, not some edit envelope) and as the thread anchor
	// when this is a top-level message. Do NOT use it as the claim key for
	// edits; see claimKey below.
	messageTS := strings.TrimPrefix(ev.ExternalID, details.ChannelID+":")
	replyThreadTS := details.ThreadTS
	if replyThreadTS == "" {
		replyThreadTS = messageTS
	}
	sessionThreadTS := replyThreadTS

	// claimKey is what we INSERT into ask_turn_claims. For plain messages
	// it is messageTS (same as today). For edits it is a composite of
	// origTS + ":" + editedTS so that:
	//   - A distinct edit (new editedTS) claims a fresh slot and gets
	//     answered, even if the original message was already answered
	//     (old origTS:oldEditTS row exists; origTS:newEditTS is new).
	//   - Redelivery of the SAME edit (same editedTS) conflicts on the
	//     same row and is suppressed by ON CONFLICT DO NOTHING.
	//   - An edit with a missing editedTS is skipped defensively (Warn)
	//     rather than claimed with a garbage or non-unique key.
	claimKey := messageTS
	if details.IsEdit {
		if details.EditedTS == "" {
			c.logger.Warn("ask: message_changed missing edited.ts; cannot form composite claim key — skipping edit route",
				"channel", details.ChannelID, "orig_ts", messageTS)
			return
		}
		// Composite: "<origTS>:<editedTS>" — unique per edit, deduped on
		// redelivery. Fits in the existing text column; no schema change.
		claimKey = messageTS + ":" + details.EditedTS
	}

	// The mention check has to look at the RAW payload's text, not
	// ev.Body — by the time we get ev, the directory resolver has
	// already rewritten `<@U0EXAMPLE04>` to the human-readable form
	// like `@diderot`. The selfUserID-form is gone.
	rawText, _ := rawMessageText(raw)

	// Channel engagement rule: every routed turn requires an explicit
	// @-mention of the bot, regardless of whether a session already
	// exists for the thread. Earlier behavior auto-routed any
	// message in a known-session thread, but that conflated
	// "humans discussing the bot's answer" with "follow-up
	// question" — two coworkers chatting about a diderot reply
	// would each trigger an uninvited ask turn. Mention-required
	// makes the engagement contract explicit (and matches user
	// expectations for slack bots). A DM is already explicitly addressed to
	// diderot, so requiring a mention there would be redundant and surprising.
	self := c.SelfUserID()
	if !isDM && !mentionsSelf(rawText, self) {
		return
	}
	// A channel mention arrives twice, as a message event and as an
	// app_mention; only the first delivery goes on.
	if !isDM && c.mentions != nil && !c.mentions.MarkOrSeen(details.ChannelID, claimKey) {
		c.logger.Debug("ask: mention already routed from its other delivery",
			"channel", details.ChannelID, "claim_key", claimKey)
		return
	}

	var (
		askerInfo     *ask.AskerInfo
		allowedScopes []string
		corpusOnly    bool
	)
	if isDM {
		// DM retrieval can expose owner-scoped and private-channel content, so
		// unlike channel mentions it requires a positively resolved internal
		// Slack identity. A missing directory or lookup failure must not degrade
		// into broad internal access.
		if c.dir == nil {
			c.logger.Warn("ask DM: user directory unavailable; refusing to route",
				"channel", details.ChannelID, "user", askerID)
			c.replyDMAccessDenied(ctx, details.ChannelID, false)
			return
		}
		ui, err := c.dir.LookupUserInfoStrict(ctx, askerID)
		if err != nil || !isVerifiedInternalHuman(ui) {
			isExternal := err == nil && ui.IsExternal()
			if isExternal {
				c.logger.Info("ask DM: external user blocked",
					"channel", details.ChannelID, "user", askerID)
			} else {
				c.logger.Warn("ask DM: user identity unresolved; refusing to route",
					"channel", details.ChannelID, "user", askerID, "err", err)
			}
			c.replyDMAccessDenied(ctx, details.ChannelID, isExternal)
			return
		}
		askerInfo = askerInfoFromUserInfo(ui)
		allowedScopes = c.scopesForDM(ctx, ui.Email)
		if askerInfo != nil {
			askerInfo.Viewing = c.viewedChannel(ctx, viewing, allowedScopes)
		}
		c.metrics.askGate.WithLabelValues(string(gateAllowed)).Inc()
	} else {
		allowedScopes = scopesForChannel(details.ChannelID)
	}

	// Channel gate evaluation. All gates run before ANY side effect so the
	// operator ping and the invoker-facing ephemeral both see the
	// final decision — blocked attempts used to be log-only ("silent
	// drop"), which meant the operator learned about them by word of
	// mouth and the invoker assumed the bot was broken.
	outcome, externals, askerIsExternal := gateAllowed, []string(nil), false
	if !isDM {
		outcome, externals, askerIsExternal = c.evaluateAskGates(ctx, details.ChannelID, askerID)
		c.metrics.askGate.WithLabelValues(string(outcome)).Inc()
	}

	// Operator ping fires for EVERY mention, blocked or routed —
	// the operator follows all usage, including the attempts that
	// went nowhere. notifyMention prefixes blocked outcomes so the
	// DM says why nothing happened.
	if !isDM && len(c.notifyEmails) > 0 && outcome != gateAllowed {
		go func() {
			defer worker.Recover(c.logger, "slack-notify-mention")
			c.notifyMention(ctx, details.ChannelID, askerID, replyThreadTS, ev.Body, outcome, externals, "")
		}()
	}

	if outcome != gateAllowed {
		// gateBlockedExternalAsker means: external asker in a
		// non-whitelisted channel. Stay silent — don't surface UI
		// affordances to guests the operator hasn't vouched for.
		// Every other blocked outcome gets an ephemeral. External
		// askers in whitelisted channels can still reach
		// gateBlockedNotEnabled (ask_enabled=false); they get a
		// text-only variant without the self-serve button
		// (handleEnableAskClick refuses external clickers, so a
		// button they can't use is worse than none).
		if outcome != gateBlockedExternalAsker {
			go func() {
				defer worker.Recover(c.logger, "slack-notify-gate-ephemeral")
				c.postGateEphemeral(ctx, details.ChannelID, askerID, details.ThreadTS, outcome, externals, askerIsExternal)
			}()
		}
		return
	}

	// The setup gate answers everyone the channel gates admit, since a Slack
	// identity says nothing about Diderot administrator groups.
	if c.askGate != nil && !c.askGate.AskAllowed(false) {
		c.logger.Info("ask: refused while Diderot is in setup", "channel", details.ChannelID, "user", askerID)
		c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, settings.SetupNotice)
		return
	}

	// Session-state check serves two purposes:
	//   1. Session continuity: LoadOrCreateForSlackThread reuses the
	//      existing session_id when this thread already has one.
	//   2. Thread-context injection: the context_through high-water
	//      mark tells us which thread messages have already been
	//      delivered into the session; we fetch only the unseen ones.
	//   3. Surface-level UX: knownSession drives :eyes: reaction
	//      (first-touch only) in runAskTurn.
	// The lookup doesn't gate engagement — the mention check above is
	// sufficient; we route regardless of session state.
	if !isDM {
		access := c.channelAuthorizer.ResolveChannel(ctx, details.ChannelID)
		allowedScopes, corpusOnly = access.AllowedScopes, access.CorpusOnly
		if len(allowedScopes) == 0 {
			c.logger.Warn("ask: channel has no authorized corpus access", "channel", details.ChannelID)
			c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, "Diderot does not currently have authorized corpus access in this channel.")
			return
		}
	}
	knownSession, contextThrough, err := c.ask.SlackSessionState(ctx, details.ChannelID, sessionThreadTS)
	if err != nil {
		c.logger.Warn("ask: session state lookup failed",
			"channel", details.ChannelID, "session_thread", sessionThreadTS, "err", err)
		return
	}

	// Hand Claude the question verbatim. ev.Body has already had
	// `<@Uxxx>` resolved to "@<display_name>"; a leading "@diderot"
	// is harmless context for the model and any in-body occurrence
	// (e.g. an email like user@diderot.example) must NOT be scrubbed.
	question := strings.TrimSpace(ev.Body)
	if question == "" {
		return
	}
	// The question names the thread's agent session in Slack, without the
	// mention it opens with.
	title := sessionTitle(question, self != "" && strings.HasPrefix(strings.TrimSpace(rawText), "<@"+self))

	// Thread-context injection: fetch thread messages the session has
	// not yet seen (occurred_at > contextSince AND < mention's
	// occurred_at), oldest-first. Inject them as a <thread_context>
	// block so the model can answer references to prior conversation.
	//
	// contextSince is normally contextThrough (the high-water mark),
	// but two special cases override it:
	//
	//   - knownSession && contextThrough.IsZero(): this is an existing
	//     session created before the context_through column was added
	//     and never stamped since. The prior thread messages are
	//     already in the session's persisted ask history. Injecting
	//     them again would duplicate context the model already has.
	//     Use ev.OccurredAt as both bounds so nothing is returned.
	//
	//   - !knownSession && contextThrough.IsZero(): true first-touch
	//     (new session, no prior turns). Use zero so fetchThread-
	//     ContextBetween applies no lower bound and returns everything
	//     before the mention — the model has no session history yet.
	//
	// Diderot's own replies are never returned here because the
	// consumer self-filters self-authored messages before ingest
	// (skipped_self counter below ~line 639). No additional dedup
	// against bot output is needed.
	//
	// Fetch failure is best-effort: log and continue without context
	// rather than blocking the turn.
	contextSince := contextThrough
	if knownSession && contextSince.IsZero() {
		// Ambiguous state: known=true with NULL context_through can mean either
		// (a) a pre-migration legacy session with history already in ask_messages,
		// or (b) a post-deploy session whose first turn failed before persist.
		//
		// Current choice: suppress injection (set since = before, so the fetch
		// returns nothing). This avoids doubling up the full thread history
		// for case (a). The downside is case (b) — a retry after a failed
		// first turn doesn't get thread context injected. That window closes
		// after the first successful turn advances the mark. Trade-off is
		// punted to the operator for a final call.
		contextSince = ev.OccurredAt
	}

	// Negative-control shortcut: when caption injection is disabled, skip
	// the entire pre-ack + barrier + caption-append machinery, assemble
	// thread context from bodies, and answer immediately. This deliberately
	// answers before a screenshot is captioned so the Slack e2e test can be
	// shown to fail against it. Never enable in a deployment except for
	// that one verification.
	if c.disableCaptionInjection {
		c.logger.Warn("ask: caption injection DISABLED (negative-control flag) — reproducing pre-fix behavior",
			"channel", details.ChannelID, "thread_ts", replyThreadTS)
	}

	// Pre-ack reaction: add :eyes: to the mention message now, on
	// first-touch only, BEFORE the caption barrier so a multi-second
	// wait isn't a silent gap. On follow-ups (knownSession) the user
	// is reading the prior reply and the placeholder is enough ack, so
	// we skip it — matching the pre-existing runAskTurn behavior.
	// Idempotent-in-effect (treats already_reacted as success), so a
	// redelivery mid-barrier re-running this is a no-op.
	if !knownSession && !c.disableCaptionInjection {
		c.addPreAckReaction(ctx, details.ChannelID, messageTS)
	}

	// Announce only when the initial query finds pending work.
	barrierStart := time.Now()
	barrierCtx, barrierCancel := context.WithDeadline(ctx, barrierStart.Add(barrierCeiling))
	defer barrierCancel()
	pending, pendErr := pendingAttachments{}, error(nil)
	if !c.disableCaptionInjection && !corpusOnly {
		pending, pendErr = countUnsettledInScope(barrierCtx, c.pool, details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt)
	}
	if c.disableCaptionInjection || corpusOnly {
		// Skip the barrier entirely (negative control).
	} else if pendErr != nil {
		c.logger.Warn("ask: attachment barrier pre-query failed; not starting turn",
			"channel", details.ChannelID, "thread_ts", replyThreadTS, "err", pendErr)
		c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, embeddingStateErrorNotice)
		c.metrics.askCaptionBarrier.WithLabelValues("state_error").Inc()
		return
	} else if pending.total() == 0 {
		c.metrics.askCaptionBarrier.WithLabelValues("no_wait").Inc()
	} else {
		var docs []docProgress
		if pending.documents > 0 {
			var docErr error
			if docs, docErr = fetchDocProgress(barrierCtx, c.pool,
				details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt); docErr != nil {
				c.logger.Warn("ask: document progress query failed; generic notice",
					"channel", details.ChannelID, "thread_ts", replyThreadTS, "err", docErr)
			}
		}
		noticeText := barrierWaitNotice(pending, docs)
		c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, noticeText)

		waitCtx, waitCancel := context.WithDeadline(ctx, barrierStart.Add(pending.deadline()))

		waitStart := time.Now()
		barrierRes := waitForAttachments(waitCtx, c.pool, details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt)
		c.metrics.askCaptionBarrierWait.WithLabelValues().Observe(time.Since(waitStart).Seconds())
		timedOut := barrierRes.timedOut || waitCtx.Err() != nil
		waitCancel()
		if timedOut {
			// Re-check with the parent context after the barrier's child
			// deadline expires. A caption/extraction may have committed a new
			// derived chunk between the last poll and the deadline. If any such
			// chunk is still unembedded, do not start Ask: doing so would start
			// the model timeout before pending embedding work had completed.
			remaining, remainingErr := countUnsettledInScope(
				ctx, c.pool, details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt,
			)
			if remainingErr != nil {
				c.logger.Warn("ask: attachment barrier final-state query failed; not starting turn",
					"channel", details.ChannelID, "thread_ts", replyThreadTS, "err", remainingErr)
				c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, embeddingStateErrorNotice)
				c.metrics.askCaptionBarrier.WithLabelValues("state_error").Inc()
				return
			}
			if remaining.total() > 0 {
				outcome := "timed_out"
				if remaining.images == 0 && remaining.documents == 0 && remaining.embeddings() > 0 {
					outcome = "embedding_timed_out"
				}
				c.postThreadNotice(ctx, details.ChannelID, replyThreadTS, barrierTimeoutNotice(remaining))
				c.metrics.askCaptionBarrier.WithLabelValues(outcome).Inc()
				return
			}
			c.metrics.askCaptionBarrier.WithLabelValues("settled").Inc()
		} else {
			c.metrics.askCaptionBarrier.WithLabelValues("settled").Inc()
		}
	}

	var threadContext string
	contextThroughToAdvance := ev.OccurredAt
	if !isDM && !corpusOnly {
		if err := hydrateThreadBotNames(ctx, c.pool, c.dir, details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt); err != nil {
			c.logger.Warn("ask: thread bot-name hydration failed; continuing with stable ids",
				"channel", details.ChannelID, "thread_ts", replyThreadTS, "err", err)
		}
		fetcher := c.threadContextFetcher
		if fetcher == nil {
			fetcher = fetchThreadContextBetween
		}
		tc, fetchErr := fetcher(ctx, c.pool, details.ChannelID, replyThreadTS, contextSince, ev.OccurredAt)
		if fetchErr != nil {
			c.logger.Warn("ask: thread-context fetch failed; injecting without context",
				"channel", details.ChannelID, "thread_ts", replyThreadTS, "err", fetchErr)
			// The answer may still proceed, but it must not certify that the
			// missed history was delivered. A zero mark makes Ask leave the
			// existing high-water mark unchanged so the next mention retries it.
			contextThroughToAdvance = time.Time{}
		} else if tc != "" {
			threadContext = tc
			c.logger.Info("ask: injecting thread context",
				"channel", details.ChannelID, "thread_ts", replyThreadTS,
				"context_bytes", len(threadContext))
		}
	}

	// Thread context excludes the mention itself.
	var mentionImages []mentionImage
	if !c.disableCaptionInjection && !corpusOnly {
		var imageErr error
		mentionImages, imageErr = fetchMentionImages(ctx, c.pool, ev.ExternalID)
		if imageErr != nil {
			fetchErr := imageErr
			var fallbackErr error
			mentionImages, fallbackErr = fallbackMentionImages(ctx, c.pool, ev.ExternalID, details.Attachments, "image_metadata_unavailable")
			if fallbackErr != nil {
				c.logger.Warn("ask: mention event-id fallback failed",
					"channel", details.ChannelID, "message_ts", messageTS, "err", fallbackErr)
			}
			c.logger.Warn("ask: mention-image fetch failed; marking direct images unavailable",
				"channel", details.ChannelID, "message_ts", messageTS, "err", fetchErr)
		} else if len(mentionImages) == 0 {
			var fallbackErr error
			mentionImages, fallbackErr = fallbackMentionImages(ctx, c.pool, ev.ExternalID, details.Attachments, "image_not_indexed")
			if fallbackErr != nil {
				c.logger.Warn("ask: mention event-id fallback failed",
					"channel", details.ChannelID, "message_ts", messageTS, "err", fallbackErr)
			}
		}
		if len(mentionImages) > 0 {
			question, mentionImages = appendMentionImages(question, mentionImages)
			c.logger.Info("ask: appended mention-image context",
				"channel", details.ChannelID, "message_ts", messageTS,
				"image_count", len(mentionImages))
		}

		mentionDocs, docErr := fetchMentionDocuments(ctx, c.pool, ev.ExternalID)
		if docErr != nil {
			c.logger.Warn("ask: mention-document fetch failed; answering without it",
				"channel", details.ChannelID, "message_ts", messageTS, "err", docErr)
		} else if len(mentionDocs) > 0 {
			question = appendMentionDocuments(question, mentionDocs)
			c.logger.Info("ask: appended mention documents",
				"channel", details.ChannelID, "message_ts", messageTS,
				"document_count", len(mentionDocs))
		}
	}

	// Admit the ask before creating its durable claim. If the hard shutdown
	// cutoff has already closed admission, leaving no claim behind is critical:
	// a late handler must not permanently suppress recovery by another leader.
	// The admitted job INSERTs (channel_id, claimKey) into ask_turn_claims.
	// ON CONFLICT DO NOTHING means a second delivery of the same asking event —
	// whether from Slack's in-connection retry, the Delayed Events 24h queue, or
	// a different pod — returns zero rows and skips the turn. The in-memory
	// seenStore upstream already guards the common fast-redelivery case; this
	// claim is the correctness layer for redeliveries that arrive after the
	// seenStore TTL expires (or on a different pod where the seenStore has no
	// entry).
	//
	// For edits, claimKey is "<origTS>:<editedTS>" (see above). This means:
	//   - An already-answered original (origTS claim exists) does NOT suppress
	//     a new edit — the composite key is novel.
	//   - Redelivery of the SAME edit (same editedTS) conflicts on the same
	//     composite row and is correctly suppressed.
	//
	if !c.run.startAsk(ctx, func(workCtx context.Context) {
		// Fail-open: a DB error here only doubles the answer when it coincides
		// with a redelivery that also escaped seenStore. Failing closed would
		// instead drop real questions on every DB blip, which is worse.
		claim, claimErr := askchat.Claim(workCtx, c.pool, details.ChannelID, claimKey)
		c.metrics.askClaims.WithLabelValues(claim).Inc()
		switch claim {
		case "error":
			c.logger.Error("ask: claim insert failed; spawning turn anyway (fail-open)",
				"channel", details.ChannelID, "claim_key", claimKey, "err", claimErr)
		case "duplicate":
			c.logger.Warn("ask: duplicate claim suppressed; redelivery reached ask gate",
				"channel", details.ChannelID, "claim_key", claimKey)
			return
		}

		c.runAskTurn(workCtx, details.ChannelID, sessionThreadTS, replyThreadTS,
			messageTS, claimKey, askerID, question, title, threadContext, contextThroughToAdvance,
			knownSession, mentionImages, askerInfo, allowedScopes, corpusOnly)
	}) {
		c.logger.Warn("ask: turn rejected because shutdown drain expired",
			"channel", details.ChannelID, "claim_key", claimKey)
	}
}

// evaluateAskGates runs the engagement gates for one @-mention and
// returns the outcome. Order matters:
//
//  1. User directory lookup. Fail-closed on errors.
//  2. Channel config fetch (now required for all paths — external
//     askers need AllowExternalAsk consulted before silencing them).
//  3. External-asker gate. External users (guests, other-org Connect
//     members) pass only when allow_external_ask=true for this channel.
//     Blocked externals (non-whitelisted channel) return
//     gateBlockedExternalAsker and receive silence — we don't surface
//     UI affordances to guests in channels the operator hasn't vouched
//     for.
//  4. External-members gate, BEFORE the ask_enabled gate: a channel
//     containing externals must never be shown the self-serve
//     opt-in button — allowing those channels is operator-only
//     (on the channel's admin page). When
//     AllowExternalAsk is true this gate trivially passes.
//  5. ask_enabled gate (default-deny). Internal askers get the
//     self-serve enable button; external askers in whitelisted channels
//     get a text-only variant pointing at internal members (see
//     postGateEphemeral).
//
// Gate-infrastructure errors fail closed (gateBlockedError) with
// one exception, preserved from the pre-restructure shape: when the
// external-members scan fails but the operator has fully opted the
// channel in (allow_external_ask AND ask_enabled), a transient
// inability to re-confirm membership doesn't block routing.
//
// Cost note: EnsureExternalMembersFresh now runs on every mention,
// including those from external askers. Acceptable — mentions are
// rare, the scan is TTL-cached (6h) and single-flighted.
//
// The second return value carries the external member ids when the
// outcome is gateBlockedExternalMembers, so the ephemeral can name
// who's blocking the channel. Empty for every other outcome.
//
// The third return value is true when the asker was determined to be
// external, regardless of whether they were ultimately allowed.
// Callers use it to pick the right ephemeral variant for
// gateBlockedNotEnabled (button vs. text-only).
func (c *Consumer) evaluateAskGates(ctx context.Context, channelID, userID string) (askGateOutcome, []string, bool) {
	// Step 1: user directory lookup. Without a dir the bot is running
	// in a test environment where externality cannot be determined —
	// treat every user as internal.
	var askerIsExternal bool
	if c.dir != nil {
		ui, err := c.dir.LookupUserInfo(ctx, userID)
		if err != nil {
			c.logger.Warn("ask gate: user lookup failed; refusing to route",
				"channel", channelID, "user", userID, "err", err)
			return gateBlockedError, nil, false
		}
		if ui.IsNegativeCache {
			// Slack would not resolve this user, so nothing says they
			// are internal: the cached absence has every flag false.
			c.logger.Warn("ask gate: user unknown to Slack; refusing to route",
				"channel", channelID, "user", userID)
			return gateBlockedError, nil, false
		}
		askerIsExternal = ui.IsExternal()
		if askerIsExternal {
			c.logger.Info("ask gate: external asker detected",
				"channel", channelID, "user", userID,
				"is_stranger", ui.IsStranger,
				"is_restricted", ui.IsRestricted,
				"is_ultra_restricted", ui.IsUltraRestricted)
		}
	}

	// Step 2: channel config fetch. External askers require this now —
	// AllowExternalAsk must be consulted before silencing them.
	//
	// channelConfig nil is test-only wiring. For internal askers: allow
	// (existing behavior). For external askers: block — no config means
	// no whitelist evidence, and deny-by-default is the posture
	// everywhere else in this file.
	if c.channelConfig == nil {
		if askerIsExternal {
			return gateBlockedExternalAsker, nil, true
		}
		return gateAllowed, nil, false
	}
	cfg, err := c.channelConfig.EnsureExternalMembersFresh(ctx, channelID)
	if err != nil {
		// Scan error: apply the external-asker silence rule before
		// the generic fail-closed path. The stale cfg returned on
		// error preserves the DB's allow_external_ask value, so we
		// can still gate on it. An external asker in a non-whitelisted
		// channel must receive silence regardless of why the scan
		// failed — emitting gateBlockedError would expose a
		// bot-side ephemeral to a guest the operator hasn't vouched for.
		if askerIsExternal && !cfg.AllowExternalAsk {
			return gateBlockedExternalAsker, nil, true
		}
		if cfg.AllowExternalAsk && cfg.AskEnabled {
			c.logger.Warn("ask gate: scan failed but channel is fully opted in; routing anyway",
				"channel", channelID, "err", err)
			return gateAllowed, nil, askerIsExternal
		}
		c.logger.Warn("ask gate: scan failed and channel not opted in; refusing to route",
			"channel", channelID, "err", err)
		return gateBlockedError, nil, false
	}

	// Step 3: external-asker gate. In a whitelisted channel
	// (AllowExternalAsk=true) the external asker passes through —
	// retrieval scope is audience-based, not asker-based; the operator
	// already accepted "externals can read answers here" when they set
	// the flag. In a non-whitelisted channel, stay silent — don't
	// advertise the bot to guests the operator hasn't vouched for.
	if askerIsExternal && !cfg.AllowExternalAsk {
		c.logger.Info("ask gate: external asker in non-whitelisted channel; silencing",
			"channel", channelID, "user", userID)
		return gateBlockedExternalAsker, nil, true
	}

	// Step 4: external-members gate. When AllowExternalAsk is true this
	// gate trivially passes (the channel is already flagged as
	// external-ok). When AllowExternalAsk is false and the channel has
	// external members, block and surface who they are so the asker
	// knows exactly what to remove.
	if cfg.HasExternalMembers && !cfg.AllowExternalAsk {
		// A positive is about to block the mention — confirm it
		// with a tight-bound rescan first. Membership events are
		// at-most-once under socket mode, so the event-driven
		// cache bust can be missed; without this, "remove the
		// guest, re-ask" stays blocked for the rest of the 6h TTL.
		fresh, err := c.channelConfig.ConfirmExternalMembers(ctx, channelID)
		if err != nil {
			c.logger.Warn("ask gate: confirm rescan failed; trusting cached positive",
				"channel", channelID, "err", err)
		} else {
			cfg = fresh
		}
	}
	if cfg.HasExternalMembers && !cfg.AllowExternalAsk {
		c.logger.Info("ask gate: channel contains externals and not opted in",
			"channel", channelID, "asker", userID,
			"externals", cfg.ExternalMembers)
		return gateBlockedExternalMembers, cfg.ExternalMembers, false
	}

	// Step 5: ask_enabled gate (default-deny).
	if !cfg.AskEnabled {
		c.logger.Info("ask gate: channel not opted into ask",
			"channel", channelID, "asker", userID)
		return gateBlockedNotEnabled, nil, askerIsExternal
	}
	return gateAllowed, nil, askerIsExternal
}

// rawMessageText extracts the original .text field (or
// .message.text for message_changed payloads) from a raw slack
// event payload. The resolver-rewritten form lives in events.body;
// rawMessageText is what the consumer uses for mention detection
// since `<@Uxxx>` is preserved here.
// routingAuthorID resolves the human who authored a message, for ask-routing
// purposes only, and returns "" when there isn't one.
//
// MessageDetails.UserID is deliberately empty for anything isBotAuthored
// considers bot-written, which includes every message carrying a bot_id. That
// protects slack_message_details.user_id's human-only contract and is left
// exactly as it is. But bot_id is only a proxy for authorship, and it is the
// wrong one here: Slack stamps a bot_id on a chat.postMessage sent with a USER
// token too, so a message a person authorized through some app is
// indistinguishable at that field from a message a bot wrote on its own. Such a
// message keeps the human's id in `user`, and refusing to route it means
// diderot cannot be reached by anything but a hand-typed message — it silently
// ignored an @-mention sent with an operator's own user token, before the
// mention check, so no gate metric even recorded the refusal.
//
// So ask about authorship directly instead of via the proxy: take `user` and
// ask the directory whether that account is a bot. This is strictly narrower
// than the bot_id test for the case that matters and no wider anywhere else —
// a bot posting under its own token still has a bot `user` and is still
// refused, now for the accurate reason. It fails closed: no directory, no
// `user`, a lookup error, or a bot account all yield "".
//
// Loop safety does not rest on this alone. diderot's own replies carry its bot
// user id in `user`, so they are caught here AND by the self-reply check that
// follows, and a reply does not @-mention diderot in the first place.
func (c *Consumer) routingAuthorID(ctx context.Context, raw []byte, details MessageDetails) string {
	if details.UserID != "" {
		return details.UserID
	}
	userID := rawMessageUser(raw)
	if userID == "" || userID == "USLACKBOT" {
		return ""
	}
	if c.dir == nil {
		// Without a directory the bot question cannot be answered, and this
		// path exists only to admit app-relayed human messages. Declining keeps
		// the pre-existing behavior rather than guessing.
		return ""
	}
	info, err := c.dir.LookupUserInfo(ctx, userID)
	if err != nil {
		c.logger.Warn("ask: author lookup failed for app-relayed message; refusing to route",
			"channel", details.ChannelID, "user", userID, "err", err)
		return ""
	}
	if info.IsBot {
		return ""
	}
	return userID
}

// mentionsSelf reports whether raw message text explicitly @-mentions the bot.
// An unknown self id never matches: without it there is no mention to find, and
// treating that as a match would route everything.
func mentionsSelf(rawText, self string) bool {
	return self != "" && strings.Contains(rawText, "<@"+self+">")
}

// rawMessageUser reads the authoring user id straight off the envelope,
// mirroring where DetailsFrom looks: the top-level field for a plain message
// and the nested one for a message_changed subevent.
func rawMessageUser(raw []byte) string {
	var m struct {
		User    string `json:"user"`
		Message struct {
			User string `json:"user"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if m.User != "" {
		return m.User
	}
	return m.Message.User
}

func rawMessageText(raw []byte) (string, error) {
	var m struct {
		Text    string `json:"text"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	if m.Text != "" {
		return m.Text, nil
	}
	return m.Message.Text, nil
}

// addPreAckReaction adds the first-touch :eyes: reaction to the
// mention message, before the caption barrier so a multi-second wait
// isn't a silent gap. Best-effort and nil-guarded on c.api, like the
// success-path swap in runAskTurn.
//
// This runs BEFORE the durable ask_turn_claims INSERT, so a Slack
// redelivery arriving mid-barrier can fire it twice. It is therefore
// idempotent-in-effect: the reaction is keyed by (channel,
// message_ts) and Slack answers a repeat add with "already_reacted",
// which we treat as success (not a failure to log). That keeps the
// "everything before the claim is safe to run twice" invariant true
// even though a reaction is a mutation.
func (c *Consumer) addPreAckReaction(ctx context.Context, channelID, messageTS string) {
	if c.api == nil {
		return
	}
	err := WithSlackRetry(ctx, c.limiter, methodReactionsAdd, func() error {
		return c.api.AddReactionContext(ctx, "eyes",
			slack.NewRefToMessage(channelID, messageTS))
	})
	if err != nil && !isAlreadyReacted(err) {
		c.logger.Warn("ask: eyes reaction failed",
			"channel", channelID, "message_ts", messageTS, "err", err)
	}
}

// isAlreadyReacted reports whether a reactions.add error is Slack's
// "already_reacted" — the reaction is present, which for the pre-ack
// is success, not failure. slack-go surfaces API errors as
// SlackErrorResponse (value or pointer form depending on the code
// path; mirror directory.go's isDefinitiveNotFound in checking both).
func isAlreadyReacted(err error) bool {
	var sErr slack.SlackErrorResponse
	if errors.As(err, &sErr) {
		return sErr.Err == "already_reacted"
	}
	var pErr *slack.SlackErrorResponse
	if errors.As(err, &pErr) && pErr != nil {
		return pErr.Err == "already_reacted"
	}
	return false
}

// removeEyes takes the first-touch :eyes: reaction off a question.
// Best-effort.
func (c *Consumer) removeEyes(ctx context.Context, channelID, messageTS string) {
	if c.api == nil {
		return
	}
	err := WithSlackRetry(ctx, c.limiter, methodReactionsRemove, func() error {
		return c.api.RemoveReactionContext(ctx, "eyes", slack.NewRefToMessage(channelID, messageTS))
	})
	if err != nil {
		c.logger.Warn("ask: remove eyes failed", "channel", channelID, "message_ts", messageTS, "err", err)
	}
}

// postThreadNotice posts a best-effort informational message into the
// thread. Used for the caption-barrier "waiting" / "timed out"
// notices; separate from the runAskTurn placeholder (which still
// posts after the claim). Nil-guarded on c.api; failures log + move
// on — a missed notice never blocks the turn.
//
// These notices post BEFORE the durable claim, so unlike the pre-ack
// reaction they are not idempotent. That residual double-post risk is
// accepted per the design doc: the upstream seenStore suppresses the
// fast in-connection retries that are the only redeliveries able to
// land inside the barrier window, and ask_turn_claims only catches
// late queue replays that arrive after the barrier has finished.
func (c *Consumer) postThreadNotice(ctx context.Context, channelID, threadTS, text string) {
	if c.api == nil {
		return
	}
	err := WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		_, _, apiErr := c.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionTS(threadTS),
			slack.MsgOptionText(text, false),
		)
		return apiErr
	})
	if err != nil {
		c.logger.Warn("ask: thread notice post failed",
			"channel", channelID, "thread_ts", threadTS, "err", err)
	}
}

const (
	// Barrier notices are posted in the mention thread.
	captionWaitNotice              = "hang on — waiting for an attached image to finish processing before I answer."
	captionTimeoutNotice           = "the image didn't finish processing in time, so I didn't start the answer. Please try again in a moment."
	docWaitNotice                  = "hang on — extracting text from an attached document before I answer."
	docTimeoutNotice               = "the document didn't finish processing in time, so I didn't start the answer. Please try again in a moment."
	imageEmbeddingWaitNotice       = "hang on — indexing the attached image before I answer."
	documentEmbeddingWaitNotice    = "hang on — indexing the attached document before I answer."
	imageEmbeddingTimeoutNotice    = "the image didn't finish indexing in time, so I didn't start the answer. Please try again in a moment."
	documentEmbeddingTimeoutNotice = "the document didn't finish indexing in time, so I didn't start the answer. Please try again in a moment."
	embeddingStateErrorNotice      = "I couldn't verify that the attachment finished indexing, so I didn't start the answer. Please try again in a moment."
)

// editRouteWindow is the maximum age of the ORIGINAL message (by its ts) for
// which a message_changed edit can still trigger an ask turn. An edit that
// introduces a mention on a 4-hour-old message almost certainly isn't "oops,
// forgot to tag the bot on a real question"; it's more likely an unrelated
// typo fix in a dormant thread. 1h covers the overwhelmingly common case of
// "user composed message, forgot the @, noticed and fixed it quickly."
const editRouteWindow = 1 * time.Hour

// runAskTurn drives one Ask turn for a Slack thread message through the
// shared Ask-over-chat engine (askchat.Engine.RunTurn): the thread's agent
// session shows it working, Slack's stop button stops it, the answer is
// posted as chunked native-markdown replies, and the first-touch :eyes: on
// the question (added by maybeRouteAsk) becomes :white_check_mark:.
//
// parent is the leader-scoped context from handleEventsAPI; replies and
// cleanup are bounded from it.
func (c *Consumer) runAskTurn(
	parent context.Context,
	channelID, sessionThreadTS, replyThreadTS, messageTS, claimKey string,
	askerUserID, question, title, threadContext string,
	contextThrough time.Time,
	knownSession bool,
	mentionImages []mentionImage,
	askerInfo *ask.AskerInfo,
	allowedScopes []string,
	corpusOnly bool,
) {
	q := askchat.Question{
		Conversation: askchat.Conversation{Place: channelID, Thread: replyThreadTS},
		MessageID:    messageTS, AskerID: askerUserID,
		Text: question, Title: title, ThreadContext: threadContext, ContextThrough: contextThrough,
		KnownSession: knownSession, Asker: askerInfo,
		AllowedScopes: allowedScopes, CorpusOnly: corpusOnly,
	}
	if len(c.notifyEmails) > 0 {
		q.OnSession = func(sessionID string) {
			go func() {
				defer worker.Recover(c.logger, "slack-notify-mention")
				c.notifyMention(parent, channelID, askerUserID, replyThreadTS, question, gateAllowed, nil, sessionID)
			}()
		}
	}
	if len(mentionImages) > 0 {
		q.BeforeTurn = func(ctx context.Context, sessionID string) {
			if err := persistAskInputImages(ctx, c.pool, sessionID, messageTS, claimKey, mentionImages); err != nil {
				c.logger.Warn("ask: input-image manifest persist failed; continuing",
					"channel", channelID, "thread_ts", replyThreadTS, "session_id", sessionID, "err", err)
			}
		}
	}
	c.engine().RunTurn(parent, q)
}

// postAskArtifacts uploads a turn's selected panels to the thread, each at
// most once per surface and thread.
func (c *Consumer) postAskArtifacts(ctx context.Context, channelID, threadTS string, sessionID uuid.UUID, assistantTurn int, artifacts []ask.ArtifactDescriptor, surface string) error {
	return c.engine().DeliverArtifacts(ctx, askchat.Conversation{Place: channelID, Thread: threadTS}, surface, sessionID, assistantTurn, artifacts)
}

// buildAskerInfo resolves a slack user_id to the per-turn AskerInfo
// the runner injects into the user-message context envelope. Returns
// nil when the directory is unwired, the user id is empty, or the
// directory lookup fails — the runner falls back to the minimal
// UTC-only envelope in those cases. Best-effort throughout: a stale
// or partial cache row still produces a useful AskerInfo (formatter
// drops missing fields).
//
// LocalTime is computed from time.LoadLocation(ui.TZ) so DST
// transitions resolve correctly. ui.TZOffset is informational only
// — the offset Slack returned at users.info fetch time can drift
// across DST boundaries, so we prefer the IANA name + LoadLocation.
func (c *Consumer) buildAskerInfo(ctx context.Context, userID string) *ask.AskerInfo {
	if c.dir == nil || userID == "" {
		return nil
	}
	ui, err := c.dir.LookupUserInfo(ctx, userID)
	if err != nil {
		return nil
	}
	return askerInfoFromUserInfo(ui)
}

// postAskAnswer delivers the authoritative result as one or more ordinary
// thread replies through the engine (askchat.Engine.Deliver): split at
// slackPostChunkBytes, rendered as native markdown blocks, the last with
// the feedback buttons, each registered in ask_answer_messages so reactions
// resolve to the session.
func (c *Consumer) postAskAnswer(
	ctx context.Context,
	channelID, threadTS, sessionID string,
	assistantTurn int,
	reply string,
	incompleteReason ask.IncompleteReason,
) error {
	_, err := c.postAskAnswerChunks(ctx, channelID, threadTS, sessionID, assistantTurn, reply, incompleteReason, "")
	return err
}

func (c *Consumer) postAskAnswerChunks(
	ctx context.Context,
	channelID, threadTS, sessionID string,
	assistantTurn int,
	reply string,
	incompleteReason ask.IncompleteReason,
	marker string,
) (string, error) {
	if c.api == nil {
		return "", errors.New("slack api not configured")
	}
	return c.engine().Deliver(ctx, askchat.Conversation{Place: channelID, Thread: threadTS}, sessionID, assistantTurn, reply, incompleteReason, marker)
}

// replyAskError posts a brief error message to the thread with a fresh timeout
// derived from the run-scoped work context. It therefore survives the 15m
// per-turn timeout but not leadership loss or the hard process-drain cutoff.
func (c *Consumer) replyAskError(parent context.Context, channelID, threadTS, sessionID, msg string) {
	if c.api == nil {
		return
	}
	c.engine().ReplyError(parent, askchat.Conversation{Place: channelID, Thread: threadTS}, sessionID, msg)
}

// replyDMAccessDenied explains why a direct message could not be routed.
// Best-effort and deliberately generic on identity-resolution failures so
// Slack directory state is not exposed as an account-existence oracle.
func (c *Consumer) replyDMAccessDenied(ctx context.Context, channelID string, external bool) {
	if c.api == nil {
		return
	}
	message := "sorry, I couldn't verify your Slack identity for DM access. Please try again in a moment."
	if external {
		message = c.externalDMDeniedMessage()
	}
	err := WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		_, _, apiErr := c.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(message, false),
		)
		return apiErr
	})
	if err != nil {
		c.logger.Warn("ask: DM access-denied reply failed", "err", err)
	}
}

// notifyMention DMs each configured operator-watcher when the bot receives an
// Ask invocation. Channel mentions are reported whether routed or blocked;
// direct-message invocations are reported after session lookup. Blocked
// outcomes get a "[blocked: <reason>]" prefix so the operator sees attempted
// usage (and can proactively opt channels in) without tailing pod logs.
// Includes the asker's resolved name, the source conversation, the question
// text, and a permalink to the thread. Best-effort — failures log + move on;
// the ask loop itself is not gated on this.
//
// Skips notifying the asker themselves (no self-pings — the asker
// is by definition already in the thread). The DM uses
// conversations.open (cheap, idempotent) to find the IM channel
// for each user-id, then posts.
//
// Wraps a 15s ctx around its work so a hung Slack API call doesn't
// leak the goroutine. Caller passes the leader ctx; we derive a
// shorter-deadline child here.
func (c *Consumer) notifyMention(parent context.Context, channelID, askerUserID, threadTS, body string, outcome askGateOutcome, externals []string, sessionID string) {
	if c.api == nil || len(c.notifyEmails) == 0 || !c.notifyQuestions {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	channelLabel := "#" + channelID
	if isDirectMessage(channelID) {
		channelLabel = "direct message"
	} else if c.dir != nil {
		if name, err := c.dir.LookupChannel(ctx, channelID); err == nil && name != "" {
			channelLabel = "#" + sanitizeSlackMarkdown(name)
		}
	}

	askerLabel := askerUserID
	askerEmail := ""
	if c.dir != nil && askerUserID != "" {
		if info, err := c.dir.LookupUserInfo(ctx, askerUserID); err == nil {
			askerEmail = info.Email
			askerLabel = pickName(info.DisplayName,
				pickName(info.RealName, pickName(info.Name, askerUserID)))
		}
	}
	if c.suppressAskNotification(askerEmail) {
		return
	}

	var permalinkResp string
	err := WithSlackRetry(ctx, c.limiter, methodGetPermalink, func() error {
		var apiErr error
		permalinkResp, apiErr = c.api.GetPermalinkContext(ctx, &slack.PermalinkParameters{
			Channel: channelID,
			Ts:      threadTS,
		})
		return apiErr
	})
	permalink := ""
	if err == nil {
		permalink = permalinkResp
	} else {
		c.logger.Warn("notify: permalink failed",
			"channel", channelID, "thread_ts", threadTS, "err", err)
	}

	blockedReason := ""
	if outcome != gateAllowed {
		blockedReason = strings.TrimPrefix(string(outcome), "blocked_")
	}
	details := ""
	if outcome == gateBlockedExternalMembers && len(externals) > 0 {
		// Name who's blocking the channel so the operator can act
		// (vet for allow-external, or remove them) without a
		// members-list spelunk.
		details = "*External members:* " + c.describeExternals(ctx, externals)
	}
	_ = c.sendAskNotification(ctx, askNotification{
		Source:        "Slack",
		Asker:         askerLabel,
		AskerEmail:    askerEmail,
		Channel:       channelLabel,
		Question:      body,
		Permalink:     permalink,
		SessionID:     sessionID,
		BlockedReason: blockedReason,
		Details:       details,
	}, askerUserID)
}

// dmUser opens (or fetches) the IM channel for one user and posts a
// plain-text message. Shared by the operator-ping paths
// (notifyMention, the opt-in handler).
func (c *Consumer) dmUser(ctx context.Context, userID, msg string) error {
	if c.api == nil {
		return nil
	}
	var imChan *slack.Channel
	err := WithSlackRetry(ctx, c.limiter, methodOpenConversation, func() error {
		var apiErr error
		imChan, _, _, apiErr = c.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
			Users: []string{userID},
		})
		return apiErr
	})
	if err != nil {
		return fmt.Errorf("conversations.open: %w", err)
	}
	return WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		_, _, apiErr := c.api.PostMessageContext(ctx, imChan.ID,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		return apiErr
	})
}

// expireExternalCheck busts the external-members scan cache for a
// channel after a membership change event. Best-effort: failure
// just means the gate falls back to the TTL / confirm-rescan path.
func (c *Consumer) expireExternalCheck(ctx context.Context, channelID string) {
	if c.channelConfig == nil || channelID == "" {
		return
	}
	if err := c.channelConfig.ExpireExternalCheck(ctx, channelID); err != nil {
		c.logger.Warn("membership change: external-check expire failed",
			"channel", channelID, "err", err)
	}
}

// memberLeftDisableBackoffs are the inter-attempt sleeps for the
// member-left disable retry. The first Disable runs immediately; a
// failure sleeps memberLeftDisableBackoffs[0] before retry, then
// [1], then [2] — 4 total attempts across ~2.6s. Kept short so the
// per-event goroutine doesn't linger, and bounded because the
// reconcile disable pass is the durable backstop for a disable we
// never land here.
var memberLeftDisableBackoffs = []time.Duration{
	100 * time.Millisecond,
	500 * time.Millisecond,
	2 * time.Second,
}

// handleMemberLeft flips ingest_enabled=false on the bot's
// slack_channel_sync row when the bot itself leaves (kicked or
// removed). The events/chunks stay in the DB — the historical data
// is still valid — but the worker stops sweeping the channel.
// Non-bot leaves are ignored.
//
// Ack-first refactor: this used to return false on a Disable failure
// so Slack would redeliver the member_left event and give us another
// shot. That mechanism is gone (the envelope is acked before we run).
// Replacement is two-layered: (1) retry the Disable UPDATE internally
// with short backoff here, so a transient DB blip usually resolves in
// the same goroutine; (2) SyncWorker.Reconcile disables rows for
// channels the bot is no longer a member of, so even a total failure
// here self-heals on the next reconcile tick.
func (c *Consumer) handleMemberLeft(ctx context.Context, evt *slackevents.MemberLeftChannelEvent) {
	self := c.SelfUserID()
	if c.sync == nil || self == "" {
		return
	}
	if evt.User != self {
		return
	}
	// Flip ingest_enabled=false on the slack_channel_sync row.
	// The row stays so an operator can inspect "this channel was
	// ingested then stopped" on the channel's page. Reconcile turns
	// it back on if the bot rejoins.
	c.logger.Info("slack: bot left channel; disabling sync row",
		"channel", evt.Channel)
	var err error
	for attempt := 0; ; attempt++ {
		if err = c.sync.Disable(ctx, evt.Channel); err == nil {
			c.metrics.events.WithLabelValues("left_disable_ok").Inc()
			return
		}
		if attempt >= len(memberLeftDisableBackoffs) {
			break
		}
		// Context-aware sleep: a Run-level shutdown mid-backoff
		// abandons the retry loop (the reconcile pass will catch the
		// channel later) rather than blocking the goroutine.
		select {
		case <-ctx.Done():
			c.logger.Warn("slack: sync disable abandoned; ctx cancelled mid-retry",
				"channel", evt.Channel, "err", ctx.Err())
			c.metrics.events.WithLabelValues("left_disable_err").Inc()
			return
		case <-time.After(memberLeftDisableBackoffs[attempt]):
		}
	}
	// Exhausted retries. Log + metric; the reconcile disable pass is
	// the durable backstop.
	c.logger.Error("slack: sync disable failed after retries; reconcile disable pass is the backstop",
		"channel", evt.Channel, "attempts", len(memberLeftDisableBackoffs)+1, "err", err)
	c.metrics.events.WithLabelValues("left_disable_err").Inc()
}

// nullIfEmpty turns "" into a typed nil so pgx encodes it as SQL NULL.
// The slack_message_details side table accepts NULL on user_id /
// thread_ts / channel_name.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
