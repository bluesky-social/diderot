package ask

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/tavily"
)

func invokeNativeWebTool(t *testing.T, reg RegisteredTool, arguments string, out any) error {
	t.Helper()
	invokable, ok := reg.Tool.(tool.InvokableTool)
	require.Truef(t, ok, "native tool %T is not invokable", reg.Tool)
	raw, err := invokable.InvokableRun(context.Background(), arguments)
	if err != nil {
		return err
	}
	require.NoError(t, json.Unmarshal([]byte(raw), out))
	return nil
}

func assertWebSchemaEqualsLegacy(t *testing.T, info *schema.ToolInfo, legacy json.RawMessage) {
	t.Helper()
	require.NotNil(t, info.ParamsOneOf)
	js, err := info.ToJSONSchema()
	require.NoError(t, err)
	gotRaw, err := json.Marshal(js)
	require.NoError(t, err)
	var got, want map[string]any
	require.NoError(t, json.Unmarshal(gotRaw, &got))
	require.NoError(t, json.Unmarshal(legacy, &want))
	require.Truef(t, reflect.DeepEqual(got, want), "native schema diverged\n got: %v\nwant: %v", got, want)
}

func TestEinoWebToolSchemas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reg    RegisteredTool
		legacy json.RawMessage
	}{
		{name: "web_search", reg: NewEinoWebSearchTool(nil), legacy: webSearchSchema},
		{name: "web_fetch", reg: NewEinoWebFetchTool(nil), legacy: webFetchSchema},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := einoToolInfo(t, tt.reg)
			require.Equal(t, tt.name, info.Name)
			assertWebSchemaEqualsLegacy(t, info, tt.legacy)
		})
	}
}

func TestEinoWebSearchNativePath(t *testing.T) {
	t.Parallel()
	stub := &stubWebClient{search: &tavily.SearchResult{Results: []tavily.SearchHit{{
		Title: "Docs", URL: "https://example.test/docs", Content: "answer", Score: .9,
	}}}}
	var out webSearchOutput
	err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(stub)),
		`{"query":"current docs","max_results":"999","topic":"weather","time_range":"week","include_domains":["example.test"]}`,
		&out)
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	require.Equal(t, "https://example.test/docs", out.Results[0].URL)
	require.Equal(t, webSearchMaxResults, stub.lastSearch.MaxResults)
	require.Empty(t, stub.lastSearch.Topic, "unknown topics retain Tavily's general default")
	require.Equal(t, "week", stub.lastSearch.TimeRange)
	require.Equal(t, []string{"example.test"}, stub.lastSearch.IncludeDomains)
}

func TestEinoWebSearchValidationAndFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		reg       RegisteredTool
		arguments string
		want      string
	}{
		{name: "no client", reg: NewEinoWebSearchTool(nil), arguments: `{"query":"q"}`, want: "no client configured"},
		{name: "blank query", reg: NewEinoWebSearchTool(fixedWeb(&stubWebClient{})), arguments: `{"query":"  "}`, want: "query required"},
		{name: "overflow max results", reg: NewEinoWebSearchTool(fixedWeb(&stubWebClient{})), arguments: `{"query":"q","max_results":"99999999999999999999"}`, want: "max_results: "},
		{name: "client error", reg: NewEinoWebSearchTool(fixedWeb(&stubWebClient{searchErr: errors.New("boom")})), arguments: `{"query":"q"}`, want: "web_search"},
		{name: "nil result invariant", reg: NewEinoWebSearchTool(fixedWeb(&stubWebClient{})), arguments: `{"query":"q"}`, want: "nil result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out webSearchOutput
			err := invokeNativeWebTool(t, tt.reg, tt.arguments, &out)
			require.ErrorContains(t, err, tt.want)
		})
	}

	var empty webSearchOutput
	require.NoError(t, invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(&stubWebClient{search: &tavily.SearchResult{}})), `{"query":"q"}`, &empty))
	require.Empty(t, empty.Results)
}

func TestEinoWebSearchNilResultIsInvariantFailure(t *testing.T) {
	var out webSearchOutput
	err := invokeNativeWebTool(t, NewEinoWebSearchTool(fixedWeb(&stubWebClient{})), `{"query":"q"}`, &out)
	require.ErrorIs(t, err, ErrToolInvariant)
}

func TestEinoWebFetchNativeSuccessAndRecovery(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		stub := &stubWebClient{extract: &tavily.ExtractResult{Results: []tavily.ExtractHit{{
			URL: "https://example.test/page", RawContent: "page body",
		}}}}
		var out webFetchOutput
		err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)),
			`{"url":"https://example.test/page","query":"answer"}`, &out)
		require.NoError(t, err)
		require.True(t, out.OK)
		require.Equal(t, "page body", out.Content)
		require.Equal(t, []string{"https://example.test/page"}, stub.lastURLs)
		require.Equal(t, "answer", stub.lastQuery)
	})

	t.Run("per-url failure", func(t *testing.T) {
		stub := &stubWebClient{extract: &tavily.ExtractResult{Failed: []tavily.ExtractFailed{{
			URL: "https://example.test/gone", Error: "404 not found",
		}}}}
		var out webFetchOutput
		err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)),
			`{"url":"https://example.test/gone"}`, &out)
		require.NoError(t, err)
		require.False(t, out.OK)
		require.Equal(t, "404 not found", out.Note)
	})

	t.Run("truncation", func(t *testing.T) {
		stub := &stubWebClient{extract: &tavily.ExtractResult{Results: []tavily.ExtractHit{{
			URL: "https://example.test/large", RawContent: strings.Repeat("界", ExpandContentBudget),
		}}}}
		var out webFetchOutput
		err := invokeNativeWebTool(t, NewEinoWebFetchTool(fixedWeb(stub)),
			`{"url":"https://example.test/large"}`, &out)
		require.NoError(t, err)
		require.True(t, out.Truncated)
		require.LessOrEqual(t, len(out.Content), ExpandContentBudget)
		require.True(t, strings.HasSuffix(out.Content, "界"), "truncation must preserve a complete rune")
	})
}

func TestEinoWebFetchValidationAndFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		reg       RegisteredTool
		arguments string
		want      string
	}{
		{name: "no client", reg: NewEinoWebFetchTool(nil), arguments: `{"url":"https://example.test"}`, want: "no client configured"},
		{name: "blank url", reg: NewEinoWebFetchTool(fixedWeb(&stubWebClient{})), arguments: `{"url":"  "}`, want: "url required"},
		{name: "client error", reg: NewEinoWebFetchTool(fixedWeb(&stubWebClient{extractErr: errors.New("boom")})), arguments: `{"url":"https://example.test"}`, want: "web_fetch"},
		{name: "nil result invariant", reg: NewEinoWebFetchTool(fixedWeb(&stubWebClient{})), arguments: `{"url":"https://example.test"}`, want: "nil result"},
		{name: "invalid utf8", reg: NewEinoWebFetchTool(fixedWeb(&stubWebClient{extract: &tavily.ExtractResult{Results: []tavily.ExtractHit{{URL: "https://example.test", RawContent: "\xff"}}}})), arguments: `{"url":"https://example.test"}`, want: "non-UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out webFetchOutput
			err := invokeNativeWebTool(t, tt.reg, tt.arguments, &out)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestEinoWebPresentation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reg     RegisteredTool
		status  func(input json.RawMessage) StatusLine
		sources func(output json.RawMessage) []StatusSource
		summary func(output json.RawMessage) string
		input   json.RawMessage
		output  json.RawMessage
	}{
		{
			name: "search", reg: NewEinoWebSearchTool(nil),
			status: webSearchStatus, sources: webSearchStatusSources, summary: webSearchStatusOutput,
			input:  json.RawMessage(`{"query":"current docs"}`),
			output: json.RawMessage(`{"results":[{"title":"Docs","url":"https://example.test","content":"x","score":1}]}`),
		},
		{
			name: "fetch", reg: NewEinoWebFetchTool(nil),
			status: webFetchStatus, sources: webFetchStatusSources, summary: webFetchStatusOutput,
			input:  json.RawMessage(`{"url":"https://example.test"}`),
			output: json.RawMessage(`{"url":"https://example.test","ok":true,"content":"x"}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.status(tt.input), tt.reg.Presentation.Status(tt.input))
			require.Equal(t, tt.sources(tt.output), tt.reg.Presentation.StatusSources(tt.output))
			require.Equal(t, tt.summary(tt.output), tt.reg.Presentation.StatusOutput(tt.output))
		})
	}
}
