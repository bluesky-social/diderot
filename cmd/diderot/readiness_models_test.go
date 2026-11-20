package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

// gatewayCatalog is a catalog of a deployment that runs every job, with
// the given provider rows as its settings cache would hold them.
func gatewayCatalog(t *testing.T, rows ...settings.FamilyRow) *modelCatalog {
	t.Helper()
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	t.Setenv("DIDEROT_IMAGES_BUCKET", "bucket")
	_, catalog := loadInference(t)
	catalog.rows = func() []settings.FamilyRow { return rows }
	catalog.providers = func() []modelregistry.Provider {
		var providers []modelregistry.Provider
		for _, row := range rows {
			if row.Row.Enabled && row.Err == nil {
				providers = append(providers, modelprovider.FromResolved(row.Resolved).Provider)
			}
		}
		return providers
	}
	return catalog
}

// gatewayRow is the gateway's provider row: on or off, with key.
func gatewayRow(enabled bool, key string) settings.FamilyRow {
	secrets := map[string]string{}
	if key != "" {
		secrets[modelprovider.FieldKey] = key
	}
	return settings.FamilyRow{
		Row: settings.Row{Connector: modelprovider.Row("gateway"), Enabled: enabled, Version: 7},
		Resolved: settings.NewResolved(modelprovider.Row("gateway"), settings.Values{
			modelprovider.FieldKind: "openai", modelprovider.FieldBaseURL: "http://gateway.invalid/v1",
		}, secrets),
	}
}

// TestSavedModelsThatCannotRunAreFaults: /readyz fails on what a deployment
// can't run as saved, so a rollout of such pods stops, and on nothing an
// administrator chooses on the Models page.
func TestSavedModelsThatCannotRunAreFaults(t *testing.T) {
	migrated := gatewayMigration(gatewayRoles)

	t.Run("a server nobody has set up is ready", func(t *testing.T) {
		catalog := gatewayCatalog(t)
		assert.Empty(t, catalog.faults(settings.Models{}))
		catalog.hadVectors.Store(true)
		assert.Empty(t, catalog.faults(settings.Models{}), "an upgrade with vectors and nothing saved is set up on the Models page")
	})
	t.Run("the migrated row runs", func(t *testing.T) {
		assert.Empty(t, gatewayCatalog(t, gatewayRow(true, "k")).faults(migrated))
	})
	t.Run("a provider key left out of the deployment", func(t *testing.T) {
		faults := gatewayCatalog(t, gatewayRow(true, "")).faults(migrated)
		require.NotEmpty(t, faults)
		assert.Contains(t, strings.Join(faults, "\n"), "Ask uses the provider gateway, which is on but has no key: set DIDEROT_PROVIDER_GATEWAY_KEY")
		assert.Contains(t, strings.Join(faults, "\n"), "Search uses the provider gateway, which is on but has no key")
	})
	t.Run("a saved key that can't be read", func(t *testing.T) {
		row := gatewayRow(true, "")
		row.Err = errors.New("no config key")
		assert.Contains(t, strings.Join(gatewayCatalog(t, row).faults(migrated), "\n"), "whose saved key can't be read")
	})
	t.Run("request headers the deployment sets that can't be read", func(t *testing.T) {
		row := gatewayRow(true, "k")
		row.Resolved = settings.NewResolved(modelprovider.Row("gateway"), settings.Values{
			modelprovider.FieldKind: "openai", modelprovider.FieldBaseURL: "http://gateway.invalid/v1",
		}, map[string]string{modelprovider.FieldKey: "k", modelprovider.FieldSecretHeaders: "not-a-header"})
		catalog := gatewayCatalog(t, row)
		faults := strings.Join(catalog.faults(migrated), "\n")
		assert.Contains(t, faults, "Ask uses the provider gateway, which is on but has request headers that can't be read: correct DIDEROT_PROVIDER_GATEWAY_SECRET_HEADERS")
		assert.NotContains(t, faults, "not-a-header")
		// And nothing is sent to it without them.
		assert.Empty(t, catalog.resolve(migrated).ask.ID)
	})
	t.Run("a provider that does not exist", func(t *testing.T) {
		assert.Contains(t, strings.Join(gatewayCatalog(t).faults(migrated), "\n"), "Ask uses the provider gateway, which does not exist.")
	})
	t.Run("a provider an administrator turned off is their choice", func(t *testing.T) {
		assert.Empty(t, gatewayCatalog(t, gatewayRow(false, "k")).faults(migrated))
	})
	t.Run("a models row an older version overwrote", func(t *testing.T) {
		// An older version's save keeps the jobs' model IDs and drops what
		// it does not know: the entries and the embedding model.
		overwritten := settings.Models{Roles: map[string]string{}, AskReasoningEffort: "medium"}
		for role, id := range gatewayRoles {
			overwritten.Roles[role] = id
		}
		catalog := gatewayCatalog(t, gatewayRow(true, "k"))
		catalog.hadVectors.Store(true)
		faults := strings.Join(catalog.faults(overwritten), "\n")
		assert.Contains(t, faults, `Ask is saved as "gpt-6.1-sol:main", which names no provider's model`)
		assert.Contains(t, faults, "Passages already have vectors, but no embedding model is saved")

		// The entries alone lost.
		entries := migrated
		entries.Available = nil
		assert.Contains(t, strings.Join(catalog.faults(entries), "\n"),
			"Ask uses gateway/gpt-6.1-sol:main, which the models settings have no entry for")
	})
	t.Run("an entry that no longer answers for its job", func(t *testing.T) {
		broken := migrated
		broken.Available = nil
		for _, entry := range migrated.Available {
			if entry.ID == "gemini-3.8-flash" {
				entry.Images = false
			}
			broken.Available = append(broken.Available, entry)
		}
		faults := gatewayCatalog(t, gatewayRow(true, "k")).faults(broken)
		require.Len(t, faults, 1)
		assert.Contains(t, faults[0], "Image captions uses gateway/gemini-3.8-flash, whose entry does not answer for that job")
	})
}

func TestReadyzSaysWhyTheModelsCannotRun(t *testing.T) {
	probe := func(readiness *httpserver.Readiness) (int, map[string]any) {
		recorder := httptest.NewRecorder()
		readiness.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		return recorder.Code, body
	}

	// Fresh: nothing saved, nothing wrong.
	fresh := gatewayCatalog(t)
	fresh.saved = func() settings.Models { return settings.Models{} }
	readiness := httpserver.NewReadiness()
	gateModelsReadiness(fresh, readiness, testLogger())
	code, _ := probe(readiness)
	assert.Equal(t, http.StatusOK, code, "ready to configure")

	// The gateway's row with the key variable left out of the deployment.
	broken := gatewayCatalog(t, gatewayRow(true, ""))
	broken.saved = func() settings.Models { return gatewayMigration(gatewayRoles) }
	readiness = httpserver.NewReadiness()
	gateModelsReadiness(broken, readiness, testLogger())
	code, body := probe(readiness)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, []any{"models"}, body["not_ready"])
	assert.Contains(t, body["reasons"].(map[string]any)["models"], "DIDEROT_PROVIDER_GATEWAY_KEY")

	// Once the key is there the replica is ready again.
	fixed := gatewayCatalog(t, gatewayRow(true, "k"))
	fixed.saved = broken.saved
	gateModelsReadiness(fixed, readiness, testLogger())
	code, _ = probe(readiness)
	assert.Equal(t, http.StatusOK, code)
}

// TestAskIsNotReadyUntilItsFirstBuildWorks: a replica whose first build of
// a chosen Ask fails stays out of rotation; one that was ready stays ready
// when a later build fails.
func TestAskIsNotReadyUntilItsFirstBuildWorks(t *testing.T) {
	readiness := httpserver.NewReadiness()
	tried := askReadiness(readiness)
	tried(&askBuild{modelID: "gpt"})
	assert.Contains(t, readiness.NotReady()["ask"], "could not be built with its saved model, gpt")
	tried(&askBuild{notSetUp: true})
	assert.Empty(t, readiness.NotReady())
	tried(&askBuild{modelID: "gpt"})
	assert.Empty(t, readiness.NotReady(), "a later failure does not take the admin pages away")
}

// TestFingerprintIsWhatARoleSends: only a change to the requests a role
// makes restarts its work.
func TestFingerprintIsWhatARoleSends(t *testing.T) {
	entry := gatewayMigration(gatewayRoles).Available[11]
	require.Equal(t, "gemini-3.8-flash", entry.ID)
	provider := modelprovider.FromResolved(gatewayRow(true, "k").Resolved).Provider
	bound := boundModel{Model: entry, endpoint: provider.Endpoint}
	print := bound.fingerprint()

	// A save of the provider that changes nothing a request carries, such
	// as the models it offers, gives it a new version and nothing else.
	resaved := gatewayRow(true, "k")
	resaved.Row.Version = 8
	assert.Equal(t, print, boundModel{Model: entry, endpoint: modelprovider.FromResolved(resaved.Resolved).Provider.Endpoint}.fingerprint())

	// A test the entry passes, and where its facts came from, change no
	// request either.
	tested := entry.WithSource(modelregistry.FactImages, modelregistry.SourceAdmin)
	tested.TestedFor = []modelregistry.Role{modelregistry.RoleImageCaptions}
	assert.Equal(t, print, boundModel{Model: tested, endpoint: provider.Endpoint}.fingerprint())

	// A new key, and a changed request field, do.
	rekeyed := modelprovider.FromResolved(gatewayRow(true, "k2").Resolved).Provider
	assert.NotEqual(t, print, boundModel{Model: entry, endpoint: rekeyed.Endpoint}.fingerprint())
	assert.NotContains(t, boundModel{Model: entry, endpoint: rekeyed.Endpoint}.fingerprint(), "k2", "the key itself is not in it")
	changed := entry
	changed.ChatExtraBody = nil
	assert.NotEqual(t, print, boundModel{Model: changed, endpoint: provider.Endpoint}.fingerprint())
}

// TestCanServeNamesWhatAnEntryChangeWouldStop is what the model's page asks
// before it saves a correction of an entry a job uses.
func TestCanServeNamesWhatAnEntryChangeWouldStop(t *testing.T) {
	catalog := gatewayCatalog(t, gatewayRow(true, "k"))
	saved := gatewayMigration(gatewayRoles)
	entry := func(id string) modelregistry.Model {
		for _, entry := range saved.Available {
			if entry.ID == id {
				return entry
			}
		}
		t.Fatalf("no entry %s", id)
		return modelregistry.Model{}
	}
	captioner := entry("gemini-3.8-flash")
	require.NoError(t, catalog.CanServe(saved, "image_captions", captioner))
	// Clearing its extra request fields changes what captions send, and
	// stops nothing.
	cleared := captioner
	cleared.ChatExtraBody = nil
	require.NoError(t, catalog.CanServe(saved, "image_captions", cleared))
	captioner.Images = false
	assert.EqualError(t, catalog.CanServe(saved, "image_captions", captioner), "the job sends images, which the model would no longer accept")

	// This deployment sends Ask's requests with a service tier.
	asker := entry("gpt-6.1-sol:main")
	require.NoError(t, catalog.CanServe(saved, "ask", asker))
	asker.ServiceTier = false
	assert.Error(t, catalog.CanServe(saved, "ask", asker))
}
