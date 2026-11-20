package sessioningest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
	"github.com/bluesky-social/diderot/internal/sessiontitle"
)

// WorkerConfig holds the knobs the session-ingest worker needs.
type WorkerConfig struct {
	// TickInterval is how often the worker starts a discovery + publish
	// pass. It is also the feed-refresh cadence while a publish pass is
	// still running. Plan says ~30s; operator can raise it.
	TickInterval time.Duration
	// PageSize caps how many change rows we drain per page.
	PageSize int
	// BatchSize caps how many eligible sessions the publish pass
	// processes per tick.
	BatchSize int
	// Concurrency bounds how many sessions the publish pass indexes in
	// parallel per tick. Zero → default (1). Each candidate in a tick's
	// bounded batch is dispatched exactly once, so raising this never
	// selects the same session twice within one tick.
	Concurrency int
	// ChunkBatchSize caps how many native map-summary chunk events the
	// level-triggered chunk reconciler upserts+tombstones per tick.
	// Zero → default (100).
	ChunkBatchSize int
	// Policy is the validated include/exclude policy.
	Policy Policy
	// Thresholds returns when a session is summarized. It is read for each
	// candidate, so a change on the Jobs and maintenance page applies to
	// the next one. Nil is DefaultSummarizePolicy.
	Thresholds func() SummarizePolicy
	// MaxRevisionBytes caps the post-decompression size of a
	// fetched Bsky Session v1 revision before it is rejected.
	// Zero = no explicit cap at the worker layer (the client has
	// its own 256 MiB transport cap).
	MaxRevisionBytes int64
	// Summarize toggles whether discovery ALSO feeds the corpus /
	// summarizer path. Off by default (shadow) — the worker only
	// observes candidates and advances the cursor, never the
	// published watermark.
	Summarize bool
	// ViewerBaseURL is the public host used to build citations for
	// published corpus events. Empty = URL field omitted.
	ViewerBaseURL string
}

// Publisher renders a validated Bsky Session v1 revision into the
// searchable corpus (repository-native events sink). Production is
// *EventPublisher (which fuses the corpus write and the ledger CAS
// into one transaction); tests stub it. When it is nil and Summarize
// is true, the worker still advances the published ledger via the
// standalone Store path (the ledger is authoritative for cursor/rewrite
// semantics) but logs that corpus rendering is not wired.
type Publisher interface {
	// PublishSession atomically writes the summary body into the corpus
	// AND compares-and-swaps the session's ledger against the revision
	// it was fetched for, in one transaction. It returns Stale when the
	// fetched revision is no longer the current candidate (nothing was
	// written).
	PublishSession(ctx context.Context, doc *sessionDocument, id Identity, viewerBaseURL, revision string, rewriteEpoch int64, contentDigest, visibilityScope string, summary SummaryResult) (PublishResult, error)
	// Tombstone removes a session's corpus event from the searchable
	// surface: the final :session event AND every native range event.
	// Idempotent; called when a head is excluded or deleted.
	Tombstone(ctx context.Context, id Identity) error
	// ReconcileChunkEvents is the bounded (limit per tick),
	// level-triggered publication of native map-summary chunks as
	// independently searchable range events, tombstoning obsolete
	// native range events after the desired ones are upserted. Pass is
	// derived purely from durable state, so a crash mid-pass
	// re-selects the remainder next tick.
	ReconcileChunkEvents(ctx context.Context, limit int) (ChunkReconcileResult, error)
}

// Worker is the leader-only session-ingest loop.
type Worker struct {
	Cfg   WorkerConfig
	Store *Store
	// Exactly one session source is set: Feed polls a change feed; Push
	// serves sessions the push endpoint queued.
	Feed      Feed
	Push      *PushStore
	Summarize Summarizer
	// TitleGenerator compresses the final reduce narrative into a display title.
	// It never receives the source transcript.
	TitleGenerator sessiontitle.Generator
	Publish        Publisher
	Logger         *slog.Logger
	Metrics        *Metrics
	Control        SummaryControl
}

func (w *Worker) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Run drives the periodic tick loop until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.Cfg.TickInterval)
	defer ticker.Stop()
	if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
		w.log().Warn("sessioningest: boot tick failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
				w.log().Warn("sessioningest: tick failed", "err", err)
			}
		}
	}
}

// Tick runs one publish pass while keeping discovery on its poll cadence.
//
//  1. Drain the change feed from the durable cursor in ordered pages,
//     durably upserting candidates + advancing the cursor per batch,
//     applying policy admission reconciliation (tombstone newly-
//     excluded, re-enable re-included) in the same tx. Continue doing
//     so on TickInterval while the publish pass remains in flight.
//  2. If the policy hash changed, apply the policy to the candidates
//     already here, so exclusions tombstone and inclusions re-enable.
//     The cursor does not move.
//  3. Find genuinely-pending candidates (fair, oldest-first) and
//     publish eligible revisions canonically when Summarize is on.
//
// Discovery always runs; publishing is gated on Summarize. Running
// with Summarize off never consumes the future published watermark.
func (w *Worker) Tick(ctx context.Context) error {
	if (w.Feed == nil) == (w.Push == nil) {
		return errors.New("sessioningest: exactly one of feed or push must be configured")
	}
	if err := w.Cfg.Policy.Validate(); err != nil {
		return err
	}
	if w.Cfg.Summarize {
		if w.Summarize == nil {
			return errors.New("sessioningest: summarize mode requires map/reduce summarizer")
		}
		if w.Publish == nil {
			return errors.New("sessioningest: summarize mode requires corpus publisher")
		}
	}

	policyHash := w.Cfg.Policy.Hash()
	if err := w.discoveryPass(ctx, policyHash); err != nil {
		if w.Metrics != nil {
			w.Metrics.FeedErrorsTotal.Inc()
		}
		return err
	}

	if w.Cfg.Summarize && w.summaryEnabled(ctx) {
		if err := w.scheduleWithDiscovery(ctx, policyHash); err != nil {
			return err
		}
		// Level-triggered chunk-event publication: publish every
		// desired native map-summary chunk as a searchable range event
		// (bounded to ~100 events per tick), then tombstone native
		// range events that no longer match current state. Selection is
		// state-derived every tick, so a mid-tick crash or stale CAS
		// simply re-selects the remainder on the next tick.
		if w.summaryEnabled(ctx) && w.Publish != nil {
			if _, err := w.Publish.ReconcileChunkEvents(ctx, w.Cfg.ChunkBatchSize); err != nil {
				return err
			}
		}
	}

	return nil
}

// discoveryPass advances the durable feed cursor and reconciles corpus
// tombstones. Keeping it separate from schedule lets a long-running batch of
// serial model calls continue polling the feed without starting a second
// summarizer or a second outer worker tick.
func (w *Worker) discoveryPass(ctx context.Context, policyHash string) error {
	// Pushed sessions are queued by the push transaction itself; only a
	// feed has a cursor to drain.
	var (
		cursor   Cursor
		position int64
	)
	if w.Feed != nil {
		var err error
		if cursor, err = w.Store.GetCursor(ctx); err != nil {
			return err
		}
		if position, err = w.drain(ctx, cursor.ChangeSeq); err != nil {
			return err
		}
	}
	// The discovery heartbeat reports a successful change-feed drain. Record
	// it before downstream reconciliation, which may fail or observe a caller
	// cancellation after the feed has already been durably consumed.
	if w.Metrics != nil {
		w.Metrics.LastDiscoverySuccess.Set(float64(time.Now().Unix()))
	}
	if w.Feed != nil && cursor.PolicyHash != policyHash {
		if err := w.applyPolicy(ctx, cursor.PolicyHash, policyHash, position); err != nil {
			return err
		}
	}
	if repaired, err := w.Store.ReconcileCanonicalEventScopes(ctx); err != nil {
		return err
	} else if repaired > 0 {
		w.log().Warn("sessioningest: repaired canonical session scope drift", "events", repaired)
	}

	// Durable corpus-tombstone reconciliation: any identity still
	// marked pending (a head excluded/soft-deleted in a prior batch
	// whose events-corpus removal did not complete before a restart)
	// is re-tombstoned here. This runs every discovery pass, so cursor
	// advancement can never lose a delete/exclusion.
	if w.Publish != nil {
		if err := w.reconcileCorpusTombstones(ctx); err != nil {
			return err
		}
	}
	if w.Metrics != nil {
		counts, err := w.Store.CountPendingCandidates(ctx)
		if err != nil {
			return err
		}
		w.Metrics.PendingSessions.Set(float64(counts.Pending))
		w.Metrics.BackoffSessions.Set(float64(counts.BackingOff))
		w.Metrics.QuarantinedSessions.Set(float64(counts.Quarantined))
	}
	return nil
}

// scheduleWithDiscovery keeps feed discovery independent from serial
// summarization latency. The schedule pass still has exactly one producer and
// preserves its oldest-first bounded batch; only the cheap metadata drain is
// repeated while that batch is running. This bounds cursor staleness to the
// configured poll interval even when one model call or a large backlog makes
// the outer Tick take much longer than that interval.
func (w *Worker) scheduleWithDiscovery(ctx context.Context, policyHash string) error {
	pollCtx, cancel := context.WithCancel(ctx)
	pollDone := make(chan error, 1)
	go func() {
		pollDone <- w.pollDiscovery(pollCtx, policyHash)
	}()

	scheduleErr := w.schedule(ctx)
	cancel()
	pollErr := <-pollDone
	if scheduleErr != nil {
		return scheduleErr
	}
	return pollErr
}

// pollDiscovery runs only for the lifetime of one schedule pass. Transient
// feed failures are retried on the next interval; a later success clears the
// pending error. If the schedule finishes while the latest poll is still
// failing, surface that failure to the outer runner after the poll goroutine
// has shut down cleanly.
func (w *Worker) pollDiscovery(ctx context.Context, policyHash string) error {
	interval := w.Cfg.TickInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
			if err := w.discoveryPass(ctx, policyHash); err != nil {
				if ctx.Err() != nil {
					return lastErr
				}
				lastErr = err
				w.log().Warn("sessioningest: feed refresh during schedule failed", "err", err)
				if w.Metrics != nil {
					w.Metrics.FeedErrorsTotal.Inc()
				}
				continue
			}
			lastErr = nil
		}
	}
}

// reconcileCorpusTombstones drains the durable pending-tombstone marker
// (set transactionally with the cursor advance on exclusion/soft-delete)
// and drives the events-corpus Tombstone for each. Only after a
// successful removal is the marker cleared, so a crash mid-way leaves
// the marker for the next tick to retry.
func (w *Worker) reconcileCorpusTombstones(ctx context.Context) error {
	pending, err := w.Store.ListPendingCorpusTombstones(ctx)
	if err != nil {
		return err
	}
	for _, id := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.Publish != nil {
			if terr := w.Publish.Tombstone(ctx, id); terr != nil {
				w.log().Warn("sessioningest: corpus tombstone failed (will retry)",
					"session", id.SessionID, "err", terr)
				// Leave the marker set so the next tick retries.
				continue
			}
		}
		if err := w.Store.ClearCorpusTombstone(ctx, id); err != nil {
			w.log().Warn("sessioningest: clear corpus tombstone marker failed",
				"session", id.SessionID, "err", err)
		}
	}
	return nil
}

func (w *Worker) admitChange(ch SessionChange) bool {
	return w.Cfg.Policy.AdmitChange(ch.SessionID, ch.OwnerKind, ch.Role, ch.Client, ch.OriginApp, ch.Owner, ch.Deleted)
}

func (w *Worker) changePolicy(ch SessionChange) ChangePolicy {
	return ChangePolicy{
		Admitted:        w.admitChange(ch),
		VisibilityScope: w.Cfg.Policy.VisibilityScope(ch.Owner),
	}
}

// policyBatchSize is how many candidates one ReapplyPolicy batch checks.
const policyBatchSize = 500

// applyPolicy applies a policy that is not the one the candidates were last
// checked against. That never moves the cursor: the candidates already hold
// every session's descriptor. The feed is drained from position again
// between batches, so new sessions keep arriving however many sessions the
// policy re-decides, and the policy is recorded as applied only once the
// last batch is through, so an interrupted pass starts over and finds its
// finished work unchanged.
func (w *Worker) applyPolicy(ctx context.Context, oldHash, policyHash string, position int64) error {
	w.log().Info("sessioningest: applying the eligibility policy to the sessions already seen",
		"old", oldHash, "new", policyHash)
	var (
		after            Identity
		checked, changed int
	)
	for {
		batch, err := w.Store.ReapplyPolicy(ctx, after, policyBatchSize, w.changePolicy)
		if err != nil {
			return err
		}
		checked += batch.Checked
		changed += batch.Changed
		if batch.Done {
			break
		}
		after = batch.Next
		if position, err = w.drain(ctx, position); err != nil {
			return err
		}
	}
	if err := w.Store.RecordAppliedPolicy(ctx, policyHash); err != nil {
		return err
	}
	w.log().Info("sessioningest: eligibility policy applied", "checked", checked, "changed", changed)
	return nil
}

// drain walks the change feed from the given position in ordered pages,
// durably persisting each batch (candidates + cursor advance + admission
// reconciliation in one tx) until the feed returns an empty page, and
// returns the position it reached. The cursor is advanced from the feed's
// explicit `next_cursor` (never re-derived from local max values), so a
// regressing or inconsistent page is detected by the connector and aborts
// the tick.
func (w *Worker) drain(ctx context.Context, after int64) (int64, error) {
	pageSize := w.Cfg.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	for {
		page, err := w.Feed.ListSessionChanges(ctx, after, pageSize)
		if err != nil {
			return after, fmt.Errorf("sessioningest: list changes (after=%d): %w", after, err)
		}
		// Validate the page's explicit next_cursor is consistent with
		// the served rows before persisting anything (a malformed or
		// regressing cursor must abort the tick, never advance the
		// durable position).
		if err := validateFeedPage(page, after); err != nil {
			return after, fmt.Errorf("sessioningest: page validation: %w", err)
		}
		if len(page.Changes) == 0 {
			// Converged on an empty page; the cursor already reflects
			// the requested position.
			return after, nil
		}
		_, skipped, err := w.Store.UpsertChangesScopedAt(ctx, page.Changes, after, page.NextCursor, w.changePolicy)
		if err != nil {
			return after, err
		}
		for _, sk := range skipped {
			reason := skipReason(sk.Err)
			w.log().Warn("sessioningest: feed change skipped (deterministic per-change refusal)",
				"session", sk.SessionID, "owner", sk.Owner, "client", sk.Client, "reason", reason, "err", sk.Err)
			if w.Metrics != nil {
				w.Metrics.FeedChangesSkippedTotal.WithLabelValues(reason).Inc()
			}
		}
		// The events-corpus tombstone for newly-excluded / soft-deleted
		// heads is NOT driven here: the exclusion tx sets a durable
		// corpus_tombstone_pending marker, and reconcileCorpusTombstones
		// (run once per tick) drains it after commit. A crash in between
		// leaves the marker for the next tick — cursor advancement can
		// never lose a delete/exclusion.
		if w.Metrics != nil {
			w.Metrics.ChangesTotal.Add(float64(len(page.Changes)))
		}
		after = page.NextCursor
	}
}

// validateFeedPage enforces the explicit next_cursor + monotonicity
// contract on a feed page before its rows are persisted. An empty page
// is valid only when the feed echoes a non-regressing cursor; a
// non-empty page must carry a cursor >= every served change_seq so a
// later drain cannot silently re-deliver rows.
func validateFeedPage(p *ChangesPage, after int64) error {
	if p == nil {
		return errors.New("nil changes page")
	}
	if len(p.Changes) == 0 {
		if p.NextCursor < after {
			return fmt.Errorf("page next_cursor %d regresses below requested after %d", p.NextCursor, after)
		}
		return nil
	}
	if p.NextCursor < after {
		return fmt.Errorf("page next_cursor %d regresses below requested after %d", p.NextCursor, after)
	}
	for _, c := range p.Changes {
		if c.ChangeSeq > p.NextCursor {
			return fmt.Errorf("page next_cursor %d trails served change_seq %d", p.NextCursor, c.ChangeSeq)
		}
	}
	return nil
}

// schedule loads genuinely-pending candidates (observed revision not
// yet published) and publishes each eligible revision canonically.
// The bounded batch is drained through a fixed-size worker pool: every
// candidate in the batch is dispatched exactly once (no queue, cross-
// worker lease, or re-selection within the tick), Concurrency bounds
// how many sessions are in flight at once, and cancellation aborts
// dispatch while in-flight workers finish their current session.
func (w *Worker) schedule(ctx context.Context) error {
	cands, err := w.Store.FindPendingCandidates(ctx, w.Cfg.BatchSize)
	if errors.Is(err, ErrNoCandidates) {
		// An empty selection does not mean the backlog is empty: revisions
		// backing off after a failure are skipped. The discovery pass owns the
		// pending gauges.
		return nil
	}
	if err != nil {
		return err
	}
	concurrency := w.Cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(cands) {
		concurrency = len(cands)
	}

	jobs := make(chan Candidate)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				// Stop starting NEW sessions once the tick is
				// cancelled; drain the queue so shutdown is clean.
				if ctx.Err() != nil {
					continue
				}
				if !w.summaryEnabled(ctx) {
					continue
				}
				w.indexCandidate(ctx, c)
			}
		}()
	}

	var dispatchErr error
dispatch:
	for _, c := range cands {
		select {
		case jobs <- c:
		case <-ctx.Done():
			dispatchErr = ctx.Err()
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	return dispatchErr
}

// summaryEnabled fails closed: a control read failure never begins paid work.
func (w *Worker) summaryEnabled(ctx context.Context) bool {
	control := w.Control
	if control.Store == nil {
		control = NewSummaryControl(w.Store, nil)
	}
	state, err := control.State(ctx)
	if err != nil {
		w.log().Warn("sessioningest: summary control unavailable; skipping paid work", "err", err)
		return false
	}
	return state.EffectiveEnabled
}

// indexCandidate applies pending bookkeeping and the canonical publish
// for one candidate. It runs on a pool worker; the durable batch is
// distinct per canonical identity, so no two workers ever select the
// same session within one tick, and PublishSession's ledger CAS guards
// against concurrent replaces across ticks.
func (w *Worker) indexCandidate(ctx context.Context, c Candidate) {
	if !w.schedulePredicate(c) {
		if _, err := w.Store.MarkPending(ctx, c.Identity, c.Revision); err != nil {
			w.log().Warn("sessioningest: mark pending failed", "session", c.SessionID, "err", err)
		}
		return
	}
	if err := w.indexOne(ctx, c); err != nil {
		// A cancelled WORKER context is not a session failure: the next
		// tick re-selects the still-pending revision. Don't noise the
		// error counter on shutdown. A model-request deadline hit while
		// the worker context is alive stays a counted index error.
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			w.log().Info("sessioningest: index interrupted by shutdown", "session", c.SessionID)
			return
		}
		if w.Metrics != nil {
			w.Metrics.IndexErrorsTotal.Inc()
		}
		permanent := errors.Is(err, ErrRevisionTooLarge) || errors.Is(err, sessiondecode.ErrStructural) ||
			errors.Is(err, ErrNoRecords)
		failure, recErr := w.Store.RecordIndexFailure(ctx, c.Identity, c.Revision, err, permanent)
		if recErr != nil {
			w.log().Warn("sessioningest: index failed; recording failure state failed",
				"session", c.SessionID, "revision", c.Revision, "err", err, "record_err", recErr)
			return
		}
		switch {
		case failure.Quarantined:
			w.log().Warn("sessioningest: index failed; revision parked",
				"session", c.SessionID, "revision", c.Revision, "attempt", failure.Count, "err", err)
		case failure.Recorded:
			w.log().Warn("sessioningest: index failed; backing off",
				"session", c.SessionID, "revision", c.Revision, "attempt", failure.Count,
				"retry_at", failure.RetryAt, "err", err)
		default:
			w.log().Warn("sessioningest: index failed", "session", c.SessionID, "err", err)
		}
	}
}

// schedulePredicate implements the scheduling expression:
//
//	revision changed
//	AND complete
//	AND (never summarized
//	     OR rewrite_epoch changed
//	     OR added user turns / records / content bytes crossed a
//	        threshold
//	     OR pending age exceeded maxPendingFor)
//	AND (quietFor elapsed OR pending age exceeded maxPendingFor)
//
// hdl is the current candidate state. "published" watermark (not
// "last indexed") is on the candidate row. We schedule a revision when
// the opaque revision token changed (replacing canonically even on a
// rewind to a shorter revision), the session is complete, and either
// new content crossed a threshold or the session has been pending past
// MaxPendingFor, AND either the session has been quiet long enough or
// it's been pending too long. A never-summarized (never-published)
// session is eligible on its first quiet pass: added-content math is
// relative to published counters, and when published_revision is empty
// the "never summarized" disjunct is true, so any complete un-published
// head that has been quiet long enough schedules.
func (w *Worker) schedulePredicate(hdl Candidate) bool {
	if !hdl.Complete {
		return false
	}
	revisionChanged := hdl.Revision != "" && hdl.Revision != hdl.PublishedRevision
	if !revisionChanged {
		return false
	}
	neverPublished := hdl.PublishedRevision == ""
	thresholds := DefaultSummarizePolicy()
	if w.Cfg.Thresholds != nil {
		thresholds = w.Cfg.Thresholds()
	}

	addedRecords := hdl.RecordCount - hdl.PublishedRecordCount
	addedTurns := hdl.UserTurnCount - hdl.PublishedUserTurns
	addedBytes := hdl.ContentBytes - hdl.PublishedContentBytes

	newContent := addedRecords >= thresholds.MinAddedRecords
	if hdl.UserTurnCount > 0 && addedTurns >= thresholds.MinAddedUserTurns {
		newContent = true
	}
	if addedBytes >= thresholds.MinAddedContentBytes {
		newContent = true
	}

	rewriteChanged := hdl.RewriteEpoch != hdl.PublishedRewrite

	// Pending age: when first_pending_at set, how long we've been
	// waiting without an eligible publish.
	now := time.Now()
	agePending := time.Duration(0)
	if hdl.FirstPendingAt != nil {
		agePending = now.Sub(*hdl.FirstPendingAt)
	}
	pendingExpired := agePending >= thresholds.MaxPendingFor

	quiet := now.Sub(hdl.UpdatedAt) >= thresholds.QuietFor

	nextTick := neverPublished || rewriteChanged || newContent || pendingExpired
	if !nextTick {
		return false
	}
	return quiet || pendingExpired
}

// indexOne fetches the pinned sanitized revision from the feed,
// validates it (Bsky Session v1 schema/identity/revision + byte cap),
// renders it through the summarizer into the corpus AND advances the
// published ledger in ONE atomic transaction (via PublishSession). This
// implements the replacement + tombstone contract:
//
//   - It fetches ONLY the sanitized revision (the sole content
//     surface; never raw audit records).
//   - PublishSession compares-and-swaps the fetched revision against
//     the session's current candidate inside the same events tx that
//     writes the corpus: an older fetch can never replace the visible
//     current summary, even transiently.
//   - The same tx supersedes any prior published revision (append/
//     rewind/rewrite all advance the head by opaque revision token) and
//     tombstones prior revisions in the ledger.
//
// The SearchableSummarizer (below) turns the validated document into
// repository-native summary content — never a raw transcript — and
// feeds it to PublishSession.
func (w *Worker) indexOne(ctx context.Context, hdl Candidate) error {
	if hdl.Deleted || hdl.ExcludedAt != nil {
		return nil // tombstoned; find-pending excludes these already
	}
	if hdl.Revision != "" && hdl.PublishedRevision == hdl.Revision {
		return nil // already published this exact revision
	}

	doc, sourceDigest, bodyBytes, err := w.loadDocument(ctx, hdl)
	if err != nil {
		return err
	}
	// A revision with no records has nothing to summarize. Sent on, it cost
	// a model call on empty input and then failed to save its result, on
	// every retry without end.
	if doc.RecordCount == 0 {
		return fmt.Errorf("revision %s for %s: %w", hdl.Revision, hdl.SessionID, ErrNoRecords)
	}
	if doc.WarningRecords > 0 {
		w.log().Warn("sessioningest: unsupported content replaced with notices", "session", hdl.SessionID,
			"revision", hdl.Revision, "warning_records", doc.WarningRecords)
	}

	// Render repository-native summary content from the validated
	// document (indexOne never publishes raw transcript) and fuse the
	// corpus write with the ledger CAS in one tx via publish().
	result, err := w.publish(ctx, doc, hdl, sourceDigest)
	if err != nil {
		return err
	}
	if result.Stale {
		w.log().Info("sessioningest: publish CAS rejected (stale fetch)",
			"session", hdl.SessionID, "revision", hdl.Revision)
		return nil
	}
	if result.Published {
		if w.Metrics != nil {
			w.Metrics.RevisionsPublished.Inc()
			if result.SummaryReused {
				w.Metrics.SummaryReusesTotal.Inc()
			}
		}
		w.log().Info("sessioningest: published revision",
			"session", hdl.SessionID, "revision", hdl.Revision,
			"rewrite_epoch", hdl.RewriteEpoch,
			"records", doc.RecordCount, "bytes", bodyBytes,
			"summary_reused", result.SummaryReused, "reduce_entries", result.ReduceEntries)
	}
	return nil
}

// loadDocument yields the validated document for a candidate's revision, its
// content digest, and its stored size: fetched from the feed and decoded, or
// read from a pushed session's materialization.
func (w *Worker) loadDocument(ctx context.Context, hdl Candidate) (*sessionDocument, string, int64, error) {
	if w.Push != nil {
		doc, sourceDigest, size, err := w.Push.loadDocument(ctx, hdl)
		if err != nil {
			return nil, "", 0, fmt.Errorf("load pushed revision %s for %s: %w", hdl.Revision, hdl.SessionID, err)
		}
		return doc, sourceDigest, size, nil
	}
	// The feed's content_bytes counts record lines without the envelope, so
	// exceeding the cap here guarantees the fetched document would too. Fail
	// before spending a cap-sized download on it.
	if w.Cfg.MaxRevisionBytes > 0 && hdl.ContentBytes > w.Cfg.MaxRevisionBytes {
		return nil, "", 0, fmt.Errorf("revision %s for %s: %w: feed reports %d content bytes, cap %d",
			hdl.Revision, hdl.SessionID, ErrRevisionTooLarge, hdl.ContentBytes, w.Cfg.MaxRevisionBytes)
	}
	body, err := w.Feed.GetSessionRevisionBytes(ctx, hdl.SessionID, hdl.Revision, w.Cfg.MaxRevisionBytes)
	if err != nil {
		return nil, "", 0, fmt.Errorf("fetch revision %s for %s: %w", hdl.Revision, hdl.SessionID, err)
	}
	if w.Metrics != nil {
		w.Metrics.RevisionsFetched.Inc()
	}
	doc, err := parseSessionDocument(body, hdl.Identity, hdl.Revision, w.Cfg.MaxRevisionBytes)
	if err != nil {
		return nil, "", 0, fmt.Errorf("validate revision %s for %s: %w", hdl.Revision, hdl.SessionID, err)
	}
	return doc, digest(body), int64(len(body)), nil
}

// publish drives the atomic corpus + ledger write through the configured
// Publisher. When no publish sink is wired (pure shadow), it falls back
// to the standalone Store CAS so the ledger semantics stay
// authoritative (the corpus is simply not rendered).
func (w *Worker) publish(ctx context.Context, doc *sessionDocument, hdl Candidate, contentDigest string) (PublishResult, error) {
	summary, err := w.Summarize.Summarize(ctx, doc, hdl.Identity, hdl.Revision, hdl.RewriteEpoch)
	if err != nil {
		return PublishResult{}, fmt.Errorf("sessioningest: summarize %s: %w", hdl.SessionID, err)
	}
	if strings.TrimSpace(summary.Narrative) == "" {
		return PublishResult{}, fmt.Errorf("sessioningest: summarizer returned empty narrative for %s", hdl.SessionID)
	}
	if summary.Title == "" && w.TitleGenerator != nil {
		title, titleErr := sessiontitle.GenerateFromSummary(ctx, w.TitleGenerator, summary.Narrative)
		if titleErr != nil {
			w.log().Warn("sessioningest: live title generation failed",
				"session", hdl.SessionID, "err", titleErr)
		} else {
			summary.Title = title.Title
			summary.TitleModel = title.ModelID
		}
	}
	result, err := w.Publish.PublishSession(ctx, doc, hdl.Identity, w.Cfg.ViewerBaseURL,
		hdl.Revision, hdl.RewriteEpoch, contentDigest, hdl.VisibilityScope, summary)
	result.SummaryReused = summary.Reused
	result.ReduceEntries = summary.ReduceEntries
	return result, err
}

func digest(body []byte) string {
	return "sha256:" + shaHex(body)
}

func shaHex(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
}
