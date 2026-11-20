// Package scheduledreport runs recurring, retrieval-grounded reports through
// Diderot's existing ask loop and delivers them to Slack.
package scheduledreport

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	CadenceDaily    = "daily"
	CadenceWeekdays = "weekdays"
	CadenceWeekly   = "weekly"

	DeliverySlackChannel = "slack_channel"
	DeliverySlackDM      = "slack_dm"

	defaultCatchupWindow = 6 * time.Hour
	maxPromptBytes       = 32 << 10
	maxNameBytes         = 200
)

// Report is one current recurring-report definition.
type Report struct {
	ID             uuid.UUID     `json:"id"`
	Name           string        `json:"name"`
	Prompt         string        `json:"prompt"`
	Cadence        string        `json:"cadence"`
	Weekday        *int          `json:"weekday,omitempty"`
	ScheduleHour   int           `json:"schedule_hour"`
	ScheduleMinute int           `json:"schedule_minute"`
	Timezone       string        `json:"timezone"`
	DeliveryKind   string        `json:"delivery_kind"`
	SlackTarget    string        `json:"slack_target"`
	OwnerEmail     string        `json:"owner_email"`
	CatchupWindow  time.Duration `json:"-"`
	Enabled        bool          `json:"enabled"`
	NextRunAt      time.Time     `json:"next_run_at"`
	LastSuccessAt  *time.Time    `json:"last_success_at,omitempty"`
	LastError      *string       `json:"last_error,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	CreatedBy      string        `json:"created_by"`
}

// CatchupWindowSeconds exposes the API representation without making the DB
// representation part of callers' in-memory API.
func (r Report) CatchupWindowSeconds() int64 { return int64(r.CatchupWindow / time.Second) }

// CreateParams is the validated input for a new definition.
type CreateParams struct {
	Name           string `json:"name"`
	Prompt         string `json:"prompt"`
	Cadence        string `json:"cadence"`
	Weekday        *int   `json:"weekday,omitempty"`
	ScheduleHour   int    `json:"schedule_hour"`
	ScheduleMinute int    `json:"schedule_minute"`
	Timezone       string `json:"timezone"`
	DeliveryKind   string `json:"delivery_kind"`
	SlackTarget    string `json:"slack_target"`
	OwnerEmail     string `json:"owner_email"`
	CatchupSeconds int    `json:"catchup_window_seconds,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
	CreatedBy      string `json:"-"`
}

// Normalize trims every text field and lowercases the cadence, delivery
// kind, and emails, as the store saves them.
func (p *CreateParams) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.Prompt = strings.TrimSpace(p.Prompt)
	p.Cadence = strings.ToLower(strings.TrimSpace(p.Cadence))
	p.Timezone = strings.TrimSpace(p.Timezone)
	p.DeliveryKind = strings.ToLower(strings.TrimSpace(p.DeliveryKind))
	p.SlackTarget = strings.TrimSpace(p.SlackTarget)
	p.OwnerEmail = strings.ToLower(strings.TrimSpace(p.OwnerEmail))
	p.CreatedBy = strings.ToLower(strings.TrimSpace(p.CreatedBy))
}

func (p *CreateParams) normalizeAndValidate() error {
	p.Normalize()

	switch {
	case p.Name == "":
		return errors.New("name is required")
	case len(p.Name) > maxNameBytes:
		return fmt.Errorf("name exceeds %d bytes", maxNameBytes)
	case p.Prompt == "":
		return errors.New("prompt is required")
	case len(p.Prompt) > maxPromptBytes:
		return fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	case p.ScheduleHour < 0 || p.ScheduleHour > 23:
		return errors.New("schedule_hour must be between 0 and 23")
	case p.ScheduleMinute < 0 || p.ScheduleMinute > 59:
		return errors.New("schedule_minute must be between 0 and 59")
	case p.Timezone == "":
		return errors.New("timezone is required")
	case p.SlackTarget == "":
		return errors.New("slack_target is required")
	case p.OwnerEmail == "" || !strings.Contains(p.OwnerEmail, "@"):
		return errors.New("owner_email must be an email address")
	case p.CreatedBy == "":
		return errors.New("created_by is required")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	switch p.Cadence {
	case CadenceDaily, CadenceWeekdays:
		if p.Weekday != nil {
			return errors.New("weekday must be omitted for daily and weekdays reports")
		}
	case CadenceWeekly:
		if p.Weekday == nil || *p.Weekday < 1 || *p.Weekday > 7 {
			return errors.New("weekday must be 1 (Monday) through 7 (Sunday) for weekly reports")
		}
	default:
		return errors.New("cadence must be daily, weekdays, or weekly")
	}
	switch p.DeliveryKind {
	case DeliverySlackChannel:
		if !strings.HasPrefix(p.SlackTarget, "C") && !strings.HasPrefix(p.SlackTarget, "G") {
			return errors.New("slack_channel target must be a C… or G… channel ID")
		}
	case DeliverySlackDM:
		if !strings.HasPrefix(p.SlackTarget, "U") && !strings.HasPrefix(p.SlackTarget, "W") {
			return errors.New("slack_dm target must be a U… or W… user ID")
		}
	default:
		return errors.New("delivery_kind must be slack_channel or slack_dm")
	}
	if p.CatchupSeconds == 0 {
		p.CatchupSeconds = int(defaultCatchupWindow / time.Second)
	}
	if p.CatchupSeconds < 60 || time.Duration(p.CatchupSeconds)*time.Second > 30*24*time.Hour {
		return errors.New("catchup_window_seconds must be between 60 and 2592000")
	}
	return nil
}

// Run is one immutable occurrence plus its execution state.
type Run struct {
	ID               uuid.UUID  `json:"id"`
	ReportID         uuid.UUID  `json:"report_id"`
	Trigger          string     `json:"trigger"`
	ScheduledFor     time.Time  `json:"scheduled_for"`
	WindowStart      time.Time  `json:"window_start"`
	WindowEnd        time.Time  `json:"window_end"`
	PromptSnapshot   string     `json:"prompt_snapshot"`
	Status           string     `json:"status"`
	Attempts         int        `json:"attempts"`
	ClaimedAt        *time.Time `json:"claimed_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	AskSessionID     *uuid.UUID `json:"ask_session_id,omitempty"`
	AskAssistantTurn *int       `json:"ask_assistant_turn,omitempty"`
	Output           *string    `json:"output,omitempty"`
	DeliveryKind     *string    `json:"delivery_kind,omitempty"`
	SlackTarget      *string    `json:"slack_target,omitempty"`
	DeliveryTS       *string    `json:"delivery_ts,omitempty"`
	Error            *string    `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ClaimedRun bundles the current definition with a claimed occurrence.
type ClaimedRun struct {
	Report Report
	Run    Run
}
