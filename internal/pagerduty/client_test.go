package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientReconcilesUnknownNoteOutcome(t *testing.T) {
	var saved string
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Token token=token", request.Header.Get("Authorization"))
		require.Equal(t, "/incidents/P123/notes", request.URL.Path)
		switch request.Method {
		case http.MethodGet:
			notes := []map[string]string{}
			if saved != "" {
				notes = append(notes, map[string]string{"id": "NOTE1", "content": saved})
			}
			require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"notes": notes}))
		case http.MethodPost:
			posts++
			require.Equal(t, "bot@example.com", request.Header.Get("From"))
			var body struct {
				Note struct {
					Content string `json:"content"`
				} `json:"note"`
			}
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			saved = body.Note.Content
			writer.WriteHeader(http.StatusBadGateway)
		default:
			t.Errorf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	client := NewClient("token", "bot@example.com")
	client.baseURL = server.URL
	_, err := client.Deliver(context.Background(), testEvent(), "report [Grafana](https://grafana.example/explore?orgId=1&from=123) [Transcript](https://diderot.example/session)")
	require.ErrorContains(t, err, "HTTP 502")
	id, err := client.Deliver(context.Background(), testEvent(), "report")
	require.NoError(t, err)
	require.Equal(t, "NOTE1", id)
	require.Equal(t, 1, posts)
	require.Contains(t, saved, "[Diderot investigation ")
	require.Contains(t, saved, "Grafana: https://grafana.example/explore?orgId=1&from=123")
	require.Contains(t, saved, "Transcript: https://diderot.example/session")
	require.NotContains(t, saved, "](https://")
}

func TestClientRefusesRedirects(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { targetCalls++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := NewClient("token", "bot@example.com")
	client.baseURL = server.URL
	_, err := client.Context(context.Background(), "P123")
	require.ErrorContains(t, err, "HTTP 307")
	require.Zero(t, targetCalls)
}

func TestClientRejectsMissingNoteListBeforePosting(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts++
		}
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := NewClient("token", "bot@example.com")
	client.baseURL = server.URL
	_, err := client.Deliver(context.Background(), testEvent(), "report")
	require.ErrorContains(t, err, "missing notes array")
	require.Zero(t, posts)
}

func TestClientContextIncludesBoundedOpenIncidentSnapshot(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/incidents/P123":
					_, _ = writer.Write([]byte(`{"incident":{"id":"P123","title":"target alert"}}`))
				case "/incidents/P123/alerts":
					_, _ = writer.Write([]byte(`{"alerts":[],"more":false}`))
				case "/incidents":
					require.Equal(t, []string{"triggered", "acknowledged"}, request.URL.Query()["statuses[]"])
					require.Equal(t, "25", request.URL.Query().Get("limit"))
					if unavailable {
						writer.WriteHeader(http.StatusForbidden)
						return
					}
					_, _ = writer.Write([]byte(`{"incidents":[{"id":"P123","title":"target alert"},{"id":"POTHER","title":"upstream failure","html_url":"https://acme.pagerduty.com/incidents/POTHER","status":"acknowledged"}],"more":true}`))
				default:
					t.Errorf("unexpected request %s", request.URL.Path)
				}
			}))
			defer server.Close()
			client := NewClient("token", "bot@example.com")
			client.baseURL = server.URL
			result, err := client.Context(t.Context(), "P123")
			require.NoError(t, err)
			require.Contains(t, result, "target alert")
			if unavailable {
				require.Contains(t, result, "Other open incidents unavailable")
			} else {
				require.Contains(t, result, "upstream failure")
				require.Contains(t, result, "more=true; current snapshot, not incident-time history")
				require.Equal(t, 1, strings.Count(result, `"id":"P123"`))
			}
		})
	}
}
