package retrieve

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/jackc/pgx/v5"
)

// ListRequest is the structured input to (*Retriever).List.
//
// "List" is the database-shaped enumeration tool: filter the events
// table, order by recency, return one row per event. No question, no
// vector search, no qdrant. Vector retrieve is wrong-shaped for
// "for each X in scope, do Y" workflows — silently lossy, no
// monotone pagination, chunks-not-events granularity.
//
// At least one of Sources / Actors / Channels / OccurredAfter /
// OccurredBefore MUST be set; an unconstrained list against a 100k-row
// corpus is rejected. Validation lives in Validate() so HTTP handlers and the
// agent tool can share one rule.
type ListRequest struct {
	Sources []string
	Actors  []string
	// Channels restricts the list to messages of these channels, each
	// given as a name (with or without '#'), a channel ID, or a channel or
	// message URL: Slack channels, and the channels of sources the
	// Retriever's ChannelFinder knows. List resolves them against
	// AllowedScopes: a channel the caller cannot read fails with
	// ErrListChannelNotFound exactly as one that does not exist, so the
	// filter never widens the result and never confirms a hidden channel.
	Channels       []string
	OccurredAfter  time.Time
	OccurredBefore time.Time
	// TopK is the number of events returned. Defaults to ListDefaultTopK
	// (25) when zero. Clamped to [1, ListMaxTopK] (50) by the handler;
	// List itself trusts the caller's value but applies the same
	// safety floor + ceiling so direct in-process callers can't blow
	// the truncation budget.
	TopK int
	// ThreadRootsOnly: when true, restrict the rows of ThreadedSources to
	// top-level messages (standalones + thread roots), excluding replies.
	// Has no effect on rows from other sources in a multi-source query.
	ThreadRootsOnly bool
	// ThreadedSources are the event sources whose events form threads
	// (source.Info.Threads), which ThreadRootsOnly applies to.
	ThreadedSources []string
	// SessionLevelOnly: when true and Sources includes
	// "agent_session", restrict to session-level rows
	// (external_id ending in ":session"), excluding the per-chunk
	// rows that share a session prefix. Has no effect on rows from
	// other sources.
	SessionLevelOnly bool
	// AllowedScopes is the visibility boundary, same contract as
	// retrieve.Filters.AllowedScopes: empty = no access,
	// non-empty = only events whose scope is in the set.
	// Set server-side by the ask layer; never model-controlled.
	// Does not count as a filter for Validate() — it bounds
	// visibility, not result volume.
	AllowedScopes []string
	// ExcludeConversations leaves whole conversations out of the listing:
	// see Filters.ExcludeConversations. Never model-controlled.
	ExcludeConversations []Conversation
	// Cursor resumes a previous List from where it left off: an
	// opaque token from Result.NextCursor encoding the last row's
	// (occurred_at, id) sort position. The follow-up call must carry
	// the SAME filters; the cursor only encodes position, not the
	// filter set, so changing filters mid-pagination yields
	// well-defined but probably unwanted results. Does not count as
	// a filter for Validate() — a cursor-bearing call still needs
	// the filter set that produced it.
	Cursor string
}

// ListDefaultTopK and ListMaxTopK gate the row count. Tight cap
// because each row carries a per-event Content truncation budget,
// and 50 rows × ~1.2KiB Content fits inside the model's ~64KiB
// tool-result cap with envelope headroom. Operators paginate by
// tightening time bounds.
const (
	ListDefaultTopK = 25
	ListMaxTopK     = 50

	// listOutputBudgetRunes is the rune budget for per-row Content,
	// minus envelope overhead (JSON keys + per-Hit metadata across
	// up to 50 rows). 60000 - 4096 ≈ 56000 leaves headroom under the
	// model's ~64KiB tool-result cap.
	listOutputBudgetRunes = 56000
	// listMaxRunesPerHit caps the per-row Content even when the
	// budget formula would allow more. If a query matches a single
	// huge event we don't want to dump 60KiB inline — past ~2KiB
	// the next move is `expand`, not "more inline." Keeps responses
	// browseable across the corpus instead of weighted by one
	// outlier event.
	listMaxRunesPerHit = 2048

	listTruncMarker = "\n…(truncated)"
)

// ErrListNoFilter is returned when ListRequest has no filter set.
// Surfaced as a 400 by the HTTP handler and as a tool-call error by
// the agent so the model can re-call with at least one filter.
var ErrListNoFilter = errors.New("list: at least one of sources, actors, channels, occurred_after, occurred_before is required")

// ErrListChannelNotFound is returned when a Channels entry names no
// channel the caller can read: unknown to the corpus, or outside
// AllowedScopes. The two are deliberately one error.
var ErrListChannelNotFound = errors.New("list: channel not found")

// ErrListBadCursor is returned when ListRequest.Cursor doesn't decode.
// Surfaced as a 400 / tool-call error; the caller should restart
// pagination from the first page rather than retry the bad token.
var ErrListBadCursor = errors.New("list: malformed cursor")

// listCursor is the decoded pagination position: the (occurred_at,
// id) sort key of the last row of the previous page. Postgres
// timestamptz has microsecond precision, so UnixMicro round-trips
// the ORDER BY key losslessly.
type listCursor struct {
	occurredAt time.Time
	id         int64
}

// encodeListCursor packs the sort position into an opaque
// URL-safe token. Opaque on purpose: callers (including the model)
// must not construct or reinterpret cursors — the encoding is free
// to change with the sort key.
func encodeListCursor(c listCursor) string {
	raw := fmt.Sprintf("%d:%d", c.occurredAt.UnixMicro(), c.id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeListCursor(s string) (listCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listCursor{}, fmt.Errorf("%w: %v", ErrListBadCursor, err)
	}
	var micros, id int64
	if _, err := fmt.Sscanf(string(b), "%d:%d", &micros, &id); err != nil {
		return listCursor{}, fmt.Errorf("%w: %v", ErrListBadCursor, err)
	}
	return listCursor{occurredAt: time.UnixMicro(micros).UTC(), id: id}, nil
}

// Validate returns ErrListNoFilter when the request has no filter.
// Other shape concerns (TopK clamping, time-range sanity) are deferred
// to the HTTP handler since they're forgiving rather than rejecting.
func (req ListRequest) Validate() error {
	if len(req.Sources) == 0 && len(req.Actors) == 0 && len(req.Channels) == 0 &&
		req.OccurredAfter.IsZero() && req.OccurredBefore.IsZero() {
		return ErrListNoFilter
	}
	return nil
}

// List runs the Postgres-only enumeration path. Returns events (one
// row per event_id), ordered by occurred_at DESC, id DESC, with
// per-row Content truncated to keep the JSON-serialized response
// under the model's tool-result cap.
//
// No qdrant. No embedder call. Pure dynamic SELECT, planner picks
// the right index from the filter set:
//   - actor-narrow + ORDER BY → idx_events_actor_canonical_occurred_at
//   - time-narrow → idx_events_occurred_at
//   - source-narrow → seq scan + filter (small cardinality, fine)
//
// Latency surfaces in Result.Latency.TotalMS so the operator can
// see if a query missed the composite index.
func (r *Retriever) List(ctx context.Context, req ListRequest) (*Result, error) {
	req.Sources = trimFilterValues(req.Sources)
	req.Actors = trimFilterValues(req.Actors)
	req.Channels = trimFilterValues(req.Channels)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	channels, err := r.resolveListChannels(ctx, req.Channels, req.AllowedScopes)
	if err != nil {
		return nil, err
	}
	topK := req.TopK
	if topK <= 0 {
		topK = ListDefaultTopK
	}
	if topK > ListMaxTopK {
		topK = ListMaxTopK
	}

	var cursor *listCursor
	if req.Cursor != "" {
		c, err := decodeListCursor(req.Cursor)
		if err != nil {
			return nil, err
		}
		cursor = &c
	}

	total := time.Now()
	out := &Result{}

	// Fetch one row past topK: the presence of row topK+1 is the
	// "more pages exist" signal, without a second COUNT query.
	sql, args := buildListSQL(req, channels, cursor, topK+1)

	t := time.Now()
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list query: %w", err)
	}
	defer rows.Close()

	hits, err := scanListRows(rows)
	if err != nil {
		return nil, fmt.Errorf("list scan: %w", err)
	}
	out.Latency.DereferenceMS = time.Since(t).Milliseconds()

	if len(hits) > topK {
		hits = hits[:topK]
		last := hits[topK-1]
		out.NextCursor = encodeListCursor(listCursor{
			occurredAt: last.OccurredAt, id: last.EventID,
		})
	}

	// Truncation pass. Budget is per-row, computed against the
	// actual returned-row count (NOT topK) so a query that matched
	// 8 rows gets the full per-row cap, not a fraction of it.
	if len(hits) > 0 {
		budget := listOutputBudgetRunes / len(hits)
		if budget > listMaxRunesPerHit {
			budget = listMaxRunesPerHit
		}
		for i := range hits {
			hits[i].Content = truncateRunes(hits[i].Content, budget)
		}
	}

	out.Hits = hits
	out.Latency.TotalMS = time.Since(total).Milliseconds()
	return out, nil
}

// buildListSQL constructs the SELECT dynamically. Each filter clause
// is appended only when the corresponding field is set, so the
// planner picks an index appropriate to the filter shape — a static
// query with `OR ($1 IS NULL OR ...)` predicates would force a
// generic plan that couldn't use the per-column indexes.
//
// Returned SQL is a single string with $N placeholders matching args
// positionally. Caller passes both to pool.Query verbatim. limit is
// the raw LIMIT value (List passes topK+1 for has-more detection).
//
// channels are req.Channels already resolved and checked against the
// caller's scopes. With only Slack channels set every row is a Slack
// message, so the time range, the cursor, and the ordering read the side
// table's (occurred_at, event_id), which its (channel_id, occurred_at DESC,
// event_id DESC) index serves: a busy channel's time range is an index
// range scan, not a filter over every event in the window. Another
// source's channels match their events by external-ID prefix.
func buildListSQL(req ListRequest, channels listChannels, cursor *listCursor, limit int) (string, []any) {
	var sb strings.Builder
	// LEFT JOIN slack_message_details + slack_channel_info: only
	// matches when source='slack'. coalesce(ci.name, '') gives ''
	// for non-slack rows and for slack rows whose channel hasn't
	// been directory-cached yet. Live read at query time so
	// channel renames self-heal within ~5min.
	sb.WriteString(`
		SELECT e.id, e.external_id, e.source,
		       COALESCE(e.actor, ''),
		       COALESCE(e.actor_canonical, ''),
		       COALESCE(e.actor_id, ''),
		       COALESCE(e.thread_id, ''),
		       e.occurred_at,
		       COALESCE(e.title, ''),
		       COALESCE(e.url, ''),
		       e.body,
		       COALESCE(ci.name, '') AS channel_name
		  FROM events e
		  LEFT JOIN slack_message_details d ON d.event_id = e.id AND e.source = 'slack'
		  LEFT JOIN slack_channel_info ci ON ci.channel_id = d.channel_id
		 WHERE e.deleted = false`)
	args := make([]any, 0, 7)

	args = append(args, events.ReadableScopes(req.AllowedScopes))
	fmt.Fprintf(&sb, " AND e.scope = ANY($%d)", len(args))
	if sources, keys := conversationKeys(req.ExcludeConversations); len(keys) > 0 {
		args = append(args, sources, keys)
		fmt.Fprintf(&sb, ` AND NOT EXISTS (
			SELECT 1 FROM unnest($%d::text[], $%d::text[]) AS x(source, key)
			 WHERE e.source = x.source AND (e.thread_id = x.key OR e.external_id = x.key))`, len(args)-1, len(args))
	}
	occurredAt, id := "e.occurred_at", "e.id"
	switch {
	case len(channels.slack) > 0 && len(channels.others) == 0:
		occurredAt, id = "d.occurred_at", "d.event_id"
		args = append(args, channels.slack)
		fmt.Fprintf(&sb, " AND d.channel_id = ANY($%d)", len(args))
	case len(channels.slack) > 0 || len(channels.others) > 0:
		var match []string
		if len(channels.slack) > 0 {
			args = append(args, channels.slack)
			match = append(match, fmt.Sprintf("d.channel_id = ANY($%d)", len(args)))
		}
		sources, prefixes := make([]string, 0, len(channels.others)), make([]string, 0, len(channels.others))
		for _, c := range channels.others {
			sources, prefixes = append(sources, c.Source), append(prefixes, c.ExternalIDPrefix)
		}
		if len(prefixes) > 0 {
			args = append(args, sources, prefixes)
			match = append(match, fmt.Sprintf(`EXISTS (
				SELECT 1 FROM unnest($%d::text[], $%d::text[]) AS c(source, prefix)
				 WHERE e.source = c.source AND starts_with(e.external_id, c.prefix))`, len(args)-1, len(args)))
		}
		fmt.Fprintf(&sb, " AND (%s)", strings.Join(match, " OR "))
	}
	if len(req.Sources) > 0 {
		args = append(args, req.Sources)
		fmt.Fprintf(&sb, " AND e.source = ANY($%d)", len(args))
	}
	if len(req.Actors) > 0 {
		// Two paths:
		//
		// Hot path (all actors look email-shaped) — match against
		// actor_canonical only. This lets the planner use the
		// composite (actor_canonical, occurred_at DESC) partial
		// index for both the equality filter AND the ORDER BY,
		// which is O(N) regardless of how recently the actor was
		// active. The OR-with-actor branch would force a backward
		// scan of the time index + filter, cliffing to near-full-
		// table-scan latency for inactive actors.
		//
		// Slow path (any actor is non-email-shaped, e.g. a slack
		// display name like "Alex") — OR across canonical + display
		// so display-name matches still work. We accept the index
		// loss for the rarer case; operators passing display names
		// should tighten time bounds.
		//
		// "Looks email-shaped" = contains '@' AND a '.' after it.
		// Defensive against future actor schemes like "svc:foo".
		args = append(args, req.Actors)
		// The trailing `AND e.actor_canonical <> ''` is load-bearing
		// for partial-index usage. The composite index is partial on
		// (actor_canonical <> ''); Postgres won't pick a partial
		// index unless the query's WHERE clause explicitly implies
		// the partial predicate. With a parameterized ANY($N), the
		// planner can't prove the array excludes '', so it falls
		// back to the time index (full scan for inactive actors).
		// Adding the redundant comparison closes the gap.
		if allEmailShaped(req.Actors) {
			fmt.Fprintf(&sb,
				" AND e.actor_canonical = ANY($%d) AND e.actor_canonical <> ''",
				len(args))
		} else {
			fmt.Fprintf(&sb,
				" AND ((e.actor_canonical = ANY($%d) AND e.actor_canonical <> '') OR e.actor = ANY($%d))",
				len(args), len(args))
		}
	}
	if !req.OccurredAfter.IsZero() {
		args = append(args, req.OccurredAfter)
		fmt.Fprintf(&sb, " AND %s >= $%d", occurredAt, len(args))
	}
	if !req.OccurredBefore.IsZero() {
		// Inclusive `<=`: occurred_before is a pure time filter.
		// Pagination is the cursor's job, not this bound's — the
		// row-comparison below strictly advances past ties.
		args = append(args, req.OccurredBefore)
		fmt.Fprintf(&sb, " AND %s <= $%d", occurredAt, len(args))
	}
	if cursor != nil {
		// Keyset pagination: strictly after the last row of the
		// previous page in (occurred_at DESC, id DESC) order. SQL
		// row comparison matches the composite ORDER BY exactly, so
		// ties on occurred_at advance by id instead of stalling —
		// which they must, because batch ingests routinely stamp
		// dozens of events with one timestamp.
		args = append(args, cursor.occurredAt)
		occArg := len(args)
		args = append(args, cursor.id)
		fmt.Fprintf(&sb, " AND (%s, %s) < ($%d, $%d)", occurredAt, id, occArg, len(args))
	}
	if req.ThreadRootsOnly && len(req.ThreadedSources) > 0 {
		// A threaded source's thread_id is empty for standalones, equal
		// to external_id for thread roots, different from external_id
		// for replies. "Top-level message" = standalone OR root. Other
		// sources' rows pass, so the knob is a no-op for them in a
		// multi-source list.
		args = append(args, req.ThreadedSources)
		fmt.Fprintf(&sb, " AND (NOT e.source = ANY($%d) OR COALESCE(e.thread_id, '') = '' OR e.thread_id = e.external_id)", len(args))
	}
	if req.SessionLevelOnly {
		// agent_session: session-level rows have external_id
		// suffix `:session`. Per-chunk rows use
		// `:<record_start>:<record_end>` shape, which doesn't end in `:session`.
		sb.WriteString(" AND (e.source <> 'agent_session' OR e.external_id LIKE '%:session')")
	}
	args = append(args, limit)
	fmt.Fprintf(&sb, " ORDER BY %s DESC, %s DESC LIMIT $%d", occurredAt, id, len(args))
	return sb.String(), args
}

// slackArchiveChannelRE finds the channel ID in a Slack archive URL or path,
// with or without a message segment after it.
var slackArchiveChannelRE = regexp.MustCompile(`/archives/([CDG][A-Z0-9]+)(?:[/?#]|$)`)

// slackChannelIDRE is the shape of a Slack conversation ID.
var slackChannelIDRE = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)

// ChannelFinder finds the channels of sources other than Slack that one
// Channels entry may name.
type ChannelFinder interface {
	FindChannels(ctx context.Context, entry string) ([]ChannelMatch, error)
}

// ChannelMatch is a channel a ChannelFinder found: the external-ID prefix of
// its source's events in it, and the scope they carry.
type ChannelMatch struct {
	Source, ExternalIDPrefix, Scope string
}

// SetChannelFinder lets List resolve the channels of the sources finder
// knows, from the next call on.
func (r *Retriever) SetChannelFinder(finder ChannelFinder) {
	r.channelFinder.Store(&finder)
}

// listChannels is a list's resolved channels: Slack channel IDs, and other
// sources' channels.
type listChannels struct {
	slack  []string
	others []ChannelMatch
}

// resolveListChannels turns each Channels entry into the channels it names
// that allowedScopes can read. Every Slack channel message is scoped to its
// channel (events.ScopeChannel), so a Slack channel is readable exactly when
// that scope is allowed; another source's channel when the scope its
// finder reports is. A name can belong to several channels; all the readable
// ones are returned. An entry that yields none fails the whole call, whether
// the channel is unknown or only hidden.
func (r *Retriever) resolveListChannels(ctx context.Context, channels, allowedScopes []string) (listChannels, error) {
	var out listChannels
	if len(channels) == 0 {
		return out, nil
	}
	readable := events.ReadableScopes(allowedScopes)
	for _, channel := range channels {
		candidates, err := r.slackChannelCandidates(ctx, channel)
		if err != nil {
			return listChannels{}, err
		}
		found := false
		for _, id := range candidates {
			if !slices.Contains(readable, events.ScopeChannel(id)) {
				continue
			}
			found = true
			if !slices.Contains(out.slack, id) {
				out.slack = append(out.slack, id)
			}
		}
		var finder ChannelFinder
		if p := r.channelFinder.Load(); p != nil {
			finder = *p
		}
		if finder != nil {
			matches, err := finder.FindChannels(ctx, channel)
			if err != nil {
				return listChannels{}, fmt.Errorf("list: resolve channel: %w", err)
			}
			for _, m := range matches {
				if !slices.Contains(readable, m.Scope) {
					continue
				}
				found = true
				if !slices.Contains(out.others, m) {
					out.others = append(out.others, m)
				}
			}
		}
		if !found {
			return listChannels{}, fmt.Errorf("%w: %q is not a channel you can read here; check the name, or pass the channel's ID or URL", ErrListChannelNotFound, channel)
		}
	}
	return out, nil
}

// slackChannelCandidates is the channel IDs one Channels entry may mean,
// before any visibility check: the ID in a URL, or else every channel the
// directory holds under that name and the entry itself when it is an ID.
func (r *Retriever) slackChannelCandidates(ctx context.Context, channel string) ([]string, error) {
	if m := slackArchiveChannelRE.FindStringSubmatch(channel); m != nil {
		return []string{m[1]}, nil
	}
	name := strings.TrimPrefix(channel, "#")
	rows, err := r.pool.Query(ctx,
		`SELECT channel_id FROM slack_channel_info WHERE name = $1 ORDER BY channel_id`, strings.ToLower(name))
	if err != nil {
		return nil, fmt.Errorf("list: resolve channel: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list: resolve channel: %w", err)
	}
	// A name typed in capitals has an ID's shape, so an ID-shaped entry is
	// tried as both.
	if slackChannelIDRE.MatchString(name) {
		ids = append(ids, name)
	}
	return ids, nil
}

// scanListRows projects events rows into Hit. Score = 0 and
// ChunkID = 0 are sentinels signalling "this came from List, not
// Retrieve" — the same Hit shape covers both paths so callers
// don't have to special-case render.
func scanListRows(rows pgx.Rows) ([]Hit, error) {
	out := make([]Hit, 0, ListDefaultTopK)
	for rows.Next() {
		var h Hit
		if err := rows.Scan(
			&h.EventID, &h.ExternalID, &h.Source,
			&h.Actor, &h.ActorCanonical, &h.ActorID, &h.ThreadID,
			&h.OccurredAt, &h.Title, &h.URL,
			&h.Content, &h.ChannelName,
		); err != nil {
			return nil, err
		}
		h.ChunkCount = 1 // List returns events, not chunks; conventional value.
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// trimFilterValues strips surrounding whitespace from each filter
// entry and drops entries that trim to empty. Model-emitted tool
// args occasionally carry a stray leading space (" alex@example.com"),
// which would silently match nothing through `= ANY($n)` — a wrong-
// answer failure, not an error. Returns the input slice unchanged
// (no alloc) when nothing needs fixing.
func trimFilterValues(vals []string) []string {
	clean := true
	for _, v := range vals {
		if strings.TrimSpace(v) != v || v == "" {
			clean = false
			break
		}
	}
	if clean {
		return vals
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// allEmailShaped reports whether every actor in the slice looks
// email-shaped (contains '@' and a '.' after it). Used to decide
// whether the actor filter can take the canonical-only fast path
// (composite-index friendly) or has to fall back to the OR-with-
// display path (which defeats the composite index's ordered output).
//
// Uses the same email-shape heuristic as the actor filter's canonical
// fast path. This is storage/query planning logic, not model formatting.
func allEmailShaped(actors []string) bool {
	for _, a := range actors {
		at := strings.IndexByte(a, '@')
		if at <= 0 {
			return false
		}
		if !strings.ContainsRune(a[at+1:], '.') {
			return false
		}
	}
	return true
}

// truncateRunes clips s to at most budget runes (NOT bytes — naive
// byte truncation would split multi-byte UTF-8, which is common in
// slack messages with emoji and non-ASCII names; the resulting
// invalid UTF-8 fails json.Marshal). Appends a marker so the model
// knows the body was clipped and can call `expand` for the full
// content.
//
// budget < 0 is treated as 0 (defensive against caller arithmetic
// that produces negatives when len(rows) is huge).
func truncateRunes(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= budget {
		return s
	}
	// Walk runes, count up to budget, slice at that byte position.
	// This is O(budget) rather than O(len(s)) of []rune(s).
	count := 0
	cut := 0
	for i := range s {
		if count == budget {
			cut = i
			break
		}
		count++
	}
	if cut == 0 {
		cut = len(s)
	}
	return s[:cut] + listTruncMarker
}
