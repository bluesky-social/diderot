// Package kagi provides access to Kagi's Search and Extract API.
package kagi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
	"github.com/bluesky-social/diderot/internal/websearch"
)

const baseURL = "https://kagi.com/api/v1"

type Client struct {
	key  string
	http *http.Client
}

func NewClient(key string, client *http.Client) *Client {
	if client == nil {
		client = safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second})
	}
	return &Client{key: key, http: client}
}

type searchLens struct {
	Include []string `json:"sites_included,omitempty"`
	Exclude []string `json:"sites_excluded,omitempty"`
	After   string   `json:"time_after,omitempty"`
}

func (c *Client) Search(ctx context.Context, p websearch.SearchParams) (*websearch.SearchResult, error) {
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return nil, errors.New("kagi search: empty query")
	}
	input := struct {
		Query    string      `json:"query"`
		Workflow string      `json:"workflow"`
		Limit    int         `json:"limit"`
		Lens     *searchLens `json:"lens,omitempty"`
	}{Query: query, Workflow: "search", Limit: 5}
	if p.MaxResults > 0 {
		input.Limit = min(p.MaxResults, 20)
	}
	if p.Topic == "news" {
		input.Workflow = "news"
	}
	now := time.Now().UTC()
	var after string
	switch p.TimeRange {
	case "day":
		after = now.AddDate(0, 0, -1).Format(time.DateOnly)
	case "week":
		after = now.AddDate(0, 0, -7).Format(time.DateOnly)
	case "month":
		after = now.AddDate(0, -1, 0).Format(time.DateOnly)
	case "year":
		after = now.AddDate(-1, 0, 0).Format(time.DateOnly)
	}
	if after != "" || len(p.IncludeDomains)+len(p.ExcludeDomains) > 0 {
		input.Lens = &searchLens{Include: p.IncludeDomains, Exclude: p.ExcludeDomains, After: after}
	}

	type hit struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	var out struct {
		Data *struct {
			Search []hit `json:"search"`
			News   []hit `json:"news"`
		} `json:"data"`
	}
	if err := c.post(ctx, "search", input, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, errors.New("kagi search: missing results")
	}
	hits := out.Data.Search
	if input.Workflow == "news" && len(out.Data.News) > 0 {
		hits = out.Data.News
	}
	result := &websearch.SearchResult{Query: query}
	for _, h := range hits {
		if h.URL == "" {
			continue
		}
		result.Results = append(result.Results, websearch.SearchHit{Title: h.Title, URL: h.URL, Content: h.Snippet})
		if len(result.Results) == input.Limit {
			break
		}
	}
	return result, nil
}

// Extract returns Markdown from the top of each page. Kagi's endpoint
// accepts URLs and returns page content without passage ranking.
func (c *Client) Extract(ctx context.Context, urls []string, _ string) (*websearch.ExtractResult, error) {
	if len(urls) == 0 || len(urls) > 10 {
		return nil, errors.New("kagi extract: expected 1 to 10 URLs")
	}
	type page struct {
		URL string `json:"url"`
	}
	input := struct {
		Pages []page `json:"pages"`
	}{}
	result := &websearch.ExtractResult{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			result.Failed = append(result.Failed, websearch.ExtractFailed{URL: raw, Error: "Kagi requires an HTTPS URL without credentials"})
			continue
		}
		input.Pages = append(input.Pages, page{URL: raw})
	}
	if len(input.Pages) == 0 {
		return result, nil
	}
	var out struct {
		Data *[]struct {
			URL      string `json:"url"`
			Markdown string `json:"markdown"`
		} `json:"data"`
	}
	if err := c.post(ctx, "extract", input, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, errors.New("kagi extract: missing results")
	}
	for _, p := range *out.Data {
		if strings.TrimSpace(p.Markdown) == "" {
			result.Failed = append(result.Failed, websearch.ExtractFailed{URL: p.URL, Error: "Kagi could not extract this page"})
			continue
		}
		result.Results = append(result.Results, websearch.ExtractHit{URL: p.URL, RawContent: p.Markdown})
	}
	return result, nil
}

// HTTPError reports the status without exposing upstream response content.
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("kagi returned HTTP %d", e.Code) }

func (c *Client) post(ctx context.Context, endpoint string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	reply, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = reply.Body.Close() }()
	if reply.StatusCode != http.StatusOK {
		return &HTTPError{Code: reply.StatusCode}
	}
	const maxResponse = 16 << 20
	data, err := io.ReadAll(io.LimitReader(reply.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponse || json.Unmarshal(data, output) != nil {
		return fmt.Errorf("kagi %s: invalid or oversized response", endpoint)
	}
	return nil
}
