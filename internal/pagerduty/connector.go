package pagerduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the PagerDuty connector's connector_settings row. While it
// is enabled the webhook accepts pages, and investigations run while Slack is
// enabled too and Ask is on.
const ConnectorName = "pagerduty"

// PagerDuty connector fields.
const (
	FieldAPIToken         = "api_token"
	FieldWebhookSecrets   = "webhook_secrets"
	FieldFromEmail        = "from_email"
	FieldSlackChannelID   = "slack_channel_id"
	FieldSlackChannelName = "slack_channel_name"
	FieldAllowedScopes    = "allowed_scopes"
)

var slackChannelID = regexp.MustCompile(`^[CG][A-Z0-9]{2,}$`)

// Settings is the PagerDuty connector's resolved settings.
type Settings struct {
	APIToken string
	// WebhookSecrets are the signing secrets a webhook may be signed with.
	WebhookSecrets   []string
	FromEmail        string
	SlackChannelID   string
	SlackChannelName string
	AllowedScopes    []string
}

// SettingsFrom reads the PagerDuty fields of resolved settings.
func SettingsFrom(r settings.Resolved) Settings {
	var secrets []string
	for _, secret := range strings.Split(r.Secret(FieldWebhookSecrets), ",") {
		if secret = strings.TrimSpace(secret); secret != "" {
			secrets = append(secrets, secret)
		}
	}
	return Settings{
		APIToken: r.Secret(FieldAPIToken), WebhookSecrets: secrets,
		FromEmail:      r.String(FieldFromEmail),
		SlackChannelID: r.String(FieldSlackChannelID), SlackChannelName: r.String(FieldSlackChannelName),
		AllowedScopes: r.List(FieldAllowedScopes),
	}
}

// Format keeps the token and the signing secrets out of every fmt verb.
func (s Settings) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "pagerduty.Settings{%s}", s.SlackChannelID)
}

// Connector is the PagerDuty automation: its page's fields, their
// validation, the pre-flight Test, and the investigation workers its Run
// holds. The webhook listener, which every replica serves, reads its
// settings as requests come.
type Connector struct {
	// APIURL is PagerDuty's REST API base; empty is the public API. Tests
	// point it at a fake.
	APIURL string
	// HTTP makes Test's requests; nil uses the client's default.
	HTTP *http.Client
	// Slack reports whether the Slack connector, which delivers
	// investigations, is enabled now.
	Slack func() bool
	// Ask says this deployment runs Ask, and Grafana reports whether a
	// Grafana connection for Ask's investigator is on. Investigations need
	// both.
	Ask     bool
	Grafana func() bool
	// PublicURL is Diderot's address, which links each investigation's
	// transcript; empty leaves the link out.
	PublicURL string
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldAPIToken, Label: "API token", Kind: settings.KindSecret, Required: true,
			Help: "A PagerDuty REST API key that can write (Integrations, API Access Keys). Diderot reads incidents with it and adds its findings as notes."},
		{Key: FieldWebhookSecrets, Label: "Webhook signing secrets", Kind: settings.KindSecret, Required: true,
			Help: "The signing secret PagerDuty shows when you add a webhook subscription for incident.triggered. " +
				"Point the subscription at this server's PagerDuty webhook port (9093 unless the deployment moves it), path /webhooks/pagerduty. " +
				"To rotate it, enter the new and the old secret separated by a comma."},
		{Key: FieldFromEmail, Label: "Notes posted as", Kind: settings.KindText, Required: true,
			Help: "The email of the PagerDuty user that Diderot's incident notes are posted as."},
		{Key: FieldSlackChannelID, Label: "Alerts channel", Kind: settings.KindText, Required: true, SlackChannel: true,
			Help: "The Slack channel where PagerDuty posts alerts: its name, its ID, or a link to it. " +
				"Diderot replies in each alert's thread there, so invite it to the channel."},
		{Key: FieldSlackChannelName, Label: "Alerts channel name", Kind: settings.KindText,
			Help: "When set, Diderot replies only while the alerts channel has this name, so investigations can't go to a channel that was mistaken for it."},
		{Key: FieldAllowedScopes, Label: "What investigations may read", Kind: settings.KindList, Required: true,
			Default: []string{"public", "internal"},
			Help: "What an investigation may search, one per line, such as public, internal, or slack-channel:C0123456789. " +
				"Everyone in the alerts channel sees its answer, so list only what all of them may read."},
	}
}

// Validate checks the email and the channel, and drops a # from the
// channel's name.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	email, _ := values[FieldFromEmail].(string)
	if address, err := mail.ParseAddress(email); err != nil || address.Address != email {
		errs[FieldFromEmail] = "must be an email address, such as oncall@example.com"
	}
	if id, _ := values[FieldSlackChannelID].(string); !slackChannelID.MatchString(id) {
		errs[FieldSlackChannelID] = "must be a Slack channel, such as #alerts or C0123456789"
	}
	name, _ := values[FieldSlackChannelName].(string)
	name = strings.TrimPrefix(name, "#")
	if strings.ContainsAny(name, " #") {
		errs[FieldSlackChannelName] = "must be a channel name, such as alerts"
	}
	if len(errs) > 0 {
		return errs
	}
	if _, ok := values[FieldSlackChannelName]; ok {
		values[FieldSlackChannelName] = name
	}
	return nil
}

func (c Connector) client(s Settings) *Client {
	client := NewClient(s.APIToken, s.FromEmail)
	if c.APIURL != "" {
		client.baseURL = strings.TrimRight(c.APIURL, "/")
	}
	if c.HTTP != nil {
		client.http = c.HTTP
	}
	return client
}

// Test checks the token and the author of notes with read-only calls, counts
// the signing secrets, and says whether Slack and Ask, which investigations
// need, are on. The report never carries PagerDuty's response.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	s := SettingsFrom(r)
	client := c.client(s)
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }

	newToken := r.SupplySecret(FieldAPIToken, "a REST API key from PagerDuty (Integrations, API Access Keys)") + "."
	_, err := client.request(ctx, http.MethodGet, "/abilities", nil)
	if err != nil {
		add(describeError("API token", err, newToken, newToken))
		add(failed("Notes posted as", "Not checked: the API token didn't work.", "Fix the API token, then test again."))
	} else {
		add(settings.Check{Name: "API token", Passed: true, Detail: "PagerDuty accepted the token."})
		add(c.authorCheck(ctx, client, s.FromEmail, newToken))
	}

	switch n := len(s.WebhookSecrets); n {
	case 0:
		add(failed("Webhook signing secrets", "No signing secret.", "Paste the signing secret PagerDuty shows for the webhook subscription."))
	case 1:
		add(settings.Check{Name: "Webhook signing secrets", Passed: true, Detail: "Webhooks signed with the secret are accepted."})
	default:
		add(settings.Check{Name: "Webhook signing secrets", Passed: true, Detail: fmt.Sprintf("Webhooks signed with any of the %d secrets are accepted.", n)})
	}

	// Investigations wait in the queue until both are on; neither stops
	// the connector from accepting pages.
	slack := settings.Check{Name: "Slack", Passed: true,
		Detail: "The Slack connector is enabled. Investigations reply in the alerts channel."}
	if c.Slack == nil || !c.Slack() {
		slack.Detail = "The Slack connector is off. Investigations need it: until it is enabled, alerts wait in the queue."
	}
	add(slack)
	ask := settings.Check{Name: "Ask", Passed: true, Detail: "Ask is on, with a Grafana connection to investigate with."}
	switch {
	case !c.Ask:
		ask.Detail = "Ask is off in this deployment. Investigations need it: until DIDEROT_ASK_ENABLED is on, alerts wait in the queue."
	case c.Grafana == nil || !c.Grafana():
		ask.Detail = "Ask has no Grafana connection to investigate with. Add one under Tool connections on the Connectors page and turn it on; until then, alerts wait in the queue."
	}
	add(ask)
	return report
}

// authorCheck looks for the PagerDuty user whose email notes are posted as.
// The report names only the email the admin entered. newToken is the remedy
// for a token PagerDuty rejects.
func (c Connector) authorCheck(ctx context.Context, client *Client, email, newToken string) settings.Check {
	const name = "Notes posted as"
	raw, err := client.request(ctx, http.MethodGet, "/users?limit=100&query="+url.QueryEscape(email), nil)
	if err != nil {
		return describeError(name, err, "Test again. If it keeps failing, check the API token.", newToken)
	}
	var page struct {
		Users []struct {
			Email string `json:"email"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return failed(name, "PagerDuty's user list wasn't readable.", "Test again.")
	}
	for _, user := range page.Users {
		if strings.EqualFold(user.Email, email) {
			return settings.Check{Name: name, Passed: true, Detail: email + " is a PagerDuty user."}
		}
	}
	return failed(name, "No PagerDuty user has the email "+email+".",
		"Enter the email of a PagerDuty user: PagerDuty accepts notes only from one.")
}

func failed(name, detail, remedy string) settings.Check {
	return settings.Check{Name: name, Detail: detail, Remedy: remedy}
}

// describeError is a failed call, by status code and without PagerDuty's
// answer: remedy for an unexpected status, newToken for a rejected token.
func describeError(name string, err error, remedy, newToken string) settings.Check {
	var status *StatusError
	switch {
	case errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized:
		return failed(name, "PagerDuty rejected the API token.", newToken)
	case errors.As(err, &status) && status.StatusCode == http.StatusForbidden:
		return failed(name, "The API token may not read this.", "Use a REST API key with full access, not a read-only or scoped one.")
	case errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests:
		return failed(name, "PagerDuty is limiting how fast this token may call it.", "Wait a minute, then test again.")
	case errors.As(err, &status):
		return failed(name, fmt.Sprintf("PagerDuty answered HTTP %d.", status.StatusCode), remedy)
	case errors.Is(err, context.DeadlineExceeded):
		return failed(name, "PagerDuty didn't answer in time.", "Check that this server can reach api.pagerduty.com, then test again.")
	}
	return failed(name, "Couldn't reach PagerDuty.", "Check that this server can reach api.pagerduty.com, then test again.")
}
