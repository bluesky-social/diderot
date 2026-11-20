package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/source"
)

// =====================================================================
// listTool: enumeration over the events store
// =====================================================================
//
// The model's "for each X, do Y" tool. Filter-shaped (no question), no
// vector search, no qdrant. Backed by retrieve.List, which is one
// indexed Postgres SELECT.
//
// Rationale: vector retrieve is
// silently lossy for map-shaped queries — top_k can drop events with
// no signal to the user, cosine to a generic question scrambles
// recency, chunks-not-events granularity, no monotone pagination.
// `list` exists so the model has a tool with the right contract for
// "summarize each of alex's threads last week" instead of faking
// it through a degraded retrieve.

// listSchema is the JSON schema shown to the model. The decision rule is
// in the tool description (listToolDescription), not the schema, so
// it stays in
// the cached prefix once. Keep this compact — every property costs
// prompt-cache tokens.
//
// No "question" property. At least one of sources/actors/occurred_after/
// occurred_before is required server-side; we say so in the tool
// description so the model surfaces a useful error in-chat instead of
// just retrying.
//
// The sources filter offers every registered source, and each source's tool
// description says what it holds, as in retrieve's.
func listSchema(sources source.Infos) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "sources": {
      "type": "array",
      "items": {"type": "string", "enum": ` + jsonText(sources.EventSources()) + `},
      "description": ` + jsonText(sourcesDescription("Limit the list to these sources.", sources)) + `
    },
    "actors": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Limit the list to events by these people. Give each as a full email address (the canonical id of one of their results, or, in Ask, asker.email from the context block): an email matches the same person in every source. A display name matches only if it is exactly the name the source stores, so a first name alone usually matches nothing, and a filter that matches no one returns no rows and no error."
    },
    "channels": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Limit the list to messages in these Slack or Discord channels. Each entry is a channel name (with or without '#'), a channel ID, or a link to the channel or one of its messages. A Discord channel includes its threads and forum posts. A channel that does not exist, or that the caller cannot read, is an error."
    },
    "occurred_after": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 time. Only events at or after it."
    },
    "occurred_before": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 time. Only events at or before it."
    },
    "top_k": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "default": 25,
      "description": "How many events to return."
    },
    "thread_roots_only": {
      "type": "boolean",
      "default": false,
      "description": "For slack and discord: return only top-level messages (thread roots and messages with no thread), one row per conversation, leaving out replies."
    },
    "session_level_only": {
      "type": "boolean",
      "default": false,
      "description": "For agent_session: return one row per session (its whole-session summary), leaving out the rows for each stretch of a session."
    },
    "cursor": {
      "type": "string",
      "description": "The next_cursor of the previous response, passed unchanged with the same filters, to get the next page. Leave it unset for the first page."
    }
  }
}`)
}

// listToolDescription is the model-facing description of `list`: when it,
// not retrieve, is the right tool, what a call needs, and how to page and
// read a channel.
const listToolDescription = "List every event that matches a set of filters, newest first, with no relevance ranking. Use it when the question is about a set and not a topic: what a person did in a period, every agent session on a day, each thread in a channel last week. To find the best matches for a topic, use `retrieve`.\n\n" +
	"At least one of `sources`, `actors`, `channels`, `occurred_after` or `occurred_before` is required. Each row is a whole event, not a passage, with its content cut to about 2,000 characters, or about 1,100 when a page holds 50 rows; `truncated` marks a cut row. For slack, discord, notion, linear and zendesk, `expand` returns the whole item. A cut agent-session summary cannot be completed that way (`expand` returns the session itself), so ask for 25 rows or fewer to get more of each summary. When more events match than fit in one response it carries `next_cursor`: pass that back as `cursor`, with the same filters, for the next page. A response that carries `next_cursor` is not the whole set. Narrow the filters before paging far.\n\n" +
	"To read a Slack or Discord channel, pass `channels` with a time range. Messages come newest first, up to 50 per call; a reply has `is_reply: true` and shares its root's `thread_id`. In a busy channel or over a long range, set `thread_roots_only` to get one row per conversation, then `expand` the threads that matter. This is also the way to find a message someone remembers by who posted it, where and roughly when, which `retrieve` cannot do.\n\n" +
	"For agent sessions, set `session_level_only` to get one row per session and not one per stretch of each session. `expand` on a session-level row returns the whole session when it is short and otherwise an outline of its stretches, each with an `event_id` to expand.\n\n" +
	"In each row, " + resultIdentityDescription

// listInput mirrors the JSON the model produces. TopK is json.Number
// for the same reason retrieveInput.TopK is — Claude sometimes ships
// numeric tool args as strings even with "type: integer" in the
// schema. The bool fields stay plain `bool` because we haven't seen
// Claude type-confuse booleans the way it does with integers; if it
// ever does, the failure mode is a clear unmarshal error the model
// can recover from on retry, not a silent value mismatch.
type listInput struct {
	Sources          []string    `json:"sources,omitempty"`
	Actors           []string    `json:"actors,omitempty"`
	Channels         []string    `json:"channels,omitempty"`
	OccurredAfter    string      `json:"occurred_after,omitempty"`
	OccurredBefore   string      `json:"occurred_before,omitempty"`
	TopK             json.Number `json:"top_k,omitempty"`
	ThreadRootsOnly  bool        `json:"thread_roots_only,omitempty"`
	SessionLevelOnly bool        `json:"session_level_only,omitempty"`
	Cursor           string      `json:"cursor,omitempty"`
}

// listOutputHit is the per-row shape the model sees. Mirrors
// retrieveOutputHit's keys for cross-tool consistency, minus the
// fields that don't apply to list (no chunk_id since list returns
// events; no score since there's no ranking; has_thread becomes
// load-bearing for the "expand for replies" decision).
type listOutputHit struct {
	Source     string         `json:"source"`
	EventID    int64          `json:"event_id"`
	ExternalID string         `json:"external_id,omitempty"`
	Actor      identity.Actor `json:"actor"`
	// Channel is the slack channel name for slack hits (empty for
	// non-slack sources). Use this verbatim — never invent a name
	// from the URL.
	Channel    string `json:"channel,omitempty"`
	OccurredAt string `json:"occurred_at"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	Content    string `json:"content"`
	HasThread  *bool  `json:"has_thread,omitempty"`
	// ThreadID and IsReply place a slack message in its thread: a root and
	// its replies share ThreadID, and IsReply marks the replies.
	ThreadID  string `json:"thread_id,omitempty"`
	IsReply   bool   `json:"is_reply,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type listOutput struct {
	Hits []listOutputHit `json:"hits"`
	// Count is the actual number of hits returned; the model can
	// see "I asked for 50 but got 8" without scanning the array.
	Count int `json:"count"`
	// NextCursor, when non-empty, means more events match beyond
	// this page; the model passes it back as `cursor` (same
	// filters) to continue.
	NextCursor string `json:"next_cursor,omitempty"`
}

func listStatusSources(output json.RawMessage, sources source.Infos) []StatusSource {
	var out listOutput
	if json.Unmarshal(output, &out) != nil {
		return nil
	}
	labels := make([]StatusSource, 0, len(out.Hits))
	for _, hit := range out.Hits {
		if hit.URL == "" {
			continue
		}
		label := hit.Title
		if label == "" && hit.Channel != "" {
			label = "#" + strings.TrimPrefix(hit.Channel, "#")
		}
		if label != "" && hit.Actor.Display != "" {
			label += " · " + hit.Actor.Display
		}
		if label == "" {
			label = resultNoun(sources, hit.Source)
		}
		labels = append(labels, StatusSource{URL: hit.URL, Label: label})
	}
	return labels
}

func listStatusOutput(output json.RawMessage) string {
	var out listOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	result := formatStatusCount(out.Count, "matching event", "matching events")
	if out.NextCursor != "" {
		result += "; more available"
	}
	return result
}

func listStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "card_index_dividers", Verb: "listing"}
	var probe struct {
		Actor    string   `json:"actor"`
		Actors   []string `json:"actors"`
		Source   string   `json:"source"`
		Sources  []string `json:"sources"`
		Channels []string `json:"channels"`
	}
	if err := json.Unmarshal(input, &probe); err != nil {
		return sl
	}
	var parts []string
	actor := probe.Actor
	if actor == "" && len(probe.Actors) > 0 {
		actor = probe.Actors[0]
	}
	if actor != "" {
		parts = append(parts, "actor="+actor)
	}
	src := probe.Source
	if src == "" && len(probe.Sources) > 0 {
		src = probe.Sources[0]
	}
	if src != "" {
		parts = append(parts, "source="+src)
	}
	if len(probe.Channels) > 0 {
		parts = append(parts, "channel="+probe.Channels[0])
	}
	if len(parts) > 0 {
		sl.Detail = truncateStatusDetail(strings.Join(parts, " "))
	}
	return sl
}

// listRequestFromInput projects the model-facing JSON input (a
// listInput) into the shared retrieve.ListRequest, injecting the
// turn's visibility boundary from ctx.
func listRequestFromInput(ctx context.Context, in listInput, sources source.Infos) (retrieve.ListRequest, error) {
	topK := 0
	if in.TopK != "" {
		n, err := in.TopK.Int64()
		if err != nil {
			return retrieve.ListRequest{}, fmt.Errorf("top_k: %w", err)
		}
		topK = int(n)
	}

	req := retrieve.ListRequest{
		Sources:          in.Sources,
		Actors:           in.Actors,
		Channels:         in.Channels,
		TopK:             topK,
		ThreadRootsOnly:  in.ThreadRootsOnly,
		SessionLevelOnly: in.SessionLevelOnly,
		ThreadedSources:  sources.Threaded(),
		Cursor:           in.Cursor,
		// Visibility boundary, injected server-side from the turn
		// ctx — not part of the model-facing schema, so the model
		// can narrow but never widen. nil or empty = no access: List
		// matches events.scope against this set and nothing else.
		AllowedScopes:        scopesFromContext(ctx),
		ExcludeConversations: conversationsBeingAnswered(ctx),
	}
	if in.OccurredAfter != "" {
		t, err := time.Parse(time.RFC3339, in.OccurredAfter)
		if err != nil {
			return retrieve.ListRequest{}, fmt.Errorf("occurred_after: %w", err)
		}
		req.OccurredAfter = t
	}
	if in.OccurredBefore != "" {
		t, err := time.Parse(time.RFC3339, in.OccurredBefore)
		if err != nil {
			return retrieve.ListRequest{}, fmt.Errorf("occurred_before: %w", err)
		}
		req.OccurredBefore = t
	}
	return req, nil
}

// listOutputFromResult projects retrieve.List results into the
// model-facing listOutput shape.
func listOutputFromResult(res *retrieve.Result, sources source.Infos) listOutput {
	out := listOutput{
		Hits:       make([]listOutputHit, 0, len(res.Hits)),
		Count:      len(res.Hits),
		NextCursor: res.NextCursor,
	}
	for _, h := range res.Hits {
		info := sources.Of(h.Source)
		threadID := ""
		if info.Threads {
			threadID = h.ThreadID
		}
		out.Hits = append(out.Hits, listOutputHit{
			Source:     h.Source,
			EventID:    h.EventID,
			ExternalID: h.ExternalID,
			Actor:      identity.FromEvent(info, h.Actor, h.ActorCanonical, h.ActorID),
			Channel:    h.ChannelName,
			OccurredAt: h.OccurredAt.Format(time.RFC3339),
			URL:        h.URL,
			Title:      h.Title,
			Content:    h.Content,
			HasThread:  hasThreadFor(info, h.ThreadID),
			ThreadID:   threadID,
			IsReply:    threadID != "" && threadID != h.ExternalID,
			Truncated:  isTruncated(h.Content),
		})
	}
	return out
}

// runList is the typed business operation behind the native Eino
// `list` tool: it decodes the typed input, runs the shared retriever,
// pushes hits into the citation accumulator, and projects the result
// into the model-facing listOutput shape.
func runList(ctx context.Context, retriever CorpusRetriever, sources source.Infos, in listInput) (listOutput, error) {
	req, err := listRequestFromInput(ctx, in, sources)
	if err != nil {
		return listOutput{}, err
	}
	if retriever == nil {
		return listOutput{}, errors.New("list tool: no retriever configured")
	}
	res, err := retriever.List(ctx, req)
	if err != nil {
		return listOutput{}, fmt.Errorf("list: %w", err)
	}
	if acc := citationsFromContext(ctx); acc != nil {
		acc.add(res.Hits)
	}
	return listOutputFromResult(res, sources), nil
}

// NewEinoListTool builds the native Eino `list` tool from the shared
// retrieve.Retriever and wires the presentation metadata (status
// line, sources, safe output summary) in as callbacks. The native
// tool decodes arguments and encodes results itself (utils.NewTool);
// it never routes through a JSON adapter. sources are the registered
// sources the tool can filter by.
func NewEinoListTool(r CorpusRetriever, sources source.Infos) RegisteredTool {
	presentation := ToolPresentation{
		Status: func(input json.RawMessage) StatusLine {
			return listStatus(input)
		},
		StatusSources: func(output json.RawMessage) []StatusSource {
			return listStatusSources(output, sources)
		},
		StatusOutput: func(output json.RawMessage) string {
			return listStatusOutput(output)
		},
	}

	// The model-facing schema is the listSchema document verbatim
	// (enum values, min/max/defaults, per-property descriptions, and
	// the required-filters contract), carried as an Eino JSON Schema.
	js, err := parseToolJSONSchema(listSchema(sources))
	if err != nil {
		// The schema is fixed text around the registered sources' names;
		// a parse failure here is a programming error, not a runtime
		// condition.
		panic(fmt.Sprintf("list: parse model schema: %v", err))
	}
	info := &schema.ToolInfo{
		Name:        "list",
		Desc:        listToolDescription,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
	invoke := func(ctx context.Context, in listInput) (listOutput, error) {
		return runList(ctx, r, sources, in)
	}

	einoTool := utils.NewTool(info, invoke)
	return RegisteredTool{Tool: einoTool, Presentation: presentation}
}

// isTruncated checks for the trailing marker truncateRunes appends
// to clipped content. Cheap suffix check; the marker is a constant.
func isTruncated(content string) bool {
	const marker = "…(truncated)"
	if len(content) < len(marker) {
		return false
	}
	return content[len(content)-len(marker):] == marker
}
