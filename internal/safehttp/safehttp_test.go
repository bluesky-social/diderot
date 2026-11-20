package safehttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// redirectServer answers every request with a redirect to itself, counting
// the requests.
func redirectServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestNewClientCarriesTheTimeout(t *testing.T) {
	if got := NewClient(Options{Timeout: 45 * time.Second}).Timeout; got != 45*time.Second {
		t.Fatalf("Timeout = %v, want 45s", got)
	}
	if got := NewClient(Options{}).Timeout; got != 0 {
		t.Fatalf("zero Options Timeout = %v, want none", got)
	}
}

func TestDefaultFollowsTenRedirects(t *testing.T) {
	srv, hits := redirectServer(t)
	_, err := NewClient(Options{}).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want net/http's ten-redirect limit", err)
	}
	if got := hits.Load(); got != 10 {
		t.Fatalf("hits = %d, want 10", got)
	}
}

func TestMaxRedirects(t *testing.T) {
	srv, hits := redirectServer(t)
	_, err := NewClient(Options{CheckRedirect: MaxRedirects(5)}).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("err = %v, want the five-redirect limit", err)
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("hits = %d, want 5", got)
	}
}

func TestNoFollowReturnsTheRedirect(t *testing.T) {
	srv, hits := redirectServer(t)
	resp, err := NewClient(Options{CheckRedirect: NoFollow}).Get(srv.URL)
	if err != nil {
		t.Fatalf("err = %v, want the redirect response", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/next" {
		t.Fatalf("got %d to %q, want the unfollowed 302", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestFailOnRedirect(t *testing.T) {
	srv, hits := redirectServer(t)
	resp, err := NewClient(Options{FailOnRedirect: true}).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("got %d, want an error", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "unexpected redirect to /next (status 302)") {
		t.Fatalf("err = %v, want the refused redirect", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestFailOnRedirectKeepsThePublicOnlyDialer(t *testing.T) {
	srv, hits := redirectServer(t)
	resp, err := NewClient(Options{FailOnRedirect: true, PublicOnly: true}).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("got %d, want a blocked dial", resp.StatusCode)
	}
	var blocked *BlockedAddressError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *BlockedAddressError", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("hits = %d, want 0", got)
	}
}

func TestFailOnRedirectPassesOtherAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	resp, err := NewClient(Options{FailOnRedirect: true}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
}
