package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/source"
)

// statusDetailCap is the maximum rune length for StatusLine.Detail.
// All Status() implementations must truncate Detail to this cap.
const statusDetailCap = 80

// truncateStatusDetail rune-truncates s to statusDetailCap, appending
// "…" when the string is shortened. It also sanitizes the characters
// that break the slack rendering: Detail is interpolated into an
// inline-code span (`...`) in the status placeholder, so a literal
// backtick would close the span early and a newline would inject a
// fake status line. Backticks become apostrophes, newlines become
// spaces — a slight visual lie about the model's exact input, but
// the faithful version is markup breakage. Centralized here because
// every Status() implementation routes Detail through this helper.
func truncateStatusDetail(s string) string {
	s = strings.NewReplacer("`", "'", "\n", " ", "\r", " ").Replace(s)
	r := []rune(s)
	if len(r) <= statusDetailCap {
		return s
	}
	return string(r[:statusDetailCap-1]) + "…"
}

// StatusLine describes how one tool invocation renders in
// operator-facing progress logs (the slack placeholder transcript).
// Emoji is a slack emoji name (no colons); Verb is a short
// present-tense phrase; Detail is the per-invocation summary and
// may be empty. Implementations must rune-truncate Detail to
// statusDetailCap.
type StatusLine struct {
	Emoji  string
	Verb   string
	Detail string
}

// NewEinoRetrieveTool builds the native Eino `retrieve` tool from the
// shared retrieve.Retriever and wires the presentation metadata
// (status line, sources, safe output summary) in as callbacks. The
// native tool decodes arguments and encodes results itself
// (utils.NewTool); it never routes through a JSON adapter. sources are
// the registered sources the tool can filter by.
func NewEinoRetrieveTool(r CorpusRetriever, sources source.Infos) RegisteredTool {
	info := &schema.ToolInfo{Name: "retrieve", Desc: retrieveToolDescription + askRetrieveCoverageNote}
	invoke := func(ctx context.Context, in retrieve.RequestInput) (retrieveOutput, error) {
		req, err := retrieve.RequestFromInput(in, scopesFromContext(ctx))
		if err != nil {
			return retrieveOutput{}, err
		}
		req.Filters.ExcludeConversations = conversationsBeingAnswered(ctx)
		out, err := runRetrieve(ctx, r, sources, req)
		if err != nil {
			return out, err
		}
		out.NotSearched = sourceCoverageFromContext(ctx).searched(in.Sources, sources.EventSources())
		return out, nil
	}

	// The model-facing schema is the retrieveSchema document verbatim
	// (per-property descriptions, enums, min/max/default, and the
	// required-question contract), carried as an Eino JSON Schema.
	js, err := parseToolJSONSchema(retrieveSchema(sources))
	if err != nil {
		// The schema is fixed text around the registered sources' facts;
		// a parse failure here is a programming error, not a runtime
		// condition.
		panic(fmt.Sprintf("retrieve: parse model schema: %v", err))
	}
	info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(js)
	einoTool := utils.NewTool(info, invoke)
	return RegisteredTool{
		Tool: einoTool,
		Presentation: ToolPresentation{
			Status: func(input json.RawMessage) StatusLine {
				return retrieveStatus(input)
			},
			StatusSources: func(output json.RawMessage) []StatusSource {
				return retrieveStatusSources(output, sources)
			},
			StatusOutput: func(output json.RawMessage) string {
				return retrieveStatusOutput(output)
			},
		},
	}
}

// CorpusRetriever is the shared read-only corpus surface used by the native
// Ask tools and the MCP server. *retrieve.Retriever is the production
// implementation; the interface keeps both transports independently testable.
type CorpusRetriever interface {
	Retrieve(ctx context.Context, req retrieve.Request) (*retrieve.Result, error)
	List(ctx context.Context, req retrieve.ListRequest) (*retrieve.Result, error)
}

// resultIdentityDescription explains the two fields every corpus result
// shares and that mean something different in each source. retrieve and
// list both carry it, so an MCP client that reads only one of them still
// learns how to attribute and date a result.
const resultIdentityDescription = "`actor` is an object: `display` is the name to use in prose; `canonical` ({namespace,id}) is the person's identity across sources, and when its namespace is `email` its id is the value to pass back in an `actors` filter, copied exactly; `source_identity` ({namespace,id}) is the source's own id for the person; and `role` says what the person is to the item: message_author for slack and discord (the sharer, for a file or an image), session_owner for agent_session, last_editor for notion (who may not be the author), issue_creator for linear, and event_actor for zendesk (the ticket's requester, a comment's author or an article's author). `occurred_at` is when a slack or discord message was posted. A discord actor's canonical identity is the Discord account (namespace `discord_user`), never an email, so it does not match the same person in other sources. For notion and linear it is the last update, which does not date the events the text describes. For agent_session, a chunk result is dated by the last record of its stretch and a session-level result by the session's last activity."

// parseToolJSONSchema decodes a JSON-schema document (a full JSON
// Schema object) into Eino's jsonschema.Schema form so a native tool
// can carry the exact model-facing contract: per-property
// descriptions, enums, min/max/defaults, and required lists.
func parseToolJSONSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	js := &jsonschema.Schema{}
	if err := json.Unmarshal(raw, js); err != nil {
		return nil, fmt.Errorf("decode model schema: %w", err)
	}
	return js, nil
}

// sessionURLPad is the per-side message-index padding applied to
// agent_session chunk URLs. A chunk covering [start, end) gets
// rewritten to [start - pad, end + pad] so a click-through shows
// additional rounds of context around the chunk. Chosen as a
// trade-off between "see enough surrounding turns" and "URL stays
// compact in slack rendering."
const sessionURLPad = 20

// padAgentSessionURL widens a session viewer chunk-range URL by
// sessionURLPad messages on each side, leaving non-chunk URLs
// (session-level, no range, only one of start/end, malformed
// numerics, not a session URL) untouched. The function is a string-in /
// string-out projection — it doesn't validate the host or path,
// only the query-param shape, because the URL is whatever ingest
// stored and the session viewer's URL format is the
// authoritative input. We only intervene when both `start` and
// `end` parse as ints.
//
// `start` clamps to 0 (so chunks near the session beginning don't
// produce negative ranges); `end` is allowed to overshoot the
// session length because the viewer's range already clamps
// to the session's last message.
func padAgentSessionURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	startRaw := q.Get("start")
	endRaw := q.Get("end")
	if startRaw == "" || endRaw == "" {
		return raw
	}
	start, err := strconv.Atoi(startRaw)
	if err != nil {
		return raw
	}
	end, err := strconv.Atoi(endRaw)
	if err != nil {
		return raw
	}
	newStart := start - sessionURLPad
	if newStart < 0 {
		newStart = 0
	}
	newEnd := end + sessionURLPad
	q.Set("start", strconv.Itoa(newStart))
	q.Set("end", strconv.Itoa(newEnd))
	u.RawQuery = q.Encode()
	return u.String()
}

// retrieveToolDescription is the model-facing description of `retrieve`:
// what it does and when to use it, how to phrase a query, and what each
// field of a hit means. Each source's own description is in the `sources`
// parameter, from the source registry.
const retrieveToolDescription = "Semantic search over the organization's corpus. Returns the passages that best match `question`, each with its source, actor, time and, when it has one, a url. Use it to find what was said, decided or done about a topic. To enumerate everything that matches a filter, or to read a Slack or Discord channel over a period, use `list`.\n\n" +
	"The search ranks by meaning, not by matching words, so describe what you are looking for in the words the content would use. Quotation marks, minus signs, boolean operators and filters written into `question` do nothing: restrict a search with `sources`, `actors` and the time parameters. With `sources` unset, or naming several, each source is ranked on its own and the best `top_k` of each come back, so a small source is not crowded out by a large one: begin with that. Name one source to get more from it. The nearest passages are always returned, however weak, so a hit is not proof of relevance: judge it by its text. An empty result means the filters excluded everything, most often an `actors` value that matches no one or a time bound: loosen them. It never shows that the thing does not exist.\n\n" +
	"Each hit carries:\n" +
	"- `event_id`: pass it to `expand` to read the full source.\n" +
	"- " + resultIdentityDescription + "\n" +
	"- `channel` (slack): the channel's name; a discord hit's `title` names its channel and thread. `has_thread` (slack, discord): what `expand` returns, the whole thread when true and the surrounding messages in the channel when false.\n" +
	"- `granularity` (agent_session): `chunk` for a summary of one stretch of a session, `session` for a summary of a whole session. Expanding a chunk returns that stretch of the session itself, which is where exact commands and output are. Expanding a session-level hit returns the whole session when it is short and otherwise an outline of its stretches, each with an `event_id` to expand.\n" +
	"- `content_kind`: present when the text is not the actor's own words. `image_caption` is a model's description of an image they shared. `document_text` is text extracted from a file they attached: `document` names the file, and `document_completeness: partial` means part of it is missing (`document_pages` and `document_pages_extracted` give the coverage). Attribute such text to the image or the file (\"contract.pdf, shared by Sam in #legal\"), not to the actor."

// askRetrieveCoverageNote is added to the description Ask reads. An Ask turn
// keeps a record of the sources it has searched (sourceCoverage); a call
// over MCP is not part of a turn and gets no such field.
const askRetrieveCoverageNote = "\n\nBeside `hits`, `not_searched` lists the sources that no search of this turn has covered yet. It is absent when there are none. Nothing can be called absent from the corpus while it is there; a search with `sources` unset clears it."

// retrieveSchema is the JSON schema shown to the model. Kept compact —
// every property here costs prompt-cache tokens for the lifetime of
// the conversation. Filters mirror retrieve.Filters. The sources filter
// offers every registered source, enabled or not: a source that is off
// still has its events in the corpus. Each source's tool description
// says what it holds.
func retrieveSchema(sources source.Infos) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "What to look for, as a descriptive phrase or sentence with the distinctive terms and enough context to tell it apart. A one-word query retrieves poorly."
    },
    "sources": {
      "type": "array",
      "items": {"type": "string", "enum": ` + jsonText(sources.EventSources()) + `},
      "description": ` + jsonText(sourcesDescription("Limit the search to these sources. With none given, every source is searched, each ranked on its own.", sources)) + `
    },
    "actors": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Limit the search to events by these people. Give each as a full email address (the canonical id of one of their results, or, in Ask, asker.email from the context block): an email matches the same person in every source. The source's own id for a person (source_identity.id of one of their results) also matches, in that source only. A display name does not match here: use list to filter by an exact display name, or take the email from one of the person's results. A filter that matches no one returns no hits and no error."
    },
    "occurred_after": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 time. Only events from this time on. Bound a search by time only when the question names a period or needs recent material, because a bound hides older content that may still hold."
    },
    "occurred_before": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 time. Only events up to this time."
    },
    "top_k": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "description": "How many passages to return. With one source named: that many from it, 20 by default. With several or none: that many from each source, 5 by default and 10 at most."
    }
  },
  "required": ["question"]
}`)
}

// sourcesDescription is a sources parameter's description: lead, then each
// registered source's tool description.
func sourcesDescription(lead string, sources source.Infos) string {
	parts := []string{lead}
	for _, info := range sources {
		parts = append(parts, "'"+info.EventSource+"' = "+info.ToolDescription)
	}
	return strings.Join(parts, " ")
}

// jsonText encodes a schema value spliced into a schema document's text.
func jsonText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode schema value: %v", err))
	}
	return string(encoded)
}

// retrieveOutputHit is what we hand back to the model. Minimal v1
// shape: source / actor / occurred_at / url / content / score, plus
// event_id (the handle internal to Diderot that the model passes to the
// `expand` tool to fetch full source content) and external_id (the
// source-side stable id, useful for human-readable identification
// in answers).
//
// HasThread is slack-specific signal that helps the model decide what
// shape an `expand` call would return: true ⇒ expand returns the
// full thread; false ⇒ expand returns ±20 messages / ±30 minutes of
// channel-adjacent context (the neighbor-window mode). Pointer-to-
// bool is load-bearing: a plain bool with `omitempty` would never
// emit `has_thread: false` (Go's encoding/json drops the zero value),
// making slack standalones indistinguishable from agent_session
// hits. With *bool, nil ⇒ field omitted (agent_session path);
// &false ⇒ emitted explicitly (slack standalone); &true ⇒ emitted
// (slack threaded).
type retrieveOutputHit struct {
	Source     string         `json:"source"`
	EventID    int64          `json:"event_id"`
	ExternalID string         `json:"external_id,omitempty"`
	Actor      identity.Actor `json:"actor"`
	// Channel is the slack channel name (e.g. "eng-platform") for
	// slack hits. Absent for non-slack sources. Use this verbatim
	// when describing where a slack message came from — never
	// invent a channel name from the URL or external_id; only this
	// field (or the empty value) is authoritative.
	Channel    string  `json:"channel,omitempty"`
	Title      string  `json:"title,omitempty"`
	OccurredAt string  `json:"occurred_at"`
	URL        string  `json:"url,omitempty"`
	Content    string  `json:"content"`
	Score      float32 `json:"score"`
	HasThread  *bool   `json:"has_thread,omitempty"`
	// Granularity distinguishes the two agent_session summary
	// tiers that otherwise look identical: "session" for the
	// whole-session narrative event, "chunk" for a per-range chunk
	// summary. Both compete as independent vector points, so a
	// result list can interleave them; without this field the only
	// tell is the external_id suffix. Absent for other sources.
	Granularity string `json:"granularity,omitempty"`
	// ContentKind marks where this chunk's text came from when it is
	// NOT the author's own words: "image_caption" (a vision model's
	// description of an attached image) or "document_text" (text
	// extracted from an attached document). Absent for ordinary message
	// or page text.
	//
	// This exists to prevent a specific misattribution: without it, text
	// extracted from an attached PDF looks exactly like something the
	// poster typed, and the model will write "Chester said retention is
	// 18 months" when Chester in fact attached a contract someone else
	// wrote. Document is the filename to cite instead.
	ContentKind string `json:"content_kind,omitempty"`
	Document    string `json:"document,omitempty"`
	// DocumentPages is the source document's page count, when known.
	DocumentPages          int    `json:"document_pages,omitempty"`
	DocumentPagesExtracted int    `json:"document_pages_extracted,omitempty"`
	DocumentCompleteness   string `json:"document_completeness,omitempty"`
	DocumentIncompleteWhy  string `json:"document_incomplete_reason,omitempty"`
}

// retrieveOutput is the wrapper. {"hits": [...]} so we can add
// non-hit fields later (e.g. truncation indicators) without the
// model breaking.
type retrieveOutput struct {
	Hits []retrieveOutputHit `json:"hits"`
	// NotSearched lists the sources that no search of this turn has covered
	// yet. It is absent once a search with no source filter has run, or
	// every source has been searched by name.
	NotSearched []string `json:"not_searched,omitempty"`
}

func retrieveStatusSources(output json.RawMessage, sources source.Infos) []StatusSource {
	var out retrieveOutput
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

func retrieveStatusOutput(output json.RawMessage) string {
	var out retrieveOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	return formatStatusCount(len(out.Hits), "relevant result", "relevant results")
}

func retrieveStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "mag", Verb: "searching"}
	var probe struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || probe.Question == "" {
		return sl
	}
	sl.Detail = truncateStatusDetail(probe.Question)
	return sl
}

// runRetrieve is the typed business operation behind the native Eino
// tool. It validates the typed model-facing input, runs the shared
// retriever, pushes hits into the citation accumulator, and projects
// the result into the model-facing retrieveOutput shape.
func runRetrieve(ctx context.Context, retr CorpusRetriever, sources source.Infos, req retrieve.Request) (retrieveOutput, error) {
	if retr == nil {
		return retrieveOutput{}, errors.New("retrieve tool: no retriever configured")
	}
	res, err := retr.Retrieve(ctx, req)
	if err != nil {
		return retrieveOutput{}, fmt.Errorf("retrieve: %w", err)
	}

	// Push hits into the per-turn citation accumulator (if one is
	// installed in ctx). The Slack consumer surfaces citations in
	// Block Kit; the HTTP path leaves the accumulator nil and this
	// is a no-op. Done before constructing the model-facing output
	// so the operator-visible side channel matches what the model
	// saw, even if we trim the model's view differently in the
	// future.
	if acc := citationsFromContext(ctx); acc != nil {
		acc.add(res.Hits)
	}

	return retrieveOutputFromResult(res, sources), nil
}

// retrieveOutputFromResult projects retrieve.Result hits into the
// model-facing retrieveOutput shape.
func retrieveOutputFromResult(res *retrieve.Result, sources source.Infos) retrieveOutput {
	out := retrieveOutput{Hits: make([]retrieveOutputHit, 0, len(res.Hits))}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, retrieveOutputHit{
			Source:      h.Source,
			EventID:     h.EventID,
			ExternalID:  h.ExternalID,
			Actor:       identity.FromEvent(sources.Of(h.Source), h.Actor, h.ActorCanonical, h.ActorID),
			Channel:     h.ChannelName,
			Title:       h.Title,
			OccurredAt:  h.OccurredAt.Format(time.RFC3339),
			URL:         padAgentSessionURL(h.URL),
			Content:     h.Content,
			Score:       h.Score,
			HasThread:   hasThreadFor(sources.Of(h.Source), h.ThreadID),
			Granularity: granularityFor(h.Source, h.ExternalID),
			// Only emitted for derived content; plain 'text' chunks stay
			// clean so the common case gains no tokens.
			ContentKind:            derivedKind(h.Kind),
			Document:               h.DocumentFilename,
			DocumentPages:          h.DocumentPageCount,
			DocumentPagesExtracted: h.DocumentPagesExtracted,
			DocumentCompleteness:   h.DocumentCompleteness,
			DocumentIncompleteWhy:  h.DocumentIncompleteWhy,
		})
	}
	return out
}

// derivedKind returns the chunk kind when the content is derived rather
// than authored, and "" for ordinary text. Keeping the field absent on
// the overwhelmingly common 'text' case costs the model nothing to
// ignore and keeps the payload small.
func derivedKind(kind string) string {
	switch kind {
	case "image_caption", "document_text":
		return kind
	default:
		return ""
	}
}

func formatStatusCount(count int, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return fmt.Sprintf("Found %d %s", count, label)
}

// resultNoun is the progress line's name for one result of eventSource:
// its source's ResultNoun, or the event source itself for a source no build
// registers.
func resultNoun(sources source.Infos, eventSource string) string {
	if noun := sources.Of(eventSource).ResultNoun; noun != "" {
		return noun
	}
	return eventSource
}

// granularityFor classifies an agent_session hit by its summary
// tier. The two tiers share one source and one embedding space; the
// external_id shape is the only structural difference:
// "<session_id>:session" for the whole-session narrative,
// "<session_id>:<record_start>:<record_end>" for a chunk summary (see
// the session-summary publisher). Non-session sources return "" so the
// field stays absent from their JSON.
func granularityFor(source, externalID string) string {
	if source != "agent_session" {
		return ""
	}
	if strings.HasSuffix(externalID, ":session") {
		return "session"
	}
	return "chunk"
}

// hasThreadFor projects a hit's thread into the model-facing has_thread
// flag, for a source whose events form conversations (Info.Threads): whether
// the event is part of a thread, as a root with replies or a reply. Other
// sources leave the field absent.
func hasThreadFor(info source.Info, threadID string) *bool {
	if !info.Threads {
		return nil
	}
	v := threadID != ""
	return &v
}
