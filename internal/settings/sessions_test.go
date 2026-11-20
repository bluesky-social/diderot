package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionSummariesRowStartsOnTheDefaultScheduleAndReachesEveryReplica: an
// install gets an empty session_summaries row, on which the default schedule
// applies; a saved schedule is versioned and audited, keeps the zeros an
// administrator chose, and reaches another replica's cache on its poll.
func TestSessionSummariesRowStartsOnTheDefaultScheduleAndReachesEveryReplica(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	row, err := store.ReadSessionSummaries(ctx)
	require.NoError(t, err)
	assert.Nil(t, row.Schedule)
	assert.Equal(t, DefaultSummarySchedule(), row.EffectiveSchedule())
	assert.Equal(t, row, store.SessionSummaries())

	other := NewStore(pool, nil)
	require.NoError(t, other.Load(ctx), "a second replica's Load keeps the row")
	actor := Actor{Email: "op@example.com", RequestID: "req-3"}
	saved := SummarySchedule{QuietFor: Duration(5 * time.Minute), MinAddedRecords: 40, MaxPendingFor: Duration(2 * time.Hour)}
	updated, err := other.UpdateSessionSummaries(ctx, row.Version, actor, "operations.session_summaries.schedule", func(summaries *SessionSummaries) error {
		summaries.Schedule = &saved
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, row.Version+1, updated.Version)
	assert.Equal(t, saved, updated.EffectiveSchedule(), "a zero is the administrator's, not a default")
	assert.Equal(t, updated, other.SessionSummaries(), "the writer's own cache updates at once")
	_, err = other.UpdateSessionSummaries(ctx, row.Version, actor, "operations.session_summaries.schedule", func(*SessionSummaries) error { return nil })
	assert.ErrorIs(t, err, ErrConflict)

	var action string
	var details []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT action, details FROM admin_audit_log WHERE target = 'connector_settings:session_summaries' ORDER BY id DESC LIMIT 1`).Scan(&action, &details))
	assert.Equal(t, "operations.session_summaries.schedule", action)
	assert.JSONEq(t, `{"version": 2, "changes": {"schedule": {"old": null, "new": {
		"quiet_for": "5m0s", "min_added_user_turns": 0, "min_added_records": 40, "min_added_bytes": 0, "max_pending_for": "2h0m0s"}}}}`, string(details))

	assert.Nil(t, store.SessionSummaries().Schedule, "the cache serves the old row until the poll")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(runCtx, 10*time.Millisecond)
	}()
	require.Eventually(t, func() bool { return store.SessionSummaries().Version == updated.Version }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, saved, store.SessionSummaries().EffectiveSchedule())
}

func TestSummaryScheduleValidate(t *testing.T) {
	valid := DefaultSummarySchedule()
	require.NoError(t, valid.Validate())
	require.NoError(t, SummarySchedule{}.Validate(), "every zero is allowed: summarize every change at once")
	for name, change := range map[string]func(*SummarySchedule){
		"the quiet time must be between 0 seconds and 7 days":    func(s *SummarySchedule) { s.QuietFor = Duration(MaxSummaryQuietFor + time.Second) },
		"the quiet time must be":                                 func(s *SummarySchedule) { s.QuietFor = -1 },
		"the longest wait must be between 0 seconds and 30 days": func(s *SummarySchedule) { s.MaxPendingFor = Duration(MaxSummaryPendingFor + time.Second) },
		"the amounts of new content must be 0 or more":           func(s *SummarySchedule) { s.MinAddedBytes = -1 },
	} {
		schedule := valid
		change(&schedule)
		err := schedule.Validate()
		require.ErrorIs(t, err, ErrInvalid, name)
		assert.ErrorContains(t, err, name)
	}
	// A saved schedule that is not valid, which only a hand edit of the row
	// makes, is not used.
	bad := SummarySchedule{MinAddedRecords: -1}
	assert.Equal(t, valid, SessionSummaries{Schedule: &bad}.EffectiveSchedule())
}

// TestAskLimitsFallBackOutOfRange: each of Ask's limits is its saved value
// while that is in the range the Models page accepts, and its default
// otherwise, so a server with nothing saved, or a row edited by hand, runs
// with sensible limits.
func TestAskLimitsFallBackOutOfRange(t *testing.T) {
	defaults := AskLimits{
		MaxOutputTokens: 32768, ModelTimeout: 5 * time.Minute, TurnDeadline: 10 * time.Minute,
		InvestigatorDeadline: 4 * time.Minute, InvestigatorGrace: 2 * time.Minute,
	}
	assert.Equal(t, defaults, Models{}.AskLimits())
	assert.Equal(t, defaults, Models{
		AskMaxOutputTokens: MaxAskMaxOutputTokens + 1, AskModelTimeout: Duration(time.Second),
		AskTurnDeadline: Duration(25 * time.Hour), AskInvestigatorDeadline: -1, AskInvestigatorGrace: Duration(MinAskDuration - 1),
	}.AskLimits())

	saved := Models{
		AskServiceTier: "fast", AskMaxOutputTokens: 8192, AskModelTimeout: Duration(90 * time.Second),
		AskTurnDeadline: Duration(MaxAskDuration), AskInvestigatorDeadline: Duration(MinAskDuration), AskInvestigatorGrace: Duration(time.Minute),
	}
	assert.Equal(t, AskLimits{
		ServiceTier: "fast", MaxOutputTokens: 8192, ModelTimeout: 90 * time.Second, TurnDeadline: 24 * time.Hour,
		InvestigatorDeadline: 10 * time.Second, InvestigatorGrace: time.Minute,
	}, saved.AskLimits())

	// The row stores each time as a connector's duration field does, and
	// leaves an unset limit out.
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ask_service_tier": "fast", "ask_max_output_tokens": 8192, "ask_model_timeout": "1m30s",
		"ask_turn_deadline": "24h0m0s", "ask_investigator_deadline": "10s", "ask_investigator_grace": "1m0s"}`, string(encoded))
	var decoded Models
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, saved, decoded)
	encoded, err = json.Marshal(Models{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(encoded))
	assert.Error(t, json.Unmarshal([]byte(`{"ask_model_timeout": "soon"}`), &decoded))
}
