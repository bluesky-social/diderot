// Package notion is the diderot connector for Notion. It wraps the
// Notion API behind a rate-limited HTTP client (Client) and provides
// discovery audit, managed-root walk, body render, and ingest plumbing
// consumed by the leader-only sync worker
// backfill.
//
// Client is the thin auth + retry layer. It enforces an in-process
// token bucket at 3 req/s (Notion's documented sustained rate),
// honors Retry-After on 429 with strictly-additive jitter, and
// retries 5xx with exponential backoff. Concurrent callers share
// one bucket — that's the load-bearing property for the small
// worker pool the design relies on to overcome Notion's per-call
// latency.
//
// Each user instantiates its own Client
// (separate processes; can't share the in-memory bucket). Cross-
// process budget contention degrades to "both retry on 429 with
// jitter" — Onyx evidence + agent_session backfill experience
// suggests this is fine in practice. The strictly-additive jitter
// (never subtract from a 429 wait window) ensures we don't violate
// Notion's stated backoff and re-trigger the limit.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/ratelimit"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// notionAPIVersion is the Notion-Version header value. The 2026 API
// separates database containers, their data sources, and views. Keep this
// pinned so wire-shape changes are deliberate and fixture-tested.
const notionAPIVersion = "2026-03-11"

// notionAPIBase is the canonical API root. Constant — Notion runs
// one cloud, no on-prem variants today.
const notionAPIBase = "https://api.notion.com/v1"

// DefaultRate is the sustained req/s budget. Notion documents this
// as 3 average; bursts above are tolerated but trip 429 quickly.
// Match the documented value to keep the client polite. The bucket
// burst is set equal to the rate so a brief idle period grants
// enough slack to absorb a small concurrent fan-out without
// stalling.
const DefaultRate = 3.0

// DefaultBurst is the bucket size — how many tokens accumulate
// during quiet periods. 3 matches DefaultRate so an idle second
// builds enough headroom to fire one tick of work-pool calls
// without immediately blocking on Wait.
const DefaultBurst = 3

// DefaultMaxRetries caps the number of retries on 429/5xx for a
// single API call. Beyond this we surface the error to the caller —
// upstream is sustained-broken at that point and we'd rather fail
// loud than hold the worker indefinitely.
const DefaultMaxRetries = 5

// DefaultHTTPTimeout is the per-attempt HTTP timeout. Notion
// individual calls are typically sub-second but block-children
// pagination on dense pages can stretch; 60s is generous without
// being a footgun.
const DefaultHTTPTimeout = 60 * time.Second

// Client wraps the Notion REST API with rate-limiting and retry.
// Goroutine-safe — concurrent calls share the same token bucket.
type Client struct {
	baseURL    string
	token      string
	limiter    *ratelimit.Bucket
	httpClient *http.Client
	maxRetries int

	// rng is used only for jitter computation; isolated from the
	// global rng to avoid contention. math/rand/v2's *rand.Rand is
	// NOT goroutine-safe, so we do all jitter sampling under nothing
	// but math/rand/v2 package-level functions (which are
	// goroutine-safe).
	//
	// (Field reserved for future test-injection of a seeded source.)
	_ struct{}
}

// Config configures Client. All fields optional; see field comments
// for defaults.
type Config struct {
	// Token is the Notion integration bearer (`ntn_…` for v2 or
	// `secret_…` for v1). Required.
	Token string
	// BaseURL overrides the API root. Empty string uses
	// notionAPIBase. Override only for tests against a fake server.
	BaseURL string
	// Rate is the sustained tokens-per-second. Zero uses
	// DefaultRate (3.0). Test-only knob; a deployment should leave it at
	// default.
	Rate float64
	// Burst is the token-bucket size. Zero uses DefaultBurst (3).
	Burst int
	// MaxRetries caps retry attempts per call. Zero uses
	// DefaultMaxRetries (5).
	MaxRetries int
	// HTTPClient overrides the underlying *http.Client. Zero-value
	// builds a 60s-timeout default. Override for tests.
	HTTPClient *http.Client
}

// NewClient builds a Client from cfg. Returns an error if Token is
// empty — that's a config bug worth surfacing loud rather than
// failing every call with 401.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("notion: empty Token")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = notionAPIBase
	}
	if cfg.Rate == 0 {
		cfg.Rate = DefaultRate
	}
	if cfg.Burst == 0 {
		cfg.Burst = DefaultBurst
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = safehttp.NewClient(safehttp.Options{Timeout: DefaultHTTPTimeout})
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		token:      cfg.Token,
		limiter:    ratelimit.New(rate.Limit(cfg.Rate), cfg.Burst),
		httpClient: cfg.HTTPClient,
		maxRetries: cfg.MaxRetries,
	}, nil
}

// HTTPError carries the non-2xx response status + body so callers can branch
// on status classes. Notion deliberately uses 404 for both missing objects
// and objects the connection cannot access; callers must not infer which.
type HTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *HTTPError) Error() string {
	preview := e.Body
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	return fmt.Sprintf("notion http %d at %s: %s", e.StatusCode, e.URL, preview)
}

// IsNotFound reports whether err is an HTTPError with a 404 status. In the
// Notion API this means "missing or inaccessible", not necessarily deleted.
func IsNotFound(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusNotFound
}

// IsForbidden reports whether err is an HTTPError with a 403 status.
func IsForbidden(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusForbidden
}

// Do executes a single API request with rate-limiting + retry.
// Caller passes path (e.g. "search" or "blocks/{id}/children"),
// method, and JSON-encodable body (nil for GET). Response is
// JSON-decoded into out (which must be a pointer or nil to ignore
// the body).
//
// Concurrency: safe to call from many goroutines; they share the
// same token bucket. Each call independently honors Retry-After on
// 429, so a thundering herd of concurrent callers all hitting 429
// at once stagger their retries via jitter rather than walking in
// lockstep.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	url := c.baseURL + "/" + strings.TrimLeft(path, "/")
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("notion: marshal body: %w", err)
		}
	}

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// Rate-limit before EVERY attempt (including retries) so
		// retry-loops don't bypass the bucket. limiter.Wait is the
		// load-bearing call here; if ctx cancels mid-wait we surface
		// it immediately.
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("notion: rate-limit wait: %w", err)
		}

		req, err := c.buildRequest(ctx, method, url, bodyBytes)
		if err != nil {
			return err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network errors retry as transient. ctx cancel
			// surfaces via the next limiter.Wait or build call.
			if attempt < c.maxRetries {
				if sleepErr := sleepWithCtx(ctx, expBackoff(attempt)); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			return fmt.Errorf("notion: http: %w", err)
		}

		// Cap reads: a misbehaving Notion response shouldn't OOM
		// us. 16 MiB matches the session feed client's defensive bound;
		// real Notion responses are at most a few hundred KB.
		const maxBody = 16 << 20
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if readErr != nil {
			if attempt < c.maxRetries {
				if sleepErr := sleepWithCtx(ctx, expBackoff(attempt)); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			return fmt.Errorf("notion: read body: %w", readErr)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= c.maxRetries {
				return &HTTPError{StatusCode: resp.StatusCode, URL: url, Body: string(respBody)}
			}
			wait := retryAfterWithJitter(resp.Header.Get("Retry-After"), attempt)
			if err := sleepWithCtx(ctx, wait); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 500 && resp.StatusCode <= 599:
			if attempt >= c.maxRetries {
				return &HTTPError{StatusCode: resp.StatusCode, URL: url, Body: string(respBody)}
			}
			if err := sleepWithCtx(ctx, expBackoff(attempt)); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 400:
			// Non-retryable 4xx (401, 403, 404, etc.). Fail fast.
			return &HTTPError{StatusCode: resp.StatusCode, URL: url, Body: string(respBody)}
		}

		// 2xx. Decode into out if requested.
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("notion: decode body (status=%d, url=%s): %w",
					resp.StatusCode, url, err)
			}
		}
		return nil
	}
	// Unreachable: the loop returns or continues on every iteration.
	return errors.New("notion: retry loop fell through")
}

// buildRequest constructs the http.Request with auth + version
// headers. Pulled out for clarity; tested implicitly via Do.
func (c *Client) buildRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("notion: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionAPIVersion)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// retryAfterWithJitter parses Notion's Retry-After header and adds
// a strictly-positive jitter so multiple processes / goroutines
// that all hit 429 at the same instant don't thundering-herd the
// retry. Jitter is 0..20% of the wait window, ALWAYS additive —
// subtracting from a 429 wait would violate Notion's stated backoff
// contract and reliably re-trigger the limit (per design doc
// § Rate limiting and retry).
//
// If Retry-After is missing or unparsable, falls back to
// expBackoff(attempt). attempt is 0-based.
func retryAfterWithJitter(header string, attempt int) time.Duration {
	if header == "" {
		return expBackoff(attempt)
	}
	// Notion sends decimal seconds (e.g. "2.5"); HTTP also
	// permits an HTTP-date but Notion doesn't use that form.
	secs, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil || secs < 0 {
		return expBackoff(attempt)
	}
	base := time.Duration(secs * float64(time.Second))
	// Strictly-additive jitter: random(0, 0.2 * base).
	jitter := time.Duration(rand.Float64() * 0.2 * float64(base))
	return base + jitter
}

// expBackoff returns 1s, 2s, 4s, 8s, 16s, ... for attempt 0, 1, 2, 3, 4, ...
// Capped at 30s so a long-stuck retry doesn't pin a worker. attempt
// is 0-based.
func expBackoff(attempt int) time.Duration {
	const cap = 30 * time.Second
	d := time.Second << attempt
	if d <= 0 || d > cap {
		// Overflow guard for very large attempts (shouldn't reach
		// here given DefaultMaxRetries=5, but cheap to be defensive).
		return cap
	}
	return d
}

// sleepWithCtx blocks for d or until ctx is cancelled, whichever
// happens first. Returns ctx.Err() on cancel.
func sleepWithCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
