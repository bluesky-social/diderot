package sourcebot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the code search connector's connector_settings row. Ask
// offers code_search and code_read while it is enabled.
const ConnectorName = "sourcebot"

// Code search connector fields.
const (
	FieldURL         = "url"
	FieldAPIKey      = "api_key"
	FieldRepoPrefix  = "repo_prefix"
	FieldSearchScope = "search_scope"
	FieldReadScope   = "read_scope"
)

// testQuery is the search Test runs, for one match of anything.
const testQuery = "main"

// Settings is the code search connector's resolved settings.
type Settings struct {
	URL, APIKey            string
	RepoPrefix             string
	SearchScope, ReadScope string
}

// SettingsFrom reads the code search fields of resolved settings.
func SettingsFrom(r settings.Resolved) Settings {
	return Settings{
		URL: r.String(FieldURL), APIKey: r.Secret(FieldAPIKey), RepoPrefix: r.String(FieldRepoPrefix),
		SearchScope: r.String(FieldSearchScope), ReadScope: r.String(FieldReadScope),
	}
}

// Format keeps the API key out of every fmt verb.
func (s Settings) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sourcebot.Settings{%s}", s.URL)
}

// Connector is the code search connector's definition: its page's fields,
// their validation, and the pre-flight Test. It has no workers: Ask reads
// its settings when a run starts and on each call.
type Connector struct {
	// HTTP makes Test's requests; nil uses the client's default.
	HTTP *http.Client
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldURL, Label: "Sourcebot URL", Kind: settings.KindText, Required: true,
			Help: "The address of your Sourcebot, such as https://sourcebot.example.com."},
		{Key: FieldAPIKey, Label: "API key", Kind: settings.KindSecret, Required: true,
			Help: "A Sourcebot API key, from Settings, API keys in Sourcebot."},
		{Key: FieldRepoPrefix, Label: "Repository prefix", Kind: settings.KindText,
			Help: "The start that most repository names share, ending in /, such as github.com/example/. " +
				"Ask shows names without it, which saves space in its answers. Leave empty to show full names."},
		{Key: FieldSearchScope, Label: "What the code index covers", Kind: settings.KindText,
			Help: "A phrase that tells Ask which code it can search. Ask reads it as \"Search the code of …\", " +
				"so write, for example: every Example repository (public and private, indexed hourly). " +
				"Leave empty for: every repository in the code index."},
		{Key: FieldReadScope, Label: "Where a file is read from", Kind: settings.KindText,
			Help: "A phrase that tells Ask which version of a file it reads. Ask reads it as \"Read the full content of one file from …\", " +
				"so write, for example: an Example repository (default branch, as of the last hourly index). " +
				"Leave empty for: a repository in the code index (default branch, as of the last index)."},
	}
}

// Validate checks the Sourcebot URL and the repository prefix, and drops the
// URL's trailing slash.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	raw, _ := values[FieldURL].(string)
	if normalized, ok := httpURL(raw); ok {
		values[FieldURL] = normalized
	} else {
		errs[FieldURL] = "must be an http or https URL, such as https://sourcebot.example.com"
	}
	if prefix, _ := values[FieldRepoPrefix].(string); prefix != "" && !strings.HasSuffix(prefix, "/") {
		errs[FieldRepoPrefix] = "must end in /, such as github.com/example/"
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// Test runs one one-match search with the key, then asks how many
// repositories are indexed, which older Sourcebots can't say. The report
// never carries Sourcebot's response.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	s := SettingsFrom(r)
	client := NewClient(s.URL, s.APIKey, c.HTTP)
	check := settings.Check{Name: "Search", Passed: true, Detail: "Sourcebot answered a search with this key."}
	if _, err := client.Search(ctx, SearchParams{Query: testQuery, Matches: 1}); err != nil {
		check = describeError(err, r)
	} else if count, err := client.RepoCount(ctx); err == nil {
		check.Detail = fmt.Sprintf("Sourcebot answered a search with this key. It indexes %d repositories.", count)
	}
	return settings.TestReport{Checks: []settings.Check{check}}
}

// describeError is a failed search with r's settings, by status code and
// without Sourcebot's answer.
func describeError(err error, r settings.Resolved) settings.Check {
	failed := func(detail, remedy string) settings.Check {
		return settings.Check{Name: "Search", Detail: detail, Remedy: remedy}
	}
	var status *HTTPError
	var unreachable *url.Error
	switch {
	case errors.As(err, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden):
		return failed("Sourcebot rejected the API key.", r.SupplySecret(FieldAPIKey, "an API key from Sourcebot (Settings, API keys)")+".")
	case errors.As(err, &status) && status.StatusCode == http.StatusNotFound:
		return failed("There's no Sourcebot search at this URL.", "Use the address you open Sourcebot at.")
	case errors.As(err, &status):
		return failed(fmt.Sprintf("Sourcebot answered HTTP %d.", status.StatusCode), "Test again. If it keeps failing, check that Sourcebot is up.")
	case errors.Is(err, context.DeadlineExceeded):
		return failed("Sourcebot didn't answer in time.", "Check that this server can reach the Sourcebot URL, then test again.")
	case errors.As(err, &unreachable):
		return failed("Couldn't reach Sourcebot.", "Check the Sourcebot URL and that this server can reach it.")
	}
	return failed("The URL didn't answer like Sourcebot.", "Use the address you open Sourcebot at.")
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
