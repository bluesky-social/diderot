package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
)

// SyncWorker is the continuous-pull replacement for the Stage-2
// queue-worker + tail-sync ticker. One goroutine, one DB row at a
// time, paced by the global Slack-API limiter.
//
// Per-iteration:
//
//  1. Claim the staleest eligible slack_channel_sync row by
//     promoting a candidate from "idle (or stale-running)" to
//     "in-flight" with a fresh worker_token.
//  2. Compute the oldest_ts floor (last_synced_at - overlap, or
//     MAX(events.occurred_at) - overlap when never-synced).
//  3. Sweep conversations.history newest→oldest. After each page
//     persist the cursor and heartbeat in_progress_at so a
//     subsequent stale-recovery doesn't falsely preempt a
//     long-running but healthy sweep.
//  4. On clean completion, mark the row done (preserving any
//     priority bump that fired during the sweep via the
//     IS NOT DISTINCT FROM guard); on error mark it errored.
//
// Threads: pass-2 work-list isn't drained inline. When the history
// pass discovers a thread root (reply_count > 0), it upserts the
// (channel_id, thread_ts) into slack_thread_sync; a sibling
// ThreadWorker pulls from that queue against the SAME global
// limiter.
//
// Concurrency: one SyncWorker goroutine is sufficient. Two
// concurrent workers would each contend on the same global
// limiter for zero throughput gain and would compete for FOR
// UPDATE SKIP LOCKED claims. The leader runs one; the standby
// waits its turn at the leader-election boundary.
type SyncWorker struct {
	pool      *pgxpool.Pool
	backfill  *Backfill
	logger    *slog.Logger
	metrics   *syncMetrics
	blacklist *IngestBlacklist
	cfg       SyncWorkerConfig
	// channelMode is "member" (default) or "all-public". Controls
	// which Slack API Reconcile uses for discovery. Set via
	// WithChannelMode; defaults to "member" (current behavior).
	channelMode string
	// onNewChannel fires once per newly-discovered channel (the
	// slack_channel_sync insert actually landed — never for
	// channels already tracked). cmd/diderot's Slack source wires it to
	// the join policy (Consumer.HandleNewChannel): default ask_enabled=true + DM the
	// operator for private channels. Runs inline in the reconcile
	// loop; implementations must be quick and non-fatal.
	onNewChannel func(ctx context.Context, ch slack.Channel)
}

// WithNewChannelHook registers the newly-discovered-channel
// callback. Optional; nil keeps discovery side-effect free.
func (w *SyncWorker) WithNewChannelHook(fn func(ctx context.Context, ch slack.Channel)) *SyncWorker {
	w.onNewChannel = fn
	return w
}

// SyncWorkerConfig captures the tunable knobs. All fields have
// production defaults via DefaultSyncWorkerConfig; tests inject
// tighter intervals to keep the suite fast.
type SyncWorkerConfig struct {
	// StaleAfter is how long an in-flight sweep can go without a
	// heartbeat before stale-recovery reclaims its row. Bounded
	// below by "longest plausible time between two pages" — a
	// normal page is sub-second, a worst-case page in degraded
	// mode is plausibly 5+ min, so 10 min has comfortable headroom
	// without parking dead workers for half an hour.
	StaleAfter time.Duration

	// Overlap is how far back of last_synced_at the sweep starts.
	// Same semantics as TailSyncOverlap. 30 min covers socket-mode
	// disconnect windows without making each sweep too chatty.
	Overlap time.Duration

	// IdleSleep is how long the loop pauses when claim returns no
	// eligible row. Reconcile is what makes new rows show up, so
	// the loop doesn't need to spin at high frequency.
	IdleSleep time.Duration

	// PageSize bounds each conversations.history call. Slack caps
	// at 1000 but recommends 200; we match the existing Backfill
	// engine's choice.
	PageSize int

	// DeepWindow is how far back of now() a deep sweep starts,
	// instead of the normal last_synced_at - Overlap. The deep
	// sweep periodically widens the history window so thread roots
	// older than Overlap re-surface — that's the only path by which
	// a reply posted (during a disconnect) to an old thread gets its
	// root re-harvested into slack_thread_sync. 7d covers a generous
	// disconnect/backlog window while keeping the re-walk cost
	// bounded (the sink's NoChange short-circuit makes re-seen
	// parents nearly free). Larger than Overlap by construction.
	DeepWindow time.Duration

	// DeepEvery is the per-channel cadence for deep sweeps. A channel
	// whose last_deep_sweep_at is NULL or older than this runs its
	// next claim as a deep sweep. 24h keeps the wide-window cost to
	// roughly once a day per channel while bounding worst-case
	// reply-recovery latency for old threads to ~one day.
	DeepEvery time.Duration
}

// DefaultSyncWorkerConfig returns production-ready knobs. See the
// per-field comments above for rationale.
func DefaultSyncWorkerConfig() SyncWorkerConfig {
	return SyncWorkerConfig{
		StaleAfter: 10 * time.Minute,
		Overlap:    30 * time.Minute,
		IdleSleep:  10 * time.Second,
		PageSize:   200,
		DeepWindow: 7 * 24 * time.Hour,
		DeepEvery:  24 * time.Hour,
	}
}

// NewSyncWorker wires the worker. backfill is the existing engine
// — we reuse its historyPage (limiter + retry-after) and
// ingestMessageCore (parse + sink + details upsert). Production
// passes one Backfill instance shared with the rest of the slack
// consumer; the engine is goroutine-safe at the operations
// SyncWorker uses.
func NewSyncWorker(pool *pgxpool.Pool, bf *Backfill, logger *slog.Logger, reg prometheus.Registerer) *SyncWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SyncWorker{
		pool:     pool,
		backfill: bf,
		logger:   logger,
		metrics:  newSyncMetrics(reg),
		cfg:      DefaultSyncWorkerConfig(),
	}
}

// WithConfig overrides the default tunables. Returns the receiver
// for builder-chain ergonomics. Zero-valued fields fall back to
// the default (so tests can override only what they need).
func (w *SyncWorker) WithConfig(cfg SyncWorkerConfig) *SyncWorker {
	def := DefaultSyncWorkerConfig()
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = def.StaleAfter
	}
	if cfg.Overlap <= 0 {
		cfg.Overlap = def.Overlap
	}
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = def.IdleSleep
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = def.PageSize
	}
	if cfg.DeepWindow <= 0 {
		cfg.DeepWindow = def.DeepWindow
	}
	if cfg.DeepEvery <= 0 {
		cfg.DeepEvery = def.DeepEvery
	}
	w.cfg = cfg
	return w
}

// WithIngestBlacklist wires the blacklist for in-claim policy
// enforcement. When set, ClaimNext checks the blacklist after the
// row is claimed; a hit flips ingest_enabled=false on the row,
// releases the claim, and continues to the next candidate. The
// per-row ingest_enabled flag is the worker's effective gate;
// blacklist is the cross-cutting policy that drives it.
func (w *SyncWorker) WithIngestBlacklist(b *IngestBlacklist) *SyncWorker {
	w.blacklist = b
	return w
}

// WithChannelMode sets the channel discovery mode for Reconcile.
// Valid values are "member" (default, uses users.conversations) and
// "all-public" (uses conversations.list and auto-joins public channels
// not already a member of, unless blacklisted). Unknown values are
// treated as "member" — callers should validate before calling this.
func (w *SyncWorker) WithChannelMode(mode string) *SyncWorker {
	w.channelMode = mode
	return w
}

// Claim is the in-flight handle for one channel's sweep. Carries
// the values markDone needs to detect a stolen claim (worker_token)
// or a priority bump (claimedLastSyncedAt), plus the originating
// oldest_ts bound that resume needs to keep Slack history walking
// the same window the original sweep started.
type Claim struct {
	ChannelID           string
	WorkerToken         uuid.UUID
	Cursor              string
	OldestTS            float64 // unix-seconds; 0 = no oldest bound
	ClaimedLastSyncedAt *time.Time

	// Deep marks this claim as a deep sweep: the fresh-sweep oldest
	// floor is now-DeepWindow (7d) instead of last_synced_at-Overlap,
	// and a clean MarkDone stamps last_deep_sweep_at. Decided at claim
	// time by ClaimNext (last_deep_sweep_at NULL or older than
	// DeepEvery), and ONLY for fresh sweeps (cursor IS NULL): a
	// resumed claim inherits the originating sweep's window via
	// OldestTS, so promoting it to deep would stamp
	// last_deep_sweep_at without the wide walk ever happening — a
	// parked normal sweep that straddled the cadence boundary would
	// then swallow that cycle's deep pass. A genuine deep sweep that
	// crashes mid-walk resumes as Deep=false (no stamp), so the next
	// fresh claim re-runs the deep pass — at-least-once, never lost.
	Deep bool
}

// Run drives the worker forever. Returns ctx.Err() on cancellation;
// otherwise loops indefinitely. Production runs it under the Slack
// source's Run, which only a leader term starts, so a non-leader pod
// doesn't fight for claims.
func (w *SyncWorker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claim, err := w.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warn("sync worker: claim failed", "err", err)
			if sleepErr := sleepCtx(ctx, w.cfg.IdleSleep); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		if claim == nil {
			// Nothing eligible. Reconcile is what creates new rows
			// (and live socket-mode events bump priorities), so a
			// modest sleep here is fine.
			w.metrics.idleTicks.Inc()
			if sleepErr := sleepCtx(ctx, w.cfg.IdleSleep); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		if err := w.SweepOne(ctx, claim); err != nil {
			if errors.Is(err, errClaimLost) {
				// Stale-recovery (or another worker) reclaimed the
				// row mid-sweep. Don't write state — the new
				// claimant's saveCursor/markDone owns the row now.
				w.metrics.sweeps.WithLabelValues("preempted").Inc()
				w.logger.Info("sync worker: claim lost mid-sweep; surrendering",
					"channel", claim.ChannelID)
				continue
			}
			if errors.Is(err, context.Canceled) {
				// Process is shutting down; don't try to write to
				// a cancelled pool. The next leader's stale-
				// recovery will reclaim this row.
				return err
			}
			w.metrics.sweeps.WithLabelValues("error").Inc()
			if mErr := w.MarkError(ctx, claim, err); mErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				w.logger.Warn("sync worker: mark-error failed",
					"channel", claim.ChannelID, "err", mErr, "sweep_err", err)
			}
			continue
		}
		w.metrics.sweeps.WithLabelValues("ok").Inc()
		if err := w.MarkDone(ctx, claim); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warn("sync worker: mark-done failed",
				"channel", claim.ChannelID, "err", err)
		}
	}
}

// errClaimLost surfaces from saveCursor / markDone when the
// worker_token guard fails (UPDATE affected 0 rows). The Run
// loop treats this as "another claimant owns the row now;
// silently surrender."
var errClaimLost = errors.New("sync worker: claim lost")

// ClaimNext atomically promotes the staleest eligible row to
// in-flight with a fresh worker_token. Returns nil (with no
// error) when no row is claimable; this is normal — the caller
// sleeps + retries.
//
// Eligibility:
//   - ingest_enabled = true
//   - in_progress_at IS NULL (idle) OR in_progress_at older than
//     StaleAfter (stale-recovery candidate)
//
// Ordering: COALESCE(last_synced_at, '-infinity') ASC. NULLs
// (never-synced) sort first; older sweeps next.
//
// Blacklist enforcement: if a blacklist is wired, the post-claim
// check flips ingest_enabled=false on a hit and releases the
// claim. Caller-level loop continues to the next candidate. This
// is naturally bounded — each iteration either returns a real
// candidate or transitions one row to ingest_enabled=false (and
// thus out of the eligible set), so a backlog of N blacklisted
// rows resolves in N iterations of cheap UPDATEs.
func (w *SyncWorker) ClaimNext(ctx context.Context) (*Claim, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("uuid: %w", err)
		}
		var (
			channelID           string
			cursor              *string
			oldestTS            *float64
			claimedLastSyncedAt *time.Time
			deep                bool
		)
		// One round-trip: WITH SELECT...FOR UPDATE SKIP LOCKED + UPDATE.
		// The UPDATE returns the freshly-claimed row's pre-UPDATE
		// cursor + oldest_ts + last_synced_at via the CTE pattern.
		// The $stale_after parameter is passed as an interval string
		// (Go's time.Duration formatted as Postgres interval).
		//
		// Deep-sweep decision is made HERE, at claim time. The
		// RETURNING expression evaluates the pre-UPDATE
		// last_deep_sweep_at against the cadence ($3 = DeepEvery),
		// gated on pick.cursor IS NULL: only FRESH sweeps can be
		// deep. A resumed claim (cursor != "") inherits the
		// originating sweep's narrow window via oldest_ts, so
		// promoting it would stamp last_deep_sweep_at in MarkDone
		// without the 7d walk having happened — silently swallowing
		// that cycle's deep pass whenever a parked/crashed normal
		// sweep straddles the cadence boundary. The flip side
		// (a crashed DEEP sweep resumes as Deep=false and never
		// stamps) is the safe direction: the next fresh claim just
		// runs the deep pass again.
		err = w.pool.QueryRow(ctx, `
			WITH pick AS (
			    SELECT channel_id, last_synced_at, cursor, oldest_ts,
			           last_deep_sweep_at
			      FROM slack_channel_sync
			     WHERE ingest_enabled
			       AND (in_progress_at IS NULL
			            OR in_progress_at < now() - $2::interval)
			     ORDER BY COALESCE(last_synced_at, '-infinity'::timestamptz)
			     LIMIT 1
			     FOR UPDATE SKIP LOCKED
			)
			UPDATE slack_channel_sync s
			   SET in_progress_at = now(),
			       worker_token   = $1,
			       last_error     = NULL
			  FROM pick
			 WHERE s.channel_id = pick.channel_id
			RETURNING s.channel_id, pick.last_synced_at, pick.cursor, pick.oldest_ts,
			          (pick.cursor IS NULL
			           AND (pick.last_deep_sweep_at IS NULL
			                OR pick.last_deep_sweep_at < now() - $3::interval))
		`, token, intervalString(w.cfg.StaleAfter), intervalString(w.cfg.DeepEvery)).
			Scan(&channelID, &claimedLastSyncedAt, &cursor, &oldestTS, &deep)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("claim: %w", err)
		}

		// Blacklist enforcement: post-claim because the in-claim
		// SQL is awkward to extend. Hot path is a lock-free in-
		// memory Match; failure here means flipping ingest_enabled
		// false (so the row drops out of the eligible set) and
		// releasing the claim so another worker doesn't see a
		// stuck in_progress_at.
		if w.blacklist != nil {
			if entry, blocked := w.blacklist.Match(channelID); blocked {
				w.metrics.blacklisted.Inc()
				if _, dErr := w.pool.Exec(ctx, `
					UPDATE slack_channel_sync
					   SET ingest_enabled = false,
					       in_progress_at = NULL,
					       worker_token   = NULL,
					       last_error     = $2
					 WHERE channel_id = $1
					   AND worker_token = $3
				`, channelID,
					fmt.Sprintf("blacklisted; ingest_enabled flipped (reason: %q)", entry.Reason),
					token); dErr != nil {
					w.logger.Warn("sync worker: blacklist disable failed",
						"channel", channelID, "err", dErr)
					// Best-effort — the row stays claimed for up to
					// StaleAfter, then stale-recovery picks it up
					// and the blacklist check runs again. Not a
					// correctness issue; surfaces as one wasted
					// claim slot.
				}
				w.logger.Info("sync worker: archived blacklisted row",
					"channel", channelID, "reason", entry.Reason)
				continue
			}
		}

		w.metrics.claims.Inc()
		c := &Claim{
			ChannelID:           channelID,
			WorkerToken:         token,
			ClaimedLastSyncedAt: claimedLastSyncedAt,
			Deep:                deep,
		}
		if cursor != nil {
			c.Cursor = *cursor
		}
		if oldestTS != nil {
			c.OldestTS = *oldestTS
		}
		return c, nil
	}
}

// SweepOne paginates through conversations.history starting at the
// claim's cursor (or fresh, if cursor is empty), ingests each
// message, and persists the cursor + heartbeat after each page.
// Returns nil on clean completion; errClaimLost on stolen-claim;
// any other error otherwise.
//
// Resume semantics: Slack's conversations.history cursor is opaque
// and does NOT encode the oldest/latest bounds — the cursor is a
// pointer into a specific result set, and oldest must be re-supplied
// on every page or pagination walks all the way to channel creation.
// To make crash + resume sound, the originating sweep's oldest_ts
// is persisted alongside the cursor (saved by saveCursor every page,
// returned by claim) and re-supplied on every resumed call.
//
// Fresh sweep (cursor == ""): compute oldest_ts from
// MIN(last_synced_at, MAX(events.occurred_at)) - overlap, persist it
// alongside cursor=NULL (so a crash before the first saveCursor
// recovers cleanly), and start paginating.
//
// Resumed sweep (cursor != ""): use the claim's OldestTS verbatim.
// Recomputing would change the window mid-walk; either over-fetching
// (no oldest) or under-fetching (newer oldest than the cursor's
// position).
func (w *SyncWorker) SweepOne(ctx context.Context, c *Claim) error {
	start := time.Now()
	defer func() {
		w.metrics.sweepDuration.Observe(time.Since(start).Seconds())
	}()

	// KNOWN IMPRECISION: c.Deep is false for resumed claims (ClaimNext
	// only marks fresh sweeps deep), so a deep sweep that crashes
	// mid-walk resumes with tier="normal" — its remaining inserts land
	// in the {tier="normal"} series and can inflate the socket-miss
	// signal. Rare (crash + resume of a mid-flight deep sweep) and
	// visible as a burst; fixing it properly means persisting the
	// tier alongside the cursor. Not worth a schema column for a
	// metric label yet.
	tier := "normal"
	if c.Deep {
		tier = "deep"
	}
	w.metrics.sweepTier.WithLabelValues(tier).Inc()

	cursor := c.Cursor
	oldestTS := c.OldestTS
	if cursor == "" {
		o, err := w.computeOldestTS(ctx, c)
		if err != nil {
			return fmt.Errorf("compute oldest: %w", err)
		}
		oldestTS = o
		// Persist the originating oldest_ts under the worker_token
		// guard so a crash before the first saveCursor still
		// records what window this sweep is walking. Cursor stays
		// empty; the next claim sees cursor=NULL → fresh recompute.
		// We persist oldest_ts even though cursor=NULL so a future
		// design that wants to surface "current sweep window" for
		// observability can read it directly.
		if err := w.persistOldestTS(ctx, c, oldestTS); err != nil {
			return err
		}
	}

	opts := BackfillOptions{OldestTS: oldestTS}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageStart := time.Now()
		resp, err := w.backfill.historyPage(ctx, c.ChannelID, opts, cursor)
		w.metrics.pageLatency.Observe(time.Since(pageStart).Seconds())
		if err != nil {
			return fmt.Errorf("history page: %w", err)
		}
		w.metrics.pages.Inc()

		for _, msg := range resp.Messages {
			// Skip thread replies on the history pass; they're the
			// thread worker's job. We DO record the thread roots for
			// the ThreadWorker's queue.
			isReply := msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp
			if isReply {
				continue
			}
			disp, ingErr := w.backfill.ingestMessageCore(ctx, c.ChannelID, msg, ingestPathSync)
			if ingErr != nil {
				// Per-message failure: count + continue. Don't fail
				// the whole sweep over one parse-error or one row's
				// upsert hiccup; the cursor advances regardless and
				// the next sweep's NoChange short-circuit handles
				// the dedup.
				w.metrics.messages.WithLabelValues("parse_error", tier).Inc()
				w.logger.Warn("sync worker: ingest failed; skipping",
					"channel", c.ChannelID, "ts", msg.Timestamp, "err", ingErr)
				continue
			}
			w.metrics.messages.WithLabelValues(disp, tier).Inc()

			// Thread root: hand off to the sibling ThreadWorker via
			// slack_thread_sync. enqueueThreadRoot gates on
			// latest_reply so an unchanged thread isn't re-walked,
			// and preserves the livelock-prevention rule (in-flight
			// rows left alone). See enqueueThreadRoot for the full
			// enqueue-vs-markDone race reasoning.
			if msg.ReplyCount > 0 {
				if err := w.enqueueThreadRoot(ctx, c.ChannelID, msg.Timestamp, msg.LatestReply); err != nil {
					// Best-effort: log and continue. The next sweep's
					// thread-root rediscovery will re-attempt the
					// upsert; we'd rather lose one queue insert than
					// fail the whole history sweep.
					w.logger.Warn("sync worker: thread-root enqueue failed",
						"channel", c.ChannelID, "thread_ts", msg.Timestamp, "err", err)
				}
			}
		}

		// Done? End-of-pagination = sweep complete.
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			return nil
		}
		cursor = resp.ResponseMetaData.NextCursor

		// Persist cursor + heartbeat. If the worker_token guard
		// fails, our claim was stolen — surrender.
		if err := w.SaveCursor(ctx, c, cursor); err != nil {
			return err
		}
	}
}

// SaveCursor persists the cursor and heartbeats in_progress_at.
// Both writes happen in one UPDATE so a partial failure can't
// leave a stale heartbeat with a fresh cursor or vice versa.
// oldest_ts is also re-stamped from the claim's value so a row
// inspected mid-sweep always shows the current window.
//
// 0-rows-affected = claim lost. The caller treats this as a hard
// signal to abort the sweep without writing further state — the
// new claimant owns this row's progress now.
func (w *SyncWorker) SaveCursor(ctx context.Context, c *Claim, cursor string) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET cursor         = $2,
		       oldest_ts      = $4,
		       in_progress_at = now()
		 WHERE channel_id = $1
		   AND worker_token = $3
	`, c.ChannelID, cursor, c.WorkerToken, nullableFloat64(c.OldestTS))
	if err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// persistOldestTS writes the originating oldest_ts on a fresh sweep,
// before the first page. Same shape as SaveCursor (worker_token
// guard, errClaimLost on 0-rows). Called once per fresh sweep;
// subsequent SaveCursor calls re-stamp it (idempotent).
func (w *SyncWorker) persistOldestTS(ctx context.Context, c *Claim, oldestTS float64) error {
	c.OldestTS = oldestTS
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET oldest_ts      = $2,
		       in_progress_at = now()
		 WHERE channel_id = $1
		   AND worker_token = $3
	`, c.ChannelID, nullableFloat64(oldestTS), c.WorkerToken)
	if err != nil {
		return fmt.Errorf("persist oldest_ts: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// nullableFloat64 maps a zero oldestTS to a SQL NULL so the
// "no oldest bound" case (full re-fetch) doesn't get persisted as
// "oldest_ts = 0" (which Slack would interpret as "everything since
// epoch" — same effect, but the NULL form is the more readable
// representation when an operator inspects the row).
func nullableFloat64(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// MarkDone clears the in-flight state and stamps last_synced_at.
// Preserves any priority bump (live socket-mode event during the
// sweep) by checking IS NOT DISTINCT FROM the claim-time
// last_synced_at: if it differs, a bump fired and we leave the
// bumped value alone so the channel stays near the front of the
// queue.
//
// last_deep_sweep_at advances to now() only when this claim ran as a
// deep sweep (c.Deep). The $4 boolean gates the CASE so a normal
// sweep leaves the prior deep-sweep timestamp untouched — otherwise
// every normal sweep would reset the cadence clock and the deep pass
// would never fire. A deep sweep that completes cleanly resets the
// clock so the next deep pass is one DeepEvery out.
//
// 0-rows-affected = claim lost. Caller logs and moves on; the
// other claimant will write the canonical state.
func (w *SyncWorker) MarkDone(ctx context.Context, c *Claim) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET in_progress_at = NULL,
		       cursor         = NULL,
		       oldest_ts      = NULL,
		       worker_token   = NULL,
		       last_synced_at = CASE
		           WHEN last_synced_at IS NOT DISTINCT FROM $2
		           THEN now()
		           ELSE last_synced_at
		       END,
		       last_deep_sweep_at = CASE
		           WHEN $4 THEN now()
		           ELSE last_deep_sweep_at
		       END,
		       last_error     = NULL
		 WHERE channel_id = $1
		   AND worker_token = $3
	`, c.ChannelID, c.ClaimedLastSyncedAt, c.WorkerToken, c.Deep)
	if err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// MarkError records the error message (truncated to 512 bytes)
// and parks the row for one StaleAfter window before it's
// reclaimable. Does NOT advance last_synced_at — a failed sweep
// doesn't count as "we read this channel through to that point
// in time."
//
// Why parked-not-released: leaving in_progress_at = NULL and
// last_synced_at unchanged would mean the failing row stays at
// (or near) the front of the priority queue and gets reclaimed
// on the very next ClaimNext iteration — an infinite retry loop
// that burns API budget and starves every other channel.
//
// Setting in_progress_at = now() keeps the row out of the
// eligible set until stale-recovery picks it up at
// (now + StaleAfter). That gives errors a natural 10-min backoff
// per attempt without needing a separate "next_attempt_at"
// column. The worker_token stays NULL so the next claim is
// "this row is stale-recovery, mint a fresh token."
//
// 0-rows-affected = claim lost. Same handling as MarkDone:
// silently surrender; the other claimant owns the row.
func (w *SyncWorker) MarkError(ctx context.Context, c *Claim, cause error) error {
	msg := truncateRunes(cause.Error(), 512)
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET in_progress_at = now(),
		       cursor         = NULL,
		       oldest_ts      = NULL,
		       worker_token   = NULL,
		       last_error     = $2
		 WHERE channel_id = $1
		   AND worker_token = $3
	`, c.ChannelID, msg, c.WorkerToken)
	if err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errClaimLost
	}
	return nil
}

// computeOldestTS returns the unix-seconds floor for a fresh sweep.
//
// Deep sweep (c.Deep): floor = now() - DeepWindow (7d), unconditionally.
// This widens the history window so thread parents older than the
// normal overlap re-surface, which is the only path by which a reply
// posted (during a disconnect) to an old thread gets its root
// re-harvested into slack_thread_sync. The wider re-walk is cheap:
// the sink's NoChange short-circuit makes already-seen parents nearly
// free, and the latest_reply gate on enqueueThreadRoot means only
// threads whose latest_reply actually advanced get re-enqueued. Deep
// floor ignores last_synced_at entirely — a recently-swept channel
// still needs the periodic wide pass to catch old-thread replies.
//
// Normal sweep: floor = last_synced_at - overlap.
//
// Why not MIN(last_synced_at, MAX(events.occurred_at)) - overlap
// (which is what the legacy TailSync uses): in the legacy design,
// last_synced_at was stamped at sweep START, so a crash mid-sweep
// could leave last_synced_at ahead of what was actually ingested,
// and the MIN was the safety net that re-anchored the floor to the
// most-recent ingested message. SyncWorker stamps last_synced_at at
// MarkDone — only on successful completion — so the value is
// already the correct anchor and there's no scenario where it
// out-runs ingest. The MIN is dead weight here, and worse, it
// actively hurts: for an inactive channel (last_synced_at = now,
// maxEventTS = a week ago), the MIN would force the next sweep to
// start at (a week ago - overlap), re-walking all of channel
// history every cycle and burning API budget on NoChange
// short-circuits.
//
// Crash safety: if the sweep crashes before MarkDone,
// last_synced_at retains its prior value. The next claim
// recomputes from that older anchor, which is the correct
// behavior. Any messages that ARE in events from the
// partial-but-not-completed sweep are protected by the sink's
// idempotent (source, external_id) upsert — re-walking them is a
// NoChange no-op.
func (w *SyncWorker) computeOldestTS(ctx context.Context, c *Claim) (float64, error) {
	normal, err := w.normalOldestTS(ctx, c.ChannelID)
	if err != nil {
		return 0, err
	}
	if !c.Deep {
		return normal, nil
	}
	// Deep sweep: floor = MIN(normal floor, now()-DeepWindow). Deep
	// WIDENS the window, never narrows it — a never-synced channel
	// (normal floor 0 = "all history") must still pull everything on
	// its first claim, which is always deep (last_deep_sweep_at is
	// NULL on fresh rows). Without the MIN, the deep floor would
	// truncate a new channel's initial sweep to 7 days.
	if normal == 0 {
		return 0, nil
	}
	floor := time.Now().Add(-w.cfg.DeepWindow)
	if floor.Before(time.Unix(0, 0)) {
		return 0, nil
	}
	deep := float64(floor.UnixNano()) / 1e9
	return math.Min(normal, deep), nil
}

// normalOldestTS is the non-deep floor: last_synced_at - overlap, or
// 0 (no bound — pull all history) when the channel was never synced.
func (w *SyncWorker) normalOldestTS(ctx context.Context, channelID string) (float64, error) {
	var lastSync *time.Time
	err := w.pool.QueryRow(ctx,
		`SELECT last_synced_at FROM slack_channel_sync WHERE channel_id = $1`,
		channelID,
	).Scan(&lastSync)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if lastSync == nil {
		// Never-synced channel (or operator-triggered re-backfill
		// via UPDATE ... SET last_synced_at = NULL): pull all
		// history Slack will give us.
		return 0, nil
	}
	floor := lastSync.Add(-w.cfg.Overlap)
	if floor.Before(time.Unix(0, 0)) {
		return 0, nil
	}
	return float64(floor.UnixNano()) / 1e9, nil
}

// Reconcile inserts a slack_channel_sync row for every channel
// the bot is currently a member of (per users.conversations).
// Channels we already track keep their sweep state (last_synced_at,
// cursor). Returns the number of fresh inserts.
//
// Membership is also what turns a row back on. ingest_enabled goes
// false only when the bot leaves a channel (the member_left event,
// or the disable pass below) or the channel is blacklisted, so a
// disabled row for a channel the walk lists, and the blacklist does
// not, is one the bot rejoined or an operator unblocked: Reconcile
// sets it ingest_enabled=true. Its last_synced_at is untouched, so
// the next sweep reads from where the last one ended and picks up
// what was posted in between.
//
// Behavior is identical in both "member" and "all-public" modes —
// membership is always the source of truth for what gets indexed.
// In all-public mode the caller runs JoinSweep first (in the same
// tick) so newly joined channels are picked up here immediately.
//
// The blacklist is consulted at insert time so newly-discovered
// blacklisted channels land with ingest_enabled=false out of the
// gate (the worker would also flip them at claim time, but
// gating up front avoids one wasted claim cycle per blacklisted
// channel).
//
// Reconcile also runs a disable pass: rows for channels the bot is
// no longer a member of (absent from the full users.conversations
// walk) get ingest_enabled=false. This is the durable backstop for a
// missed member_left_channel event — socket mode is at-most-once past
// ~30s, so a missed leave would otherwise leave the worker forever
// failing to sweep a channel it can't read. CRITICAL: the disable
// pass runs only if the full paginated walk completed WITHOUT error.
// A partial membership set (some page failed) must never mass-disable
// — the missing channels would look "absent" purely because we never
// saw the page that lists them.
//
// Replaces Queue.Reconcile in the new design. Same upstream
// users.conversations call — the rate-limited shape is unchanged
// — but the destination table is slack_channel_sync, not
// slack_channel_backfills.
func (w *SyncWorker) Reconcile(ctx context.Context, api reconcileAPI) (int, error) {
	var (
		cursor  string
		added   int
		resumed int
		checked int
	)
	// memberSet accumulates every channel id the bot is a member of
	// across ALL pages. Only consulted for the disable pass, and only
	// when the full walk completes — a mid-walk error returns before
	// we touch it.
	memberSet := make(map[string]struct{})
	var lim *Limiter
	if w.backfill != nil {
		lim = w.backfill.limiter
	}
	for {
		if err := ctx.Err(); err != nil {
			return added, err
		}
		var (
			channels []slack.Channel
			next     string
		)
		err := WithSlackRetry(ctx, lim, methodConvUserList, func() error {
			var apiErr error
			channels, next, apiErr = api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
				ExcludeArchived: true,
				Types:           []string{"public_channel", "private_channel"},
				Limit:           200,
				Cursor:          cursor,
			})
			if apiErr != nil && isRateLimited(apiErr) {
				w.logger.Info("sync reconcile: rate-limited; sleeping",
					"err", apiErr)
			}
			return apiErr
		})
		if err != nil {
			// Partial walk: return BEFORE the disable pass. Inserts
			// done so far are harmless (they're all channels we
			// confirmed membership of); the danger is only disabling
			// on an incomplete set, which we skip by returning here.
			return added, fmt.Errorf("users.conversations: %w", err)
		}
		for _, c := range channels {
			checked++
			memberSet[c.ID] = struct{}{}
			ingestEnabled := true
			if w.blacklist != nil {
				if _, blocked := w.blacklist.Match(c.ID); blocked {
					ingestEnabled = false
				}
			}
			// A conflict that changes nothing returns no row.
			var inserted bool
			err := w.pool.QueryRow(ctx, `
				INSERT INTO slack_channel_sync (channel_id, ingest_enabled)
				VALUES ($1, $2)
				ON CONFLICT (channel_id) DO UPDATE
				   SET ingest_enabled = true
				 WHERE $2 AND NOT slack_channel_sync.ingest_enabled
				RETURNING xmax::text = '0'
			`, c.ID, ingestEnabled).Scan(&inserted)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				w.logger.Warn("sync reconcile: insert failed; continuing",
					"channel", c.ID, "err", err)
				continue
			}
			if !inserted {
				resumed++
				w.logger.Info("sync reconcile: the bot is in the channel again; reading it again",
					"channel", c.ID)
				continue
			}
			added++
			if w.onNewChannel != nil {
				w.onNewChannel(ctx, c)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	// Disable pass. The full walk completed without error, so
	// memberSet is authoritative membership truth. Disable any
	// currently-enabled row whose channel is absent from it — that's
	// a channel the bot has left (missed member_left) or was removed
	// from. See disableAbsentChannels for the safety guards.
	disabled, err := w.disableAbsentChannels(ctx, memberSet)
	if err != nil {
		// A disable-pass failure is non-fatal to reconcile: the
		// inserts already landed and the pass retries next tick.
		w.logger.Warn("sync reconcile: disable pass failed; continuing",
			"err", err)
	}
	w.logger.Info("sync reconcile complete",
		"checked", checked, "inserted", added, "resumed", resumed, "disabled", disabled)
	return added, nil
}

// disableAbsentChannels flips ingest_enabled=false on every
// slack_channel_sync row whose channel_id is not in memberSet AND is
// currently enabled. It is the backstop for a missed
// member_left_channel event (socket mode drops leaves past ~30s).
//
// Safety:
//   - Caller MUST only invoke this after a COMPLETE users.conversations
//     walk. A partial set would wrongly flag every unseen channel as
//     "left" and mass-disable. This function trusts its argument.
//   - An empty memberSet is treated as "no membership data" and skips
//     the pass entirely rather than disabling everything. A bot with
//     genuinely zero channels is indistinguishable here from a walk
//     that returned nothing spurious, so we choose the safe no-op.
//   - Blacklist-disabled rows are already ingest_enabled=false, so the
//     `AND ingest_enabled` filter leaves them untouched.
//   - Rows with an active (non-stale) in-flight sweep are excluded:
//     in_progress_at recent means a worker is mid-sweep against a
//     membership snapshot that may be newer than this reconcile's.
//     Disabling under it would fight a live claim. A stale
//     in_progress_at (older than StaleAfter) is fair game — that sweep
//     is abandoned. The excluded live-sweep row gets disabled on the
//     next reconcile tick once its claim clears, so nothing is
//     permanently missed.
//
// Note on operator re-enable races: an operator manually setting
// ingest_enabled=true for a channel the bot IS in stays consistent —
// membership is the truth source, and a channel the bot is in appears
// in memberSet, so it's never a disable target.
func (w *SyncWorker) disableAbsentChannels(ctx context.Context, memberSet map[string]struct{}) (int, error) {
	if len(memberSet) == 0 {
		// No membership evidence — refuse to disable anything. See
		// docstring: safer to no-op than to interpret an empty walk as
		// "bot is in no channels, disable all".
		return 0, nil
	}
	members := make([]string, 0, len(memberSet))
	for id := range memberSet {
		members = append(members, id)
	}
	tag, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET ingest_enabled = false
		 WHERE ingest_enabled
		   AND channel_id <> ALL($1)
		   AND (in_progress_at IS NULL
		        OR in_progress_at < now() - $2::interval)
	`, members, intervalString(w.cfg.StaleAfter))
	if err != nil {
		return 0, fmt.Errorf("disable absent channels: %w", err)
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		w.logger.Info("sync reconcile: disabled channels absent from membership",
			"count", n)
	}
	return n, nil
}

// JoinSweep joins every non-blacklisted public channel the bot is not
// yet a member of. It only changes Slack-side membership — it writes
// nothing to slack_channel_sync. After JoinSweep completes, the
// caller should run Reconcile (in the same tick) so newly joined
// channels are picked up by the normal membership-based discovery
// path.
//
// Designed for "all-public" mode. Callers in other modes should not
// call JoinSweep; the method is safe to call regardless but is a
// no-op if the workspace has no public channels the bot isn't in.
//
// Return value is the number of channels successfully joined this
// sweep (already-member and blacklisted channels are not counted).
// Errors on individual channels are logged and skipped — one bad
// channel must not stall the sweep. A missing_scope error is logged
// at ERROR and the loop continues for already-member channels.
func (w *SyncWorker) JoinSweep(ctx context.Context, api reconcileAPI) (int, error) {
	var (
		cursor  string
		joined  int
		checked int
	)
	var lim *Limiter
	if w.backfill != nil {
		lim = w.backfill.limiter
	}
	for {
		if err := ctx.Err(); err != nil {
			return joined, err
		}
		var (
			channels []slack.Channel
			next     string
		)
		err := WithSlackRetry(ctx, lim, methodConvList, func() error {
			var apiErr error
			channels, next, apiErr = api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
				ExcludeArchived: true,
				Types:           []string{"public_channel"},
				Limit:           200,
				Cursor:          cursor,
			})
			if apiErr != nil && isRateLimited(apiErr) {
				w.logger.Info("join sweep: rate-limited; sleeping", "err", apiErr)
			}
			return apiErr
		})
		if err != nil {
			return joined, fmt.Errorf("join sweep conversations.list: %w", err)
		}
		for _, c := range channels {
			checked++
			if c.IsMember {
				continue
			}
			if w.blacklist != nil {
				if _, blocked := w.blacklist.Match(c.ID); blocked {
					continue
				}
			}
			joinErr := WithSlackRetry(ctx, lim, methodConvJoin, func() error {
				var apiErr error
				_, _, _, apiErr = api.JoinConversationContext(ctx, c.ID)
				return apiErr
			})
			if joinErr != nil {
				if isSlackErrorCode(joinErr, "missing_scope") {
					w.logger.Error("join sweep: missing channels:join scope; all-public mode requires channels:join",
						"channel", c.ID, "scope_required", "channels:join")
					continue
				}
				w.logger.Warn("join sweep: join failed; skipping channel",
					"channel", c.ID, "err", joinErr)
				continue
			}
			w.logger.Info("join sweep: joined channel", "channel", c.ID)
			joined++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	w.logger.Info("join sweep complete", "checked", checked, "joined", joined)
	return joined, nil
}

// isSlackErrorCode reports whether err is a Slack API error with the
// given error code (e.g. "missing_scope", "is_archived"). slack-go
// returns SlackErrorResponse for API-level errors; this unwraps both
// value and pointer forms to match the pattern in isDefinitiveNotFound.
func isSlackErrorCode(err error, code string) bool {
	var sErr slack.SlackErrorResponse
	if errors.As(err, &sErr) {
		return sErr.Err == code
	}
	var pErr *slack.SlackErrorResponse
	if errors.As(err, &pErr) && pErr != nil {
		return pErr.Err == code
	}
	return false
}

// ChannelSync is the operator's view of slack_channel_sync, the queue the
// leader's SyncWorker drains. It reads and writes only Postgres, so every
// replica has one.
type ChannelSync struct {
	pool *pgxpool.Pool
}

func NewChannelSync(pool *pgxpool.Pool) *ChannelSync {
	return &ChannelSync{pool: pool}
}

// RequestBackfill is the operator-driven "re-walk this channel"
// trigger. Sets last_synced_at = NULL so the channel sorts to the
// front of the worker's pick order. Also clears any in-flight state
// so a stuck row gets re-claimed cleanly. Returns true if the row
// existed (and was reset), false if the channel wasn't tracked yet
// (in which case a row is also inserted, with last_synced_at NULL).
func (c *ChannelSync) RequestBackfill(ctx context.Context, channelID string) (bool, error) {
	// A blacklist row is itself an operator decision, and re-backfill
	// is also operator-driven; surfacing the conflict beats silently
	// re-enabling the channel. The table is read here, not a
	// worker's snapshot of it, so the answer is the same on every
	// replica.
	var reason string
	err := c.pool.QueryRow(ctx, `SELECT reason FROM slack_ingest_blacklist WHERE channel_id = $1`, channelID).Scan(&reason)
	switch {
	case err == nil:
		return false, fmt.Errorf("channel %s is on the ingest blacklist (reason: %q); remove it from the blacklist first, on the channel's page under /admin/connectors/slack",
			channelID, reason)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("request backfill %s: read blacklist: %w", channelID, err)
	}
	var existed bool
	err = c.pool.QueryRow(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ($1, NULL, true)
		ON CONFLICT (channel_id) DO UPDATE
		   SET last_synced_at = NULL,
		       cursor         = NULL,
		       oldest_ts      = NULL,
		       in_progress_at = NULL,
		       worker_token   = NULL,
		       last_error     = NULL,
		       ingest_enabled = true
		RETURNING xmax::text <> '0'
	`, channelID).Scan(&existed)
	if err != nil {
		return false, fmt.Errorf("request backfill %s: %w", channelID, err)
	}
	return existed, nil
}

// PriorityBump pulls a channel forward in the sweep order. Called
// from the live MessageEvent handler — when a live event arrives,
// the channel is by definition active enough that the next sweep
// should happen sooner. Implemented as last_synced_at = LEAST(...,
// now() - overlap), which moves the value backward in time but
// only as far back as `overlap` ago (so we don't pull WAY back
// past where the message actually lives).
//
// Guarded WHERE last_synced_at IS NOT NULL because LEAST(NULL, ts)
// returns ts in Postgres — without the guard, a brand-new row
// (last_synced_at = NULL = -infinity in our sort order) would get
// DEMOTED from highest priority to ~now-overlap.
//
// No-op if the channel isn't tracked yet (reconcile will catch it
// soon).
func (w *SyncWorker) PriorityBump(ctx context.Context, channelID string) error {
	overlap := w.cfg.Overlap
	_, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET last_synced_at = LEAST(last_synced_at, now() - $2::interval)
		 WHERE channel_id = $1
		   AND last_synced_at IS NOT NULL
	`, channelID, intervalString(overlap))
	if err != nil {
		return fmt.Errorf("priority bump %s: %w", channelID, err)
	}
	return nil
}

// Disable flips ingest_enabled=false for a channel. Called from
// the live member_left_channel handler when the bot is removed —
// the row stays so the operator can see "this channel was
// ingested then stopped" on the channel's page, but the worker
// won't sweep it. Reconcile turns it back on once the bot is in
// the channel again.
//
// Replaces Queue.MarkArchived in the new design.
func (w *SyncWorker) Disable(ctx context.Context, channelID string) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE slack_channel_sync
		   SET ingest_enabled = false
		 WHERE channel_id = $1
	`, channelID)
	if err != nil {
		return fmt.Errorf("disable %s: %w", channelID, err)
	}
	return nil
}

// reconcileAPI is the subset of slack-go Reconcile needs.
// users.conversations (called with an empty user id) returns the
// bot's own channels. GetConversationsContext and
// JoinConversationContext are used in all-public mode only.
type reconcileAPI interface {
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
	JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error)
}

// enqueueThreadRoot upserts (channel_id, thread_ts) into
// slack_thread_sync so the sibling ThreadWorker walks the thread.
// latestReply is the parent's latest_reply (Slack ts) as seen by
// this sweep; "" when the API didn't populate it.
//
// Re-discovery semantics: only previously-completed rows
// (done_at IS NOT NULL) get re-opened, AND only when the parent's
// latest_reply has advanced past walked_latest_reply (the thread
// gate). Rows currently being walked (done_at IS NULL, regardless
// of in_progress_at) are left alone — that's the no-op path.
//
// Why not unconditionally re-open (livelock): each history sweep
// covers the last 30 minutes (Overlap) — and the deep sweep covers
// the last 7 days — re-discovering thread roots on every pass.
// Sweep cadence runs ahead of thread-walk completion for big
// threads. An unconditional upsert would NULL the in-flight
// walker's worker_token + cursor every sweep, which (a) restarts
// the walker from page 1, and (b) makes the walker's MarkDone fail
// its worker_token guard so the row stays done_at = NULL. Sweep
// re-runs, NULLs again — permanent livelock for any thread bigger
// than the cadence allows. So in-flight rows are NEVER touched.
//
// latest_reply gate (the new part): a completed row is re-opened
// only when latestReply > walked_latest_reply. walked_latest_reply
// is the value promoted at the last successful walk (markDone). If a
// thread hasn't gained a reply since we last walked it, latestReply
// equals walked_latest_reply, the WHERE fails, and we skip the
// re-walk entirely — this is what stops every reply-bearing root in
// the window from being re-walked every sweep.
//
//   - latestReply IS DISTINCT FROM ” guards the case where Slack
//     omitted latest_reply (defensive — a root with reply_count>0
//     should always carry it). Treating "" as "don't gate" would
//     re-open unconditionally; treating it as a value would compare
//     "" < any-ts and never re-open. We require a real value to
//     re-open, falling back to the conservative "skip" — the deep
//     sweep / next populated discovery will catch it.
//   - walked_latest_reply IS NULL (re-opened row was previously
//     completed before this column existed, or completed via a path
//     that didn't record it) counts as "older than anything," so a
//     populated latestReply re-opens it. NULLs sort first under the
//     `IS DISTINCT FROM ... AND (... IS NULL OR latest >walked)` form.
//
// Race: enqueue vs the walker's markDone, on the SAME row. Both are
// single-statement UPDATEs, serialized by the row lock, so they
// never interleave partially — the only question is ordering:
//
//  1. Walk in flight (done_at NULL). Sweep re-discovers with a
//     newer latestReply. This UPDATE's WHERE requires done_at IS
//     NOT NULL → no-op. The in-flight row (including its pending
//     latest_reply) is untouched. (Livelock rule.)
//  2. Walker's markDone runs: sets done_at = now(), worker_token =
//     NULL, and promotes walked_latest_reply = latest_reply (the
//     enqueue-time pending value the claim carried). Because (1)
//     was a no-op, latest_reply is still the value from when the
//     walk was enqueued — a CONSERVATIVE lower bound on what the
//     walk actually covered (the walk reads the live thread, so it
//     ingested any replies up to markDone, but only claims to have
//     covered up to the enqueue-time value).
//  3. The NEXT sweep re-discovers (done_at IS NOT NULL now). If a
//     reply arrived during the walk, latestReply > walked_latest_reply
//     → re-open. The walker re-walks and re-ingests (NoChange at the
//     sink for already-seen replies, real insert for the new one).
//
// So a reply that lands mid-walk is never lost: worst case it costs
// one extra (mostly-NoChange) re-walk one cycle later. The dangerous
// interleaving — re-open racing ahead of markDone and erasing the
// re-walk request — cannot happen: before markDone the row has
// done_at NULL, so the re-open's WHERE fails; the only viable order
// is markDone-then-reopen. The worker_token guard on markDone
// remains the backstop if a (future) code path ever DID null the
// token on an in-flight row.
//
// Plain INSERT ... ON CONFLICT DO NOTHING wouldn't satisfy the
// re-open requirement; the WHERE-gated DO UPDATE is the in-between.
func (w *SyncWorker) enqueueThreadRoot(ctx context.Context, channelID, threadTS, latestReply string) error {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO slack_thread_sync
		    (channel_id, thread_ts, enqueued_at, latest_reply)
		VALUES ($1, $2, now(), NULLIF($3, ''))
		ON CONFLICT (channel_id, thread_ts) DO UPDATE SET
		    done_at        = NULL,
		    in_progress_at = NULL,
		    cursor         = NULL,
		    worker_token   = NULL,
		    last_error     = NULL,
		    enqueued_at    = now(),
		    latest_reply   = NULLIF($3, '')
		WHERE slack_thread_sync.done_at IS NOT NULL
		  AND $3 <> ''
		  AND (slack_thread_sync.walked_latest_reply IS NULL
		       OR $3 > slack_thread_sync.walked_latest_reply)
	`, channelID, threadTS, latestReply)
	if err != nil {
		return fmt.Errorf("enqueue thread root: %w", err)
	}
	return nil
}

// intervalString formats a Go time.Duration as a Postgres interval
// literal accepted by the `$N::interval` cast in the claim SQL.
// Microsecond precision matches what Postgres natively supports;
// longer durations stay accurate, sub-millisecond durations clamp
// to "0.001 milliseconds" rather than rounding to zero (which
// would degenerate the stale-recovery threshold to "any non-null
// in_progress_at is stale" — a real bug surfaced by tests with
// short StaleAfter).
func intervalString(d time.Duration) string {
	if d <= 0 {
		return "0 microseconds"
	}
	micros := d.Microseconds()
	if micros == 0 {
		return "1 microseconds"
	}
	return fmt.Sprintf("%d microseconds", micros)
}
