package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/worker"
)

// SyncWorker runs four handlers — sync, visibility sweep, reconcile, and
// operator backfill — all inline in a
// single goroutine's select loop (see Run). Nothing in this package runs
// concurrently with anything else in this package:
//
//	Handler 1 (every SyncInterval, default 5m): incremental sync via
//	updatedAt filter. Walks all issues updated since the watermark,
//	paginates with cursor, fetches comments nested in the same query.
//	Watermark advances to tick_start - WatermarkOverlap on full success.
//
//	Handler 2 (every VisibilityInterval, default 1h): visibility sweep.
//	Pages through all currently ingestible issue ids and diffs against
//	stored events. Rows whose external_id is no longer ingestible get
//	deleted (tombstone + Qdrant), after a per-candidate point-query
//	re-verification (see recheckCandidate). This is the only removal path
//	for public→private team moves.
//
//	Handler 3 (every ReconcileInterval, default 24h): drift catch-up.
//	Full updatedAt-ordered walk to catch anything the incremental loop
//	missed. With a real updatedAt filter this should find nothing; it's
//	the backstop.
//
// Why serialized rather than concurrent: the visibility sweep's API walk
// takes minutes, and running it concurrently with the incremental loop can
// produce a false-positive delete — an issue created or updated by a sync
// tick mid-walk is missed
// by the sweep's already-past cursor and wrongly deleted as
// "not ingestible." Serializing closes this at the source: stored (DB)
// linear rows cannot change while the visibility sweep runs, because the
// only writers — sync tick, reconcile tick, and operator backfill tick —
// are serialized with the sweep by construction. There is no other
// goroutine in this package that can insert/update/delete linear-sourced
// events while runVisibility is executing.
//
// The accepted cost is latency, not correctness: Go tickers drop missed
// ticks rather than queuing them, so a sync tick that fires while a long
// visibility sweep or reconcile is still running simply doesn't run —
// it coalesces into whichever tick fires next on that ticker once the
// select loop is free again. No tick queues up and no handler ever
// double-runs.
//
// Operator backfill is a fourth serialized handler. It consumes one
// durable GraphQL page per tick and does not pause incremental work.
//
// Leader-only: the source's Run starts it, and only a leader term runs
// sources, so two replicas don't double-ingest.
type SyncWorker struct {
	pool    *pgxpool.Pool
	client  *Client
	sink    *events.DBSink
	logger  *slog.Logger
	metrics *syncMetrics

	// ExcludeTeams is the set of team keys to skip at ingest time.
	// Matches against team.key (case-insensitive). Set from config.
	ExcludeTeams []string
	// Observer, when set, is told how each sync, sweep and reconcile went,
	// and of each failed backfill batch.
	Observer worker.Observer

	SyncInterval       time.Duration
	VisibilityInterval time.Duration
	ReconcileInterval  time.Duration
	BackfillInterval   time.Duration
	WatermarkOverlap   time.Duration
}

// NewSyncWorker constructs a SyncWorker with default intervals.
func NewSyncWorker(pool *pgxpool.Pool, client *Client, sink *events.DBSink, logger *slog.Logger, reg prometheus.Registerer) *SyncWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	return &SyncWorker{
		pool:               pool,
		client:             client,
		sink:               sink,
		logger:             logger,
		metrics:            newSyncMetrics(reg),
		SyncInterval:       5 * time.Minute,
		VisibilityInterval: time.Hour,
		ReconcileInterval:  24 * time.Hour,
		BackfillInterval:   time.Second,
		WatermarkOverlap:   2 * time.Minute,
	}
}

// Run blocks until ctx is cancelled, driving all four handlers inline in
// a single select loop — no goroutines, no concurrency between handlers.
// Boot tick: all four fire once immediately, in order, so a fresh pod
// catches up without waiting for the first ticker fire.
func (w *SyncWorker) Run(ctx context.Context) error {
	w.logger.Info("linear sync worker starting",
		"sync_interval", w.SyncInterval,
		"visibility_interval", w.VisibilityInterval,
		"reconcile_interval", w.ReconcileInterval,
		"backfill_interval", w.BackfillInterval,
		"overlap", w.WatermarkOverlap,
		"exclude_teams", strings.Join(w.ExcludeTeams, ","))

	syncTicker := time.NewTicker(w.SyncInterval)
	defer syncTicker.Stop()
	visibilityTicker := time.NewTicker(w.VisibilityInterval)
	defer visibilityTicker.Stop()
	reconcileTicker := time.NewTicker(w.ReconcileInterval)
	defer reconcileTicker.Stop()
	backfillTicker := time.NewTicker(w.BackfillInterval)
	defer backfillTicker.Stop()

	// Boot tick: all four run inline, in order. See the SyncWorker doc
	// comment for why this is safe to run un-parallelized.
	w.runSyncSafe(ctx)
	w.runVisibilitySafe(ctx)
	w.runReconcileSafe(ctx)
	w.runBackfillSafe(ctx)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("linear sync worker stopping", "reason", ctx.Err())
			return ctx.Err()
		case <-syncTicker.C:
			w.runSyncSafe(ctx)
		case <-visibilityTicker.C:
			w.runVisibilitySafe(ctx)
		case <-reconcileTicker.C:
			w.runReconcileSafe(ctx)
		case <-backfillTicker.C:
			w.runBackfillSafe(ctx)
		}
	}
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

// runSyncSafe wraps runSync with error handling.
func (w *SyncWorker) runSyncSafe(ctx context.Context) {
	err := w.runSync(ctx)
	w.observe(err)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Error("linear sync loop failed", "err", err)
	}
}

// runVisibilitySafe wraps runVisibility with error handling.
func (w *SyncWorker) runVisibilitySafe(ctx context.Context) {
	err := w.runVisibility(ctx)
	w.observe(err)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Error("linear visibility sweep failed", "err", err)
		w.metrics.visibilitySweepFailures.Inc()
	}
}

// runReconcileSafe wraps runReconcile with error handling.
func (w *SyncWorker) runReconcileSafe(ctx context.Context) {
	err := w.runReconcile(ctx)
	w.observe(err)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Error("linear reconcile loop failed", "err", err)
	}
}

func (w *SyncWorker) runBackfillSafe(ctx context.Context) {
	if err := w.runBackfillBatch(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			w.observe(err)
			w.logger.Error("linear backfill batch failed", "err", err)
		}
	}
}

func (w *SyncWorker) runBackfillBatch(ctx context.Context) error {
	var requestedAt *time.Time
	var cursor, project, projectID *string
	if err := w.pool.QueryRow(ctx, `
		SELECT backfill_requested_at, backfill_cursor,
		       backfill_project, backfill_project_id
		  FROM linear_sync WHERE id = 1
	`).Scan(&requestedAt, &cursor, &project, &projectID); err != nil {
		return fmt.Errorf("read backfill state: %w", err)
	}
	if requestedAt == nil {
		return nil
	}
	resolvedProjectID := ""
	if projectID != nil {
		resolvedProjectID = *projectID
	} else if project != nil && *project != "" {
		var err error
		resolvedProjectID, err = w.client.ResolveProjectID(ctx, *project)
		if err != nil {
			return fmt.Errorf("resolve project %q: %w", *project, err)
		}
		if _, err := w.pool.Exec(ctx, `
			UPDATE linear_sync SET backfill_project_id = $2
			 WHERE id = 1 AND backfill_requested_at = $1
		`, *requestedAt, resolvedProjectID); err != nil {
			return fmt.Errorf("persist project id: %w", err)
		}
	}
	startCursor := ""
	if cursor != nil {
		startCursor = *cursor
	}
	runner := NewBackfillRunner(w.client, w)
	batch, err := runner.RunPage(ctx, startCursor, resolvedProjectID, syncPageSize, false)
	if err != nil {
		return err
	}
	w.metrics.observeComplexity(batch.complexity)

	if batch.TotalErrors > 0 {
		// Don't advance the cursor (or clear the request as done) past a
		// page that had a write failure: the next backfill tick must
		// refetch and retry this same page, not silently skip the
		// failed issue(s). Leaving backfill_cursor untouched means the
		// retry starts from the same startCursor; already-written issues
		// in the page come back as cheap NoChange upserts. runBackfillSafe
		// logs and reports this failure to the Observer.
		return fmt.Errorf("linear backfill batch: %d of %d issue(s) failed to write; cursor not advanced", batch.TotalErrors, batch.TotalIssues)
	}

	if batch.Done {
		_, err = w.pool.Exec(ctx, `
			UPDATE linear_sync
			   SET backfill_requested_at = NULL, backfill_cursor = NULL,
			       backfill_project = NULL, backfill_project_id = NULL
			 WHERE id = 1 AND backfill_requested_at = $1
		`, *requestedAt)
	} else {
		_, err = w.pool.Exec(ctx, `
			UPDATE linear_sync SET backfill_cursor = $2
			 WHERE id = 1 AND backfill_requested_at = $1
		`, *requestedAt, batch.NextCursor)
	}
	if err != nil {
		return fmt.Errorf("advance backfill cursor: %w", err)
	}
	w.logger.Info("linear backfill batch complete",
		"issues", batch.TotalIssues, "upserted", batch.TotalUpserted,
		"errors", batch.TotalErrors, "done", batch.Done)
	return nil
}

// incrementalQuery is the GraphQL query used for incremental sync and
// reconcile. Fetches issues updated since a cutoff with nested comments
// (first 50 per issue) to minimize request count.
const incrementalQuery = `
query Sync($after: String, $since: DateTimeOrDuration!, $first: Int!) {
  issues(
    filter: { updatedAt: { gte: $since } }
    orderBy: updatedAt
    first: $first
    after: $after
    includeArchived: true
  ) {
    nodes {
      id identifier title description url
      createdAt updatedAt priority
      creator { id name displayName email }
      assignee { id name displayName email }
      state { name type }
      team { id name key private }
      project { id name }
      labels { nodes { name } }
      comments(first: 50) {
        nodes {
          id body createdAt updatedAt
          user { id name displayName email }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// commentsQuery fetches additional comments for an issue when the inline
// comments connection had more pages. Used only for comment-overflow issues.
const commentsQuery = `
query IssueComments($issueID: String!, $after: String!) {
  issue(id: $issueID) {
    comments(first: 50, after: $after) {
      nodes {
        id body createdAt updatedAt
        user { id name displayName email }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// syncPageSize is the number of issues per page for incremental/reconcile.
const syncPageSize = 50

// runSync executes one incremental sync tick.
func (w *SyncWorker) runSync(ctx context.Context) error {
	start := time.Now()
	timer := w.metrics.startSyncTimer()

	cutoff, err := w.readSyncWatermark(ctx)
	if err != nil {
		timer.done("err")
		return fmt.Errorf("read sync watermark: %w", err)
	}
	w.logger.Info("linear sync tick: starting",
		"cutoff", cutoff.Format(time.RFC3339),
		"now", start.Format(time.RFC3339))

	count, failed, err := w.walkUpdatedSince(ctx, cutoff)
	if err != nil {
		timer.done("err")
		return fmt.Errorf("incremental walk: %w", err)
	}

	if failed > 0 {
		// Do not advance the watermark past data that failed to write
		// this tick: the next tick's `updatedAt gte since` filter must
		// still see the failed issue(s). Leaving last_sync_at untouched
		// means the next tick re-walks the same window; already-written
		// issues come back as cheap NoChange upserts. runSyncSafe logs
		// and reports this failure to the Observer.
		timer.done("err")
		return fmt.Errorf("linear sync tick: %d of %d issue(s) failed to write; watermark not advanced", failed, count+failed)
	}

	newWatermark := start.Add(-w.WatermarkOverlap)
	if err := w.advanceSyncWatermark(ctx, newWatermark); err != nil {
		timer.done("err")
		return fmt.Errorf("advance sync watermark: %w", err)
	}

	timer.done("ok")
	w.logger.Info("linear sync tick: done",
		"upserted", count,
		"new_watermark", newWatermark.Format(time.RFC3339),
		"duration", time.Since(start))
	return nil
}

// runReconcile executes the full drift catch-up walk. Uses the same
// updatedAt-ordered pagination as the incremental loop but with a
// zero-time cutoff (i.e. "all issues").
func (w *SyncWorker) runReconcile(ctx context.Context) error {
	start := time.Now()
	w.logger.Info("linear reconcile tick: starting")

	// Zero time = walk everything. Unlike the incremental loop, reconcile's
	// own cutoff is always zero regardless of last_reconcile_at, so a
	// write failure here can't make it skip anything on the next run —
	// everything is re-walked either way. That timestamp is purely
	// informational, so it's fine to advance it even when some writes in
	// this walk failed (they'll be retried by the next reconcile, or
	// sooner by the incremental loop's own watermark-hold-back).
	count, failed, err := w.walkUpdatedSince(ctx, time.Time{})
	if err != nil {
		return fmt.Errorf("reconcile walk: %w", err)
	}
	if failed > 0 {
		w.logger.Error("linear reconcile tick: issue write(s) failed",
			"upserted", count, "failed", failed)
	}

	if err := w.advanceReconcileTimestamp(ctx, start); err != nil {
		return fmt.Errorf("advance reconcile timestamp: %w", err)
	}
	w.logger.Info("linear reconcile tick: done",
		"upserted", count, "duration", time.Since(start))
	return nil
}

// walkUpdatedSince paginates through all issues updated since cutoff
// (zero = all issues), upserts each one, and returns the total upserted
// count and the total number of per-issue write/fetch failures
// (batch.TotalErrors summed across pages) so the caller can decide
// whether it's safe to advance anything keyed to "this walk completed
// cleanly."
func (w *SyncWorker) walkUpdatedSince(ctx context.Context, cutoff time.Time) (int, int, error) {
	var (
		cursor string
		total  int
		failed int
	)

	sinceFmt := cutoff.UTC().Format(time.RFC3339)
	if cutoff.IsZero() {
		// Use a far-past date so the filter matches everything.
		sinceFmt = "1970-01-01T00:00:00Z"
	}
	runner := NewBackfillRunner(w.client, w)

	for {
		select {
		case <-ctx.Done():
			return total, failed, ctx.Err()
		default:
		}

		vars := map[string]any{
			"since": sinceFmt,
			"first": syncPageSize,
		}
		if cursor != "" {
			vars["after"] = cursor
		}

		batch, err := runner.runIssuePage(ctx, incrementalQuery, vars, false)
		if err != nil {
			return total, failed, err
		}
		w.metrics.observeComplexity(batch.complexity)
		total += batch.TotalUpserted + batch.TotalNoChange + batch.TotalRefused
		failed += batch.TotalErrors
		if batch.Done {
			break
		}
		cursor = batch.NextCursor
	}

	return total, failed, nil
}

// fetchRemainingComments fetches additional pages of comments for an issue
// that has more than 50 comments, appending them to issue.Comments.Nodes.
func (w *SyncWorker) fetchRemainingComments(ctx context.Context, issue *Issue) error {
	cursor := issue.Comments.PageInfo.EndCursor
	for cursor != "" {
		vars := map[string]any{
			"issueID": issue.ID,
			"after":   cursor,
		}
		data, ci, err := w.client.Do(ctx, commentsQuery, vars)
		if err != nil {
			return err
		}
		w.metrics.observeComplexity(ci)

		var resp struct {
			Issue struct {
				Comments CommentConn `json:"comments"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("decode comments: %w", err)
		}

		issue.Comments.Nodes = append(issue.Comments.Nodes, resp.Issue.Comments.Nodes...)
		if !resp.Issue.Comments.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Issue.Comments.PageInfo.EndCursor
	}
	return nil
}

// upsertOutcome describes what upsertIssue did. Used by BackfillRunner to
// populate per-category counters in BackfillResult.
type upsertOutcome int

const (
	outcomeInserted upsertOutcome = iota
	outcomeUpdated
	outcomeNoChange
	outcomeRefused // guard hit; issue was not written (and any stale row was deleted)
)

// guardReason reports why an issue must be refused (and any
// previously-ingested row for it deleted) -- "private_team",
// "excluded_team", or "" if the issue is ingestible. It depends only on
// issue/team metadata that's present on every issue in a page, never on
// whether that issue's comments fetched cleanly, so callers can make the
// refusal decision before doing any per-issue work (like the comment
// overflow fetch) that a refused issue's content would never need.
func (w *SyncWorker) guardReason(issue *Issue) string {
	switch {
	case issue.Team != nil && issue.Team.Private:
		return "private_team"
	case w.isExcluded(issue):
		return "excluded_team"
	}
	return ""
}

// upsertIssue applies the privacy guard and writes the issue to the sink.
// On a guard hit, it also deletes any previously-ingested event for this
// issue so that a public→private transition is caught within one tick rather
// than waiting for the hourly visibility sweep. The guard counter is
// incremented here; guard deletions have their own counter.
func (w *SyncWorker) upsertIssue(ctx context.Context, issue *Issue) (upsertOutcome, error) {
	guardReason := w.guardReason(issue)

	if guardReason != "" {
		w.metrics.guardRefusals.WithLabelValues(guardReason).Inc()
		w.logger.Info("linear: refusing issue",
			"reason", guardReason,
			"issue_id", issue.ID,
			"identifier", issue.Identifier,
			"team_key", teamKey(issue))

		// Delete any previously-ingested event so a public→private move
		// is cleaned up within the incremental tick, not deferred to the
		// hourly visibility sweep.
		res, err := w.sink.Delete(ctx, "linear", issue.ID)
		if err != nil {
			w.metrics.guardDeletions.WithLabelValues("err").Inc()
			w.logger.Warn("linear: guard delete failed",
				"issue_id", issue.ID, "err", err)
			// Non-fatal: the visibility sweep will catch it on the next hour.
		} else if res.Existed {
			w.metrics.guardDeletions.WithLabelValues("ok").Inc()
			w.logger.Info("linear: guard delete: removed previously-ingested event",
				"reason", guardReason,
				"issue_id", issue.ID,
				"identifier", issue.Identifier)
		}
		return outcomeRefused, nil
	}

	ev := events.Event{
		Source:         "linear",
		ExternalID:     issue.ID,
		OccurredAt:     issue.UpdatedAt,
		Actor:          actorName(issue),
		ActorCanonical: actorCanonical(issue),
		ActorID:        actorID(issue),
		ThreadID:       teamID(issue),
		Title:          renderTitle(issue),
		Body:           renderBody(issue),
		URL:            issue.URL,
		Raw:            json.RawMessage(`{}`),
		Scope:          events.ScopeInternal,
	}

	res, err := w.sink.Upsert(ctx, ev)
	if err != nil {
		return outcomeInserted, err
	}
	switch {
	case res.NoChange:
		w.metrics.syncUpserts.WithLabelValues("no_change").Inc()
		return outcomeNoChange, nil
	case res.WasUpdate:
		w.metrics.syncUpserts.WithLabelValues("updated").Inc()
		return outcomeUpdated, nil
	default:
		w.metrics.syncUpserts.WithLabelValues("inserted").Inc()
		return outcomeInserted, nil
	}
}

// isExcluded reports whether an issue's team key is in the exclusion list.
func (w *SyncWorker) isExcluded(issue *Issue) bool {
	key := teamKey(issue)
	if key == "" {
		return false
	}
	keyLower := strings.ToLower(key)
	for _, ex := range w.ExcludeTeams {
		if strings.ToLower(ex) == keyLower {
			return true
		}
	}
	return false
}

// teamKey returns the team key for an issue or "" if the issue has no team.
func teamKey(issue *Issue) string {
	if issue.Team != nil {
		return issue.Team.Key
	}
	return ""
}

// visibilityQuery fetches the minimal issue set needed for the visibility
// sweep: just id, team key, and team.private. Cheap per request.
//
// orderBy: createdAt (not the default updatedAt) is deliberate: cursor
// pagination over an updatedAt-ordered collection can skip issues that get
// touched mid-walk — an issue updated after the walk has passed its old
// position teleports ahead of the cursor, so the walk never revisits it
// and it's wrongly treated as "not currently ingestible." createdAt
// ordering is append-only during a walk: an issue's position relative to
// already-visited items can't change, so an issue that existed before the
// walk started cannot be skipped by it. This is defense-in-depth on top of
// the worker serialization in Run (which removes the concurrent-write race
// entirely) and the pre-delete recheck below (recheckCandidate) — an extra
// layer against pagination-inconsistency bugs on Linear's side.
const visibilityQuery = `
query VisibilitySweep($after: String, $first: Int!) {
  issues(
    first: $first
    after: $after
    includeArchived: true
    orderBy: createdAt
  ) {
    nodes {
      id
      team { id key private }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// runVisibility pages through all currently visible issues, diffs against
// stored linear events, and deletes rows that are no longer ingestible
// (tombstoned team moved to private, or team now in exclusion list).
//
// Before deleting, every candidate is re-verified with a point query
// (recheckCandidate) — belt-and-suspenders on top of the createdAt walk
// ordering (visibilityQuery) and the worker serialization (Run): a point
// query at delete-time can only be stale by the time between fetching
// that response and issuing the delete, not by the ~350s duration of the
// whole walk.
func (w *SyncWorker) runVisibility(ctx context.Context) error {
	start := time.Now()
	w.logger.Info("linear visibility sweep: starting")

	// Build the ingestible set from the API.
	ingestible, err := w.fetchIngestibleIDs(ctx)
	if err != nil {
		return fmt.Errorf("fetch ingestible ids: %w", err)
	}

	// Fetch all stored linear external_ids from the DB.
	stored, err := w.storedLinearIDs(ctx)
	if err != nil {
		return fmt.Errorf("fetch stored linear ids: %w", err)
	}

	// Diff: delete anything stored that is not in the ingestible set,
	// after re-verifying each candidate with a point query.
	var deleted, spared, skipped int
	for id := range stored {
		if ingestible[id] {
			continue
		}

		switch w.recheckCandidate(ctx, id) {
		case recheckSpare:
			spared++
			w.metrics.visibilityFalsePositives.Inc()
			continue
		case recheckSkipTransient:
			skipped++
			continue
		case recheckDelete:
			// fall through to delete below
		}

		res, err := w.sink.Delete(ctx, "linear", id)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			w.logger.Error("linear visibility sweep: delete failed",
				"external_id", id, "err", err)
			w.metrics.visibilityDeletions.WithLabelValues("err").Inc()
			continue
		}
		if res.Existed {
			deleted++
			w.metrics.visibilityDeletions.WithLabelValues("ok").Inc()
			w.logger.Info("linear visibility sweep: deleted",
				"external_id", id)
		}
	}

	if err := w.advanceVisibilityTimestamp(ctx, start); err != nil {
		return fmt.Errorf("advance visibility timestamp: %w", err)
	}
	w.logger.Info("linear visibility sweep: done",
		"ingestible", len(ingestible),
		"stored", len(stored),
		"deleted", deleted,
		"spared_false_positive", spared,
		"skipped_transient", skipped,
		"duration", time.Since(start))
	return nil
}

// visibilityRecheckQuery re-verifies a single candidate issue immediately
// before deletion. Belt-and-suspenders on top of the walk-ordering fix
// (createdAt orderBy) and the serialization fix (Run no longer races
// writers against the sweep): a point query at delete-time can only be
// stale by the time between fetching this response and issuing the
// delete, not by the ~350s duration of the whole walk.
const visibilityRecheckQuery = `
query VisibilityRecheck($id: String!) {
  issue(id: $id) {
    id
    archivedAt
    trashed
    team { id key private }
  }
}`

// recheckResult is the outcome of a point-query re-verification for one
// visibility-sweep delete candidate.
type recheckResult int

const (
	recheckDelete        recheckResult = iota // confirmed not-ingestible: proceed with delete
	recheckSpare                              // false positive: do NOT delete
	recheckSkipTransient                      // transient error: skip this candidate this sweep
)

// recheckCandidate re-verifies one visibility-sweep delete candidate with
// a point query and returns the outcome per this decision table:
//
//  1. Entity-not-found-shaped GraphQL error (issue is gone/trashed via
//     Linear's own deletion) -> recheckDelete.
//  2. Query succeeds, team is private or excluded -> recheckDelete
//     (correctly not-ingestible).
//  3. Query succeeds, team is public + non-excluded, issue not archived
//     -> recheckSpare (false positive): the walk missed a currently
//     ingestible issue.
//  4. Query succeeds, team is public + non-excluded, issue IS archived
//     -> recheckSpare (false positive), not recheckDelete: the
//     incremental loop and the visibility walk both use
//     includeArchived: true, so archived-but-public issues ARE
//     ingestible and should have appeared in the walk's ingestible set.
//     Their absence means the walk missed them, same as case 3.
//  5. Any other error (transient: 5xx, network, timeout — i.e. NOT the
//     entity-not-found shape from case 1) -> recheckSkipTransient.
//     Skipping a delete is recoverable next sweep; a wrong delete loses
//     data. The fail-safe direction only goes one way.
func (w *SyncWorker) recheckCandidate(ctx context.Context, id string) recheckResult {
	data, _, err := w.client.Do(ctx, visibilityRecheckQuery, map[string]any{"id": id})
	if err != nil {
		if isEntityNotFoundError(err) {
			return recheckDelete
		}
		w.logger.Warn("linear visibility sweep: recheck query failed; skipping candidate this sweep",
			"external_id", id, "err", err)
		return recheckSkipTransient
	}

	var resp struct {
		Issue struct {
			ID         string     `json:"id"`
			ArchivedAt *time.Time `json:"archivedAt"`
			Trashed    bool       `json:"trashed"`
			Team       *Team      `json:"team"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		w.logger.Warn("linear visibility sweep: recheck decode failed; skipping candidate this sweep",
			"external_id", id, "err", err)
		return recheckSkipTransient
	}

	if resp.Issue.Trashed {
		return recheckDelete
	}

	mini := &Issue{ID: id, Team: resp.Issue.Team}
	if (resp.Issue.Team != nil && resp.Issue.Team.Private) || w.isExcluded(mini) {
		return recheckDelete
	}

	// Public, non-excluded team. Whether archived or not, the issue is
	// ingestible (both the incremental loop and the visibility walk use
	// includeArchived: true), so its absence from the walk's ingestible
	// set is a false positive — the sweep's walk missed it.
	w.logger.Warn("linear visibility sweep: false positive — walk missed a still-ingestible issue; sparing",
		"external_id", id,
		"archived_at", resp.Issue.ArchivedAt,
		"team_key", teamKey(mini))
	return recheckSpare
}

// isEntityNotFoundError reports whether err is Linear's "entity not
// found" GraphQL error — the shape returned when a point query targets
// an issue that no longer exists (permanently deleted / purged from the
// trash). Client.Do wraps GraphQL-level errors as a single
// fmt.Errorf("linear: GraphQL errors: %s", ...) with no structured error
// type (see client.go doOnce), so this matches on message content.
// Confirmed error text shape (independently corroborated across several
// third-party Linear API clients): "Entity not found: Issue". Matching is
// a case-insensitive substring on "entity not found" rather than the
// exact "Entity not found: Issue" string, so it also catches the shape
// for a differently-typed entity if Linear's error wording varies at all
// — narrower matching risked silently falling through to
// recheckSkipTransient (the safe direction, but it would mean sweeps
// never actually confirm a trashed-issue delete via this path).
//
// This is intentionally narrow — one substring check for one call site,
// not a general GraphQL error-code parser. If more call sites need to
// distinguish GraphQL error kinds, Client.Do should expose structured
// error info (e.g. a typed error wrapping the raw messages) instead of
// every caller re-matching on text.
func isEntityNotFoundError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "entity not found")
}

// fetchIngestibleIDs pages through all issues and returns the set of IDs
// that are currently ingestible (non-private, non-excluded teams).
func (w *SyncWorker) fetchIngestibleIDs(ctx context.Context) (map[string]bool, error) {
	result := make(map[string]bool)
	cursor := ""

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		vars := map[string]any{"first": syncPageSize}
		if cursor != "" {
			vars["after"] = cursor
		}

		data, ci, err := w.client.Do(ctx, visibilityQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("linear API: %w", err)
		}
		w.metrics.observeComplexity(ci)

		var resp struct {
			Issues struct {
				Nodes []struct {
					ID   string `json:"id"`
					Team *Team  `json:"team"`
				} `json:"nodes"`
				PageInfo PageInfo `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("decode visibility response: %w", err)
		}

		for _, n := range resp.Issues.Nodes {
			if n.Team != nil && n.Team.Private {
				continue
			}
			// Build a minimal issue to check the exclusion list.
			mini := &Issue{ID: n.ID, Team: n.Team}
			if w.isExcluded(mini) {
				continue
			}
			result[n.ID] = true
		}

		if !resp.Issues.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Issues.PageInfo.EndCursor
	}
	return result, nil
}

// storedLinearIDs returns the set of external_ids currently stored with
// source='linear' in the events table (non-deleted rows).
func (w *SyncWorker) storedLinearIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := w.pool.Query(ctx,
		`SELECT external_id FROM events WHERE source = 'linear' AND NOT deleted`)
	if err != nil {
		return nil, fmt.Errorf("query stored linear ids: %w", err)
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan external_id: %w", err)
		}
		result[id] = true
	}
	return result, rows.Err()
}

// --- DB helpers ---

func (w *SyncWorker) readSyncWatermark(ctx context.Context) (time.Time, error) {
	var t *time.Time
	err := w.pool.QueryRow(ctx,
		`SELECT last_sync_at FROM linear_sync WHERE id = 1`,
	).Scan(&t)
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

func (w *SyncWorker) advanceSyncWatermark(ctx context.Context, at time.Time) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE linear_sync
		   SET last_sync_at = $1,
		       sync_runs_total = sync_runs_total + 1
		 WHERE id = 1
	`, at)
	return err
}

func (w *SyncWorker) advanceReconcileTimestamp(ctx context.Context, at time.Time) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE linear_sync
		   SET last_reconcile_at = $1,
		       reconcile_runs_total = reconcile_runs_total + 1
		 WHERE id = 1
	`, at)
	return err
}

func (w *SyncWorker) advanceVisibilityTimestamp(ctx context.Context, at time.Time) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE linear_sync
		   SET visibility_swept_at = $1,
		       visibility_runs_total = visibility_runs_total + 1
		 WHERE id = 1
	`, at)
	return err
}
