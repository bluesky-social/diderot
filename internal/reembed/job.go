package reembed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/adminjob"
)

// JobKind is the admin_jobs kind of a re-embed. Any replica records a
// request as a queued job; the leader's JobWorker claims and runs it.
const JobKind = "reembed"

const (
	jobHeartbeatInterval = 2 * time.Second
	jobStaleAfter        = 30 * time.Second
	jobFinishAttempts    = 3
	jobFinishRetryDelay  = 100 * time.Millisecond
)

// JobRequest is a job's request column. No sources means every source the
// driver can rebuild.
type JobRequest struct {
	Sources []string `json:"sources,omitempty"`
}

// JobProgress is a job's progress column.
type JobProgress struct {
	Source    string `json:"source,omitempty"`
	Total     int64  `json:"total"`
	Processed int64  `json:"processed"`
	Rewritten int64  `json:"rewritten"`
	NoChange  int64  `json:"no_change"`
	Skipped   int64  `json:"skipped"`
	Errors    int64  `json:"errors"`
	LastError string `json:"last_error,omitempty"`
}

// Request records a re-embed of sources for the leader to run and returns
// its job id, or adminjob.ErrActive while another is queued or running.
func Request(ctx context.Context, jobs *adminjob.Store, requestedBy string, sources []string) (int64, error) {
	requestedBy = strings.ToLower(strings.TrimSpace(requestedBy))
	job, err := jobs.Enqueue(ctx, JobKind, requestedBy, JobRequest{Sources: sources}, jobStaleAfter)
	if err != nil {
		return 0, err
	}
	return job.ID, nil
}

// JobLedger is what JobWorker needs of adminjob.Store.
type JobLedger interface {
	Claim(ctx context.Context, kind string) (adminjob.Job, bool, error)
	Heartbeat(ctx context.Context, id int64, progress any) error
	Finish(ctx context.Context, id int64, status string, progress any, jobErr error) error
}

// JobRunner is what JobWorker needs of Driver.
type JobRunner interface {
	Run(ctx context.Context, sources []string, progress *Progress) error
}

// JobWorker runs requested re-embeds, one at a time, on the leader.
type JobWorker struct {
	Jobs   JobLedger
	Driver JobRunner
	Logger *slog.Logger
}

// Tick claims the queued re-embed, if there is one, and runs it to its end,
// recording progress as it goes. A job cut short because ctx ended, as when
// leadership is lost, is recorded as canceled. The job's own outcome is in
// the ledger; the error returned is a failure to claim it or to record its
// end.
func (w *JobWorker) Tick(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	job, claimed, err := w.Jobs.Claim(ctx, JobKind)
	if err != nil || !claimed {
		return err
	}
	var request JobRequest
	if err := json.Unmarshal(job.Request, &request); err != nil {
		return w.finish(job.ID, "failed", JobProgress{}, fmt.Errorf("decode request: %w", err))
	}

	progress := &Progress{}
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(jobHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Not ctx: the heartbeat stops only once the run has.
				persistCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := w.Jobs.Heartbeat(persistCtx, job.ID, snapshotProgress(progress))
				cancel()
				if err != nil && !errors.Is(err, adminjob.ErrNotRunning) {
					w.Logger.Warn("persist reembed heartbeat", "job_id", job.ID, "err", err)
				}
			}
		}
	}()

	runErr := w.Driver.Run(ctx, request.Sources, progress)
	close(done)
	heartbeat.Wait()

	status, jobErr := "succeeded", runErr
	switch {
	case errors.Is(runErr, context.Canceled):
		status = "canceled"
	case runErr != nil || progress.Errors.Load() > 0:
		status = "failed"
		if jobErr == nil {
			lastError, _ := progress.LastErr.Load().(string)
			if lastError == "" {
				lastError = fmt.Sprintf("reembed completed with %d errors", progress.Errors.Load())
			}
			jobErr = errors.New(lastError)
		}
	}
	return w.finish(job.ID, status, snapshotProgress(progress), jobErr)
}

// finish records a job's end, outside the run's context, which may be what
// ended it.
func (w *JobWorker) finish(jobID int64, status string, progress JobProgress, jobErr error) error {
	if jobErr != nil {
		w.Logger.Warn("reembed run exited", "job_id", jobID, "status", status, "err", jobErr)
	}
	var finishErr error
	for attempt := range jobFinishAttempts {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		finishErr = w.Jobs.Finish(finishCtx, jobID, status, progress, jobErr)
		cancel()
		if finishErr == nil || errors.Is(finishErr, adminjob.ErrNotRunning) {
			return nil
		}
		if attempt+1 < jobFinishAttempts {
			time.Sleep(time.Duration(attempt+1) * jobFinishRetryDelay)
		}
	}
	return fmt.Errorf("record reembed job %d as %s: %w", jobID, status, finishErr)
}

func snapshotProgress(progress *Progress) JobProgress {
	snapshot := JobProgress{
		Total: progress.Total.Load(), Processed: progress.Processed.Load(),
		Rewritten: progress.Rewritten.Load(), NoChange: progress.NoChange.Load(),
		Skipped: progress.Skipped.Load(), Errors: progress.Errors.Load(),
	}
	snapshot.Source, _ = progress.Source.Load().(string)
	snapshot.LastError, _ = progress.LastErr.Load().(string)
	return snapshot
}
