package zendesk

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// ConnectorName is the Zendesk connector's connector_settings row.
const ConnectorName = "zendesk"

// Zendesk connector fields. The secret's variable is
// DIDEROT_ZENDESK_OAUTH_SECRET.
const (
	FieldSubdomain       = "subdomain"
	FieldOAuthClientID   = "oauth_client_id"
	FieldOAuthSecret     = "oauth_secret"
	FieldIndexedGroupIDs = "indexed_group_ids"
	FieldDocIngest       = "doc_ingest"
)

const (
	// requestsPerMinute is the API budget of every client.
	requestsPerMinute = 300
	// RecentLookback starts the recent-tickets stream this far back, so
	// recently updated tickets are imported before the full-history backfill
	// reaches them. The start is fixed on the first sync.
	RecentLookback = 30 * 24 * time.Hour
)

// Settings is the Zendesk connector's resolved settings.
type Settings struct {
	Subdomain, OAuthClientID, OAuthSecret string
	// IndexedGroups are the only groups whose tickets are indexed.
	IndexedGroups []int64
	DocIngest     bool
}

// SettingsFrom reads the Zendesk fields of resolved settings. Validate has
// checked every group ID.
func SettingsFrom(r settings.Resolved) Settings {
	s := Settings{
		Subdomain: r.String(FieldSubdomain), OAuthClientID: r.String(FieldOAuthClientID),
		OAuthSecret: r.Secret(FieldOAuthSecret), DocIngest: r.Bool(FieldDocIngest),
	}
	for _, raw := range r.List(FieldIndexedGroupIDs) {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			s.IndexedGroups = append(s.IndexedGroups, id)
		}
	}
	return s
}

// Format keeps the secret out of every fmt verb.
func (s Settings) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "zendesk.Settings{%s}", s.Subdomain)
}

// Connector is the Zendesk source: its page's fields, their validation, the
// pre-flight Test, and, in source.go, its facts, its leader workers, and its
// expansion.
type Connector struct {
	// Origin replaces https://<subdomain>.zendesk.com. Tests point it at a
	// fake.
	Origin string
	// FileStorage says the deployment has the file storage Index documents
	// needs (DIDEROT_IMAGES_BUCKET).
	FileStorage bool
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldSubdomain, Label: "Subdomain", Kind: settings.KindText, Required: true,
			Help: "The name before .zendesk.com, such as example for example.zendesk.com."},
		{Key: FieldOAuthClientID, Label: "OAuth client ID", Kind: settings.KindText, Required: true,
			Help: "The identifier of an OAuth client in Admin Center (Apps and integrations, APIs, OAuth clients), created with the Confidential client kind. Diderot signs in with the client credentials grant and read scope, as the client's owner, who must be able to read every group."},
		{Key: FieldOAuthSecret, Label: "OAuth client secret", Kind: settings.KindSecret, Required: true,
			Help: "That client's secret."},
		// Required: a ticket outside these groups is removed from the index,
		// so an empty list would remove every ticket.
		{Key: FieldIndexedGroupIDs, Label: "Groups to index", Kind: settings.KindList, Required: true,
			Help: "Zendesk group IDs, the number in a group's URL. Only tickets in these groups are indexed, and everyone who can search Zendesk in Diderot sees them, so list only groups they may all read. A ticket that leaves them is removed."},
		{Key: FieldDocIngest, Label: "Index documents", Kind: settings.KindBool,
			Help: "Index PDF, Word, and Excel attachments."},
	}
}

// Validate checks the subdomain and group IDs, and normalizes them in
// values.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	if subdomain, _ := values[FieldSubdomain].(string); subdomain != "" {
		subdomain = strings.ToLower(subdomain)
		if !subdomainPattern.MatchString(subdomain) {
			errs[FieldSubdomain] = "enter only the subdomain, such as example for example.zendesk.com"
		} else {
			values[FieldSubdomain] = subdomain
		}
	}
	if ids, _ := values[FieldIndexedGroupIDs].([]string); ids != nil {
		normalized := make([]string, 0, len(ids))
		for _, raw := range ids {
			id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil || id <= 0 {
				errs[FieldIndexedGroupIDs] = fmt.Sprintf("%q is not a Zendesk group ID", strings.TrimSpace(raw))
				break
			}
			if formatted := strconv.FormatInt(id, 10); !slices.Contains(normalized, formatted) {
				normalized = append(normalized, formatted)
			}
		}
		values[FieldIndexedGroupIDs] = normalized
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// NewClient builds the API client the workers share for r's credentials.
func (c Connector) NewClient(r settings.Resolved) (*Client, error) {
	s := SettingsFrom(r)
	client, err := NewClient(s.Subdomain, s.OAuthClientID, s.OAuthSecret, requestsPerMinute)
	if err != nil {
		return nil, err
	}
	if c.Origin != "" {
		client.origin = c.Origin
	}
	return client, nil
}

// Test exchanges the OAuth client's credentials for a token, reads the
// account's name, and checks that each group to index exists. Every call is
// read-only; the report carries the account's name, counts, and the group
// IDs the admin entered, never an API response.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }
	remedy := "Check the subdomain and the OAuth client ID. " +
		r.SupplySecret(FieldOAuthSecret, "the client's secret from Admin Center (Apps and integrations, APIs, OAuth clients)") + "."
	notChecked := failed("Groups", "Not checked: the OAuth client did not sign in.", "Fix the OAuth client, then test again.")

	s := SettingsFrom(r)
	if s.DocIngest {
		add(source.DocumentIngestCheck(c.FileStorage))
	}
	client, err := c.NewClient(r)
	if err != nil {
		add(failed("OAuth client", "The subdomain, client ID, or secret is missing.", remedy))
		add(notChecked)
		return report
	}
	if _, err := client.accessToken(ctx); err != nil {
		add(failed("OAuth client", describeOAuthError(err), remedy))
		add(notChecked)
		return report
	}
	var brands struct {
		Brands []struct {
			Name    string `json:"name"`
			Default bool   `json:"default"`
		} `json:"brands"`
	}
	if err := client.get(ctx, "/api/v2/brands.json", url.Values{"page[size]": {"100"}}, true, &brands); err != nil {
		add(failed("OAuth client", "Signed in, but couldn't read the account: "+describeError(err)+".",
			"Use an OAuth client whose owner is a Zendesk admin."))
		add(notChecked)
		return report
	}
	account := ""
	for _, brand := range brands.Brands {
		if brand.Default || account == "" {
			account = brand.Name
		}
	}
	add(settings.Check{Name: "OAuth client", Passed: true,
		Detail: fmt.Sprintf("Signed in to %s.zendesk.com, account %s.", s.Subdomain, orUnnamed(account))})
	report.Facts = append(report.Facts, settings.Fact{Key: "account", Label: "Account", Value: account})

	groups, err := client.groupIDs(ctx)
	if err != nil {
		add(failed("Groups", "Couldn't list the groups: "+describeError(err)+".",
			"Use an OAuth client whose owner can read every group, such as an admin's."))
		return report
	}
	var missing []string
	for _, id := range s.IndexedGroups {
		if !slices.Contains(groups, id) {
			missing = append(missing, strconv.FormatInt(id, 10))
		}
	}
	switch {
	case len(missing) > 0:
		add(failed("Groups", "No group has the ID "+strings.Join(missing, ", ")+".",
			"Fix Groups to index: use the number in a group's URL in Admin Center (People, Groups)."))
	case len(s.IndexedGroups) == 0:
		add(failed("Groups", fmt.Sprintf("The account has %d groups; none is listed to index.", len(groups)),
			"List the groups whose tickets to index under Groups to index."))
	default:
		add(settings.Check{Name: "Groups", Passed: true,
			Detail: fmt.Sprintf("All %d groups to index exist, of %d in the account.", len(s.IndexedGroups), len(groups))})
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

// describeOAuthError is a short, body-free description of a failed token
// exchange.
func describeOAuthError(err error) string {
	var status *oauthStatusError
	switch {
	case errors.As(err, &status) && (status.status == 400 || status.status == 401 || status.status == 403):
		return fmt.Sprintf("Zendesk refused the OAuth client (HTTP %d).", status.status)
	case errors.As(err, &status):
		return fmt.Sprintf("Zendesk answered the sign-in with HTTP %d.", status.status)
	case errors.Is(err, context.DeadlineExceeded):
		return "Zendesk did not answer in time."
	case errors.Is(err, errInvalidGrant):
		return "Zendesk's token is not read-only; Diderot asks for read access only."
	}
	return "Zendesk is unreachable."
}

// describeError is a short, body-free description of a failed API read.
func describeError(err error) string {
	var failure *APIError
	switch {
	case errors.As(err, &failure):
		return fmt.Sprintf("Zendesk answered HTTP %d", failure.Status)
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, errProtocol):
		return "Zendesk's answer was not understood"
	}
	return "Zendesk is unreachable"
}
