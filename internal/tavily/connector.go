package tavily

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the web search connector's connector_settings row. Ask
// offers web_search and web_fetch while it is enabled.
const ConnectorName = "tavily"

// Web search connector fields.
const (
	FieldAPIKey  = "api_key"
	FieldBaseURL = "base_url"
)

// testQuery is the search Test runs: one result, about anything.
const testQuery = "Tavily"

// Connector is the web search connector's definition: its page's fields,
// their validation, and the pre-flight Test. It has no workers: Ask reads
// its settings when a run starts and on each call.
type Connector struct {
	// HTTP makes Test's request; nil uses the client's default.
	HTTP *http.Client
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldAPIKey, Label: "API key", Kind: settings.KindSecret, Required: true,
			Help: "Starts with tvly-. From your account at app.tavily.com. Ask's web searches are sent to Tavily with it."},
		{Key: FieldBaseURL, Label: "API URL", Kind: settings.KindText,
			Help: "Leave empty for Tavily's API. Set it only to send requests through a proxy."},
	}
}

// Validate checks that the API URL, when set, is an http(s) URL, and drops
// a trailing slash.
func (Connector) Validate(values settings.Values) error {
	raw, _ := values[FieldBaseURL].(string)
	if raw == "" {
		return nil
	}
	normalized, ok := httpURL(raw)
	if !ok {
		return settings.FieldErrors{FieldBaseURL: "must be an http or https URL, such as " + DefaultBaseURL}
	}
	values[FieldBaseURL] = normalized
	return nil
}

// ClientFrom builds a client with resolved settings.
func ClientFrom(r settings.Resolved, httpClient *http.Client) *Client {
	return NewClient(r.String(FieldBaseURL), r.Secret(FieldAPIKey), httpClient)
}

// Test runs one one-result search. The report never carries Tavily's
// response.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	check := settings.Check{Name: "Search", Passed: true, Detail: "Tavily answered a one-result search."}
	if _, err := ClientFrom(r, c.HTTP).Search(ctx, SearchParams{Query: testQuery, MaxResults: 1}); err != nil {
		check = describeError(err, r)
	}
	return settings.TestReport{Checks: []settings.Check{check}}
}

// describeError is a failed search with r's settings, by status code and
// without Tavily's answer.
func describeError(err error, r settings.Resolved) settings.Check {
	baseURL := r.String(FieldBaseURL)
	failed := func(detail, remedy string) settings.Check {
		return settings.Check{Name: "Search", Detail: detail, Remedy: remedy}
	}
	var status *HTTPError
	var unreachable *url.Error
	switch {
	case errors.As(err, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden):
		return failed("Tavily rejected the API key.", r.SupplySecret(FieldAPIKey, "an API key from app.tavily.com")+"; it starts with tvly-.")
	case errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests:
		return failed("Tavily is limiting how fast this key may search.", "Wait a minute, then test again.")
	case errors.As(err, &status) && (status.StatusCode == 432 || status.StatusCode == 433):
		return failed("This key has used its Tavily plan's credits.", "Raise the plan or its limit at app.tavily.com, or use another key.")
	case errors.As(err, &status):
		return failed(fmt.Sprintf("Tavily answered HTTP %d.", status.StatusCode), "Test again. If it keeps failing, check the API URL.")
	case errors.Is(err, context.DeadlineExceeded):
		return failed("Tavily didn't answer in time.", "Check that this server can reach the API URL, then test again.")
	case errors.As(err, &unreachable):
		host := "Tavily's API"
		if baseURL != "" {
			host = baseURL
		}
		return failed("Couldn't reach "+host+".", "Check the API URL, or leave it empty for Tavily's API, and that this server can reach it.")
	}
	return failed("The API URL didn't answer like Tavily's API.", "Check the API URL, or leave it empty for Tavily's API.")
}

// httpURL reports whether raw is an absolute http(s) URL with a host and
// nothing after its path, and returns it without a trailing slash.
func httpURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(raw, "/"), true
}
