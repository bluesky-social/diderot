package reembed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func jobStore(t *testing.T, name string) (*adminjob.Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, name))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `TRUNCATE admin_jobs`)
	require.NoError(t, err)
	return adminjob.NewStore(pool), pool
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runnerFunc is a re-embed run as a function.
type runnerFunc func(ctx context.Context, sources []string, progress *Progress) error

func (f runnerFunc) Run(ctx context.Context, sources []string, progress *Progress) error {
	return f(ctx, sources, progress)
}

// TestJobWorkerLeavesARequestForTheNextLeader: a job cut short when its
// leader's term ends is recorded as canceled, and a request recorded while
// no leader runs waits, however long, for the next leader, which runs it.
func TestJobWorkerLeavesARequestForTheNextLeader(t *testing.T) {
	ctx := context.Background()
	jobs, pool := jobStore(t, "reembed_job_failover")

	first, err := Request(ctx, jobs, "Operator@Example.com", []string{"slack"})
	require.NoError(t, err)

	// The first leader starts the job and loses its term mid-run.
	started := make(chan struct{})
	term, endTerm := context.WithCancel(ctx)
	leader := &JobWorker{Jobs: jobs, Logger: discard(), Driver: runnerFunc(func(ctx context.Context, _ []string, progress *Progress) error {
		progress.Processed.Store(3)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})}
	done := make(chan error, 1)
	go func() { done <- leader.Tick(term) }()
	<-started
	_, err = Request(ctx, jobs, "operator@example.com", nil)
	require.ErrorIs(t, err, adminjob.ErrActive, "one re-embed at a time")
	endTerm()
	require.NoError(t, <-done)
	job, err := jobs.Latest(ctx, JobKind)
	require.NoError(t, err)
	assert.Equal(t, first, job.ID)
	assert.Equal(t, "canceled", job.Status)
	assert.Equal(t, "operator@example.com", job.RequestedBy)
	assert.JSONEq(t, `{"total":0,"processed":3,"rewritten":0,"no_change":0,"skipped":0,"errors":0}`, string(job.Progress))
	require.NoError(t, leader.Tick(term), "a term that has ended claims nothing")

	// A request made while no leader runs waits past any heartbeat window.
	second, err := Request(ctx, jobs, "operator@example.com", nil)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET heartbeat_at = now() - interval '1 day' WHERE id = $1`, second)
	require.NoError(t, err)
	_, err = Request(ctx, jobs, "operator@example.com", nil)
	require.ErrorIs(t, err, adminjob.ErrActive, "a waiting request is not expired by the next one")

	// The next leader runs it.
	var ran [][]string
	next := &JobWorker{Jobs: jobs, Logger: discard(), Driver: runnerFunc(func(_ context.Context, sources []string, progress *Progress) error {
		ran = append(ran, sources)
		progress.Processed.Store(5)
		return nil
	})}
	require.NoError(t, next.Tick(ctx))
	assert.Len(t, ran, 1)
	assert.Empty(t, ran[0], "no sources means every source")
	job, err = jobs.Latest(ctx, JobKind)
	require.NoError(t, err)
	assert.Equal(t, second, job.ID)
	assert.Equal(t, "succeeded", job.Status)
	assert.NotNil(t, job.StartedAt)
}

// TestJobWorkerRecordsARunWithErrorsAsFailed: a run that finishes with row
// or source errors is a failed job carrying the last error.
func TestJobWorkerRecordsARunWithErrorsAsFailed(t *testing.T) {
	ctx := context.Background()
	jobs, _ := jobStore(t, "reembed_job_failed")
	_, err := Request(ctx, jobs, "operator@example.com", nil)
	require.NoError(t, err)

	worker := &JobWorker{Jobs: jobs, Logger: discard(), Driver: runnerFunc(func(_ context.Context, _ []string, progress *Progress) error {
		progress.Errors.Add(1)
		progress.LastErr.Store(ErrSourceOff.Error())
		return nil
	})}
	require.NoError(t, worker.Tick(ctx))
	job, err := jobs.Latest(ctx, JobKind)
	require.NoError(t, err)
	assert.Equal(t, "failed", job.Status)
	assert.Equal(t, ErrSourceOff.Error(), job.Error)
}

// flakyLedger hands out one job and fails the first failures Finish calls.
type flakyLedger struct {
	failures, finishes int
}

func (*flakyLedger) Claim(context.Context, string) (adminjob.Job, bool, error) {
	return adminjob.Job{ID: 17, Request: []byte(`{}`)}, true, nil
}

func (*flakyLedger) Heartbeat(context.Context, int64, any) error { return nil }

func (l *flakyLedger) Finish(context.Context, int64, string, any, error) error {
	l.finishes++
	if l.finishes <= l.failures {
		return errors.New("transient database failure")
	}
	return nil
}

func TestJobWorkerRetriesRecordingAJobsEnd(t *testing.T) {
	run := runnerFunc(func(context.Context, []string, *Progress) error { return nil })

	ledger := &flakyLedger{failures: jobFinishAttempts - 1}
	require.NoError(t, (&JobWorker{Jobs: ledger, Driver: run, Logger: discard()}).Tick(context.Background()))
	assert.Equal(t, jobFinishAttempts, ledger.finishes)

	ledger = &flakyLedger{failures: jobFinishAttempts}
	err := (&JobWorker{Jobs: ledger, Driver: run, Logger: discard()}).Tick(context.Background())
	require.ErrorContains(t, err, "transient database failure")
	assert.Equal(t, jobFinishAttempts, ledger.finishes)
}
