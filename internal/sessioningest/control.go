package sessioningest

import (
	"context"
	"fmt"
)

// SummaryBlock is what stops new agent-session summaries.
type SummaryBlock string

const (
	// SummaryNoModel is no model for the summary jobs.
	SummaryNoModel SummaryBlock = "model"
	// SummaryPaused is an administrator's pause, on the Jobs and maintenance
	// page.
	SummaryPaused SummaryBlock = "paused"
)

// SummaryControlState says whether new agent-session summaries run and, when
// they don't, the first thing that stops them. Effective is the only state
// that permits paid summary or title inference.
type SummaryControlState struct {
	// Enabled is the administrator's switch: false is paused.
	Enabled bool `json:"enabled"`
	// ModelAvailable reports a model for both summary jobs.
	ModelAvailable   bool `json:"model_available"`
	EffectiveEnabled bool `json:"effective_enabled"`
	// Blocked is what stops summaries, empty while they run.
	Blocked SummaryBlock `json:"blocked,omitempty"`
	// Reason says it, and what to do, to an administrator; empty while they
	// run.
	Reason string `json:"reason,omitempty"`
}

// OwnerReason says to a session's owner, who can't change it, why their
// sessions that aren't indexed are stored but not searchable; empty while
// summaries run.
func (s SummaryControlState) OwnerReason() string {
	switch s.Blocked {
	case SummaryNoModel:
		return "Sessions that aren't indexed are stored, but not searchable: this server isn't set up to summarize them. An administrator can set it up."
	case SummaryPaused:
		return "Sessions that aren't indexed are stored, but not searchable until an administrator resumes summaries."
	}
	return ""
}

// SummaryControl is the runtime gate for native session summarization.
// Workers read it immediately before starting each candidate; no mutable
// configuration or background control loop is involved.
type SummaryControl struct {
	Store *Store
	// NoModel returns "" when a model can write summaries, and otherwise
	// says what to set so one can. Nil means a model can.
	NoModel func() string
}

// NewSummaryControl reads the administrator's switch from store. noModel
// returns "" while a model serves both summary jobs, and otherwise a
// sentence saying what to set so one can. It is read each time, because the
// Models page changes it; nil means a model can.
func NewSummaryControl(store *Store, noModel func() string) SummaryControl {
	return SummaryControl{Store: store, NoModel: noModel}
}

func (c SummaryControl) State(ctx context.Context) (SummaryControlState, error) {
	if c.Store == nil {
		return SummaryControlState{}, fmt.Errorf("sessioningest: summary control store not configured")
	}
	enabled, err := c.Store.SessionSummarizationEnabled(ctx)
	if err != nil {
		return SummaryControlState{}, err
	}
	noModel := ""
	if c.NoModel != nil {
		noModel = c.NoModel()
	}
	state := SummaryControlState{Enabled: enabled, ModelAvailable: noModel == ""}
	switch {
	case !state.ModelAvailable:
		state.Blocked, state.Reason = SummaryNoModel, noModel
	case !state.Enabled:
		state.Blocked = SummaryPaused
		state.Reason = "Session summaries are paused: resume them on the Jobs and maintenance page, under Operations."
	default:
		state.EffectiveEnabled = true
	}
	return state, nil
}

func (c SummaryControl) SetEnabled(ctx context.Context, enabled bool) error {
	if c.Store == nil {
		return fmt.Errorf("sessioningest: summary control store not configured")
	}
	return c.Store.SetSessionSummarizationEnabled(ctx, enabled)
}
