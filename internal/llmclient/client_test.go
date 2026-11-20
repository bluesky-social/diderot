package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	for _, baseURL := range []string{"", "ftp://gateway", "/v1", "https://user:secret@gateway/v1", "https://gateway?", "https://gateway?key=secret", "https://gateway#fragment", "://"} {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := New(Config{BaseURL: baseURL, APIKey: "key"}); err == nil {
				t.Fatal("accepted invalid base URL")
			}
		})
	}
	// No key is a server that takes none.
	if _, err := New(Config{BaseURL: "https://gateway/v1", APIKey: "  "}); err != nil {
		t.Fatalf("refused a server that takes no key: %v", err)
	}
}

func TestPostJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/prefix/v1/messages" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer key" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("unexpected headers: %v", request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"model":"claude"}` {
			t.Errorf("body = %s", body)
		}
		_, _ = io.WriteString(writer, `{"ok":true}`)
	}))
	defer server.Close()
	headers := http.Header{"Anthropic-Version": {"2023-06-01"}}
	httpClient := &http.Client{Timeout: 17 * time.Second}
	client, err := New(Config{BaseURL: " " + server.URL + "/prefix/v1/ ", APIKey: " key ", HTTPClient: httpClient, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	headers.Set("Anthropic-Version", "changed")
	if client.http == httpClient || client.http.Timeout != httpClient.Timeout || httpClient.CheckRedirect != nil {
		t.Fatal("client was mutated or timeout lost")
	}
	body, err := client.PostJSON(t.Context(), "/messages", map[string]string{"model": "claude"}, 11)
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("body = %s, error = %v", body, err)
	}
}

// TestAPIKeyAuthStyle: x-api-key replaces the bearer rather than joining it,
// so an Anthropic key is never also offered as a bearer token.
func TestAPIKeyAuthStyle(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got = request.Header.Clone()
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "sk-ant", Auth: AuthXAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PostJSON(t.Context(), "/messages", map[string]string{}, 8); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Api-Key") != "sk-ant" || got.Get("Authorization") != "" {
		t.Fatalf("headers = %v", got)
	}
	if _, err := New(Config{BaseURL: server.URL, APIKey: "k", Auth: "basic"}); err == nil {
		t.Fatal("accepted an unknown auth style")
	}
}

func TestWithExtraFields(t *testing.T) {
	type request struct {
		Model  string         `json:"model"`
		Nested map[string]any `json:"nested"`
	}
	payload := request{Model: "m", Nested: map[string]any{"a": map[string]any{"b": 1}}}

	unchanged, err := WithExtraFields(payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unchanged.(request); !ok {
		t.Fatalf("empty extras must return the payload untouched, got %T", unchanged)
	}

	merged, err := WithExtraFields(payload, map[string]any{"z": true, "thinking_config": map[string]any{"thinkingBudget": 0}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","nested":{"a":{"b":1}},"thinking_config":{"thinkingBudget":0},"z":true}`
	if string(encoded) != want {
		t.Fatalf("merged = %s, want %s", encoded, want)
	}

	if _, err := WithExtraFields(payload, map[string]any{"model": "other"}); err == nil {
		t.Fatal("accepted an extra field that overrides a request field")
	}
	if _, err := WithExtraFields([]string{"not", "an", "object"}, map[string]any{"x": 1}); err == nil {
		t.Fatal("accepted a non-object payload")
	}
	empty, err := WithExtraFields(struct{}{}, map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(empty); string(encoded) != `{"x":1}` {
		t.Fatalf("empty object merge = %s", encoded)
	}
}

// A ChatTokenLimit embedded in a request encodes exactly one field, where the
// request places it.
func TestChatTokenParam(t *testing.T) {
	type request struct {
		Model string `json:"model"`
		ChatTokenLimit
		Messages []string `json:"messages"`
	}
	for raw, want := range map[string]string{
		"":                      `{"model":"m","max_tokens":16,"messages":null}`,
		"max_tokens":            `{"model":"m","max_tokens":16,"messages":null}`,
		"max_completion_tokens": `{"model":"m","max_completion_tokens":16,"messages":null}`,
	} {
		param, err := ParseChatTokenParam(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		encoded, err := json.Marshal(request{Model: "m", ChatTokenLimit: param.Limit(16)})
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != want {
			t.Fatalf("%q: encoded %s, want %s", raw, encoded, want)
		}
	}
	if _, err := ParseChatTokenParam("max_output_tokens"); err == nil {
		t.Fatal("accepted an unknown chat token parameter")
	}
}

func TestResponseLimitsAndStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		limit  int64
	}{
		{"exact limit", 200, "1234", 4},
		{"oversize", 200, "12345", 4},
		{"status", 429, strings.Repeat("x", 1024), 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, APIKey: "key"})
			if err != nil {
				t.Fatal(err)
			}
			body, err := client.PostJSON(t.Context(), "/responses", struct{}{}, test.limit)
			switch test.name {
			case "exact limit":
				if err != nil || string(body) != test.body {
					t.Fatalf("body = %q, error = %v", body, err)
				}
			case "oversize":
				var bodyError *BodyError
				if !errors.As(err, &bodyError) || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("error = %v", err)
				}
			case "status":
				var httpError *HTTPError
				if !errors.As(err, &httpError) || httpError.StatusCode != 429 || len(httpError.Body) != 512 {
					t.Fatalf("error = %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("request count = %d, want 1", calls.Load())
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected.Store(true)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PostJSON(t.Context(), "/messages", struct{}{}, 1024)
	var httpError *HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != 307 || redirected.Load() {
		t.Fatalf("redirected = %v, error = %v", redirected.Load(), err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (transport roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }

func TestReadErrorAndCancellation(t *testing.T) {
	client, err := New(Config{BaseURL: "https://gateway/v1", APIKey: "key", HTTPClient: &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.Context().Err(); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Body: brokenBody{}, Header: make(http.Header)}, nil
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PostJSON(t.Context(), "/messages", struct{}{}, 1024)
	var bodyError *BodyError
	if !errors.As(err, &bodyError) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.PostJSON(ctx, "/messages", struct{}{}, 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
