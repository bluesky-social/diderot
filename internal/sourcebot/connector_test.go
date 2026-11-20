package sourcebot

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
	fixtureCredential = "sbk_fixture_value"
	fixtureCode       = "func privateCodeNobodyShouldSeeInAReport()"
)

func codeDraft(baseURL, credential string) settings.Resolved {
	return settings.NewResolved(ConnectorName, settings.Values{FieldURL: baseURL},
		map[string]string{FieldAPIKey: credential})
}

func TestCodeSearchConnectorFields(t *testing.T) {
	require.NotPanics(t, func() { settings.NewRegistry(Connector{}) })
	assert.Equal(t, "DIDEROT_SOURCEBOT_API_KEY", settings.EnvName(ConnectorName, FieldAPIKey),
		"the key keeps its variable")

	values := settings.Values{FieldURL: "http://sourcebot.example.svc.cluster.local:3000/", FieldRepoPrefix: "github.com/example/"}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, "http://sourcebot.example.svc.cluster.local:3000", values[FieldURL])

	var errs settings.FieldErrors
	require.ErrorAs(t, Connector{}.Validate(settings.Values{FieldURL: "sourcebot:3000", FieldRepoPrefix: "github.com/example"}), &errs)
	assert.Contains(t, errs[FieldURL], "http or https URL")
	assert.Contains(t, errs[FieldRepoPrefix], "must end in /")

	s := SettingsFrom(settings.NewResolved(ConnectorName, settings.Values{
		FieldURL: "https://sourcebot.example.com", FieldRepoPrefix: "github.com/example/",
		FieldSearchScope: "every Example repository", FieldReadScope: "an Example repository",
	}, map[string]string{FieldAPIKey: fixtureCredential}))
	assert.Equal(t, Settings{URL: "https://sourcebot.example.com", APIKey: fixtureCredential, RepoPrefix: "github.com/example/",
		SearchScope: "every Example repository", ReadScope: "an Example repository"}, s)
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		assert.NotContains(t, fmt.Sprintf(verb, s), fixtureCredential, verb)
	}
}

func TestCodeSearchConnectorTest(t *testing.T) {
	searchStatus, reposStatus := 0, 0
	var searched searchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureCredential {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(fixtureCode))
			return
		}
		switch r.URL.Path {
		case "/api/search":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&searched))
			if searchStatus != 0 {
				w.WriteHeader(searchStatus)
				_, _ = w.Write([]byte(fixtureCode))
				return
			}
			_, _ = fmt.Fprintf(w, `{"stats":{"actualMatchCount":1,"totalMatchCount":1},"files":[{"repository":"github.com/example/x","chunks":[{"content":%q}]}]}`, fixtureCode)
		case "/api/repos":
			assert.Equal(t, "1", r.URL.Query().Get("perPage"))
			if reposStatus != 0 {
				w.WriteHeader(reposStatus)
				return
			}
			w.Header().Set("X-Total-Count", "163")
			_, _ = w.Write([]byte(`[{"repoName":"github.com/example/x"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	connector := Connector{HTTP: server.Client()}

	report := connector.Test(context.Background(), codeDraft(server.URL+"/", fixtureCredential))
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Sourcebot answered a search with this key. It indexes 163 repositories.", report.Checks[0].Detail)
	assert.Equal(t, 1, searched.Matches, "the smallest search")
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixtureCredential)
	assert.NotContains(t, string(encoded), "privateCode", "the report never echoes Sourcebot")

	reposStatus = http.StatusNotFound
	report = connector.Test(context.Background(), codeDraft(server.URL, fixtureCredential))
	require.True(t, report.Passed(), "a Sourcebot that can't count its repositories still passes")
	assert.Equal(t, "Sourcebot answered a search with this key.", report.Checks[0].Detail)

	for _, test := range []struct {
		name, url, credential, detail string
		status                        int
	}{
		{name: "rejected key", url: server.URL, credential: "sbk_revoked", detail: "Sourcebot rejected the API key."},
		{name: "not Sourcebot's address", url: server.URL + "/elsewhere", credential: fixtureCredential, detail: "no Sourcebot search at this URL"},
		{name: "server error", url: server.URL, credential: fixtureCredential, status: http.StatusBadGateway, detail: "Sourcebot answered HTTP 502."},
		{name: "not Sourcebot", url: server.URL, credential: fixtureCredential, status: http.StatusOK, detail: "didn't answer like Sourcebot"},
	} {
		t.Run(test.name, func(t *testing.T) {
			searchStatus = test.status
			report := connector.Test(context.Background(), codeDraft(test.url, test.credential))
			require.False(t, report.Passed())
			assert.Contains(t, report.Checks[0].Detail, test.detail)
			assert.NotEmpty(t, report.Checks[0].Remedy)
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "privateCode")
		})
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	report = Connector{}.Test(context.Background(), codeDraft(closed.URL, fixtureCredential))
	require.False(t, report.Passed())
	assert.Equal(t, "Couldn't reach Sourcebot.", report.Checks[0].Detail)
}
