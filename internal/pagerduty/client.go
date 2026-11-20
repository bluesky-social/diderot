package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
)

type Client struct {
	baseURL string
	token   string
	from    string
	http    *http.Client
}

var noteMarkdownLink = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s()]+(?:\([^\s()]*\)[^\s()]*)*)\)`)

func NewClient(token, from string) *Client {
	return &Client{baseURL: "https://api.pagerduty.com", token: token, from: from,
		http: safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second, CheckRedirect: safehttp.NoFollow})}
}

func (client *Client) Context(ctx context.Context, incidentID string) (string, error) {
	if !validID(incidentID) {
		return "", fmt.Errorf("invalid PagerDuty incident ID")
	}
	incident, err := client.request(ctx, http.MethodGet, "/incidents/"+incidentID, nil)
	if err != nil {
		return "", err
	}
	alerts, err := client.request(ctx, http.MethodGet, "/incidents/"+incidentID+"/alerts?limit=10", nil)
	details := string(incident)
	if err != nil {
		details += "\nAlert details unavailable: " + err.Error()
	} else {
		details += "\nFirst page of alerts (limit 10; may be incomplete):\n" + string(alerts)
	}
	open, err := client.request(ctx, http.MethodGet, "/incidents?statuses[]=triggered&statuses[]=acknowledged&sort_by=created_at:desc&limit=25", nil)
	if err != nil {
		return details + "\nOther open incidents unavailable: " + err.Error(), nil
	}
	var snapshot struct {
		Incidents []Incident `json:"incidents"`
		More      bool       `json:"more"`
	}
	if err := json.Unmarshal(open, &snapshot); err != nil || snapshot.Incidents == nil {
		return details + "\nOther open incidents unavailable: invalid incident list response", nil
	}
	others := make([]Incident, 0, len(snapshot.Incidents))
	for _, incident := range snapshot.Incidents {
		if incident.ID != incidentID {
			others = append(others, incident)
		}
	}
	encoded, err := json.Marshal(others)
	if err != nil {
		return "", err
	}
	return details + fmt.Sprintf("\nOther open incidents at %s (newest 25; more=%t; current snapshot, not incident-time history):\n%s",
		time.Now().UTC().Format(time.RFC3339), snapshot.More, encoded), nil
}

func (client *Client) Deliver(ctx context.Context, event Event, body string) (string, error) {
	if !validID(event.Data.ID) {
		return "", fmt.Errorf("invalid PagerDuty incident ID")
	}
	digest := sha256EventID(event.ID)
	marker := "[Diderot investigation " + digest + "]"
	path := "/incidents/" + event.Data.ID + "/notes"
	raw, err := client.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var existing struct {
		Notes *[]struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(raw, &existing); err != nil {
		return "", fmt.Errorf("decode PagerDuty notes: %w", err)
	}
	if existing.Notes == nil {
		return "", fmt.Errorf("PagerDuty notes response missing notes array")
	}
	for _, note := range *existing.Notes {
		if strings.HasPrefix(note.Content, marker+"\n") && note.ID != "" {
			return note.ID, nil
		}
	}
	encoded, err := json.Marshal(map[string]any{"note": map[string]string{"content": marker + "\n" + noteMarkdownLink.ReplaceAllString(body, "$1: $2")}})
	if err != nil {
		return "", err
	}
	raw, err = client.request(ctx, http.MethodPost, path, encoded)
	if err != nil {
		return "", err
	}
	var response struct {
		Note struct {
			ID string `json:"id"`
		} `json:"note"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("decode PagerDuty note: %w", err)
	}
	if response.Note.ID == "" {
		return "", fmt.Errorf("PagerDuty note response missing ID")
	}
	return response.Note.ID, nil
}

// StatusError is a PagerDuty API answer outside 2xx. It carries no body.
type StatusError struct {
	Method, Path string
	StatusCode   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("PagerDuty %s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
}

func (client *Client) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token token="+client.token)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("From", client.from)
	}
	response, err := client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PagerDuty %s request: %w", method, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &StatusError{Method: method, Path: path, StatusCode: response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		return nil, fmt.Errorf("PagerDuty response exceeds 2 MiB")
	}
	return raw, nil
}
