package asktool

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/settings"
)

// fakeTool is a tool with only a name and a title. Its Tools records the
// settings current returns.
type fakeTool struct {
	name, title string
	seen        *[]string
}

func (f fakeTool) Name() string                 { return f.name }
func (fakeTool) Fields() []settings.Field       { return nil }
func (fakeTool) Validate(settings.Values) error { return nil }
func (fakeTool) Test(context.Context, settings.Resolved) settings.TestReport {
	return settings.TestReport{}
}
func (f fakeTool) Info() Info {
	if f.title == "" {
		return Info{}
	}
	return Info{Title: f.title, Step: f.name, Summary: "Gives Ask " + f.title + "."}
}

// steplessTool is a tool with a title but no setup step.
type steplessTool struct{ fakeTool }

func (s steplessTool) Info() Info { return Info{Title: s.title, Summary: "Gives Ask nothing."} }
func (f fakeTool) Tools(current Current) []ask.RegisteredTool {
	resolved, ok := current()
	if ok {
		*f.seen = append(*f.seen, resolved.Connector)
	}
	return []ask.RegisteredTool{{}}
}

type fakeSettings map[string]settings.Resolved

func (f fakeSettings) Connector(name string) (settings.Resolved, bool) {
	resolved, ok := f[name]
	return resolved, ok
}

func TestNewRegistryRefusesIncompleteTools(t *testing.T) {
	require.Panics(t, func() { NewRegistry(fakeTool{name: "web"}) }, "a tool needs a title")
	require.Panics(t, func() { NewRegistry(steplessTool{fakeTool{name: "web", title: "Web"}}) }, "a tool needs a setup step")
	require.Panics(t, func() {
		NewRegistry(fakeTool{name: "web", title: "Web"}, fakeTool{name: "web", title: "Other web"})
	}, "two tools can't share a name")
}

// TestRegistrationsFollowEachToolsConnector: each tool's tools read its own
// connector's settings, in registration order.
func TestRegistrationsFollowEachToolsConnector(t *testing.T) {
	var seen []string
	registry := NewRegistry(fakeTool{name: "zeta", title: "Zeta", seen: &seen}, fakeTool{name: "alpha", title: "Alpha", seen: &seen})
	require.Equal(t, []string{"zeta", "alpha"}, registry.Names())
	store := fakeSettings{
		"zeta":  settings.NewResolved("zeta", nil, nil),
		"alpha": settings.NewResolved("alpha", nil, nil),
	}
	require.Len(t, registry.Registrations(store), 2)
	require.Equal(t, []string{"zeta", "alpha"}, seen)
	tool, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, "Alpha", tool.Info().Title)
	_, ok = registry.Lookup("missing")
	require.False(t, ok)
	require.Empty(t, Registry{}.Names(), "the zero registry has no tools")
}

// TestBuiltRebuildsOnlyWhenTheSettingsChange: one build serves every call
// until the connector's version changes, and nothing is built while it is
// off.
func TestBuiltRebuildsOnlyWhenTheSettingsChange(t *testing.T) {
	var mu sync.Mutex
	var enabled *settings.Resolved
	current := func() (settings.Resolved, bool) {
		mu.Lock()
		defer mu.Unlock()
		if enabled == nil {
			return settings.Resolved{}, false
		}
		return *enabled, true
	}
	set := func(version int64, key string) {
		mu.Lock()
		defer mu.Unlock()
		resolved := settings.NewResolved("web", nil, map[string]string{"api_key": key})
		resolved.Version = version
		enabled = &resolved
	}
	builds := 0
	client := Built(current, func(r settings.Resolved) string {
		builds++
		return r.Secret("api_key")
	})

	got, ok := client()
	require.False(t, ok)
	require.Empty(t, got)
	require.Zero(t, builds, "nothing is built while the connector is off")

	set(3, "first")
	for range 3 {
		got, ok = client()
		require.True(t, ok)
		require.Equal(t, "first", got)
	}
	require.Equal(t, 1, builds)

	set(4, "second")
	got, _ = client()
	require.Equal(t, "second", got)
	require.Equal(t, 2, builds)

	mu.Lock()
	enabled = nil
	mu.Unlock()
	_, ok = client()
	require.False(t, ok, "off again")
}
