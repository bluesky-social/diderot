package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureKey       = "linear-fixture-key"
	fixtureErrorBody = "an upstream error body no report should show"
)

// fakeLinearTest answers testQuery for fixtureKey.
type fakeLinearTest struct {
	server *httptest.Server
	teams  []map[string]any
	more   bool
	calls  atomic.Int32
}

func newFakeLinearTest(t *testing.T) *fakeLinearTest {
	t.Helper()
	fake := &fakeLinearTest{teams: []map[string]any{
		{"key": "ENG", "private": false}, {"key": "SUP", "private": false}, {"key": "SEC", "private": true},
	}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		if r.Header.Get("Authorization") != fixtureKey {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fixtureErrorBody))
			return
		}
		var request graphqlRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, testQuery, request.Query)
		assert.EqualValues(t, testTeamLimit, request.Variables["first"])
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"viewer":       map[string]any{"name": "Diderot Bot"},
			"organization": map[string]any{"name": "Example Org"},
			"teams":        map[string]any{"nodes": fake.teams, "pageInfo": map[string]any{"hasNextPage": fake.more}},
		}})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeLinearTest) test(t *testing.T, values settings.Values, secrets map[string]string) settings.TestReport {
	t.Helper()
	httpClient := f.server.Client()
	httpClient.Transport = &rewriteTransport{base: f.server.Client().Transport, target: f.server.URL}
	report := Connector{HTTP: httpClient}.Test(context.Background(), settings.NewResolved(ConnectorName, values, secrets))
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixtureErrorBody)
	for _, secret := range secrets {
		assert.NotContains(t, string(encoded), secret)
	}
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

// TestConnectorDefinition pins the fields an existing deployment's secret variables set.
func TestConnectorDefinition(t *testing.T) {
	def := Connector{}
	require.NotPanics(t, func() { settings.NewRegistry(def) })
	assert.Equal(t, "linear", def.Name())
	assert.Equal(t, "DIDEROT_LINEAR_TOKEN", settings.EnvName(def.Name(), FieldToken))
	kinds, required := map[string]settings.FieldKind{}, map[string]bool{}
	for _, field := range def.Fields() {
		kinds[field.Key], required[field.Key] = field.Kind, field.Required
	}
	assert.Equal(t, map[string]settings.FieldKind{FieldToken: settings.KindSecret, FieldExcludeTeams: settings.KindList}, kinds)
	assert.Equal(t, map[string]bool{FieldToken: true, FieldExcludeTeams: false}, required)
}

func TestConnectorValidate(t *testing.T) {
	values := settings.Values{FieldExcludeTeams: []string{"sup", "SUP", " ops "}}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, []string{"SUP", "OPS"}, values[FieldExcludeTeams])

	err := Connector{}.Validate(settings.Values{FieldExcludeTeams: []string{"two words"}})
	var fieldErrs settings.FieldErrors
	require.ErrorAs(t, err, &fieldErrs)
	assert.Contains(t, fieldErrs[FieldExcludeTeams], "two words")

	assert.NoError(t, Connector{}.Validate(settings.Values{}))
}

func TestConnectorTestPasses(t *testing.T) {
	fake := newFakeLinearTest(t)
	report := fake.test(t, settings.Values{FieldExcludeTeams: []string{"SUP"}}, map[string]string{FieldToken: fixtureKey})
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Signed in to Example Org as Diderot Bot.", check(t, report, "Credentials").Detail)
	assert.Equal(t, "The credentials see 3 teams, 1 of them private and never indexed.", check(t, report, "Teams").Detail)
	organization, ok := report.Fact("organization")
	assert.True(t, ok)
	assert.Equal(t, "Example Org", organization)

	// A key matches whatever case it was saved in.
	report = fake.test(t, settings.Values{FieldExcludeTeams: []string{"sup"}}, map[string]string{FieldToken: fixtureKey})
	assert.True(t, report.Passed())
}

func TestConnectorTestFails(t *testing.T) {
	t.Run("unknown excluded team", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		report := fake.test(t, settings.Values{FieldExcludeTeams: []string{"SUP", "NOPE"}}, map[string]string{FieldToken: fixtureKey})
		assert.True(t, check(t, report, "Credentials").Passed)
		teams := check(t, report, "Teams")
		assert.False(t, teams.Passed)
		assert.Equal(t, "No team has the key NOPE.", teams.Detail)
		assert.NotEmpty(t, teams.Remedy)
	})
	t.Run("more teams than one page", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		fake.more = true
		report := fake.test(t, settings.Values{FieldExcludeTeams: []string{"NOPE"}}, map[string]string{FieldToken: fixtureKey})
		assert.True(t, report.Passed(), "an unseen team may be on a later page")
		assert.Contains(t, check(t, report, "Teams").Detail, "more than 3 teams")
	})
	t.Run("rejected key", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		report := fake.test(t, nil, map[string]string{FieldToken: "some-other-key"})
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "Linear refused the credentials (HTTP 400).", credentials.Detail)
		assert.False(t, check(t, report, "Teams").Passed)
	})
	t.Run("no API key", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		report := fake.test(t, nil, nil)
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "No API key.", credentials.Detail)
		assert.False(t, check(t, report, "Teams").Passed)
		assert.Zero(t, fake.calls.Load())
	})
}
