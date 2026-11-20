package adminaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/scheduledreport"
)

// Reports is the scheduled reports store (*scheduledreport.Store).
type Reports interface {
	Create(context.Context, scheduledreport.CreateParams) (*scheduledreport.Report, error)
	Update(context.Context, uuid.UUID, scheduledreport.CreateParams) (*scheduledreport.Report, error)
	SetEnabled(context.Context, uuid.UUID, bool) (*scheduledreport.Report, error)
	Delete(context.Context, uuid.UUID) (bool, error)
	EnqueueNow(context.Context, uuid.UUID) (*scheduledreport.Run, error)
}

var errReportsUnavailable = errors.New("scheduled reports are unavailable")

// reportDetails is what a report's audit record keeps of its definition.
func reportDetails(params scheduledreport.CreateParams) map[string]any {
	return map[string]any{
		"cadence": params.Cadence, "weekday": params.Weekday,
		"schedule_time": fmt.Sprintf("%02d:%02d", params.ScheduleHour, params.ScheduleMinute),
		"timezone":      params.Timezone, "delivery_kind": params.DeliveryKind,
		"slack_target": params.SlackTarget, "owner_email": params.OwnerEmail,
		"catchup_seconds": params.CatchupSeconds,
	}
}

// CreateReport creates a scheduled report, created by the actor.
func CreateReport(ctx context.Context, a Audit, reports Reports, params scheduledreport.CreateParams) (*scheduledreport.Report, error) {
	params.CreatedBy = a.actor()
	params.Normalize()
	var report *scheduledreport.Report
	err := a.Run(ctx, "reports.create", params.Name, reportDetails(params), func() error {
		if reports == nil {
			return errReportsUnavailable
		}
		var err error
		report, err = reports.Create(ctx, params)
		return err
	})
	return report, err
}

// UpdateReport replaces a report's definition.
func UpdateReport(ctx context.Context, a Audit, reports Reports, id uuid.UUID, params scheduledreport.CreateParams) (*scheduledreport.Report, error) {
	params.Normalize()
	var report *scheduledreport.Report
	err := a.Run(ctx, "reports.update", id.String(), reportDetails(params), func() error {
		if reports == nil {
			return errReportsUnavailable
		}
		var err error
		report, err = reports.Update(ctx, id, params)
		return err
	})
	return report, err
}

// SetReportEnabled turns a report's schedule on or off. It fails with
// pgx.ErrNoRows when there is no such report.
func SetReportEnabled(ctx context.Context, a Audit, reports Reports, id uuid.UUID, enabled bool) (*scheduledreport.Report, error) {
	action := "reports.disable"
	if enabled {
		action = "reports.enable"
	}
	var report *scheduledreport.Report
	err := a.Run(ctx, action, id.String(), map[string]any{"enabled": enabled}, func() error {
		if reports == nil {
			return errReportsUnavailable
		}
		var err error
		report, err = reports.SetEnabled(ctx, id, enabled)
		return err
	})
	return report, err
}

// RunReport queues a run of a report now. It fails with pgx.ErrNoRows when
// there is no such report.
func RunReport(ctx context.Context, a Audit, reports Reports, id uuid.UUID) (*scheduledreport.Run, error) {
	var run *scheduledreport.Run
	err := a.Run(ctx, "reports.run", id.String(), map[string]any{"trigger": "manual"}, func() error {
		if reports == nil {
			return errReportsUnavailable
		}
		var err error
		run, err = reports.EnqueueNow(ctx, id)
		return err
	})
	return run, err
}

// DeleteReport deletes a report and its run history. It fails with
// pgx.ErrNoRows when there is no such report.
func DeleteReport(ctx context.Context, a Audit, reports Reports, id uuid.UUID) error {
	return a.Run(ctx, "reports.delete", id.String(), map[string]any{"run_history": "cascade"}, func() error {
		if reports == nil {
			return errReportsUnavailable
		}
		deleted, err := reports.Delete(ctx, id)
		if err == nil && !deleted {
			err = pgx.ErrNoRows
		}
		return err
	})
}
