package modelprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

func TestNamesAndKeyVariables(t *testing.T) {
	assert.NoError(t, CheckName("gateway"))
	assert.Error(t, CheckName("Gateway"))
	assert.Error(t, CheckName("1st"))
	assert.Equal(t, "provider:gateway", Row("gateway"))
	assert.Equal(t, "gateway", Name(Row("gateway")))
	assert.Equal(t, "DIDEROT_PROVIDER_GATEWAY_KEY", EnvName("gateway", FieldKey))
	assert.Equal(t, map[string]string{"DIDEROT_PROVIDER_GATEWAY_KEY": "k"}, ReadEnv([]string{
		"DIDEROT_PROVIDER_GATEWAY_KEY= k ", "DIDEROT_PROVIDER_EMPTY_KEY=", "DIDEROT_CONNECTION_X_TOKEN=t", "OTHER=1",
	}))
}

func TestValidateBaseURL(t *testing.T) {
	cluster := Definitions{}.Define(Row("p"))
	standalone := Definitions{Standalone: true}.Define(Row("p"))
	for _, tc := range []struct {
		def  settings.Definition
		url  string
		want string
	}{
		{cluster, "https://api.openai.com/v1/", ""},
		{cluster, "http://gateway.example.svc.cluster.local:4001/v1", ""},
		{cluster, "http://gateway.example.com/v1", "use https"},
		{cluster, "http://127.0.0.1:8080/v1", "use https"},
		{standalone, "http://embedder/v1", ""},
		{cluster, "ftp://x", "enter an http or https address"},
		{cluster, "https://user:pass@x/v1", "enter an http or https address"},
	} {
		values := settings.Values{FieldBaseURL: tc.url}
		err := tc.def.Validate(values)
		if tc.want == "" {
			assert.NoError(t, err, tc.url)
			continue
		}
		require.Error(t, err, tc.url)
		assert.Contains(t, err.Error(), tc.want, tc.url)
	}
	values := settings.Values{FieldBaseURL: "https://api.openai.com/v1/"}
	require.NoError(t, cluster.Validate(values))
	assert.Equal(t, "https://api.openai.com/v1", values[FieldBaseURL], "a trailing slash is trimmed")
	err := cluster.Validate(settings.Values{FieldModels: []string{" padded"}})
	assert.ErrorContains(t, err, "without spaces")
}

func resolved(url, key string, offered ...string) settings.Resolved {
	secrets := map[string]string{}
	if key != "" {
		secrets[FieldKey] = key
	}
	return settings.NewResolved(Row("p"), settings.Values{
		FieldKind: string(modelregistry.KindOpenAI), FieldBaseURL: url, FieldModels: offered,
	}, secrets)
}

func TestTestListsTheModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a","context_length":1000},{"id":"b"}]}`))
	}))
	defer server.Close()
	def := Definitions{Standalone: true}.Define(Row("p"))

	report := def.Test(context.Background(), resolved(server.URL+"/v1", "k"))
	require.True(t, report.Passed())
	assert.Equal(t, "The provider lists 2 models.", report.Checks[0].Detail)
	require.Len(t, report.Facts, 2)
	assert.Equal(t, FactModel, report.Facts[0].Key)
	assert.Equal(t, "a", report.Facts[0].Label)
	assert.JSONEq(t, `{"id":"a","context":1000}`, report.Facts[0].Value)

	report = def.Test(context.Background(), resolved(server.URL+"/v1", "k", "a", "c"))
	require.True(t, report.Passed())
	assert.Equal(t, "The provider lists 2 models. 2 models offered; the list does not have c, which a job's test will check.", report.Checks[0].Detail)

	report = def.Test(context.Background(), resolved(server.URL+"/v1", "wrong"))
	assert.False(t, report.Passed())
	assert.Contains(t, report.Checks[0].Detail, "the provider refused the key (HTTP 401)")
	assert.NotContains(t, report.Checks[0].Detail, "wrong")

	report = def.Test(context.Background(), resolved(server.URL+"/v1", ""))
	assert.False(t, report.Passed())
	assert.Equal(t, "There is no API key.", report.Checks[0].Detail)
}

func TestTestOfAProviderWithoutAList(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	def := Definitions{Standalone: true}.Define(Row("p"))
	report := def.Test(context.Background(), resolved(server.URL+"/v1", "k"))
	assert.False(t, report.Passed(), "nothing would be offered")
	assert.Equal(t, "Enter the IDs of the models it offers under Models offered, then save.", report.Checks[0].Remedy)

	report = def.Test(context.Background(), resolved(server.URL+"/v1", "k", "embed"))
	assert.True(t, report.Passed())
	assert.Equal(t, []settings.Fact{{Key: FactNoListing, Label: "No model list", Value: "true"}}, report.Facts)
}

func TestMenuOf(t *testing.T) {
	saved := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	view := settings.View{
		Name: Row("p"), Enabled: true, Version: 2,
		Settings: settings.Values{FieldKind: "openai"},
		LastTest: &settings.TestRecord{Passed: true, At: saved, TestReport: settings.TestReport{
			Checks: []settings.Check{{Name: "List models", Passed: true}},
			Facts:  []settings.Fact{{Key: FactModel, Label: "a", Value: `{"id":"a"}`}, {Key: FactModel, Label: "b", Value: `{"id":"b","retires":"2027-01-01"}`}},
		}},
	}
	menu := MenuOf(view, nil)
	assert.Equal(t, "p", menu.Name)
	assert.Equal(t, modelregistry.KindOpenAI, menu.Kind)
	assert.Equal(t, []modelregistry.Listed{{ID: "a"}, {ID: "b", Retires: "2027-01-01"}}, menu.Models)
	assert.True(t, menu.InListing("b"))

	// A newer check of the saved version replaces the save's test; one of
	// an older version does not.
	failed := Check{Version: 2, Test: settings.TestRecord{At: saved.Add(time.Hour), TestReport: settings.TestReport{
		Checks: []settings.Check{{Name: "List models", Detail: "the provider answered HTTP 500"}},
	}}}
	menu = MenuOf(view, &failed)
	assert.Equal(t, "the provider answered HTTP 500", menu.Error)
	assert.Empty(t, menu.Models)
	failed.Version = 1
	assert.Empty(t, MenuOf(view, &failed).Error)

	view.Settings[FieldModels] = []string{"b", "z"}
	menu = MenuOf(view, nil)
	assert.Equal(t, []modelregistry.Listed{{ID: "b", Retires: "2027-01-01"}, {ID: "z"}}, menu.Models, "Models offered narrows and extends the menu")
	assert.False(t, menu.InListing("z"))
}

func TestSettingsHideTheKey(t *testing.T) {
	s := FromResolved(resolved("https://x.example/v1/", "sk-secret"))
	assert.Equal(t, "https://x.example/v1", s.Provider.Endpoint.BaseURL)
	assert.Equal(t, "sk-secret", s.Provider.Endpoint.APIKey)
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		assert.NotContains(t, fmt.Sprintf(verb, s), "sk-secret")
	}
}
