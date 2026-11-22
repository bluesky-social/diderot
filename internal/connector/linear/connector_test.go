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

// fakeLinearTest is Linear for the fixture credentials: its token endpoint
// signs the fixture application in, and its API answers testQuery for
// fixtureKey and for the application's tokens.
type fakeLinearTest struct {
	server *httptest.Server
	tokens *fakeTokenEndpoint
	teams  []map[string]any
	more   bool
	// calls counts the API requests; tokens counts its own.
	calls atomic.Int32
}

func newFakeLinearTest(t *testing.T) *fakeLinearTest {
	t.Helper()
	fake := &fakeLinearTest{
		tokens: &fakeTokenEndpoint{t: t, expiresIn: thirtyDays},
		teams: []map[string]any{
			{"key": "ENG", "private": false}, {"key": "SUP", "private": false}, {"key": "SEC", "private": true},
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/oauth/token", fake.tokens)
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		// viewer is who the credential acts as: its owner for an API key,
		// the application itself for an application's token.
		var viewer string
		switch auth := r.Header.Get("Authorization"); {
		case auth == fixtureKey:
			viewer = "Diderot Bot"
		case fake.tokens.minted(auth):
			viewer = "Diderot App"
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fixtureErrorBody))
			return
		}
		var request graphqlRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, testQuery, request.Query)
		assert.EqualValues(t, testTeamLimit, request.Variables["first"])
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"viewer":       map[string]any{"name": viewer},
			"organization": map[string]any{"name": "Example Org"},
			"teams":        map[string]any{"nodes": fake.teams, "pageInfo": map[string]any{"hasNextPage": fake.more}},
		}})
	})
	fake.server = httptest.NewServer(mux)
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
	assert.Equal(t, "DIDEROT_LINEAR_OAUTH_SECRET", settings.EnvName(def.Name(), FieldOAuthSecret))
	kinds, required := map[string]settings.FieldKind{}, map[string]bool{}
	for _, field := range def.Fields() {
		kinds[field.Key], required[field.Key] = field.Kind, field.Required
	}
	assert.Equal(t, map[string]settings.FieldKind{
		FieldOAuthClientID: settings.KindText, FieldOAuthSecret: settings.KindSecret,
		FieldToken: settings.KindSecret, FieldExcludeTeams: settings.KindList,
	}, kinds)
	// Either credential signs in, so the form can require neither.
	assert.Equal(t, map[string]bool{
		FieldOAuthClientID: false, FieldOAuthSecret: false, FieldToken: false, FieldExcludeTeams: false,
	}, required)
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

func TestConnectorNewClient(t *testing.T) {
	application := settings.Values{FieldOAuthClientID: fixtureClientID}
	tests := []struct {
		name    string
		values  settings.Values
		secrets map[string]string
		want    string
	}{
		{
			name:    "API key",
			secrets: map[string]string{FieldToken: fixtureKey},
			want:    fixtureKey,
		},
		{
			name:    "OAuth application",
			values:  application,
			secrets: map[string]string{FieldOAuthSecret: fixtureClientSecret},
			want:    "Bearer " + fixtureAppToken(1),
		},
		{
			// A deployment adds the application before it removes the key.
			name:    "OAuth application and API key",
			values:  application,
			secrets: map[string]string{FieldOAuthSecret: fixtureClientSecret, FieldToken: fixtureKey},
			want:    "Bearer " + fixtureAppToken(1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeLinearTest(t)
			connector := Connector{HTTP: fakeLinearClient(fake.server)}
			client, err := connector.NewClient(settings.NewResolved(ConnectorName, tt.values, tt.secrets))
			require.NoError(t, err)
			got, err := client.provider.Authorization(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConnectorNewClientRefusesIncompleteCredentials(t *testing.T) {
	tests := []struct {
		name    string
		values  settings.Values
		secrets map[string]string
		want    error
	}{
		{
			// The API key is no fallback: it would hide a secret that was
			// never set until the key's owner leaves.
			name:    "OAuth application without its secret",
			values:  settings.Values{FieldOAuthClientID: fixtureClientID},
			secrets: map[string]string{FieldToken: fixtureKey},
			want:    errNoOAuthSecret,
		},
		{
			name: "no credentials",
			want: errNoCredentials,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Connector{}.NewClient(settings.NewResolved(ConnectorName, tt.values, tt.secrets))
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

// TestConnectorNewClientSharesItsHTTPClient covers the connector as it
// runs, with no HTTP client of a test's: the token requests, which carry the
// client secret, must go through the client NewClient builds for the API,
// the one that refuses redirects.
func TestConnectorNewClientSharesItsHTTPClient(t *testing.T) {
	client, err := Connector{}.NewClient(settings.NewResolved(ConnectorName,
		settings.Values{FieldOAuthClientID: fixtureClientID}, map[string]string{FieldOAuthSecret: fixtureClientSecret}))
	require.NoError(t, err)
	provider, ok := client.provider.(*oauthTokenProvider)
	require.True(t, ok, "provider is a %T", client.provider)
	assert.Same(t, client.httpClient, provider.httpClient)
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

func TestConnectorTestPassesAsAnOAuthApplication(t *testing.T) {
	fake := newFakeLinearTest(t)
	report := fake.test(t, settings.Values{FieldOAuthClientID: fixtureClientID},
		map[string]string{FieldOAuthSecret: fixtureClientSecret})
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Signed in to Example Org as Diderot App.", check(t, report, "Credentials").Detail)
	assert.Equal(t, "The credentials see 3 teams, 1 of them private and never indexed.", check(t, report, "Teams").Detail)
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
	t.Run("refused OAuth application", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		report := fake.test(t, settings.Values{FieldOAuthClientID: fixtureClientID},
			map[string]string{FieldOAuthSecret: "some-other-secret"})
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "Linear refused the OAuth application (HTTP 401).", credentials.Detail)
		assert.Equal(t, "Check the client ID and that the application has client credentials tokens turned on. "+
			"Paste the application's client secret from Linear's Settings, API.", credentials.Remedy)
		assert.False(t, check(t, report, "Teams").Passed)
		assert.Zero(t, fake.calls.Load())
	})
	t.Run("token endpoint outage", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		fake.tokens.outage = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(fixtureErrorBody))
		}
		report := fake.test(t, settings.Values{FieldOAuthClientID: fixtureClientID},
			map[string]string{FieldOAuthSecret: fixtureClientSecret})
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "Linear answered HTTP 503.", credentials.Detail)
		assert.Zero(t, fake.calls.Load())
	})
	t.Run("unusable token answer", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		fake.tokens.outage = func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"lin_oauth_fixture1","expires_in":2591999,"token_type":"mac"}`))
		}
		report := fake.test(t, settings.Values{FieldOAuthClientID: fixtureClientID},
			map[string]string{FieldOAuthSecret: fixtureClientSecret})
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "Linear's answer was not understood.", credentials.Detail)
		// Nothing the administrator entered is at fault.
		assert.Equal(t, "Test again. If it keeps failing, report it as a Diderot bug.", credentials.Remedy)
		assert.Zero(t, fake.calls.Load())
	})
	t.Run("OAuth application without its secret", func(t *testing.T) {
		fake := newFakeLinearTest(t)
		report := fake.test(t, settings.Values{FieldOAuthClientID: fixtureClientID}, map[string]string{FieldToken: fixtureKey})
		credentials := check(t, report, "Credentials")
		assert.False(t, credentials.Passed)
		assert.Equal(t, "No client secret for the OAuth application.", credentials.Detail)
		assert.Equal(t, "Paste the application's client secret from Linear's Settings, API.", credentials.Remedy)
		assert.False(t, check(t, report, "Teams").Passed)
		assert.Zero(t, fake.calls.Load())
		assert.Zero(t, fake.tokens.mints.Load())
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
