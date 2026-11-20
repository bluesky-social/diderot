package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelfacts"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// publishedFixture is the two published lists cut down for tests
// (internal/modelfacts/testdata).
func publishedFixture(t *testing.T) *modelfacts.Catalog {
	t.Helper()
	list := func(name string, parse func([]byte) (modelfacts.List, error)) modelfacts.List {
		raw, err := os.ReadFile("../../internal/modelfacts/testdata/" + name)
		require.NoError(t, err)
		parsed, err := parse(raw)
		require.NoError(t, err)
		return parsed
	}
	return modelfacts.Merge(list("models.dev.json", modelfacts.ParseModelsDev), list("litellm.json", modelfacts.ParseLiteLLM))
}

// gatewayListing is a gateway's model list: each model with who the gateway
// says owns it, and a virtual ":main" route beside the models Diderot uses
// through one.
func gatewayListing() []modelregistry.Listed {
	return []modelregistry.Listed{
		{ID: "deepseek-v4.1-flash", OwnedBy: "fireworks"}, {ID: "deepseek-v4.1-flash:main", OwnedBy: "virtual"},
		{ID: "gemini-3.6-flash", OwnedBy: "gcp.vertex_ai"}, {ID: "gemini-3.8-flash", OwnedBy: "gcp.vertex_ai"},
		{ID: "gpt-6.1-sol", OwnedBy: "openai"}, {ID: "gpt-6.1-sol:main", OwnedBy: "virtual"},
		{ID: "local-embed", OwnedBy: "local-vllm"},
	}
}

// factsCatalog is a catalog of a deployment that runs every job behind one
// gateway, with the published fixture read at now.
func factsCatalog(t *testing.T, now time.Time, listing []modelregistry.Listed) *modelCatalog {
	t.Helper()
	catalog := gatewayCatalog(t, gatewayRow(true, "k"))
	published := publishedFixture(t)
	catalog.loadFacts = func(context.Context) (*modelfacts.Catalog, modelfacts.Status, error) {
		return published, modelfacts.Status{modelfacts.SourceModelsDev: {FetchedAt: now}}, nil
	}
	catalog.now = func() time.Time { return now }
	catalog.menus = func(context.Context) ([]modelprovider.Menu, error) {
		return []modelprovider.Menu{{Name: "gateway", Kind: modelregistry.KindOpenAI, Enabled: true, Models: listing, Listing: listing}}, nil
	}
	return catalog
}

// captionsOn is the models row of a deployment whose captions use id.
func captionsOn(id string) settings.Models {
	return settings.Models{
		Roles: map[string]string{"image_captions": "gateway/" + id},
		Available: []modelregistry.Model{{
			Provider: "gateway", ID: id, Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}, Images: true,
		}},
	}
}

// TestAModelBeingRetiredNeedsAttentionInTime is the incident this exists
// for: the caption model's provider retired it at a day's notice that nobody
// saw. The published lists had the date; the job that uses the model says so
// from ninety days before.
func TestAModelBeingRetiredNeedsAttentionInTime(t *testing.T) {
	dayBefore := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	saved := captionsOn("gemini-3.6-flash")
	captions := func(catalog *modelCatalog, saved settings.Models) setup.RoleView {
		views, err := catalog.Roles(t.Context(), saved)
		require.NoError(t, err)
		return viewOf(t, views, "image_captions")
	}

	view := captions(factsCatalog(t, dayBefore, gatewayListing()), saved)
	assert.Equal(t, "gemini-3.6-flash retires on 2026-11-19 (tomorrow), the published list for Vertex says. Its replacement there is gemini-3.8-flash. Choose another model.",
		view.Attention)

	// Ninety days ahead it already needs someone; before that it is a note.
	for _, tc := range []struct {
		now       time.Time
		attention string
		note      string
	}{
		{time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC), "retires on 2026-11-19 (in 90 days)", ""},
		// Vertex also marks it deprecated, which needs someone whatever the date.
		{time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), "is deprecated, the published list for Vertex says, and retires on 2026-11-19 (in 91 days)", ""},
		{time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), "retires on 2026-11-19 (today)", ""},
		{time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), "was retired on 2026-11-19 (6 days ago)", ""},
	} {
		view := captions(factsCatalog(t, tc.now, gatewayListing()), saved)
		if tc.attention == "" {
			assert.Empty(t, view.Attention, tc.now)
		} else {
			assert.Contains(t, view.Attention, tc.attention, tc.now)
		}
		assert.Equal(t, tc.note, view.Note, tc.now)
	}

	// The provider's own list, when it gives a date, is the one believed.
	listed := gatewayListing()
	listed[2].Retires = "2026-12-24"
	view = captions(factsCatalog(t, dayBefore, listed), saved)
	assert.Contains(t, view.Attention, "retires on 2026-12-24 (in 36 days), its provider's list says.")

	// Deprecated is the provider saying to move, however far off its date.
	listed[2].Retires, listed[2].Deprecated = "2028-03-11", true
	view = captions(factsCatalog(t, dayBefore, listed), saved)
	assert.Contains(t, view.Attention, "gemini-3.6-flash is deprecated, its provider's list says, and retires on 2028-03-11 (in 479 days).")
	assert.Empty(t, view.Note)

	// A gateway that does not say who serves the model: the maker's own
	// API is not retiring it, but a cloud that resells it is, and that is
	// said.
	bare := []modelregistry.Listed{{ID: "gemini-3.6-flash"}}
	view = captions(factsCatalog(t, dayBefore, bare), saved)
	assert.Equal(t, "Vertex retires gemini-3.6-flash on 2026-11-19 (tomorrow). If this provider serves it from there, choose another model before then; "+
		"if not, say who serves it on the model's page.", view.Attention)
	// Deprecated there with a date far off still needs someone.
	early := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	view = captions(factsCatalog(t, early, bare), saved)
	assert.Contains(t, view.Attention, "Vertex lists gemini-3.6-flash as deprecated, and retires it on 2026-11-19 (in 171 days).")
	// An administrator who says Google's own API serves it ends that.
	pinned := captionsOn("gemini-3.6-flash")
	pinned.Available[0].Catalog = "google/gemini-3.6-flash"
	assert.Empty(t, captions(factsCatalog(t, dayBefore, bare), pinned).Attention)

	// A retirement further off than ninety days, of a model nobody has
	// deprecated, is a note under the job and not an alarm.
	far := gatewayListing()
	far[3].Retires = "2027-06-01"
	view = captions(factsCatalog(t, dayBefore, far), captionsOn("gemini-3.8-flash"))
	assert.Empty(t, view.Attention)
	assert.Equal(t, "Retires on 2027-06-01, its provider's list says.", view.Note)

	// The model in use now is not ending, and with the facts turned off
	// nothing published is used.
	assert.Empty(t, captions(factsCatalog(t, dayBefore, gatewayListing()), captionsOn("gemini-3.8-flash")).Attention)
	off := saved
	off.PublishedFactsOff = true
	assert.Empty(t, captions(factsCatalog(t, dayBefore, gatewayListing()), off).Attention)

	// So is the retirement of an embedding model that is being filled to
	// replace the present one: the embedder is already calling it.
	filling := settings.Models{
		Embedding: &settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 8, Collection: "c"},
		Filling:   &settings.Embedding{Provider: "gateway", Model: "gemini-3.6-flash", Dimensions: 8, Collection: "d"},
	}
	_, _, fillingAttention, err := factsCatalog(t, dayBefore, gatewayListing()).Embedding(t.Context(), filling)
	require.NoError(t, err)
	assert.Contains(t, fillingAttention, "The embedding model being filled is ending too. gemini-3.6-flash retires on 2026-11-19 (tomorrow)")

	// The embedding model's retirement is said too, on its own line.
	embedding := settings.Models{Embedding: &settings.Embedding{Provider: "gateway", Model: "gemini-3.6-flash", Dimensions: 8, Collection: "c"}}
	_, warning, attention, err := factsCatalog(t, dayBefore, gatewayListing()).Embedding(t.Context(), embedding)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Contains(t, attention, "retires on 2026-11-19 (tomorrow)")
	assert.Contains(t, attention, "embeds every passage again")
}

// TestModelCatalogAttention: a model in use that a whole listing no longer
// has needs attention; one an administrator typed in does not.
func TestModelCatalogAttention(t *testing.T) {
	listing := []modelregistry.Listed{{ID: "a"}}
	menus := []modelprovider.Menu{{Name: "p", Enabled: true, Listing: listing, Models: listing}}
	assert.Empty(t, unlisted(menus, modelregistry.Model{Provider: "p", ID: "a"}))
	assert.Equal(t, "The provider no longer lists gone.", unlisted(menus, modelregistry.Model{Provider: "p", ID: "gone"}))

	typed := []modelprovider.Menu{{Name: "p", Enabled: true, Offered: []string{"gone"}, Listing: listing, Models: []modelregistry.Listed{{ID: "gone"}}}}
	assert.Empty(t, unlisted(typed, modelregistry.Model{Provider: "p", ID: "gone"}), "IDs typed in are the administrator's to keep")
	noListing := []modelprovider.Menu{{Name: "p", Enabled: true, NoListing: true, Models: []modelregistry.Listed{{ID: "x"}}}}
	assert.Empty(t, unlisted(noListing, modelregistry.Model{Provider: "p", ID: "y"}))
	failing := []modelprovider.Menu{{Name: "p", Enabled: true, Error: "HTTP 500"}}
	assert.Empty(t, unlisted(failing, modelregistry.Model{Provider: "p", ID: "a"}), "a failing listing is the provider's attention, not the model's")
}

// TestJobsAreOfferedModelsWithWhatIsPublished: a job's choices carry the
// price and the match the catalog gives, and the provider's own list wins
// where it says something.
func TestJobsAreOfferedModelsWithWhatIsPublished(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	listing := gatewayListing()
	no, listedPrice := false, 9.0
	listing[3].Images, listing[3].InputPrice, listing[3].OutputPrice = &no, &listedPrice, &listedPrice
	views, err := factsCatalog(t, now, listing).Roles(t.Context(), settings.Models{})
	require.NoError(t, err)
	ask := viewOf(t, views, "ask")

	sol, ok := ask.Option("gateway/gpt-6.1-sol:main")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-6.1-sol", sol.Matched, "the route is the model the gateway lists without its suffix")
	require.NotNil(t, sol.InputPrice)
	assert.InDelta(t, 2.0, *sol.InputPrice, 1e-9)
	assert.InDelta(t, 10.0, *sol.OutputPrice, 1e-9)
	assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, sol.ReasoningEfforts, "Ask's efforts are known before any test")
	require.NotNil(t, sol.Images)
	assert.True(t, *sol.Images)

	gemini, ok := ask.Option("gateway/gemini-3.8-flash")
	require.True(t, ok)
	assert.Equal(t, "google-vertex/gemini-3.8-flash", gemini.Matched)
	assert.False(t, *gemini.Images, "the provider's own list wins over the catalog")
	assert.InDelta(t, 9.0, *gemini.InputPrice, 1e-9)

	// A model that has an entry offers the efforts its entry says, even
	// none: what is published now is not what its requests are built from.
	stated := settings.Models{Available: []modelregistry.Model{
		{Provider: "gateway", ID: "gpt-6.1-sol:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses}},
	}}
	views, err = factsCatalog(t, now, listing).Roles(t.Context(), stated)
	require.NoError(t, err)
	sol, _ = viewOf(t, views, "ask").Option("gateway/gpt-6.1-sol:main")
	assert.Empty(t, sol.ReasoningEfforts)

	deepseek, ok := ask.Option("gateway/deepseek-v4.1-flash:main")
	require.True(t, ok)
	assert.Empty(t, deepseek.Matched, "an entry spelt another way is only suggested, on the model's page")
	assert.Nil(t, deepseek.InputPrice)
	local, ok := ask.Option("gateway/local-embed")
	require.True(t, ok)
	assert.Empty(t, local.Matched, "a model no catalog lists is no error")
}

// TestANewModelIsCalledAsPublishedWithoutTryingShapes: giving a job a model
// the catalog describes sends the job's one request, shaped by the published
// facts, and the test finds only what nothing publishes.
func TestANewModelIsCalledAsPublishedWithoutTryingShapes(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	recorder := newInferenceRecorder(t, 0)
	catalog := factsCatalog(t, now, gatewayListing())
	catalog.providers = func() []modelregistry.Provider {
		return []modelregistry.Provider{openAIProvider("gateway", recorder, "k")}
	}
	trace := &modelregistry.TestTrace{}
	catalog.trace = trace

	entry, err := catalog.TestRole(t.Context(), settings.Models{}, "thread_summaries", "gateway/gpt-6.1-sol:main", "")
	require.NoError(t, err)
	assert.Equal(t, 1, trace.Requests, "one request: the job's own, as a confirmation")
	assert.Equal(t, []string{modelregistry.FactProtocols, modelregistry.FactTokenParam}, trace.Learned,
		"only what no catalog publishes is found by the test")
	sent := recorder.last().Body
	assert.NotContains(t, sent, "temperature", "the catalog says it refuses one, so none is tried")
	assert.Equal(t, "low", sent["reasoning_effort"], "the least reasoning it offers")
	assert.True(t, entry.ChatOmitTemperature)
	assert.Equal(t, modelregistry.SourceCatalog, entry.SourceOf(modelregistry.FactTemperature))
	assert.Equal(t, "low", entry.ChatReasoningEffort)
	assert.Equal(t, modelregistry.DefaultReasoningTokenAllowance, entry.ChatReasoningTokenAllowance, "it reasons anyway, so its output limit gets room")
	assert.True(t, entry.Images)
	assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, entry.ReasoningEfforts)
	assert.Equal(t, modelregistry.SourceTest, entry.SourceOf(modelregistry.FactTokenParam))

	// Ask with an effort the catalog says it does not take is refused
	// before any request.
	saved := settings.Models{Available: []modelregistry.Model{entry}}
	before := requestCount(recorder)
	_, err = catalog.TestRole(t.Context(), saved, "ask", "gateway/gpt-6.1-sol:main", "none")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "low, medium, high, xhigh, max, not none")
	assert.Equal(t, before, requestCount(recorder))

	// An entry that was stated, as a migration writes one, is sent as
	// stated: what is published about its model changes nothing.
	stated := captionsOn("gemini-3.8-flash")
	stated.Available[0].ChatExtraBody = map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}}
	kept, err := catalog.TestRole(t.Context(), stated, "image_captions", "gateway/gemini-3.8-flash", "")
	require.NoError(t, err)
	assert.True(t, stated.Available[0].SameRequests(kept))
	assert.NotContains(t, recorder.last().Body, "reasoning_effort")
}

// TestModelDetailShowsWhereEachFactCameFrom: the model's page lists each
// fact with its source, what is published now when that differs, and the
// catalog entry the model is matched to or the entries it might be.
func TestModelDetailShowsWhereEachFactCameFrom(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	catalog := factsCatalog(t, now, gatewayListing())
	saved := settings.Models{Available: []modelregistry.Model{
		{Provider: "gateway", ID: "gpt-6.1-sol:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}, Images: true},
		{Provider: "gateway", ID: "deepseek-v4.1-flash:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}},
	}}
	fact := func(detail setup.ModelDetail, key string) setup.FactRow {
		for _, row := range detail.Facts {
			if row.Key == key {
				return row
			}
		}
		t.Fatalf("no fact %s", key)
		return setup.FactRow{}
	}

	detail, err := catalog.Detail(t.Context(), saved, "gateway/gpt-6.1-sol:main")
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-6.1-sol", detail.Match.Ref)
	assert.Contains(t, detail.Match.How, "OpenAI lists gpt-6.1-sol")
	temperature := fact(detail, modelregistry.FactTemperature)
	assert.Equal(t, "yes", temperature.Value, "what is sent now")
	assert.Equal(t, "set when the model was added", temperature.Source)
	assert.Equal(t, "no", temperature.Published, "what the catalog says")
	assert.Equal(t, "from the published catalog", temperature.PublishedBy)
	assert.True(t, detail.Differs)
	assert.Empty(t, fact(detail, modelregistry.FactImages).Published, "the same as published: nothing to show")
	var labels []string
	for _, row := range detail.Info {
		labels = append(labels, row.Label+": "+row.Value)
	}
	assert.Contains(t, labels, "Price: $2 in, $10 out per million tokens ($0.1 for input read from the cache)")
	assert.Contains(t, labels, "Calls tools: yes")

	// No effort and the effort "none" are different requests, and read
	// differently.
	assert.Equal(t, "not sent", factValue(modelregistry.Model{}, modelregistry.FactChatEffort))
	assert.Equal(t, "none", factValue(modelregistry.Model{ChatReasoningEffort: "none"}, modelregistry.FactChatEffort))
	assert.NotEqual(t, factValue(modelregistry.Model{}, modelregistry.FactEfforts), factValue(modelregistry.Model{ReasoningEfforts: []string{"none"}}, modelregistry.FactEfforts))

	// A fact an administrator set is theirs: the page does not offer the
	// published one in its place.
	saved.Available[0] = saved.Available[0].WithSource(modelregistry.FactTemperature, modelregistry.SourceAdmin)
	detail, err = catalog.Detail(t.Context(), saved, "gateway/gpt-6.1-sol:main")
	require.NoError(t, err)
	assert.Empty(t, fact(detail, modelregistry.FactTemperature).Published)
	assert.Equal(t, "set by an administrator", fact(detail, modelregistry.FactTemperature).Source)

	// The model whose catalog entry is spelt another way: nothing is
	// matched, and the entry is offered.
	detail, err = catalog.Detail(t.Context(), saved, "gateway/deepseek-v4.1-flash:main")
	require.NoError(t, err)
	assert.Empty(t, detail.Match.Ref)
	require.NotEmpty(t, detail.Match.Suggestions)
	assert.Equal(t, "fireworks-ai/accounts/fireworks/models/deepseek-v4p1-flash", detail.Match.Suggestions[0].Ref)
	assert.False(t, detail.Differs, "nothing unconfirmed is compared with what is sent")

	// Confirmed, its facts and price are known.
	pin := detail.Match.Suggestions[0].Ref
	published, err := catalog.Published(t.Context(), saved, "gateway/deepseek-v4.1-flash:main", &pin)
	require.NoError(t, err)
	_, _, source, priced := published.Price()
	assert.True(t, priced)
	assert.Equal(t, modelregistry.SourceCatalog, source)
	missing := "openai/gpt-0"
	_, err = catalog.Published(t.Context(), saved, "gateway/deepseek-v4.1-flash:main", &missing)
	require.ErrorIs(t, err, settings.ErrInvalid)
}

// TestTheLeaderRaisesAModelsRetirementOutsideThePages: the job whose model
// is being retired is logged and counted, for an alert, by a leader nobody
// is watching.
func TestTheLeaderRaisesAModelsRetirementOutsideThePages(t *testing.T) {
	dayBefore := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	catalog := factsCatalog(t, dayBefore, gatewayListing())
	pool, err := pgxpool.New(t.Context(), testdb.URL(t, "cmd_model_attention"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `DELETE FROM connector_settings WHERE connector = 'models'`)
	require.NoError(t, err)
	store := settings.NewStore(pool, testLogger())
	require.NoError(t, store.Load(t.Context()))
	_, err = store.UpdateModels(t.Context(), store.Models().Version, settings.Actor{Email: "op@example.com"}, "test", func(models *settings.Models) error {
		*models = captionsOn("gemini-3.6-flash")
		return nil
	})
	require.NoError(t, err)

	var logged bytes.Buffer
	runner := modelAttentionRunner(catalog, store, slog.New(slog.NewTextHandler(&logged, nil)))
	require.NoError(t, runner.Run(t.Context()))
	assert.Contains(t, logged.String(), "a model in use needs attention")
	assert.Contains(t, logged.String(), "job=image_captions")
	assert.Contains(t, logged.String(), "retires on 2026-11-19 (tomorrow)")
	value := func(job string) float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		require.NoError(t, err)
		for _, family := range families {
			if family.GetName() != "diderot_model_attention" {
				continue
			}
			for _, metric := range family.GetMetric() {
				if metric.GetLabel()[0].GetValue() == job {
					return metric.GetGauge().GetValue()
				}
			}
		}
		return -1
	}
	assert.InDelta(t, 1.0, value("image_captions"), 1e-9)
	assert.InDelta(t, 0.0, value("ask"), 1e-9)

	// Said once, not every ten minutes; and cleared when the job has
	// another model.
	logged.Reset()
	require.NoError(t, runner.Run(t.Context()))
	assert.Empty(t, logged.String())
	_, err = store.UpdateModels(t.Context(), store.Models().Version, settings.Actor{Email: "op@example.com"}, "test", func(models *settings.Models) error {
		*models = captionsOn("gemini-3.8-flash")
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(t.Context()))
	assert.Contains(t, logged.String(), "no longer needs attention")
	assert.InDelta(t, 0.0, value("image_captions"), 1e-9)
}

func TestStalePublishedFactsNeedAttention(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	catalog := factsCatalog(t, now, gatewayListing())
	published := publishedFixture(t)
	status := modelfacts.Status{
		modelfacts.SourceModelsDev: {FetchedAt: now.Add(-3 * 24 * time.Hour), AttemptedAt: now, Error: "it could not be reached", FailingSince: now.Add(-2 * 24 * time.Hour)},
		modelfacts.SourceLiteLLM:   {FetchedAt: now},
	}
	catalog.loadFacts = func(context.Context) (*modelfacts.Catalog, modelfacts.Status, error) { return published, status, nil }
	facts, err := catalog.FactsStatus(t.Context(), settings.Models{})
	require.NoError(t, err)
	assert.Empty(t, facts.Attention, "three days old still serves")
	require.Len(t, facts.Lists, 2)
	assert.Equal(t, "it could not be reached", facts.Lists[0].Error)
	assert.Positive(t, facts.Models)

	status[modelfacts.SourceModelsDev] = modelfacts.ListStatus{FetchedAt: now.Add(-9 * 24 * time.Hour), AttemptedAt: now, Error: "it could not be reached"}
	facts, err = catalog.FactsStatus(t.Context(), settings.Models{})
	require.NoError(t, err)
	assert.Contains(t, facts.Attention, "models.dev has not been read since 2026-11-09 (it could not be reached)")

	// Turned off, nothing is stale and nothing is used.
	facts, err = catalog.FactsStatus(t.Context(), settings.Models{PublishedFactsOff: true})
	require.NoError(t, err)
	assert.True(t, facts.Off)
	assert.Empty(t, facts.Attention)
	assert.Zero(t, facts.Models)
}

// requestCount is how many requests the recorder has received.
func requestCount(r *inferenceRecorder) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}
