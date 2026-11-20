package adminapi

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/adminjob"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
)

const (
	// SlackDocumentRebuildJobKind is the admin_jobs kind of a rebuild of
	// every opted-in channel's documents.
	SlackDocumentRebuildJobKind    = "slack_document_rebuild"
	slackDocumentRebuildStaleAfter = 2 * time.Minute
	slackDocumentRebuildHeartbeat  = 5 * time.Second
	// slackDocumentRebuildPageTimeout bounds one page of the rebuild, the
	// first of which deletes every opted-in channel's extracted documents.
	slackDocumentRebuildPageTimeout = 10 * time.Minute
)

// ErrNoDocumentChannels is a document rebuild asked for while no channel has
// document indexing on.
var ErrNoDocumentChannels = errors.New("no Slack channel has document indexing on")

// SlackDocumentRebuildProgress is a document rebuild's progress column.
type SlackDocumentRebuildProgress struct {
	Cursor int64 `json:"cursor"`
	// Scanned counts the stored messages with files read so far, Queued
	// those whose documents wait to be read again, and Documents those
	// documents.
	Scanned   int    `json:"scanned"`
	Queued    int    `json:"queued"`
	Documents int    `json:"documents"`
	Errors    int    `json:"errors"`
	LastError string `json:"last_error,omitempty"`
}

// StartSlackDocumentRebuild deletes what Diderot read from the documents of
// every channel with document indexing on and queues those documents to be
// read again, as a detached, heartbeat-backed job, one at a time across
// replicas. It works on stored messages; the document workers, which need
// Slack on, then read the queued documents.
func (s *Server) StartSlackDocumentRebuild(ctx context.Context, requestedBy string) (int64, error) {
	if _, on := s.slackControls(); !on {
		return 0, ErrSlackOff
	}
	pool := s.getStatusPool()
	if pool == nil {
		return 0, errNoJobStore
	}
	var found bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM slack_channel_config WHERE doc_ingest_enabled)`).Scan(&found); err != nil {
		return 0, fmt.Errorf("check document channels: %w", err)
	}
	if !found {
		return 0, ErrNoDocumentChannels
	}
	jobs := adminjob.NewStore(pool)
	job, err := jobs.Start(ctx, SlackDocumentRebuildJobKind, strings.ToLower(strings.TrimSpace(requestedBy)),
		nil, slackDocumentRebuildStaleAfter)
	if err != nil {
		return 0, err
	}
	go s.runSlackDocumentRebuild(jobs, job.ID)
	return job.ID, nil
}

func (s *Server) runSlackDocumentRebuild(jobs detachedJobLedger, jobID int64) {
	var mu sync.Mutex
	var progress SlackDocumentRebuildProgress
	snapshot := func() SlackDocumentRebuildProgress {
		mu.Lock()
		defer mu.Unlock()
		return progress
	}
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(slackDocumentRebuildHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := jobs.Heartbeat(persistCtx, jobID, snapshot())
				cancel()
				if err != nil && !errors.Is(err, adminjob.ErrNotRunning) {
					s.logger.Warn("persist Slack document rebuild heartbeat", "job_id", jobID, "err", err)
				}
			}
		}
	}()

	var runErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("slack document rebuild panic: %v", recovered)
				s.logger.Error("Slack document rebuild panic", "job_id", jobID, "panic", recovered, "stack", string(debug.Stack()))
			}
		}()
		runErr = s.rebuildSlackDocuments(context.Background(), func(page slackcx.DocumentBackfillResult) {
			mu.Lock()
			defer mu.Unlock()
			progress.Cursor = page.NextCursor
			progress.Scanned += page.Scanned
			progress.Queued += page.Enqueued
			progress.Documents += page.Documents
			progress.Errors += len(page.Errors)
			if len(page.Errors) > 0 {
				last := page.Errors[len(page.Errors)-1]
				progress.LastError = fmt.Sprintf("event %d: %s", last.EventID, last.Err)
			}
		})
	}()
	final := snapshot()
	if runErr == nil && final.Errors > 0 {
		runErr = fmt.Errorf("document rebuild completed with %d message errors; last: %s", final.Errors, final.LastError)
	}
	status := "succeeded"
	if runErr != nil {
		status = "failed"
	}
	close(done)
	heartbeat.Wait()
	finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	finishErr := jobs.Finish(finishCtx, jobID, status, final, runErr)
	cancel()
	if finishErr != nil && !errors.Is(finishErr, adminjob.ErrNotRunning) {
		s.logger.Error("persist Slack document rebuild completion", "job_id", jobID, "status", status, "err", finishErr)
	}
	if runErr != nil {
		s.logger.Warn("Slack document rebuild failed", "job_id", jobID, "err", runErr)
	}
}

// rebuildSlackDocuments pages through the stored messages of every opted-in
// channel, reporting each page; only the first page deletes.
func (s *Server) rebuildSlackDocuments(ctx context.Context, report func(slackcx.DocumentBackfillResult)) error {
	pool := s.getStatusPool()
	if pool == nil {
		return errNoJobStore
	}
	cursor := int64(0)
	for {
		// A page that waits on a lock would otherwise keep the job, and its
		// heartbeat, alive for good, and every later request refused.
		pageCtx, cancel := context.WithTimeout(ctx, slackDocumentRebuildPageTimeout)
		page, err := slackcx.BackfillDocuments(pageCtx, pool, slackcx.DocumentBackfillOptions{SinceID: cursor, Reset: true})
		cancel()
		if err != nil {
			return err
		}
		report(page)
		if page.Done {
			return nil
		}
		if page.NextCursor <= cursor {
			return fmt.Errorf("document rebuild cursor did not advance beyond %d", cursor)
		}
		cursor = page.NextCursor
	}
}
