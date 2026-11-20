package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/bluesky-social/diderot/internal/tavily"
)

// stubWebClient is a canned WebClient for the tool tests — no HTTP.
type stubWebClient struct {
	search     *tavily.SearchResult
	searchErr  error
	extract    *tavily.ExtractResult
	extractErr error
	lastSearch tavily.SearchParams
	lastURLs   []string
	lastQuery  string
}

func (s *stubWebClient) Search(_ context.Context, p tavily.SearchParams) (*tavily.SearchResult, error) {
	s.lastSearch = p
	return s.search, s.searchErr
}

func (s *stubWebClient) Extract(_ context.Context, urls []string, query string) (*tavily.ExtractResult, error) {
	s.lastURLs = urls
	s.lastQuery = query
	return s.extract, s.extractErr
}

// TestWebSearchInput_MaxResultsAcceptsBothShapes locks in the
// json.Number lesson: Claude ships numeric args as strings sometimes.
func TestWebSearchInput_MaxResultsAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"int", `{"query":"q","max_results":5}`, 5},
		{"string", `{"query":"q","max_results":"5"}`, 5},
		{"omitted", `{"query":"q"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in webSearchInput
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			var got int64
			if in.MaxResults != "" {
				n, err := in.MaxResults.Int64()
				if err != nil {
					t.Fatalf("Int64: %v", err)
				}
				got = n
			}
			if got != tc.want {
				t.Errorf("MaxResults = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWebSearch_Invoke maps stub results to the output envelope and
// clamps max_results to the cap.
func TestWebSearch_Invoke(t *testing.T) {
	stub := &stubWebClient{
		search: &tavily.SearchResult{
			Query: "q",
			Results: []tavily.SearchHit{
				{Title: "T", URL: "https://ex.com", Content: "snip", Score: 0.9},
			},
		},
	}
	var out webSearchOutput
	err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(stub)), `{"query":"q","max_results":999,"topic":"news"}`, &out)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out.Results) != 1 || out.Results[0].URL != "https://ex.com" {
		t.Fatalf("out = %+v", out)
	}
	if stub.lastSearch.MaxResults != webSearchMaxResults {
		t.Errorf("max_results clamped to %d, want %d", stub.lastSearch.MaxResults, webSearchMaxResults)
	}
	if stub.lastSearch.Topic != "news" {
		t.Errorf("topic = %q, want news", stub.lastSearch.Topic)
	}
}

// TestWebSearch_CoercesInvalidTopic: an out-of-enum topic (models
// sometimes ignore schema enums, as with topic="weather") is
// coerced to "" rather than 400ing against Tavily.
func TestWebSearch_CoercesInvalidTopic(t *testing.T) {
	stub := &stubWebClient{search: &tavily.SearchResult{}}
	if err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(stub)), `{"query":"q","topic":"weather"}`, &webSearchOutput{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if stub.lastSearch.Topic != "" {
		t.Errorf("topic = %q, want coerced to empty", stub.lastSearch.Topic)
	}
	// Valid enum values pass through untouched.
	if err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(stub)), `{"query":"q","topic":"finance"}`, &webSearchOutput{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if stub.lastSearch.Topic != "finance" {
		t.Errorf("topic = %q, want finance", stub.lastSearch.Topic)
	}
}

// TestWebSearch_EmptyQuery rejects a blank query.
func TestWebSearch_EmptyQuery(t *testing.T) {
	var out webSearchOutput
	err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(&stubWebClient{})), `{"query":"  "}`, &out)
	if err == nil {
		t.Fatal("expected error on empty query")
	}
}

// TestWebSearch_ClientErrorPropagates: a real Tavily failure returns
// a Go error so the loop feeds it back as a tool_result error.
func TestWebSearch_ClientErrorPropagates(t *testing.T) {
	var out webSearchOutput
	err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(&stubWebClient{searchErr: errors.New("boom")})), `{"query":"q"}`, &out)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

// TestWebFetch_Success maps a successful extract to ok=true + content.
func TestWebFetch_Success(t *testing.T) {
	stub := &stubWebClient{
		extract: &tavily.ExtractResult{
			Results: []tavily.ExtractHit{{URL: "https://ok.com", RawContent: "# Page body"}},
		},
	}
	var out webFetchOutput
	err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)), `{"url":"https://ok.com","query":"intent"}`, &out)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.OK || out.Content != "# Page body" || out.Truncated {
		t.Errorf("out = %+v", out)
	}
	if stub.lastQuery != "intent" || len(stub.lastURLs) != 1 || stub.lastURLs[0] != "https://ok.com" {
		t.Errorf("extract called with urls=%v query=%q", stub.lastURLs, stub.lastQuery)
	}
}

// TestWebFetch_FailedResultRecovers: a per-URL fetch failure comes
// back as ok=false + note, NOT a hard tool error, so the model can
// back off to the search snippet.
func TestWebFetch_FailedResultRecovers(t *testing.T) {
	stub := &stubWebClient{
		extract: &tavily.ExtractResult{
			Failed: []tavily.ExtractFailed{{URL: "https://bad.com", Error: "404 not found"}},
		},
	}
	var out webFetchOutput
	err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)), `{"url":"https://bad.com"}`, &out)
	if err != nil {
		t.Fatalf("Invoke should recover, got err: %v", err)
	}
	if out.OK || out.Note != "404 not found" {
		t.Errorf("out = %+v, want ok=false note=404", out)
	}
}

// TestWebFetch_TruncatesToBudget caps oversized content at the
// expand budget on a rune boundary.
func TestWebFetch_TruncatesToBudget(t *testing.T) {
	big := strings.Repeat("a", ExpandContentBudget+100)
	stub := &stubWebClient{
		extract: &tavily.ExtractResult{
			Results: []tavily.ExtractHit{{URL: "https://ok.com", RawContent: big}},
		},
	}
	var out webFetchOutput
	err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)), `{"url":"https://ok.com"}`, &out)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Truncated || len(out.Content) > ExpandContentBudget {
		t.Errorf("content len = %d, truncated = %v (budget %d)", len(out.Content), out.Truncated, ExpandContentBudget)
	}
}

// TestWebFetch_EmptyURL rejects a blank url.
func TestWebFetch_EmptyURL(t *testing.T) {
	var out webFetchOutput
	err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(&stubWebClient{})), `{"url":""}`, &out)
	if err == nil {
		t.Fatal("expected error on empty url")
	}
}

// TestWebSearchTool_Status covers the Status() presentation helper.
func TestWebSearchTool_Status(t *testing.T) {
	line := webSearchStatus(json.RawMessage(`{"query":"golang context timeout"}`))
	if line.Emoji != "globe_with_meridians" || line.Verb != "searching the web" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "golang context timeout" {
		t.Errorf("detail = %q", line.Detail)
	}

	// Malformed JSON → no detail, static fields intact.
	line2 := webSearchStatus(json.RawMessage(`{bad`))
	if line2.Emoji != "globe_with_meridians" || line2.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line2.Emoji, line2.Detail)
	}
}

// TestWebSearchDescriptionOrgName proves that a run for a named organization
// sends the description with that name filled in, that a run with no saved
// name names no organization, and that rendering leaves the registered
// description alone.
func TestWebSearchDescriptionOrgName(t *testing.T) {
	const namedGolden = "Search the public web. Use it for what lies outside Acme: a project's " +
		"documentation or current version, a vendor's limits or pricing, a public specification, the news. " +
		"It knows nothing of Acme's internal work, which the corpus tools cover. " +
		"Returns results with a title, a url and a snippet; use web_fetch to read a page when the snippet is not enough."
	registered, err := NewEinoWebSearchTool(nil).Tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := &schema.ToolInfo{Name: "retrieve", Desc: "no placeholder"}
	render := func(name string) []*schema.ToolInfo {
		t.Helper()
		ctx := withOrgName(context.Background(), name)
		_, state, err := newAgenticOrgWordingMiddleware().BeforeModelRewriteState(ctx,
			&adk.TypedChatModelAgentState[*schema.AgenticMessage]{ToolInfos: []*schema.ToolInfo{registered, other}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return state.ToolInfos
	}

	if got := render("Acme")[0].Desc; got != namedGolden {
		t.Errorf("named description changed:\n got: %q\nwant: %q", got, namedGolden)
	}
	neutral := render("  ")[0].Desc
	if !strings.Contains(neutral, "Use it for what lies outside "+DefaultOrgName+":") ||
		strings.Contains(strings.ToLower(neutral), "acme") {
		t.Errorf("neutral description = %q", neutral)
	}
	if got := render("Example Corp"); got[1] != other {
		t.Error("a description without the placeholder must pass through unchanged")
	}
	if registered.Desc != webSearchToolDescription || !strings.Contains(registered.Desc, OrgNamePlaceholder) {
		t.Errorf("rendering modified the registered description: %q", registered.Desc)
	}
}

// TestWebFetchTool_Status covers the Status() presentation helper.
func TestWebFetchTool_Status(t *testing.T) {
	line := webFetchStatus(json.RawMessage(`{"url":"https://pkg.go.dev/context"}`))
	if line.Emoji != "globe_with_meridians" || line.Verb != "fetching" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "https://pkg.go.dev/context" {
		t.Errorf("detail = %q", line.Detail)
	}

	// Malformed JSON → no detail.
	line2 := webFetchStatus(json.RawMessage(`{bad`))
	if line2.Emoji != "globe_with_meridians" || line2.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line2.Emoji, line2.Detail)
	}
}
