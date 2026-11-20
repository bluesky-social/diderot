package ask

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/source"
)

// Surface identifies which UX is consuming the assistant's reply.
// The runner appends a Surface-specific format addendum after the
// base system prompt so the model knows whether to emit Slack Block
// Kit JSON, plain markdown, etc. Keeping the addendum a separate
// system block (not a string concat) keeps the base prompt
// byte-identical across surfaces so a provider's prompt cache can match
// the shared prefix.
type Surface int

const (
	// SurfaceHTTP is the default — markdown prose for the /v1/ask
	// HTTP handler.
	SurfaceHTTP Surface = iota
	// SurfaceSlack tells the model to emit standard Markdown for Slack's
	// native markdown Block Kit block (not Slack's legacy mrkdwn dialect and
	// not Block Kit JSON). The Slack consumer chunks long answers before
	// delivery.
	SurfaceSlack
)

// surfaceAddendum returns the format-specific snippet that gets
// appended as a separate SystemContentBlock after the base prompt.
// Each snippet is short on purpose — every token costs across every
// turn for the lifetime of every session.
func surfaceAddendum(s Surface) string {
	switch s {
	case SurfaceSlack:
		return embeddedAskPrompts.MustText(promptcatalog.AskSurfaceSlack)
	default:
		return embeddedAskPrompts.MustText(promptcatalog.AskSurfaceHTTP)
	}
}

// SurfaceInstructions exposes the deploy-managed output-format addendum for
// operator prompt catalogs. The runner still calls surfaceAddendum directly;
// this read-only projection keeps the effective prompt reviewable.
func SurfaceInstructions(s Surface) string { return surfaceAddendum(s) }

// StatusUpdate is one progress notification from the runner to its
// caller. Phase classifies WHAT is happening; Emoji + Verb + Summary
// let the consumer format human-readable text. The runner stays
// surface-agnostic — the slack consumer renders the fields as a
// per-tool status line.
type StatusUpdate struct {
	// Phase: "tool_use" — model decided to call a tool.
	// "synthesizing" — final-answer turn just started (no tools).
	// (Other phases reserved for future: "thinking", "error".)
	Phase string
	// ToolName is the name from the tool_use block, e.g. "retrieve".
	// Empty for non-tool phases.
	ToolName string
	// TaskID is the model-provided tool_use id. It is stable across
	// pending, in_progress, and terminal updates for one invocation, so
	// presentation layers can update a task in place instead of
	// appending duplicate rows.
	TaskID string
	// State is the lifecycle state for a tool invocation. Empty on
	// legacy/non-tool phases such as synthesizing.
	State StatusState
	// Emoji is the slack emoji name (no colons) from the tool's
	// Status() call. Empty for unknown tools or non-tool phases.
	Emoji string
	// Verb is the short present-tense phrase from the tool's
	// Status() call. Empty for unknown tools or non-tool phases.
	Verb string
	// Summary is the per-invocation detail from the tool's Status()
	// call (e.g. the search query). Empty when nothing useful is
	// available.
	Summary string
	// Output is a concise result summary produced after a successful tool
	// call. Progress surfaces can add it to the same task that was created
	// by the in-progress update.
	Output string
	// Sources are URL references produced by a completed tool call.
	// Tools derive these from their own typed output; surfaces validate
	// and limit them for their transport.
	Sources []StatusSource
}

// StatusSource is a URL reference suitable for progress surfaces.
type StatusSource struct {
	URL   string
	Label string
}

// StatusState is the surface-agnostic lifecycle of one tool invocation.
// Values intentionally match Slack's task_update status vocabulary.
type StatusState string

const (
	StatusPending    StatusState = "pending"
	StatusInProgress StatusState = "in_progress"
	StatusComplete   StatusState = "complete"
	StatusError      StatusState = "error"
)

// StatusEmitter is the per-call callback the runner invokes at
// progress boundaries. nil-safe — internal code MUST guard with a
// nil-check before calling. Per-call (not Runner-field) because
// concurrent ask turns from different Slack threads each need their
// own emitter; a shared Runner field would race.
type StatusEmitter func(StatusUpdate)

// Citation is one source attribution surfaced by the runner alongside
// a final reply. Built from the union of retrieve.Hit results across
// all retrieve calls in a turn, deduped by (Source, ExternalID),
// keeping the highest-scoring hit per key. Capped at maxCitations
// after sort by Score desc.
type Citation struct {
	Source     string         `json:"source"`
	ExternalID string         `json:"external_id"`
	URL        string         `json:"url"`
	Actor      identity.Actor `json:"actor"`
	OccurredAt time.Time      `json:"occurred_at"`
	// Score is retrieve.Hit.Score. Load-bearing for dedup
	// (highest-wins) and ordering (sort desc before cap). Not
	// rendered to the user.
	Score float32 `json:"score"`
}

const maxCitations = 10

// citationAccumulator collects retrieve.Hit projections during a
// turn. Read by the runner after the loop; written by RetrieveTool
// during dispatch. Stored in ctx via withCitations / fromContext.
//
// Eino may execute independent tool calls concurrently, so both writes and
// reads are synchronized. Readers work from snapshots and never retain the
// accumulator's backing slice.
type citationAccumulator struct {
	// sources project the citations' actors.
	sources source.Infos

	mu   sync.Mutex
	hits []retrieve.Hit
}

func (a *citationAccumulator) add(hits []retrieve.Hit) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hits = append(a.hits, hits...)
}

func (a *citationAccumulator) snapshot() []retrieve.Hit {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]retrieve.Hit(nil), a.hits...)
}

// finalize collapses the accumulated hits into a Citation list,
// deduplicated and capped.
func (a *citationAccumulator) finalize() []Citation {
	hits := a.snapshot()
	if len(hits) == 0 {
		return nil
	}
	byKey := make(map[string]Citation, len(hits))
	for _, h := range hits {
		key := h.Source + "\x00" + h.ExternalID
		c := Citation{
			Source:     h.Source,
			ExternalID: h.ExternalID,
			URL:        h.URL,
			Actor:      identity.FromEvent(a.sources.Of(h.Source), h.Actor, h.ActorCanonical, h.ActorID),
			OccurredAt: h.OccurredAt,
			Score:      h.Score,
		}
		// Existence check is load-bearing: a first-seen citation
		// with Score <= 0 (cosine similarity is bounded in [-1,1]
		// and degenerate queries can drift below zero) would
		// otherwise lose to the zero-value Citation in the map.
		if existing, ok := byKey[key]; !ok || h.Score > existing.Score {
			byKey[key] = c
		}
	}
	out := make([]Citation, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > maxCitations {
		out = out[:maxCitations]
	}
	return out
}

// citationCtxKey is the unexported context key for the per-turn
// accumulator. Tools fish it back out via fromContext.
type citationCtxKey struct{}

func withCitations(ctx context.Context, a *citationAccumulator) context.Context {
	return context.WithValue(ctx, citationCtxKey{}, a)
}

// citationsFromContext returns the per-turn accumulator if one is
// installed. Tools that retrieve hits should append to this; if no
// accumulator is installed (e.g. HTTP path that doesn't surface
// citations), the call is a no-op (a.add nil-checks).
func citationsFromContext(ctx context.Context) *citationAccumulator {
	a, _ := ctx.Value(citationCtxKey{}).(*citationAccumulator)
	return a
}
