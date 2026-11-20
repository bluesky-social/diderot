package adminui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// TestModelsPageAsksLimits: Ask's speed, length and time limits sit under
// Ask, closed while every one is at its default, with each time said in
// words. A save reads times as people type them and stores them; one it
// can't read or that is out of range is refused by the field's label, with
// what was typed kept and the limits open.
func TestModelsPageAsksLimits(t *testing.T) {
	h := newSetupHarness(t)
	body := h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `<details class="disclosure" id="ask-limits">`, "closed while nothing is changed")
	assert.Contains(t, body, `<option value="" selected>Not set: the provider decides</option>`)
	assert.Contains(t, body, `<option value="fast">fast</option>`)
	assert.Contains(t, body, `name="ask_max_output_tokens" type="number" min="256" max="1000000" value="32768"`)
	assert.Contains(t, body, `name="ask_model_timeout" value="5 minutes"`)
	assert.Contains(t, body, `name="ask_turn_deadline" value="10 minutes"`)
	assert.Contains(t, body, `name="ask_investigator_deadline" value="4 minutes"`)
	assert.Contains(t, body, `name="ask_investigator_grace" value="2 minutes"`)
	assert.NotContains(t, body, "DIDEROT_ASK", "no variable is named")

	form := func(change func(url.Values)) url.Values {
		values := url.Values{
			"role.ask": {"openai/model-a"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"8"},
			"ask_service_tier": {"fast"}, "ask_max_output_tokens": {"8192"},
			"ask_model_timeout": {"90 seconds"}, "ask_turn_deadline": {"1h30m"},
			"ask_investigator_deadline": {"3 min"}, "ask_investigator_grace": {"1 minute 30 seconds"},
		}
		if change != nil {
			change(values)
		}
		return values
	}
	for want, change := range map[string]func(url.Values){
		"Time limit for one model request must be between 10 seconds and 1 day": func(v url.Values) { v.Set("ask_model_timeout", "a while") },
		"Time limit for one question must be between 10 seconds and 1 day":      func(v url.Values) { v.Set("ask_turn_deadline", "2 days") },
		"Longest reply must be a number of tokens from 256 to 1000000":          func(v url.Values) { v.Set("ask_max_output_tokens", "lots") },
		"Service tier must be one of default, fast, priority":                   func(v url.Values) { v.Set("ask_service_tier", "turbo") },
	} {
		w := h.post("/admin/settings/models", form(change), false)
		require.Equal(t, http.StatusUnprocessableEntity, w.Code, want)
		assert.Contains(t, w.Body.String(), want)
		assert.Contains(t, w.Body.String(), `<details class="disclosure" id="ask-limits" open>`, "the refused limits are shown")
		assert.Contains(t, w.Body.String(), `name="ask_investigator_grace" value="1 minute 30 seconds"`, "what was typed is kept")
	}
	assert.Contains(t, h.post("/admin/settings/models", form(func(v url.Values) { v.Set("ask_model_timeout", "a while") }), false).Body.String(),
		`name="ask_model_timeout" value="a while"`)
	assert.Zero(t, h.catalog.tests, "a refused form calls no model")

	w := h.post("/admin/settings/models", form(nil), false)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	saved := h.store.Models()
	assert.Equal(t, settings.AskLimits{
		ServiceTier: "fast", MaxOutputTokens: 8192, ModelTimeout: 90 * time.Second, TurnDeadline: 90 * time.Minute,
		InvestigatorDeadline: 3 * time.Minute, InvestigatorGrace: 90 * time.Second,
	}, saved.AskLimits())

	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, `<details class="disclosure" id="ask-limits" open>`, "open once a limit is changed")
	assert.Contains(t, body, `<option value="fast" selected>fast</option>`)
	assert.Contains(t, body, `name="ask_model_timeout" value="1 minute 30 seconds"`)
	assert.Contains(t, body, `name="ask_turn_deadline" value="1 hour 30 minutes"`)

	// A save without the limits, as the first-run pages post, keeps them.
	w = h.post("/admin/settings/models", url.Values{"version": {"2"}, "ask_max_tool_turns": {"9"}, "ask_reasoning_effort": {""}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, 8192, h.store.Models().AskLimits().MaxOutputTokens)
}

// TestJobsPageSummarySchedule: once agent sessions reach the install, Jobs
// and maintenance shows when they are summarized, at the defaults until
// someone saves; a save is validated, versioned and audited, and names no
// internal setting.
func TestJobsPageSummarySchedule(t *testing.T) {
	h := newSetupHarness(t)
	ctx := context.Background()
	body := h.get("/admin/operations/jobs", false).Body.String()
	assert.NotContains(t, body, "When sessions are summarized", "nothing to set while no agent session reaches the install")

	_, err := h.pool.Exec(ctx, `TRUNCATE session_ingest_candidates CASCADE`)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, published_revision, complete)
		VALUES ('operator@example.com', 'operator@example.com', 'claude-code', 'summarized-session', 'rev-1', 'rev-1', true)`)
	require.NoError(t, err)
	w := h.get("/admin/operations/jobs", false)
	require.Equal(t, http.StatusOK, w.Code)
	body = w.Body.String()
	assert.Contains(t, body, `<h3>When sessions are summarized</h3>`)
	assert.Contains(t, body, `name="quiet_for" value="10 minutes"`)
	assert.Contains(t, body, `name="min_added_user_turns" type="number" min="0" value="2"`)
	assert.Contains(t, body, `name="min_added_records" type="number" min="0" value="12"`)
	assert.Contains(t, body, `name="min_added_bytes" type="number" min="0" value="32768"`)
	assert.Contains(t, body, `name="max_pending_for" value="6 hours"`)
	assert.NotContains(t, body, "Last changed by", "nobody has changed it")
	assert.NotContains(t, body, "DIDEROT_", "no variable is named")

	version := strconv.FormatInt(h.store.SessionSummaries().Version, 10)
	assert.Contains(t, body, `name="version" value="`+version+`"`)
	form := func(change func(url.Values)) url.Values {
		values := url.Values{
			"version":   {version},
			"quiet_for": {"0 seconds"}, "min_added_user_turns": {"0"}, "min_added_records": {"30"}, "min_added_bytes": {"65536"},
			"max_pending_for": {"1 day"},
		}
		if change != nil {
			change(values)
		}
		return values
	}
	for want, change := range map[string]func(url.Values){
		"the+quiet+time+must+be+a+length+of+time":             func(v url.Values) { v.Set("quiet_for", "soon") },
		"the+quiet+time+must+be+between+0+seconds+and+7+days": func(v url.Values) { v.Set("quiet_for", "8 days") },
		"the+longest+wait+must+be+a+length+of+time":           func(v url.Values) { v.Set("max_pending_for", "") },
		"new+entries+must+be+a+whole+number":                  func(v url.Values) { v.Set("min_added_records", "-1") },
		"new+bytes+must+be+a+whole+number":                    func(v url.Values) { v.Set("min_added_bytes", "32k") },
	} {
		w := h.post("/admin/operations/session-summaries/schedule", form(change), false)
		require.Equal(t, http.StatusSeeOther, w.Code)
		assert.Contains(t, w.Header().Get("Location"), "/admin/operations/jobs?error=When+sessions+are+summarized+was+not+changed")
		assert.Contains(t, w.Header().Get("Location"), want)
	}
	assert.Nil(t, h.store.SessionSummaries().Schedule, "a refused form saves nothing")

	w = h.post("/admin/operations/session-summaries/schedule", form(nil), false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/operations/jobs?notice=When+sessions+are+summarized+was+saved.+It+applies+within+a+minute.", w.Header().Get("Location"))
	assert.Equal(t, settings.SummarySchedule{
		MinAddedRecords: 30, MinAddedBytes: 65536, MaxPendingFor: settings.Duration(24 * time.Hour),
	}, h.store.SessionSummaries().EffectiveSchedule(), "the zeros an administrator chose are kept")
	var actor, action string
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT actor_email, action FROM admin_audit_log WHERE target = 'connector_settings:session_summaries'`).Scan(&actor, &action))
	assert.Equal(t, "op@example.com", actor)
	assert.Equal(t, "operations.session_summaries.schedule", action)

	w = h.post("/admin/operations/session-summaries/schedule", form(nil), false)
	assert.Contains(t, w.Header().Get("Location"), "Someone+changed+this", "the form's version is stale")

	body = h.get("/admin/operations/jobs", false).Body.String()
	assert.Contains(t, body, `name="quiet_for" value="0 seconds"`)
	assert.Contains(t, body, `name="max_pending_for" value="1 day"`)
	assert.Contains(t, body, "Last changed by op@example.com")
}
