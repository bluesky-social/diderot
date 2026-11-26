// Package websearch defines the data shared by Ask and its web providers.
package websearch

// SearchParams describes a web search and its optional filters.
type SearchParams struct {
	Query          string
	MaxResults     int      // 0 = provider default
	Topic          string   // "" = "general"; also "news", "finance"
	SearchDepth    string   // "" = "basic"; also "advanced", "fast", "ultra-fast"
	TimeRange      string   // "" = none; "day"|"week"|"month"|"year"
	IncludeDomains []string // "" = no domain filter
	ExcludeDomains []string
}

// SearchResult holds the ranked web results returned to Ask.
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

// ExtractFailed is one URL the provider could not fetch.
type ExtractFailed struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}
