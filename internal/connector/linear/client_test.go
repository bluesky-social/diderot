package linear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/ratelimit"
)

// testProvider returns a TokenProvider backed by a static token.
// Shape: lin_api_ + 40 random-ish chars to match the real key shape.
func testProvider(token string) *StaticTokenProvider {
	return NewStaticTokenProvider(token)
}

// syntheticToken is a plausible-shape personal API key for tests.
// Synthetic — not a real key.
const syntheticToken = "lin_api_test00000000000000000000000000000000"

// newTestClient builds a Client that talks to srv and uses syntheticToken.
// Rate limiter is set to high values so it's not the bottleneck in tests.
func newTestClient(srv *httptest.Server) *Client {
	provider := testProvider(syntheticToken)
	c := NewClient(provider)
	// Override the HTTP client to use the test server's.
	c.httpClient = srv.Client()
	// Override the URL by swapping transport target — since the client
	// hardcodes linearAPIURL, we use a custom transport that rewrites
	// the host to the test server.
	c.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	return c
}

// rewriteTransport rewrites requests to target host for testing.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Build a copy of the request with the host rewritten.
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.target[len("http://"):]
	clone.URL.Path = req.URL.Path
	if t.base == nil {
		return http.DefaultTransport.RoundTrip(clone)
	}
	return t.base.RoundTrip(clone)
}

func TestClient_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != syntheticToken {
		t.Errorf("Authorization=%q, want bare token %q", gotAuth, syntheticToken)
	}
}

func TestClient_ComplexityHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Complexity", "42")
		w.Header().Set("X-Complexity-Budget", "250000")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, ci, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if ci.RequestedTokens != 42 {
		t.Errorf("RequestedTokens=%d, want 42", ci.RequestedTokens)
	}
	if ci.AllowedTokens != 250000 {
		t.Errorf("AllowedTokens=%d, want 250000", ci.AllowedTokens)
	}
}

func TestClient_401RetryOnce(t *testing.T) {
	// The static provider can't actually get a new token; this tests
	// that we retry exactly once on 401 before surfacing the error.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("HTTP calls=%d, want 2 (one attempt + one retry)", n)
	}
}

func TestClient_RetriesTransient(t *testing.T) {
	// Two 503s then a success: Do should retry through them and return the
	// data. Without transient retries this fails on the first 503 — the
	// exact failure mode behind the visibility-sweep-stalled alert.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.retryBackoff = time.Millisecond
	c.limiter = ratelimit.New(rate.Inf, 1) // don't let pacing slow the test

	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err != nil {
		t.Fatalf("Do: expected success after retries, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("HTTP calls=%d, want 3 (two 503s + one success)", n)
	}
}

func TestClient_RetriesExhausted(t *testing.T) {
	// Persistent 503: Do should give up after 1 initial + maxTransientRetries
	// attempts and surface the HTTP 503.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.retryBackoff = time.Millisecond
	c.limiter = ratelimit.New(rate.Inf, 1)

	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if want := int32(1 + maxTransientRetries); atomic.LoadInt32(&calls) != want {
		t.Errorf("HTTP calls=%d, want %d (1 initial + %d retries)", calls, want, maxTransientRetries)
	}
	if !containsStr(err.Error(), "HTTP 503") {
		t.Errorf("error=%q, want to contain %q", err.Error(), "HTTP 503")
	}
}

func TestClient_GraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"not authorized"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err == nil {
		t.Fatal("expected error from GraphQL errors, got nil")
	}
	if want := "not authorized"; !containsStr(err.Error(), want) {
		t.Errorf("error=%q, want to contain %q", err.Error(), want)
	}
}

func TestClient_RefuseRedirect(t *testing.T) {
	// Set up a server that redirects. The client should refuse the redirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/steal", http.StatusFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.Do(context.Background(), "query { viewer { id } }", nil)
	if err == nil {
		t.Fatal("expected error on redirect, got nil")
	}
	if !containsStr(err.Error(), "redirect") {
		t.Errorf("error=%q, want redirect error", err.Error())
	}
}

func TestStaticTokenProvider_Invalidate(t *testing.T) {
	// Invalidate is a no-op for static provider; the token is still
	// returned on the next call (there's nothing to refresh).
	p := testProvider(syntheticToken)
	p.Invalidate()
	tok, err := p.Authorization(context.Background())
	if err != nil {
		t.Fatalf("Authorization after Invalidate: %v", err)
	}
	if tok != syntheticToken {
		t.Errorf("tok=%q, want %q", tok, syntheticToken)
	}
}

func TestParseComplexityHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Complexity", "100")
	h.Set("X-Complexity-Budget", "250000")
	h.Set("X-Complexity-Reset-At", "2026-07-11T10:00:00Z")

	ci := parseComplexityHeaders(h)
	if ci.RequestedTokens != 100 {
		t.Errorf("RequestedTokens=%d, want 100", ci.RequestedTokens)
	}
	if ci.AllowedTokens != 250000 {
		t.Errorf("AllowedTokens=%d, want 250000", ci.AllowedTokens)
	}
	expected, _ := time.Parse(time.RFC3339, "2026-07-11T10:00:00Z")
	if !ci.ResetAt.Equal(expected) {
		t.Errorf("ResetAt=%v, want %v", ci.ResetAt, expected)
	}
}

func TestParseComplexityHeaders_Missing(t *testing.T) {
	ci := parseComplexityHeaders(http.Header{})
	if ci.RequestedTokens != 0 || ci.AllowedTokens != 0 || !ci.ResetAt.IsZero() {
		t.Errorf("expected zero ComplexityInfo for missing headers, got %+v", ci)
	}
}

func TestIsUUID(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"550e8400-e29b-41d4-a716-446655440000", true},
		{"550e8400-e29b-41d4-a716-44665544000g", false},  // invalid hex char
		{"550e8400-e29b-41d4-a716-44665544000", false},   // too short
		{"550e8400-e29b-41d4-a716-4466554400000", false}, // too long
		{"550e8400-e29b-41d4-a716_446655440000", false},  // wrong separator
		{"my-project-name", false},
		{"", false},
		{"a0000000-0000-0000-0000-000000000000", true},
		{"A0000000-0000-0000-0000-000000000000", true}, // uppercase OK
	}
	for _, c := range cases {
		got := isUUID(c.s)
		if got != c.want {
			t.Errorf("isUUID(%q)=%v, want %v", c.s, got, c.want)
		}
	}
}

func TestResolveProjectID_UUID(t *testing.T) {
	// When arg is already a UUID, no HTTP call should be made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP call during UUID passthrough")
	}))
	defer srv.Close()

	c := newTestClient(srv)
	const uuid = "550e8400-e29b-41d4-a716-446655440000"
	got, err := c.ResolveProjectID(context.Background(), uuid)
	if err != nil {
		t.Fatalf("ResolveProjectID: %v", err)
	}
	if got != uuid {
		t.Errorf("got=%q, want %q", got, uuid)
	}
}

func TestResolveProjectID_ByName_Found(t *testing.T) {
	const projectID = "proj-uuid-0000-0000-0000-000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"projects": map[string]any{
					"nodes": []any{
						map[string]any{"id": projectID, "name": "My Project"},
					},
				},
			},
		}))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ResolveProjectID(context.Background(), "My Project")
	if err != nil {
		t.Fatalf("ResolveProjectID: %v", err)
	}
	if got != projectID {
		t.Errorf("got=%q, want %q", got, projectID)
	}
}

func TestResolveProjectID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"projects": map[string]any{
					"nodes": []any{},
				},
			},
		}))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ResolveProjectID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for no-match, got nil")
	}
	if !containsStr(err.Error(), "no Linear project found") {
		t.Errorf("err=%q, want 'no Linear project found'", err.Error())
	}
}

func TestResolveProjectID_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"projects": map[string]any{
					"nodes": []any{
						map[string]any{"id": "id-1", "name": "My Project"},
						map[string]any{"id": "id-2", "name": "My Project 2"},
					},
				},
			},
		}))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ResolveProjectID(context.Background(), "My Project")
	if err == nil {
		t.Fatal("expected error for ambiguous match, got nil")
	}
	if !containsStr(err.Error(), "ambiguous") {
		t.Errorf("err=%q, want 'ambiguous'", err.Error())
	}
}

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && findStr(s, sub)
}

func findStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
