// Package sourcebot is the diderot-side client for Sourcebot's code
// search REST API (a Sourcebot deployment indexing the organization's
// repositories). It backs the model-facing code_search / code_read tools in
// internal/ask, completing the tool-pair symmetry:
//
//	corpus:   retrieve / expand      (Postgres + Qdrant)
//	web:      web_search / web_fetch (Tavily)
//	code:     code_search / code_read (Sourcebot/Zoekt)
//
// Thin bearer-auth wrapper modeled on internal/tavily. Three calls:
//
//   - Search: POST /api/search — Zoekt query over the org index.
//   - Source: GET /api/source — full file content at a repo+path.
//   - RepoCount: GET /api/repos — how many repositories are indexed, for
//     the connector's Test.
package sourcebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
)

// Client is a thin wrapper around Sourcebot's public HTTP API. Auth
// is a bearer API key (sbk_…). Goroutine-safe.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a client. baseURL is the Sourcebot deployment
// (e.g. https://sourcebot.example.com). httpClient is
// optional (default 30s timeout — searches are milliseconds; the
// margin is for network hiccups, not workload).
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second})
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// SearchParams configures a POST /api/search call. Query uses
// Zoekt/Sourcebot query syntax (repo:, file:, lang:, sym:, regex).
// Matches is required by the API (max match count).
type SearchParams struct {
	Query        string
	Matches      int
	ContextLines int  // lines of context around each match; 0 = none
	Whole        bool // return whole file content instead of chunks
}

// searchRequest is the wire body for POST /api/search.
type searchRequest struct {
	Query        string `json:"query"`
	Matches      int    `json:"matches"`
	ContextLines int    `json:"contextLines,omitempty"`
	Whole        bool   `json:"whole,omitempty"`
}

// SearchResult is the trimmed /api/search response — only what the
// ask tool layer needs. Sourcebot returns much more (per-shard
// stats, byte offsets, match ranges) that we drop.
type SearchResult struct {
	// TotalMatchCount is the estimated total matches in the index;
	// ActualMatchCount is how many came back under Matches.
	TotalMatchCount  int
	ActualMatchCount int
	Files            []FileMatch
}

// FileMatch is one file with matching chunks.
type FileMatch struct {
	Repository     string  // e.g. "github.com/bluesky-social/diderot"
	Path           string  // repo-relative file path
	Language       string  // linguist language name
	WebURL         string  // sourcebot UI deep link (may be relative)
	ExternalWebURL string  // github.com blob URL (may be empty)
	Chunks         []Chunk // matching regions, each with its start line
}

// Chunk is one matching region of a file.
type Chunk struct {
	// StartLine is the 1-based line number of the chunk's first line.
	StartLine int
	Content   string
}

// searchResponse mirrors the wire shape (subset).
type searchResponse struct {
	Stats struct {
		ActualMatchCount int `json:"actualMatchCount"`
		TotalMatchCount  int `json:"totalMatchCount"`
	} `json:"stats"`
	Files []struct {
		FileName struct {
			Text string `json:"text"`
		} `json:"fileName"`
		WebURL         string `json:"webUrl"`
		ExternalWebURL string `json:"externalWebUrl"`
		Repository     string `json:"repository"`
		Language       string `json:"language"`
		Chunks         []struct {
			Content      string `json:"content"`
			ContentStart struct {
				LineNumber int `json:"lineNumber"`
			} `json:"contentStart"`
		} `json:"chunks"`
	} `json:"files"`
}

// Search runs a code search. Zero matches is a normal empty result,
// not an error.
func (c *Client) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	if strings.TrimSpace(p.Query) == "" {
		return nil, errors.New("sourcebot search: empty query")
	}
	if p.Matches <= 0 {
		return nil, errors.New("sourcebot search: matches must be > 0")
	}
	// SearchParams and searchRequest have identical fields (only
	// json tags differ), so a direct struct conversion is exact.
	body, err := json.Marshal(searchRequest(p))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	raw, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("sourcebot search: %w", err)
	}
	var resp searchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("sourcebot search: decode: %w", err)
	}
	out := &SearchResult{
		TotalMatchCount:  resp.Stats.TotalMatchCount,
		ActualMatchCount: resp.Stats.ActualMatchCount,
		Files:            make([]FileMatch, 0, len(resp.Files)),
	}
	for _, f := range resp.Files {
		fm := FileMatch{
			Repository:     f.Repository,
			Path:           f.FileName.Text,
			Language:       f.Language,
			WebURL:         f.WebURL,
			ExternalWebURL: f.ExternalWebURL,
			Chunks:         make([]Chunk, 0, len(f.Chunks)),
		}
		for _, ch := range f.Chunks {
			fm.Chunks = append(fm.Chunks, Chunk{
				StartLine: ch.ContentStart.LineNumber,
				Content:   ch.Content,
			})
		}
		out.Files = append(out.Files, fm)
	}
	return out, nil
}

// SourceFile is the trimmed GET /api/source response: the full
// content of one file plus identity metadata.
type SourceFile struct {
	Repo           string
	Path           string
	Language       string
	WebURL         string // sourcebot UI link
	ExternalWebURL string // github.com blob URL (may be empty)
	Source         string // full file content
}

// sourceResponse mirrors the wire shape (subset).
type sourceResponse struct {
	Source         string `json:"source"`
	Language       string `json:"language"`
	Path           string `json:"path"`
	Repo           string `json:"repo"`
	WebURL         string `json:"webUrl"`
	ExternalWebURL string `json:"externalWebUrl"`
}

// ErrNotFound is returned by Source when the repo or path doesn't
// exist in the index. Callers translate it into a model-recoverable
// envelope rather than a hard tool error.
var ErrNotFound = errors.New("file not found in index")

// Source fetches the full content of one file. repo is the
// sourcebot repository identifier (e.g.
// "github.com/bluesky-social/diderot"); path is repo-relative. ref
// optional (default branch when empty).
func (c *Client) Source(ctx context.Context, repo, path, ref string) (*SourceFile, error) {
	if repo == "" || path == "" {
		return nil, errors.New("sourcebot source: repo and path required")
	}
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("path", path)
	if ref != "" {
		q.Set("ref", ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/source?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	raw, err := c.do(req)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("sourcebot source %s:%s: %w", repo, path, ErrNotFound)
		}
		return nil, fmt.Errorf("sourcebot source: %w", err)
	}
	var resp sourceResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("sourcebot source: decode: %w", err)
	}
	return &SourceFile{
		Repo:           resp.Repo,
		Path:           resp.Path,
		Language:       resp.Language,
		WebURL:         resp.WebURL,
		ExternalWebURL: resp.ExternalWebURL,
		Source:         resp.Source,
	}, nil
}

// RepoCount returns how many repositories the index holds, from the
// X-Total-Count header of GET /api/repos. A Sourcebot without that endpoint
// answers an *HTTPError.
func (c *Client) RepoCount(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/repos?perPage=1", nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sourcebot repos: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return 0, &HTTPError{StatusCode: resp.StatusCode, URL: req.URL.String()}
	}
	count, err := strconv.Atoi(resp.Header.Get("X-Total-Count"))
	if err != nil || count < 0 {
		return 0, errors.New("sourcebot repos: no repository count in the answer")
	}
	return count, nil
}

// do executes the request and returns the body. Non-2xx surfaces as
// *HTTPError. Body cap is 16 MiB — same defensive bound as the
// tavily and session feed clients.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBody = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        req.URL.String(),
			Body:       string(body),
		}
	}
	return body, nil
}

// HTTPError carries the non-2xx status + body so callers can branch
// on codes (401 = bad key, 404 = missing file, 5xx = transient).
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
	return fmt.Sprintf("sourcebot http %d at %s: %s", e.StatusCode, e.URL, preview)
}
