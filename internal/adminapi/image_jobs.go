package adminapi

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminjob"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/events"
)

const (
	slackImageReplayJobKind    = "slack_image_replay"
	slackImageReplayBatchSize  = 500
	slackImageReplayStaleAfter = 2 * time.Minute
	slackImageReplayHeartbeat  = 5 * time.Second
)

// errNoJobStore is a job asked of a server that was given no database.
var errNoJobStore = errors.New("admin job store not configured")

type detachedJobLedger interface {
	Heartbeat(context.Context, int64, any) error
	Finish(context.Context, int64, string, any, error) error
}

type slackImageReplayRequest struct {
	ChannelID string `json:"channel_id"`
}

// SlackImageReplayProgress is persisted in admin_jobs and intentionally
// compact: malformed-event details stay in the terminal error, and the jobs
// page shows the counters as they grow.
type SlackImageReplayProgress struct {
	Phase         string `json:"phase"`
	ChannelID     string `json:"channel_id"`
	Cursor        int64  `json:"cursor"`
	Scanned       int    `json:"scanned"`
	Reconciled    int    `json:"reconciled"`
	NoAttachments int    `json:"no_attachments"`
	Errors        int    `json:"errors"`
	LastError     string `json:"last_error,omitempty"`
	Done          bool   `json:"done"`
}

type slackImageReplayState struct {
	mu       sync.Mutex
	progress SlackImageReplayProgress
}

func (s *slackImageReplayState) snapshot() SlackImageReplayProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

func (s *slackImageReplayState) update(fn func(*SlackImageReplayProgress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.progress)
}

// StartSlackImageReplay atomically replaces each parseable event's derived
// image rows in a detached, heartbeat-backed job, which works on stored
// events whether or not Slack is on. The admin_jobs uniqueness constraint
// allows one at a time across replicas. Captioning remains
// asynchronous: this job repopulates event_images for the existing caption
// worker to drain while malformed events retain their prior derived state.
func (s *Server) StartSlackImageReplay(ctx context.Context, requestedBy, channelID string) (int64, error) {
	pool := s.getStatusPool()
	if pool == nil {
		return 0, errNoJobStore
	}
	requestedBy = strings.ToLower(strings.TrimSpace(requestedBy))
	channelID = strings.TrimSpace(channelID)
	if channelID != "" {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM slack_channel_info WHERE channel_id=$1
				UNION ALL SELECT 1 FROM slack_channel_sync WHERE channel_id=$1
				UNION ALL SELECT 1 FROM slack_message_details WHERE channel_id=$1
			)`, channelID).Scan(&exists); err != nil {
			return 0, fmt.Errorf("check Slack channel: %w", err)
		}
		if !exists {
			return 0, errors.New("slack channel not found")
		}
	}

	jobs := adminjob.NewStore(pool)
	job, err := jobs.Start(ctx, slackImageReplayJobKind, requestedBy,
		slackImageReplayRequest{ChannelID: channelID}, slackImageReplayStaleAfter)
	if err != nil {
		return 0, err
	}
	state := &slackImageReplayState{progress: SlackImageReplayProgress{
		Phase: "resetting", ChannelID: channelID,
	}}
	go s.runSlackImageReplay(jobs, job.ID, state)
	return job.ID, nil
}

func (s *Server) runSlackImageReplay(jobs detachedJobLedger, jobID int64, state *slackImageReplayState) {
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(slackImageReplayHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := jobs.Heartbeat(persistCtx, jobID, state.snapshot())
				cancel()
				if err != nil && !errors.Is(err, adminjob.ErrNotRunning) {
					s.logger.Warn("persist Slack image replay heartbeat", "job_id", jobID, "err", err)
				}
			}
		}
	}()

	var runErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("slack image replay panic: %v", recovered)
				s.logger.Error("Slack image replay panic", "job_id", jobID, "panic", recovered, "stack", string(debug.Stack()))
			}
		}()
		runErr = s.replaySlackImages(context.Background(), state)
	}()

	status := "succeeded"
	state.update(func(progress *SlackImageReplayProgress) {
		progress.Done = runErr == nil
		progress.Phase = "complete"
		if runErr != nil {
			status = "failed"
			progress.Phase = "failed"
			if progress.LastError == "" {
				progress.LastError = runErr.Error()
			}
		}
	})
	close(done)
	heartbeat.Wait()
	finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	finishErr := jobs.Finish(finishCtx, jobID, status, state.snapshot(), runErr)
	cancel()
	if finishErr != nil && !errors.Is(finishErr, adminjob.ErrNotRunning) {
		s.logger.Error("persist Slack image replay completion", "job_id", jobID, "status", status, "err", finishErr)
	}
	if runErr != nil {
		s.logger.Warn("Slack image replay failed", "job_id", jobID, "err", runErr)
	}
}

func (s *Server) replaySlackImages(ctx context.Context, state *slackImageReplayState) error {
	pool := s.getStatusPool()
	if pool == nil {
		return errNoJobStore
	}
	progress := state.snapshot()
	cursor := int64(0)
	for {
		batch, err := backfillSlackImagesBatch(ctx, pool, progress.ChannelID, cursor)
		if err != nil {
			return err
		}
		state.update(func(progress *SlackImageReplayProgress) {
			progress.Phase = "scanning"
			progress.Cursor = batch.NextCursor
			progress.Scanned += batch.Scanned
			progress.Reconciled += batch.Reconciled
			progress.NoAttachments += batch.NoAttachments
			progress.Errors += len(batch.Errors)
			if len(batch.Errors) > 0 {
				last := batch.Errors[len(batch.Errors)-1]
				progress.LastError = fmt.Sprintf("event %d: %s", last.EventID, last.Err)
			}
		})
		if batch.Done {
			break
		}
		if batch.NextCursor <= cursor {
			return fmt.Errorf("image replay cursor did not advance beyond %d", cursor)
		}
		cursor = batch.NextCursor
	}
	final := state.snapshot()
	if final.Errors > 0 {
		return fmt.Errorf("image replay completed with %d event errors; last: %s", final.Errors, final.LastError)
	}
	return nil
}

// slackImageBatch is the outcome of one batch of an image replay.
type slackImageBatch struct {
	Scanned       int
	Reconciled    int
	NoAttachments int
	Errors        []slackImageError
	NextCursor    int64
	Done          bool
}

// slackImageError is one event the replay could not rebuild; the replay
// continues past it.
type slackImageError struct {
	EventID int64
	Err     string
}

// backfillSlackImagesBatch replaces the derived image rows of one bounded
// batch of stored Slack events after sinceID, in one channel or, with an
// empty channelID, in all of them. The image replay job loops it.
func backfillSlackImagesBatch(ctx context.Context, pool *pgxpool.Pool, channelID string, sinceID int64) (slackImageBatch, error) {
	resp := slackImageBatch{NextCursor: sinceID}

	// Drive the sweep with a single-statement query that filters
	// has_files via the slack_message_details join (events doesn't have a
	// has_files column). Existing derived rows are also selected so a replay
	// cleans up historical images whose source event no longer has files.
	// Pull events.raw eagerly so per-row replacement needs no second read.
	args := []any{sinceID, slackImageReplayBatchSize}
	channelClause := ""
	if channelID != "" {
		channelClause = "AND d.channel_id = $3"
		args = append(args, channelID)
	}
	q := fmt.Sprintf(`
		SELECT e.id, e.raw
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.source = 'slack'
		   AND e.deleted = false
		   AND (d.has_files = true OR EXISTS (
		         SELECT 1 FROM event_images i WHERE i.event_id=e.id))
		   AND e.id > $1
		   %s
		 ORDER BY e.id ASC
		 LIMIT $2
	`, channelClause)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return resp, fmt.Errorf("query events: %w", err)
	}
	type rawRow struct {
		id  int64
		raw []byte
	}
	var batch []rawRow
	for rows.Next() {
		var rr rawRow
		if err := rows.Scan(&rr.id, &rr.raw); err != nil {
			rows.Close()
			return resp, fmt.Errorf("scan image replay event: %w", err)
		}
		batch = append(batch, rr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return resp, fmt.Errorf("iterate image replay events: %w", err)
	}

	for _, rr := range batch {
		resp.Scanned++
		resp.NextCursor = rr.id

		details, derr := slackcx.DetailsFrom(rr.raw)
		if derr != nil {
			resp.Errors = append(resp.Errors, slackImageError{
				EventID: rr.id,
				Err:     "parse: " + derr.Error(),
			})
			continue
		}
		if len(details.Attachments) == 0 {
			// has_files=true but the raw blob has no usable file
			// records — Slack tombstoned the file or the parser
			// dropped malformed entries. Count separately so the
			// operator can sanity-check expectations.
			resp.NoAttachments++
			if err := events.ReplaceImages(ctx, pool, rr.id, nil); err != nil {
				resp.Errors = append(resp.Errors, slackImageError{
					EventID: rr.id, Err: "replace: " + err.Error(),
				})
			}
			continue
		}

		if err := events.ReplaceImages(ctx, pool, rr.id, slackcx.ImageRefsFromAttachments(details.Attachments)); err != nil {
			resp.Errors = append(resp.Errors, slackImageError{
				EventID: rr.id,
				Err:     "replace: " + err.Error(),
			})
			continue
		}
		resp.Reconciled++
	}

	resp.Done = len(batch) < slackImageReplayBatchSize
	return resp, nil
}
