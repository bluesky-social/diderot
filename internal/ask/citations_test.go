package ask

import (
	"reflect"
	"testing"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

func TestResolveCitations(t *testing.T) {
	t.Parallel()
	acc := &citationAccumulator{hits: []retrieve.Hit{
		{EventID: 11, URL: "https://slack.example.com/archives/C1/p100"},
		{EventID: 22, URL: "https://sessions.internal/sessions/abc?owner=a%40example.com&client=codex&start=0&end=5"},
		{EventID: 33, URL: "https://notion.example.com/page-3"},
		{EventID: 0, URL: "https://broken.example.com/no-event"}, // no event id
		{EventID: 44, URL: ""}, // no URL
	}}

	answer := `Per <https://slack.example.com/archives/C1/p100|the thread>, X is true.
The session [here](https://sessions.internal/sessions/abc?owner=a%40example.com&client=codex&start=0&end=5) confirms it.
Also cites https://unretrieved.example.com/other which retrieval never surfaced.
And repeats <https://slack.example.com/archives/C1/p100> once more.`

	got := resolveCitations(answer, acc)
	want := []CitationRow{
		{EventID: 11, URL: "https://slack.example.com/archives/C1/p100"},
		{EventID: 22, URL: "https://sessions.internal/sessions/abc?owner=a%40example.com&client=codex&start=0&end=5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveCitations = %+v, want %+v", got, want)
	}
}

func TestResolveCitationsPaddedBedrockURL(t *testing.T) {
	t.Parallel()
	// The tool layer pads agent_session ranges before the model
	// sees them; the answer cites the padded form. sessionURLPad=20,
	// so stored start=25&end=30 renders as start=5&end=50.
	acc := &citationAccumulator{hits: []retrieve.Hit{
		{EventID: 77, URL: "https://sessions.internal/sessions/abc?start=25&end=30"},
	}}
	answer := "see [the session](https://sessions.internal/sessions/abc?end=50&start=5)"
	got := resolveCitations(answer, acc)
	if len(got) != 1 || got[0].EventID != 77 {
		t.Errorf("padded session URL should resolve to event 77, got %+v", got)
	}
}

// TestResolveCitationsPaddedURLCollisionRecordsBothEvents is a
// deterministic regression test for a misattribution bug in
// resolveCitations (citations.go).
//
// Trigger: two distinct agent_session hits whose (start, end) ranges are
// different but both pad (via padAgentSessionURL, sessionURLPad=20) to the
// IDENTICAL URL — the only form the model ever sees for either hit — are
// both in the turn's retrieved hits, and the answer cites that one shared
// URL.
//
// Contract (resolveCitations' doc comment): "URLs in the answer that match
// no retrieved hit ... are dropped" — implicitly, a URL that DOES match a
// retrieved hit must be attributed to that hit's event. Before the fix,
// byURL was a map[string]int64: when two hits collided on one URL, the
// second hit added to the accumulator silently overwrote the first, so the
// citation recorded was whichever hit happened to be appended LAST — not
// necessarily the one the model actually read — and this depended only on
// accumulation order, which Eino's concurrent tool-call execution does not
// guarantee is deterministic.
//
// Expected: both events are recorded as citations for the shared URL,
// regardless of which order the hits were accumulated in, so the citation
// metadata never silently drops one event in favor of the other.
func TestResolveCitationsPaddedURLCollisionRecordsBothEvents(t *testing.T) {
	t.Parallel()
	const padded = "https://sessions.internal/sessions/abc?end=50&start=0"
	hitA := retrieve.Hit{EventID: 101, Source: "agent_session", URL: "https://sessions.internal/sessions/abc?start=0&end=30"}
	hitB := retrieve.Hit{EventID: 202, Source: "agent_session", URL: "https://sessions.internal/sessions/abc?start=15&end=30"}
	if got := padAgentSessionURL(hitA.URL); got != padded {
		t.Fatalf("setup invariant broken: hitA should pad to %q, got %q", padded, got)
	}
	if got := padAgentSessionURL(hitB.URL); got != padded {
		t.Fatalf("setup invariant broken: hitB should pad to %q, got %q", padded, got)
	}

	answer := "see the session: " + padded
	want := map[int64]bool{101: true, 202: true}

	for _, hits := range [][]retrieve.Hit{{hitA, hitB}, {hitB, hitA}} {
		acc := &citationAccumulator{hits: hits}
		got := resolveCitations(answer, acc)
		gotIDs := map[int64]bool{}
		for _, row := range got {
			if row.URL != padded {
				t.Errorf("hits=%+v: citation row URL = %q, want %q", hits, row.URL, padded)
			}
			gotIDs[row.EventID] = true
		}
		if !reflect.DeepEqual(gotIDs, want) {
			t.Errorf("hits=%+v: resolveCitations event IDs = %v, want both events %v", hits, gotIDs, want)
		}
	}
}

func TestResolveCitationsEmpty(t *testing.T) {
	t.Parallel()
	if got := resolveCitations("cites https://example.com/1", nil); got != nil {
		t.Errorf("nil accumulator should yield nil, got %+v", got)
	}
	acc := &citationAccumulator{hits: []retrieve.Hit{{EventID: 1, URL: "https://example.com/1"}}}
	if got := resolveCitations("no links in this answer", acc); got != nil {
		t.Errorf("no URLs should yield nil, got %+v", got)
	}
}
