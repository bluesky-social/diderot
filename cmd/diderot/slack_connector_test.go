package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
)

// TestConnectorRegistry checks every registered connector: no secret field is
// a prompt variable (registration panics on one, so building the registry is
// itself the check), and every connector variable promptcatalog allows comes
// from a registered field.
func TestConnectorRegistry(t *testing.T) {
	var registry *settings.Registry
	require.NotPanics(t, func() {
		registry = connectorRegistry(sourceRegistry(nil, nil, nil, nil, nil, nil, false), toolRegistry(), automationRegistry(&config.Config{}, nil))
	})
	provided := map[string]bool{}
	for _, def := range registry.Definitions() {
		for _, field := range def.Fields() {
			if field.Kind == settings.KindSecret {
				assert.Empty(t, field.PromptVar, "%s.%s is a secret", def.Name(), field.Key)
			}
			if field.PromptVar != "" {
				provided[field.PromptVar] = true
			}
		}
	}
	builtIn := []string{promptcatalog.VarOrgName, promptcatalog.VarOrgDescription, promptcatalog.VarMaxTitleRunes, promptcatalog.VarSources,
		promptcatalog.VarGrafanaDatasources}
	for _, name := range promptcatalog.VariableNames() {
		if !slices.Contains(builtIn, name) {
			assert.True(t, provided[name], "{{%s}} has no registered connector field", name)
		}
	}
}

// TestConnectorSecretVariables: each registered secret field's variable is
// part of a deployment's interface, so the names are pinned here and each
// has a row in docs/configuration.md.
func TestConnectorSecretVariables(t *testing.T) {
	var names []string
	for _, def := range connectorRegistry(sourceRegistry(nil, nil, nil, nil, nil, nil, false), toolRegistry(), automationRegistry(&config.Config{}, nil)).Definitions() {
		for _, field := range def.Fields() {
			if field.Kind == settings.KindSecret {
				names = append(names, settings.EnvName(def.Name(), field.Key))
			}
		}
	}
	assert.Equal(t, []string{
		"DIDEROT_SLACK_BOT_TOKEN", "DIDEROT_SLACK_APP_TOKEN", "DIDEROT_SLACK_USER_TOKEN",
		"DIDEROT_DISCORD_BOT_TOKEN",
		"DIDEROT_SESSION_FEED_TOKEN",
		"DIDEROT_NOTION_TOKEN",
		"DIDEROT_LINEAR_TOKEN",
		"DIDEROT_ZENDESK_OAUTH_SECRET",
		"DIDEROT_TAVILY_API_KEY", "DIDEROT_SOURCEBOT_API_KEY",
		"DIDEROT_PAGERDUTY_API_TOKEN", "DIDEROT_PAGERDUTY_WEBHOOK_SECRETS",
	}, names)
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	require.NoError(t, err)
	for _, name := range names {
		assert.Contains(t, string(doc), "| `"+name+"` |", "docs/configuration.md needs a row for %s", name)
	}
}
