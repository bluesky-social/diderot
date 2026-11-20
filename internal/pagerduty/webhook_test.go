package pagerduty

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeInbox struct {
	events []Event
	err    error
}

func (inbox *fakeInbox) Enqueue(_ context.Context, event Event) error {
	inbox.events = append(inbox.events, event)
	return inbox.err
}

func testEvent() Event {
	return Event{ID: "event-123", EventType: "incident.triggered",
		OccurredAt: time.Date(2026, 10, 14, 19, 30, 0, 0, time.UTC),
		Data:       Incident{ID: "P123", Title: "API errors", Service: Reference{ID: "PSVC", Summary: "api"}}}
}

func signature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// secretsOn is an enabled connector's signing secrets.
func secretsOn(secrets ...string) func() ([]string, bool) {
	return func() ([]string, bool) { return secrets, true }
}

// TestWebhookFollowsTheConnector: the listener answers 404 to everything
// while the connector is disabled, and verifies each signature against the
// secrets saved when the request arrives.
func TestWebhookFollowsTheConnector(t *testing.T) {
	var current atomic.Pointer[[]string]
	inbox := &fakeInbox{}
	handler := &Webhook{Inbox: inbox, Secrets: func() ([]string, bool) {
		if secrets := current.Load(); secrets != nil {
			return *secrets, true
		}
		return nil, false
	}}
	body, err := json.Marshal(map[string]any{"event": testEvent()})
	require.NoError(t, err)
	post := func(method, secret string) int {
		request := httptest.NewRequest(method, WebhookPath, strings.NewReader(string(body)))
		request.Header.Set("X-PagerDuty-Signature", signature(body, secret))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}

	require.Equal(t, http.StatusNotFound, post(http.MethodPost, "first"), "disabled")
	require.Equal(t, http.StatusNotFound, post(http.MethodGet, "first"), "disabled, whatever the request")
	current.Store(&[]string{"first"})
	require.Equal(t, http.StatusAccepted, post(http.MethodPost, "first"))
	current.Store(&[]string{"second", "first"})
	require.Equal(t, http.StatusAccepted, post(http.MethodPost, "first"), "the old secret during rotation")
	current.Store(&[]string{"second"})
	require.Equal(t, http.StatusForbidden, post(http.MethodPost, "first"), "a removed secret")
	require.Equal(t, http.StatusAccepted, post(http.MethodPost, "second"))
	current.Store(nil)
	require.Equal(t, http.StatusNotFound, post(http.MethodPost, "second"), "disabled again")
	require.Len(t, inbox.events, 3)
}

func TestWebhookSignedDurableAcceptance(t *testing.T) {
	inbox := &fakeInbox{}
	handler := &Webhook{Secrets: secretsOn("new-secret", "old-secret"), Inbox: inbox}
	body, err := json.Marshal(map[string]any{"event": testEvent()})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	request.Header.Set("X-PagerDuty-Signature", "v1=bad, "+signature(body, "old-secret"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code)
	require.Equal(t, []Event{testEvent()}, inbox.events)
}

func TestWebhookRejectsBeforeEnqueue(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		secret string
		method string
		status int
	}{
		{"unsigned", `{}`, "", "POST", 403},
		{"wrong secret", `{}`, "wrong", "POST", 403},
		{"bad JSON", `{`, "secret", "POST", 400},
		{"missing event", `{}`, "secret", "POST", 400},
		{"missing incident", `{"event":{"id":"e","event_type":"incident.triggered"}}`, "secret", "POST", 400},
		{"path injection", `{"event":{"id":"e","event_type":"incident.triggered","occurred_at":"2026-10-14T19:00:00Z","data":{"id":"../users","service":{"id":"s"}}}}`, "secret", "POST", 400},
		{"wrong method", `{}`, "secret", "GET", 405},
		{"oversized", strings.Repeat("x", (2<<20)+1), "secret", "POST", 413},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			inbox := &fakeInbox{}
			request := httptest.NewRequest(test.method, WebhookPath, strings.NewReader(test.body))
			if test.secret != "" {
				request.Header.Set("X-PagerDuty-Signature", signature([]byte(test.body), test.secret))
			}
			response := httptest.NewRecorder()
			(&Webhook{Secrets: secretsOn("secret"), Inbox: inbox}).ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code)
			require.Empty(t, inbox.events)
		})
	}
}

func TestWebhookIgnoredEventsAndQueueFailure(t *testing.T) {
	for _, eventType := range []string{"incident.acknowledged", "incident.resolved", "incident.escalated", "incident.triggered"} {
		t.Run(eventType, func(t *testing.T) {
			event := testEvent()
			event.EventType = eventType
			body, err := json.Marshal(map[string]any{"event": event})
			require.NoError(t, err)
			inbox := &fakeInbox{err: errors.New("database unavailable")}
			request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
			request.Header.Set("X-PagerDuty-Signature", signature(body, "secret"))
			response := httptest.NewRecorder()
			(&Webhook{Secrets: secretsOn("secret"), Inbox: inbox}).ServeHTTP(response, request)
			if eventType == "incident.triggered" {
				require.Equal(t, 503, response.Code)
			} else {
				require.Equal(t, 204, response.Code)
				require.Empty(t, inbox.events)
			}
		})
	}
}
