// Package asktool defines a tool, a connector that gives Ask tools while it
// is enabled, and the registry of the tools a build knows.
package asktool

import (
	"fmt"
	"sync"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/settings"
)

// Tool is a connector that gives Ask one or more tools while it is enabled.
type Tool interface {
	settings.Definition

	// Info is fixed facts about the tool.
	Info() Info

	// Tools builds the Ask tools it gives over current, the connector's
	// settings as the cache holds them now. Ask registers them once; a run
	// offers them only if the connector is enabled as the run starts, and
	// each call reads current again, so a change applies without a restart.
	Tools(current Current) []ask.RegisteredTool
}

// Info is fixed facts about a tool.
type Info struct {
	// Title is what pages call it ("Web search").
	Title string
	// Step is the ID of its step on the setup checklist ("web-search"), and
	// Summary that step's sentence on what it does ("Lets Ask search the
	// web.").
	Step, Summary string
}

// Current returns a connector's settings as the cache holds them now, and
// false while it is disabled.
type Current func() (settings.Resolved, bool)

// Settings is the settings cache: a connector's settings while it is
// enabled.
type Settings interface {
	Connector(name string) (settings.Resolved, bool)
}

// Registry is the tools a build knows, in the order Ask lists their tools
// and pages list them. The zero value has none.
type Registry struct {
	tools []Tool
}

// NewRegistry checks and registers tools. A reused name or an empty Info
// field panics: these are programming errors.
func NewRegistry(tools ...Tool) Registry {
	names := map[string]bool{}
	for _, tool := range tools {
		name := tool.Name()
		if names[name] {
			panic(fmt.Sprintf("asktool: %q is registered twice", name))
		}
		names[name] = true
		if info := tool.Info(); info.Title == "" || info.Step == "" || info.Summary == "" {
			panic(fmt.Sprintf("asktool: %s: Info needs a title, a setup step, and a summary", name))
		}
	}
	return Registry{tools: tools}
}

// Tools returns every registered tool.
func (r Registry) Tools() []Tool { return r.tools }

// Names returns every registered tool's connector name.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for _, tool := range r.tools {
		names = append(names, tool.Name())
	}
	return names
}

// Lookup returns the tool named name.
func (r Registry) Lookup(name string) (Tool, bool) {
	for _, tool := range r.tools {
		if tool.Name() == name {
			return tool, true
		}
	}
	return nil, false
}

// Registrations returns every registered tool's Ask tools, in registration
// order, each over its connector's settings as store caches them.
func (r Registry) Registrations(store Settings) []ask.RegisteredTool {
	var registrations []ask.RegisteredTool
	for _, tool := range r.tools {
		name := tool.Name()
		registrations = append(registrations, tool.Tools(func() (settings.Resolved, bool) {
			return store.Connector(name)
		})...)
	}
	return registrations
}

// Built returns what build makes of current's settings, and false while the
// connector is disabled. It builds again only when the settings change, which
// changes their version, so a client is shared by every call between changes.
func Built[T any](current Current, build func(settings.Resolved) T) func() (T, bool) {
	var mu sync.Mutex
	var built T
	var version int64
	var have bool
	return func() (T, bool) {
		resolved, ok := current()
		if !ok {
			var zero T
			return zero, false
		}
		mu.Lock()
		defer mu.Unlock()
		if !have || resolved.Version != version {
			built, version, have = build(resolved), resolved.Version, true
		}
		return built, true
	}
}
