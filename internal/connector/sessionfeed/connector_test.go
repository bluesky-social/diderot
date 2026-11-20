package sessionfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fixtureFeedValue = "feed-fixture-value"
	fixtureOwner     = "owner-nobody-should-see@example.com"
)

func feedTest(t *testing.T, handler http.HandlerFunc, values settings.Values, secret string) settings.TestReport {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := settings.Values{FieldURL: server.URL}
	for key, value := range values {
		base[key] = value
	}
	secrets := map[string]string{}
	if secret != "" {
		secrets[FieldToken] = secret
	}
	report := Connector{}.Test(context.Background(), settings.NewResolved(ConnectorName, base, secrets))
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), fixtureOwner)
	assert.NotContains(t, string(encoded), fixtureFeedValue)
	require.Len(t, report.Checks, 1)
	return report
}

// feedHandler is a feed holding one change, at sequence 4. Like a real feed,
// it answers a read after its end with an empty page that echoes the cursor.
func feedHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureFeedValue {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(fixtureOwner))
			return
		}
		if r.URL.Path != "/api/v1/session-changes" {
			http.NotFound(w, r)
			return
		}
		var after int64
		if raw := r.URL.Query().Get("after"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			after = parsed
		}
		page := map[string]any{"changes": []map[string]any{}, "next_cursor": after}
		if after < 4 {
			page = map[string]any{
				"changes":     []map[string]any{{"change_seq": 4, "session_id": "s1", "owner": fixtureOwner}},
				"next_cursor": 4,
			}
		}
		_ = json.NewEncoder(w).Encode(page)
	}
}

// TestConnectorDefinition pins the field an existing deployment's secret variable sets.
func TestConnectorDefinition(t *testing.T) {
	def := Connector{}
	require.NotPanics(t, func() { settings.NewRegistry(def) })
	assert.Equal(t, "session_feed", def.Name())
	assert.Equal(t, "DIDEROT_SESSION_FEED_TOKEN", settings.EnvName(def.Name(), FieldToken))
}

func TestConnectorValidate(t *testing.T) {
	values := settings.Values{FieldURL: "http://feed.example.internal/", FieldViewerBaseURL: "https://sessions.example.com/"}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, "http://feed.example.internal", values[FieldURL])
	assert.Equal(t, "https://sessions.example.com", values[FieldViewerBaseURL])
	require.NoError(t, Connector{}.Validate(settings.Values{FieldURL: "https://feed.example.com/prefix"}))

	for key, bad := range map[string][]string{
		FieldURL:           {"feed.local", "ftp://feed.example.com", "https://user@feed.example.com", "https://feed.example.com/?q=1"},
		FieldViewerBaseURL: {"sessions.example.com", "https://sessions.example.com/sessions", "javascript:alert(1)"},
	} {
		for _, value := range bad {
			err := Connector{}.Validate(settings.Values{key: value})
			var fieldErrs settings.FieldErrors
			require.ErrorAs(t, err, &fieldErrs, "%s=%s", key, value)
			assert.NotEmpty(t, fieldErrs[key])
		}
	}
}

func TestConnectorTest(t *testing.T) {
	report := feedTest(t, feedHandler(t), nil, fixtureFeedValue)
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "The feed answered with the token.", report.Checks[0].Detail)
	assert.Empty(t, report.Facts)

	report = feedTest(t, feedHandler(t), nil, "another-value")
	assert.False(t, report.Passed())
	assert.Equal(t, "The feed refused the token (HTTP 401).", report.Checks[0].Detail)
	assert.NotEmpty(t, report.Checks[0].Remedy)

	report = feedTest(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "not json "+fixtureOwner)
	}, nil, fixtureFeedValue)
	assert.Equal(t, "The feed's answer was not understood.", report.Checks[0].Detail)

	report = feedTest(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"changes":[],"next_cursor":0}`)
	}, nil, fixtureFeedValue)
	assert.Equal(t, "The feed's answer was not understood.", report.Checks[0].Detail,
		"an answer that does not echo the cursor is not a session feed")

	server := httptest.NewServer(feedHandler(t))
	t.Cleanup(server.Close)
	report = feedTest(t, feedHandler(t), settings.Values{FieldURL: server.URL + "/elsewhere"}, fixtureFeedValue)
	assert.Equal(t, "The feed answered HTTP 404.", report.Checks[0].Detail)

	report = feedTest(t, feedHandler(t), settings.Values{FieldURL: "http://127.0.0.1:1"}, fixtureFeedValue)
	assert.Equal(t, "The feed is unreachable.", report.Checks[0].Detail)

	report = feedTest(t, feedHandler(t), nil, "")
	assert.Equal(t, "The feed URL or token is missing.", report.Checks[0].Detail)
}

// TestConnectorTestReadsPastTheFeedEnd pins the Test's request: one change
// after the largest cursor, which the feed answers without reading a change.
func TestConnectorTestReadsPastTheFeedEnd(t *testing.T) {
	requests := make(chan url.Values, 2)
	handler := feedHandler(t)
	report := feedTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query()
		handler(w, r)
	}, nil, fixtureFeedValue)
	require.True(t, report.Passed(), "%+v", report)
	require.Len(t, requests, 1)
	assert.Equal(t, url.Values{"after": {"9223372036854775807"}, "limit": {"1"}}, <-requests)
}

func TestSettingsFormatHidesToken(t *testing.T) {
	s := SettingsFrom(settings.NewResolved(ConnectorName, settings.Values{FieldURL: "http://feed"}, map[string]string{FieldToken: fixtureFeedValue}))
	assert.Equal(t, fixtureFeedValue, s.Token)
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v %s", s, s, s, s), fixtureFeedValue)
}
