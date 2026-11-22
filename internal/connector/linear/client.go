// Package linear is the diderot connector for Linear. It provides a
// GraphQL client, incremental sync, visibility reconciliation, and
// backfill plumbing for Linear issues as diderot events.
//
// Client is the thin auth + rate-limiting layer. It enforces an
// in-process token bucket shared across all callers (incremental,
// visibility sweep, reconcile, backfill). Linear returns complexity
// cost in response headers; the client exposes those for pacing.
//
// TokenProvider owns auth token management. The static provider holds
// the connector's personal API key.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/ratelimit"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

const (
	linearAPIURL = "https://api.linear.app/graphql"

	// defaultRatePerSecond matches Linear's 1,500 req/hr limit expressed
	// as tokens per second. 1500/3600 ≈ 0.417 req/s; we pace conservatively.
	defaultRatePerSecond = 0.4
	// defaultBurst allows a small burst for the page-fetching fan-out while
	// staying well within the hourly budget.
	defaultBurst = 3

	// defaultHTTPTimeout is the per-request timeout. Linear responses are
	// typically fast but complex nested queries can take longer.
	defaultHTTPTimeout = 30 * time.Second

	// maxTransientRetries bounds retries on transient Linear failures
	// (HTTP 429, HTTP 5xx, and transport errors like timeouts). The
	// visibility sweep walks the entire issue corpus (hundreds of
	// sequential requests), so without retries a single transient 503 on
	// any one page aborts the whole multi-minute sweep and leaves it
	// stalled. Retries go through the same rate limiter, so they stay
	// within Linear's budget.
	maxTransientRetries = 3

	// baseRetryBackoff is the first sleep between transient retries; it
	// doubles each attempt (0.5s, 1s, 2s).
	baseRetryBackoff = 500 * time.Millisecond
)

// TokenProvider owns acquisition and caching of the Authorization header
// value. The static personal-API-key implementation is the only provider.
//
// Contract:
//   - Authorization() returns the current header value (e.g. "lin_api_...").
//     It may block while refreshing. Concurrent callers are serialized by
//     the provider — callers must not take their own locks.
//   - Invalidate() signals that the last-returned token was rejected (401).
//     The next Authorization() call must fetch a fresh token.
type TokenProvider interface {
	// Authorization returns the current Authorization header value.
	Authorization(ctx context.Context) (string, error)
	// Invalidate marks the current token as rejected so the next
	// Authorization() call fetches a fresh one.
	Invalidate()
}

// StaticTokenProvider implements TokenProvider for personal API keys.
// Acquisition is reading config; refresh is a no-op (personal keys don't
// expire via a 401 cycle; operators rotate them by updating the env var).
// This provider is also the test fixture.
type StaticTokenProvider struct {
	token string
}

// NewStaticTokenProvider returns a provider that always returns token.
// token should be the bare API key (e.g. "lin_api_xxxx...").
func NewStaticTokenProvider(token string) *StaticTokenProvider {
	return &StaticTokenProvider{token: token}
}

func (p *StaticTokenProvider) Authorization(_ context.Context) (string, error) {
	if p.token == "" {
		return "", fmt.Errorf("linear: no token configured")
	}
	return p.token, nil
}

func (p *StaticTokenProvider) Invalidate() {
	// Personal keys can't be refreshed; Invalidate is a no-op. A 401
	// from a static key means the key is wrong or revoked — the caller
	// logs the error and the next tick will fail the same way, surfacing
	// the problem without busy-looping.
}

// Client wraps the Linear GraphQL API with rate-limiting and token auth.
// Goroutine-safe. All calls share one token bucket and one TokenProvider.
type Client struct {
	provider   TokenProvider
	httpClient *http.Client
	limiter    *ratelimit.Bucket

	// maxRetries and retryBackoff control transient-failure retries in Do.
	// Fields (not consts) so tests can shrink the backoff; production uses
	// the defaults set in NewClient.
	maxRetries   int
	retryBackoff time.Duration
}

// NewClient constructs a Client using provider for auth.
func NewClient(provider TokenProvider) *Client {
	return &Client{
		provider:     provider,
		httpClient:   newHTTPClient(),
		limiter:      ratelimit.New(defaultRatePerSecond, defaultBurst),
		maxRetries:   maxTransientRetries,
		retryBackoff: baseRetryBackoff,
	}
}

// newHTTPClient returns the HTTP client for requests to Linear, the API's
// and the token endpoint's alike. It fails on any redirect: neither
// endpoint should ever redirect, so one is a sign of misconfiguration or a
// SSRF-style attack on the credentials.
func newHTTPClient() *http.Client {
	return safehttp.NewClient(safehttp.Options{Timeout: defaultHTTPTimeout, FailOnRedirect: true})
}

// ComplexityInfo carries the complexity budget from Linear's response
// headers. Linear returns these on every GraphQL response.
type ComplexityInfo struct {
	// RequestedTokens is the complexity cost of this specific request.
	RequestedTokens int
	// AllowedTokens is the per-period budget (typically 250,000/hr).
	AllowedTokens int
	// ResetAt is when the complexity window resets. Zero if not parsed.
	ResetAt time.Time
}

// graphqlRequest is the JSON body for a GraphQL request to Linear.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlResponse is the envelope Linear returns for all GraphQL calls.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Do executes a GraphQL query against Linear's API. It:
//  1. Waits for rate-limiter token.
//  2. Fetches an Authorization header from the TokenProvider.
//  3. Sends the request; on 401, calls Invalidate() + retries once.
//  4. Returns the raw Data payload and any complexity info from headers.
//
// Retries: 401 is retried exactly once after Invalidate() (token refresh).
// Transient failures — HTTP 429, HTTP 5xx, and transport errors (timeout,
// connection reset) — are retried up to maxRetries with exponential
// backoff, all paced through the same rate limiter. Returns an error for
// other non-2xx responses, for GraphQL-level errors in the response body,
// and after transient retries are exhausted.
func (c *Client) Do(ctx context.Context, query string, variables map[string]any) (json.RawMessage, ComplexityInfo, error) {
	var (
		authRetried bool
		retries     int
		backoff     = c.retryBackoff
	)
	for {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, ComplexityInfo{}, fmt.Errorf("linear: rate limiter: %w", err)
		}

		auth, err := c.provider.Authorization(ctx)
		if err != nil {
			return nil, ComplexityInfo{}, fmt.Errorf("linear: token provider: %w", err)
		}

		data, ci, status, err := c.doOnce(ctx, auth, query, variables)
		if err != nil {
			// status == 0 means no HTTP response arrived (transport error:
			// timeout, connection reset, DNS) — transient, worth retrying.
			// A non-zero status with an error is a body read/decode or
			// GraphQL-level failure that will not change on retry; surface it.
			if status == 0 && retries < c.maxRetries {
				retries++
				slog.Warn("linear: transient transport error; retrying",
					"attempt", retries, "backoff", backoff, "err", err)
				if werr := sleepCtx(ctx, backoff); werr != nil {
					return nil, ComplexityInfo{}, werr
				}
				backoff *= 2
				continue
			}
			return nil, ComplexityInfo{}, err
		}

		switch {
		case status == http.StatusUnauthorized && !authRetried:
			authRetried = true
			c.provider.Invalidate()
			continue
		case isTransientStatus(status) && retries < c.maxRetries:
			retries++
			slog.Warn("linear: transient HTTP status; retrying",
				"status", status, "attempt", retries, "backoff", backoff)
			if werr := sleepCtx(ctx, backoff); werr != nil {
				return nil, ComplexityInfo{}, werr
			}
			backoff *= 2
			continue
		case status != http.StatusOK:
			return nil, ComplexityInfo{}, fmt.Errorf("linear: HTTP %d", status)
		}

		return data, ci, nil
	}
}

// isTransientStatus reports whether an HTTP status warrants a retry:
// 429 (rate limited) or any 5xx (server-side, typically transient).
func isTransientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// sleepCtx sleeps for d, returning early with ctx.Err() if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// doOnce issues one HTTP request and returns the raw data, complexity
// info, HTTP status, and any transport/encoding error. HTTP errors (4xx,
// 5xx) are returned as status codes, not errors — the caller decides
// whether to retry.
func (c *Client) doOnce(ctx context.Context, auth, query string, variables map[string]any) (json.RawMessage, ComplexityInfo, int, error) {
	reqBody, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, ComplexityInfo{}, 0, fmt.Errorf("linear: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearAPIURL,
		bytes.NewReader(reqBody))
	if err != nil {
		return nil, ComplexityInfo{}, 0, fmt.Errorf("linear: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ComplexityInfo{}, 0, fmt.Errorf("linear: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	ci := parseComplexityHeaders(resp.Header)

	if resp.StatusCode != http.StatusOK {
		return nil, ci, resp.StatusCode, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ci, resp.StatusCode, fmt.Errorf("linear: read body: %w", err)
	}

	var gr graphqlResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, ci, resp.StatusCode, fmt.Errorf("linear: decode response: %w", err)
	}

	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, ci, resp.StatusCode, fmt.Errorf("linear: GraphQL errors: %s",
			strings.Join(msgs, "; "))
	}

	return gr.Data, ci, resp.StatusCode, nil
}

// projectsByNameQuery fetches projects matching a name filter (case-insensitive).
const projectsByNameQuery = `
query ProjectsByName($name: String!) {
  projects(filter: { name: { eqIgnoreCase: $name } }) {
    nodes { id name }
  }
}`

// ResolveProjectID resolves a project name-or-UUID to a Linear project UUID.
// If arg already parses as a UUID (8-4-4-4-12 hex form), it is returned as-is
// without a network call. Otherwise, a GraphQL query fetches projects whose
// name matches arg (case-insensitive exact match). On ambiguity or no match,
// an error is returned listing the candidate names.
func (c *Client) ResolveProjectID(ctx context.Context, arg string) (string, error) {
	if isUUID(arg) {
		return arg, nil
	}

	data, _, err := c.Do(ctx, projectsByNameQuery, map[string]any{"name": arg})
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", arg, err)
	}

	var resp struct {
		Projects struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("resolve project %q: decode response: %w", arg, err)
	}

	switch len(resp.Projects.Nodes) {
	case 0:
		return "", fmt.Errorf("no Linear project found with name %q", arg)
	case 1:
		return resp.Projects.Nodes[0].ID, nil
	default:
		names := make([]string, len(resp.Projects.Nodes))
		for i, n := range resp.Projects.Nodes {
			names[i] = n.Name
		}
		return "", fmt.Errorf("ambiguous project name %q; matches: %s",
			arg, strings.Join(names, ", "))
	}
}

// isUUID reports whether s matches the standard 8-4-4-4-12 hex UUID form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// parseComplexityHeaders reads Linear's X-Complexity-* response headers.
// Missing or unparseable headers yield zero values; callers treat zero
// AllowedTokens as "budget unknown."
func parseComplexityHeaders(h http.Header) ComplexityInfo {
	var ci ComplexityInfo
	if v := h.Get("X-Complexity"); v != "" {
		ci.RequestedTokens, _ = strconv.Atoi(v)
	}
	if v := h.Get("X-Complexity-Budget"); v != "" {
		ci.AllowedTokens, _ = strconv.Atoi(v)
	}
	if v := h.Get("X-Complexity-Reset-At"); v != "" {
		ci.ResetAt, _ = time.Parse(time.RFC3339, v)
	}
	return ci
}
