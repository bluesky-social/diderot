package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/sessioningest"
)

// TestToolAndAutomationStepsKeepTheirIDs: the checklist's steps for this
// binary's tools and automations, derived from their registries, keep the
// IDs, titles and sentences the API and the setup page show.
func TestToolAndAutomationStepsKeepTheirIDs(t *testing.T) {
	var steps [][3]string
	for _, tool := range toolRegistry().Tools() {
		info := tool.Info()
		steps = append(steps, [3]string{info.Step, info.Title, info.Summary})
	}
	for _, auto := range automationRegistry(&config.Config{}, nil).Automations() {
		info := auto.Info()
		steps = append(steps, [3]string{info.Step, info.Title, info.Summary})
	}
	assert.Equal(t, [][3]string{
		{"web-search", "Web search", "Lets Ask search the web."},
		{"code-search", "Code search", "Lets Ask search and read your code."},
		{"pagerduty", "PagerDuty", "Investigates PagerDuty incidents as they trigger and replies in Slack."},
	}, steps)
}

// TestSetupFactsSessionPush: push ingest is a source fact only while it is
// on, and in static mode it can't count while no token may push.
func TestSetupFactsSessionPush(t *testing.T) {
	cfg := &config.Config{SelfServiceGroups: []string{"everyone"}}
	summaries := sessioningest.SummaryControl{Store: sessioningest.NewStore(nil)}
	assert.Nil(t, setupFacts(cfg, &modelCatalog{}, nil, summaries).SessionPush, "push is off")

	cfg.SessionPush.Enabled = true
	push := setupFacts(cfg, &modelCatalog{}, nil, summaries).SessionPush
	if assert.NotNil(t, push) {
		assert.Empty(t, push.NoPusher, "an identity provider's groups can't be known ahead")
		assert.Equal(t, summaries, push.Summaries)
	}

	tokens := func(groups string) *serverAuth {
		static, err := auth.ParseStaticTokens([]byte(`tokens:
  - sha256: 0850123315d21ab90f4f7236408a52ef6dbd6a02a6550e5c10dc73f4d993680e
    email: operator@example.com
    groups: [` + groups + `]
`))
		require.NoError(t, err)
		return &serverAuth{mode: config.AuthModeStatic, static: static}
	}
	assert.Empty(t, setupFacts(cfg, &modelCatalog{}, tokens("diderot-admins, everyone"), summaries).SessionPush.NoPusher)
	assert.Equal(t, "Nobody can push agent sessions: no token in the token file has a group from DIDEROT_SELF_SERVICE_GROUPS.",
		setupFacts(cfg, &modelCatalog{}, tokens("diderot-admins"), summaries).SessionPush.NoPusher)

	// DIDEROT_SELF_SERVICE_GROUPS="," lets nobody push, in any auth mode.
	cfg.SelfServiceGroups = nil
	assert.Equal(t, "Nobody can push agent sessions: DIDEROT_SELF_SERVICE_GROUPS names no group.",
		setupFacts(cfg, &modelCatalog{}, nil, summaries).SessionPush.NoPusher)
}
