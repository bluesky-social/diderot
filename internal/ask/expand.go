package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/source"
)

// ScopeLookup resolves an event_id to its visibility scope string
// (events.scope). The concrete impl is events.DBSink.EventScope.
// The expand tool checks the event's scope against the turn's
// allowedScopes set BEFORE fetching content — without this gate,
// expand is a full bypass of the retrieval filter (any event_id ->
// up to 64KiB of raw content).
//
// Missing rows must surface as events.ErrEventNotFound (what
// DBSink returns) or ErrExpandEventNotFound — the gate matches
// both, and MUST: a not-found that falls through to the generic
// error branch returns a hard tool error, which is distinguishable
// from the out-of-scope deny shape and turns expand into an
// existence oracle for private event ids.
type ScopeLookup interface {
	EventScope(ctx context.Context, eventID int64) (string, error)
}

// SourceExpander is the dependency the native `expand` tool consumes. The
// concrete impl in cmd/diderot/expander.go looks the event up through the
// events DBSink and hands it to its source's Expand.
//
// Returning a structured ExpandResult — rather than just bytes —
// lets us add per-source metadata later (e.g. number of messages
// in the slice, total session length) without breaking the wire
// shape with the model.
type SourceExpander interface {
	Expand(ctx context.Context, eventID int64) (ExpandResult, error)
}

// ExpandResult is the in-process shape returned by SourceExpander.
// Source identifies what kind of content lives in Content; future
// sources may add extra fields next to it.
type ExpandResult struct {
	// Source mirrors the events.source of the row — always set so
	// the tool's JSON output makes the source explicit to the
	// model.
	Source string
	// Actor is the model-facing identity of the event that was expanded.
	// Source-specific expanded content can contain additional speakers, but
	// this identifies the retrieved event that authorized the expansion.
	Actor identity.Actor
	// Content is the fetched content. For agent_session this is
	// a redacted session record slice. Future sources
	// MUST also return UTF-8 text; the tool layer rejects non-UTF-8
	// bytes so the model never sees garbled input.
	Content string
	// Truncated is true when the underlying source returned more
	// content than the model-facing budget allows; Content holds
	// the leading slice and the model is told the rest is dropped.
	Truncated bool
	// Attachments are the files of the events Content covers.
	Attachments []events.Attachment
}

// ErrSourceNotExpandable is returned by a SourceExpander when the
// row exists but the source doesn't support expand yet (e.g. slack
// today — the expand-to-thread path is a follow-up). The tool
// translates this into a clean "not supported" tool result rather
// than a tool-layer error so the model can recover and proceed.
var ErrSourceNotExpandable = errors.New("source does not support expand yet")

// ErrExpandEventNotFound is returned by a SourceExpander when no
// row with the given event_id exists.
var ErrExpandEventNotFound = errors.New("event not found")

// expandSchema. Single required field. Kept tight so the
// prompt-cache prefix stays compact.
var expandSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "event_id": {
      "type": "integer",
      "description": "The event_id of a retrieve or list result."
    }
  },
  "required": ["event_id"]
}`)

// The expand description is shared by Ask and MCP except for the sentence
// that says where explicit corpus handles get resolved, which differs by
// surface. Each surface composes head + its own sentence + tail.
const (
	expandToolDescriptionHead = "Read the full source behind one result: pass the `event_id` of a `retrieve` or `list` hit. Use it when the answer depends on something the hit does not show: the rest of a thread, the exact text of a command or an error, a decision further down a page. What comes back depends on the source. For slack: the whole thread, or, for a message with no thread, the messages around it in the channel (up to 20 messages and 30 minutes either side), where a message that started a thread is marked with its reply count and an event_id you can expand in turn. For agent_session: the stretch of the session that a chunk hit summarizes, or, for a session-level hit, the whole session when it is short and otherwise an outline of its stretches, each with a summary and the `event_id` to expand to read that stretch. For notion: the whole page. For linear: the whole issue with its comments. For zendesk: the indexed ticket conversation or article, with the attachment content the asker may see. For discord: the whole thread or forum post, or, for a message outside a thread, the messages around it in its channel and the message it replies to. The result carries the same `actor` object as the hit, and `truncated` when the content was cut to fit. Files attached to the returned messages are listed under `attachments` with name, type, size in bytes and, for an image, its caption."
	expandToolDescriptionTail = "When `supported` is false, `note` says why (the event cannot be read from here, or its source cannot be expanded right now): work from the hit you have, as you should when the call returns an error. Results can run to tens of kilobytes, so expand the hits that matter, not every hit."
)

// expandToolDescription is Ask's model-facing description of `expand`. Ask
// resolves explicit handles in the user's turn before generation.
const expandToolDescription = expandToolDescriptionHead + " Items the question named exactly (a Slack link, an issue key, a session ID) have already been fetched and are in the user's message, so do not expand to find those. " + expandToolDescriptionTail

// expandInput is what the model passes us. event_id is declared as
// an integer in the schema, but Claude periodically ships numbers
// as JSON strings even with strict schemas — accept json.Number
// (as the retrieve tool does for top_k) so the tool doesn't 400
// on a string-shaped int.
type expandInput struct {
	EventID json.Number `json:"event_id"`
}

// expandOutput is what we hand back to the model.
//
// On the supported path: source + content (+ truncated flag if
// content was capped). On the unsupported path: source +
// supported=false + a one-line note. The model's prompt
// instructions tell it to back off and use the chunk summary in
// the unsupported case.
type expandOutput struct {
	Source    string         `json:"source"`
	Actor     identity.Actor `json:"actor"`
	Supported bool           `json:"supported"`
	Note      string         `json:"note,omitempty"`
	Content   string         `json:"content,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	// Attachments lists every file of the returned content.
	Attachments []attachmentInfo `json:"attachments,omitempty"`
}

func expandStatusOutput(output json.RawMessage, sources source.Infos) string {
	var out expandOutput
	if json.Unmarshal(output, &out) != nil {
		return ""
	}
	if !out.Supported {
		if out.Note != "" {
			return truncateStatusDetail(out.Note)
		}
		return "No additional context was available"
	}
	result := "Loaded source context"
	if out.Source != "" {
		result += " from " + resultNoun(sources, out.Source)
	}
	if out.Truncated {
		result += " (truncated)"
	}
	return result
}

// ExpandContentBudget is the cap on how many bytes of expanded
// content we'll hand back to the model in a single tool result.
// 64 KiB is comfortable for a multi-turn ask without blowing the
// context window: at ~3 chars/token that's ~21k tokens worth of
// content, dwarfed by Claude's 200k context but a meaningful
// fraction of a typical ask turn's budget. The model's prompt
// tells it to budget 1-3 expands per question; at 64 KiB each
// that's still <200 KB cumulative.
const ExpandContentBudget = 64 * 1024

// CapExpandContent cuts content to expand's model-facing budget, less the
// reserved bytes a caller has already spent on text that must stay whole,
// walking back to a rune boundary so no partial codepoint ships. It reports
// whether it cut. Exact corpus references share it so a resolved handle is
// bounded the same way an expand result is.
func CapExpandContent(content string, reserved int) (string, bool) {
	limit := max(ExpandContentBudget-reserved, 0)
	if len(content) <= limit {
		return content, false
	}
	for limit > 0 && !utf8.RuneStart(content[limit]) {
		limit--
	}
	return content[:limit], true
}

func expandStatus(input json.RawMessage) StatusLine {
	sl := StatusLine{Emoji: "open_file_folder", Verb: "expanding"}
	var probe struct {
		EventID json.RawMessage `json:"event_id"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || len(probe.EventID) == 0 {
		return sl
	}
	// event_id can come over as a JSON number or a JSON string; trim
	// quotes so both shapes render identically.
	s := strings.Trim(string(probe.EventID), `"`)
	if s == "" {
		return sl
	}
	sl.Detail = truncateStatusDetail("event=" + s)
	return sl
}

// expandFromInput is the single expand execution path behind the
// native Eino tool. It validates the typed model-facing input,
// applies the visibility gate (before any content fetch), runs the
// shared SourceExpander, validates UTF-8, and caps content at the
// model-facing budget.
func expandFromInput(ctx context.Context, expander SourceExpander, scopes ScopeLookup, in expandInput) (expandOutput, error) {
	if expander == nil {
		return expandOutput{}, errors.New("expand tool: no expander configured")
	}
	if in.EventID == "" {
		return expandOutput{}, errors.New("event_id required")
	}
	eventID, err := in.EventID.Int64()
	if err != nil {
		return expandOutput{}, fmt.Errorf("event_id: %w", err)
	}
	if eventID <= 0 {
		return expandOutput{}, errors.New("event_id must be > 0")
	}

	// Visibility gate — BEFORE any content fetch. The turn ctx
	// carries the allowedScopes set for every surface. Without this check,
	// expand bypasses every retrieval filter: the model (or a
	// prompt-injected instruction in retrieved content) could pass
	// an arbitrary event_id and read private-channel content from
	// a public channel.
	readable, err := eventReadable(ctx, scopes, eventID)
	if err != nil {
		return expandOutput{}, fmt.Errorf("expand event_id=%d: %w", eventID, err)
	}
	if !readable {
		return expandOutput{
			Supported: false,
			Note:      fmt.Sprintf("no event with event_id=%d accessible from this context", eventID),
		}, nil
	}

	res, err := expander.Expand(ctx, eventID)
	switch {
	case errors.Is(err, ErrExpandEventNotFound):
		// Not-found is a model-recoverable condition (model may have
		// hallucinated an event_id, or the row was deleted between
		// retrieve and expand). Return a structured "not supported"
		// shape so the model gets the message without a hard tool
		// error stopping the turn. Source is empty because we
		// couldn't resolve one.
		return expandOutput{
			Supported: false,
			Note:      fmt.Sprintf("no event with event_id=%d", eventID),
		}, nil
	case errors.Is(err, ErrSourceNotExpandable):
		return expandOutput{
			Source:    res.Source,
			Actor:     res.Actor,
			Supported: false,
			Note:      fmt.Sprintf("source %q does not support expand yet; work with the chunk summary", res.Source),
		}, nil
	case err != nil:
		// Real errors (session feed down, DB error, etc.) propagate up to
		// the loop so the operator sees the failure and the model
		// gets a clean tool-error envelope rather than a fake
		// "supported=true" result that hides a bug.
		return expandOutput{}, fmt.Errorf("expand event_id=%d: %w", eventID, err)
	}

	// Validate UTF-8 — the tool result is JSON-encoded into the
	// model request, and a binary blob would either
	// blow up encoding or, worse, sneak through and confuse the
	// model. Reject loud rather than encode garbage.
	if !utf8.ValidString(res.Content) {
		return expandOutput{}, fmt.Errorf("expand event_id=%d: source returned non-UTF-8 content", eventID)
	}

	// Apply the model-facing cap regardless of whether the
	// expander already truncated. Keeps the budget enforced at the
	// tool boundary so a future expander that doesn't bound itself
	// can't blow up the model context.
	content, capped := CapExpandContent(res.Content, 0)
	truncated := res.Truncated || capped

	out := expandOutput{
		Source:    res.Source,
		Actor:     res.Actor,
		Supported: true,
		Content:   content,
		Truncated: truncated,
	}
	out.Attachments = listedAttachments(res.Attachments)
	return out, nil
}

// eventReadable is the visibility gate every tool that takes an event_id
// applies before fetching anything: the turn's scopes must allow the event's
// scope. Without it such a tool bypasses every retrieval filter, because the
// model (or an instruction injected through retrieved content) can pass any
// event_id. A missing event answers false exactly as an out-of-scope one, so
// the tool is not an existence oracle for event ids outside the audience.
func eventReadable(ctx context.Context, scopes ScopeLookup, eventID int64) (bool, error) {
	allowed := scopesFromContext(ctx)
	if len(events.ReadableScopes(allowed)) == 0 {
		return false, nil
	}
	if scopes == nil {
		// A wiring bug, not a model-recoverable state: fail closed, loudly.
		return false, errors.New("scoped turn but no ScopeLookup configured")
	}
	scope, err := scopes.EventScope(ctx, eventID)
	switch {
	case errors.Is(err, ErrExpandEventNotFound), errors.Is(err, events.ErrEventNotFound):
		// Both not-found sentinels: the ask-layer one and the one
		// events.DBSink returns.
		return false, nil
	case err != nil:
		// Unknown scope is no access, surfaced so the operator sees it.
		return false, fmt.Errorf("scope lookup: %w", err)
	}
	return events.ScopeAllowed(scope, allowed), nil
}

// NewEinoExpandTool builds the native Eino `expand` tool from the
// shared SourceExpander and ScopeLookup and wires the presentation
// metadata (status line, safe output summary) in as callbacks. The
// native tool decodes arguments and encodes results itself
// (utils.NewTool); it never routes through a JSON adapter. sources name
// the expanded event's source on the progress line.
func NewEinoExpandTool(e SourceExpander, scopes ScopeLookup, sources source.Infos) RegisteredTool {
	presentation := ToolPresentation{
		Status: func(input json.RawMessage) StatusLine {
			return expandStatus(input)
		},
		StatusOutput: func(output json.RawMessage) string {
			return expandStatusOutput(output, sources)
		},
	}

	// The model-facing schema is the expandSchema document verbatim
	// (the single required event_id with its description), carried as
	// an Eino JSON Schema.
	js, err := parseToolJSONSchema(expandSchema)
	if err != nil {
		// The schema is a compile-time constant; a parse failure
		// here is a programming error, not a runtime condition.
		panic(fmt.Sprintf("expand: parse model schema: %v", err))
	}
	info := &schema.ToolInfo{
		Name:        "expand",
		Desc:        expandToolDescription,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
	invoke := func(ctx context.Context, in expandInput) (expandOutput, error) {
		out, err := expandFromInput(ctx, e, scopes, in)
		// Ask has no tool that takes an attachment id.
		for i := range out.Attachments {
			out.Attachments[i].ID = ""
		}
		return out, err
	}

	einoTool := utils.NewTool(info, invoke)
	return RegisteredTool{Tool: einoTool, Presentation: presentation}
}
