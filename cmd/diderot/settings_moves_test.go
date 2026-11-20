package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// TestAskIsBuiltAgainWhenALimitChanges: Ask's limits are read from the saved
// models row, at their defaults while nothing is saved, and a change to any
// one makes a replica build Ask again without restarting the leader's other
// work. A service tier saved for a model that takes none takes Ask out of
// use, and the page says why.
func TestAskIsBuiltAgainWhenALimitChanges(t *testing.T) {
	recorder := newInferenceRecorder(t, 8)
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	_, catalog := loadInference(t, openAIProvider("lab", recorder, "k"))
	responses := []modelregistry.Protocol{modelregistry.ProtocolResponses}
	saved := settings.Models{
		Roles: map[string]string{"ask": "lab/big"},
		Available: []modelregistry.Model{
			{Provider: "lab", ID: "big", Protocols: responses, ServiceTier: true, TestedFor: []modelregistry.Role{"ask"}},
			{Provider: "lab", ID: "plain", Protocols: responses, TestedFor: []modelregistry.Role{"ask"}},
		},
	}
	built := catalog.resolve(saved)
	require.Equal(t, "big", built.ask.ID)
	assert.Equal(t, settings.Models{}.AskLimits(), built.askLimits)
	assert.True(t, sameAskModel(built, catalog.resolve(saved)))

	for name, change := range map[string]func(*settings.Models){
		"service tier":           func(m *settings.Models) { m.AskServiceTier = "fast" },
		"longest reply":          func(m *settings.Models) { m.AskMaxOutputTokens = 8192 },
		"model request limit":    func(m *settings.Models) { m.AskModelTimeout = settings.Duration(90 * time.Second) },
		"question limit":         func(m *settings.Models) { m.AskTurnDeadline = settings.Duration(time.Minute) },
		"investigation time":     func(m *settings.Models) { m.AskInvestigatorDeadline = settings.Duration(time.Minute) },
		"investigation write-up": func(m *settings.Models) { m.AskInvestigatorGrace = settings.Duration(time.Minute) },
	} {
		changed := saved
		change(&changed)
		next := catalog.resolve(changed)
		assert.Equal(t, "big", next.ask.ID, name)
		assert.False(t, sameAskModel(built, next), "%s: Ask is built again", name)
		assert.True(t, sameRoleModels(built, next), "%s: the leader's work goes on", name)
	}
	// Tool turns and effort are read as each question starts, with no build.
	changed := saved
	changed.AskMaxToolTurns, changed.AskReasoningEffort = 20, "high"
	assert.True(t, sameAskModel(built, catalog.resolve(changed)))

	refused := saved
	refused.Roles = map[string]string{"ask": "lab/plain"}
	refused.AskServiceTier = "fast"
	assert.Empty(t, catalog.resolve(refused).ask.ID)
	views, err := catalog.Roles(t.Context(), refused)
	require.NoError(t, err)
	assert.Equal(t, `This job can't use it: model "plain" does not support a service tier.`, viewOf(t, views, "ask").Warning)
}

// TestSessionSummaryJobsNeedNoDeploymentSwitch: the session summary and
// title jobs are offered a model on any server; only the switch on the Jobs
// and maintenance page, and a model, decide whether summaries run.
func TestSessionSummaryJobsNeedNoDeploymentSwitch(t *testing.T) {
	serveEnv(t)
	_, catalog := loadInference(t)
	views, err := catalog.Roles(t.Context(), settings.Models{})
	require.NoError(t, err)
	for _, key := range []string{"session_summary_map", "session_summary_reduce", "titles"} {
		assert.Empty(t, viewOf(t, views, key).Unavailable, key)
	}
}

// TestSummaryThresholdsFollowTheSavedSchedule: the leader's session ingest
// reads when sessions are summarized from the settings cache each time, so
// a schedule saved on the Jobs and maintenance page applies with no restart;
// with none saved it is the default.
func TestSummaryThresholdsFollowTheSavedSchedule(t *testing.T) {
	pool := ingestTestPool(t, "cmd_summary_thresholds")
	ctx := context.Background()
	store := settings.NewStore(pool, discardLogger())
	require.NoError(t, store.Load(ctx))
	thresholds := summaryThresholds(store)
	assert.Equal(t, sessioningest.DefaultSummarizePolicy(), thresholds())
	assert.Equal(t, sessioningest.DefaultSummarizePolicy(), summaryThresholds(nil)())

	_, err := store.UpdateSessionSummaries(ctx, store.SessionSummaries().Version, settings.Actor{Email: "op@example.com"},
		"operations.session_summaries.schedule", func(summaries *settings.SessionSummaries) error {
			summaries.Schedule = &settings.SummarySchedule{
				QuietFor: settings.Duration(time.Minute), MinAddedUserTurns: 0, MinAddedRecords: 40, MinAddedBytes: 1 << 20,
				MaxPendingFor: settings.Duration(time.Hour),
			}
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, sessioningest.SummarizePolicy{
		QuietFor: time.Minute, MinAddedUserTurns: 0, MinAddedRecords: 40, MinAddedContentBytes: 1 << 20, MaxPendingFor: time.Hour,
	}, thresholds(), "the same reader, with no rebuild")
}

// TestSessionIngestSummarizesOnceAModelIsChosen: with no model for session
// summaries the worker still records the sessions it sees, and with one it
// summarizes; no deployment variable decides.
func TestSessionIngestSummarizesOnceAModelIsChosen(t *testing.T) {
	pool := ingestTestPool(t, "cmd_session_ingest_modes")
	cfg := &config.Config{SessionIngestTick: time.Minute, SessionIngestMaxRevisionBytes: 1 << 20, SessionIngestChunkConcurrency: 2}
	feed := sessioningest.NewClientFeed(sessionfeed.NewClient("http://feed.invalid", "x", nil))
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	summaryModel := boundModel{
		Model:    modelregistry.Model{Provider: "lab", ID: "summary", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages}},
		endpoint: llmclient.Endpoint{BaseURL: "https://provider.invalid/v1", APIKey: "k", Auth: llmclient.AuthXAPIKey},
	}
	for mode, models := range map[string]*inferenceModels{
		"shadow":    {},
		"summarize": {summaryMap: summaryModel, summaryReduce: summaryModel},
	} {
		var logs bytes.Buffer
		ingest := testSessionIngest(t, pool, cfg)
		ingest.models, ingest.thresholds = models, summaryThresholds(nil)
		ingest.logger = slog.New(slog.NewTextHandler(&logs, nil))
		require.NoError(t, ingest.register(worker.NewSupervisor(nil, nil), sink, feed, ""), mode)
		assert.Contains(t, logs.String(), "sessioningest worker enabled ("+mode+")")
	}
}
