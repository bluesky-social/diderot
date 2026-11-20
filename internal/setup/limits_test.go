package setup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// TestSaveModelsStoresAsksLimits: the Models page shows Ask's limits at
// their defaults until one is saved; a save outside a limit's range is
// refused by the field's name and saves nothing; and a new service tier,
// which is sent with every request, tests Ask's model again, as a new effort
// does.
func TestSaveModelsStoresAsksLimits(t *testing.T) {
	h := newHarness(t)
	defaults := settings.Models{}.AskLimits()
	assert.Equal(t, defaults, h.models(t).AskLimits)

	for want, change := range map[string]func(*settings.AskLimits){
		"Service tier must be one of default, fast, priority":                         func(l *settings.AskLimits) { l.ServiceTier = "turbo" },
		"Longest reply must be a number of tokens from 256 to 1000000":                func(l *settings.AskLimits) { l.MaxOutputTokens = 0 },
		"Time limit for one model request must be between 10 seconds and 1 day":       func(l *settings.AskLimits) { l.ModelTimeout = time.Second },
		"Time limit for one question must be between 10 seconds and 1 day":            func(l *settings.AskLimits) { l.TurnDeadline = 0 },
		"Investigation time must be between 10 seconds and 1 day":                     func(l *settings.AskLimits) { l.InvestigatorDeadline = 25 * time.Hour },
		"Time to write the investigation report must be between 10 seconds and 1 day": func(l *settings.AskLimits) { l.InvestigatorGrace = -time.Minute },
	} {
		limits := defaults
		change(&limits)
		err := h.saveModels(t, ModelsUpdate{AskLimits: &limits})
		require.ErrorIs(t, err, settings.ErrInvalid, want)
		assert.ErrorContains(t, err, want)
	}
	assert.Equal(t, int64(1), h.models(t).Version, "nothing is saved")
	assert.Empty(t, h.catalog.tests, "a refused form calls no model")

	// Limits that send nothing new to the model call none.
	require.NoError(t, h.saveModels(t, ModelsUpdate{Roles: map[string]string{"ask": "p/big"}}))
	h.catalog.tests = nil
	limits := defaults
	limits.MaxOutputTokens, limits.ModelTimeout, limits.TurnDeadline = 8192, 90*time.Second, 5*time.Minute
	limits.InvestigatorDeadline, limits.InvestigatorGrace = time.Minute, 30*time.Second
	require.NoError(t, h.saveModels(t, ModelsUpdate{AskLimits: &limits}))
	assert.Empty(t, h.catalog.tests)
	assert.Equal(t, limits, h.models(t).AskLimits)

	// A service tier tests Ask's model with it; taking it off again does not.
	limits.ServiceTier = "fast"
	require.NoError(t, h.saveModels(t, ModelsUpdate{AskLimits: &limits}))
	assert.Equal(t, []string{"ask p/big"}, h.catalog.tests)
	assert.Equal(t, "fast", h.catalog.tier)
	assert.Equal(t, limits, h.models(t).AskLimits)
	h.catalog.tests = nil
	limits.ServiceTier = ""
	require.NoError(t, h.saveModels(t, ModelsUpdate{AskLimits: &limits}))
	assert.Empty(t, h.catalog.tests)

	// A model that refuses the tier saves nothing.
	h.catalog.fails["p/big"] = "a service tier needs a model that answers on Responses"
	limits.ServiceTier = "priority"
	err := h.saveModels(t, ModelsUpdate{AskLimits: &limits})
	var failed *ModelTestError
	require.ErrorAs(t, err, &failed)
	assert.Empty(t, h.models(t).AskLimits.ServiceTier)

	// A save that carries no limits keeps them.
	delete(h.catalog.fails, "p/big")
	turns := 12
	require.NoError(t, h.saveModels(t, ModelsUpdate{AskMaxToolTurns: &turns}))
	assert.Equal(t, 8192, h.models(t).AskLimits.MaxOutputTokens)
}

// TestSummaryScheduleIsSavedAtItsVersion: when sessions are summarized is
// read at the default until saved, validated, and saved against the version
// the page loaded.
func TestSummaryScheduleIsSavedAtItsVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	row, err := h.service.SessionSummaries(ctx)
	require.NoError(t, err)
	assert.Equal(t, settings.DefaultSummarySchedule(), row.EffectiveSchedule())

	schedule := settings.SummarySchedule{QuietFor: settings.Duration(time.Minute), MinAddedUserTurns: 1, MaxPendingFor: settings.Duration(time.Hour)}
	bad := schedule
	bad.MinAddedRecords = -1
	_, err = h.service.SaveSummarySchedule(ctx, operator, row.Version, bad)
	require.ErrorIs(t, err, settings.ErrInvalid)

	saved, err := h.service.SaveSummarySchedule(ctx, operator, row.Version, schedule)
	require.NoError(t, err)
	assert.Equal(t, schedule, saved.EffectiveSchedule())
	assert.Equal(t, "op@example.com", saved.UpdatedBy)
	_, err = h.service.SaveSummarySchedule(ctx, operator, row.Version, schedule)
	require.ErrorIs(t, err, settings.ErrConflict)
}
