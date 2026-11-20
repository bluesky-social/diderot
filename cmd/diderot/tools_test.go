package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/sourcebot"
	"github.com/bluesky-social/diderot/internal/tavily"
)

// fakeConnectors is the settings cache with the enabled connectors' settings.
type fakeConnectors struct {
	mu      sync.Mutex
	enabled map[string]settings.Resolved
}

func (f *fakeConnectors) Connector(name string) (settings.Resolved, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resolved, ok := f.enabled[name]
	return resolved, ok
}

func (f *fakeConnectors) set(name string, resolved *settings.Resolved) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled == nil {
		f.enabled = map[string]settings.Resolved{}
	}
	if resolved == nil {
		delete(f.enabled, name)
		return
	}
	f.enabled[name] = *resolved
}

func available(t *testing.T, registration ask.RegisteredTool) bool {
	t.Helper()
	optional, ok := registration.Tool.(ask.OptionalTool)
	require.True(t, ok, "web and code tools are optional")
	return optional.Available()
}

// TestToolRegistryIsComplete: every tool this binary registers gives Ask
// tools it can register, each optional so a run offers it only while the
// tool is enabled, and is a connector its pages know, after the sources.
func TestToolRegistryIsComplete(t *testing.T) {
	var tools asktool.Registry
	require.NotPanics(t, func() { tools = toolRegistry() })
	require.Equal(t, []string{"tavily", "sourcebot"}, tools.Names())
	registrations := tools.Registrations(&fakeConnectors{})
	_, _, err := ask.NewToolRegistry(t.Context(), registrations)
	require.NoError(t, err)
	for _, registration := range registrations {
		assert.False(t, available(t, registration))
	}
	sources := sourceRegistry(nil, nil, nil, nil, nil, nil, false)
	var connectors []string
	for _, def := range connectorRegistry(sources, tools, automationRegistry(&config.Config{}, nil)).Definitions() {
		connectors = append(connectors, def.Name())
	}
	require.Equal(t, append(sources.Names(), tools.Names()...), connectors[:len(sources.Names())+len(tools.Names())])
}

// TestWebToolsFollowTheWebSearchConnector: the web tools are offered only
// while the connector is enabled, and each call uses the key and URL cached
// then.
func TestWebToolsFollowTheWebSearchConnector(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"query": "q", "results": []any{}})
	}))
	defer server.Close()
	store := &fakeConnectors{}
	registrations := asktool.NewRegistry(webSearchTool{}).Registrations(store)
	require.Len(t, registrations, 2)
	search, fetch := registrations[0], registrations[1]
	assert.False(t, available(t, search), "disabled")
	assert.False(t, available(t, fetch), "disabled")

	version := int64(0)
	enable := func(credential string) {
		version++
		resolved := settings.NewResolved(tavily.ConnectorName, settings.Values{tavily.FieldBaseURL: server.URL},
			map[string]string{tavily.FieldAPIKey: credential})
		resolved.Version = version
		store.set(tavily.ConnectorName, &resolved)
	}
	invoke := func() {
		t.Helper()
		_, err := search.Tool.(tool.InvokableTool).InvokableRun(context.Background(), `{"query":"q"}`)
		require.NoError(t, err)
	}
	enable("tvly-first")
	assert.True(t, available(t, search))
	assert.True(t, available(t, fetch))
	invoke()
	enable("tvly-second")
	invoke()
	mu.Lock()
	assert.Equal(t, []string{"Bearer tvly-first", "Bearer tvly-second"}, seen, "a changed key is used by the next call")
	mu.Unlock()

	store.set(tavily.ConnectorName, nil)
	assert.False(t, available(t, search), "disabled again")
}

// TestCodeToolsFollowTheCodeSearchConnector: the code tools are offered only
// while the connector is enabled, and their wording comes from its fields.
func TestCodeToolsFollowTheCodeSearchConnector(t *testing.T) {
	store := &fakeConnectors{}
	registrations := asktool.NewRegistry(codeSearchTool{}).Registrations(store)
	require.Len(t, registrations, 2)
	search, read := registrations[0], registrations[1]
	assert.False(t, available(t, search))
	assert.False(t, available(t, read))
	assert.Equal(t, ask.Wording{}, toolWording(store)(), "neutral while off")

	// A deployment's values.
	resolved := settings.NewResolved(sourcebot.ConnectorName, settings.Values{
		sourcebot.FieldURL:         "http://sourcebot.example.svc.cluster.local:3000",
		sourcebot.FieldRepoPrefix:  "github.com/example/",
		sourcebot.FieldSearchScope: "every Example repository (public and private, indexed hourly)",
		sourcebot.FieldReadScope:   "an Example repository (default branch, as of the last hourly index)",
	}, map[string]string{sourcebot.FieldAPIKey: "sbk_fixture"})
	store.set(sourcebot.ConnectorName, &resolved)
	assert.True(t, available(t, search))
	assert.True(t, available(t, read))
	info, err := search.Tool.Info(context.Background())
	require.NoError(t, err)
	assert.Contains(t, info.Desc, "Search the code of every Example repository (public and private, indexed hourly).")
	info, err = read.Tool.Info(context.Background())
	require.NoError(t, err)
	assert.Contains(t, info.Desc, "Read the full content of ONE file from an Example repository (default branch, as of the last hourly index).")
	assert.Equal(t, "github.com/example/", toolWording(store)().Code.RepoPrefix)

	store.set(sourcebot.ConnectorName, nil)
	assert.False(t, available(t, search))
	info, err = search.Tool.Info(context.Background())
	require.NoError(t, err)
	assert.Contains(t, info.Desc, "Search the code of "+ask.DefaultCodeSearchScope+".")
}
