package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the Notion connector's connector_settings row.
const ConnectorName = "notion"

// FieldToken is the integration secret, which DIDEROT_NOTION_TOKEN sets.
const FieldToken = "token"

// testPageSample is the page size of Test's one search call.
const testPageSample = 100

// Connector is the Notion source: its page's fields, the pre-flight Test,
// and, in source.go, its facts, its leader workers, and its expansion.
type Connector struct {
	// BaseURL is the Notion API root; empty is Notion's. Tests point it at a
	// fake.
	BaseURL string
	// HTTP makes the API requests; nil is the client's default.
	HTTP *http.Client
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldToken, Label: "Integration secret", Kind: settings.KindSecret, Required: true,
			Help: "Starts with ntn_. Create an internal integration with read access at notion.so/profile/integrations, then share the pages to index with it (a page's ••• menu, Connections)."},
	}
}

// Validate has nothing to check: the only field is a secret.
func (Connector) Validate(settings.Values) error { return nil }

// NewClient builds the API client the workers share for token.
func (c Connector) NewClient(token string) (*Client, error) {
	return NewClient(Config{Token: token, BaseURL: c.BaseURL, HTTPClient: c.HTTP})
}

// Test authenticates with the secret and counts the pages the integration
// can see with one search call. The report carries the integration's and
// workspace's names and a count, never a page title or a response body.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }
	secretRemedy := r.SupplySecret(FieldToken, "the integration's Internal Integration Secret from notion.so/profile/integrations") + "."

	client, err := NewClient(Config{Token: r.Secret(FieldToken), BaseURL: c.BaseURL, HTTPClient: c.HTTP, MaxRetries: 1})
	if err != nil {
		add(failed("Integration secret", "No secret.", secretRemedy))
		add(failed("Pages", "Not checked: the secret did not authenticate.", "Fix the secret, then test again."))
		return report
	}
	var self struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Bot  struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := client.Do(ctx, http.MethodGet, "users/me", nil, &self); err != nil {
		add(failed("Integration secret", "users/me: "+describeError(err), secretRemedy))
		add(failed("Pages", "Not checked: the secret did not authenticate.", "Fix the secret, then test again."))
		return report
	}
	if self.Type != "bot" {
		add(failed("Integration secret", "Notion says this secret belongs to a person, not an integration.", secretRemedy))
		add(failed("Pages", "Not checked: the secret is not an integration's.", "Fix the secret, then test again."))
		return report
	}
	add(settings.Check{Name: "Integration secret", Passed: true,
		Detail: fmt.Sprintf("Integration %s in workspace %s.", orUnnamed(self.Name), orUnnamed(self.Bot.WorkspaceName))})
	report.Facts = append(report.Facts,
		settings.Fact{Key: "workspace", Label: "Workspace", Value: self.Bot.WorkspaceName},
		settings.Fact{Key: "integration", Label: "Integration", Value: self.Name})

	pages, err := client.Search(ctx, SearchParams{FilterObject: "page", PageSize: testPageSample})
	switch {
	case err != nil:
		add(failed("Pages", "search: "+describeError(err), "Test again. If it keeps failing, check the integration's capabilities include reading content."))
	case len(pages.Results) == 0:
		add(failed("Pages", "The integration can see no pages.",
			"In Notion, share the pages to index with this integration: a page's ••• menu, Connections."))
	case pages.HasMore:
		add(settings.Check{Name: "Pages", Passed: true,
			Detail: fmt.Sprintf("The integration can see more than %d pages.", len(pages.Results))})
	default:
		add(settings.Check{Name: "Pages", Passed: true,
			Detail: fmt.Sprintf("The integration can see %d pages.", len(pages.Results))})
	}
	return report
}

func failed(name, detail, remedy string) settings.Check {
	return settings.Check{Name: name, Detail: detail, Remedy: remedy}
}

func orUnnamed(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	return name
}

// describeError is a short, body-free description of a client error.
func describeError(err error) string {
	var status *HTTPError
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	switch {
	case errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized:
		return "Notion rejected the secret"
	case errors.As(err, &status):
		return fmt.Sprintf("Notion answered HTTP %d", status.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.As(err, &syntax), errors.As(err, &shape):
		return "Notion's answer was not understood"
	}
	return "Notion is unreachable"
}
