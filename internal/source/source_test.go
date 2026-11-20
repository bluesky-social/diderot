package source

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// fakeSource is a source with only a name and facts.
type fakeSource struct {
	name string
	info Info
}

func (f fakeSource) Name() string                 { return f.name }
func (fakeSource) Fields() []settings.Field       { return nil }
func (fakeSource) Validate(settings.Values) error { return nil }
func (fakeSource) Test(context.Context, settings.Resolved) settings.TestReport {
	return settings.TestReport{}
}
func (f fakeSource) Info() Info                                       { return f.info }
func (fakeSource) Run(context.Context, Deps, settings.Resolved) error { return nil }
func (fakeSource) Expand(context.Context, Deps, int64, []string) (Expanded, error) {
	return Expanded{}, ErrNotExpandable
}

func completeInfo(eventSource string) Info {
	return Info{
		EventSource: eventSource, Title: "Example", ResultNoun: "Example", Summary: "Example records.", ToolDescription: "example records.",
		ActorRole: "author", IdentityNamespace: func(string) string { return "example_user" },
	}
}

// TestNewRegistryRefusesIncompleteSources: every consumer of the registry
// relies on these facts, so a source without one doesn't register.
func TestNewRegistryRefusesIncompleteSources(t *testing.T) {
	for name, mutate := range map[string]func(*Info){
		"no event source":       func(info *Info) { info.EventSource = "" },
		"bad event source":      func(info *Info) { info.EventSource = "Example-Source" },
		"no title":              func(info *Info) { info.Title = "" },
		"no result noun":        func(info *Info) { info.ResultNoun = "" },
		"no summary":            func(info *Info) { info.Summary = "" },
		"no tool description":   func(info *Info) { info.ToolDescription = "" },
		"multiline description": func(info *Info) { info.ToolDescription = "one\ntwo" },
		"untrimmed description": func(info *Info) { info.ToolDescription = "records. " },
		"no actor role":         func(info *Info) { info.ActorRole = "" },
		"no identity namespace": func(info *Info) { info.IdentityNamespace = nil },
	} {
		t.Run(name, func(t *testing.T) {
			info := completeInfo("example")
			mutate(&info)
			require.Panics(t, func() { NewRegistry(fakeSource{name: "example", info: info}) })
		})
	}
	require.NotPanics(t, func() {
		info := completeInfo("example")
		info.ActorRole, info.IdentityNamespace = "", nil
		NewRegistry(fakeSource{name: "example", info: info})
	}, "a source whose events have no actor sets neither")
	require.Panics(t, func() {
		NewRegistry(fakeSource{name: "one", info: completeInfo("example")}, fakeSource{name: "two", info: completeInfo("example")})
	}, "two sources can't share an event source")
	require.Panics(t, func() {
		NewRegistry(fakeSource{name: "one", info: completeInfo("first")}, fakeSource{name: "one", info: completeInfo("second")})
	}, "two sources can't share a name")
}

func TestRegistryKeepsRegistrationOrder(t *testing.T) {
	registry := NewRegistry(
		fakeSource{name: "zeta", info: completeInfo("zeta_events")},
		fakeSource{name: "alpha", info: completeInfo("alpha_events")},
	)
	require.Equal(t, []string{"zeta", "alpha"}, registry.Names())
	require.Equal(t, []string{"zeta_events", "alpha_events"}, registry.Infos().EventSources())
	src, ok := registry.Lookup("alpha_events")
	require.True(t, ok)
	require.Equal(t, "alpha", src.Name())
	_, ok = registry.Lookup("missing")
	require.False(t, ok)
	require.Equal(t, "Example", registry.Infos().Of("zeta_events").Title)
	require.Equal(t, Info{EventSource: "missing"}, registry.Infos().Of("missing"))
	require.Empty(t, Registry{}.Names(), "the zero registry has no sources")
}

func TestPromptListIsOneLinePerSource(t *testing.T) {
	registry := NewRegistry(
		fakeSource{name: "zeta", info: completeInfo("zeta_events")},
		fakeSource{name: "alpha", info: completeInfo("alpha_events")},
	)
	require.Equal(t, "- `zeta_events` — example records.\n- `alpha_events` — example records.", registry.Infos().PromptList())
}

// backfillingSource is a source with a backfill.
type backfillingSource struct {
	fakeSource
	backfill Backfill
}

func (s backfillingSource) Backfill() Backfill { return s.backfill }
func (backfillingSource) RequestBackfill(context.Context, Deps, settings.Values) (time.Time, error) {
	return time.Time{}, nil
}

// TestNewRegistryRefusesIncompleteBackfills: the page generates a backfill's
// form from its words and fields, so each must be there and every field an
// optional text one.
func TestNewRegistryRefusesIncompleteBackfills(t *testing.T) {
	complete := func() Backfill {
		return Backfill{Summary: "Re-read everything.", Confirm: "Re-read", Action: "Backfill",
			Fields: []settings.Field{{Key: "project", Label: "Project", Kind: settings.KindText}}}
	}
	register := func(backfill Backfill) {
		NewRegistry(backfillingSource{fakeSource: fakeSource{name: "notes", info: completeInfo("notes")}, backfill: backfill})
	}
	require.NotPanics(t, func() { register(complete()) })
	for name, mutate := range map[string]func(*Backfill){
		"no summary":       func(b *Backfill) { b.Summary = "" },
		"no confirmation":  func(b *Backfill) { b.Confirm = "" },
		"no action":        func(b *Backfill) { b.Action = "" },
		"unlabelled field": func(b *Backfill) { b.Fields[0].Label = "" },
		"required field":   func(b *Backfill) { b.Fields[0].Required = true },
		"list field":       func(b *Backfill) { b.Fields[0].Kind = settings.KindList },
		"flag-unsafe key":  func(b *Backfill) { b.Fields[0].Key = "Project ID" },
		"repeated key":     func(b *Backfill) { b.Fields = append(b.Fields, b.Fields[0]) },
		"response's key":   func(b *Backfill) { b.Fields[0].Key = "requested_at" },
	} {
		t.Run(name, func(t *testing.T) {
			backfill := complete()
			mutate(&backfill)
			require.Panics(t, func() { register(backfill) })
		})
	}
	registry := NewRegistry(fakeSource{name: "plain", info: completeInfo("plain")},
		backfillingSource{fakeSource: fakeSource{name: "notes", info: completeInfo("notes")}, backfill: complete()})
	require.Len(t, registry.Backfillers(), 1)
	require.Equal(t, "notes", registry.Backfillers()[0].Name())
	src, ok := registry.Named("plain")
	require.True(t, ok)
	require.Equal(t, "plain", src.Name())
}
