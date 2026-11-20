package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
)

// ThreadWorker is the pass-2 sibling to SyncWorker. It drains
// slack_thread_sync, calling conversations.replies for each
// thread and ingesting the messages through the same backfill
// engine used by the channel sweep. Both workers share the same
// global Slack-API limiter — that's the load-bearing property
// for "concurrent callers don't self-DoS."
//
// Splitting threads from the history sweep means a noisy channel
// with a slow thread backlog doesn't block its own next history
// pass. Recent messages keep flowing; threads catch up
// asynchronously.
type ThreadWorker struct {
	pool     *pgxpool.Pool
	backfill *Backfill
	logger   *slog.Logger
	metrics  *threadMetrics
	cfg      ThreadWorkerConfig
}

// ThreadWorkerConfig captures the tunable knobs. Mirrors the
// SyncWorker shape; production wires the defaults via
// DefaultThreadWorkerConfig.
type ThreadWorkerConfig struct {
	// StaleAfter is how long an in-flight thread walk can go
	// without a heartbeat before stale-recovery reclaims its
	// row. Same semantics as SyncWorkerConfig.StaleAfter.
	StaleAfter time.Duration

	// IdleSleep is how long the loop pauses when claim returns no
	// eligible row. Pass-1 history sweeps create new rows; the
	// thread queue doesn't need a tight loop.
	IdleSleep time.Duration
}

// DefaultThreadWorkerConfig returns production-ready knobs.
func DefaultThreadWorkerConfig() ThreadWorkerConfig {
	return ThreadWorkerConfig{
		StaleAfter: 10 * time.Minute,
		IdleSleep:  10 * time.Second,
	}
}

// NewThreadWorker wires the worker. backfill is shared with the
// channel SyncWorker; we reuse repliesCall (limiter + retry-after)
// and ingestMessageCore (parse + sink + details upsert).
func NewThreadWorker(pool *pgxpool.Pool, bf *Backfill, logger *slog.Logger, reg prometheus.Registerer) *ThreadWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ThreadWorker{
		pool:     pool,
		backfill: bf,
		logger:   logger,
		metrics:  newThreadMetrics(reg),
		cfg:      DefaultThreadWorkerConfig(),
	}
}

// WithConfig overrides the default tunables. Returns the receiver
// for builder-chain ergonomics. Zero-valued fields fall back to
// the default.
func (w *ThreadWorker) WithConfig(cfg ThreadWorkerConfig) *ThreadWorker {
	def := DefaultThreadWorkerConfig()
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = def.StaleAfter
	}
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = def.IdleSleep
	}
	w.cfg = cfg
	return w
}

// ThreadClaim is the in-flight handle for one thread's walk.
type ThreadClaim struct {
	ChannelID   string
	ThreadTS    string
	WorkerToken uuid.UUID
	Cursor      string

	// LatestReply is the pending latest_reply (Slack ts) the sweep
	// stamped when it enqueued/re-opened this row, or "" if unset.
	// MarkDone promotes this into walked_latest_reply so the next
	// sweep's enqueueThreadRoot gate can tell whether the thread has
	// gained a reply since this walk. Carried verbatim from the row
	// at claim time; the walk itself doesn't recompute it (the walk
	// reads the live thread and may cover more, but recording the
	// enqueue-time value is the conservative choice — see
	// enqueueThreadRoot's race reasoning).
	LatestReply string
}

// Run drives the worker forever. Production runs it under the Slack
// source's Run, which only a leader term starts.
func (w *ThreadWorker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claim, err := w.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warn("thread worker: claim failed", "err", err)
			if sleepErr := sleepCtx(ctx, w.cfg.IdleSleep); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		if claim == nil {
			w.metrics.idleTicks.Inc()
			if sleepErr := sleepCtx(ctx, w.cfg.IdleSleep); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		if err := w.WalkOne(ctx, claim); err != nil {
			if errors.Is(err, errClaimLost) {
				w.metrics.walks.WithLabelValues("preempted").Inc()
				w.logger.Info("thread worker: claim lost mid-walk; surrendering",
					"channel", claim.ChannelID, "thread_ts", claim.ThreadTS)
				continue
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			w.metrics.walks.WithLabelValues("error").Inc()
			if mErr := w.MarkError(ctx, claim, err); mErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				w.logger.Warn("thread worker: mark-error failed",
					"channel", claim.ChannelID, "thread_ts", claim.ThreadTS,
					"err", mErr, "walk_err", err)
			}
			continue
		}
		w.metrics.walks.WithLabelValues("ok").Inc()
		if err := w.MarkDone(ctx, claim); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warn("thread worker: mark-done failed",
				"channel", claim.ChannelID, "thread_ts", claim.ThreadTS, "err", err)
		}
	}
}

// ClaimNext atomically promotes the oldest-enqueued claimable thread
// row to in-flight with a fresh worker_token. Returns nil (with no
// error) when no row is claimable.
//
// Eligibility:
//   - done_at IS NULL (not-yet-walked, or re-opened by a sweep
//     re-discovering the thread root)
//   - in_progress_at IS NULL (idle) OR older than StaleAfter
//
// Ordering: activity_ts DESC — the thread with the most recent
// SOURCE activity (GREATEST(thread_ts, latest_reply)) is claimed
// first, so a thread with a live reply a
// user is waiting on beats a stale historical backlog. This is a
// Slack-source recency value, NOT a processing timestamp: enqueued_at
// (when the sweep discovered the root) is deliberately not the
// recency key, because re-discovering a thread would let "recently
// enqueued" win over genuinely fresher activity. thread_ts DESC and
// channel_id are deterministic tiebreakers for equal activity_ts.
func (w *ThreadWorker) ClaimNext(ctx context.Context) (*ThreadClaim, error) {
	token, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("uuid: %w", err)
	}
	var (
		channelID   string
		threadTS    string
		cursor      *string
		latestReply *string
	)
	err = w.pool.QueryRow(ctx, `
		WITH pick AS (
		    SELECT channel_id, thread_ts, cursor, latest_reply
		      FROM slack_thread_sync
		     WHERE done_at IS NULL
		       AND (in_progress_at IS NULL
		            OR in_progress_at < now() - $2::interval)
		       AND NOT EXISTS (SELECT 1 FROM slack_ingest_blacklist blocked
		                        WHERE blocked.channel_id = slack_thread_sync.channel_id)
		     ORDER BY activity_ts DESC, thread_ts DESC, channel_id
		     LIMIT 1
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE slack_thread_sync s
		   SET in_progress_at = now(),
		       worker_token   = $1,
		       last_error     = NULL
		  FROM pick
		 WHERE s.channel_id = pick.channel_id
		   AND s.thread_ts  = pick.thread_ts
		RETURNING s.channel_id, s.thread_ts, pick.cursor, pick.latest_reply
	`, token, intervalString(w.cfg.StaleAfter)).
		Scan(&channelID, &threadTS, &cursor, &latestReply)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	w.metrics.claims.Inc()
	c := &ThreadClaim{
		ChannelID:   channelID,
		ThreadTS:    threadTS,
		WorkerToken: token,
	}
	if cursor != nil {
		c.Cursor = *cursor
	}
	if latestReply != nil {
		c.LatestReply = *latestReply
	}
	return c, nil
}

// WalkOne paginates conversations.replies starting at the claim's
// cursor (or fresh, if cursor is empty), ingests each reply, and
// persists the cursor + heartbeat after each page. Returns nil on
// clean completion; errClaimLost on stolen-claim; any other error
// otherwise.
//
// Slack's conversations.replies returns the thread root as the
// first message of the first page (oldest-first within the
// thread). The root is already in events from pass 1; re-upserting
// it is a NoChange no-op, but skipping it explicitly avoids one
// pointless sink call per thread walk.
//
// Resume semantics differ from SyncWorker.SweepOne: replies has
// no oldest/latest filter (Slack returns the whole thread); the
// cursor alone is sufficient state. So no oldest_ts persistence,
// no fresh-vs-resume branch.
func (w *ThreadWorker) WalkOne(ctx context.Context, c *ThreadClaim) error {
	start := time.Now()
	defer func() {
		w.metrics.walkDuration.Observe(time.Since(start).Seconds())
	}()

	cursor := c.Cursor
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := &slack.GetConversationRepliesParameters{
			ChannelID: c.ChannelID,
			Timestamp: c.ThreadTS,
			Cursor:    cursor,
			Limit:     200,
		}
		pageStart := time.Now()
		msgs, hasMore, next, err := w.backfill.repliesCall(ctx, params)
		w.metrics.pageLatency.Observe(time.Since(pageStart).Seconds())
		if err != nil {
			return fmt.Errorf("replies call: %w", err)
		}
		w.metrics.pages.Inc()

		for i, msg := range msgs {
			// First message of the first page is the thread root,
			// already ingested by pass 1. Skip explicitly so we
			// don't spend a sink Upsert (the NoChange short-circuit
			// would handle it anyway, but skipping is slightly
			// cheaper and the bookkeeping is clearer).
			if cursor == "" && i == 0 && msg.Timestamp == c.ThreadTS {
				continue
			}
			disp, ingErr := w.backfill.ingestMessageCore(ctx, c.ChannelID, msg, ingestPathThreadSync)
			if ingErr != nil {
				w.metrics.messages.WithLabelValues("parse_error").Inc()
				w.logger.Warn("thread worker: ingest reply failed; skipping",
					"channel", c.ChannelID, "thread_ts", c.ThreadTS,
					"ts", msg.Timestamp, "err", ingErr)
				continue
			}
			w.metrics.messages.WithLabelValues(disp).Inc()
		}

		if !hasMore || next == "" {
			return nil
		}
		cursor = next
		if err := w.SaveCursor(ctx, c, cursor); err != nil {
			return err
		}
	}
}

// SaveCursor persists the thread cursor and heartbeats
// in_progress_at under the worker_token guard.
func (w *ThreadWorker) SaveCursor(ctx context.Context, c *ThreadClaim, cursor string) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_thread_sync
		   SET cursor         = $3,
		       in_progress_at = now()
		 WHERE channel_id = $1
		   AND thread_ts  = $2
		   AND worker_token = $4
	`, c.ChannelID, c.ThreadTS, cursor, c.WorkerToken)
	if err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// MarkDone clears in-flight state and stamps done_at. Honors the
// worker_token guard: if a re-discovery upsert (in
// SyncWorker.enqueueThreadRoot) fired during the walk and NULLed
// the token, this UPDATE affects 0 rows and we silently surrender —
// the re-opened row will be reclaimed by the next ClaimNext, and
// the worker walks the thread again so the new replies aren't lost.
//
// Promotes walked_latest_reply = the claim's pending LatestReply so
// the next sweep's enqueueThreadRoot gate can tell whether the
// thread gained a reply since this walk. COALESCE(NULLIF(...))
// preserves any prior walked value when the pending value is empty
// (Slack omitted latest_reply), so an empty observation never
// regresses the gate's bound. The value only ever advances: the
// enqueue gate re-opens a completed row only when the incoming
// latest_reply is strictly greater than walked_latest_reply, so the
// pending value on a re-opened row is always >= the recorded one.
func (w *ThreadWorker) MarkDone(ctx context.Context, c *ThreadClaim) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_thread_sync
		   SET in_progress_at = NULL,
		       cursor         = NULL,
		       worker_token   = NULL,
		       done_at        = now(),
		       walked_latest_reply = COALESCE(NULLIF($4, ''), walked_latest_reply),
		       last_error     = NULL
		 WHERE channel_id = $1
		   AND thread_ts  = $2
		   AND worker_token = $3
	`, c.ChannelID, c.ThreadTS, c.WorkerToken, c.LatestReply)
	if err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// MarkError records the error and parks the row at
// in_progress_at = now() to back off for one StaleAfter window
// before the next claim. Same shape as SyncWorker.MarkError —
// without the backoff, a persistently-failing thread would
// infinite-loop at the front of the queue.
//
// Does NOT set done_at; the row stays claimable (after backoff)
// so transient errors get retried.
//
// PRESERVES cursor — a transient error deep in a thread's
// pagination should not throw away the prior pages of progress.
// The next claim resumes from the last successfully saved cursor.
// (If the cursor is genuinely stale, e.g. the thread was deleted,
// retries will continue to fail; the operator-facing last_error
// surface gives visibility.)
//
// Releases worker_token so the next ClaimNext can mint a fresh
// one — a different worker reclaiming this row owns the cursor
// from that point forward.
func (w *ThreadWorker) MarkError(ctx context.Context, c *ThreadClaim, cause error) error {
	msg := truncateRunes(cause.Error(), 512)
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_thread_sync
		   SET in_progress_at = now(),
		       worker_token   = NULL,
		       last_error     = $4
		 WHERE channel_id = $1
		   AND thread_ts  = $2
		   AND worker_token = $3
	`, c.ChannelID, c.ThreadTS, c.WorkerToken, msg)
	if err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}
