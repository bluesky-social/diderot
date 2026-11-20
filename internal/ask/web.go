package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/bluesky-social/diderot/internal/tavily"
)

// =====================================================================
// web_search / web_fetch: open-internet tools
// =====================================================================
//
// These two mirror the corpus pair (retrieve + expand) but point at
// the open web via Tavily instead of diderot's own event store.
// web_search ranks results for a query; web_fetch pulls the full
// parsed content of one URL a search surfaced. They carry NO scope
// gate — unlike retrieve/expand they touch no events rows, so there's
// no events.scope to check. The boundary that matters for them is
// egress (the query text leaves the building); that's an administrator's
// decision (the Tavily connector enabled ⇒ tools offered), not a
// per-audience check.

// WebClient is the subset of *tavily.Client the web tools need.
// Narrowed to an interface so tests can stub it without an HTTP
// round-trip.
type WebClient interface {
	Search(ctx context.Context, p tavily.SearchParams) (*tavily.SearchResult, error)
	Extract(ctx context.Context, urls []string, query string) (*tavily.ExtractResult, error)
}

// webSearchMaxResults caps the model-requested max_results. Tavily
// clamps to 20 server-side too, but we enforce it here so the
// model-facing schema and behavior are self-consistent.
const webSearchMaxResults = 20

// -------------------------------------------------------------------
// web_search
// -------------------------------------------------------------------

// webSearchSchema. Kept compact — every property costs prompt-cache
// tokens for the session lifetime. Mirrors the subset of Tavily
// params worth exposing to the model.
var webSearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "What to search for, in natural language, with the distinctive terms."
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "maximum": 20,
      "default": 5,
      "description": "How many results to return."
    },
    "topic": {
      "type": "string",
      "enum": ["general", "news", "finance"],
      "description": "'general' for most questions, 'news' for current events, 'finance' for markets."
    },
    "time_range": {
      "type": "string",
      "enum": ["day", "week", "month", "year"],
      "description": "Only results published within this period before today. Use it for recent events; for anything that may be older, leave it out."
    },
    "include_domains": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Only results from these domains, such as [\"docs.python.org\"]. Empty searches everywhere."
    },
    "exclude_domains": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Leave out results from these domains."
    }
  },
  "required": ["query"]
}`)

// webSearchToolDescription is the model-facing description of `web_search`.
// Each run fills OrgNamePlaceholder.
const webSearchToolDescription = "Search the public web. Use it for what lies outside " + OrgNamePlaceholder + ": a project's " +
	"documentation or current version, a vendor's limits or pricing, a public specification, the news. " +
	"It knows nothing of " + OrgNamePlaceholder + "'s internal work, which the corpus tools cover. " +
	"Returns results with a title, a url and a snippet; use web_fetch to read a page when the snippet is not enough."

// webSearchInput mirrors the model's JSON. max_results is json.Number
// so a string-shaped int ("5") round-trips — the same lesson as
// retrieve's top_k and expand's event_id.
type webSearchInput struct {
	Query          string      `json:"query"`
	MaxResults     json.Number `json:"max_results,omitempty"`
	Topic          string      `json:"topic,omitempty"`
	TimeRange      string      `json:"time_range,omitempty"`
	IncludeDomains []string    `json:"include_domains,omitempty"`
	ExcludeDomains []string    `json:"exclude_domains,omitempty"`
}

// webSearchOutputHit is one model-facing web result.
type webSearchOutputHit struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// webSearchOutput wraps the hits so we can add non-hit fields later
// without breaking the model.
type webSearchOutput struct {
	Results []webSearchOutputHit `json:"results"`
}

func webSearchStatusSources(output json.RawMessage) []StatusSource {
	var out webSearchOutput
	if json.Unmarshal(output, &out) != nil {
		return nil
	}
	sources := make([]StatusSource, 0, len(out.Results))
	for _, result := range out.Results {
		if result.URL != "" {
			sources = append(sources, StatusSource{URL: result.URL, Label: result.Title})
		}
	}
	return sources
}

func webSearchStatusOutput(output json.RawMessage) string {
	var out webSearchOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	return formatStatusCount(len(out.Results), "web result", "web results")
}

func webSearchStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "globe_with_meridians", Verb: "searching the web"}
	var probe struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || probe.Query == "" {
		return sl
	}
	sl.Detail = truncateStatusDetail(probe.Query)
	return sl
}

func runWebSearch(ctx context.Context, client WebClient, in webSearchInput) (webSearchOutput, error) {
	if client == nil {
		return webSearchOutput{}, errors.New("web_search tool: no client configured")
	}
	if strings.TrimSpace(in.Query) == "" {
		return webSearchOutput{}, errors.New("query required")
	}
	maxResults := 0
	if in.MaxResults != "" {
		n, err := in.MaxResults.Int64()
		if err != nil {
			return webSearchOutput{}, fmt.Errorf("max_results: %w", err)
		}
		maxResults = min(max(int(n), 0), webSearchMaxResults)
	}

	// Coerce an out-of-enum topic to "" (Tavily's 'general' default)
	// rather than letting the API 400. Models sometimes ignore schema
	// enums (topic="weather", for example), and a wasted
	// error round-trip is strictly worse than a general-topic search.
	topic := in.Topic
	switch topic {
	case "", "general", "news", "finance":
	default:
		topic = ""
	}

	res, err := client.Search(ctx, tavily.SearchParams{
		Query:          in.Query,
		MaxResults:     maxResults,
		Topic:          topic,
		TimeRange:      in.TimeRange,
		IncludeDomains: in.IncludeDomains,
		ExcludeDomains: in.ExcludeDomains,
	})
	if err != nil {
		// Real Tavily failure (auth, 5xx, network). Propagate up to
		// the loop, which feeds it back as a tool_result error so the
		// model can back off or retry — the turn continues.
		return webSearchOutput{}, fmt.Errorf("web_search: %w", err)
	}
	if res == nil {
		return webSearchOutput{}, fmt.Errorf("web_search: client returned nil result: %w", ErrToolInvariant)
	}
	return webSearchOutputFromResult(res), nil
}

func webSearchOutputFromResult(res *tavily.SearchResult) webSearchOutput {
	out := webSearchOutput{Results: make([]webSearchOutputHit, 0, len(res.Results))}
	for _, h := range res.Results {
		out.Results = append(out.Results, webSearchOutputHit{
			Title:   h.Title,
			URL:     h.URL,
			Content: h.Content,
			Score:   h.Score,
		})
	}
	return out
}

// webClient reads current, which is nil in the static tool catalog.
func webClient(current func() WebClient) WebClient {
	if current == nil {
		return nil
	}
	return current()
}

// newWebTool makes a native web tool optional: a run offers it while current
// returns a client.
func newWebTool(native tool.InvokableTool, current func() WebClient) *optionalTool {
	return &optionalTool{InvokableTool: native, info: native.Info,
		available: func() bool { return webClient(current) != nil }}
}

// NewEinoWebSearchTool builds `web_search` over the client current returns,
// read as each run starts and again on each call; nil means web search is
// off. Its description names the organization through OrgNamePlaceholder.
func NewEinoWebSearchTool(current func() WebClient) RegisteredTool {
	js, err := parseToolJSONSchema(webSearchSchema)
	if err != nil {
		panic(fmt.Sprintf("web_search: parse model schema: %v", err))
	}
	info := &schema.ToolInfo{
		Name:        "web_search",
		Desc:        webSearchToolDescription,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
	native := utils.NewTool(info, func(ctx context.Context, in webSearchInput) (webSearchOutput, error) {
		return runWebSearch(ctx, webClient(current), in)
	})
	return RegisteredTool{
		Tool: newWebTool(native, current),
		Presentation: ToolPresentation{
			Status:        webSearchStatus,
			StatusSources: webSearchStatusSources,
			StatusOutput:  webSearchStatusOutput,
		},
	}
}

// -------------------------------------------------------------------
// web_fetch
// -------------------------------------------------------------------

// webFetchSchema. Single required url plus an optional query used to
// rerank the extracted chunks by relevance.
var webFetchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL to read, usually from a web_search result."
    },
    "query": {
      "type": "string",
      "description": "What you are looking for on the page. With it, only the passages most relevant to it come back; leave it empty to get the page from the top."
    }
  },
  "required": ["url"]
}`)

// webFetchToolDescription is the model-facing description of
// `web_fetch`.
const webFetchToolDescription = "Read one web page as Markdown. Use it on a web_search result when the snippet is not " +
	"enough. With `query`, it returns only the passages of the page most relevant to the query, which is " +
	"not the whole page: do not conclude that something is absent from a page you read that way. Without " +
	"`query`, it returns the page from the top, with `truncated` set when a long page was cut. When the " +
	"page cannot be read (a dead link, a block, a timeout), `ok` is false and `note` says why: use the " +
	"search snippet and do not retry. Pages are large, so read the ones that matter."

// webFetchInput is what the model passes.
type webFetchInput struct {
	URL   string `json:"url"`
	Query string `json:"query,omitempty"`
}

// webFetchOutput is the model-facing shape. On success: url +
// content (+ truncated flag if capped). On a failed fetch: url +
// ok=false + note. The recover-not-abort convention matches expand's
// "not supported" envelope — a dead link is model-recoverable, not a
// hard tool error.
type webFetchOutput struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Note      string `json:"note,omitempty"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func webFetchStatusSources(output json.RawMessage) []StatusSource {
	var out webFetchOutput
	if json.Unmarshal(output, &out) != nil || !out.OK || out.URL == "" {
		return nil
	}
	return []StatusSource{{URL: out.URL}}
}

func webFetchStatusOutput(output json.RawMessage) string {
	var out webFetchOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	if !out.OK {
		if out.Note != "" {
			return "Could not read the page: " + truncateStatusDetail(out.Note)
		}
		return "Could not read the page"
	}
	if out.Truncated {
		return "Loaded page content (truncated)"
	}
	return "Loaded page content"
}

func webFetchStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "globe_with_meridians", Verb: "fetching"}
	var probe struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || probe.URL == "" {
		return sl
	}
	sl.Detail = truncateStatusDetail(probe.URL)
	return sl
}

func runWebFetch(ctx context.Context, client WebClient, in webFetchInput) (webFetchOutput, error) {
	if client == nil {
		return webFetchOutput{}, errors.New("web_fetch tool: no client configured")
	}
	if strings.TrimSpace(in.URL) == "" {
		return webFetchOutput{}, errors.New("url required")
	}

	res, err := client.Extract(ctx, []string{in.URL}, in.Query)
	if err != nil {
		// Call-level failure (auth, 5xx, network) — propagate so the
		// loop surfaces it as a tool_result error.
		return webFetchOutput{}, fmt.Errorf("web_fetch: %w", err)
	}
	if res == nil {
		return webFetchOutput{}, fmt.Errorf("web_fetch: client returned nil result: %w", ErrToolInvariant)
	}

	// A per-URL fetch failure comes back in failed_results, not as a
	// call error. Translate it to the recover envelope so the model
	// backs off to the search snippet rather than aborting.
	if len(res.Results) == 0 {
		note := "fetch returned no content"
		for _, f := range res.Failed {
			if f.URL == in.URL && f.Error != "" {
				note = f.Error
				break
			}
		}
		return webFetchOutput{
			URL:  in.URL,
			OK:   false,
			Note: note,
		}, nil
	}

	hit := res.Results[0]
	if !utf8.ValidString(hit.RawContent) {
		return webFetchOutput{}, fmt.Errorf("web_fetch %q: source returned non-UTF-8 content", in.URL)
	}

	// Reuse the expand budget: cap the model-facing content and walk
	// back to a rune boundary so we never ship a partial codepoint.
	content := hit.RawContent
	truncated := false
	if len(content) > ExpandContentBudget {
		cap := ExpandContentBudget
		for cap > 0 && !utf8.RuneStart(content[cap]) {
			cap--
		}
		content = content[:cap]
		truncated = true
	}

	return webFetchOutput{
		URL:       hit.URL,
		OK:        true,
		Content:   content,
		Truncated: truncated,
	}, nil
}

// NewEinoWebFetchTool builds `web_fetch` over the client current returns, as
// NewEinoWebSearchTool does.
func NewEinoWebFetchTool(current func() WebClient) RegisteredTool {
	js, err := parseToolJSONSchema(webFetchSchema)
	if err != nil {
		panic(fmt.Sprintf("web_fetch: parse model schema: %v", err))
	}
	info := &schema.ToolInfo{
		Name:        "web_fetch",
		Desc:        webFetchToolDescription,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
	native := utils.NewTool(info, func(ctx context.Context, in webFetchInput) (webFetchOutput, error) {
		return runWebFetch(ctx, webClient(current), in)
	})
	return RegisteredTool{
		Tool: newWebTool(native, current),
		Presentation: ToolPresentation{
			Status:        webFetchStatus,
			StatusSources: webFetchStatusSources,
			StatusOutput:  webFetchStatusOutput,
		},
	}
}
