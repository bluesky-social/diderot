package ask

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateReasoningEffortSchedule(t *testing.T) {
	valid := ModelOverrides{ReasoningEffortSchedule: &ReasoningEffortSchedule{
		AfterToolRounds: 2,
		Effort:          "high",
	}}
	require.NoError(t, ValidateModelOverrides(registryModel(t, "gpt-5.6-sol:direct"), valid))

	for _, tc := range []struct {
		name      string
		modelID   string
		schedule  ReasoningEffortSchedule
		wantError string
	}{
		{name: "zero threshold", modelID: "gpt-5.6-sol:direct", schedule: ReasoningEffortSchedule{Effort: "high"}, wantError: "at least 1"},
		{name: "negative threshold", modelID: "gpt-5.6-sol:direct", schedule: ReasoningEffortSchedule{AfterToolRounds: -1, Effort: "high"}, wantError: "at least 1"},
		{name: "empty effort", modelID: "gpt-5.6-sol:direct", schedule: ReasoningEffortSchedule{AfterToolRounds: 2}, wantError: "must not be empty"},
		{name: "unsupported effort", modelID: "gpt-5.6-sol:direct", schedule: ReasoningEffortSchedule{AfterToolRounds: 2, Effort: "ultra"}, wantError: "does not support"},
		{name: "messages model", modelID: "claude-opus-5:main", schedule: ReasoningEffortSchedule{AfterToolRounds: 2, Effort: "high"}, wantError: "require a Responses model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateModelOverrides(registryModel(t, tc.modelID), ModelOverrides{ReasoningEffortSchedule: &tc.schedule})
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}
