package pagerduty

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const WebhookPath = "/webhooks/pagerduty"

type Reference struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Name    string `json:"name,omitempty"`
	HTMLURL string `json:"html_url"`
}

type Incident struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	HTMLURL   string    `json:"html_url"`
	Status    string    `json:"status"`
	Urgency   string    `json:"urgency"`
	CreatedAt time.Time `json:"created_at"`
	Service   Reference `json:"service"`
}

type Event struct {
	ID         string    `json:"id"`
	EventType  string    `json:"event_type"`
	OccurredAt time.Time `json:"occurred_at"`
	Data       Incident  `json:"data"`
}

type Inbox interface {
	Enqueue(context.Context, Event) error
}

type Webhook struct {
	// Secrets returns the signing secrets of the PagerDuty connector's
	// current settings, and false while it is disabled, when the webhook
	// answers 404 to everything.
	Secrets func() ([]string, bool)
	Inbox   Inbox
	Logger  *slog.Logger
}

func (handler *Webhook) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	secrets, enabled := handler.Secrets()
	if !enabled {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 2<<20))
	if err != nil {
		http.Error(writer, "invalid or oversized webhook", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(body, request.Header.Values("X-PagerDuty-Signature"), secrets) {
		http.Error(writer, "invalid signature", http.StatusForbidden)
		return
	}
	var payload struct {
		Event Event `json:"event"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Event.ID == "" || payload.Event.EventType == "" {
		http.Error(writer, "invalid v3 event", http.StatusBadRequest)
		return
	}
	event := payload.Event
	if event.EventType != "incident.triggered" {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if !validID(event.Data.ID) || event.OccurredAt.IsZero() || event.Data.Service.ID == "" {
		http.Error(writer, "incomplete incident event", http.StatusBadRequest)
		return
	}
	if handler.Inbox == nil {
		http.Error(writer, "inbox unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := handler.Inbox.Enqueue(request.Context(), event); err != nil {
		logger := handler.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("pagerduty enqueue failed", "event_id", event.ID, "err", err)
		http.Error(writer, "inbox unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.WriteHeader(http.StatusAccepted)
}

func validSignature(body []byte, headers, secrets []string) bool {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		expected := mac.Sum(nil)
		for _, header := range headers {
			for _, candidate := range strings.Split(header, ",") {
				version, digest, ok := strings.Cut(strings.TrimSpace(candidate), "=")
				if !ok || version != "v1" {
					continue
				}
				actual, err := hex.DecodeString(digest)
				if err == nil && hmac.Equal(expected, actual) {
					return true
				}
			}
		}
	}
	return false
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}
