package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	etool "github.com/cloudwego/eino/components/tool"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/identity"
)

// fakeExpander lets tests script the SourceExpander reply per
// event_id. Keeps each test compact: declare the (id → result) map,
// invoke the tool, assert.
type fakeExpander struct {
	results map[int64]ask_fakeResult

	// fetchCount receives one increment per Expand call when non-nil,
	// so tests can prove the scope gate runs before any content fetch.
	fetchCount *int
}

// ask_fakeResult bundles result + err so the script can return
// either (or both, since errors carry partial-result data — e.g.
// ErrSourceNotExpandable returns Source populated).
type ask_fakeResult struct {
	res ExpandResult
	err error
}

func (f *fakeExpander) Expand(_ context.Context, eventID int64) (ExpandResult, error) {
	if f.fetchCount != nil {
		*f.fetchCount++
	}
	r, ok := f.results[eventID]
	if !ok {
		return ExpandResult{}, ErrExpandEventNotFound
	}
	return r.res, r.err
}

func newPublicExpandTool(e SourceExpander) RegisteredTool {
	return NewEinoExpandTool(e, publicScopeLookup{}, testSources)
}

type publicScopeLookup struct{}

func (publicScopeLookup) EventScope(context.Context, int64) (string, error) {
	return events.ScopePublic, nil
}

// fakeScopeLookup scripts EventScope per event_id for the gate
// tests. Unknown ids return events.ErrEventNotFound — the sentinel
// the real impl (events.DBSink.EventScope) returns. Using the real
// sentinel here is load-bearing: the gate must match it, or a
// missing row falls through to the hard-error branch and the deny
// shape diverges from not-found (existence oracle).
type fakeScopeLookup struct {
	scopes map[int64]string
}

func (f *fakeScopeLookup) EventScope(_ context.Context, eventID int64) (string, error) {
	s, ok := f.scopes[eventID]
	if !ok {
		return "", events.ErrEventNotFound
	}
	return s, nil
}

// callTool drives the native Eino `expand` tool exactly as Eino's
// ToolsNode does — arguments arrive as a JSON string, results come
// back as a string — and decodes the response into expandOutput so
// tests can inspect the model-facing shape directly.
func callTool(t *testing.T, tool RegisteredTool, eventID any) (expandOutput, error) {
	t.Helper()
	invokable, ok := tool.Tool.(etool.InvokableTool)
	if !ok {
		t.Fatalf("native expand tool %T is not tool.InvokableTool", tool.Tool)
	}
	in, err := json.Marshal(map[string]any{"event_id": eventID})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	rawOut, err := invokable.InvokableRun(WithAllowedScopes(context.Background(), []string{events.ScopePublic}), string(in))
	if err != nil {
		return expandOutput{}, err
	}
	var out expandOutput
	if err := json.Unmarshal([]byte(rawOut), &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return out, nil
}

func TestExpandTool_BedrockHappyPath(t *testing.T) {
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			42: {res: ExpandResult{
				Source:  "agent_session",
				Actor:   identity.FromEvent(testSources.Of("agent_session"), "alex@example.com", "alex@example.com", "alex@example.com"),
				Content: "user: hello\nassistant: hi back",
			}},
		},
	})
	out, err := callTool(t, tool, 42)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Supported {
		t.Errorf("Supported = false, want true")
	}
	if out.Source != "agent_session" {
		t.Errorf("Source = %q, want agent_session", out.Source)
	}
	if out.Actor.Canonical.ID != "alex@example.com" || out.Actor.Role != "session_owner" {
		t.Errorf("Actor = %+v, want structured session owner", out.Actor)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Errorf("Content = %q, want it to include 'hello'", out.Content)
	}
	if out.Truncated {
		t.Errorf("Truncated = true, want false (under budget)")
	}
}

func TestExpandTool_NotFoundReturnsCleanShape(t *testing.T) {
	// Missing event_id should NOT raise a tool-error; instead the
	// model gets a structured "not supported" envelope so the turn
	// continues rather than aborting.
	tool := newPublicExpandTool(&fakeExpander{results: map[int64]ask_fakeResult{}})
	out, err := callTool(t, tool, 999)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Supported {
		t.Errorf("Supported = true, want false")
	}
	if !strings.Contains(out.Note, "999") {
		t.Errorf("Note = %q, want it to include the event_id", out.Note)
	}
}

func TestExpandTool_SourceNotExpandable(t *testing.T) {
	// ErrSourceNotExpandable carries the actual source name in the
	// returned ExpandResult. Tool surfaces it so the model knows
	// which kind of row it tried to expand.
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			7: {res: ExpandResult{Source: "slack"}, err: ErrSourceNotExpandable},
		},
	})
	out, err := callTool(t, tool, 7)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Supported {
		t.Errorf("Supported = true, want false")
	}
	if out.Source != "slack" {
		t.Errorf("Source = %q, want slack", out.Source)
	}
	if !strings.Contains(out.Note, "slack") {
		t.Errorf("Note = %q, want mention of slack", out.Note)
	}
}

func TestExpandTool_TruncatesOversizeContent(t *testing.T) {
	// Build content that exceeds the 64 KiB model-facing budget.
	// Tool should cap and set truncated=true regardless of what
	// the expander said.
	big := strings.Repeat("x", ExpandContentBudget+1024)
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "agent_session", Content: big}},
		},
	})
	out, err := callTool(t, tool, 1)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Truncated {
		t.Errorf("Truncated = false, want true (oversize)")
	}
	if len(out.Content) > ExpandContentBudget {
		t.Errorf("Content len = %d, want <= %d", len(out.Content), ExpandContentBudget)
	}
}

func TestExpandTool_InvalidUTF8Errors(t *testing.T) {
	// A source that returns garbled bytes must not encode into the
	// tool result silently — better to fail loud than confuse the
	// model with non-UTF-8 input.
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "agent_session", Content: "\xff\xfe\xfd"}},
		},
	})
	_, err := callTool(t, tool, 1)
	if err == nil {
		t.Fatalf("Invoke succeeded on non-UTF-8 input; expected error")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("err = %v, want a UTF-8 message", err)
	}
}

func TestExpandTool_AcceptsStringEventID(t *testing.T) {
	// Claude periodically ships integer args as JSON strings even
	// when the schema declares "type: integer". Same json.Number
	// handling as RetrieveTool's top_k.
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			55: {res: ExpandResult{Source: "agent_session", Content: "ok"}},
		},
	})
	// Pass the id as a JSON string; the schema-declared shape is
	// integer but json.Number on the receiver side accepts both.
	out, err := callTool(t, tool, "55")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Supported {
		t.Errorf("Supported = false, want true")
	}
}

func TestExpandTool_RejectsZeroOrNegativeID(t *testing.T) {
	tool := newPublicExpandTool(&fakeExpander{})
	for _, id := range []int{0, -1} {
		if _, err := callTool(t, tool, id); err == nil {
			t.Errorf("Invoke(event_id=%d) succeeded; expected error", id)
		}
	}
}

func TestExpandTool_PropagatesRealErrors(t *testing.T) {
	// A real expander failure (session feed down, DB error) should
	// bubble up as a tool-layer error, not a fake-success
	// envelope. The loop sees the error and decides whether to
	// surface it to the operator vs feed it back to the model.
	wantErr := errors.New("session feed: connection refused")
	tool := newPublicExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "agent_session"}, err: wantErr},
		},
	})
	_, err := callTool(t, tool, 1)
	if err == nil {
		t.Fatal("Invoke succeeded; expected propagation of expander error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want propagation of underlying message", err)
	}
}

func TestExpandTool_NoExpanderConfigured(t *testing.T) {
	tool := newPublicExpandTool(nil)
	if _, err := callTool(t, tool, 1); err == nil {
		t.Fatal("Invoke succeeded without expander; expected error")
	}
}

// callToolScoped is callTool with an allowedScopes set installed in
// ctx — the shape every scoped (slack) turn runs with.
func callToolScoped(t *testing.T, tool RegisteredTool, eventID any, allowed []string) (expandOutput, error) {
	t.Helper()
	invokable, ok := tool.Tool.(etool.InvokableTool)
	if !ok {
		t.Fatalf("native expand tool %T is not tool.InvokableTool", tool.Tool)
	}
	in, err := json.Marshal(map[string]any{"event_id": eventID})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	ctx := WithAllowedScopes(context.Background(), allowed)
	rawOut, err := invokable.InvokableRun(ctx, string(in))
	if err != nil {
		return expandOutput{}, err
	}
	var out expandOutput
	if err := json.Unmarshal([]byte(rawOut), &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return out, nil
}

func TestExpandTool_ScopeGateAllows(t *testing.T) {
	tool := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			42: {res: ExpandResult{Source: "agent_session", Content: "the goods"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{42: "internal"}}, testSources,
	)
	out, err := callToolScoped(t, tool, 42, []string{"public", "internal"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Supported {
		t.Errorf("Supported = false, want true (scope in allowed set)")
	}
	if !strings.Contains(out.Content, "the goods") {
		t.Errorf("Content = %q, want expansion", out.Content)
	}
}

func TestExpandTool_ScopeGateDenies(t *testing.T) {
	// A private-channel event expanded from a different channel:
	// denied, with the same shape as not-found so expand can't be
	// used as an existence oracle.
	tool := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			42: {res: ExpandResult{Source: "slack", Content: "secret"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{42: "slack-channel:CPRIVATE"}}, testSources,
	)
	out, err := callToolScoped(t, tool, 42, []string{"public", "internal", "slack-channel:COTHER"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Supported {
		t.Errorf("Supported = true, want false (scope outside allowed set)")
	}
	if strings.Contains(out.Note, "channel:") || strings.Contains(out.Note, "scope") {
		t.Errorf("Note = %q leaks scope detail; want the not-found shape", out.Note)
	}
	if out.Content != "" {
		t.Errorf("Content = %q, want empty on deny", out.Content)
	}
}

func TestExpandTool_ScopeGateDenyMatchesNotFoundShape(t *testing.T) {
	// The deny response and the genuinely-missing-event response
	// must be indistinguishable (modulo the event_id echo) so an
	// out-of-scope probe learns nothing.
	tool := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "slack", Content: "x"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{1: "slack-channel:CPRIVATE"}}, testSources,
	)
	denied, err := callToolScoped(t, tool, 1, []string{"public"})
	if err != nil {
		t.Fatalf("Invoke (denied): %v", err)
	}
	missing, err := callToolScoped(t, tool, 2, []string{"public"})
	if err != nil {
		t.Fatalf("Invoke (missing): %v", err)
	}
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "1", "N")
		return strings.ReplaceAll(s, "2", "N")
	}
	if normalize(denied.Note) != normalize(missing.Note) || denied.Supported != missing.Supported {
		t.Errorf("deny shape %+v differs from not-found shape %+v", denied, missing)
	}
}

func TestExpandTool_ScopeGateUnstampedDenied(t *testing.T) {
	// The 'unstamped' sentinel is never in any resolver's set —
	// deny-by-default for events that missed their stamp.
	tool := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			9: {res: ExpandResult{Source: "slack", Content: "limbo"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{9: "unstamped"}}, testSources,
	)
	out, err := callToolScoped(t, tool, 9, []string{"public", "internal", "slack-channel:C1"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Supported {
		t.Errorf("Supported = true, want false (unstamped sentinel)")
	}
}

func TestExpandTool_ScopeGateNilLookupFailsClosed(t *testing.T) {
	// Scoped turn + no ScopeLookup wired = wiring bug. Must error,
	// not fall through to an ungated expand.
	tool := NewEinoExpandTool(&fakeExpander{
		results: map[int64]ask_fakeResult{
			1: {res: ExpandResult{Source: "agent_session", Content: "x"}},
		},
	}, nil, testSources)
	_, err := callToolScoped(t, tool, 1, []string{"public"})
	if err == nil {
		t.Fatal("Invoke succeeded on scoped turn without ScopeLookup; want fail-closed error")
	}
}

func TestExpandTool_UnscopedCtxDeniesAccess(t *testing.T) {
	tool := NewEinoExpandTool(
		&fakeExpander{results: map[int64]ask_fakeResult{
			3: {res: ExpandResult{Source: "agent_session", Content: "admin sees all"}},
		}},
		&fakeScopeLookup{scopes: map[int64]string{3: "slack-channel:CPRIVATE"}}, testSources,
	)
	out, err := callToolScoped(t, tool, 3, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Supported {
		t.Errorf("unscoped context must deny access")
	}
}

// TestExpandTool_Status covers Status() for both int and string
// event_id shapes, and for malformed input.
func TestExpandTool_Status(t *testing.T) {
	// Integer event_id.
	line := expandStatus(json.RawMessage(`{"event_id":42}`))
	if line.Emoji != "open_file_folder" || line.Verb != "expanding" {
		t.Errorf("emoji/verb = %q/%q", line.Emoji, line.Verb)
	}
	if line.Detail != "event=42" {
		t.Errorf("detail = %q, want event=42", line.Detail)
	}

	// String-shaped event_id (Claude sometimes ships ints as strings).
	line2 := expandStatus(json.RawMessage(`{"event_id":"99"}`))
	if line2.Detail != "event=99" {
		t.Errorf("detail = %q, want event=99", line2.Detail)
	}

	// Malformed JSON → no detail.
	line3 := expandStatus(json.RawMessage(`{bad`))
	if line3.Emoji != "open_file_folder" || line3.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line3.Emoji, line3.Detail)
	}
}

func TestExpandTool_SchemaAndSpec(t *testing.T) {
	// Sanity-check the surface presented to the model. Ensures the
	// schema parses, the name is stable, and the description reflects
	// every currently supported source plus exact-reference semantics.
	tool := newPublicExpandTool(&fakeExpander{})
	info, err := tool.Tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "expand" {
		t.Errorf("Name = %q, want expand", info.Name)
	}
	for _, want := range []string{"slack", "agent_session", "notion", "linear", "Items the question named exactly"} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("Description does not contain %q: %s", want, info.Desc)
		}
	}
	if info.ParamsOneOf == nil {
		t.Fatal("native expand tool has nil ParamsOneOf")
	}
	js, err := info.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if _, err := json.Marshal(js); err != nil {
		t.Fatalf("schema does not serialize: %v", err)
	}
}
