package ask

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

func TestCitationAccumulator_DedupKeepsHighestScore(t *testing.T) {
	a := &citationAccumulator{}

	// Same (Source, ExternalID) showing up twice: low first then
	// high. The dedup must keep the high-score hit.
	a.add([]retrieve.Hit{
		{Source: "slack", ExternalID: "C1:100", Score: 0.4, Actor: "Riley"},
		{Source: "slack", ExternalID: "C1:200", Score: 0.7, Actor: "Sue"},
	})
	a.add([]retrieve.Hit{
		{Source: "slack", ExternalID: "C1:100", Score: 0.9, Actor: "Riley"},
		// Same key but actor changed — the higher-score hit wins,
		// so this Sue actor should NOT be the one we surface.
		{Source: "slack", ExternalID: "C1:200", Score: 0.5, Actor: "Other"},
	})

	cits := a.finalize()
	if len(cits) != 2 {
		t.Fatalf("len(cits) = %d, want 2", len(cits))
	}
	// Sort order: by score desc.
	if cits[0].ExternalID != "C1:100" || cits[0].Score != 0.9 {
		t.Errorf("cits[0] = %+v, want (C1:100, 0.9)", cits[0])
	}
	if cits[1].ExternalID != "C1:200" || cits[1].Score != 0.7 {
		t.Errorf("cits[1] = %+v, want (C1:200, 0.7)", cits[1])
	}
	// Verify the actor on cits[1] is the higher-score Sue, not the
	// later-but-lower Other.
	if cits[1].Actor.Display != "Sue" {
		t.Errorf("cits[1].Actor = %+v, want Sue (the higher-score one wins)", cits[1].Actor)
	}
}

func TestCitationAccumulator_FirstSeenNegativeScoreNotDropped(t *testing.T) {
	// Regression for the dedup-with-zero-value bug. A first-seen
	// hit with a non-positive score must still land in the map.
	a := &citationAccumulator{}
	a.add([]retrieve.Hit{
		{Source: "slack", ExternalID: "C1:1", Score: -0.2},
		{Source: "agent_session", ExternalID: "S1:abc", Score: 0.0},
	})
	cits := a.finalize()
	if len(cits) != 2 {
		t.Fatalf("len(cits) = %d, want 2 (both hits should survive dedup)", len(cits))
	}
}

func TestCitationAccumulator_CapsAtMax(t *testing.T) {
	a := &citationAccumulator{}
	hits := make([]retrieve.Hit, 0, maxCitations+5)
	for i := 0; i < maxCitations+5; i++ {
		hits = append(hits, retrieve.Hit{
			Source:     "slack",
			ExternalID: "key" + string(rune('A'+i)),
			Score:      float32(i) * 0.01,
		})
	}
	a.add(hits)
	cits := a.finalize()
	if len(cits) != maxCitations {
		t.Errorf("len(cits) = %d, want %d", len(cits), maxCitations)
	}
}

func TestCitationAccumulator_NilSafe(t *testing.T) {
	var a *citationAccumulator
	a.add([]retrieve.Hit{{Source: "slack"}})
	if got := a.finalize(); got != nil {
		t.Errorf("finalize on nil accumulator = %v, want nil", got)
	}
}

func TestCitationAccumulator_ConcurrentAddAndSnapshot(t *testing.T) {
	t.Parallel()
	a := &citationAccumulator{}
	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				a.add([]retrieve.Hit{{
					Source:     "slack",
					ExternalID: fmt.Sprintf("%d:%d", writer, i),
				}})
				_ = a.snapshot()
			}
		}()
	}
	wg.Wait()
	if got := len(a.snapshot()); got != writers*perWriter {
		t.Fatalf("snapshot length = %d, want %d", got, writers*perWriter)
	}
}

func TestSurfaceAddendum_DistinctPerSurface(t *testing.T) {
	slack := surfaceAddendum(SurfaceSlack)
	http := surfaceAddendum(SurfaceHTTP)
	if slack == http {
		t.Error("Slack and HTTP addenda should differ")
	}
	if !strings.Contains(slack, "standard Markdown") || !strings.Contains(slack, "**bold**") || !strings.Contains(slack, "[text](url)") {
		t.Errorf("Slack addendum missing standard-Markdown contract: %q", slack)
	}
	if strings.Contains(slack, "Slack mrkdwn directly") || strings.Contains(slack, "<https://example.com|link text>") {
		t.Errorf("Slack addendum still permits legacy mrkdwn: %q", slack)
	}
	if !strings.Contains(http, "standard Markdown") || !strings.Contains(http, "**bold**") {
		t.Errorf("HTTP addendum missing the standard-Markdown contract: %q", http)
	}
}

func TestCitationFromContext_NoAccumulator(t *testing.T) {
	if got := citationsFromContext(context.Background()); got != nil {
		t.Errorf("plain ctx should yield nil from context, got %+v", got)
	}
}

func TestCitationAccumulator_RoundTripThroughContext(t *testing.T) {
	a := &citationAccumulator{}
	ctx := withCitations(context.Background(), a)
	got := citationsFromContext(ctx)
	if got != a {
		t.Errorf("round-trip yields different pointer: got %p want %p", got, a)
	}
	got.add([]retrieve.Hit{{Source: "slack", ExternalID: "x", Score: 0.5, OccurredAt: time.Now()}})
	if len(a.hits) != 1 {
		t.Errorf("write through ctx-fetched accumulator didn't reach original: len=%d", len(a.hits))
	}
}
