package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureCredential = "integration-fixture-value"
	fixturePageTitle  = "A page title no report should show"
	fixtureBody       = "an upstream error body no report should show"
)

// fakeNotion serves users/me and search for fixtureCredential.
type fakeNotion struct {
	server *httptest.Server
	pages  int
	more   bool
	self   map[string]any
	calls  atomic.Int32
}

func newFakeNotion(t *testing.T) *fakeNotion {
	t.Helper()
	fake := &fakeNotion{pages: 3, self: map[string]any{
		"object": "user", "id": "bot-1", "type": "bot", "name": "Diderot",
		"bot": map[string]any{"workspace_name": "Example Workspace"},
	}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fixtureCredential {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(fixtureBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/me":
			_ = json.NewEncoder(w).Encode(fake.self)
		case "/search":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]any{"property": "object", "value": "page"}, body["filter"])
			assert.EqualValues(t, testPageSample, body["page_size"])
			results := []map[string]any{}
			for i := range fake.pages {
				results = append(results, map[string]any{"object": "page", "id": fmt.Sprint(i), "title": fixturePageTitle})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "results": results, "has_more": fake.more})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeNotion) test(t *testing.T, credential string) settings.TestReport {
	t.Helper()
	secrets := map[string]string{}
	if credential != "" {
		secrets[FieldToken] = credential
	}
	report := Connector{BaseURL: f.server.URL}.Test(context.Background(), settings.NewResolved(ConnectorName, nil, secrets))
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixturePageTitle)
	assert.NotContains(t, string(encoded), fixtureBody)
	assert.NotContains(t, string(encoded), fixtureCredential)
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
	assert.Equal(t, "notion", def.Name())
	require.Len(t, def.Fields(), 1)
	assert.Equal(t, settings.KindSecret, def.Fields()[0].Kind)
	assert.Equal(t, "DIDEROT_NOTION_TOKEN", settings.EnvName(def.Name(), FieldToken))
	assert.NoError(t, def.Validate(settings.Values{}))
}

func TestConnectorTestPasses(t *testing.T) {
	fake := newFakeNotion(t)
	report := fake.test(t, fixtureCredential)
	require.True(t, report.Passed(), "%+v", report)
	assert.Contains(t, check(t, report, "Integration secret").Detail, "Diderot")
	assert.Contains(t, check(t, report, "Integration secret").Detail, "Example Workspace")
	assert.Equal(t, "The integration can see 3 pages.", check(t, report, "Pages").Detail)
	workspace, ok := report.Fact("workspace")
	assert.True(t, ok)
	assert.Equal(t, "Example Workspace", workspace)
	integration, _ := report.Fact("integration")
	assert.Equal(t, "Diderot", integration)

	fake.more = true
	report = fake.test(t, fixtureCredential)
	require.True(t, report.Passed())
	assert.Equal(t, "The integration can see more than 3 pages.", check(t, report, "Pages").Detail)
}

func TestConnectorTestFails(t *testing.T) {
	t.Run("rejected secret", func(t *testing.T) {
		fake := newFakeNotion(t)
		report := fake.test(t, "some-other-value")
		assert.False(t, report.Passed())
		secret := check(t, report, "Integration secret")
		assert.False(t, secret.Passed)
		assert.Contains(t, secret.Detail, "Notion rejected the secret")
		assert.NotEmpty(t, secret.Remedy)
		assert.False(t, check(t, report, "Pages").Passed)
	})
	t.Run("no secret", func(t *testing.T) {
		fake := newFakeNotion(t)
		report := fake.test(t, "")
		assert.False(t, report.Passed())
		assert.Equal(t, "No secret.", check(t, report, "Integration secret").Detail)
		assert.Zero(t, fake.calls.Load(), "Test called Notion without a secret")
	})
	t.Run("a person's token", func(t *testing.T) {
		fake := newFakeNotion(t)
		fake.self = map[string]any{"object": "user", "id": "u1", "type": "person", "name": "Someone"}
		report := fake.test(t, fixtureCredential)
		assert.False(t, check(t, report, "Integration secret").Passed)
		assert.False(t, check(t, report, "Pages").Passed)
	})
	t.Run("no pages shared", func(t *testing.T) {
		fake := newFakeNotion(t)
		fake.pages = 0
		report := fake.test(t, fixtureCredential)
		assert.True(t, check(t, report, "Integration secret").Passed)
		pages := check(t, report, "Pages")
		assert.False(t, pages.Passed)
		assert.True(t, strings.Contains(pages.Remedy, "Connections"), pages.Remedy)
	})
}
