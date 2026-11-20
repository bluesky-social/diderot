// Package tavily is the diderot-side client for Tavily's web search
// and extract endpoints (https://api.tavily.com). It backs the
// model-facing web_search / web_fetch tools in internal/ask: the ask
// loop reaches the open internet through here when a question's
// answer lives outside diderot's own corpus (upstream docs, current
// versions, public references).
//
// Thin wrapper around Tavily's REST surface, modeled on
// internal/connector/sessionfeed/client.go: bearer-auth http.Client plus
// a typed HTTPError so callers can branch on status. Two calls:
//
//   - Search: POST /search — ranked web results for a query.
//   - Extract: POST /extract — full parsed content of one or more URLs.
package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
)

// DefaultBaseURL is Tavily's public API host. Overridable (config /
// tests) via NewClient's baseURL argument.
const DefaultBaseURL = "https://api.tavily.com"

// Client is a thin wrapper around Tavily's HTTP API. Auth is a
// bearer key (tvly-…). Goroutine-safe.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a client. baseURL empty falls back to
// DefaultBaseURL. apiKey is the tvly-… bearer. httpClient is
// optional (default 30s timeout — a web extract of a heavy page is
// slower than a LAN read, but we still want to fail rather than hang
// an ask turn).
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second})
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// SearchParams configures a POST /search call. Query is required;
// the rest are optional and omitted from the request body when zero.
type SearchParams struct {
	Query          string
	MaxResults     int      // 0 = Tavily default (5); capped to 20 server-side
	Topic          string   // "" = "general"; also "news", "finance"
	SearchDepth    string   // "" = "basic"; also "advanced", "fast", "ultra-fast"
	TimeRange      string   // "" = none; "day"|"week"|"month"|"year"
	IncludeDomains []string // "" = no domain filter
	ExcludeDomains []string
}

// SearchResult is the trimmed /search response — only the fields the
// ask tool layer surfaces to the model. Tavily returns more
// (images, favicons, usage, request_id) that we deliberately drop to
// keep the model-facing payload compact.
type SearchResult struct {
	Query   string      `json:"query"`
	Results []SearchHit `json:"results"`
}

// SearchHit is one ranked web result.
type SearchHit struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// searchRequest is the wire body for POST /search. json tags match
// Tavily's field names verbatim; omitempty keeps optional fields off
// the wire when unset so we take Tavily's defaults.
type searchRequest struct {
	Query          string   `json:"query"`
	MaxResults     int      `json:"max_results,omitempty"`
	Topic          string   `json:"topic,omitempty"`
	SearchDepth    string   `json:"search_depth,omitempty"`
	TimeRange      string   `json:"time_range,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
}

// Search runs a web search. Returns the trimmed result set; a
// zero-result query is a normal (non-error) empty Results slice.
func (c *Client) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	if strings.TrimSpace(p.Query) == "" {
		return nil, errors.New("tavily search: empty query")
	}
	// SearchParams and searchRequest have identical fields (only json
	// tags differ), so a direct struct conversion is exact.
	body := searchRequest(p)
	var out SearchResult
	if err := c.postJSON(ctx, "/search", body, &out); err != nil {
		return nil, fmt.Errorf("tavily search: %w", err)
	}
	return &out, nil
}

// ExtractResult is the trimmed /extract response. Results holds the
// successfully-fetched pages; Failed holds per-URL errors (a URL
// that 404'd or timed out shows up here, not as a call-level error).
type ExtractResult struct {
	Results []ExtractHit    `json:"results"`
	Failed  []ExtractFailed `json:"failed_results"`
}

// ExtractHit is one successfully-fetched page. RawContent is the
// parsed page body (markdown, per our request format).
type ExtractHit struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// ExtractFailed is one URL Tavily couldn't fetch, with the reason.
type ExtractFailed struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// extractRequest is the wire body for POST /extract. Format is
// pinned to markdown; Query (optional) reranks the extracted chunks
// by relevance when the caller supplies the user's intent.
type extractRequest struct {
	URLs   []string `json:"urls"`
	Query  string   `json:"query,omitempty"`
	Format string   `json:"format"`
}

// Extract fetches the full parsed content of one or more URLs. query
// is optional (reranks chunks by relevance). A URL that fails to
// fetch lands in the returned ExtractResult.Failed, not as an error —
// only a call-level failure (auth, network, non-2xx) returns error.
func (c *Client) Extract(ctx context.Context, urls []string, query string) (*ExtractResult, error) {
	if len(urls) == 0 {
		return nil, errors.New("tavily extract: no urls")
	}
	body := extractRequest{URLs: urls, Query: query, Format: "markdown"}
	var out ExtractResult
	if err := c.postJSON(ctx, "/extract", body, &out); err != nil {
		return nil, fmt.Errorf("tavily extract: %w", err)
	}
	return &out, nil
}

// postJSON is the shared bearer-auth POST + JSON round-trip. Surfaces
// non-2xx as *HTTPError so callers can branch on status codes without
// string-matching. Body cap is 16 MiB — a defensive bound on a
// misbehaving upstream; a single extracted page is at most a few
// hundred KB.
func (c *Client) postJSON(ctx context.Context, path string, reqBody, dst any) error {
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBody = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        c.baseURL + path,
			Body:       string(body),
		}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

// HTTPError carries the non-2xx status + body so callers can branch
// on specific codes (401 = bad key / config bug, 429 = rate limit,
// 5xx = transient).
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
	return fmt.Sprintf("tavily http %d at %s: %s", e.StatusCode, e.URL, preview)
}
