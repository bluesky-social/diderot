package sessionfeed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the session feed connector's connector_settings row.
const ConnectorName = "session_feed"

// Session feed connector fields. The token's variable is
// DIDEROT_SESSION_FEED_TOKEN.
const (
	FieldURL           = "url"
	FieldToken         = "token"
	FieldViewerBaseURL = "viewer_base_url"
)

// Settings is the session feed connector's resolved settings.
type Settings struct {
	URL, Token string
	// ViewerBaseURL is the public origin of the session viewer that
	// citations link to; empty leaves them unlinked.
	ViewerBaseURL string
}

// SettingsFrom reads the session feed fields of resolved settings.
func SettingsFrom(r settings.Resolved) Settings {
	return Settings{URL: r.String(FieldURL), Token: r.Secret(FieldToken), ViewerBaseURL: r.String(FieldViewerBaseURL)}
}

// Format keeps the token out of every fmt verb.
func (s Settings) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sessionfeed.Settings{%s}", s.URL)
}

// Connector is the session feed connector's definition: its page's fields,
// their validation, the pre-flight Test, and, in source.go, its facts. The
// leader's workers and agent session expansion are wired in cmd/diderot.
type Connector struct {
	// HTTP makes the feed's requests; nil is NewClient's default.
	HTTP *http.Client
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldURL, Label: "Feed URL", Kind: settings.KindText, Required: true,
			Help: "The base URL of the session feed, such as https://sessions.example.com."},
		{Key: FieldToken, Label: "Token", Kind: settings.KindSecret, Required: true,
			Help: "The bearer token the feed accepts from Diderot."},
		{Key: FieldViewerBaseURL, Label: "Session viewer URL", Kind: settings.KindText,
			Help: "The public address of the session viewer, such as https://sessions.example.com. Citations of agent sessions link there."},
	}
}

// Validate checks that the feed URL is an http(s) URL and the viewer an
// http(s) origin, and drops their trailing slashes.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	if raw, _ := values[FieldURL].(string); raw != "" {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			errs[FieldURL] = "enter an http or https URL, without a query"
		} else {
			values[FieldURL] = strings.TrimRight(raw, "/")
		}
	}
	if raw, _ := values[FieldViewerBaseURL].(string); raw != "" {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			errs[FieldViewerBaseURL] = "enter an http or https address without a path, such as https://sessions.example.com"
		} else {
			values[FieldViewerBaseURL] = strings.TrimRight(raw, "/")
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// NewClient builds the feed client for r's settings.
func (c Connector) NewClient(r settings.Resolved) *Client {
	s := SettingsFrom(r)
	return NewClient(s.URL, s.Token, c.HTTP)
}

// testCursor is past the end of any feed. The feed answers a read after it
// with an empty page that echoes the cursor, so the Test proves the URL, the
// token, and the feed's protocol without the feed reading a change. From
// cursor 0, a feed may build every session's head before returning the
// first, which can take many seconds.
const testCursor = math.MaxInt64

// Test reads the changes after testCursor with the token. The report says
// only whether the feed answered.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	const name = "Feed"
	remedy := "Check that Diderot can reach the feed URL. " + r.SupplySecret(FieldToken, "the token the feed issued for Diderot") + "."
	var report settings.TestReport
	s := SettingsFrom(r)
	if s.URL == "" || s.Token == "" {
		report.Checks = append(report.Checks, settings.Check{Name: name, Detail: "The feed URL or token is missing.", Remedy: remedy})
		return report
	}
	if _, err := c.NewClient(r).ListSessionChanges(ctx, testCursor, 1); err != nil {
		report.Checks = append(report.Checks, settings.Check{Name: name, Detail: describeTestError(err), Remedy: remedy})
		return report
	}
	report.Checks = append(report.Checks, settings.Check{Name: name, Passed: true, Detail: "The feed answered with the token."})
	return report
}

// describeTestError is a short, body-free description of a failed read.
func describeTestError(err error) string {
	var status *HTTPError
	var transport *url.Error
	switch {
	case IsAuth(err) && errors.As(err, &status):
		return fmt.Sprintf("The feed refused the token (HTTP %d).", status.StatusCode)
	case errors.As(err, &status):
		return fmt.Sprintf("The feed answered HTTP %d.", status.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return "The feed did not answer in time."
	case errors.As(err, &transport):
		return "The feed is unreachable."
	}
	return "The feed's answer was not understood."
}
