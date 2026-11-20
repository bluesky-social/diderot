package tavily

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureCredential = "tvly-fixture-value"
	fixtureAnswer     = "a page body nobody should see in a report"
)

func webDraft(baseURL, credential string) settings.Resolved {
	return settings.NewResolved(ConnectorName, settings.Values{FieldBaseURL: baseURL},
		map[string]string{FieldAPIKey: credential})
}

func TestWebSearchConnectorFields(t *testing.T) {
	require.NotPanics(t, func() { settings.NewRegistry(Connector{}) })
	assert.Equal(t, "DIDEROT_TAVILY_API_KEY", settings.EnvName(ConnectorName, FieldAPIKey),
		"an existing deployment's key variable keeps working")

	values := settings.Values{FieldBaseURL: ""}
	require.NoError(t, Connector{}.Validate(values))
	values = settings.Values{FieldBaseURL: "https://proxy.example.com/tavily/"}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, "https://proxy.example.com/tavily", values[FieldBaseURL])
	for _, bad := range []string{"api.tavily.com", "ftp://api.tavily.com", "https://", "https://u:p@api.tavily.com", "https://api.tavily.com?x=1"} {
		var errs settings.FieldErrors
		require.ErrorAs(t, Connector{}.Validate(settings.Values{FieldBaseURL: bad}), &errs, bad)
		assert.Contains(t, errs[FieldBaseURL], "http or https URL", bad)
	}
}

func TestWebSearchConnectorTest(t *testing.T) {
	var status int
	var searched searchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/search", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&searched))
		if r.Header.Get("Authorization") != "Bearer "+fixtureCredential {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":{"error":"` + fixtureAnswer + `"}}`))
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(fixtureAnswer))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"query": searched.Query,
			"results": []any{map[string]any{"title": "t", "url": "https://example.com", "content": fixtureAnswer}}})
	}))
	defer server.Close()
	connector := Connector{HTTP: server.Client()}

	report := connector.Test(context.Background(), webDraft(server.URL, fixtureCredential))
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Tavily answered a one-result search.", report.Checks[0].Detail)
	assert.Equal(t, 1, searched.MaxResults, "the smallest search")
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixtureCredential)
	assert.NotContains(t, string(encoded), fixtureAnswer, "the report never echoes Tavily")

	for _, test := range []struct {
		name, credential, detail string
		status                   int
	}{
		{name: "rejected key", credential: "tvly-revoked", detail: "Tavily rejected the API key."},
		{name: "rate limited", credential: fixtureCredential, status: http.StatusTooManyRequests, detail: "limiting how fast"},
		{name: "out of credits", credential: fixtureCredential, status: 432, detail: "used its Tavily plan's credits"},
		{name: "server error", credential: fixtureCredential, status: http.StatusBadGateway, detail: "Tavily answered HTTP 502."},
		{name: "not Tavily", credential: fixtureCredential, status: http.StatusOK, detail: "didn't answer like Tavily's API"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status = test.status
			report := connector.Test(context.Background(), webDraft(server.URL, test.credential))
			require.False(t, report.Passed())
			require.Len(t, report.Checks, 1)
			assert.Contains(t, report.Checks[0].Detail, test.detail)
			assert.NotEmpty(t, report.Checks[0].Remedy)
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), fixtureAnswer)
		})
	}

	// The remedy for a rejected key says where the key comes from.
	report = connector.Test(context.Background(), webDraft(server.URL, "tvly-revoked"))
	assert.Equal(t, "Paste an API key from app.tavily.com; it starts with tvly-.", report.Checks[0].Remedy)
	report = connector.Test(context.Background(), webDraft(server.URL, "tvly-revoked").WithSecretEnv(FieldAPIKey, "DIDEROT_TAVILY_API_KEY"))
	assert.Equal(t, "Set DIDEROT_TAVILY_API_KEY in the deployment to an API key from app.tavily.com; it starts with tvly-.",
		report.Checks[0].Remedy)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	report = Connector{}.Test(context.Background(), webDraft(closed.URL, fixtureCredential))
	require.False(t, report.Passed())
	assert.Equal(t, "Couldn't reach "+closed.URL+".", report.Checks[0].Detail)
}
