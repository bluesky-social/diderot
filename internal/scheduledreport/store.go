package scheduledreport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxAttempts = 3
	leaseTime   = 30 * time.Minute
)

var ErrNoRun = errors.New("scheduled reports: no runnable occurrence")

// Store owns report definitions, durable occurrence materialization, and run
// state transitions.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

func (s *Store) Create(ctx context.Context, p CreateParams) (*Report, error) {
	if err := p.normalizeAndValidate(); err != nil {
		return nil, err
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	next, err := NextOccurrence(s.now(), p.Cadence, p.Timezone, p.Weekday, p.ScheduleHour, p.ScheduleMinute)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_reports
		       (name, prompt, cadence, weekday, schedule_hour, schedule_minute,
		        timezone, delivery_kind, slack_target, owner_email,
		        catchup_window_seconds, enabled, next_run_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+reportColumns,
		p.Name, p.Prompt, p.Cadence, p.Weekday, p.ScheduleHour, p.ScheduleMinute,
		p.Timezone, p.DeliveryKind, p.SlackTarget, p.OwnerEmail,
		p.CatchupSeconds, enabled, next, p.CreatedBy)
	r, err := scanReport(row)
	if err != nil {
		return nil, fmt.Errorf("create scheduled report: %w", err)
	}
	return &r, nil
}

func (s *Store) List(ctx context.Context) ([]Report, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reportColumns+` FROM scheduled_reports ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list scheduled reports: %w", err)
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, fmt.Errorf("scan scheduled report: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Report, error) {
	r, err := scanReport(s.pool.QueryRow(ctx,
		`SELECT `+reportColumns+` FROM scheduled_reports WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Update replaces the mutable definition fields while preserving identity,
// creator, enabled state, and run history. Recomputing next_run_at makes a
// schedule edit take effect from the next future wall-clock occurrence.
func (s *Store) Update(ctx context.Context, id uuid.UUID, p CreateParams) (*Report, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanReport(tx.QueryRow(ctx,
		`SELECT `+reportColumns+` FROM scheduled_reports WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	p.CreatedBy = current.CreatedBy
	p.Enabled = &current.Enabled
	if err := p.normalizeAndValidate(); err != nil {
		return nil, err
	}
	next, err := NextOccurrence(s.now(), p.Cadence, p.Timezone, p.Weekday, p.ScheduleHour, p.ScheduleMinute)
	if err != nil {
		return nil, err
	}
	updated, err := scanReport(tx.QueryRow(ctx, `
		UPDATE scheduled_reports
		   SET name=$2, prompt=$3, cadence=$4, weekday=$5,
		       schedule_hour=$6, schedule_minute=$7, timezone=$8,
		       delivery_kind=$9, slack_target=$10, owner_email=$11,
		       catchup_window_seconds=$12, next_run_at=$13, updated_at=now()
		 WHERE id=$1 RETURNING `+reportColumns,
		id, p.Name, p.Prompt, p.Cadence, p.Weekday, p.ScheduleHour,
		p.ScheduleMinute, p.Timezone, p.DeliveryKind, p.SlackTarget,
		p.OwnerEmail, p.CatchupSeconds, next))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *Store) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (*Report, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := scanReport(tx.QueryRow(ctx,
		`SELECT `+reportColumns+` FROM scheduled_reports WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	next := r.NextRunAt
	if enabled {
		next, err = NextOccurrence(s.now(), r.Cadence, r.Timezone, r.Weekday, r.ScheduleHour, r.ScheduleMinute)
		if err != nil {
			return nil, err
		}
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE scheduled_report_runs
			   SET status='skipped', error='report disabled', finished_at=now(), updated_at=now()
			 WHERE report_id=$1 AND status='pending'`, id)
		if err != nil {
			return nil, err
		}
	}
	r, err = scanReport(tx.QueryRow(ctx, `
		UPDATE scheduled_reports SET enabled=$2, next_run_at=$3, updated_at=now()
		 WHERE id=$1 RETURNING `+reportColumns, id, enabled, next))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM scheduled_reports WHERE id=$1`, id)
	if err != nil {
		return false, fmt.Errorf("delete scheduled report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EnqueueNow creates a manual occurrence using now as its deterministic window
// end. The stored cadence determines the prior reporting boundary.
func (s *Store) EnqueueNow(ctx context.Context, id uuid.UUID) (*Run, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	start, end, err := WindowFor(*r, now)
	if err != nil {
		return nil, err
	}
	run, err := scanRun(s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_report_runs
		       (report_id, trigger, scheduled_for, window_start, window_end,
		        prompt_snapshot, status)
		VALUES ($1,'manual',$2,$3,$4,$5,'pending')
		RETURNING `+runColumns, id, now, start, end, r.Prompt))
	if err != nil {
		return nil, fmt.Errorf("enqueue scheduled report: %w", err)
	}
	return &run, nil
}

func (s *Store) ListRuns(ctx context.Context, reportID uuid.UUID, limit int) ([]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+runColumns+` FROM scheduled_report_runs
		 WHERE report_id=$1 ORDER BY scheduled_for DESC LIMIT $2`, reportID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRuns returns each report's most recent run, by report ID. A report
// that has never run has no entry.
func (s *Store) LatestRuns(ctx context.Context) (map[uuid.UUID]Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (report_id) `+runColumns+` FROM scheduled_report_runs
		 ORDER BY report_id, scheduled_for DESC, created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("latest scheduled report runs: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan scheduled report run: %w", err)
		}
		out[r.ReportID] = r
	}
	return out, rows.Err()
}

// MaterializeDue converts due schedule cursors into durable occurrences and
// advances every locked definition to its first future wall-clock slot.
func (s *Store) MaterializeDue(ctx context.Context) error {
	now := s.now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT `+reportColumns+` FROM scheduled_reports
		 WHERE enabled AND next_run_at <= $1
		 ORDER BY next_run_at LIMIT 100 FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return err
	}
	var reports []Report
	for rows.Next() {
		r, scanErr := scanReport(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		reports = append(reports, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, report := range reports {
		next := report.NextRunAt.UTC()
		for count := 0; !next.After(now); count++ {
			if count >= 4000 {
				return fmt.Errorf("report %s has more than 4000 missed occurrences", report.ID)
			}
			start, end, windowErr := WindowFor(report, next)
			if windowErr != nil {
				return windowErr
			}
			status := "pending"
			var finished any
			var runErr any
			if now.Sub(next) > report.CatchupWindow {
				status = "skipped"
				finished = now
				runErr = "outside catch-up window"
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO scheduled_report_runs
				       (report_id, trigger, scheduled_for, window_start, window_end,
				        prompt_snapshot, status, finished_at, error)
				VALUES ($1,'scheduled',$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (report_id, scheduled_for) DO NOTHING`,
				report.ID, next, start, end, report.Prompt, status, finished, runErr)
			if err != nil {
				return err
			}
			next, err = NextOccurrence(next, report.Cadence, report.Timezone,
				report.Weekday, report.ScheduleHour, report.ScheduleMinute)
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE scheduled_reports SET next_run_at=$2, updated_at=now() WHERE id=$1`, report.ID, next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ClaimNext leases one pending/generated occurrence or reclaims a stale
// running occurrence. Fresh active runs block another run for the same report.
func (s *Store) ClaimNext(ctx context.Context) (*ClaimedRun, error) {
	now := s.now().UTC()
	staleBefore := now.Add(-leaseTime)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT r.id
		  FROM scheduled_report_runs r
		  JOIN scheduled_reports d ON d.id=r.report_id
		 WHERE (d.enabled OR r.trigger='manual')
		   AND r.attempts < $1
		   AND (r.status IN ('pending','generated') OR
		        (r.status='running' AND r.claimed_at < $2))
		   AND NOT EXISTS (
		       SELECT 1 FROM scheduled_report_runs active
		        WHERE active.report_id=r.report_id
		          AND active.id<>r.id
		          AND active.status='running'
		          AND active.claimed_at >= $2)
		 ORDER BY r.scheduled_for, r.created_at
		 LIMIT 1 FOR UPDATE OF r SKIP LOCKED`, maxAttempts, staleBefore).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoRun
	}
	if err != nil {
		return nil, err
	}
	run, err := scanRun(tx.QueryRow(ctx, `
		UPDATE scheduled_report_runs
		   SET status='running', attempts=attempts+1, claimed_at=$2, updated_at=$2
		 WHERE id=$1 RETURNING `+runColumns, runID, now))
	if err != nil {
		return nil, err
	}
	report, err := scanReport(tx.QueryRow(ctx,
		`SELECT `+reportColumns+` FROM scheduled_reports WHERE id=$1`, run.ReportID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &ClaimedRun{Report: report, Run: run}, nil
}

func (s *Store) SaveGenerated(ctx context.Context, runID, sessionID uuid.UUID, assistantTurn int, output, deliveryKind, slackTarget string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_report_runs
		   SET ask_session_id=$2, ask_assistant_turn=$3, output=$4,
		       delivery_kind=$5, slack_target=$6, error=NULL, updated_at=now()
		 WHERE id=$1 AND status='running' AND output IS NULL`, runID, sessionID, assistantTurn, output, deliveryKind, slackTarget)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("scheduled reports: generation checkpoint lost its running claim")
	}
	return nil
}

func (s *Store) SaveDeliveryTS(ctx context.Context, runID uuid.UUID, deliveryTS string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_report_runs SET delivery_ts=$2, updated_at=now()
		 WHERE id=$1 AND status='running'`, runID, deliveryTS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("scheduled reports: delivery checkpoint lost its running claim")
	}
	return nil
}

func (s *Store) Succeed(ctx context.Context, reportID, runID uuid.UUID, deliveryTS string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE scheduled_report_runs
		   SET status='succeeded', delivery_ts=$2, finished_at=$3,
		       claimed_at=NULL, error=NULL, updated_at=$3
		 WHERE id=$1`, runID, deliveryTS, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE scheduled_reports SET last_success_at=$2, last_error=NULL, updated_at=$2
		 WHERE id=$1`, reportID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Fail records a generation or delivery failure. Generated output remains
// retryable without another model call. The third failed attempt is terminal.
func (s *Store) Fail(ctx context.Context, reportID, runID uuid.UUID, generated bool, runErr error) error {
	msg := truncateError(runErr)
	nextStatus := "pending"
	if generated {
		nextStatus = "generated"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE scheduled_report_runs
		   SET status=CASE WHEN attempts >= $3 THEN 'failed' ELSE $2 END,
		       finished_at=CASE WHEN attempts >= $3 THEN now() ELSE NULL END,
		       claimed_at=NULL, error=$4, updated_at=now()
		 WHERE id=$1`, runID, nextStatus, maxAttempts, msg); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE scheduled_reports SET last_error=$2, updated_at=now() WHERE id=$1`, reportID, msg); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const reportColumns = `id,name,prompt,cadence,weekday,schedule_hour,schedule_minute,
timezone,delivery_kind,slack_target,owner_email,catchup_window_seconds,enabled,
next_run_at,last_success_at,last_error,created_at,updated_at,created_by`

type rowScanner interface{ Scan(...any) error }

func scanReport(row rowScanner) (Report, error) {
	var r Report
	var catchupSeconds int64
	err := row.Scan(&r.ID, &r.Name, &r.Prompt, &r.Cadence, &r.Weekday,
		&r.ScheduleHour, &r.ScheduleMinute, &r.Timezone, &r.DeliveryKind,
		&r.SlackTarget, &r.OwnerEmail, &catchupSeconds, &r.Enabled, &r.NextRunAt,
		&r.LastSuccessAt, &r.LastError, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy)
	r.CatchupWindow = time.Duration(catchupSeconds) * time.Second
	return r, err
}

const runColumns = `id,report_id,trigger,scheduled_for,window_start,window_end,
prompt_snapshot,status,attempts,claimed_at,finished_at,ask_session_id,ask_assistant_turn,output,
delivery_ts,error,created_at,updated_at,delivery_kind,slack_target`

func scanRun(row rowScanner) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.ReportID, &r.Trigger, &r.ScheduledFor,
		&r.WindowStart, &r.WindowEnd, &r.PromptSnapshot, &r.Status,
		&r.Attempts, &r.ClaimedAt, &r.FinishedAt, &r.AskSessionID, &r.AskAssistantTurn, &r.Output,
		&r.DeliveryTS, &r.Error, &r.CreatedAt, &r.UpdatedAt, &r.DeliveryKind, &r.SlackTarget)
	return r, err
}

func truncateError(err error) string {
	if err == nil {
		return "unknown error"
	}
	s := strings.TrimSpace(err.Error())
	const limit = 2000
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
