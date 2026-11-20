package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

// backfillAPI is the subset of slack-go we need for history +
// replies pagination. Kept as an interface so tests can substitute
// a fake; production passes *slack.Client.
type backfillAPI interface {
	warmAPI // users+channels list helpers for the pre-warm step
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error)
}

// Backfill drives the "ingest channel history from scratch" flow.
// Single-use: construct, Run, read the final Progress, discard. Not
// safe to call Run concurrently on the same instance — the admin
// API holds a mutex to ensure only one backfill runs at a time
// across the deployment.
type Backfill struct {
	pool          *pgxpool.Pool
	sink          *events.DBSink
	api           backfillAPI
	dir           *Directory
	prefix        *PrefixBuilder
	logger        *slog.Logger
	workspaceHost string
	metrics       *backfillMetrics

	// historyPageSize bounds each conversations.history response.
	// Slack caps at 1000 but recommends 200 for performance; we
	// pick 200 to stay within the recommendation.
	historyPageSize int

	// limiter is the global Slack-API budget governor. Optional —
	// nil means "no proactive pacing" (fall back to the prior
	// retry-after-sleep-on-429 loop). Production wires it via
	// WithLimiter from cmd/diderot's Slack source; tests usually leave
	// it nil.
	limiter *Limiter

	// selfUserIDFn is a thunk that returns the diderot bot's slack
	// user_id when called. Function-typed (not a static string)
	// because the Consumer resolves the bot's id asynchronously
	// inside Run via auth.test, AFTER backfill construction —
	// reading it lazily lets backfill see the live value once
	// auth.test completes without requiring a re-construction.
	// nil disables the filter (tests + cases where the consumer
	// isn't running). Wired via WithSelfUserIDFn from cmd/diderot's
	// Slack source.
	selfUserIDFn func() string

	// ingest emits per-path ingest counters + lag histogram at sink-
	// write time. Optional — nil disables the observability without
	// affecting ingest correctness. Production wires it via
	// WithIngestMetrics from cmd/diderot's Slack source.
	ingest *ingestMetrics

	// docGate is the per-channel document-ingest opt-in. Optional; nil
	// means no event_documents rows are created on this path
	// (default-deny). Production wires it via WithDocGate from
	// cmd/diderot's Slack source so a backfill of an opted-in channel enqueues its documents the
	// same way live ingest does.
	docGate DocIngestGate

	// blacklist is the snapshot of blocked channels. Only Run, through
	// runChannel, consults it; nil means Run skips no channel. The sync
	// and thread workers share this Backfill for its per-message ingest
	// and never read this field: each checks the block where it claims
	// work, as the caption and document workers do.
	blacklist *IngestBlacklist
}

// WithDocGate wires the per-channel document-ingest opt-in check.
// Without it, backfill and the sync/thread workers create no
// event_documents rows at all — default-deny, matching the live path's
// posture when channel config isn't wired.
func (b *Backfill) WithDocGate(g DocIngestGate) *Backfill {
	b.docGate = g
	return b
}

// WithIngestBlacklist wires the operator-managed ingest blacklist.
// When set, runChannel checks it before making any Slack API call
// for that channel: a blacklisted channel is skipped outright, with
// no history page or replies call and no state of any kind written
// or advanced for it. Run has no cursor or claim to preserve to
// begin with, so this leaves a blacklisted channel exactly as an
// un-run backfill would — fully ingestable later, once it's
// removed from the blacklist and a backfill is re-requested.
func (b *Backfill) WithIngestBlacklist(bl *IngestBlacklist) *Backfill {
	b.blacklist = bl
	return b
}

// WithSelfUserIDFn wires a thunk that returns the bot's own slack
// user_id. With it set, ingestMessageCore skips messages diderot
// itself authored — those rows are duplicative with ask_messages
// and pollute retrieve hits because they're literally about
// whatever the asker just asked. Filter is by actor_id only, so
// human follow-ups in diderot threads still ingest normally. The
// thunk shape lets the consumer resolve selfUserID asynchronously
// (auth.test) and the backfill picks up the value on the next
// ingestMessageCore call rather than at construction.
func (b *Backfill) WithSelfUserIDFn(fn func() string) *Backfill {
	b.selfUserIDFn = fn
	return b
}

// WithLimiter wires the budget governor for proactive pacing on
// every conversations.history / conversations.replies call. With
// it, the backfill engine and the rest of the slack consumer
// share one token bucket — concurrent backfill + tail-sync
// callers can't starve each other on Slack's per-method budget.
func (b *Backfill) WithLimiter(l *Limiter) *Backfill {
	b.limiter = l
	return b
}

// WithIngestMetrics wires the per-path ingest observability metrics.
// SyncWorker and ThreadWorker pass their own path label when calling
// ingestMessageCore; the backfill engine uses ingestPathBackfill.
func (b *Backfill) WithIngestMetrics(m *IngestMetrics) *Backfill {
	b.ingest = m
	return b
}

// BackfillOptions configures a single Run. All fields are optional.
type BackfillOptions struct {
	// OldestTS is the unix-seconds lower bound. Zero means "all
	// history Slack will give us" (which at free-tier workspaces
	// is usually capped at 90 days by Slack itself).
	OldestTS float64
	// LatestTS is the unix-seconds upper bound. Zero means "now".
	LatestTS float64
	// SkipPrewarm skips WarmUsers + WarmChannels (typically a
	// second backfill run within a session, where the cache is
	// already fresh).
	SkipPrewarm bool
}

// Progress is the atomic-counters view of a running backfill. Fields
// are safe to read while Run is in flight; the admin endpoint polls
// it for its status response.
type Progress struct {
	Channel        atomic.Value // string: current channel being processed
	Stage          atomic.Value // string: "warm", "history", "replies", "done", "error"
	ParentMessages atomic.Int64
	ThreadReplies  atomic.Int64
	InsertedEvents atomic.Int64 // sink reported Inserted path
	UpdatedEvents  atomic.Int64 // sink reported Updated path (re-run picking up an edit)
	NoChangeEvents atomic.Int64 // sink NoChange (re-run of identical content)
	SkippedEvents  atomic.Int64 // parse returned zero ParseResult
	Errors         atomic.Int64
	StartedAt      time.Time
	LastErr        atomic.Value // string: most recent error message
}

// NewBackfill wires a Backfill. api must be a *slack.Client in production;
// tests pass a fake implementing backfillAPI. sink is the shared
// events.DBSink the live consumer also writes to.
func NewBackfill(
	pool *pgxpool.Pool,
	sink *events.DBSink,
	api backfillAPI,
	dir *Directory,
	prefix *PrefixBuilder,
	logger *slog.Logger,
	reg prometheus.Registerer,
	workspaceHost string,
) *Backfill {
	if logger == nil {
		logger = slog.Default()
	}
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &Backfill{
		pool:            pool,
		sink:            sink,
		api:             api,
		dir:             dir,
		prefix:          prefix,
		logger:          logger,
		workspaceHost:   workspaceHost,
		metrics:         newBackfillMetrics(reg),
		historyPageSize: 200,
	}
}

// Run processes each channel in order. One channel at a time, two
// passes per channel:
//
//  1. conversations.history newest-first (Slack's only supported
//     direction). We insert every message we see. Thread roots are
//     identified by reply_count > 0 AND ts == thread_ts; we record
//     their thread_ts for pass 2.
//
//  2. conversations.replies for each recorded thread, which returns
//     oldest-first — so the prefix builder sees the parent already
//     in the DB when it looks up the thread root.
//
// Two-pass is necessary because thread parent + reply can land on
// different history pages; processing strictly newest→oldest would
// mean a reply's prefix lookup for its root could miss.
func (b *Backfill) Run(ctx context.Context, channels []string, opts BackfillOptions, progress *Progress) error {
	progress.StartedAt = time.Now()

	if !opts.SkipPrewarm {
		progress.Stage.Store("warm")
		b.logger.Info("backfill warming directory caches")
		if n, err := WarmUsers(ctx, b.api, b.limiter, b.pool); err != nil {
			progress.LastErr.Store(err.Error())
			progress.Errors.Add(1)
			b.logger.Warn("backfill warm users failed; continuing with lazy fill", "err", err)
		} else {
			b.logger.Info("backfill warmed users", "count", n)
		}
		if n, err := WarmChannels(ctx, b.api, b.limiter, b.pool); err != nil {
			progress.LastErr.Store(err.Error())
			progress.Errors.Add(1)
			b.logger.Warn("backfill warm channels failed; continuing with lazy fill", "err", err)
		} else {
			b.logger.Info("backfill warmed channels", "count", n)
		}
	}

	for _, ch := range channels {
		progress.Channel.Store(ch)
		if err := b.runChannel(ctx, ch, opts, progress); err != nil {
			if errors.Is(err, context.Canceled) {
				progress.Stage.Store("error")
				progress.LastErr.Store("cancelled")
				return err
			}
			progress.Errors.Add(1)
			progress.LastErr.Store(err.Error())
			b.logger.Error("backfill channel failed; continuing", "channel", ch, "err", err)
			continue
		}
	}
	progress.Stage.Store("done")
	return nil
}

func (b *Backfill) runChannel(ctx context.Context, channelID string, opts BackfillOptions, progress *Progress) error {
	// Checked before any Slack API call or DB write for this channel,
	// so a blacklisted channel gets neither a history page nor a
	// replies call — nothing for it to leak through, and nothing for
	// an unblock + re-backfill to have to recover from.
	if b.blacklist != nil {
		if entry, blocked := b.blacklist.Match(channelID); blocked {
			b.logger.Info("backfill skipping blacklisted channel",
				"channel", channelID, "reason", entry.Reason)
			progress.Stage.Store("skipped")
			return nil
		}
	}

	// Pass 1: walk history, insert every message, collect thread roots.
	progress.Stage.Store("history")
	var (
		cursor     string
		threadTSes []string
		seen       = map[string]struct{}{} // thread_ts dedupe for pass 2
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := b.historyPage(ctx, channelID, opts, cursor)
		if err != nil {
			return fmt.Errorf("history page: %w", err)
		}
		for _, msg := range resp.Messages {
			// Slack includes thread replies in conversations.history
			// output. Skip them in pass 1 — we'll process them in
			// pass 2 after the thread root is in the DB, so the
			// prefix builder can find it.
			isReply := msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp
			if isReply {
				continue
			}

			if err := b.ingestMessage(ctx, channelID, msg, progress); err != nil {
				progress.Errors.Add(1)
				progress.LastErr.Store(err.Error())
				b.logger.Warn("backfill ingest message failed; skipping",
					"channel", channelID, "ts", msg.Timestamp, "err", err)
				continue
			}
			progress.ParentMessages.Add(1)

			// Thread root: has replies. Slack sets thread_ts==ts
			// on the root itself (the isReply check above already
			// confirmed ts matches thread_ts or thread_ts is empty).
			if msg.ReplyCount > 0 {
				rootTS := msg.Timestamp
				if _, dupe := seen[rootTS]; !dupe {
					seen[rootTS] = struct{}{}
					threadTSes = append(threadTSes, rootTS)
				}
			}
		}
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}

	b.logger.Info("backfill history pass done",
		"channel", channelID,
		"parents", progress.ParentMessages.Load(),
		"threads", len(threadTSes),
	)

	// Pass 2: drain each thread.
	progress.Stage.Store("replies")
	for _, threadTS := range threadTSes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.runThread(ctx, channelID, threadTS, progress); err != nil {
			progress.Errors.Add(1)
			progress.LastErr.Store(err.Error())
			b.logger.Warn("backfill thread failed; skipping",
				"channel", channelID, "thread_ts", threadTS, "err", err)
			continue
		}
	}
	return nil
}

func (b *Backfill) runThread(ctx context.Context, channelID, threadTS string, progress *Progress) error {
	var cursor string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Cursor:    cursor,
			Limit:     b.historyPageSize,
		}
		msgs, hasMore, next, err := b.repliesCall(ctx, params)
		if err != nil {
			return fmt.Errorf("replies call: %w", err)
		}
		for i, msg := range msgs {
			// The first message returned by conversations.replies is
			// the thread root itself — already ingested in pass 1,
			// and the sink's no-op short-circuit makes re-upserting
			// cheap. Skip it explicitly anyway so we don't double-
			// count in progress.ThreadReplies.
			if i == 0 && msg.Timestamp == threadTS {
				continue
			}
			if err := b.ingestMessage(ctx, channelID, msg, progress); err != nil {
				progress.Errors.Add(1)
				progress.LastErr.Store(err.Error())
				b.logger.Warn("backfill ingest reply failed; skipping",
					"channel", channelID, "thread_ts", threadTS, "ts", msg.Timestamp, "err", err)
				continue
			}
			progress.ThreadReplies.Add(1)
		}
		if !hasMore || next == "" {
			return nil
		}
		cursor = next
	}
}

// ingestMessage is the legacy Backfill entry-point: drives
// ingestMessageCore and translates the disposition into Progress
// counters. SyncWorker calls ingestMessageCore directly so it
// can drive its own metrics vocabulary (per-source labels).
//
// The disposition vocabulary itself lives on the shared core
// (ingest.go's ingest* consts); the per-source metric labels emitted
// by SyncWorker / ThreadWorker reuse the same string values so a
// disposition flows straight into WithLabelValues without a second
// mapping table.
func (b *Backfill) ingestMessage(ctx context.Context, channelID string, msg slack.Message, progress *Progress) error {
	disp, err := b.ingestMessageCore(ctx, channelID, msg, ingestPathBackfill)
	if err != nil {
		return err
	}
	switch ingestDisposition(disp) {
	case ingestInserted:
		progress.InsertedEvents.Add(1)
	case ingestUpdated:
		progress.UpdatedEvents.Add(1)
	case ingestNoChange:
		progress.NoChangeEvents.Add(1)
	case ingestSkippedSubtype, ingestSkippedSelf:
		// Both shapes count as "skipped" for the legacy Progress
		// counter; SyncWorker keeps the labels distinct in its own
		// per-source metric vocabulary for observability.
		progress.SkippedEvents.Add(1)
	}
	return nil
}

// ingestMessageCore is the per-message core: synthesize the raw
// JSON shape ParseMessage expects, parse it, build the contextual
// prefix, push through sink.Upsert. Also writes slack_message_details.
//
// Returns a disposition label drawn from the disposition* consts.
// Callers translate the label to their own counter vocabulary.
// This split exists so SyncWorker can reuse the per-
// message core without inheriting the legacy Progress shape.
func (b *Backfill) ingestMessageCore(ctx context.Context, channelID string, msg slack.Message, path string) (string, error) {
	// Round-trip through JSON so ParseMessage sees the exact shape
	// it does for live events. slack.Message + its inner Msg are
	// JSON-serializable and match Slack's own wire format. This
	// also preserves files + edited metadata unmodified. (The live
	// path arrives at this raw shape by remarshaling the inbound
	// MessageEvent instead.)
	raw, err := json.Marshal(msgEnvelope{
		Type:    "message",
		Channel: channelID,
		Msg:     msg.Msg,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	// Delegate to the shared per-message core. Backfill's posture is
	// retry-on-error: any hard error propagates so the worker's claim
	// retries (or the legacy Backfill skips + logs), and the image-
	// reconcile result — best-effort on the live path — is PROMOTED to
	// a hard error here so a backfill replay that can't reconcile
	// images re-walks rather than silently leaving the image untracked.
	res, err := ingestRawMessage(ctx, b.ingestDeps(path), raw)
	if err != nil {
		return "", err
	}
	if res.ImageErr != nil {
		return "", fmt.Errorf("attachments: %w", res.ImageErr)
	}
	if res.DocErr != nil {
		return "", fmt.Errorf("documents: %w", res.DocErr)
	}

	// Translate the neutral disposition into the backfill metric
	// vocabulary. The success-counter NAME (diderot_slack_backfill_-
	// events_total) is caller-side on purpose — the dashboards key on
	// it. SyncWorker / ThreadWorker drive their own per-source
	// counters off the returned disposition string instead, so the
	// skipped-subtype / skipped-self / nochange disposition values
	// match the labels those workers already emit.
	switch res.Disposition {
	case ingestInserted, ingestUpdated, ingestNoChange:
		b.metrics.events.WithLabelValues(upsertDispositionLabel(res.Disposition)).Inc()
	}
	return string(res.Disposition), nil
}

// ingestDeps assembles the shared-core dependency set from the
// Backfill's fields. path is the per-call ingest-metrics label
// (ingestPathBackfill / ingestPathSync / ingestPathThreadSync).
func (b *Backfill) ingestDeps(path string) ingestDeps {
	return ingestDeps{
		pool:          b.pool,
		sink:          b.sink,
		dir:           b.dir,
		prefix:        b.prefix,
		logger:        b.logger,
		workspaceHost: b.workspaceHost,
		selfUserID:    b.selfUserIDFn,
		ingest:        b.ingest,
		path:          path,
		docGate:       b.docGate,
	}
}

// msgEnvelope is the shape ParseMessage's rawMessage expects:
// top-level type+channel, then the whole Msg flattened alongside.
// We produce this by explicit marshaling rather than re-marshaling
// slack.Message directly so the channel field (not on slack.Msg) is
// present.
type msgEnvelope struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	slack.Msg
}

// historyPage wraps GetConversationHistoryContext with the global
// limiter (if wired) for proactive pacing AND with the existing
// retry-after-sleep loop for reactive 429 handling. The two layers
// are complementary: the limiter minimizes how often we earn a
// 429 in the first place, but Slack can still issue them under
// pressure (e.g. another tenant on the same workspace burning the
// shared budget) and we honor Retry-After when that happens.
func (b *Backfill) historyPage(ctx context.Context, channelID string, opts BackfillOptions, cursor string) (*slack.GetConversationHistoryResponse, error) {
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Cursor:    cursor,
		Limit:     b.historyPageSize,
	}
	if opts.OldestTS > 0 {
		params.Oldest = formatSlackTS(opts.OldestTS)
	}
	if opts.LatestTS > 0 {
		params.Latest = formatSlackTS(opts.LatestTS)
	}
	var resp *slack.GetConversationHistoryResponse
	err := WithSlackRetry(ctx, b.limiter, methodHistory, func() error {
		var apiErr error
		resp, apiErr = b.api.GetConversationHistoryContext(ctx, params)
		if apiErr != nil && isRateLimited(apiErr) {
			b.logger.Info("backfill history rate-limited; sleeping",
				"channel", channelID, "err", apiErr)
			b.metrics.rateLimited.WithLabelValues("history").Inc()
		}
		return apiErr
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (b *Backfill) repliesCall(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	var (
		msgs    []slack.Message
		hasMore bool
		next    string
	)
	err := WithSlackRetry(ctx, b.limiter, methodReplies, func() error {
		var apiErr error
		msgs, hasMore, next, apiErr = b.api.GetConversationRepliesContext(ctx, params)
		if apiErr != nil && isRateLimited(apiErr) {
			b.logger.Info("backfill replies rate-limited; sleeping",
				"err", apiErr)
			b.metrics.rateLimited.WithLabelValues("replies").Inc()
		}
		return apiErr
	})
	if err != nil {
		return nil, false, "", err
	}
	return msgs, hasMore, next, nil
}

// formatSlackTS renders a unix-seconds float as Slack's expected
// "seconds.microseconds" string. Uses %.6f rather than manual
// secs+micros extraction because float64 cannot perfectly represent
// every decimal microsecond — a literal 1700000000.000001 rounds to
// 1700000000.00000095... internally, and truncating the fractional
// part to int micros yields ".000000". %.6f rounds correctly and
// recovers the original microsecond. Boundary errors here would
// shift the oldest_ts bound by one microsecond, exactly the
// resolution Slack uses for ts on the wire.
func formatSlackTS(f float64) string {
	return fmt.Sprintf("%.6f", f)
}

// upsertDispositionLabel maps the shared core's neutral disposition
// onto the backfill success-counter label vocabulary
// (diderot_slack_backfill_events_total{outcome=...}). The label
// spellings are load-bearing — dashboards key on them — so
// they stay caller-side rather than being emitted by the core. Only
// the upsert dispositions reach here (inserted/updated/nochange); the
// skipped dispositions are filtered out by the caller before calling.
func upsertDispositionLabel(d ingestDisposition) string {
	switch d {
	case ingestNoChange:
		return "backfill_noop"
	case ingestUpdated:
		return "backfill_update"
	default:
		return "backfill_insert"
	}
}
