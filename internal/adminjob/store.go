// Package adminjob persists progress and outcomes for long-running operator
// work. Execution stays in the owning domain; this package is only the durable
// lifecycle ledger behind the browser UI's long-running actions.
package adminjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const activeKindConstraint = "admin_jobs_one_active_kind"

var (
	ErrActive     = errors.New("admin job already active")
	ErrNotRunning = errors.New("admin job is not running")
)

type Job struct {
	ID          int64
	Kind        string
	Status      string
	RequestedBy string
	Request     json.RawMessage
	Progress    json.RawMessage
	RequestedAt time.Time
	StartedAt   *time.Time
	HeartbeatAt time.Time
	StaleAfter  time.Duration
	FinishedAt  *time.Time
	Error       string
}

const jobColumns = `id, kind, status, requested_by, request, progress,
	requested_at, started_at, heartbeat_at, stale_after_seconds, finished_at,
	COALESCE(error, '')`

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Start creates a running job, for work the caller executes itself, after
// failing an expired running job of the same kind. The partial unique index
// is the final arbiter when two callers race.
func (s *Store) Start(ctx context.Context, kind, requestedBy string, request any, staleAfter time.Duration) (Job, error) {
	return s.insert(ctx, "running", kind, requestedBy, request, staleAfter)
}

// Enqueue records a queued job for whichever process executes the kind to
// Claim, after failing an expired running job of the same kind. A queued job
// has no executor yet, so it never expires: it waits until one claims it.
func (s *Store) Enqueue(ctx context.Context, kind, requestedBy string, request any, staleAfter time.Duration) (Job, error) {
	return s.insert(ctx, "queued", kind, requestedBy, request, staleAfter)
}

// Claim marks the queued job of kind running, for the caller to execute, and
// reports false when none is queued. The job's heartbeat starts now.
func (s *Store) Claim(ctx context.Context, kind string) (Job, bool, error) {
	var job Job
	err := scanJob(s.pool.QueryRow(ctx, `
		UPDATE admin_jobs
		   SET status = 'running', started_at = now(), heartbeat_at = now()
		 WHERE kind = $1 AND status = 'queued'
		RETURNING `+jobColumns, strings.TrimSpace(kind)), &job)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim admin job: %w", err)
	}
	return job, true, nil
}

func (s *Store) insert(ctx context.Context, status, kind, requestedBy string, request any, staleAfter time.Duration) (Job, error) {
	var job Job
	if s == nil || s.pool == nil {
		return job, fmt.Errorf("admin job store is not configured")
	}
	kind = strings.TrimSpace(kind)
	requestedBy = strings.TrimSpace(requestedBy)
	if kind == "" {
		return job, fmt.Errorf("admin job kind is required")
	}
	if requestedBy == "" {
		return job, fmt.Errorf("admin job requester is required")
	}
	if staleAfter <= 0 {
		return job, fmt.Errorf("admin job stale interval must be positive")
	}
	staleSeconds := int64((staleAfter + time.Second - 1) / time.Second)
	requestJSON, err := marshalPayload(request)
	if err != nil {
		return job, fmt.Errorf("encode admin job request: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return job, fmt.Errorf("begin admin job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		UPDATE admin_jobs
		   SET status = 'failed', finished_at = now(),
		       error = 'job heartbeat expired before completion'
		 WHERE kind = $1
		   AND status = 'running'
		   AND heartbeat_at < now() - make_interval(secs => stale_after_seconds)
	`, kind)
	if err != nil {
		return job, fmt.Errorf("expire stale admin job: %w", err)
	}

	err = scanJob(tx.QueryRow(ctx, `
		INSERT INTO admin_jobs
		       (kind, status, requested_by, request, started_at, stale_after_seconds)
		VALUES ($1, $2::text, $3, $4, CASE WHEN $2::text = 'running' THEN now() END, $5)
		RETURNING `+jobColumns, kind, status, requestedBy, requestJSON, staleSeconds), &job)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == activeKindConstraint {
			return Job{}, ErrActive
		}
		return Job{}, fmt.Errorf("insert admin job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("commit admin job: %w", err)
	}
	return job, nil
}

// Heartbeat replaces the job's progress snapshot and proves its executor is
// still alive. A terminal or missing row is never revived.
func (s *Store) Heartbeat(ctx context.Context, id int64, progress any) error {
	progressJSON, err := marshalPayload(progress)
	if err != nil {
		return fmt.Errorf("encode admin job progress: %w", err)
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE admin_jobs
		   SET progress = $2, heartbeat_at = now()
		 WHERE id = $1 AND status = 'running'
	`, id, progressJSON)
	if err != nil {
		return fmt.Errorf("heartbeat admin job: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrNotRunning
	}
	return nil
}

// Finish records the final progress snapshot and terminal state.
func (s *Store) Finish(ctx context.Context, id int64, status string, progress any, jobErr error) error {
	if status != "succeeded" && status != "failed" && status != "canceled" {
		return fmt.Errorf("invalid terminal admin job status %q", status)
	}
	progressJSON, err := marshalPayload(progress)
	if err != nil {
		return fmt.Errorf("encode final admin job progress: %w", err)
	}
	errorText := ""
	if jobErr != nil {
		errorText = compactError(jobErr.Error(), 2000)
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE admin_jobs
		   SET status = $2, progress = $3, heartbeat_at = now(),
		       finished_at = now(), error = NULLIF($4, '')
		 WHERE id = $1 AND status = 'running'
	`, id, status, progressJSON, errorText)
	if err != nil {
		return fmt.Errorf("finish admin job: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrNotRunning
	}
	return nil
}

func (s *Store) Latest(ctx context.Context, kind string) (Job, error) {
	var job Job
	err := scanJob(s.pool.QueryRow(ctx, `
		SELECT `+jobColumns+`
		  FROM admin_jobs
		 WHERE kind = $1
		 ORDER BY requested_at DESC, id DESC
		 LIMIT 1
	`, strings.TrimSpace(kind)), &job)
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner, job *Job) error {
	var staleSeconds int64
	err := row.Scan(
		&job.ID, &job.Kind, &job.Status, &job.RequestedBy,
		&job.Request, &job.Progress, &job.RequestedAt, &job.StartedAt,
		&job.HeartbeatAt, &staleSeconds, &job.FinishedAt, &job.Error,
	)
	job.StaleAfter = time.Duration(staleSeconds) * time.Second
	return err
}

func marshalPayload(value any) ([]byte, error) {
	if value == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(value)
}

func compactError(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}
