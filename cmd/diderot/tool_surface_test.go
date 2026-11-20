package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/sourcebot"
	"github.com/bluesky-social/diderot/internal/tavily"
)

// The tool surface golden records what models and the connector pages see
// of the connectors that aren't sources: the Ask tools web search and code
// search give, and every non-source connector's fields. A diff is a
// behavior change: regenerate with DIDEROT_UPDATE_GOLDEN=1 only for an
// intended one.
const toolSurfaceGoldenPath = "testdata/tool_surface.golden.json"

type toolSurface struct {
	// Connectors are every connector's name in the order pages and the API
	// list them.
	Connectors []string `json:"connectors"`
	// AskTools are the tools the tool connectors give Ask, in the order Ask
	// lists them, as they are while the connectors are off; Configured as
	// referenceToolSettings describe them.
	AskTools   []goldenTool `json:"ask_tools"`
	Configured []goldenTool `json:"configured_ask_tools"`
	// Available is which of them a run offers, off and then on.
	Available map[string][]bool `json:"available"`
	// Fields are each non-source connector's settings, as its page and the
	// API show them.
	Fields map[string][]settings.FieldView `json:"fields"`
	// Defaults are their fields' defaults.
	Defaults map[string]map[string]any `json:"defaults"`
}

// referenceToolSettings are one deployment's web and code search settings.
func referenceToolSettings() map[string]settings.Resolved {
	return map[string]settings.Resolved{
		tavily.ConnectorName: settings.NewResolved(tavily.ConnectorName, settings.Values{tavily.FieldBaseURL: ""},
			map[string]string{tavily.FieldAPIKey: "tvly-fixture"}),
		sourcebot.ConnectorName: settings.NewResolved(sourcebot.ConnectorName, settings.Values{
			sourcebot.FieldURL:         "http://sourcebot.example.svc.cluster.local:3000",
			sourcebot.FieldRepoPrefix:  "github.com/example/",
			sourcebot.FieldSearchScope: "every Example repository (public and private, indexed hourly)",
			sourcebot.FieldReadScope:   "an Example repository (default branch, as of the last hourly index)",
		}, map[string]string{sourcebot.FieldAPIKey: "sbk_fixture"}),
	}
}

func TestToolSurfaceMatchesGolden(t *testing.T) {
	ctx := t.Context()
	sources := sourceRegistry(nil, nil, nil, nil, nil, nil, false)
	registry := connectorRegistry(sources, toolRegistry(), automationRegistry(&config.Config{}, nil))
	got := toolSurface{Available: map[string][]bool{}, Fields: map[string][]settings.FieldView{}, Defaults: map[string]map[string]any{}}
	for _, def := range registry.Definitions() {
		got.Connectors = append(got.Connectors, def.Name())
		if slices.Contains(sources.Names(), def.Name()) {
			continue
		}
		defaults := map[string]any{}
		for _, field := range def.Fields() {
			got.Fields[def.Name()] = append(got.Fields[def.Name()], settings.FieldView{
				Key: field.Key, Label: field.Label, Help: field.Help, Kind: field.Kind, Required: field.Required,
				Discovered: field.Discovered, Options: field.Options, PromptVar: field.PromptVar,
			})
			if field.Default != nil {
				defaults[field.Key] = field.Default
			}
		}
		got.Defaults[def.Name()] = defaults
	}

	store := &fakeConnectors{}
	for _, registration := range toolRegistry().Registrations(store) {
		golden := toolGolden(t, ctx, registration)
		got.AskTools = append(got.AskTools, golden)
		got.Available[golden.Name] = []bool{available(t, registration)}
	}
	for name, resolved := range referenceToolSettings() {
		store.set(name, &resolved)
	}
	for i, registration := range toolRegistry().Registrations(store) {
		golden := toolGolden(t, ctx, registration)
		require.Equal(t, got.AskTools[i].Name, golden.Name)
		got.Configured = append(got.Configured, golden)
		got.Available[golden.Name] = append(got.Available[golden.Name], available(t, registration))
	}
	checkGolden(t, toolSurfaceGoldenPath, got)
}
