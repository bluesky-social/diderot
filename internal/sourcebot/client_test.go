package sourcebot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestSearch_RequestAndResponse asserts the /api/search request
// shape (method, auth, body) and the trimmed decode.
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
			"stats": {"actualMatchCount": 2, "totalMatchCount": 9},
			"files": [
				{
					"fileName": {"text": "internal/ask/session.go"},
					"webUrl": "/repo/x/session.go",
					"externalWebUrl": "https://github.com/bluesky-social/diderot/blob/main/internal/ask/session.go",
					"repository": "github.com/bluesky-social/diderot",
					"language": "Go",
					"chunks": [
						{"content": "func LoadOrCreate(...)", "contentStart": {"lineNumber": 42}}
					]
				}
			]
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sbk_test", srv.Client())
	res, err := c.Search(context.Background(), SearchParams{Query: "LoadOrCreate repo:diderot", Matches: 10, ContextLines: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/search" {
		t.Errorf("request = %s %s, want POST /api/search", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sbk_test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.Query != "LoadOrCreate repo:diderot" || gotBody.Matches != 10 || gotBody.ContextLines != 3 {
		t.Errorf("body = %+v", gotBody)
	}
	if res.TotalMatchCount != 9 || len(res.Files) != 1 {
		t.Fatalf("res = %+v", res)
	}
	f := res.Files[0]
	if f.Repository != "github.com/bluesky-social/diderot" || f.Path != "internal/ask/session.go" {
		t.Errorf("file = %+v", f)
	}
	wantExtURL := "https://github.com/bluesky-social/diderot/blob/main/internal/ask/session.go"
	if f.ExternalWebURL != wantExtURL {
		t.Errorf("ExternalWebURL = %q, want %q", f.ExternalWebURL, wantExtURL)
	}
	if len(f.Chunks) != 1 || f.Chunks[0].StartLine != 42 {
		t.Errorf("chunks = %+v", f.Chunks)
	}
}

// TestSearch_Validation rejects empty query and non-positive matches
// before any HTTP.
func TestSearch_Validation(t *testing.T) {
	c := NewClient("http://unused", "k", nil)
	if _, err := c.Search(context.Background(), SearchParams{Query: " ", Matches: 5}); err == nil {
		t.Error("expected error on empty query")
	}
	if _, err := c.Search(context.Background(), SearchParams{Query: "q", Matches: 0}); err == nil {
		t.Error("expected error on matches=0")
	}
}

// TestSource_RequestAndResponse asserts the /api/source query params
// and decode.
func TestSource_RequestAndResponse(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{
			"source": "package main",
			"language": "Go",
			"path": "cmd/diderot/main.go",
			"repo": "github.com/bluesky-social/diderot",
			"webUrl": "/repo/diderot/main.go",
			"externalWebUrl": "https://github.com/bluesky-social/diderot/blob/main/cmd/diderot/main.go"
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", srv.Client())
	src, err := c.Source(context.Background(), "github.com/bluesky-social/diderot", "cmd/diderot/main.go", "")
	if err != nil {
		t.Fatalf("Source: %v", err)
	}
	if gotQuery.Get("repo") != "github.com/bluesky-social/diderot" || gotQuery.Get("path") != "cmd/diderot/main.go" {
		t.Errorf("query = %v", gotQuery)
	}
	if _, present := gotQuery["ref"]; present {
		t.Error("ref should be omitted when empty")
	}
	if src.Source != "package main" || src.Language != "Go" {
		t.Errorf("src = %+v", src)
	}
	wantExtURL := "https://github.com/bluesky-social/diderot/blob/main/cmd/diderot/main.go"
	if src.ExternalWebURL != wantExtURL {
		t.Errorf("ExternalWebURL = %q, want %q", src.ExternalWebURL, wantExtURL)
	}
}

// TestSource_NotFound maps a 404 to ErrNotFound.
func TestSource_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", srv.Client())
	_, err := c.Source(context.Background(), "github.com/example/x", "nope.go", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestDo_HTTPError surfaces non-2xx as *HTTPError.
func TestDo_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad", srv.Client())
	_, err := c.Search(context.Background(), SearchParams{Query: "q", Matches: 5})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want HTTPError 401", err)
	}
}
