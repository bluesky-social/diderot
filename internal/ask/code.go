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

	"github.com/bluesky-social/diderot/internal/sourcebot"
)

// =====================================================================
// code_search / code_read: org codebase tools
// =====================================================================
//
// Third tool pair, mirroring the corpus (retrieve/expand) and web
// (web_search/web_fetch) pairs, backed by a Sourcebot deployment
// (a Zoekt index over the organization's repositories).
// code_search finds matching chunks; code_read pulls one whole file.
// The code tools apply no per-caller scope: whoever can use Ask can search
// and read every repository the Sourcebot deployment indexes. Enabling the
// Sourcebot connector is the access decision, so index only repositories
// every Ask user may read.

// CodeClient is the subset of *sourcebot.Client the code tools use.
// An interface so tests can stub without HTTP.
type CodeClient interface {
	Search(ctx context.Context, p sourcebot.SearchParams) (*sourcebot.SearchResult, error)
	Source(ctx context.Context, repo, path, ref string) (*sourcebot.SourceFile, error)
}

// CodeIndex is what a deployment tells the model about its Sourcebot index.
// The zero value is neutral: names stay fully qualified and the tool
// descriptions use DefaultCodeSearchScope and DefaultCodeReadScope.
type CodeIndex struct {
	// RepoPrefix is the code-host prefix stripped from repository names in
	// results and restored on code_read, such as "github.com/example/".
	RepoPrefix string
	// SearchScope completes code_search's description: "Search the code of
	// <SearchScope>."
	SearchScope string
	// ReadScope completes code_read's description: "Read the full content of
	// ONE file from <ReadScope>."
	ReadScope string
}

const (
	DefaultCodeSearchScope = "every repository in the code index"
	DefaultCodeReadScope   = "a repository in the code index (default branch, as of the last index)"
)

func (ix CodeIndex) searchScope() string {
	if ix.SearchScope == "" {
		return DefaultCodeSearchScope
	}
	return ix.SearchScope
}

func (ix CodeIndex) readScope() string {
	if ix.ReadScope == "" {
		return DefaultCodeReadScope
	}
	return ix.ReadScope
}

// codeSearchMaxMatches caps the model-requested match count.
const codeSearchMaxMatches = 50

// -------------------------------------------------------------------
// code_search
// -------------------------------------------------------------------

var codeSearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Terms match file content literally, ignoring case, and every term must match. They are not regular expressions: /Load.*Thread/ and (a|b) find nothing. Put a phrase in double quotes. Write a lowercase 'or' between alternatives and group with parentheses: (retry or backoff) timeout. Filters: repo:<regex> on the repository's full name, which starts with its host and owner, so write repo:billing-api or repo:billing-api$ and never repo:^billing-api; file:<regex> on the path; lang:<language>; sym:<name> for a symbol's definition. A leading - excludes: -file:test. Example: repo:billing-api lang:go \"context deadline exceeded\"."
    },
    "matches": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "default": 20,
      "description": "The most matches to return."
    },
    "context_lines": {
      "type": "integer",
      "minimum": 0,
      "maximum": 10,
      "default": 3,
      "description": "Lines of context around each match."
    }
  },
  "required": ["query"]
}`)

// codeSearchToolDescription is the model-facing description of
// `code_search`.
func codeSearchToolDescription(ix CodeIndex) string {
	return "Search the code of " + ix.searchScope() + ". Use it for what the code says now: where something is defined, how a " +
		"function works, which services read a configuration value, what produces an error message. The corpus " +
		"tools cover what people said and decided; this covers what the code does. Each result is a file with " +
		"its repo, path, and matching chunks with their line numbers and, when the index has one, a `url` to " +
		"cite. Search for exact identifiers or distinctive strings, not descriptions, and narrow with repo:, " +
		"file: or lang:. `total_matches` of 0 means no file has every term under these filters: drop a filter or a term " +
		"once before concluding the code is not there. Use code_read when you need more of a file than the chunks show."
}

// codeSearchInput mirrors the model's JSON. Numerics are json.Number
// (the ints-as-strings lesson from top_k / event_id / max_results).
type codeSearchInput struct {
	Query        string      `json:"query"`
	Matches      json.Number `json:"matches,omitempty"`
	ContextLines json.Number `json:"context_lines,omitempty"`
}

// codeSearchChunk is one model-facing match region.
type codeSearchChunk struct {
	StartLine int    `json:"start_line"`
	Content   string `json:"content"`
	// URL is the GitHub blob URL anchored to the chunk's start line
	// (e.g. https://github.com/…/blob/main/file.go#L42). Only present
	// when the sourcebot index carries externalWebUrl for the file and
	// StartLine > 0. Cite byte-for-byte; never hand-construct.
	URL string `json:"url,omitempty"`
}

// codeSearchFile is one model-facing file hit.
type codeSearchFile struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	// URL is the GitHub blob URL for the file (no line anchor).
	// Only present when the sourcebot index carries externalWebUrl.
	URL    string            `json:"url,omitempty"`
	Chunks []codeSearchChunk `json:"chunks"`
}

// codeSearchOutput wraps the hits plus a total so the model knows
// when the result set was truncated.
type codeSearchOutput struct {
	TotalMatches int              `json:"total_matches"`
	Files        []codeSearchFile `json:"files"`
	// Truncated and Note are set when the matches did not fit the size
	// budget: Note says what was left out and how to narrow the search.
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// The match count and context lines bound a result in lines, not bytes, and a
// match in a minified bundle, a lock file or generated data is one line of
// any length, so a single result can run to hundreds of thousands of
// characters. The budgets are in bytes of matched content, set so that the
// total cuts only the rare outsized search.
const (
	// codeSearchMaxChunkBytes cuts one match. Twenty-one lines of ordinary
	// code (the most context a search can ask for) are well under it.
	codeSearchMaxChunkBytes = 8 << 10
	// codeSearchMaxContentBytes is the sum over all matches returned.
	codeSearchMaxContentBytes = 56 << 10
)

// boundCodeSearchOutput fits a result to the budgets. A result that fits
// codeSearchMaxContentBytes is returned as it is, long lines and all. One
// that does not has each match longer than codeSearchMaxChunkBytes cut at a
// character boundary, so that one line cannot take the whole budget; matches
// are then kept in ranked order until the total is spent, and the rest are
// dropped whole. What is returned is always valid JSON.
func boundCodeSearchOutput(out codeSearchOutput) codeSearchOutput {
	total := 0
	for _, file := range out.Files {
		for _, chunk := range file.Chunks {
			total += len(chunk.Content)
		}
	}
	if total <= codeSearchMaxContentBytes {
		return out
	}
	matchedFiles := len(out.Files)
	kept := make([]codeSearchFile, 0, matchedFiles)
	spent, shortened := 0, 0
	full := false
	for _, file := range out.Files {
		if full {
			break
		}
		chunks := make([]codeSearchChunk, 0, len(file.Chunks))
		for _, chunk := range file.Chunks {
			if len(chunk.Content) > codeSearchMaxChunkBytes {
				cut := codeSearchMaxChunkBytes
				for cut > 0 && !utf8.RuneStart(chunk.Content[cut]) {
					cut--
				}
				chunk.Content = chunk.Content[:cut] + fmt.Sprintf("\n…(cut: %d more bytes of this match)", len(chunk.Content)-cut)
				shortened++
			}
			if spent+len(chunk.Content) > codeSearchMaxContentBytes {
				full = true
				break
			}
			spent += len(chunk.Content)
			chunks = append(chunks, chunk)
		}
		// A file is listed with no matches only when the index returned it so.
		if len(chunks) > 0 || len(file.Chunks) == 0 {
			file.Chunks = chunks
			kept = append(kept, file)
		}
	}
	out.Files = kept
	if !full && shortened == 0 {
		return out
	}
	out.Truncated = true
	var note []string
	if full {
		note = append(note, fmt.Sprintf("The matches did not fit: %d of %d matching files are shown, the last of them possibly in part.", len(kept), matchedFiles))
	}
	if shortened > 0 {
		note = append(note, fmt.Sprintf("%d matches with very long lines were cut short.", shortened))
	}
	note = append(note, "Narrow the search with repo:, file: or lang:, add a term, or ask for fewer matches or context lines; code_read returns a whole file.")
	out.Note = strings.Join(note, " ")
	return out
}

func codeSearchStatusSources(output json.RawMessage) []StatusSource {
	var out codeSearchOutput
	if json.Unmarshal(output, &out) != nil {
		return nil
	}
	var sources []StatusSource
	for _, file := range out.Files {
		label := file.Repo + "/" + file.Path
		url := file.URL
		for _, chunk := range file.Chunks {
			if chunk.URL != "" {
				url = chunk.URL
				break
			}
		}
		if url != "" {
			sources = append(sources, StatusSource{URL: url, Label: label})
		}
	}
	return sources
}

func codeSearchStatusOutput(output json.RawMessage) string {
	var out codeSearchOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	files := "files"
	if len(out.Files) == 1 {
		files = "file"
	}
	return fmt.Sprintf("Found %d matches across %d %s", out.TotalMatches, len(out.Files), files)
}

func codeSearchStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "computer", Verb: "searching code"}
	var probe struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || probe.Query == "" {
		return sl
	}
	sl.Detail = truncateStatusDetail(probe.Query)
	return sl
}

// runCodeSearch is the execution path behind the native Eino
// `code_search` tool. It validates the typed model-facing input,
// applies the match/context clamps against the schema bounds, and
// runs the shared CodeClient.
func runCodeSearch(ctx context.Context, client CodeClient, ix CodeIndex, in codeSearchInput) (codeSearchOutput, error) {
	if client == nil {
		return codeSearchOutput{}, errors.New("code_search tool: no client configured")
	}
	if strings.TrimSpace(in.Query) == "" {
		return codeSearchOutput{}, errors.New("query required")
	}
	matches := 20
	if in.Matches != "" {
		n, err := in.Matches.Int64()
		if err != nil {
			return codeSearchOutput{}, fmt.Errorf("matches: %w", err)
		}
		matches = min(max(int(n), 1), codeSearchMaxMatches)
	}
	contextLines := 3
	if in.ContextLines != "" {
		n, err := in.ContextLines.Int64()
		if err != nil {
			return codeSearchOutput{}, fmt.Errorf("context_lines: %w", err)
		}
		contextLines = min(max(int(n), 0), 10)
	}

	res, err := client.Search(ctx, sourcebot.SearchParams{
		Query:        in.Query,
		Matches:      matches,
		ContextLines: contextLines,
	})
	if err != nil {
		return codeSearchOutput{}, fmt.Errorf("code_search: %w", err)
	}
	if res == nil {
		return codeSearchOutput{}, fmt.Errorf("code_search: client returned nil result: %w", ErrToolInvariant)
	}
	return boundCodeSearchOutput(codeSearchOutputFromResult(ix, res)), nil
}

// codeSearchOutputFromResult projects a sourcebot SearchResult into the
// model-facing codeSearchOutput shape.
func codeSearchOutputFromResult(ix CodeIndex, res *sourcebot.SearchResult) codeSearchOutput {
	out := codeSearchOutput{
		TotalMatches: res.TotalMatchCount,
		Files:        make([]codeSearchFile, 0, len(res.Files)),
	}
	for _, f := range res.Files {
		cf := codeSearchFile{
			Repo:     ix.shortRepo(f.Repository),
			Path:     f.Path,
			Language: f.Language,
			URL:      f.ExternalWebURL,
			Chunks:   make([]codeSearchChunk, 0, len(f.Chunks)),
		}
		for _, ch := range f.Chunks {
			chunkURL := ""
			if f.ExternalWebURL != "" && ch.StartLine > 0 {
				chunkURL = fmt.Sprintf("%s#L%d", f.ExternalWebURL, ch.StartLine)
			}
			cf.Chunks = append(cf.Chunks, codeSearchChunk{
				StartLine: ch.StartLine,
				Content:   ch.Content,
				URL:       chunkURL,
			})
		}
		out.Files = append(out.Files, cf)
	}
	return out
}

// shortRepo strips the code-host prefix from a sourcebot repository
// identifier for the model-facing output: keeping 20 copies of
// "github.com/<org>/" in every tool result wastes context.
// code_read's Invoke re-expands it server-side, so the model can
// pass the short form back verbatim. Only names directly under the
// prefix are shortened: a short form never contains "/", so longRepo
// can't mistake one (say "platform.v2/api" under a nested group) for
// a qualified name.
func (ix CodeIndex) shortRepo(repository string) string {
	short, ok := strings.CutPrefix(repository, ix.RepoPrefix)
	if !ok || strings.Contains(short, "/") {
		return repository
	}
	return short
}

// longRepo is shortRepo's inverse; idempotent when the model passes
// an already-qualified name. Short names never contain "/", so any
// name that does is already qualified (e.g. github.com/org/x).
func (ix CodeIndex) longRepo(repo string) string {
	if strings.Contains(repo, "/") {
		return repo
	}
	return ix.RepoPrefix + repo
}

// -------------------------------------------------------------------
// code_read
// -------------------------------------------------------------------

var codeReadSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "repo": {
      "type": "string",
      "description": "The repo of a code_search result, exactly as given."
    },
    "path": {
      "type": "string",
      "description": "The path of a code_search result, exactly as given."
    }
  },
  "required": ["repo", "path"]
}`)

// codeReadToolDescription is the model-facing description of
// `code_read`.
func codeReadToolDescription(ix CodeIndex) string {
	return "Read the full content of ONE file from " + ix.readScope() + ". Use it when code_search chunks are not enough and you need the " +
		"whole function, the imports or the surrounding logic. Pass repo and path exactly as a code_search " +
		"result gives them. The result carries a `url` for the file, when the index has one, to cite, and " +
		"`truncated` when a long file was cut. When the file is not in the index, `ok` is false: go back to " +
		"the search results and do not guess another path. Files are large, so read the ones that matter."
}

// codeReadInput is what the model passes.
type codeReadInput struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

// codeReadOutput is the model-facing shape. Recover-not-abort: a
// missing file returns ok=false + note (model may have mangled the
// path, or the file moved since indexing) rather than a hard error.
type codeReadOutput struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	OK       bool   `json:"ok"`
	Note     string `json:"note,omitempty"`
	Language string `json:"language,omitempty"`
	// URL is the GitHub blob URL for the file. Only present when the
	// sourcebot index carries externalWebUrl and ok=true. Cite
	// byte-for-byte; never hand-construct.
	URL       string `json:"url,omitempty"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func codeReadStatusSources(output json.RawMessage) []StatusSource {
	var out codeReadOutput
	if json.Unmarshal(output, &out) != nil || !out.OK || out.URL == "" {
		return nil
	}
	return []StatusSource{{URL: out.URL, Label: out.Repo + "/" + out.Path}}
}

func codeReadStatusOutput(output json.RawMessage) string {
	var out codeReadOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	if !out.OK {
		if out.Note != "" {
			return "Could not read the file: " + truncateStatusDetail(out.Note)
		}
		return "Could not read the file"
	}
	result := "Loaded " + out.Repo + "/" + out.Path
	if out.Truncated {
		result += " (truncated)"
	}
	return result
}

func codeReadStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "computer", Verb: "reading"}
	var probe struct {
		Repo string `json:"repo"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || (probe.Repo == "" && probe.Path == "") {
		return sl
	}
	detail := probe.Repo + "/" + probe.Path
	if probe.Repo == "" {
		detail = probe.Path
	} else if probe.Path == "" {
		detail = probe.Repo
	}
	sl.Detail = truncateStatusDetail(detail)
	return sl
}

// codeIndex reads current, which is nil in the static tool catalog.
func codeIndex(current func() (CodeClient, CodeIndex)) (CodeClient, CodeIndex) {
	if current == nil {
		return nil, CodeIndex{}
	}
	return current()
}

// newCodeTool makes a native code tool optional: a run offers it while
// current returns a client, and describes it with the index wording current
// returns as the run starts.
func newCodeTool(native tool.InvokableTool, current func() (CodeClient, CodeIndex), describe func(CodeIndex) *schema.ToolInfo) *optionalTool {
	return &optionalTool{
		InvokableTool: native,
		info: func(context.Context) (*schema.ToolInfo, error) {
			_, ix := codeIndex(current)
			return describe(ix), nil
		},
		available: func() bool {
			client, _ := codeIndex(current)
			return client != nil
		},
	}
}

// NewEinoCodeSearchTool builds the native Eino `code_search` tool over the
// client and index wording current returns, read as each run starts and
// again on each call; a nil client means code search is off. It wires the
// presentation metadata (status line, sources, safe output summary) in as
// callbacks. The native tool decodes arguments and encodes results itself
// (utils.NewTool); it never routes through a JSON adapter.
func NewEinoCodeSearchTool(current func() (CodeClient, CodeIndex)) RegisteredTool {
	presentation := ToolPresentation{
		Status: func(input json.RawMessage) StatusLine {
			return codeSearchStatus(input)
		},
		StatusSources: func(output json.RawMessage) []StatusSource {
			return codeSearchStatusSources(output)
		},
		StatusOutput: func(output json.RawMessage) string {
			return codeSearchStatusOutput(output)
		},
	}

	// The model-facing schema is the codeSearchSchema document
	// verbatim (per-property descriptions, min/max/defaults, and the
	// required-query contract), carried as an Eino JSON Schema.
	js, err := parseToolJSONSchema(codeSearchSchema)
	if err != nil {
		// The schema is a compile-time constant; a parse failure here
		// is a programming error, not a runtime condition.
		panic(fmt.Sprintf("code_search: parse model schema: %v", err))
	}
	params := schema.NewParamsOneOfByJSONSchema(js)
	describe := func(ix CodeIndex) *schema.ToolInfo {
		return &schema.ToolInfo{Name: "code_search", Desc: codeSearchToolDescription(ix), ParamsOneOf: params}
	}
	invoke := func(ctx context.Context, in codeSearchInput) (codeSearchOutput, error) {
		client, ix := codeIndex(current)
		return runCodeSearch(ctx, client, ix, in)
	}

	einoTool := utils.NewTool(describe(CodeIndex{}), invoke)
	return RegisteredTool{Tool: newCodeTool(einoTool, current, describe), Presentation: presentation}
}

// NewEinoCodeReadTool builds the native Eino `code_read` tool over the
// client and index wording current returns, as NewEinoCodeSearchTool does,
// and wires the presentation metadata (status line, sources, safe output
// summary) in as callbacks. The native tool decodes arguments and encodes
// results itself (utils.NewTool); it never routes through a JSON adapter.
func NewEinoCodeReadTool(current func() (CodeClient, CodeIndex)) RegisteredTool {
	presentation := ToolPresentation{
		Status: func(input json.RawMessage) StatusLine {
			return codeReadStatus(input)
		},
		StatusSources: func(output json.RawMessage) []StatusSource {
			return codeReadStatusSources(output)
		},
		StatusOutput: func(output json.RawMessage) string {
			return codeReadStatusOutput(output)
		},
	}

	// The model-facing schema is the codeReadSchema document verbatim
	// (the two required repo/path strings with their descriptions),
	// carried as an Eino JSON Schema.
	js, err := parseToolJSONSchema(codeReadSchema)
	if err != nil {
		// The schema is a compile-time constant; a parse failure here
		// is a programming error, not a runtime condition.
		panic(fmt.Sprintf("code_read: parse model schema: %v", err))
	}
	params := schema.NewParamsOneOfByJSONSchema(js)
	describe := func(ix CodeIndex) *schema.ToolInfo {
		return &schema.ToolInfo{Name: "code_read", Desc: codeReadToolDescription(ix), ParamsOneOf: params}
	}
	invoke := func(ctx context.Context, in codeReadInput) (codeReadOutput, error) {
		client, ix := codeIndex(current)
		return runCodeRead(ctx, client, ix, in)
	}

	einoTool := utils.NewTool(describe(CodeIndex{}), invoke)
	return RegisteredTool{Tool: newCodeTool(einoTool, current, describe), Presentation: presentation}
}

// runCodeRead is the execution path behind the native Eino `code_read`
// tool. It validates the typed model-facing input, fetches the file
// through the shared CodeClient, and applies the recoverable
// not-found/binary envelopes plus the model-facing budget cap.
func runCodeRead(ctx context.Context, client CodeClient, ix CodeIndex, in codeReadInput) (codeReadOutput, error) {
	if client == nil {
		return codeReadOutput{}, errors.New("code_read tool: no client configured")
	}
	if strings.TrimSpace(in.Repo) == "" || strings.TrimSpace(in.Path) == "" {
		return codeReadOutput{}, errors.New("repo and path required")
	}

	src, err := client.Source(ctx, ix.longRepo(in.Repo), in.Path, "")
	switch {
	case errors.Is(err, sourcebot.ErrNotFound):
		return codeReadOutput{
			Repo: ix.shortRepo(ix.longRepo(in.Repo)),
			Path: in.Path,
			OK:   false,
			Note: "file not found in the index; check repo and path against a code_search hit",
		}, nil
	case err != nil:
		return codeReadOutput{}, fmt.Errorf("code_read: %w", err)
	case src == nil:
		return codeReadOutput{}, fmt.Errorf("code_read: client returned nil source: %w", ErrToolInvariant)
	}

	if !utf8.ValidString(src.Source) {
		// Binary or mis-encoded file: model-recoverable, not a bug.
		return codeReadOutput{
			Repo: ix.shortRepo(ix.longRepo(in.Repo)),
			Path: in.Path,
			OK:   false,
			Note: "file is not valid UTF-8 (binary?); work with the code_search chunks",
		}, nil
	}

	// Reuse the expand budget: cap the model-facing content and walk
	// back to a rune boundary.
	content := src.Source
	truncated := false
	if len(content) > ExpandContentBudget {
		cap := ExpandContentBudget
		for cap > 0 && !utf8.RuneStart(content[cap]) {
			cap--
		}
		content = content[:cap]
		truncated = true
	}

	return codeReadOutput{
		Repo:      ix.shortRepo(src.Repo),
		Path:      src.Path,
		OK:        true,
		Language:  src.Language,
		URL:       src.ExternalWebURL,
		Content:   content,
		Truncated: truncated,
	}, nil
}
