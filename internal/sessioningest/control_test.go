package sessioningest

import (
	"context"
	"testing"
)

// TestSummaryControlStateSaysWhatStopsSummaries: summaries run only when a
// model can write them and they are not paused; otherwise the state names
// the first of those that fails.
func TestSummaryControlStateSaysWhatStopsSummaries(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	const noModel = "No model can write session summaries: set ANTHROPIC_API_KEY."
	for _, tc := range []struct {
		name    string
		noModel string
		paused  bool
		blocked SummaryBlock
		reason  string
	}{
		{"running", "", false, "", ""},
		{"no model", noModel, true, SummaryNoModel, noModel},
		{"no model, not paused", noModel, false, SummaryNoModel, noModel},
		{"paused", "", true, SummaryPaused, "Session summaries are paused: resume them on the Jobs and maintenance page, under Operations."},
	} {
		if err := store.SetSessionSummarizationEnabled(ctx, !tc.paused); err != nil {
			t.Fatal(err)
		}
		state, err := NewSummaryControl(store, func() string { return tc.noModel }).State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Blocked != tc.blocked || state.Reason != tc.reason || state.EffectiveEnabled != (tc.blocked == "") {
			t.Errorf("%s: state = %+v, want blocked %q, reason %q", tc.name, state, tc.blocked, tc.reason)
		}
		if state.Enabled == tc.paused || state.ModelAvailable != (tc.noModel == "") {
			t.Errorf("%s: facts = %+v", tc.name, state)
		}
	}
}
