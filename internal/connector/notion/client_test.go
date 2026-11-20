package notion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastClientCfg returns a Config with retry timing turned way down
// so the tests don't actually wait seconds. Rate is 100 req/s,
// burst 100, so the limiter is effectively a no-op for the test.
func fastClientCfg(srv *httptest.Server) Config {
	return Config{
		Token:      "ntn_test",
		BaseURL:    srv.URL,
		Rate:       100,
		Burst:      100,
		MaxRetries: 3,
		HTTPClient: srv.Client(),
	}
}

func TestClient_AuthAndVersionHeaders(t *testing.T) {
	var gotAuth, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("Notion-Version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var out map[string]any
	if err := c.Do(context.Background(), http.MethodGet, "anything", nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer ntn_test" {
		t.Errorf("Authorization=%q, want %q", gotAuth, "Bearer ntn_test")
	}
	if gotVersion != notionAPIVersion {
		t.Errorf("Notion-Version=%q, want %q", gotVersion, notionAPIVersion)
	}
}

func TestClient_RetriesOn429AndRecovers(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			// Honor a tiny Retry-After so the test runs quickly.
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"got_attempt": 3}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	var out map[string]any
	if err := c.Do(context.Background(), http.MethodGet, "anything", nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts=%d, want 3", got)
	}
	// Sanity: we should have waited at least the 2 Retry-After sleeps
	// (~10ms each, plus jitter). Use a lower bound to avoid flake on
	// fast machines — exact wait is non-deterministic with jitter.
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("elapsed=%v, expected >=10ms (Retry-After sleep didn't happen)", elapsed)
	}
}

func TestClient_GivesUpAfterMaxRetriesOn429(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0.001")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = c.Do(context.Background(), http.MethodGet, "anything", nil, nil)
	if err == nil {
		t.Fatal("Do returned nil, want HTTPError")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err type=%T, want *HTTPError; %v", err, err)
	}
	if he.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode=%d, want 429", he.StatusCode)
	}
	// MaxRetries=3 means 1 initial + 3 retries = 4 total attempts.
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("attempts=%d, want 4 (1 initial + 3 retries)", got)
	}
}

func TestClient_FailsFastOn404(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"object": "error", "code": "object_not_found"}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = c.Do(context.Background(), http.MethodGet, "pages/abc", nil, nil)
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(err)=false, want true; err=%v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts=%d, want 1 (404 should fail fast, not retry)", got)
	}
}

func TestClient_FailsFastOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = c.Do(context.Background(), http.MethodGet, "pages/abc", nil, nil)
	if !IsForbidden(err) {
		t.Errorf("IsForbidden(err)=false, want true; err=%v", err)
	}
}

func TestClient_RetriesOn5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := fastClientCfg(srv)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "anything", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts=%d, want 2", got)
	}
}

func TestClient_ContextCancelInterruptsRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Always return 429 with a long Retry-After so cancellation
		// catches us inside sleepWithCtx.
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err = c.Do(ctx, http.MethodGet, "anything", nil, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

func TestRetryAfterWithJitter(t *testing.T) {
	tests := []struct {
		name               string
		header             string
		minWaitMs          int64
		maxWaitMs          int64
		fallsBackToBackoff bool
	}{
		// "2.0" → base 2s, jitter 0..400ms → [2000, 2400] ms.
		{"valid decimal", "2.0", 2000, 2400, false},
		// "0.5" → base 500ms, jitter 0..100ms → [500, 600] ms.
		{"sub-second", "0.5", 500, 600, false},
		// Empty header → falls back to expBackoff(0) = 1s exactly,
		// no jitter from this function.
		{"empty header", "", 1000, 1000, true},
		// Garbage → fall back to expBackoff(0) = 1s.
		{"garbage", "not a number", 1000, 1000, true},
		// Negative → fall back.
		{"negative", "-5", 1000, 1000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Run a bunch of iterations to exercise the jitter.
			for i := 0; i < 100; i++ {
				got := retryAfterWithJitter(tt.header, 0)
				ms := got.Milliseconds()
				if ms < tt.minWaitMs || ms > tt.maxWaitMs {
					t.Fatalf("got %dms, want [%d,%d]ms (header=%q)",
						ms, tt.minWaitMs, tt.maxWaitMs, tt.header)
				}
				if tt.fallsBackToBackoff && got != expBackoff(0) {
					t.Fatalf("expected fallback to expBackoff(0); got %v", got)
				}
			}
		})
	}
}

func TestRetryAfterJitter_StrictlyAdditive(t *testing.T) {
	// Critical invariant from the design doc: jitter must NEVER
	// reduce the wait window below Retry-After. Sample 1000 times
	// against a known base.
	const baseSec = 5.0
	const baseMs = int64(baseSec * 1000)
	for i := 0; i < 1000; i++ {
		got := retryAfterWithJitter("5.0", 0)
		if got.Milliseconds() < baseMs {
			t.Fatalf("jitter dropped wait below base: got %v, base %v", got, time.Duration(baseSec*float64(time.Second)))
		}
	}
}

func TestExpBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // capped
		{50, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt=%d", tt.attempt), func(t *testing.T) {
			if got := expBackoff(tt.attempt); got != tt.want {
				t.Errorf("expBackoff(%d)=%v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestSleepWithCtx_NegativeNoop(t *testing.T) {
	// Negative or zero sleeps should return immediately without
	// blocking.
	start := time.Now()
	if err := sleepWithCtx(context.Background(), -time.Second); err != nil {
		t.Fatalf("sleepWithCtx: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Errorf("negative sleep took %v, expected near-zero", elapsed)
	}
}

func TestNewClient_RejectsEmptyToken(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil ||
		!strings.Contains(err.Error(), "empty Token") {
		t.Errorf("NewClient(empty)=%v, want 'empty Token' error", err)
	}
}

func TestStripDashes(t *testing.T) {
	got := stripDashes("abcd1234-ef56-7890-abcd-1234567890ab")
	want := "abcd1234ef567890abcd1234567890ab"
	if got != want {
		t.Errorf("stripDashes()=%q, want %q", got, want)
	}
}
