package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sourcebot"
)

// invokeNativeCodeSearch drives the production native code_search tool
// exactly as Eino's ToolsNode does: arguments arrive as a JSON string
// and the result comes back as a string, with no access to a legacy
// JSON-adapter Invoke.
func invokeNativeCodeSearch(t *testing.T, reg RegisteredTool, arguments string) (codeSearchOutput, error) {
	t.Helper()
	var raw string
	if err := invokeNativeCodeTool(t, reg, arguments, &raw); err != nil {
		return codeSearchOutput{}, err
	}
	var out codeSearchOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("native code_search output is not the model-facing shape: %v", err)
	}
	return out, nil
}

// invokeNativeCodeRead drives the production native code_read tool
// exactly as Eino's ToolsNode does: arguments arrive as a JSON string
// and the result comes back as a string, with no access to a legacy
// JSON-adapter Invoke.
func invokeNativeCodeRead(t *testing.T, reg RegisteredTool, arguments string) (codeReadOutput, error) {
	t.Helper()
	var raw string
	if err := invokeNativeCodeTool(t, reg, arguments, &raw); err != nil {
		return codeReadOutput{}, err
	}
	var out codeReadOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("native code_read output is not the model-facing shape: %v", err)
	}
	return out, nil
}

func invokeNativeCodeTool(t *testing.T, reg RegisteredTool, arguments string, raw *string) error {
	t.Helper()
	invokable, ok := reg.Tool.(tool.InvokableTool)
	if !ok {
		t.Fatalf("native constructor returned %T, want tool.InvokableTool", reg.Tool)
	}
	var err error
	*raw, err = invokable.InvokableRun(context.Background(), arguments)
	return err
}

// TestEinoCodeSearchToolSchema pins the native tool's model-facing
// contract to the legacy codeSearchSchema document verbatim: name,
// description, and the exact JSON Schema with the required query and
// the matches/context_lines bounds and defaults.
func TestEinoCodeSearchToolSchema(t *testing.T) {
	reg := NewEinoCodeSearchTool(fixedCode(nil, testCodeIndex))
	info := einoToolInfo(t, reg)

	if info.Name != "code_search" {
		t.Errorf("name = %q, want code_search", info.Name)
	}
	if !strings.Contains(info.Desc, "code_read") {
		t.Errorf("description lost the code_read handoff contract:\n%q", info.Desc)
	}
	if !strings.Contains(info.Desc, testCodeIndex.SearchScope) {
		t.Errorf("description lost the index contract:\n%q", info.Desc)
	}
	assertCodeSchemaEqualsLegacy(t, "code_search", info, codeSearchSchema)
}

// TestEinoCodeReadToolSchema pins the native tool's model-facing
// contract to the legacy codeReadSchema document verbatim: name,
// description, and the exact JSON Schema with the two required
// repo/path strings.
func TestEinoCodeReadToolSchema(t *testing.T) {
	reg := NewEinoCodeReadTool(fixedCode(nil, testCodeIndex))
	info := einoToolInfo(t, reg)

	if info.Name != "code_read" {
		t.Errorf("name = %q, want code_read", info.Name)
	}
	if !strings.Contains(info.Desc, "`ok` is false") {
		t.Errorf("description lost the recoverable-not-found contract:\n%q", info.Desc)
	}
	assertCodeSchemaEqualsLegacy(t, "code_read", info, codeReadSchema)
}

// assertCodeSchemaEqualsLegacy compares the native and legacy schemas as
// generic JSON documents. The expected side deliberately does not pass
// through Eino's schema type: otherwise an unsupported legacy field could be
// dropped from both sides and make the parity assertion vacuous.
func assertCodeSchemaEqualsLegacy(t *testing.T, name string, info *schema.ToolInfo, legacy json.RawMessage) {
	t.Helper()
	if info.ParamsOneOf == nil {
		t.Fatal("native tool has nil ParamsOneOf")
	}
	js, err := info.ToJSONSchema()
	require.NoError(t, err)
	got, err := json.Marshal(js)
	require.NoError(t, err)

	var gotDocument, wantDocument map[string]any
	if err := json.Unmarshal(got, &gotDocument); err != nil {
		t.Fatalf("native schema: %v", err)
	}
	if err := json.Unmarshal(legacy, &wantDocument); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if !reflect.DeepEqual(gotDocument, wantDocument) {
		t.Errorf("native %s schema diverges from legacy schema\n got: %v\nwant: %v", name, gotDocument, wantDocument)
	}
}

// TestEinoCodeSearchToolHappyPath executes the native tool end to end:
// a stub search result projects into the model-facing envelope with
// the repo host stripped, the chunk line anchor appended to the file
// URL, and total_matches surfaced.
func TestEinoCodeSearchToolHappyPath(t *testing.T) {
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
	reg := NewEinoCodeSearchTool(fixedCode(stub, testCodeIndex))
	out, err := invokeNativeCodeSearch(t, reg, `{"query":"LoadOrCreate","matches":999,"context_lines":"5"}`)
	require.NoError(t, err)
	if out.TotalMatches != 7 || len(out.Files) != 1 {
		t.Fatalf("out = %+v", out)
	}
	file := out.Files[0]
	if file.Repo != "diderot" {
		t.Errorf("repo = %q, want short form 'diderot'", file.Repo)
	}
	if file.URL != extURL {
		t.Errorf("file URL = %q, want %q", file.URL, extURL)
	}
	if file.Chunks[0].StartLine != 42 {
		t.Errorf("chunk = %+v", file.Chunks[0])
	}
	wantChunkURL := extURL + "#L42"
	if file.Chunks[0].URL != wantChunkURL {
		t.Errorf("chunk URL = %q, want %q", file.Chunks[0].URL, wantChunkURL)
	}
	if stub.lastSearch.Matches != codeSearchMaxMatches {
		t.Errorf("matches clamped to %d, want %d", stub.lastSearch.Matches, codeSearchMaxMatches)
	}
	if stub.lastSearch.ContextLines != 5 {
		t.Errorf("context_lines = %d (string-shaped int should parse)", stub.lastSearch.ContextLines)
	}
}

// TestEinoCodeSearchToolValidation keeps the typed validation on the
// native path: a blank query is a hard tool error, a nonsense numeric
// is rejected mid-decode with the field name, and client errors
// propagate for the runner's tool-error envelope.
func TestEinoCodeSearchToolValidation(t *testing.T) {
	cases := []struct {
		name      string
		reg       RegisteredTool
		arguments string
		wantError string
	}{
		{name: "no client", reg: NewEinoCodeSearchTool(fixedCode(nil, testCodeIndex)), arguments: `{"query":"q"}`, wantError: "no client configured"},
		{name: "blank query", reg: NewEinoCodeSearchTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"query":"  "}`, wantError: "query required"},
		{name: "bad matches", reg: NewEinoCodeSearchTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"query":"q","matches":"99999999999999999999"}`, wantError: "matches: "},
		{name: "bad context lines", reg: NewEinoCodeSearchTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"query":"q","context_lines":"99999999999999999999"}`, wantError: "context_lines: "},
		{name: "client error", reg: NewEinoCodeSearchTool(fixedCode(&stubCodeClient{searchErr: errors.New("boom")}, testCodeIndex)), arguments: `{"query":"q"}`, wantError: "code_search"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := invokeNativeCodeSearch(t, tt.reg, tt.arguments)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestEinoCodeSearchToolEmptySuccess(t *testing.T) {
	out, err := invokeNativeCodeSearch(t, NewEinoCodeSearchTool(fixedCode(&stubCodeClient{search: &sourcebot.SearchResult{}}, testCodeIndex)), `{"query":"q"}`)
	require.NoError(t, err)
	require.Empty(t, out.Files)
	require.Zero(t, out.TotalMatches)
}

func TestEinoCodeSearchNilResultIsInvariantFailure(t *testing.T) {
	_, err := invokeNativeCodeSearch(t, NewEinoCodeSearchTool(fixedCode(&stubCodeClient{}, testCodeIndex)), `{"query":"q"}`)
	require.ErrorIs(t, err, ErrToolInvariant)
}

// TestEinoCodeReadToolHappyPath executes the native tool end to end: a
// source fetch projects ok=true, the short repo name is expanded for
// the client call, and the ExternalWebURL passes through as URL.
func TestEinoCodeReadToolHappyPath(t *testing.T) {
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
	reg := NewEinoCodeReadTool(fixedCode(stub, testCodeIndex))
	out, err := invokeNativeCodeRead(t, reg, `{"repo":"diderot","path":"cmd/diderot/main.go"}`)
	require.NoError(t, err)
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

// TestEinoCodeReadToolRecoverableNotFound keeps the model-recoverable
// not-found envelope on the native path: sourcebot.ErrNotFound (even
// wrapped) becomes ok=false with the guidance note, never a hard tool
// error.
func TestEinoCodeReadToolRecoverableNotFound(t *testing.T) {
	stub := &stubCodeClient{sourceErr: fmt.Errorf("wrap: %w", sourcebot.ErrNotFound)}
	reg := NewEinoCodeReadTool(fixedCode(stub, testCodeIndex))
	out, err := invokeNativeCodeRead(t, reg, `{"repo":"github.com/bluesky-social/diderot","path":"gone.go"}`)
	require.NoError(t, err)
	if out.OK {
		t.Errorf("out = %+v, want ok=false on not-found", out)
	}
	if !strings.Contains(out.Note, "not found") {
		t.Errorf("Note = %q, want the recoverable guidance", out.Note)
	}
	if out.Repo != "diderot" {
		t.Errorf("Repo = %q, want normalized short form", out.Repo)
	}
}

// TestEinoCodeReadToolBinaryRecovers keeps the recoverable binary
// envelope on the native path: non-UTF-8 source is ok=false, not an
// error.
func TestEinoCodeReadToolBinaryRecovers(t *testing.T) {
	stub := &stubCodeClient{
		source: &sourcebot.SourceFile{Repo: "github.com/bluesky-social/x", Path: "img.png", Source: "\xff\xfe"},
	}
	reg := NewEinoCodeReadTool(fixedCode(stub, testCodeIndex))
	out, err := invokeNativeCodeRead(t, reg, `{"repo":"github.com/bluesky-social/x","path":"img.png"}`)
	require.NoError(t, err)
	if out.OK {
		t.Errorf("out = %+v, want ok=false on binary", out)
	}
	if out.Repo != "x" {
		t.Errorf("Repo = %q, want normalized short form", out.Repo)
	}
}

// TestEinoCodeReadToolTruncatesToBudget proves the 64 KiB rune-safe cap
// holds on the native path and truncated=true is reported to the model.
func TestEinoCodeReadToolTruncatesToBudget(t *testing.T) {
	stub := &stubCodeClient{
		source: &sourcebot.SourceFile{
			Repo:   "github.com/bluesky-social/x",
			Path:   "big.go",
			Source: strings.Repeat("a", ExpandContentBudget+50),
		},
	}
	reg := NewEinoCodeReadTool(fixedCode(stub, testCodeIndex))
	out, err := invokeNativeCodeRead(t, reg, `{"repo":"x","path":"big.go"}`)
	require.NoError(t, err)
	if !out.Truncated || len(out.Content) > ExpandContentBudget {
		t.Errorf("len=%d truncated=%v", len(out.Content), out.Truncated)
	}
}

// TestEinoCodeReadToolValidation keeps the typed validation on the
// native path: missing or blank repo/path is a hard tool error, and
// real source errors propagate.
func TestEinoCodeReadToolValidation(t *testing.T) {
	cases := []struct {
		name      string
		reg       RegisteredTool
		arguments string
		wantError string
	}{
		{name: "no client", reg: NewEinoCodeReadTool(fixedCode(nil, testCodeIndex)), arguments: `{"repo":"diderot","path":"x.go"}`, wantError: "no client configured"},
		{name: "missing repo", reg: NewEinoCodeReadTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"path":"x.go"}`, wantError: "repo and path required"},
		{name: "missing path", reg: NewEinoCodeReadTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"repo":"diderot"}`, wantError: "repo and path required"},
		{name: "blank path", reg: NewEinoCodeReadTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"repo":"diderot","path":"  "}`, wantError: "repo and path required"},
		{name: "nil source", reg: NewEinoCodeReadTool(fixedCode(&stubCodeClient{}, testCodeIndex)), arguments: `{"repo":"diderot","path":"x.go"}`, wantError: "nil source"},
		{name: "source error", reg: NewEinoCodeReadTool(fixedCode(&stubCodeClient{sourceErr: errors.New("boom")}, testCodeIndex)), arguments: `{"repo":"diderot","path":"x.go"}`, wantError: "code_read"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := invokeNativeCodeRead(t, tt.reg, tt.arguments)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestEinoCodeReadNilSourceIsInvariantFailure(t *testing.T) {
	_, err := invokeNativeCodeRead(t, NewEinoCodeReadTool(fixedCode(&stubCodeClient{}, testCodeIndex)), `{"repo":"diderot","path":"x.go"}`)
	require.ErrorIs(t, err, ErrToolInvariant)
}

// TestEinoCodeSearchPresentation checks that the registered callbacks
// produce the expected status surface for the native tool.
func TestEinoCodeSearchPresentation(t *testing.T) {
	reg := NewEinoCodeSearchTool(fixedCode(nil, testCodeIndex))

	if reg.Presentation.Status == nil || reg.Presentation.StatusSources == nil || reg.Presentation.StatusOutput == nil {
		t.Fatal("native code_search presentation incomplete")
	}
	input := json.RawMessage(`{"query":"repo:diderot sym:Runner"}`)
	output := json.RawMessage(`{"total_matches":7,"files":[{"repo":"diderot","path":"internal/ask/session.go","url":"https://x/1","chunks":[{"start_line":42,"content":"func LoadOrCreate","url":"https://x/1#L42"}]}]}`)
	if got, want := reg.Presentation.Status(input), codeSearchStatus(input); got != want {
		t.Errorf("presentation Status = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusSources(output), codeSearchStatusSources(output); !statusSourcesEqual(got, want) {
		t.Errorf("presentation StatusSources = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusOutput(output), codeSearchStatusOutput(output); got != want {
		t.Errorf("presentation StatusOutput = %q, want %q", got, want)
	}
}

// TestEinoCodeReadPresentation checks that the registered callbacks
// produce the expected status surface for the native tool.
func TestEinoCodeReadPresentation(t *testing.T) {
	reg := NewEinoCodeReadTool(fixedCode(nil, testCodeIndex))

	if reg.Presentation.Status == nil || reg.Presentation.StatusSources == nil || reg.Presentation.StatusOutput == nil {
		t.Fatal("native code_read presentation incomplete")
	}
	for _, tt := range []struct {
		name   string
		input  json.RawMessage
		output json.RawMessage
	}{
		{name: "loaded", input: json.RawMessage(`{"repo":"diderot","path":"cmd/main.go"}`), output: json.RawMessage(`{"repo":"diderot","path":"cmd/main.go","ok":true,"url":"https://x/1","content":"hi"}`)},
		{name: "loaded truncated", input: json.RawMessage(`{"repo":"diderot","path":"cmd/main.go"}`), output: json.RawMessage(`{"repo":"diderot","path":"cmd/main.go","ok":true,"url":"https://x/1","content":"hi","truncated":true}`)},
		{name: "not found", input: json.RawMessage(`{"repo":"diderot","path":"gone.go"}`), output: json.RawMessage(`{"repo":"diderot","path":"gone.go","ok":false,"note":"file not found in the index; check repo and path against a code_search hit"}`)},
		{name: "binary", input: json.RawMessage(`{"repo":"diderot","path":"img.png"}`), output: json.RawMessage(`{"repo":"diderot","path":"img.png","ok":false,"note":"file is not valid UTF-8 (binary?); work with the code_search chunks"}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := reg.Presentation.Status(tt.input), codeReadStatus(tt.input); got != want {
				t.Errorf("presentation Status = %+v, want %+v", got, want)
			}
			if got, want := reg.Presentation.StatusSources(tt.output), codeReadStatusSources(tt.output); !statusSourcesEqual(got, want) {
				t.Errorf("presentation StatusSources = %+v, want %+v", got, want)
			}
			if got, want := reg.Presentation.StatusOutput(tt.output), codeReadStatusOutput(tt.output); got != want {
				t.Errorf("presentation StatusOutput = %q, want %q", got, want)
			}
		})
	}
}
