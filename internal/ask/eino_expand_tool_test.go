package ask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/require"
)

// invokeNativeExpand drives the production native expand tool exactly
// as Eino's ToolsNode does: arguments arrive as a JSON string and the
// result comes back as a string, with no access to a legacy
// JSON-adapter Invoke.
func invokeNativeExpand(t *testing.T, reg RegisteredTool, scoped []string, arguments string) (expandOutput, error) {
	t.Helper()
	invokable, ok := reg.Tool.(tool.InvokableTool)
	if !ok {
		t.Fatalf("NewEinoExpandTool returned %T, want tool.InvokableTool", reg.Tool)
	}
	ctx := context.Background()
	if scoped != nil {
		ctx = WithAllowedScopes(ctx, scoped)
	}
	var (
		raw string
		err error
	)
	if raw, err = invokable.InvokableRun(ctx, arguments); err != nil {
		return expandOutput{}, err
	}
	var out expandOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("native expand output is not the model-facing shape: %v", err)
	}
	return out, nil
}

// TestEinoExpandToolSchema pins the native tool's model-facing
// contract to the legacy expandSchema document verbatim: name,
// description, and the exact JSON Schema with the required event_id.
func TestEinoExpandToolSchema(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{}, nil, testSources)
	if reg.Tool == nil {
		t.Fatal("native constructor returned nil tool")
	}
	if reg.Presentation.Status == nil || reg.Presentation.StatusOutput == nil {
		t.Fatal("native expand presentation incomplete")
	}
	info, err := reg.Tool.Info(context.Background())
	require.NoError(t, err)
	if info.Name != "expand" {
		t.Errorf("name = %q, want expand", info.Name)
	}
	if !strings.Contains(info.Desc, "agent_session") {
		t.Errorf("description lost the agent_session contract:\n%q", info.Desc)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("native tool has nil ParamsOneOf")
	}
	js, err := info.ToJSONSchema()
	require.NoError(t, err)
	got, err := json.Marshal(js)
	require.NoError(t, err)
	var normalizedLegacy map[string]any
	if err := json.Unmarshal(expandSchema, &normalizedLegacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	want, err := json.Marshal(normalizedLegacy)
	require.NoError(t, err)
	if string(got) != string(want) {
		t.Errorf("native schema diverges from legacy expandSchema\n got: %s\nwant: %s", got, want)
	}
}

// TestEinoExpandToolScopeDeniedBeforeFetch proves the visibility gate
// runs before any content fetch on the native path: a DENY (or a
// genuinely missing event, which the gate resolves first) surfaces
// the indistinguishable not-found deny shape and the expander is
// never consulted.
func TestEinoExpandToolScopeDeniedBeforeFetch(t *testing.T) {
	var fetchCalls int
	exp := &fakeExpander{results: map[int64]ask_fakeResult{
		42: {res: ExpandResult{Source: "slack", Content: "secret private content"}},
	}}
	exp.fetchCount = &fetchCalls
	reg := NewEinoExpandTool(exp, &fakeScopeLookup{scopes: map[int64]string{42: "slack-channel:CPRIVATE"}}, testSources)

	out, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":42}`)
	require.NoError(t, err)
	if out.Supported {
		t.Errorf("Supported = true, want false (scope outside allowed set)")
	}
	if out.Content != "" {
		t.Errorf("Content = %q, want empty on deny", out.Content)
	}
	if strings.Contains(out.Note, "channel:") || strings.Contains(out.Note, "scope") {
		t.Errorf("Note = %q leaks scope detail; want the not-found shape", out.Note)
	}
	if fetchCalls != 0 {
		t.Errorf("expander consulted %d times; scope deny must happen before content fetch", fetchCalls)
	}
}

// TestEinoExpandToolScopeDenyMatchesNotFoundShape pins the native
// deny envelope to the not-found envelope so expand isn't an
// existence oracle on the Eino path either.
func TestEinoExpandToolScopeDenyMatchesNotFoundShape(t *testing.T) {
	reg := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "slack", Content: "x"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{1: "slack-channel:CPRIVATE"}}, testSources,
	)
	denied, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":1}`)
	require.NoError(t, err)
	missing, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":2}`)
	require.NoError(t, err)
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "1", "N")
		return strings.ReplaceAll(s, "2", "N")
	}
	if normalize(denied.Note) != normalize(missing.Note) || denied.Supported != missing.Supported {
		t.Errorf("deny shape %+v differs from not-found shape %+v", denied, missing)
	}
}

// TestEinoExpandToolScopedNilLookupFailsClosed ensures a scoped turn
// with no ScopeLookup wired stays a hard error on the native path,
// exactly like the legacy Invoke.
func TestEinoExpandToolScopedNilLookupFailsClosed(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{
		1: {res: ExpandResult{Source: "agent_session", Content: "x"}},
	}}, nil, testSources)
	_, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":1}`)
	require.ErrorContains(t, err, "no ScopeLookup configured")
}

// TestEinoExpandToolHappyPath executes the native tool end to end:
// an agent_session row expands to its full content with the
// model-facing envelope.
func TestEinoExpandToolHappyPath(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{
		42: {res: ExpandResult{Source: "agent_session", Content: "user: hello\nassistant: hi back"}},
	}}, publicScopeLookup{}, testSources)
	out, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":42}`)
	require.NoError(t, err)
	if !out.Supported {
		t.Errorf("Supported = false, want true")
	}
	if out.Source != "agent_session" {
		t.Errorf("Source = %q, want agent_session", out.Source)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Errorf("Content = %q, want it to include 'hello'", out.Content)
	}
	if out.Truncated {
		t.Errorf("Truncated = true, want false (under budget)")
	}
}

// TestEinoExpandToolStringEventID covers the schema-declared integer
// tolerating a JSON-string event_id through the native decode path,
// mirroring the legacy json.Number handling.
func TestEinoExpandToolStringEventID(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{
		55: {res: ExpandResult{Source: "agent_session", Content: "ok"}},
	}}, publicScopeLookup{}, testSources)
	out, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":"55"}`)
	require.NoError(t, err)
	if !out.Supported {
		t.Errorf("Supported = false, want true")
	}
}

// TestEinoExpandToolTruncatesOversizeContent verifies the 64 KiB
// rune-safe cap holds on the native path and truncated=true is
// reported to the model.
func TestEinoExpandToolTruncatesOversizeContent(t *testing.T) {
	big := strings.Repeat("x", ExpandContentBudget+1024)
	reg := NewEinoExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{
		1: {res: ExpandResult{Source: "agent_session", Content: big}},
	}}, publicScopeLookup{}, testSources)
	out, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":1}`)
	require.NoError(t, err)
	if !out.Supported {
		t.Errorf("Supported = false, want true")
	}
	if !out.Truncated {
		t.Errorf("Truncated = false, want true (oversize)")
	}
	if len(out.Content) > ExpandContentBudget {
		t.Errorf("Content len = %d, want <= %d", len(out.Content), ExpandContentBudget)
	}
}

// TestEinoExpandToolInvalidUTF8Errors keeps the loud UTF-8 rejection
// on the native path: the model must never see garbled bytes.
func TestEinoExpandToolInvalidUTF8Errors(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{
		1: {res: ExpandResult{Source: "agent_session", Content: "\xff\xfe\xfd"}},
	}}, publicScopeLookup{}, testSources)
	_, err := invokeNativeExpand(t, reg, []string{"public"}, `{"event_id":1}`)
	require.ErrorContains(t, err, "UTF-8")
}

// TestEinoExpandPresentation checks that the registered callbacks
// produce the expected status surface for the native tool.
func TestEinoExpandPresentation(t *testing.T) {
	reg := NewEinoExpandTool(&fakeExpander{}, nil, testSources)

	input := json.RawMessage(`{"event_id":42}`)
	output := json.RawMessage(`{"source":"agent_session","supported":true,"content":"hi","truncated":true}`)
	if got, want := reg.Presentation.Status(input), expandStatus(input); got != want {
		t.Errorf("presentation Status = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusOutput(output), expandStatusOutput(output, testSources); got != want {
		t.Errorf("presentation StatusOutput = %q, want %q", got, want)
	}
}
