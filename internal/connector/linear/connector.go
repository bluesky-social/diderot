package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the Linear connector's connector_settings row.
const ConnectorName = "linear"

// Linear connector fields. The secrets' variables are
// DIDEROT_LINEAR_OAUTH_SECRET and DIDEROT_LINEAR_TOKEN.
const (
	FieldOAuthClientID = "oauth_client_id"
	FieldOAuthSecret   = "oauth_secret"
	FieldToken         = "token"
	FieldExcludeTeams  = "exclude_teams"
)

// testTeamLimit is how many teams Test's one query lists.
const testTeamLimit = 250

// Connector is the Linear source: its page's fields, their validation, the
// pre-flight Test, and, in source.go, its facts, its leader workers, and its
// expansion.
type Connector struct {
	// HTTP makes the API requests; nil is the client's own, which refuses
	// redirects. Tests point it at a fake.
	HTTP *http.Client
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		// No credential field is Required: either credential signs in, and
		// whether one is complete depends on secrets, which the form's
		// validation doesn't see. Test and Run refuse settings with neither.
		{Key: FieldOAuthClientID, Label: "OAuth application client ID", Kind: settings.KindText,
			Help: "The client ID of an OAuth application in Linear (Settings, API) that has client credentials tokens turned on. Diderot signs in as the application with the read scope and reads every public team. The API key is not used while this is set."},
		{Key: FieldOAuthSecret, Label: "OAuth application client secret", Kind: settings.KindSecret,
			Help: "That application's client secret."},
		{Key: FieldToken, Label: "API key", Kind: settings.KindSecret,
			Help: "A personal API key (lin_api_...) from Linear's Settings, Security & access. It acts as the member who made it and stops working when that member's account is disabled, so prefer an OAuth application."},
		{Key: FieldExcludeTeams, Label: "Teams to leave out", Kind: settings.KindList,
			Help: "Team keys, such as SUP. Their issues are not indexed. Private teams are never indexed."},
	}
}

// Validate upper-cases the excluded team keys, which match
// case-insensitively. Whether the API key works is Test's check: Validate
// doesn't see secrets.
func (Connector) Validate(values settings.Values) error {
	keys, _ := values[FieldExcludeTeams].([]string)
	if keys == nil {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	for _, entered := range keys {
		key := strings.ToUpper(strings.TrimSpace(entered))
		if strings.ContainsFunc(key, func(r rune) bool { return r == ' ' || r == '\t' }) {
			return settings.FieldErrors{FieldExcludeTeams: fmt.Sprintf("%q is not a team key", strings.TrimSpace(entered))}
		}
		if !slices.Contains(normalized, key) {
			normalized = append(normalized, key)
		}
	}
	values[FieldExcludeTeams] = normalized
	return nil
}

// Test or Run on settings whose credentials can't sign in.
var (
	errNoCredentials = errors.New("linear: no OAuth application or API key")
	errNoOAuthSecret = errors.New("linear: no client secret for the OAuth application")
)

// NewClient builds the API client for r's credentials: the OAuth
// application when its client ID is set, so a deployment can add the
// application before it removes the API key, and the API key otherwise.
func (c Connector) NewClient(r settings.Resolved) (*Client, error) {
	// One HTTP client for both: the application's token requests need the
	// same refusal of redirects as the API requests.
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = newHTTPClient()
	}
	var provider TokenProvider
	clientID, token := r.String(FieldOAuthClientID), r.Secret(FieldToken)
	switch {
	case clientID != "":
		// Not the API key instead: it would hide the missing secret until
		// the key itself stopped working.
		clientSecret := r.Secret(FieldOAuthSecret)
		if clientSecret == "" {
			return nil, errNoOAuthSecret
		}
		provider = newOAuthTokenProvider(clientID, clientSecret, httpClient)
	case token != "":
		provider = NewStaticTokenProvider(token)
	default:
		return nil, errNoCredentials
	}
	client := NewClient(provider)
	client.httpClient = httpClient
	return client, nil
}

// testQuery reads who the credentials act as and the teams they see, in one
// request.
const testQuery = `
query DiderotConnectorTest($first: Int!) {
  viewer { name }
  organization { name }
  teams(first: $first) {
    nodes { key private }
    pageInfo { hasNextPage }
  }
}`

// Test makes one request with the credentials: who they act as, and the
// teams they see. The report carries the organization's name, counts, and
// the team keys the admin entered, never an API response.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }
	remedy := r.SupplySecret(FieldToken, "a personal API key from Linear's Settings, Security & access") + "."
	secretRemedy := r.SupplySecret(FieldOAuthSecret, "the application's client secret from Linear's Settings, API") + "."
	noTeamsRemedy := "Use the key of a member of the teams to index."
	if r.String(FieldOAuthClientID) != "" {
		remedy = "Check the client ID and that the application has client credentials tokens turned on. " + secretRemedy
		noTeamsRemedy = "Give the OAuth application access to the teams to index."
	}
	notChecked := failed("Teams", "Not checked: the credentials did not authenticate.", "Fix the credentials, then test again.")
	// An answer Diderot can't read is no fault of the credentials entered.
	notUnderstood := failed("Credentials", "Linear's answer was not understood.", "Test again. If it keeps failing, report it as a Diderot bug.")

	client, err := c.NewClient(r)
	switch {
	case errors.Is(err, errNoOAuthSecret):
		add(failed("Credentials", "No client secret for the OAuth application.", secretRemedy))
		add(notChecked)
		return report
	case err != nil:
		add(failed("Credentials", "No OAuth application or API key.",
			"Enter the client ID and secret of an OAuth application from Linear's Settings, API, or a personal API key from Settings, Security & access."))
		add(notChecked)
		return report
	}
	// The provider signs in: for an OAuth application that is a request of
	// its own, for an API key it is the key.
	auth, err := client.provider.Authorization(ctx)
	switch {
	case errors.Is(err, errTokenAnswer):
		add(notUnderstood)
		add(notChecked)
		return report
	case err != nil:
		add(failed("Credentials", describeSignInFailure(err), remedy))
		add(notChecked)
		return report
	}
	data, _, status, err := client.doOnce(ctx, auth, testQuery, map[string]any{"first": testTeamLimit})
	if detail := describeTestFailure(status, err); detail != "" {
		add(failed("Credentials", detail, remedy))
		add(notChecked)
		return report
	}
	var answer struct {
		Viewer struct {
			Name string `json:"name"`
		} `json:"viewer"`
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Teams struct {
			Nodes []struct {
				Key     string `json:"key"`
				Private bool   `json:"private"`
			} `json:"nodes"`
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		add(notUnderstood)
		add(notChecked)
		return report
	}
	add(settings.Check{Name: "Credentials", Passed: true,
		Detail: fmt.Sprintf("Signed in to %s as %s.", orUnnamed(answer.Organization.Name), orUnnamed(answer.Viewer.Name))})
	report.Facts = append(report.Facts, settings.Fact{Key: "organization", Label: "Organization", Value: answer.Organization.Name})

	teams := answer.Teams.Nodes
	private := 0
	keys := map[string]bool{}
	for _, team := range teams {
		keys[strings.ToUpper(team.Key)] = true
		if team.Private {
			private++
		}
	}
	count := fmt.Sprintf("%d", len(teams))
	if answer.Teams.PageInfo.HasNextPage {
		count = fmt.Sprintf("more than %d", len(teams))
	}
	var missing []string
	for _, key := range r.List(FieldExcludeTeams) {
		if !keys[strings.ToUpper(key)] {
			missing = append(missing, key)
		}
	}
	switch {
	case len(teams) == 0:
		add(failed("Teams", "The credentials see no teams.", noTeamsRemedy))
	case len(missing) > 0 && !answer.Teams.PageInfo.HasNextPage:
		add(failed("Teams", "No team has the key "+strings.Join(missing, ", ")+".",
			"Fix Teams to leave out: use team keys as they appear in issue IDs, such as SUP in SUP-123."))
	default:
		add(settings.Check{Name: "Teams", Passed: true,
			Detail: fmt.Sprintf("The credentials see %s teams, %d of them private and never indexed.", count, private)})
	}
	return report
}

// describeSignInFailure is a short, body-free description of a provider
// that returned err instead of an Authorization value.
func describeSignInFailure(err error) string {
	refused, ok := errors.AsType[*tokenStatusError](err)
	if !ok {
		return describeTestFailure(0, err)
	}
	switch refused.status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("Linear refused the OAuth application (HTTP %d).", refused.status)
	}
	return describeTestFailure(refused.status, nil)
}

// describeTestFailure is a short, body-free description of a failed
// request, or "" when it succeeded.
func describeTestFailure(status int, err error) string {
	switch {
	case err != nil && status == 0 && errors.Is(err, context.DeadlineExceeded):
		return "Linear did not answer in time."
	case err != nil && status == 0:
		return "Linear is unreachable."
	case err != nil:
		return "Linear refused the query."
	case status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("Linear refused the credentials (HTTP %d).", status)
	case status != http.StatusOK:
		return fmt.Sprintf("Linear answered HTTP %d.", status)
	}
	return ""
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
