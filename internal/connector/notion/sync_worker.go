package notion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/worker"
)

// SyncWorker runs authoritative managed-root reconciliation plus a
// best-effort workspace discovery audit.
//
//	Loop 1 (every SearchInterval, default 30 min): incremental
//	search by last_edited_time DESC. Results update the discovery
//	catalog only; Search never authorizes ingestion.
//	Watermark advances to (tick_start - WatermarkOverlap) on
//	success — overlap covers Notion's eventual-consistency
//	window without bloating the catch-up window. The 10-minute
//	default is informed by Onyx's experience with how long
//	Notion's index can lag.
//
//	Loop 2 (every ReconcileInterval, default 24h): full graph
//	reconcile from notion_roots. Only fully walked roots advance
//	state; their missing descendants are tombstoned.
//
// Operator backfill requests a managed-root replay. It never turns
// global Search results into corpus events.
//
// Leader-only: the source's Run starts it, and only a leader term runs
// sources, so two replicas don't double-ingest. (The
// idempotent NoChange short-circuit on events.Upsert makes
// double-ingest cheap, but skipping it entirely is cheaper.)
type SyncWorker struct {
	pool   *pgxpool.Pool
	walker *Walker
	roots  *RootsStore
	state  *PageStateStore
	logger *slog.Logger

	SearchInterval          time.Duration
	ReconcileInterval       time.Duration
	BackfillInterval        time.Duration
	WatermarkOverlap        time.Duration
	CredentialRetryInterval time.Duration
	RootConcurrency         int
	// Observer, when set, is told how each search and walk went.
	Observer    worker.Observer
	reconcileMu sync.Mutex
}

// observe tells the Observer how a pass went; a pass cut short by shutdown
// says nothing.
func (w *SyncWorker) observe(err error) {
	switch {
	case w.Observer == nil || errors.Is(err, context.Canceled):
	case err != nil:
		w.Observer.Failed()
	default:
		w.Observer.Succeeded()
	}
}

type rootWalkTask struct {
	index int
	root  Root
}

type rootWalkResult struct {
	task     rootWalkTask
	report   WalkReport
	err      error
	duration time.Duration
}

type rootWalkFunc func(context.Context, RootRef) (WalkReport, error)

type partialReconcileError struct {
	failed int
	total  int
}

func (e *partialReconcileError) Error() string {
	return fmt.Sprintf("%d of %d notion roots failed", e.failed, e.total)
}

func isPartialReconcileError(err error) bool {
	var partial *partialReconcileError
	return errors.As(err, &partial)
}

func reconcileAttemptCompletesBackfill(err error) bool {
	return err == nil || isPartialReconcileError(err)
}

func reconcileRootFailureResult(failed, total int) (string, error) {
	if failed >= total {
		return "failed", fmt.Errorf("all %d notion roots failed", total)
	}
	return "partial", &partialReconcileError{failed: failed, total: total}
}

// NewSyncWorker constructs a SyncWorker. roots may be nil; when
// nil, managed reconciliation does nothing — discovery-only mode. Defaults
// for intervals: 30m search, 24h reconcile, 1s backfill, 10m overlap.
func NewSyncWorker(pool *pgxpool.Pool, walker *Walker, roots *RootsStore, logger *slog.Logger) *SyncWorker {
	if logger == nil {
		logger = slog.Default()
	}
	var hider NotionHider
	if walker != nil {
		hider, _ = walker.Ingest.(NotionHider)
	}
	return &SyncWorker{
		pool:                    pool,
		walker:                  walker,
		roots:                   roots,
		state:                   NewPageStateStore(pool, hider),
		logger:                  logger,
		SearchInterval:          30 * time.Minute,
		ReconcileInterval:       24 * time.Hour,
		BackfillInterval:        time.Second,
		WatermarkOverlap:        10 * time.Minute,
		CredentialRetryInterval: time.Minute,
		RootConcurrency:         3,
	}
}

// Run blocks until ctx is cancelled, driving both loops on their
// configured cadences. Each loop fires on its own ticker.
//
// Boot tick semantics: both loops fire ONE tick at startup
// (immediately) so a fresh pod doesn't wait 30 min / 24 h before
// catching up. Subsequent ticks fire on the configured cadence.
func (w *SyncWorker) Run(ctx context.Context) error {
	w.logger.Info("notion sync worker starting",
		"search_interval", w.SearchInterval,
		"reconcile_interval", w.ReconcileInterval,
		"backfill_interval", w.BackfillInterval,
		"overlap", w.WatermarkOverlap,
		"root_concurrency", w.RootConcurrency)

	if err := w.waitForCredentialIdentity(ctx); err != nil {
		return fmt.Errorf("notion credential identity: %w", err)
	}

	// Empty notion_roots: warn so the operator notices
	// reconcile is effectively disabled.
	if w.roots != nil {
		if rs, err := w.roots.List(ctx); err == nil && len(rs) == 0 {
			w.logger.Warn("notion: notion_roots is empty; discovery will continue but nothing will be ingested until roots are added at /admin/connectors/notion")
		}
	}

	searchTicker := time.NewTicker(w.SearchInterval)
	defer searchTicker.Stop()
	reconcileTicker := time.NewTicker(w.ReconcileInterval)
	defer reconcileTicker.Stop()
	backfillTicker := time.NewTicker(w.BackfillInterval)
	defer backfillTicker.Stop()

	// Boot discovery is synchronous. Reconcile runs in at most one joined
	// goroutine so long walks do not block discovery and cannot overlap.
	w.runSearchSafe(ctx)
	var reconcileWG sync.WaitGroup
	reconcileRunning := false
	var reconcileStateMu sync.Mutex
	startReconcile := func(backfillRequestedAt *time.Time) bool {
		reconcileStateMu.Lock()
		if reconcileRunning {
			reconcileStateMu.Unlock()
			return false
		}
		reconcileRunning = true
		reconcileWG.Add(1)
		reconcileStateMu.Unlock()
		go func() {
			defer reconcileWG.Done()
			defer func() {
				reconcileStateMu.Lock()
				reconcileRunning = false
				reconcileStateMu.Unlock()
			}()
			err := w.runReconcile(ctx)
			if errors.Is(err, context.Canceled) {
				return
			}
			w.observe(err)
			if !reconcileAttemptCompletesBackfill(err) {
				w.logger.Error("notion reconcile loop failed", "err", err)
				return
			}
			if err != nil {
				w.logger.Warn("notion reconcile loop partial", "err", err)
			}
			if backfillRequestedAt != nil {
				if _, err := w.pool.Exec(ctx, `
					UPDATE notion_sync
					   SET backfill_requested_at = NULL, backfill_cursor = NULL
					 WHERE id = 1 AND backfill_requested_at = $1
				`, *backfillRequestedAt); err != nil {
					if ctx.Err() != nil {
						return
					}
					w.logger.Error("notion clear managed-root backfill request", "err", err)
					return
				}
				w.logger.Info("notion managed-root backfill complete")
			}
		}()
		return true
	}
	queueBackfill := func() bool {
		requestedAt, err := w.readBackfillRequest(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				w.logger.Error("notion read backfill request", "err", err)
			}
			return false
		}
		if requestedAt == nil {
			return false
		}
		return startReconcile(requestedAt)
	}
	if !queueBackfill() {
		startReconcile(nil)
	}

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("notion sync worker stopping", "reason", ctx.Err())
			reconcileWG.Wait()
			return ctx.Err()
		case <-searchTicker.C:
			w.runSearchSafe(ctx)
		case <-reconcileTicker.C:
			if !startReconcile(nil) {
				w.logger.Warn("notion reconcile tick coalesced; previous run still active")
			}
		case <-backfillTicker.C:
			queueBackfill()
		}
	}
}

// waitForCredentialIdentity fails discovery closed without permanently
// killing the long-running worker. A transient /users/me failure must not let
// Search run against a watermark established by an unknown credential, but it
// also should not require a pod restart once Notion recovers.
func (w *SyncWorker) waitForCredentialIdentity(ctx context.Context) error {
	retryInterval := w.CredentialRetryInterval
	if retryInterval <= 0 {
		retryInterval = time.Minute
	}
	for {
		if err := w.refreshCredentialIdentity(ctx); err == nil {
			return nil
		} else if ctx.Err() == nil {
			w.logger.Error("notion credential identity unavailable; discovery paused", "err", err)
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// refreshCredentialIdentity invalidates incremental discovery when the token
// changes. A replacement integration can expose old resources whose edit
// timestamps predate the existing watermark; retaining that watermark would
// make them permanently invisible to Search.
func (w *SyncWorker) refreshCredentialIdentity(ctx context.Context) error {
	self, err := w.walker.Client.GetSelf(ctx)
	if err != nil {
		return fmt.Errorf("get current integration: %w", err)
	}
	if self.ID == "" {
		return errors.New("current integration has empty id")
	}
	if _, err := w.pool.Exec(ctx, `
		UPDATE notion_sync
		   SET last_discovery_at = CASE
		           WHEN integration_id IS DISTINCT FROM $1 THEN NULL
		           ELSE last_discovery_at
		       END,
		       integration_id = $1
		 WHERE id = 1
	`, self.ID); err != nil {
		return fmt.Errorf("persist integration id: %w", err)
	}
	w.logger.Info("notion credential identity loaded", "integration_id", self.ID)
	return nil
}

// runSearchSafe wraps runSearch with logging.
// Errors don't propagate — a transient API failure shouldn't kill
// the worker; the next tick will retry.
func (w *SyncWorker) runSearchSafe(ctx context.Context) {
	err := w.runSearch(ctx)
	w.observe(err)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Error("notion search loop failed", "err", err)
	}
}

// runReconcileSafe wraps runReconcile with logging.
func (w *SyncWorker) readBackfillRequest(ctx context.Context) (*time.Time, error) {
	var requestedAt *time.Time
	if err := w.pool.QueryRow(ctx, `
		SELECT backfill_requested_at
		  FROM notion_sync WHERE id = 1
	`).Scan(&requestedAt); err != nil {
		return nil, fmt.Errorf("read backfill state: %w", err)
	}
	return requestedAt, nil
}

// runSearch executes one discovery tick and never ingests events.
func (w *SyncWorker) runSearch(ctx context.Context) error {
	tickStart := time.Now().UTC()
	runID, err := w.startRun(ctx, "discovery", tickStart)
	if err != nil {
		return err
	}
	discovered := 0
	runFinished := false
	finish := func(finishCtx context.Context, status string, attempted, failed int, runErr error) error {
		err := w.finishRun(finishCtx, runID, status, attempted, failed, runErr)
		if err == nil {
			runFinished = true
		}
		return err
	}
	defer func() {
		if runFinished || ctx.Err() == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = finish(cleanupCtx, "failed", discovered, 1, ctx.Err())
	}()
	fail := func(runErr error) error {
		_ = finish(ctx, "failed", discovered, 1, runErr)
		return runErr
	}
	cutoff, err := w.readDiscoveryWatermark(ctx)
	if err != nil {
		return fail(fmt.Errorf("read watermark: %w", err))
	}
	w.logger.Info("notion discovery tick: starting",
		"cutoff", cutoff.Format(time.RFC3339),
		"now", tickStart.Format(time.RFC3339))

	cursor := ""
	for {
		batch, err := w.walker.DiscoverDataSourcesPage(ctx, cutoff, cursor)
		if err != nil {
			return fail(fmt.Errorf("discover data sources: %w", err))
		}
		if err := w.storeDiscoveries(ctx, runID, tickStart, batch.Resources); err != nil {
			return fail(err)
		}
		discovered += len(batch.Resources)
		if batch.Done {
			break
		}
		cursor = batch.NextCursor
	}
	newWatermark := tickStart.Add(-w.WatermarkOverlap)
	if err := w.advanceDiscoveryWatermark(ctx, newWatermark); err != nil {
		return fail(fmt.Errorf("advance watermark: %w", err))
	}
	if err := finish(ctx, "complete", discovered, 0, nil); err != nil {
		return err
	}
	w.logger.Info("notion discovery tick: done",
		"discovered", discovered,
		"new_watermark", newWatermark.Format(time.RFC3339),
		"duration", time.Since(tickStart))
	return nil
}

// runReconcile executes one Loop-2 tick. Walks every root in
// notion_roots; on success, stamps last_walked_at on each
// root and advances the global last_reconcile_at.
func (w *SyncWorker) runReconcile(ctx context.Context) error {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.roots == nil {
		return nil
	}
	tickStart := time.Now().UTC()
	runID, err := w.startRun(ctx, "reconcile", tickStart)
	if err != nil {
		return err
	}
	totalCount := 0
	failedRoots := 0
	runFinished := false
	finish := func(finishCtx context.Context, status string, attempted, failed int, runErr error) error {
		err := w.finishRun(finishCtx, runID, status, attempted, failed, runErr)
		if err == nil {
			runFinished = true
		}
		return err
	}
	defer func() {
		if runFinished || ctx.Err() == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = finish(cleanupCtx, "failed", totalCount, max(failedRoots, 1), ctx.Err())
	}()
	roots, err := w.roots.List(ctx)
	if err != nil {
		_ = finish(ctx, "failed", 0, 1, err)
		return fmt.Errorf("list roots: %w", err)
	}
	if len(roots) == 0 {
		w.logger.Info("notion reconcile tick: no roots configured; skipping")
		_ = finish(ctx, "complete", 0, 0, nil)
		return nil
	}
	w.logger.Info("notion reconcile tick: starting", "roots", len(roots))

	createdCount := 0
	updatedCount := 0
	unchangedCount := 0
	walked := make(map[RootRef][]PageSnapshot, len(roots))
	canceled := false
	results := walkRootsConcurrently(ctx, roots, w.RootConcurrency, w.logger, w.walker.WalkRootReport)
	for result := range results {
		r := result.task.root
		rootIndex := result.task.index
		ref := RootRef{Type: r.ObjectType, ObjectID: r.ObjectID}
		if result.err != nil {
			if ctx.Err() != nil {
				canceled = true
				continue
			}
			failedRoots++
			// Per-root failure is logged but doesn't abort the
			// whole reconcile — other roots may still walk
			// cleanly. The next 24h tick retries.
			w.logger.Error("notion reconcile: root failed",
				"root", rootIndex+1, "roots", len(roots),
				"object_type", r.ObjectType, "object_id", r.ObjectID,
				"label", r.Label, "duration", result.duration, "err", result.err)
			if markErr := w.roots.MarkFailed(ctx, ref, result.err); markErr != nil {
				w.logger.Warn("notion reconcile: mark failed failed", "err", markErr)
			}
			continue
		}
		report := result.report
		totalCount += report.Processed
		createdCount += report.Created
		updatedCount += report.Updated
		unchangedCount += report.Unchanged
		walked[ref] = report.Pages
		w.logger.Info("notion reconcile: root complete",
			"root", rootIndex+1, "roots", len(roots),
			"object_type", r.ObjectType, "object_id", r.ObjectID, "label", r.Label,
			"processed", report.Processed, "created", report.Created,
			"updated", report.Updated, "unchanged", report.Unchanged,
			"duration", result.duration)
	}
	if canceled {
		return context.Cause(ctx)
	}
	if err := w.setRunOutcomes(ctx, runID, createdCount, updatedCount, unchangedCount); err != nil {
		_ = finish(ctx, "failed", totalCount, 1, err)
		return err
	}
	if err := w.state.ApplySuccessfulRoots(ctx, runID, walked); err != nil {
		_ = finish(ctx, "failed", totalCount, 1, err)
		return fmt.Errorf("apply managed-root state: %w", err)
	}
	for ref := range walked {
		if err := w.roots.MarkWalkedRef(ctx, ref, tickStart); err != nil {
			_ = finish(ctx, "failed", totalCount, 1, err)
			return fmt.Errorf("mark root %s:%s walked: %w", ref.Type, ref.ObjectID, err)
		}
	}
	if failedRoots > 0 {
		status, runErr := reconcileRootFailureResult(failedRoots, len(roots))
		_ = finish(ctx, status, totalCount, failedRoots, runErr)
		return runErr
	}

	if err := w.advanceReconcile(ctx, tickStart); err != nil {
		_ = finish(ctx, "failed", totalCount, 1, err)
		return fmt.Errorf("advance reconcile timestamp: %w", err)
	}
	if err := finish(ctx, "complete", totalCount, 0, nil); err != nil {
		return err
	}
	w.logger.Info("notion reconcile tick: done",
		"processed", totalCount,
		"created", createdCount,
		"updated", updatedCount,
		"unchanged", unchangedCount,
		"duration", time.Since(tickStart))
	return nil
}

// walkRootsConcurrently keeps independent root frontiers moving while every
// API request still flows through the Client's shared integration-wide rate
// limiter. The result channel is buffered for the full task count so a caller
// returning on cancellation cannot strand a worker trying to report.
func walkRootsConcurrently(
	ctx context.Context,
	roots []Root,
	concurrency int,
	logger *slog.Logger,
	walk rootWalkFunc,
) <-chan rootWalkResult {
	results := make(chan rootWalkResult, len(roots))
	if len(roots) == 0 {
		close(results)
		return results
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	concurrency = min(concurrency, len(roots))
	jobs := make(chan rootWalkTask, len(roots))
	for index, root := range roots {
		jobs <- rootWalkTask{index: index, root: root}
	}
	close(jobs)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for task := range jobs {
				startedAt := time.Now()
				logger.Info("notion reconcile: walking root",
					"root", task.index+1, "roots", len(roots),
					"object_type", task.root.ObjectType,
					"object_id", task.root.ObjectID, "label", task.root.Label)
				var report WalkReport
				var err error
				if ctx.Err() != nil {
					err = context.Cause(ctx)
				} else {
					ref := RootRef{Type: task.root.ObjectType, ObjectID: task.root.ObjectID}
					report, err = walk(ctx, ref)
				}
				results <- rootWalkResult{
					task: task, report: report, err: err,
					duration: time.Since(startedAt),
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

func (w *SyncWorker) setRunOutcomes(ctx context.Context, runID string, created, updated, unchanged int) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE notion_sync_runs
		   SET created_count = $2, updated_count = $3, unchanged_count = $4
		 WHERE run_id = $1
	`, runID, created, updated, unchanged)
	if err != nil {
		return fmt.Errorf("update notion run outcomes %s: %w", runID, err)
	}
	return nil
}

func (w *SyncWorker) startRun(ctx context.Context, runType string, startedAt time.Time) (string, error) {
	runID := uuid.NewString()
	_, err := w.pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ($1, $2, $3, 'running')
	`, runID, runType, startedAt)
	if err != nil {
		return "", fmt.Errorf("start notion %s run: %w", runType, err)
	}
	return runID, nil
}

func (w *SyncWorker) finishRun(ctx context.Context, runID, status string, attempted, failed int, runErr error) error {
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	_, err := w.pool.Exec(ctx, `
		UPDATE notion_sync_runs
		   SET completed_at = now(), status = $2, attempted = $3,
		       failed_count = $4, error = $5
		 WHERE run_id = $1
	`, runID, status, attempted, failed, message)
	if err != nil {
		return fmt.Errorf("finish notion run %s: %w", runID, err)
	}
	return nil
}

func (w *SyncWorker) storeDiscoveries(ctx context.Context, runID string, seenAt time.Time, resources []DiscoveredResource) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin notion discovery batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, resource := range resources {
		// Title is a Notion Search result's title, genuinely external
		// text the API returns — the same NUL-byte/invalid-UTF-8 hazard
		// the events sink sanitizes, but this insert bypasses
		// events.DBSink entirely (it isn't a corpus event; see the
		// table comment), so it needs its own sanitizing.
		_, err := tx.Exec(ctx, `
			INSERT INTO notion_discovered_resources
			       (object_type, object_id, parent_id, title, url, first_seen_at, last_seen_at, last_seen_run_id)
			VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
			ON CONFLICT (object_type, object_id) DO UPDATE
			   SET parent_id = EXCLUDED.parent_id,
			       title = EXCLUDED.title,
			       url = EXCLUDED.url,
			       last_seen_at = EXCLUDED.last_seen_at,
			       last_seen_run_id = EXCLUDED.last_seen_run_id
		`, resource.ObjectType, resource.ObjectID, events.SanitizeText(resource.ParentID),
			events.SanitizeText(resource.Title), events.SanitizeText(resource.URL), seenAt, runID)
		if err != nil {
			return fmt.Errorf("store notion discovery %s: %w", resource.ObjectID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit notion discovery batch: %w", err)
	}
	return nil
}

// readDiscoveryWatermark returns the cutoff from
// notion_sync. Returns zero-time if the column is NULL — the
// first sync ever runs without a lower bound (we ingest
// everything visible). Subsequent syncs read whatever the
// previous tick wrote.
func (w *SyncWorker) readDiscoveryWatermark(ctx context.Context) (time.Time, error) {
	var t *time.Time
	err := w.pool.QueryRow(ctx, `
		SELECT last_discovery_at FROM notion_sync WHERE id = 1
	`).Scan(&t)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if t == nil {
		return time.Time{}, nil
	}
	return *t, nil
}

// advanceDiscoveryWatermark stamps the discovery watermark and increments the
// run counter. Always uses UPDATE (the row is INSERT'd at
// migration time).
func (w *SyncWorker) advanceDiscoveryWatermark(ctx context.Context, at time.Time) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE notion_sync
		   SET last_discovery_at = $1,
		       discovery_runs_total = discovery_runs_total + 1
		 WHERE id = 1
	`, at)
	return err
}

// advanceReconcile stamps last_reconcile_at and increments the
// run counter.
func (w *SyncWorker) advanceReconcile(ctx context.Context, at time.Time) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE notion_sync
		   SET last_reconcile_at = $1,
		       reconcile_runs_total = reconcile_runs_total + 1
		 WHERE id = 1
	`, at)
	return err
}
