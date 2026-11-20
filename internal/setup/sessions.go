package setup

import (
	"context"

	"github.com/bluesky-social/diderot/internal/settings"
)

// SessionSummaries reads when agent sessions are summarized from the
// database (not the cache, so a page shows the version it posts back).
func (s *Service) SessionSummaries(ctx context.Context) (settings.SessionSummariesRow, error) {
	return s.settings.ReadSessionSummaries(ctx)
}

// SaveSummarySchedule saves when agent sessions are summarized, at version.
// The leader reads it from its settings cache for each session it
// considers, so it applies within one settings poll.
func (s *Service) SaveSummarySchedule(ctx context.Context, actor Actor, version int64, schedule settings.SummarySchedule) (settings.SessionSummariesRow, error) {
	if err := schedule.Validate(); err != nil {
		return settings.SessionSummariesRow{}, err
	}
	return s.settings.UpdateSessionSummaries(ctx, version, actor.settings(), "operations.session_summaries.schedule",
		func(summaries *settings.SessionSummaries) error {
			summaries.Schedule = &schedule
			return nil
		})
}
