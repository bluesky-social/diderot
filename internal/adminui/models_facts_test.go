package adminui

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/setup"
)

// TestPublishedFactsSwitch: the Models page says what the published facts
// are and turns reading them off and on, for a server with no way out.
func TestPublishedFactsSwitch(t *testing.T) {
	h := newSetupHarness(t)
	body := h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, "Published model facts")
	assert.Contains(t, body, "say nothing about this server")
	assert.Contains(t, body, "Turn published facts off")

	w := h.post("/admin/settings/models/facts", url.Values{"published": {"off"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "notice=Published+model+facts+are+off.")
	assert.True(t, h.store.Models().PublishedFactsOff)
	body = h.get("/admin/settings/models", false).Body.String()
	assert.Contains(t, body, "Nothing is fetched.")
	assert.Contains(t, body, "Turn published facts on")
	var action string
	require.NoError(t, h.pool.QueryRow(t.Context(), `SELECT action FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.facts", action)

	w = h.post("/admin/settings/models/facts", url.Values{"version": {"2"}, "published": {"on"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.False(t, h.store.Models().PublishedFactsOff)
}

// TestModelPageMatchAndPublishedFacts: an administrator says which catalog
// entry describes a model, which changes no request; and asks for the
// published facts, which lists what that changes for the jobs using the
// model before it does anything.
func TestModelPageMatchAndPublishedFacts(t *testing.T) {
	h := newSetupHarness(t)
	no := false
	h.catalog.published = map[string]modelregistry.Published{"openai/model-a": {Catalog: modelregistry.Facts{Temperature: &no}}}
	w := h.post("/admin/settings/models", url.Values{"role.ask": {"openai/model-a"}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"8"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	version := func() string { return strconv.FormatInt(h.store.Models().Version, 10) }
	const page = "/admin/settings/models/model?ref=openai%2Fmodel-a"

	body := h.get(page, false).Body.String()
	assert.Contains(t, body, "nothing: no entry is certainly this model")
	assert.Contains(t, body, `<input type="radio" name="match" value="maker/model-a-1">`, "an entry that may be it is offered, not used")
	assert.Contains(t, body, "A suggestion: its ID is close to this model's, not the same.")
	assert.Contains(t, body, "<td>no <span class=\"sub\">from the published catalog</span></td>", "what is published, beside what is sent")
	assert.Contains(t, body, "Use the published facts")

	// The match is the administrator's to set, and no request changes.
	before := h.store.Models().Available[0]
	w = h.post("/admin/settings/models/model/match", url.Values{"version": {version()}, "ref": {"openai/model-a"}, "match": {"maker/model-a-1"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	entry := h.store.Models().Available[0]
	assert.Equal(t, "maker/model-a-1", entry.Catalog)
	assert.True(t, before.SameRequests(entry), "a match changes what the page shows, never a request")
	body = h.get(page, false).Body.String()
	assert.Contains(t, body, "<code>maker/model-a-1</code>")
	assert.Contains(t, body, `value="maker/model-a-1" checked`)

	// An entry the catalog does not have is refused; another may be typed;
	// and the match can be cleared.
	w = h.post("/admin/settings/models/model/match", url.Values{"version": {version()}, "ref": {"openai/model-a"}, "match": {"other"}, "match_other": {"nowhere/none"}}, false)
	assert.Contains(t, w.Header().Get("Location"), "error=")
	assert.Equal(t, "maker/model-a-1", h.store.Models().Available[0].Catalog)
	w = h.post("/admin/settings/models/model/match", url.Values{"version": {version()}, "ref": {"openai/model-a"}, "match": {"other"}, "match_other": {" other/model "}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "other/model", h.store.Models().Available[0].Catalog)
	assert.Contains(t, h.get(page, false).Body.String(), `name="match_other" class="mono" value="other/model"`)
	w = h.post("/admin/settings/models/model/match", url.Values{"version": {version()}, "ref": {"openai/model-a"}, "match": {"none"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, modelregistry.CatalogNone, h.store.Models().Available[0].Catalog)
	var action string
	require.NoError(t, h.pool.QueryRow(t.Context(), `SELECT action FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action))
	assert.Equal(t, "setup.models.entry.match", action)

	// Using the published facts changes what Ask sends: it says so first.
	w = h.post("/admin/settings/models/model/published", url.Values{"version": {version()}, "ref": {"openai/model-a"}}, false)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "Using the published facts changes what Ask sends:")
	assert.Contains(t, w.Body.String(), "<li>Sends a temperature: yes becomes no</li>")
	assert.False(t, h.store.Models().Available[0].ChatOmitTemperature, "nothing changes until it is confirmed")
	w = h.post("/admin/settings/models/model/published", url.Values{"version": {version()}, "ref": {"openai/model-a"}, "confirm": {"change"}}, false)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	entry = h.store.Models().Available[0]
	assert.True(t, entry.ChatOmitTemperature)
	assert.Equal(t, modelregistry.SourceCatalog, entry.SourceOf(modelregistry.FactTemperature))
	body = h.get(page, false).Body.String()
	assert.Contains(t, body, "<td>from the published catalog</td>")
	assert.NotContains(t, body, "Use the published facts", "nothing differs any more")
}

func TestSummaryCostUsesThePublishedPrices(t *testing.T) {
	price := func(in, out float64) setup.RoleView { return setup.RoleView{} }
	_ = price
	role := func(key string, in, out *float64) setup.RoleView {
		return setup.RoleView{
			ModelRole: setup.ModelRole{Key: key, Options: []setup.ModelOption{{Ref: "p/" + key, InputPrice: in, OutputPrice: out}}},
			Model:     "p/" + key,
		}
	}
	models := setup.ModelsView{Roles: []setup.RoleView{
		role("session_summary_map", ptr(1.0), ptr(5.0)), role("session_summary_reduce", ptr(2.0), ptr(10.0)),
	}}
	// The combined summary alone: 3M tokens read at $2, 1M written at $10.
	assert.Equal(t, "$16", summaryCost(models, false, 9_000_000, 3_000_000, 3_000_000, 1_000_000))
	// Every part again as well: 9M read at $1 and 3M written at $5.
	assert.Equal(t, "$40", summaryCost(models, true, 9_000_000, 3_000_000, 3_000_000, 1_000_000))
	assert.Equal(t, "$0.02", summaryCost(models, false, 0, 0, 5000, 1000))
	// A price that is not known gives no figure rather than a wrong one.
	models.Roles[0] = role("session_summary_map", nil, nil)
	assert.Empty(t, summaryCost(models, true, 1, 1, 1, 1))
	assert.Equal(t, "less than $0.01", dollars(0.001))
}
