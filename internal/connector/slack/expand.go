// expand.go: source-specific expand for slack events.
//
// The /v1/ask runner's `expand` tool dispatches by source. For
// agent_session it asks the session feed for the redacted Bsky Session v1
// slice. For slack it lands here: given an event_id, return the surrounding
// conversation context.
//
// Two modes, dispatched by whether the matched message is part of a
// thread:
//
//	threaded  -> return the full thread (root + all replies, oldest
//	             first). The thread is the natural semantic boundary;
//	             content outside it is unrelated channel chatter.
//
//	standalone -> return a time/count window of channel-adjacent
//	             messages (default ±20 messages OR ±30 minutes,
//	             whichever bounds tighter). Same channel only;
//	             cross-channel context isn't meaningful here.
//
// The shapes are intentionally NOT symmetric to agent_session.
// agent_session chunks are LOSSY summaries — expand recovers verbatim.
// Slack chunks are already verbatim message bodies — expand exists
// to recover CONVERSATION CONTEXT (replies / ambient channel slice).
// The model's prompt teaches this distinction; this code's job is
// just to assemble the right string.
package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

// DefaultWindowCount + DefaultWindowMinutes are the neighbor-window
// knobs. Looser-of-the-two semantics — both bounds are AND'd, so
// whichever fires first prunes the result. In a busy incident channel
// the count is binding; in a quiet ops channel the time bound is
// binding.
const (
	DefaultWindowCount   = 20
	DefaultWindowMinutes = 30 * time.Minute
)

// Expander resolves a slack event_id to its surrounding conversation
// context. Safe for concurrent use; pgxpool handles pooling.
type Expander struct {
	pool          *pgxpool.Pool
	windowCount   int
	windowMinutes time.Duration
}

// NewExpander wires a slack Expander with the default ±20 / ±30min
// neighbor-window knobs. Future tuning surface is the WithWindow
// builder method.
func NewExpander(pool *pgxpool.Pool) *Expander {
	return &Expander{
		pool:          pool,
		windowCount:   DefaultWindowCount,
		windowMinutes: DefaultWindowMinutes,
	}
}

// WithWindow returns a new Expander with overridden neighbor-window
// knobs. Used by tests; production calls NewExpander and lives with
// the defaults.
func (e *Expander) WithWindow(count int, minutes time.Duration) *Expander {
	clone := *e
	if count > 0 {
		clone.windowCount = count
	}
	if minutes > 0 {
		clone.windowMinutes = minutes
	}
	return &clone
}

// resolved bundles what the resolution query returns about the
// matched event row.
type resolved struct {
	channelID   string
	channelName string // best-effort; "" when slack_channel_info is missing
	threadTS    string // populated for replies AND for roots-with-replies
	threadID    string // events.thread_id, the in-DB representation
	occurredAt  time.Time
}

// Expand returns the conversation around the given event_id, as far as
// scopes may read it. Caller has already verified source=slack via
// SourceForEvent.
//
// Errors:
//   - events.ErrEventNotFound when the event doesn't exist (or
//     was deleted between resolution and the inner query — race we
//     handle gracefully).
//   - genuine DB errors propagate up so the loop can decide whether
//     to surface or feed back to the model.
func (e *Expander) Expand(ctx context.Context, eventID int64, scopes []string) (source.Expanded, error) {
	if e.pool == nil {
		return source.Expanded{}, errors.New("slack expand: no pool configured")
	}

	readable := events.ReadableScopes(scopes)
	r, err := e.resolve(ctx, eventID, readable)
	if err != nil {
		return source.Expanded{}, err
	}

	threaded := r.threadID != "" || r.threadTS != ""
	if threaded {
		return e.expandThread(ctx, r, readable)
	}
	return e.expandNeighbors(ctx, eventID, r, readable)
}

// resolve runs the channel/thread anchor query. Joins
// slack_channel_info to lift the channel name into the same round
// trip — the rendering needs it for the header.
func (e *Expander) resolve(ctx context.Context, eventID int64, readable []string) (resolved, error) {
	const q = `
		SELECT
			d.channel_id,
			COALESCE(ci.name, ''),
			COALESCE(d.thread_ts, ''),
			COALESCE(e.thread_id, ''),
			e.occurred_at
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		  LEFT JOIN slack_channel_info ci ON ci.channel_id = d.channel_id
		 WHERE e.id = $1 AND e.source = 'slack' AND e.deleted = false AND e.scope = ANY($2)
	`
	var r resolved
	err := e.pool.QueryRow(ctx, q, eventID, readable).Scan(
		&r.channelID, &r.channelName, &r.threadTS, &r.threadID, &r.occurredAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return resolved{}, events.ErrEventNotFound
		}
		return resolved{}, fmt.Errorf("slack expand: resolve event %d: %w", eventID, err)
	}
	return r, nil
}

// rootTS picks the thread root timestamp from a resolved row. For
// replies and roots-with-replies, slack_message_details.thread_ts is
// populated and equals the root ts; for the rare case where only
// events.thread_id is populated (a stale row from a prior schema), we
// fall back to parsing it from the "channel:ts" form.
func rootTS(r resolved) string {
	if r.threadTS != "" {
		return r.threadTS
	}
	// thread_id format is "channel:ts" (see threadKey in parse.go).
	if i := strings.IndexByte(r.threadID, ':'); i >= 0 {
		return r.threadID[i+1:]
	}
	return ""
}

// threadMessage is one row in the rendered output (used for both
// modes — the per-line shape is shared).
type threadMessage struct {
	eventID    int64
	actor      string
	occurredAt time.Time
	body       string
	url        string
	externalID string // "channel:ts"
	threadTS   string // == "" for standalone, == ts for root, == root_ts for reply
	replyCount int    // populated for thread roots (non-zero) so renderer can annotate
}

// expandThread fetches every message in the thread and renders it.
//
// The query matches both shapes: the root by external_id (= channel:root_ts)
// AND replies by their thread_ts column (= root_ts). Reply count is
// computed as a subquery so the renderer can annotate the root with
// "(N replies in thread)" in the rare case the matched message itself
// is the root in a window-mode caller — for thread mode we don't use
// the count column on individual rows.
func (e *Expander) expandThread(ctx context.Context, r resolved, readable []string) (source.Expanded, error) {
	root := rootTS(r)
	if root == "" {
		// Defensive: thread mode entered but neither thread_ts nor a
		// parseable thread_id. Treat as standalone.
		return source.Expanded{
			Content: fmt.Sprintf("(message in #%s has thread metadata but no resolvable root)",
				channelLabel(r.channelName)),
		}, nil
	}
	rootExtID := r.channelID + ":" + root

	const q = `
		SELECT
			e.id,
			COALESCE(e.actor, ''),
			e.occurred_at,
			e.body,
			COALESCE(e.url, ''),
			e.external_id,
			COALESCE(d.thread_ts, '')
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.source = 'slack'
		   AND d.channel_id = $1
		   AND e.deleted = false
		   AND e.scope = ANY($4)
		   AND (
		       e.external_id = $2  -- the root
		       OR d.thread_ts = $3 -- the replies
		   )
		 ORDER BY e.occurred_at ASC, e.id ASC
	`
	rows, err := e.pool.Query(ctx, q, r.channelID, rootExtID, root, readable)
	if err != nil {
		return source.Expanded{},
			fmt.Errorf("slack expand: thread query: %w", err)
	}
	defer rows.Close()

	var msgs []threadMessage
	for rows.Next() {
		var m threadMessage
		if err := rows.Scan(&m.eventID, &m.actor, &m.occurredAt,
			&m.body, &m.url, &m.externalID, &m.threadTS); err != nil {
			return source.Expanded{},
				fmt.Errorf("slack expand: thread scan: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return source.Expanded{},
			fmt.Errorf("slack expand: thread rows: %w", err)
	}

	rendered, truncated := renderThread(r, msgs)
	// Append extracted text for any documents attached to the messages
	// in this thread. Without it, expand can show "here is the contract"
	// and still not let the model read the contract.
	rendered += e.documentAppendix(ctx, eventIDsOf(msgs))
	return source.Expanded{Content: rendered, Truncated: truncated, Attachments: e.attachments(ctx, eventIDsOf(msgs))}, nil
}

// expandNeighbors fetches a count+time-bounded slice of channel-
// adjacent messages around the matched event_id, and renders it.
//
// Filter logic on the thread side: include top-level channel-stream
// messages (standalones, where d.thread_ts IS NULL) AND thread roots
// (where d.thread_ts = the message's own ts, so external_id =
// channel:thread_ts). Exclude thread replies — they're a different
// conversation that happens to overlap in time. Without this filter,
// an active concurrent thread in a busy channel would dominate the
// window with replies that aren't actually surrounding context.
//
// Reply counts on roots are projected via a correlated subquery so
// the renderer can annotate "(N replies in thread)" on lines that
// correspond to thread roots.
func (e *Expander) expandNeighbors(ctx context.Context, eventID int64, r resolved, readable []string) (source.Expanded, error) {
	// $1 = eventID, $2 = window minutes interval, $3 = window count.
	const q = `
		WITH matched AS (
			SELECT e.id, e.occurred_at, d.channel_id
			  FROM events e
			  JOIN slack_message_details d ON d.event_id = e.id
			 WHERE e.id = $1
			   AND e.source = 'slack'
			   AND e.deleted = false
			   AND e.scope = ANY($4)
		),
		before_msgs AS (
			SELECT e.id,
			       COALESCE(e.actor, '') AS actor,
			       e.occurred_at,
			       e.body,
			       COALESCE(e.url, '') AS url,
			       e.external_id,
			       COALESCE(d.thread_ts, '') AS thread_ts
			  FROM events e
			  JOIN slack_message_details d ON d.event_id = e.id
			  JOIN matched m ON m.channel_id = d.channel_id
			 WHERE e.source = 'slack'
			   AND e.deleted = false
			   AND e.scope = ANY($4)
			   AND (e.occurred_at, e.id) < (m.occurred_at, m.id)
			   AND e.occurred_at >= m.occurred_at - ($2 || ' minutes')::interval
			   AND (
			       d.thread_ts IS NULL
			       OR e.external_id = d.channel_id || ':' || d.thread_ts
			   )
			 ORDER BY e.occurred_at DESC, e.id DESC
			 LIMIT $3
		),
		self_msg AS (
			SELECT e.id,
			       COALESCE(e.actor, '') AS actor,
			       e.occurred_at,
			       e.body,
			       COALESCE(e.url, '') AS url,
			       e.external_id,
			       COALESCE(d.thread_ts, '') AS thread_ts
			  FROM events e
			  JOIN slack_message_details d ON d.event_id = e.id
			 WHERE e.id = $1
			   AND e.source = 'slack'
			   AND e.deleted = false
			   AND e.scope = ANY($4)
		),
		after_msgs AS (
			SELECT e.id,
			       COALESCE(e.actor, '') AS actor,
			       e.occurred_at,
			       e.body,
			       COALESCE(e.url, '') AS url,
			       e.external_id,
			       COALESCE(d.thread_ts, '') AS thread_ts
			  FROM events e
			  JOIN slack_message_details d ON d.event_id = e.id
			  JOIN matched m ON m.channel_id = d.channel_id
			 WHERE e.source = 'slack'
			   AND e.deleted = false
			   AND e.scope = ANY($4)
			   AND (e.occurred_at, e.id) > (m.occurred_at, m.id)
			   AND e.occurred_at <= m.occurred_at + ($2 || ' minutes')::interval
			   AND (
			       d.thread_ts IS NULL
			       OR e.external_id = d.channel_id || ':' || d.thread_ts
			   )
			 ORDER BY e.occurred_at ASC, e.id ASC
			 LIMIT $3
		),
		all_rows AS (
			SELECT * FROM before_msgs
			UNION ALL
			SELECT * FROM self_msg
			UNION ALL
			SELECT * FROM after_msgs
		)
		SELECT
			a.id,
			a.actor,
			a.occurred_at,
			a.body,
			a.url,
			a.external_id,
			a.thread_ts,
			-- Reply count for thread roots only. A row is a thread root
			-- iff its thread_ts is non-empty AND its external_id is
			-- channel:thread_ts (so the "thread anchor" is itself).
			-- Anything else gets 0; the renderer reads zero as "not a
			-- root, no annotation."
			CASE
				WHEN a.thread_ts <> '' AND a.external_id = (SELECT m.channel_id FROM matched m) || ':' || a.thread_ts THEN
					(SELECT COUNT(*)
					   FROM events e2
					   JOIN slack_message_details d2 ON d2.event_id = e2.id
					  WHERE e2.source = 'slack'
					    AND e2.deleted = false
					    AND e2.scope = ANY($4)
					    AND d2.channel_id = (SELECT m.channel_id FROM matched m)
					    AND d2.thread_ts = a.thread_ts
					    AND e2.id <> a.id)
				ELSE 0
			END AS reply_count
		  FROM all_rows a
		 ORDER BY a.occurred_at ASC, a.id ASC
	`
	minutes := int(e.windowMinutes / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	// minutes goes in as a text arg because the SQL builds the interval
	// via "$N || ' minutes'"::interval — pgx encodes ints to numeric
	// types by default, and concatenation against text wants a text
	// arg. Stringifying here keeps pgx happy without making the SQL
	// uglier.
	rows, err := e.pool.Query(ctx, q, eventID, fmt.Sprintf("%d", minutes), e.windowCount, readable)
	if err != nil {
		return source.Expanded{},
			fmt.Errorf("slack expand: neighbor query: %w", err)
	}
	defer rows.Close()

	var msgs []threadMessage
	for rows.Next() {
		var m threadMessage
		if err := rows.Scan(&m.eventID, &m.actor, &m.occurredAt,
			&m.body, &m.url, &m.externalID, &m.threadTS, &m.replyCount); err != nil {
			return source.Expanded{},
				fmt.Errorf("slack expand: neighbor scan: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return source.Expanded{},
			fmt.Errorf("slack expand: neighbor rows: %w", err)
	}

	rendered, truncated := renderNeighbors(r, eventID, e.windowCount, e.windowMinutes, msgs)
	rendered += e.documentAppendix(ctx, eventIDsOf(msgs))
	return source.Expanded{Content: rendered, Truncated: truncated, Attachments: e.attachments(ctx, eventIDsOf(msgs))}, nil
}

// eventIDsOf projects the rendered message set to its event ids, for
// the document-appendix lookup. Uses the full set rather than just the
// matched event: in a thread where one person posts a contract and
// another asks about it, the contract should surface regardless of
// which message the vector search matched.
func eventIDsOf(msgs []threadMessage) []int64 {
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.eventID)
	}
	return out
}

// channelLabel renders a human-readable channel reference for headers.
// Falls back to "(unknown channel)" when slack_channel_info is missing
// — happens for channels we ingested before the directory cache had
// fetched them.
func channelLabel(name string) string {
	if name == "" {
		return "(unknown)"
	}
	return name
}

// renderThreshold is how close to the model-facing budget we render
// before triggering middle-truncation. Leaves headroom for the ask
// tool's UTF-8-rune-walk-back step at the outer cap.
const renderThreshold = 60 * 1024

// renderThread formats a thread for the model. Header + per-message
// lines. Truncation strategy when content exceeds the budget: keep
// root + first-few + last-few, drop middle, with a clear marker.
// Root anchors the question; tail likely contains the resolution;
// mid-thread back-and-forth is the most-skippable.
func renderThread(r resolved, msgs []threadMessage) (string, bool) {
	if len(msgs) == 0 {
		return fmt.Sprintf("Thread in #%s: (no messages found — root may not be in corpus)\n",
			channelLabel(r.channelName)), false
	}

	// Header summarizes the thread. Span = last - first occurredAt.
	var span time.Duration
	if len(msgs) > 1 {
		span = msgs[len(msgs)-1].occurredAt.Sub(msgs[0].occurredAt)
	}
	header := fmt.Sprintf("Thread in #%s (%d %s%s, starting %s):\n",
		channelLabel(r.channelName),
		len(msgs),
		pluralize("message", len(msgs)),
		spanSuffix(span),
		msgs[0].occurredAt.UTC().Format("2006-01-02 15:04 UTC"),
	)

	// Render naive first; middle-truncate if oversize. strings.Builder
	// keeps this O(N): a thread can have thousands of messages and
	// the naive `s += renderLine(m)` form is O(N^2) on string copies.
	var sb strings.Builder
	sb.WriteString(header)
	for _, m := range msgs {
		sb.WriteString(renderLine(m, false /* not a matched-marker line */))
	}
	if sb.Len() <= renderThreshold {
		return sb.String(), false
	}

	// Oversize — keep the root + first ~5 + last ~5 messages.
	const headKeep = 6 // root + 5 leading replies
	const tailKeep = 5
	if len(msgs) <= headKeep+tailKeep {
		// Already in the keep set — this only happens with
		// pathologically long single messages. Just return full and
		// let the outer cap clip; truncated stays false here because
		// we couldn't trim further.
		return sb.String(), false
	}
	var out strings.Builder
	out.WriteString(header)
	for i := 0; i < headKeep; i++ {
		out.WriteString(renderLine(msgs[i], false))
	}
	dropped := len(msgs) - headKeep - tailKeep
	fmt.Fprintf(&out, "[... %d %s truncated ...]\n",
		dropped, pluralize("message", dropped))
	for i := len(msgs) - tailKeep; i < len(msgs); i++ {
		out.WriteString(renderLine(msgs[i], false))
	}
	return out.String(), true
}

// renderNeighbors formats the neighbor-window slice. Header includes
// the channel name + matched timestamp + actual count. The matched
// message gets the ◄── marker so the model knows which line was the
// retrieve hit. Thread roots get a "(N replies in thread, event_id=...)"
// annotation so the model knows it can drill into a parallel thread
// separately.
func renderNeighbors(r resolved, matchedID int64, count int, minutes time.Duration, msgs []threadMessage) (string, bool) {
	if len(msgs) == 0 {
		// Resolution succeeded but the inner query returned 0 — the
		// matched row was deleted between resolution and fetch. Rare;
		// surface a clear note rather than an empty block.
		return fmt.Sprintf("Channel context for #%s around %s: (matched message no longer present)\n",
			channelLabel(r.channelName),
			r.occurredAt.UTC().Format("2006-01-02 15:04 UTC")), false
	}
	header := fmt.Sprintf(
		"Channel context for #%s around %s (%d %s within ±%dmin / ±%d messages of the matched line):\n",
		channelLabel(r.channelName),
		r.occurredAt.UTC().Format("2006-01-02 15:04 UTC"),
		len(msgs),
		pluralize("message", len(msgs)),
		int(minutes/time.Minute),
		count,
	)

	var sb strings.Builder
	sb.WriteString(header)
	for _, m := range msgs {
		sb.WriteString(renderLine(m, m.eventID == matchedID))
	}
	if sb.Len() <= renderThreshold {
		return sb.String(), false
	}
	// Oversize neighbor window — drop from the edges symmetrically so
	// the matched line stays centered in what we keep. Find the
	// matched index and walk outward until we hit budget.
	matchedIdx := -1
	for i, m := range msgs {
		if m.eventID == matchedID {
			matchedIdx = i
			break
		}
	}
	if matchedIdx < 0 {
		// Should not happen — the matched row came from self_msg in
		// the SQL — but degrade gracefully by keeping the head. Walk
		// back to a rune boundary so we don't ship a partial
		// codepoint (multi-byte UTF-8 in slack message bodies is
		// common — emoji shortcodes get rendered to bytes by the
		// directory cache, links can contain unicode).
		s := sb.String()
		cap := renderThreshold
		if cap > len(s) {
			cap = len(s)
		}
		for cap > 0 && !utf8.RuneStart(s[cap]) {
			cap--
		}
		return s[:cap], true
	}
	// Walk outward until cumulative size hits the budget.
	keep := []int{matchedIdx}
	cum := len(header) + len(renderLine(msgs[matchedIdx], true))
	left, right := matchedIdx-1, matchedIdx+1
	for left >= 0 || right < len(msgs) {
		if right < len(msgs) {
			line := renderLine(msgs[right], false)
			if cum+len(line) > renderThreshold {
				break
			}
			cum += len(line)
			keep = append(keep, right)
			right++
		}
		if left >= 0 {
			line := renderLine(msgs[left], false)
			if cum+len(line) > renderThreshold {
				break
			}
			cum += len(line)
			keep = append(keep, left)
			left--
		}
	}
	// Sort kept indices ascending and render.
	sortInts(keep)
	var out strings.Builder
	out.WriteString(header)
	for _, i := range keep {
		out.WriteString(renderLine(msgs[i], msgs[i].eventID == matchedID))
	}
	return out.String(), true
}

// renderLine formats one message into a single output line. Body is
// rendered verbatim with whitespace preserved; multi-line bodies
// stay multi-line (the model handles them fine and indenting would
// triple the token cost).
//
// Annotations:
//   - matched: appends a ◄── marker so the neighbor-mode renderer can
//     mark which line was the retrieve hit.
//   - thread root with replies: annotates "(N replies in thread,
//     event_id=...)" so the model knows it can re-call expand on
//     this event_id to fetch the thread separately.
func renderLine(m threadMessage, matched bool) string {
	timeStr := m.occurredAt.UTC().Format("15:04:05")
	actor := m.actor
	if actor == "" {
		actor = "(unknown)"
	}
	body := m.body
	// Compress trailing newlines so each rendered line ends with
	// exactly one \n. Leading + interior newlines stay (they
	// preserve the slack message's own multi-line shape).
	body = strings.TrimRight(body, "\n")

	var annotations []string
	if m.replyCount > 0 {
		annotations = append(annotations,
			fmt.Sprintf("%d %s in thread, event_id=%d",
				m.replyCount, pluralize("reply", m.replyCount), m.eventID))
	}
	if matched {
		annotations = append(annotations, "matched")
	}
	annotation := ""
	if len(annotations) > 0 {
		annotation = " [" + strings.Join(annotations, "; ") + "]"
	}
	return fmt.Sprintf("[%s] @%s%s: %s\n", timeStr, actor, annotation, body)
}

// pluralize is a tiny helper to keep the rendered prose readable
// without dragging in a full pluralization library.
func pluralize(noun string, n int) string {
	if n == 1 {
		return noun
	}
	switch noun {
	case "reply":
		return "replies"
	default:
		return noun + "s"
	}
}

// spanSuffix renders the human-readable thread duration suffix used
// in the thread-mode header. Returns "" when span is < 1min so
// near-simultaneous threads don't get a confusing "0m" tag.
func spanSuffix(span time.Duration) string {
	if span < time.Minute {
		return ""
	}
	if span < time.Hour {
		return fmt.Sprintf(" over %dm", int(span/time.Minute))
	}
	if span < 24*time.Hour {
		hours := span / time.Hour
		mins := (span % time.Hour) / time.Minute
		if mins == 0 {
			return fmt.Sprintf(" over %dh", hours)
		}
		return fmt.Sprintf(" over %dh%dm", hours, mins)
	}
	return fmt.Sprintf(" over %dd", span/(24*time.Hour))
}

// sortInts is a tiny in-place ascending sort. Saves a sort.Slice +
// closure for the ~tens-of-elements-max case in renderNeighbors.
func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
