package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureCredential = "pd-fixture-value"
	fixtureSigning    = "signing-fixture-new, signing-fixture-old"
	fixtureIncident   = "an incident title nobody should see in a report"
)

func pagerDutyDraft(credential, signing, email string) settings.Resolved {
	return settings.NewResolved(ConnectorName, settings.Values{
		FieldFromEmail: email, FieldSlackChannelID: "C0EXAMPLE01", FieldSlackChannelName: "bot-alerts",
		FieldAllowedScopes: []string{"public", "internal"},
	}, map[string]string{FieldAPIToken: credential, FieldWebhookSecrets: signing})
}

func checksByName(report settings.TestReport) map[string]settings.Check {
	out := map[string]settings.Check{}
	for _, check := range report.Checks {
		out[check.Name] = check
	}
	return out
}

func TestPagerDutyConnectorFields(t *testing.T) {
	require.NotPanics(t, func() { settings.NewRegistry(Connector{}) })
	for field, env := range map[string]string{
		FieldAPIToken: "DIDEROT_PAGERDUTY_API_TOKEN", FieldWebhookSecrets: "DIDEROT_PAGERDUTY_WEBHOOK_SECRETS",
	} {
		assert.Equal(t, env, settings.EnvName(ConnectorName, field), "an existing deployment's secret variables keep working")
	}

	values := settings.Values{FieldFromEmail: "oncall@example.com", FieldSlackChannelID: "C0EXAMPLE01", FieldSlackChannelName: "#bot-alerts"}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, "bot-alerts", values[FieldSlackChannelName])
	values = settings.Values{FieldFromEmail: "oncall@example.com", FieldSlackChannelID: "C0EXAMPLE01", FieldSlackChannelName: ""}
	require.NoError(t, Connector{}.Validate(values), "the channel name is optional")

	var errs settings.FieldErrors
	require.ErrorAs(t, Connector{}.Validate(settings.Values{
		FieldFromEmail: "On Call <oncall@example.com>", FieldSlackChannelID: "#bot-alerts", FieldSlackChannelName: "bot alerts",
	}), &errs)
	assert.Contains(t, errs[FieldFromEmail], "email address")
	assert.Contains(t, errs[FieldSlackChannelID], "Slack channel")
	assert.Contains(t, errs[FieldSlackChannelName], "channel name")

	s := SettingsFrom(pagerDutyDraft(fixtureCredential, " signing-fixture-new, ,signing-fixture-old ", "oncall@example.com"))
	assert.Equal(t, []string{"signing-fixture-new", "signing-fixture-old"}, s.WebhookSecrets, "today's comma-separated format")
	assert.Equal(t, []string{"public", "internal"}, s.AllowedScopes)
	assert.Equal(t, "C0EXAMPLE01", s.SlackChannelID)
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(verb, s)
		assert.NotContains(t, formatted, fixtureCredential, verb)
		assert.NotContains(t, formatted, "signing-fixture", verb)
	}
}

// fakePagerDuty serves the read-only calls Test makes.
func fakePagerDuty(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method, "Test only reads")
		if r.Header.Get("Authorization") != "Token token="+fixtureCredential {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(fixtureIncident))
			return
		}
		switch r.URL.Path {
		case "/abilities":
			_, _ = w.Write([]byte(`{"abilities":["teams"]}`))
		case "/users":
			users := []any{map[string]any{"email": "someone-else@example.com", "name": fixtureIncident}}
			if r.URL.Query().Get("query") == "oncall@example.com" {
				users = append(users, map[string]any{"email": "OnCall@Example.com", "name": "On Call"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPagerDutyConnectorTest(t *testing.T) {
	server := fakePagerDuty(t)
	slackOn := true
	connector := Connector{APIURL: server.URL, HTTP: server.Client(), Slack: func() bool { return slackOn }, Ask: true, Grafana: func() bool { return true }}

	report := connector.Test(context.Background(), pagerDutyDraft(fixtureCredential, fixtureSigning, "oncall@example.com"))
	require.True(t, report.Passed(), "%+v", report)
	got := checksByName(report)
	assert.Len(t, got, 5)
	assert.Equal(t, "oncall@example.com is a PagerDuty user.", got["Notes posted as"].Detail)
	assert.Equal(t, "Webhooks signed with any of the 2 secrets are accepted.", got["Webhook signing secrets"].Detail)
	assert.Contains(t, got["Slack"].Detail, "is enabled")
	assert.Contains(t, got["Ask"].Detail, "Ask is on")
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, hidden := range []string{fixtureCredential, "signing-fixture", fixtureIncident} {
		assert.NotContains(t, string(encoded), hidden, "the report never carries secrets or PagerDuty's answers")
	}

	// Slack and Ask being off doesn't block enabling, but the report says
	// plainly that investigations wait.
	slackOn = false
	for name, test := range map[string]struct {
		connector Connector
		check     string
		detail    string
	}{
		"slack off":  {connector, "Slack", "The Slack connector is off. Investigations need it"},
		"ask off":    {Connector{APIURL: server.URL, HTTP: server.Client()}, "Ask", "Ask is off in this deployment"},
		"no grafana": {Connector{APIURL: server.URL, HTTP: server.Client(), Ask: true}, "Ask", "no Grafana connection"},
	} {
		t.Run(name, func(t *testing.T) {
			report := test.connector.Test(context.Background(), pagerDutyDraft(fixtureCredential, fixtureSigning, "oncall@example.com"))
			assert.True(t, report.Passed())
			assert.Contains(t, checksByName(report)[test.check].Detail, test.detail)
		})
	}
}

func TestPagerDutyConnectorTestFailures(t *testing.T) {
	server := fakePagerDuty(t)
	connector := Connector{APIURL: server.URL, HTTP: server.Client(), Ask: true, Grafana: func() bool { return true }}

	report := connector.Test(context.Background(), pagerDutyDraft("pd-revoked", fixtureSigning, "oncall@example.com"))
	got := checksByName(report)
	require.False(t, report.Passed())
	assert.Equal(t, "PagerDuty rejected the API token.", got["API token"].Detail)
	assert.Equal(t, "Paste a REST API key from PagerDuty (Integrations, API Access Keys).", got["API token"].Remedy)
	assert.Equal(t, "Not checked: the API token didn't work.", got["Notes posted as"].Detail)
	got = checksByName(connector.Test(context.Background(), pagerDutyDraft("pd-revoked", fixtureSigning, "oncall@example.com").
		WithSecretEnv(FieldAPIToken, "DIDEROT_PAGERDUTY_API_TOKEN")))
	assert.Equal(t, "Set DIDEROT_PAGERDUTY_API_TOKEN in the deployment to a REST API key from PagerDuty (Integrations, API Access Keys).",
		got["API token"].Remedy, "the remedy says where the token comes from")

	report = connector.Test(context.Background(), pagerDutyDraft(fixtureCredential, " , ", "nobody@example.com"))
	got = checksByName(report)
	require.False(t, report.Passed())
	assert.True(t, got["API token"].Passed)
	assert.Equal(t, "No PagerDuty user has the email nobody@example.com.", got["Notes posted as"].Detail)
	assert.False(t, got["Webhook signing secrets"].Passed)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "someone-else", "only the email the admin entered is named")

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	report = Connector{APIURL: closed.URL}.Test(context.Background(), pagerDutyDraft(fixtureCredential, fixtureSigning, "oncall@example.com"))
	assert.Equal(t, "Couldn't reach PagerDuty.", checksByName(report)["API token"].Detail)
}
