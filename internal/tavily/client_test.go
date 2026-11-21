package tavily

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/websearch"
)

// TestSearch_RequestAndResponse asserts the /search request shape
// (method, path, auth header, body) and that the response decodes to
// the trimmed websearch.SearchResult.
func TestSearch_RequestAndResponse(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody searchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{
			"query": "q",
			"results": [
				{"title": "T", "url": "https://ex.com", "content": "snip", "score": 0.9},
				{"title": "T2", "url": "https://ex.com/2", "content": "snip2", "score": 0.5}
			],
			"response_time": 0.1
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tvly-testkey", srv.Client())
	res, err := c.Search(context.Background(), websearch.SearchParams{
		Query:      "q",
		MaxResults: 2,
		Topic:      "news",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/search" {
		t.Errorf("path = %q, want /search", gotPath)
	}
	if gotAuth != "Bearer tvly-testkey" {
		t.Errorf("auth = %q, want Bearer tvly-testkey", gotAuth)
	}
	if gotBody.Query != "q" || gotBody.MaxResults != 2 || gotBody.Topic != "news" {
		t.Errorf("body = %+v, want query=q max=2 topic=news", gotBody)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
	if res.Results[0].URL != "https://ex.com" || res.Results[0].Score != 0.9 {
		t.Errorf("result[0] = %+v", res.Results[0])
	}
}

// TestSearch_OmitsEmptyOptionalFields confirms unset optional params
// stay off the wire so Tavily applies its own defaults.
func TestSearch_OmitsEmptyOptionalFields(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &raw)
		_, _ = io.WriteString(w, `{"query":"q","results":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", srv.Client())
	if _, err := c.Search(context.Background(), websearch.SearchParams{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, k := range []string{"max_results", "topic", "search_depth", "time_range", "include_domains", "exclude_domains"} {
		if _, present := raw[k]; present {
			t.Errorf("expected %q omitted from body, got %v", k, raw[k])
		}
	}
}

// TestExtract_RequestAndResponse asserts the /extract request shape
// and that both results and failed_results decode.
func TestExtract_RequestAndResponse(t *testing.T) {
	var gotBody extractRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{
			"results": [{"url": "https://ok.com", "raw_content": "# Page"}],
			"failed_results": [{"url": "https://bad.com", "error": "404"}],
			"response_time": 0.2
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", srv.Client())
	res, err := c.Extract(context.Background(), []string{"https://ok.com", "https://bad.com"}, "intent")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(gotBody.URLs) != 2 || gotBody.Query != "intent" || gotBody.Format != "markdown" {
		t.Errorf("body = %+v, want 2 urls, query=intent, format=markdown", gotBody)
	}
	if len(res.Results) != 1 || res.Results[0].RawContent != "# Page" {
		t.Errorf("results = %+v", res.Results)
	}
	if len(res.Failed) != 1 || res.Failed[0].Error != "404" {
		t.Errorf("failed = %+v", res.Failed)
	}
}

// TestPostJSON_HTTPError surfaces a non-2xx as *HTTPError with the
// status and body preview.
func TestPostJSON_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"bad key"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", srv.Client())
	_, err := c.Search(context.Background(), websearch.SearchParams{Query: "q"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var he *HTTPError
	if !strings.Contains(err.Error(), "tavily http 401") {
		t.Errorf("error = %v, want tavily http 401", err)
	}
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected *HTTPError with 401, got %v", err)
	}
}

// TestSearch_EmptyQuery rejects an empty query before any HTTP call.
func TestSearch_EmptyQuery(t *testing.T) {
	c := NewClient("", "k", nil)
	if _, err := c.Search(context.Background(), websearch.SearchParams{Query: "  "}); err == nil {
		t.Fatal("expected error on empty query")
	}
}
