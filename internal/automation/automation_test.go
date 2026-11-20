package automation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// fakeAutomation is an automation with only a name and a title.
type fakeAutomation struct {
	name, title string
}

func (f fakeAutomation) Name() string           { return f.name }
func (fakeAutomation) Fields() []settings.Field { return nil }
func (fakeAutomation) Validate(settings.Values) error {
	return nil
}
func (fakeAutomation) Test(context.Context, settings.Resolved) settings.TestReport {
	return settings.TestReport{}
}
func (f fakeAutomation) Info() Info {
	if f.title == "" {
		return Info{}
	}
	return Info{Title: f.title, Step: f.name, Summary: "Answers " + f.title + "."}
}
func (fakeAutomation) Run(context.Context, Deps, settings.Resolved) error { return nil }

// summarylessAutomation is an automation with a setup step but no summary.
type summarylessAutomation struct{ fakeAutomation }

func (s summarylessAutomation) Info() Info { return Info{Title: s.title, Step: s.name} }

func TestNewRegistryRefusesIncompleteAutomations(t *testing.T) {
	require.Panics(t, func() { NewRegistry(fakeAutomation{name: "pages"}) }, "an automation needs a title")
	require.Panics(t, func() { NewRegistry(summarylessAutomation{fakeAutomation{name: "pages", title: "Pages"}}) },
		"an automation needs a setup summary")
	require.Panics(t, func() {
		NewRegistry(fakeAutomation{name: "pages", title: "Pages"}, fakeAutomation{name: "pages", title: "Other pages"})
	}, "two automations can't share a name")
}

func TestRegistryKeepsRegistrationOrder(t *testing.T) {
	registry := NewRegistry(fakeAutomation{name: "zeta", title: "Zeta"}, fakeAutomation{name: "alpha", title: "Alpha"})
	require.Equal(t, []string{"zeta", "alpha"}, registry.Names())
	automation, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, "Alpha", automation.Info().Title)
	_, ok = registry.Lookup("missing")
	require.False(t, ok)
	require.Empty(t, Registry{}.Names(), "the zero registry has no automations")
}
