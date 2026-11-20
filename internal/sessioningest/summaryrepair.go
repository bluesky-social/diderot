package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

// SummaryRepairJobKind is the admin job that repairs published session
// summaries the reduce model wrote with part of its tool call inside a field
// (sessionsummary.RepairStoredSummary). Any replica records the request; the
// leader's SummaryRepairWorker runs it.
const SummaryRepairJobKind = "session_summary_repair"

const (
	summaryRepairHeartbeat  = 2 * time.Second
	summaryRepairStaleAfter = 2 * time.Minute
	summaryRepairPage       = 100
)

// summaryRepairScope selects the published summaries the repair considers:
// written in the last 30 days, current for their session, and holding
// something that may be tool-call markup, which always follows a closing
// tag. RepairStoredSummary decides.
const summaryRepairScope = `
	     s.summarized_at > now() - interval '30 days'
	 AND s.revision = c.revision AND c.published_revision = c.revision
	 AND c.complete AND NOT c.deleted AND c.excluded_at IS NULL
	 AND (s.narrative LIKE '%</%' OR COALESCE(s.embedding_text,'') LIKE '%</%'
	      OR COALESCE(s.context_hint,'') LIKE '%</%'
	      OR EXISTS (SELECT 1 FROM unnest(s.tags) tag WHERE tag LIKE '%<%>%'))`

// SummaryRepairPlan is what a summary repair would do now.
type SummaryRepairPlan struct {
	// FromText counts summaries whose every field can be restored from their
	// stored text, with no model call.
	FromText int64
	// Resummarize counts summaries that must be written again: one call to
	// the reduce model each, over chunk summaries already stored.
	Resummarize int64
	// ResummarizeBytes is the chunk-summary text those calls read.
	ResummarizeBytes int64
}

type summaryRepairTarget struct {
	Identity
	Stored sessionsummary.StoredSummary
}

// summaryRepairTargets pages through the summaries in scope, by session ID.
func (s *Store) summaryRepairTargets(ctx context.Context, after string, limit int) ([]summaryRepairTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.owner, s.client, s.session_id, s.narrative, COALESCE(s.embedding_text,''),
		       COALESCE(s.context_hint,''), s.tags
		  FROM session_ingest_summaries s
		  JOIN session_ingest_candidates c
		    ON c.owner=s.owner AND c.client=s.client AND c.session_id=s.session_id
		 WHERE `+summaryRepairScope+` AND s.session_id > $1
		 ORDER BY s.session_id
		 LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: list summaries to repair: %w", err)
	}
	defer rows.Close()
	var targets []summaryRepairTarget
	for rows.Next() {
		var target summaryRepairTarget
		if err := rows.Scan(&target.Owner, &target.Client, &target.SessionID, &target.Stored.Narrative,
			&target.Stored.EmbeddingText, &target.Stored.ContextHint, &target.Stored.Tags); err != nil {
			return nil, fmt.Errorf("sessioningest: scan summary to repair: %w", err)
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// PlanSummaryRepair counts the summaries a repair would restore from their
// text and those it would write again.
func (s *Store) PlanSummaryRepair(ctx context.Context) (SummaryRepairPlan, error) {
	var plan SummaryRepairPlan
	after := ""
	for {
		targets, err := s.summaryRepairTargets(ctx, after, 1000)
		if err != nil {
			return SummaryRepairPlan{}, err
		}
		for _, target := range targets {
			_, changed, complete := sessionsummary.RepairStoredSummary(target.Stored)
			switch {
			case !changed:
			case complete:
				plan.FromText++
			default:
				plan.Resummarize++
				var bytes int64
				if err := s.pool.QueryRow(ctx, `
					SELECT COALESCE(sum(length(summary) + length(array_to_string(evidence, ' '))), 0)
					  FROM session_ingest_chunk_summaries
					 WHERE owner=$1 AND client=$2 AND session_id=$3
				`, target.Owner, target.Client, target.SessionID).Scan(&bytes); err != nil {
					return SummaryRepairPlan{}, fmt.Errorf("sessioningest: size chunk summaries to reduce: %w", err)
				}
				plan.ResummarizeBytes += bytes
			}
		}
		if len(targets) < 1000 {
			return plan, nil
		}
		after = targets[len(targets)-1].SessionID
	}
}

// LoadPublishedSummary returns a session's published summary for revision
// with its chunks, as stored.
func (s *Store) LoadPublishedSummary(ctx context.Context, id Identity, revision string) (SummaryResult, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: begin published summary read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var result SummaryResult
	err = tx.QueryRow(ctx, `
		SELECT narrative, COALESCE(embedding_text,''), COALESCE(context_hint,''), tags, model_id,
		       input_tokens, output_tokens, input_digest,
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title END, ''),
		       COALESCE(CASE WHEN title_input_digest=input_digest THEN title_model END, '')
		  FROM session_ingest_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3 AND revision=$4
	`, id.Owner, id.Client, id.SessionID, revision).Scan(&result.Narrative, &result.EmbeddingText,
		&result.ContextHint, &result.Tags, &result.ModelID, &result.InputTokens, &result.OutputTokens,
		&result.InputDigest, &result.Title, &result.TitleModel)
	if errors.Is(err, pgx.ErrNoRows) {
		return SummaryResult{}, false, nil
	}
	if err != nil {
		return SummaryResult{}, false, fmt.Errorf("sessioningest: load published summary: %w", err)
	}
	if result.Chunks, err = publishedChunks(ctx, tx, id); err != nil {
		return SummaryResult{}, false, err
	}
	return result, true, nil
}

// unusableSummaryDigest replaces the input digest of a summary whose text
// must not be used again. It matches no computed digest, so neither exact
// reuse nor folding trusts the summary, and, unlike an empty digest, it is
// never taken for a summary from before digests to adopt.
const unusableSummaryDigest = "unusable"

// ForgetSummaryInput marks a session's published summary unusable: the next
// summary of the session reads every chunk summary again.
func (s *Store) ForgetSummaryInput(ctx context.Context, id Identity) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE session_ingest_summaries SET input_digest=$4
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, id.Owner, id.Client, id.SessionID, unusableSummaryDigest); err != nil {
		return fmt.Errorf("sessioningest: forget summary input: %w", err)
	}
	return nil
}

// SummaryRepairRequest records a summary repair for the leader to run and
// returns its job ID, or adminjob.ErrActive while one is queued or running.
func SummaryRepairRequest(ctx context.Context, jobs *adminjob.Store, requestedBy string) (int64, error) {
	job, err := jobs.Enqueue(ctx, SummaryRepairJobKind, strings.ToLower(strings.TrimSpace(requestedBy)), struct{}{}, summaryRepairStaleAfter)
	if err != nil {
		return 0, err
	}
	return job.ID, nil
}

// SummaryRepairProgress is a summary repair job's progress column.
type SummaryRepairProgress struct {
	Total       int64  `json:"total"`
	Processed   int64  `json:"processed"`
	FromText    int64  `json:"from_text"`
	Resummarize int64  `json:"resummarized"`
	Skipped     int64  `json:"skipped"`
	Errors      int64  `json:"errors"`
	LastError   string `json:"last_error,omitempty"`
}

// SummaryRepairLedger is what SummaryRepairWorker needs of adminjob.Store.
type SummaryRepairLedger interface {
	Claim(ctx context.Context, kind string) (adminjob.Job, bool, error)
	Heartbeat(ctx context.Context, id int64, progress any) error
	Finish(ctx context.Context, id int64, status string, progress any, jobErr error) error
}

// SummaryRepairWorker runs a requested summary repair on the leader. A
// summary whose fields are all in its stored text is corrected and published
// again with no model call; any other is summarized again from its stored
// chunk summaries. Each is republished through the ingest worker, so its
// search text is embedded again. Running it twice repairs nothing twice: a
// repaired summary no longer holds markup.
type SummaryRepairWorker struct {
	Ingest *Worker
	Jobs   SummaryRepairLedger
	Logger *slog.Logger
}

// Tick claims the queued repair, if there is one, and runs it to its end.
func (r *SummaryRepairWorker) Tick(ctx context.Context) error {
	job, claimed, err := r.Jobs.Claim(ctx, SummaryRepairJobKind)
	if err != nil || !claimed {
		return err
	}
	plan, err := r.Ingest.Store.PlanSummaryRepair(ctx)
	if err != nil {
		return r.finish(job.ID, "failed", SummaryRepairProgress{}, err)
	}
	var (
		mu       sync.Mutex
		progress = SummaryRepairProgress{Total: plan.FromText + plan.Resummarize}
	)
	snapshot := func() SummaryRepairProgress {
		mu.Lock()
		defer mu.Unlock()
		return progress
	}
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(summaryRepairHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				persistCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				if err := r.Jobs.Heartbeat(persistCtx, job.ID, snapshot()); err != nil && !errors.Is(err, adminjob.ErrNotRunning) {
					r.Logger.Warn("persist summary repair heartbeat", "job_id", job.ID, "err", err)
				}
				cancel()
			}
		}
	}()

	runErr := func() error {
		after := ""
		for {
			targets, err := r.Ingest.Store.summaryRepairTargets(ctx, after, summaryRepairPage)
			if err != nil {
				return err
			}
			for _, target := range targets {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				outcome, err := r.Ingest.repairSummary(ctx, target)
				mu.Lock()
				switch {
				case err != nil:
					progress.Errors++
					progress.LastError = err.Error()
				case outcome == summaryRepairedFromText:
					progress.FromText++
				case outcome == summaryResummarized:
					progress.Resummarize++
				case outcome == summaryRepairSkipped:
					progress.Skipped++
				}
				if outcome != summaryRepairUnchanged || err != nil {
					progress.Processed++
				}
				mu.Unlock()
				if err != nil {
					r.Logger.Warn("summary repair failed", "session", target.SessionID, "err", err)
				}
			}
			if len(targets) < summaryRepairPage {
				return nil
			}
			after = targets[len(targets)-1].SessionID
		}
	}()
	close(done)
	heartbeat.Wait()

	final := snapshot()
	status := "succeeded"
	switch {
	case errors.Is(runErr, context.Canceled):
		status = "canceled"
	case runErr != nil:
		status = "failed"
	case final.Errors > 0:
		status, runErr = "failed", fmt.Errorf("%d summaries could not be repaired; last: %s", final.Errors, final.LastError)
	}
	return r.finish(job.ID, status, final, runErr)
}

func (r *SummaryRepairWorker) finish(jobID int64, status string, progress SummaryRepairProgress, jobErr error) error {
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Jobs.Finish(finishCtx, jobID, status, progress, jobErr); err != nil && !errors.Is(err, adminjob.ErrNotRunning) {
		return fmt.Errorf("record summary repair job %d as %s: %w", jobID, status, err)
	}
	return nil
}

type summaryRepairOutcome int

const (
	summaryRepairUnchanged summaryRepairOutcome = iota
	summaryRepairSkipped
	summaryRepairedFromText
	summaryResummarized
)

// repairSummary repairs one published summary and publishes it again.
func (w *Worker) repairSummary(ctx context.Context, target summaryRepairTarget) (summaryRepairOutcome, error) {
	if _, changed, _ := sessionsummary.RepairStoredSummary(target.Stored); !changed {
		return summaryRepairUnchanged, nil
	}
	candidate, err := w.Store.LoadCandidate(ctx, target.Identity)
	if err != nil {
		return summaryRepairSkipped, err
	}
	// A session with a newer revision waiting is summarized again anyway,
	// from the repaired text of this one (foldEarlierSummary).
	if candidate.Revision == "" || candidate.Revision != candidate.PublishedRevision ||
		!candidate.Complete || candidate.Deleted || candidate.ExcludedAt != nil {
		return summaryRepairSkipped, nil
	}
	// Repair the stored summary of the revision about to be republished, as
	// it is now: the page of targets may predate a newer publish.
	summary, ok, err := w.Store.LoadPublishedSummary(ctx, target.Identity, candidate.Revision)
	if err != nil || !ok {
		return summaryRepairSkipped, err
	}
	repaired, changed, complete := sessionsummary.RepairStoredSummary(sessionsummary.StoredSummary{
		Narrative: summary.Narrative, EmbeddingText: summary.EmbeddingText,
		ContextHint: summary.ContextHint, Tags: summary.Tags,
	})
	if !changed {
		return summaryRepairUnchanged, nil
	}
	doc, contentDigest, _, err := w.loadDocument(ctx, candidate)
	if err != nil {
		return summaryRepairSkipped, err
	}
	if !complete {
		if err := w.Store.ForgetSummaryInput(ctx, target.Identity); err != nil {
			return summaryRepairSkipped, err
		}
		result, err := w.publish(ctx, doc, candidate, contentDigest)
		if err != nil {
			return summaryRepairSkipped, err
		}
		if result.Stale {
			return summaryRepairSkipped, nil
		}
		return summaryResummarized, nil
	}
	summary.Narrative, summary.EmbeddingText, summary.ContextHint, summary.Tags =
		repaired.Narrative, repaired.EmbeddingText, repaired.ContextHint, repaired.Tags
	summary.Reused = true
	result, err := w.Publish.PublishSession(ctx, doc, target.Identity, w.Cfg.ViewerBaseURL,
		candidate.Revision, candidate.RewriteEpoch, contentDigest, candidate.VisibilityScope, summary)
	if err != nil {
		return summaryRepairSkipped, err
	}
	if result.Stale {
		return summaryRepairSkipped, nil
	}
	return summaryRepairedFromText, nil
}
