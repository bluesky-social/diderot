package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sourcebot"
)

// testCodeIndex shortens repository names the way a deployment whose
// repositories all live in one GitHub organization would.
var testCodeIndex = CodeIndex{RepoPrefix: "github.com/bluesky-social/", SearchScope: "every test repository"}

// stubCodeClient is a canned CodeClient — no HTTP.
type stubCodeClient struct {
	search     *sourcebot.SearchResult
	searchErr  error
	source     *sourcebot.SourceFile
	sourceErr  error
	lastSearch sourcebot.SearchParams
	lastRepo   string
	lastPath   string
}

func (s *stubCodeClient) Search(_ context.Context, p sourcebot.SearchParams) (*sourcebot.SearchResult, error) {
	s.lastSearch = p
	return s.search, s.searchErr
}

func (s *stubCodeClient) Source(_ context.Context, repo, path, _ string) (*sourcebot.SourceFile, error) {
	s.lastRepo, s.lastPath = repo, path
	return s.source, s.sourceErr
}

// TestCodeSearch_Invoke maps stub results to the output envelope,
// clamps matches, strips the repo host prefix, and wires the
// per-chunk GitHub URL anchor.
func TestCodeSearch_Invoke(t *testing.T) {
	const extURL = "https://github.com/bluesky-social/diderot/blob/main/internal/ask/session.go"
	stub := &stubCodeClient{
		search: &sourcebot.SearchResult{
			TotalMatchCount:  7,
			ActualMatchCount: 1,
			Files: []sourcebot.FileMatch{
				{
					Repository:     "github.com/bluesky-social/diderot",
					Path:           "internal/ask/session.go",
					Language:       "Go",
					ExternalWebURL: extURL,
					Chunks:         []sourcebot.Chunk{{StartLine: 42, Content: "func LoadOrCreate"}},
				},
			},
		},
	}
	out, err := invokeNativeCodeSearch(t, NewEinoCodeSearchTool(fixedCode(stub, testCodeIndex)), `{"query":"LoadOrCreate","matches":999,"context_lines":"5"}`)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.TotalMatches != 7 || len(out.Files) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.Files[0].Repo != "diderot" {
		t.Errorf("repo = %q, want short form 'diderot'", out.Files[0].Repo)
	}
	if out.Files[0].URL != extURL {
		t.Errorf("file URL = %q, want %q", out.Files[0].URL, extURL)
	}
	if out.Files[0].Chunks[0].StartLine != 42 {
		t.Errorf("chunk = %+v", out.Files[0].Chunks[0])
	}
	wantChunkURL := extURL + "#L42"
	if out.Files[0].Chunks[0].URL != wantChunkURL {
		t.Errorf("chunk URL = %q, want %q", out.Files[0].Chunks[0].URL, wantChunkURL)
	}
	if stub.lastSearch.Matches != codeSearchMaxMatches {
		t.Errorf("matches clamped to %d, want %d", stub.lastSearch.Matches, codeSearchMaxMatches)
	}
	if stub.lastSearch.ContextLines != 5 {
		t.Errorf("context_lines = %d (string-shaped int should parse)", stub.lastSearch.ContextLines)
	}
}

// TestCodeSearch_Validation rejects blank queries; client errors
// propagate for the loop's tool-error envelope.
func TestCodeSearch_Validation(t *testing.T) {
	tool := NewEinoCodeSearchTool(fixedCode(&stubCodeClient{}, testCodeIndex))
	if _, err := invokeNativeCodeSearch(t, tool, `{"query":"  "}`); err == nil {
		t.Error("expected error on blank query")
	}
	tool = NewEinoCodeSearchTool(fixedCode(&stubCodeClient{searchErr: errors.New("boom")}, testCodeIndex))
	if _, err := invokeNativeCodeSearch(t, tool, `{"query":"q"}`); err == nil {
		t.Error("expected client error to propagate")
	}
}

// TestCodeRead_Success maps a source fetch to ok=true, expands
// the short repo name for the client call, and passes through the
// ExternalWebURL as the URL field.
func TestCodeRead_Success(t *testing.T) {
	const extURL = "https://github.com/bluesky-social/diderot/blob/main/cmd/diderot/main.go"
	stub := &stubCodeClient{
		source: &sourcebot.SourceFile{
			Repo:           "github.com/bluesky-social/diderot",
			Path:           "cmd/diderot/main.go",
			Language:       "Go",
			ExternalWebURL: extURL,
			Source:         "package main",
		},
	}
	out, err := invokeNativeCodeRead(t, NewEinoCodeReadTool(fixedCode(stub, testCodeIndex)), `{"repo":"diderot","path":"cmd/diderot/main.go"}`)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if stub.lastRepo != "github.com/bluesky-social/diderot" {
		t.Errorf("client got repo %q, want long form", stub.lastRepo)
	}
	if !out.OK || out.Content != "package main" || out.Repo != "diderot" {
		t.Errorf("out = %+v", out)
	}
	if out.URL != extURL {
		t.Errorf("URL = %q, want %q", out.URL, extURL)
	}
}

// TestCodeRead_NotFoundRecovers: ErrNotFound → ok=false envelope,
// not a hard tool error.
func TestCodeRead_NotFoundRecovers(t *testing.T) {
	stub := &stubCodeClient{sourceErr: fmt.Errorf("wrap: %w", sourcebot.ErrNotFound)}
	out, err := invokeNativeCodeRead(t, NewEinoCodeReadTool(fixedCode(stub, testCodeIndex)), `{"repo":"diderot","path":"gone.go"}`)
	if err != nil {
		t.Fatalf("should recover, got: %v", err)
	}
	if out.OK {
		t.Errorf("out = %+v, want ok=false", out)
	}
}

// TestCodeRead_TruncatesToBudget caps oversized files at the expand
// budget.
func TestCodeRead_TruncatesToBudget(t *testing.T) {
	stub := &stubCodeClient{
		source: &sourcebot.SourceFile{
			Repo:   "github.com/bluesky-social/x",
			Path:   "big.go",
			Source: strings.Repeat("a", ExpandContentBudget+50),
		},
	}
	out, err := invokeNativeCodeRead(t, NewEinoCodeReadTool(fixedCode(stub, testCodeIndex)), `{"repo":"x","path":"big.go"}`)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Truncated || len(out.Content) > ExpandContentBudget {
		t.Errorf("len=%d truncated=%v", len(out.Content), out.Truncated)
	}
}

// TestCodeRead_BinaryRecovers: non-UTF-8 content → ok=false, not an
// error.
func TestCodeRead_BinaryRecovers(t *testing.T) {
	stub := &stubCodeClient{
		source: &sourcebot.SourceFile{Repo: "github.com/bluesky-social/x", Path: "img.png", Source: "\xff\xfe"},
	}
	out, err := invokeNativeCodeRead(t, NewEinoCodeReadTool(fixedCode(stub, testCodeIndex)), `{"repo":"x","path":"img.png"}`)
	if err != nil {
		t.Fatalf("should recover, got: %v", err)
	}
	if out.OK {
		t.Errorf("out = %+v, want ok=false on binary", out)
	}
}

// TestLongRepo_Idempotent: already-qualified names pass through.
func TestLongRepo_Idempotent(t *testing.T) {
	if got := testCodeIndex.longRepo("github.com/bluesky-social/diderot"); got != "github.com/bluesky-social/diderot" {
		t.Errorf("longRepo(qualified) = %q", got)
	}
	if got := testCodeIndex.longRepo("diderot"); got != "github.com/bluesky-social/diderot" {
		t.Errorf("longRepo(short) = %q", got)
	}
}

// TestCodeIndexShortNamesRoundTrip: a shortened name always expands back to
// the repository it came from, including names nested below the prefix.
func TestCodeIndexShortNamesRoundTrip(t *testing.T) {
	ix := CodeIndex{RepoPrefix: "gitlab.com/acme/"}
	for _, repository := range []string{
		"gitlab.com/acme/tools",
		"gitlab.com/acme/site.io",
		"gitlab.com/acme/platform.v2/api",
		"gitlab.com/acme/platform/api",
		"github.com/other/x",
	} {
		short := ix.shortRepo(repository)
		require.Equal(t, repository, ix.longRepo(short), "round trip of %q via %q", repository, short)
	}
	require.Equal(t, "tools", ix.shortRepo("gitlab.com/acme/tools"))
	require.Equal(t, "gitlab.com/acme/platform.v2/api", ix.shortRepo("gitlab.com/acme/platform.v2/api"))

	// A self-hosted index may use a single-label host.
	local := CodeIndex{RepoPrefix: "gitlab/acme/"}
	for _, repository := range []string{"gitlab/acme/tools", "gitlab/acme/platform/api"} {
		require.Equal(t, repository, local.longRepo(local.shortRepo(repository)))
	}
}

// TestCodeIndexZeroValueIsNeutral: without configuration, names stay fully
// qualified and the descriptions name no organization.
func TestCodeIndexZeroValueIsNeutral(t *testing.T) {
	var ix CodeIndex
	require.Equal(t, "github.com/example/api", ix.shortRepo("github.com/example/api"))
	require.Equal(t, "api", ix.longRepo("api"))
	require.Equal(t, "github.com/example/api", ix.longRepo("github.com/example/api"))
	require.True(t, strings.HasPrefix(codeSearchToolDescription(ix), "Search the code of "+DefaultCodeSearchScope+". Use it for"))
	require.True(t, strings.HasPrefix(codeReadToolDescription(ix), "Read the full content of ONE file from "+DefaultCodeReadScope+". Use it when"))
	for _, desc := range []string{codeSearchToolDescription(ix), codeReadToolDescription(ix)} {
		require.NotContains(t, strings.ToLower(desc), "bluesky")
	}
}

// configuredCodeIndex is a deployment that words its index for the model: the
// scope sentences come from the Code search connector's settings.
var configuredCodeIndex = CodeIndex{
	RepoPrefix:  "github.com/bluesky-social/",
	SearchScope: "every example-org repository, public and private (indexed hourly)",
	ReadScope:   "an example-org repository (default branch, as of the last hourly index)",
}

// TestCodeToolDescriptionsCarryTheConfiguredScope pins the whole text of both
// descriptions for a configured index, so a change to what the model reads
// about the code tools is a deliberate one.
func TestCodeToolDescriptionsCarryTheConfiguredScope(t *testing.T) {
	const searchGolden = "Search the code of every example-org repository, public and private (indexed hourly). " +
		"Use it for what the code says now: where something is defined, how a " +
		"function works, which services read a configuration value, what produces an error message. The corpus " +
		"tools cover what people said and decided; this covers what the code does. Each result is a file with " +
		"its repo, path, and matching chunks with their line numbers and, when the index has one, a `url` to " +
		"cite. Search for exact identifiers or distinctive strings, not descriptions, and narrow with repo:, " +
		"file: or lang:. `total_matches` of 0 means no file has every term under these filters: drop a filter or a term " +
		"once before concluding the code is not there. Use code_read when you need more of a file than the chunks show."
	const readGolden = "Read the full content of ONE file from an example-org repository (default branch, as of " +
		"the last hourly index). Use it when code_search chunks are not enough and you need the " +
		"whole function, the imports or the surrounding logic. Pass repo and path exactly as a code_search " +
		"result gives them. The result carries a `url` for the file, when the index has one, to cite, and " +
		"`truncated` when a long file was cut. When the file is not in the index, `ok` is false: go back to " +
		"the search results and do not guess another path. Files are large, so read the ones that matter."
	ctx := context.Background()
	search, err := NewEinoCodeSearchTool(fixedCode(nil, configuredCodeIndex)).Tool.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, searchGolden, search.Desc)
	read, err := NewEinoCodeReadTool(fixedCode(nil, configuredCodeIndex)).Tool.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, readGolden, read.Desc)
	require.Equal(t, "diderot", configuredCodeIndex.shortRepo("github.com/bluesky-social/diderot"))
	require.Equal(t, "github.com/bluesky-social/diderot", configuredCodeIndex.longRepo("diderot"))
}

// TestCodeSearchTool_Status covers the Status() presentation helper.
func TestCodeSearchTool_Status(t *testing.T) {
	line := codeSearchStatus(json.RawMessage(`{"query":"repo:diderot sym:Runner"}`))
	if line.Emoji != "computer" || line.Verb != "searching code" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "repo:diderot sym:Runner" {
		t.Errorf("detail = %q", line.Detail)
	}

	// Malformed JSON → no detail.
	line2 := codeSearchStatus(json.RawMessage(`{bad`))
	if line2.Emoji != "computer" || line2.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line2.Emoji, line2.Detail)
	}
}

// TestCodeReadTool_Status covers the Status() presentation helper,
// including the repo/path concatenation and the partial-input edge
// cases.
func TestCodeReadTool_Status(t *testing.T) {
	line := codeReadStatus(json.RawMessage(`{"repo":"diderot","path":"internal/ask/loop.go"}`))
	if line.Emoji != "computer" || line.Verb != "reading" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "diderot/internal/ask/loop.go" {
		t.Errorf("detail = %q, want repo/path", line.Detail)
	}

	// Only path set.
	line2 := codeReadStatus(json.RawMessage(`{"path":"main.go"}`))
	if line2.Detail != "main.go" {
		t.Errorf("path-only detail = %q", line2.Detail)
	}

	// Only repo set.
	line3 := codeReadStatus(json.RawMessage(`{"repo":"diderot"}`))
	if line3.Detail != "diderot" {
		t.Errorf("repo-only detail = %q", line3.Detail)
	}

	// Malformed JSON → no detail.
	line4 := codeReadStatus(json.RawMessage(`{bad`))
	if line4.Emoji != "computer" || line4.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line4.Emoji, line4.Detail)
	}
}

// TestEinoCodeToolsRegister keeps a compile-time-ish assertion that
// the production native constructors return registrable, invokable
// tools.
func TestEinoCodeToolsRegister(t *testing.T) {
	for _, reg := range []RegisteredTool{NewEinoCodeSearchTool(fixedCode(nil, testCodeIndex)), NewEinoCodeReadTool(fixedCode(nil, testCodeIndex))} {
		_, ok := reg.Tool.(tool.InvokableTool)
		require.Truef(t, ok, "native tool %T is not invokable", reg.Tool)
		tools, presentations, err := NewToolRegistry(context.Background(), []RegisteredTool{reg})
		require.NoError(t, err)
		require.Len(t, tools, 1)
		info, err := tools[0].Info(context.Background())
		require.NoError(t, err)
		require.NotNil(t, presentations[info.Name].Status)
	}
}
