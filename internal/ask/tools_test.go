package ask

import (
	"encoding/json"
	urlPkg "net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// TestRetrieveInput_TopKAcceptsBothShapes: models sometimes ship
// numeric tool args as JSON strings even when the schema declares
// "type: integer". A strict int unmarshal fails the tool call; we use
// json.Number
// (and convert with .Int64()) so both shapes round-trip.
func TestRetrieveInput_TopKAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"int", `{"question":"q","top_k": 20}`, 20},
		{"string", `{"question":"q","top_k": "20"}`, 20},
		{"omitted", `{"question":"q"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in retrieve.RequestInput
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatalf("decode: %v", err)
			}
			req, err := retrieve.RequestFromInput(in, nil)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if int64(req.TopK) != tc.want {
				t.Errorf("TopK = %d, want %d", req.TopK, tc.want)
			}
		})
	}
}

// TestHasThreadFor verifies the projection from retrieve.Hit.ThreadID
// (DB representation) to the model-facing has_thread flag. The
// pointer-to-bool encoding is load-bearing: nil → field omitted from
// JSON, &true → "has_thread": true, &false → "has_thread": false.
// A plain bool with omitempty would silently drop the false case.
func TestHasThreadFor(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		threadID string
		want     string // JSON encoding of just the field
	}{
		{"agent_session ⇒ omitted", "agent_session", "", ``},
		{"agent_session never sets thread_id", "agent_session", "ignored", ``},
		{"slack standalone ⇒ false", "slack", "", `false`},
		{"slack threaded ⇒ true", "slack", "C123:1700000000.000100", `true`},
		{"discord standalone ⇒ false", "discord", "", `false`},
		{"discord threaded ⇒ true", "discord", "100000000000000001/200000000000000002/300000000000000003", `true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit := retrieveOutputHit{
				Source:    tc.source,
				HasThread: hasThreadFor(testSources.Of(tc.source), tc.threadID),
			}
			b, err := json.Marshal(hit)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, ok := raw["has_thread"]
			switch {
			case tc.want == "" && ok:
				t.Errorf("has_thread should be omitted, got %s", got)
			case tc.want != "" && !ok:
				t.Errorf("has_thread should be %s, got omitted", tc.want)
			case tc.want != "" && string(got) != tc.want:
				t.Errorf("has_thread = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRetrieveOutputProjectsStructuredActorWithoutEmailLoss(t *testing.T) {
	res := &retrieve.Result{Hits: []retrieve.Hit{{
		Source:         "agent_session",
		Actor:          "alex@example.com",
		ActorCanonical: "alex@example.com",
		ActorID:        "alex@example.com",
	}}}
	got := retrieveOutputFromResult(res, testSources).Hits[0].Actor
	if got.Display != "alex@example.com" || got.Canonical.ID != "alex@example.com" {
		t.Fatalf("actor lost email: %+v", got)
	}
	if got.Canonical.Namespace != "email" || got.SourceIdentity.Namespace != "agent_session" || got.Role != "session_owner" {
		t.Fatalf("actor identity = %+v", got)
	}
}

// TestPadAgentSessionURL verifies that agent_session chunk URLs
// get a ±20-message padding so a click-through shows additional
// rounds of context around the relevant chunk. Operator request:
// http://sessions.example/sessions/<id>?start=112&end=159 → ?start=92&end=179.
func TestPadAgentSessionURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "chunk URL pads on both sides",
			in:   "http://sessions.example.com/sessions/00000000-0000-4000-8000-000000000008?start=112&end=159",
			want: "http://sessions.example.com/sessions/00000000-0000-4000-8000-000000000008?start=92&end=179",
		},
		{
			name: "start near zero clamps",
			in:   "http://sessions.example/sessions/abc?start=5&end=20",
			want: "http://sessions.example/sessions/abc?start=0&end=40",
		},
		{
			name: "session-level URL (no range) untouched",
			in:   "http://sessions.example/sessions/abc",
			want: "http://sessions.example/sessions/abc",
		},
		{
			name: "session-level URL with empty query untouched",
			in:   "http://sessions.example/sessions/abc?",
			want: "http://sessions.example/sessions/abc?",
		},
		{
			name: "URL with only start (no end) untouched",
			in:   "http://sessions.example/sessions/abc?start=10",
			want: "http://sessions.example/sessions/abc?start=10",
		},
		{
			name: "URL with only end (no start) untouched",
			in:   "http://sessions.example/sessions/abc?end=10",
			want: "http://sessions.example/sessions/abc?end=10",
		},
		{
			name: "non-session URL passes through (not our place to touch)",
			in:   "https://bsky.app/profile/x/post/y",
			want: "https://bsky.app/profile/x/post/y",
		},
		{
			name: "garbage start passes through",
			in:   "http://sessions.example/sessions/abc?start=foo&end=bar",
			want: "http://sessions.example/sessions/abc?start=foo&end=bar",
		},
		{
			name: "empty url passes through",
			in:   "",
			want: "",
		},
		{
			name: "preserves other query params",
			in:   "http://sessions.example/sessions/abc?start=100&end=200&token=xyz",
			want: "http://sessions.example/sessions/abc?end=220&start=80&token=xyz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := padAgentSessionURL(tc.in)
			if !urlsEquivalent(got, tc.want) {
				t.Errorf("padAgentSessionURL(%q)\n  got  %q\n  want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

// urlsEquivalent compares two URL strings ignoring query-param
// order, because url.Values.Encode() sorts alphabetically and
// we don't want to over-specify the test against that detail.
// Empty strings compare equal.
func urlsEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	au, errA := urlPkg.Parse(a)
	bu, errB := urlPkg.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if au.Scheme != bu.Scheme || au.Host != bu.Host || au.Path != bu.Path {
		return false
	}
	// Compare query value-sets.
	aq := au.Query()
	bq := bu.Query()
	if len(aq) != len(bq) {
		return false
	}
	for k, av := range aq {
		bv := bq[k]
		if len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	// Both must have the same `?` presence — distinguishes
	// "abc" from "abc?" in the empty-query case.
	if (au.RawQuery != "" || strings.HasSuffix(a, "?")) !=
		(bu.RawQuery != "" || strings.HasSuffix(b, "?")) {
		return false
	}
	return true
}

// TestRetrieveToolStatus covers the Status() presentation helper for
// valid input and malformed JSON.
func TestRetrieveToolStatus(t *testing.T) {
	line := retrieveStatus(json.RawMessage(`{"question":"what happened in dc2"}`))
	if line.Emoji != "mag" || line.Verb != "searching" {
		t.Errorf("emoji/verb = %q/%q, want mag/searching", line.Emoji, line.Verb)
	}
	if line.Detail != "what happened in dc2" {
		t.Errorf("detail = %q, want question text", line.Detail)
	}

	// Long question truncates.
	long := strings.Repeat("a", 100)
	line2 := retrieveStatus(json.RawMessage(`{"question":"` + long + `"}`))
	if len([]rune(line2.Detail)) > statusDetailCap {
		t.Errorf("detail len = %d, want <= %d", len([]rune(line2.Detail)), statusDetailCap)
	}
	if !strings.HasSuffix(line2.Detail, "…") {
		t.Errorf("truncated detail should end with ellipsis: %q", line2.Detail)
	}

	// Malformed JSON → no detail, static emoji/verb intact.
	line3 := retrieveStatus(json.RawMessage(`{bad json`))
	if line3.Emoji != "mag" || line3.Detail != "" {
		t.Errorf("malformed: emoji=%q detail=%q", line3.Emoji, line3.Detail)
	}
}

// TestTruncateStatusDetail_SanitizesMarkup: Detail is interpolated
// into a slack inline-code span; backticks would close the span and
// newlines would inject fake status lines. Both get replaced.
func TestTruncateStatusDetail_SanitizesMarkup(t *testing.T) {
	got := truncateStatusDetail("run `rm -rf`\nfake line")
	if strings.ContainsAny(got, "`\n\r") {
		t.Errorf("sanitized detail still contains markup-breaking chars: %q", got)
	}
	if got != "run 'rm -rf' fake line" {
		t.Errorf("got %q", got)
	}
}

// TestGranularityFor covers the agent_session summary-tier
// classifier: session events end in ":session", chunk events carry a
// ":<start>:<end>" range, and other sources emit no granularity at
// all (keeps the field out of their JSON).
func TestGranularityFor(t *testing.T) {
	cases := []struct {
		source, externalID, want string
	}{
		{"agent_session", "00000000-0000-4000-8000-000000000007:session", "session"},
		{"agent_session", "00000000-0000-4000-8000-000000000007:120:190", "chunk"},
		{"agent_session", "00000000-0000-4000-8000-000000000007:0:15", "chunk"},
		{"slack", "C0EXAMPLE01:1700000000.123456", ""},
		{"notion", "page-abc123", ""},
	}
	for _, c := range cases {
		if got := granularityFor(c.source, c.externalID); got != c.want {
			t.Errorf("granularityFor(%q, %q) = %q, want %q", c.source, c.externalID, got, c.want)
		}
	}
}
