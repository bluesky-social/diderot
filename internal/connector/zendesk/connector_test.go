package zendesk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureClientValue = "zendesk-fixture-value"
	fixtureErrorBody   = "an upstream error body no report should show"
)

// fakeZendeskTest serves the token exchange, brands, and groups Test reads.
type fakeZendeskTest struct {
	server   *httptest.Server
	groups   []int64
	brandsOK bool
	calls    atomic.Int32
	// fileStorage is the connector's: the deployment has file storage.
	fileStorage bool
}

func newFakeZendeskTest(t *testing.T) *fakeZendeskTest {
	t.Helper()
	fake := &fakeZendeskTest{groups: []int64{7, 360000000042}, brandsOK: true}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		switch r.URL.Path {
		case "/oauth/tokens":
			var grant struct {
				ClientSecret string `json:"client_secret"`
				Scope        string `json:"scope"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&grant))
			assert.Equal(t, "read", grant.Scope)
			if grant.ClientSecret != fixtureClientValue {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(fixtureErrorBody))
				return
			}
			respond(t, w, map[string]any{"access_token": "granted", "token_type": "bearer", "scope": "read", "expires_in": 1800})
			return
		}
		require.Equal(t, "Bearer granted", r.Header.Get("Authorization"))
		require.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/api/v2/brands.json":
			if !fake.brandsOK {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(fixtureErrorBody))
				return
			}
			respond(t, w, map[string]any{"brands": []map[string]any{
				{"name": "Side Brand", "default": false}, {"name": "Example Support", "default": true},
			}})
		case "/api/v2/groups.json":
			groups := []map[string]any{}
			for _, id := range fake.groups {
				groups = append(groups, map[string]any{"id": id})
			}
			respond(t, w, map[string]any{"groups": groups, "meta": map[string]any{"has_more": false}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeZendeskTest) test(t *testing.T, values settings.Values, secret string) settings.TestReport {
	t.Helper()
	secrets := map[string]string{}
	if secret != "" {
		secrets[FieldOAuthSecret] = secret
	}
	base := settings.Values{FieldSubdomain: "example", FieldOAuthClientID: "diderot-ingest"}
	for key, value := range values {
		base[key] = value
	}
	report := Connector{Origin: f.server.URL, FileStorage: f.fileStorage}.Test(context.Background(), settings.NewResolved(ConnectorName, base, secrets))
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixtureErrorBody)
	assert.NotContains(t, string(encoded), fixtureClientValue)
	assert.NotContains(t, string(encoded), "granted")
	return report
}

func check(t *testing.T, report settings.TestReport, name string) settings.Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, report.Checks)
	return settings.Check{}
}

// TestConnectorDefinition pins the field an existing deployment's secret variable sets.
func TestConnectorDefinition(t *testing.T) {
	def := Connector{}
	require.NotPanics(t, func() { settings.NewRegistry(def) })
	assert.Equal(t, "zendesk", def.Name())
	assert.Equal(t, "DIDEROT_ZENDESK_OAUTH_SECRET", settings.EnvName(def.Name(), FieldOAuthSecret))
	required := map[string]bool{}
	for _, field := range def.Fields() {
		required[field.Key] = field.Required
	}
	assert.Equal(t, map[string]bool{
		FieldSubdomain: true, FieldOAuthClientID: true, FieldOAuthSecret: true,
		FieldIndexedGroupIDs: true, FieldDocIngest: false,
	}, required, "an empty group list would remove every indexed ticket")
}

func TestConnectorValidate(t *testing.T) {
	values := settings.Values{FieldSubdomain: "ExampleCo", FieldIndexedGroupIDs: []string{"360000000042", " 7", "007"}}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, "exampleco", values[FieldSubdomain])
	assert.Equal(t, []string{"360000000042", "7"}, values[FieldIndexedGroupIDs])

	for _, bad := range []settings.Values{
		{FieldSubdomain: "example.zendesk.com"},
		{FieldSubdomain: "https://example"},
		{FieldIndexedGroupIDs: []string{"support"}},
		{FieldIndexedGroupIDs: []string{"0"}},
	} {
		err := Connector{}.Validate(bad)
		var fieldErrs settings.FieldErrors
		require.ErrorAs(t, err, &fieldErrs, "%v", bad)
		for key := range bad {
			assert.NotEmpty(t, fieldErrs[key], "%v", bad)
		}
	}
}

func TestSettingsFrom(t *testing.T) {
	s := SettingsFrom(settings.NewResolved(ConnectorName, settings.Values{
		FieldSubdomain: "example", FieldOAuthClientID: "client", FieldIndexedGroupIDs: []string{"7", "360000000042"},
		FieldDocIngest: true,
	}, map[string]string{FieldOAuthSecret: fixtureClientValue}))
	assert.Equal(t, []int64{7, 360000000042}, s.IndexedGroups)
	assert.True(t, s.DocIngest)
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v %s", s, s, s, s), fixtureClientValue)
}

func TestConnectorTestPasses(t *testing.T) {
	fake := newFakeZendeskTest(t)
	report := fake.test(t, settings.Values{FieldIndexedGroupIDs: []string{"360000000042"}}, fixtureClientValue)
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Signed in to example.zendesk.com, account Example Support.", check(t, report, "OAuth client").Detail)
	assert.Equal(t, "All 1 groups to index exist, of 2 in the account.", check(t, report, "Groups").Detail)
	account, ok := report.Fact("account")
	assert.True(t, ok)
	assert.Equal(t, "Example Support", account)
}

// Index documents keeps every attachment in file storage; on a deployment
// without it the setting would pass its Test and extract nothing, so the Test
// fails it, whatever else fails first.
func TestConnectorTestChecksDocumentIngestStorage(t *testing.T) {
	fake := newFakeZendeskTest(t)
	with := settings.Values{FieldIndexedGroupIDs: []string{"7"}, FieldDocIngest: true}
	report := fake.test(t, with, fixtureClientValue)
	assert.False(t, report.Passed())
	docs := check(t, report, "Index documents")
	assert.False(t, docs.Passed)
	assert.Equal(t, "Indexing documents needs file storage, and this deployment has none.", docs.Detail)
	assert.Contains(t, docs.Remedy, "DIDEROT_IMAGES_BUCKET")
	assert.True(t, check(t, report, "Groups").Passed, "only the storage is missing")

	report = fake.test(t, with, "another-value")
	assert.False(t, check(t, report, "Index documents").Passed, "reported though the client is refused")

	fake.fileStorage = true
	report = fake.test(t, with, fixtureClientValue)
	require.True(t, report.Passed(), "%+v", report)
	assert.True(t, check(t, report, "Index documents").Passed)

	fake.fileStorage = false
	report = fake.test(t, settings.Values{FieldIndexedGroupIDs: []string{"7"}}, fixtureClientValue)
	require.True(t, report.Passed(), "off, Index documents needs nothing: %+v", report)
	for _, c := range report.Checks {
		assert.NotEqual(t, "Index documents", c.Name)
	}
}

func TestConnectorTestFails(t *testing.T) {
	t.Run("unknown group", func(t *testing.T) {
		fake := newFakeZendeskTest(t)
		report := fake.test(t, settings.Values{FieldIndexedGroupIDs: []string{"7", "99"}}, fixtureClientValue)
		assert.True(t, check(t, report, "OAuth client").Passed)
		groups := check(t, report, "Groups")
		assert.False(t, groups.Passed)
		assert.Equal(t, "No group has the ID 99.", groups.Detail)
	})
	t.Run("no groups", func(t *testing.T) {
		fake := newFakeZendeskTest(t)
		report := fake.test(t, nil, fixtureClientValue)
		assert.True(t, check(t, report, "OAuth client").Passed)
		groups := check(t, report, "Groups")
		assert.False(t, groups.Passed)
		assert.NotEmpty(t, groups.Remedy)
	})
	t.Run("refused client", func(t *testing.T) {
		fake := newFakeZendeskTest(t)
		report := fake.test(t, nil, "another-value")
		client := check(t, report, "OAuth client")
		assert.False(t, client.Passed)
		assert.Equal(t, "Zendesk refused the OAuth client (HTTP 401).", client.Detail)
		assert.False(t, check(t, report, "Groups").Passed)
	})
	t.Run("account unreadable", func(t *testing.T) {
		fake := newFakeZendeskTest(t)
		fake.brandsOK = false
		report := fake.test(t, nil, fixtureClientValue)
		client := check(t, report, "OAuth client")
		assert.False(t, client.Passed)
		assert.Contains(t, client.Detail, "HTTP 403")
	})
	t.Run("missing secret", func(t *testing.T) {
		fake := newFakeZendeskTest(t)
		report := fake.test(t, nil, "")
		assert.False(t, check(t, report, "OAuth client").Passed)
		assert.Zero(t, fake.calls.Load())
	})
}
