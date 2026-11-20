package settings

import (
	"context"
	"fmt"
	"time"
)

// SessionSummariesConnector is the pseudo-connector row that holds when
// agent sessions are summarized, edited on the Jobs and maintenance page.
const SessionSummariesConnector = "session_summaries"

// The most the Jobs and maintenance page accepts for the two waits of a
// SummarySchedule.
const (
	MaxSummaryQuietFor   = 7 * 24 * time.Hour
	MaxSummaryPendingFor = 30 * 24 * time.Hour
)

// SessionSummaries is the session_summaries row's settings document.
type SessionSummaries struct {
	// Schedule is nil until an administrator saves one, and the default
	// schedule applies.
	Schedule *SummarySchedule `json:"schedule,omitempty"`
}

// SummarySchedule decides when a finished agent session is summarized, and
// when one already summarized is summarized again. A zero is a value an
// administrator may choose, not a default.
type SummarySchedule struct {
	// QuietFor is how long a session must have gone without a change.
	QuietFor Duration `json:"quiet_for"`
	// A session already summarized is summarized again once it has grown by
	// any one of these.
	MinAddedUserTurns int   `json:"min_added_user_turns"`
	MinAddedRecords   int   `json:"min_added_records"`
	MinAddedBytes     int64 `json:"min_added_bytes"`
	// MaxPendingFor is how long a changed session waits at most, however
	// little it grew and whether or not it is quiet.
	MaxPendingFor Duration `json:"max_pending_for"`
}

// DefaultSummarySchedule is the schedule until an administrator saves one.
func DefaultSummarySchedule() SummarySchedule {
	return SummarySchedule{
		QuietFor:          Duration(10 * time.Minute),
		MinAddedUserTurns: 2,
		MinAddedRecords:   12,
		MinAddedBytes:     32768,
		MaxPendingFor:     Duration(6 * time.Hour),
	}
}

// Validate checks a schedule an administrator entered. Its messages are
// written for the page.
func (s SummarySchedule) Validate() error {
	switch {
	case s.QuietFor < 0 || time.Duration(s.QuietFor) > MaxSummaryQuietFor:
		return fmt.Errorf("%w: the quiet time must be between 0 seconds and %s", ErrInvalid, FormatDuration(MaxSummaryQuietFor))
	case s.MaxPendingFor < 0 || time.Duration(s.MaxPendingFor) > MaxSummaryPendingFor:
		return fmt.Errorf("%w: the longest wait must be between 0 seconds and %s", ErrInvalid, FormatDuration(MaxSummaryPendingFor))
	case s.MinAddedUserTurns < 0 || s.MinAddedRecords < 0 || s.MinAddedBytes < 0:
		return fmt.Errorf("%w: the amounts of new content must be 0 or more", ErrInvalid)
	}
	return nil
}

// EffectiveSchedule is the saved schedule, or the default while none is
// saved or the saved one is not valid.
func (s SessionSummaries) EffectiveSchedule() SummarySchedule {
	if s.Schedule == nil || s.Schedule.Validate() != nil {
		return DefaultSummarySchedule()
	}
	return *s.Schedule
}

// SessionSummariesRow is the session_summaries row with its concurrency
// token.
type SessionSummariesRow struct {
	SessionSummaries
	Version   int64     `json:"version"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func sessionSummariesRow(d document[SessionSummaries]) SessionSummariesRow {
	return SessionSummariesRow{SessionSummaries: d.doc, Version: d.version, UpdatedBy: d.updatedBy, UpdatedAt: d.updatedAt}
}

// storeSessionSummaries caches row unless the cache already holds a newer
// version.
func (s *Store) storeSessionSummaries(row SessionSummariesRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.Version >= s.sessionSummaries.Version {
		s.sessionSummaries = row
	}
}

// SessionSummaries returns the cached session_summaries row.
func (s *Store) SessionSummaries() SessionSummariesRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionSummaries
}

// ReadSessionSummaries reads the session_summaries row from the database,
// bypassing the cache.
func (s *Store) ReadSessionSummaries(ctx context.Context) (SessionSummariesRow, error) {
	row, err := readDocument[SessionSummaries](ctx, s.pool, SessionSummariesConnector, "")
	return sessionSummariesRow(row), err
}

// UpdateSessionSummaries is UpdateOrg for the session_summaries row.
func (s *Store) UpdateSessionSummaries(ctx context.Context, version int64, actor Actor, action string, change func(*SessionSummaries) error) (SessionSummariesRow, error) {
	row, err := updateDocument(ctx, s.pool, SessionSummariesConnector, &version, actor, action, change)
	if err != nil {
		return SessionSummariesRow{}, err
	}
	s.storeSessionSummaries(sessionSummariesRow(row))
	return sessionSummariesRow(row), nil
}
