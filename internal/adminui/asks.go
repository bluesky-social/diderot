package adminui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var askSessionsPage = mustPage("ask_sessions.html")

const (
	askPageSize = 50
	// askPreviewLength is how much of an answer a list row shows, in runes.
	askPreviewLength = 180
	askListPath      = "/admin/ask/sessions"
	untitledAsk      = "Untitled conversation"
)

// A conversation's state is its latest question's: still being worked on,
// answered, answered only in part, stopped by the person who asked,
// cancelled by the API or MCP caller that asked, or not answered.
const (
	askRunning    = "running"
	askAnswered   = "answered"
	askIncomplete = "incomplete"
	askStopped    = "stopped"
	askCancelled  = "cancelled"
	askFailed     = "failed"
)

// askStateSQL is a lateral subquery over the ask_sessions row s. It yields
// the conversation's state (askRunning and the rest), the error its latest
// turn recorded, and the incomplete reason of its last message when that
// message belongs to the latest turn. A turn holds a lease while it runs and
// records its outcome when it ends. When no recorded turn covers the last
// message (a session from before turns were recorded, or a turn that never
// finished), the last message decides: an answer, or not.
const askStateSQL = `
	SELECT CASE
	         WHEN EXISTS (SELECT 1 FROM ask_session_leases l WHERE l.session_id = s.id AND l.expires_at > now()) THEN 'running'
	         WHEN lr.completed_at IS NOT NULL
	              AND (lm.created_at IS NULL OR lr.completed_at >= lm.created_at) THEN
	              CASE lr.outcome WHEN 'answered' THEN 'answered' WHEN 'incomplete' THEN 'incomplete'
	                              WHEN 'stopped' THEN 'stopped' WHEN 'cancelled' THEN 'cancelled'
	                              ELSE 'failed' END
	         WHEN lm.answered THEN 'answered'
	         ELSE 'failed'
	       END AS state,
	       CASE WHEN lr.completed_at IS NULL OR lm.created_at IS NULL OR lr.completed_at >= lm.created_at
	            THEN COALESCE(lr.error, '') ELSE '' END AS run_error,
	       CASE WHEN lr.started_at IS NULL OR lm.created_at >= lr.started_at
	            THEN COALESCE(lm.incomplete_reason, '') ELSE '' END AS incomplete_reason
	  FROM (SELECT 1) one
	  LEFT JOIN LATERAL (
	    SELECT r.outcome, r.error, r.started_at, r.completed_at
	      FROM ask_runs r
	     WHERE r.session_id = s.id
	     ORDER BY r.started_at DESC, r.id DESC
	     LIMIT 1
	  ) lr ON true
	  LEFT JOIN LATERAL (
	    SELECT m.created_at, m.incomplete_reason,
	           m.role = 'assistant'
	           AND (m.incomplete_reason IS NULL OR m.incomplete_reason = 'reference_unavailable')
	           AND jsonb_path_exists(m.message, '$.content_blocks[*] ? (@.type == "assistant_gen_text" && @.assistant_gen_text.text != "")')
	           AND NOT jsonb_path_exists(m.message, '$.content_blocks[*] ? (@.type == "function_tool_call")') AS answered
	      FROM ask_messages m
	     WHERE m.session_id = s.id
	     ORDER BY m.turn DESC
	     LIMIT 1
	  ) lm ON true`

// askSurfaceSQL is where the ask_sessions row s began, with its Slack thread
// row sl: a stored surface, Slack for a mapped thread from before surfaces
// were stored, and otherwise unknown.
const askSurfaceSQL = `COALESCE(NULLIF(s.surface, ''), CASE WHEN sl.session_id IS NOT NULL THEN 'slack' ELSE 'unknown' END)`

// askStateTone is a state's badge tone.
func askStateTone(state string) string {
	switch state {
	case askAnswered:
		return toneGood
	case askRunning, askIncomplete:
		return toneAttention
	case askFailed:
		return toneBad
	}
	return toneNeutral
}

// askStateReason says why the latest question got no full answer, from what
// askStateSQL found; empty while the conversation is running or answered.
func askStateReason(state, runError, incomplete string) string {
	if state != askFailed && state != askIncomplete {
		return ""
	}
	if notice := ask.IncompleteNotice(ask.IncompleteReason(incomplete)); notice != "" {
		return sentence(notice)
	}
	if runError != "" {
		return askRunReason(runError)
	}
	return "No answer was recorded."
}

// askRunCauses are the causes of knownCauses a model call can fail with.
var askRunCauses = map[string]bool{
	"rate-limited": true, "too-large": true, "refused": true, "credentials": true,
	"provider-error": true, "unreachable": true,
}

// askInterrupted is the reason of a run whose context was cancelled.
const askInterrupted = "Interrupted: Diderot restarted or the request was cancelled before the answer was finished."

// askElsewhere is the reason of a follow-up Ask refused because it came from
// somewhere other than where its conversation began: a conversation's
// history holds what its first asker could read. askElsewhereError is what
// Ask records for it.
const (
	askElsewhere      = "Refused: the conversation began somewhere else, and this follow-up may not be allowed to read everything already in it."
	askElsewhereError = "session is not available to this audience surface"
)

var (
	// askNodePath is the tail the agent framework ends a run's error with:
	// a dashed rule, then the graph nodes the error passed through.
	askNodePath = regexp.MustCompile(`(?s)\s*-{8,}.*$`)
	// askErrorWrapper is one wrapper before a run error's cause.
	askErrorWrapper = regexp.MustCompile(`^(?:(?:ask service|Eino run)\s*:|\[[A-Za-z]+\])\s*`)
	// askToolFailure is the wrapper of a tool call's error, with the
	// tool's name where it has one.
	askToolFailure = regexp.MustCompile(`^failed to (?:invoke tool\[name:([^\s\]]+)[^\]]*\]:|invoke tool, toolName=([^\s,]+), err=|stream tool call \S+:)\s*`)
	// askModelFailure is the wrapper of a model call's error.
	askModelFailure = regexp.MustCompile(`^failed to create (?:streaming )?(?:response|chat completion|message)[a-z ]*:\s*`)
)

// askRunReason says why a run failed in one readable line. A cancelled run,
// a provider failure and a network failure read in plain words, as on the
// Failures page. Anything else loses what only wraps its cause: the service
// and framework prefixes, the node path, and a tool call's ID. The
// transcript keeps the error as recorded.
func askRunReason(runError string) string {
	cause := classifyFailure(runError)
	if cause.Key == causeInterrupted {
		return askInterrupted
	}
	if strings.Contains(runError, askElsewhereError) {
		return askElsewhere
	}
	if askRunCauses[cause.Key] {
		return cause.Message + "."
	}
	text := strings.TrimSpace(askNodePath.ReplaceAllString(runError, ""))
	subject := ""
	for {
		if next := askErrorWrapper.ReplaceAllString(text, ""); next != text {
			text = next
			continue
		}
		if match := askToolFailure.FindStringSubmatch(text); match != nil {
			text = text[len(match[0]):]
			if name := cmp.Or(match[1], match[2]); name != "" && subject == "" {
				subject = "The " + name + " tool failed"
			}
			subject = cmp.Or(subject, "A tool failed")
			continue
		}
		if match := askModelFailure.FindString(text); match != "" {
			text = text[len(match):]
			subject = cmp.Or(subject, "The model call failed")
			continue
		}
		break
	}
	if message := providerMessage(text); message != "" {
		text = message
	}
	text = compactText(text, 400)
	switch {
	case subject == "":
		return cmp.Or(text, "No answer was recorded.")
	case text == "":
		return subject + "."
	}
	return subject + ": " + text
}

// sentence starts text with a capital letter and ends it with a full stop.
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	text = string(unicode.ToUpper(first)) + text[size:]
	if !strings.HasSuffix(text, ".") {
		text += "."
	}
	return text
}

// askPlace is where a conversation happened.
type askPlace struct {
	// Kind is channel, dm, slack, api, report, pagerduty, or unknown.
	Kind  string
	Label string
	// Detail is the report's name, or a channel's ID while its name is
	// unknown.
	Detail string
	// Href is the Slack thread or the report, where there is one.
	Href string
}

func newAskPlace(surface, channelID, channelName, reportName string) askPlace {
	switch surface {
	case "slack":
		switch {
		case strings.HasPrefix(channelID, "D"):
			return askPlace{Kind: "dm", Label: "Slack DM"}
		case channelName != "":
			return askPlace{Kind: "channel", Label: "#" + channelName}
		case channelID != "":
			return askPlace{Kind: "channel", Label: "Slack", Detail: channelID}
		}
		return askPlace{Kind: "slack", Label: "Slack"}
	case "http":
		return askPlace{Kind: "api", Label: "API"}
	case "scheduled":
		return askPlace{Kind: "report", Label: "Scheduled report", Detail: reportName}
	case "pagerduty":
		return askPlace{Kind: "pagerduty", Label: "PagerDuty alert"}
	}
	return askPlace{Kind: "unknown", Label: "Unknown"}
}

// askSurfaceLabel names a stored surface.
func askSurfaceLabel(surface string) string {
	return newAskPlace(surface, "", "", "").Label
}

// The Where filter's values besides a Slack channel's ID.
const (
	askWhereSlack     = "slack"
	askWhereDM        = "dm"
	askWhereAPI       = "http"
	askWhereReport    = "scheduled"
	askWherePagerDuty = "pagerduty"
)

type askSessionsData struct {
	pageData
	Sessions []askSessionRow
	// The filters as submitted: Query is the search box.
	Query  string
	Range  string
	Owner  string
	Where  string
	Status string
	Tool   string
	From   string
	To     string
	// Channels are the Slack channels the Where filter offers; Tools the
	// tool names the Tool filter suggests.
	Channels    []askOption
	Tools       []string
	NextURL     string
	FilterError string
	// NoMatch says what the search box held when it was a conversation ID
	// or a Slack link that no conversation has.
	NoMatch string
}

type askOption struct {
	Value string
	Label string
}

// Filtered reports whether any filter narrows the list.
func (d askSessionsData) Filtered() bool {
	return d.Query != "" || d.Range != "all" || d.Owner != "" || d.Where != "" || d.Status != "" || d.MoreFiltered()
}

// MoreFiltered reports whether a filter behind "More filters" is set, so the
// disclosure stays open.
func (d askSessionsData) MoreFiltered() bool {
	return d.Tool != "" || d.From != "" || d.To != ""
}

// Pages is the pagination row under the list.
func (d askSessionsData) Pages() pagination {
	return pagination{
		Summary:    fmt.Sprintf("%s shown, newest first", plural(len(d.Sessions), "conversation")),
		ForwardURL: d.NextURL,
	}
}

type askSessionRow struct {
	ID      string
	Title   string
	Owner   string
	Place   askPlace
	Created time.Time
	// Took is the time spent answering, zero when unknown.
	Took  time.Duration
	State string
	Tone  string
	// Preview is the start of the latest answer as plain text.
	Preview string
	// Reason says why the latest question got no full answer.
	Reason string
}

type askSessionsCursor struct {
	Created time.Time
	ID      uuid.UUID
}

func (s *Server) handleAskSessions(w http.ResponseWriter, r *http.Request) {
	data, found, err := s.loadAskSessions(r.Context(), r)
	if err != nil {
		s.logger.Error("admin ask sessions query failed", "err", err)
		http.Error(w, "ask sessions query failed", http.StatusInternalServerError)
		return
	}
	if found != uuid.Nil {
		http.Redirect(w, r, askListPath+"/"+found.String(), http.StatusSeeOther)
		return
	}
	data.pageData = s.page(r, "Conversations", navAsk)
	data.Stylesheet = "asks.css"
	s.render(w, askSessionsPage, data)
}

// loadAskSessions reads the list. When the search box holds a conversation ID
// or a Slack link that resolves, it returns that conversation instead, for
// the handler to open.
func (s *Server) loadAskSessions(ctx context.Context, r *http.Request) (askSessionsData, uuid.UUID, error) {
	q := r.URL.Query()
	data := askSessionsData{
		Query: strings.TrimSpace(q.Get("q")), Range: strings.TrimSpace(q.Get("range")),
		Owner: strings.TrimSpace(q.Get("owner")), Where: strings.TrimSpace(q.Get("where")),
		Status: strings.TrimSpace(q.Get("status")), Tool: strings.TrimSpace(q.Get("tool")),
		From: strings.TrimSpace(q.Get("from")), To: strings.TrimSpace(q.Get("to")),
	}
	var problems []string
	if data.Range != "24h" && data.Range != "7d" && data.Range != "30d" {
		data.Range = "all"
	}
	switch data.Status {
	case "", askRunning, askAnswered, askIncomplete, askStopped, askCancelled, askFailed:
	default:
		problems = append(problems, "State must be running, answered, incomplete, stopped, cancelled, or failed.")
		data.Status = ""
	}
	switch data.Where {
	case "", askWhereSlack, askWhereDM, askWhereAPI, askWhereReport, askWherePagerDuty:
	default:
		if !validSlackChannel(data.Where) {
			problems = append(problems, "Where must be one of the places listed.")
			data.Where = ""
		}
	}

	now := time.Now()
	var createdAfter, createdBefore *time.Time
	if since := map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}[data.Range]; since > 0 {
		value := now.Add(-since)
		createdAfter = &value
	}
	if data.From != "" {
		if value, err := time.Parse(time.DateOnly, data.From); err != nil {
			problems = append(problems, "From must be a date.")
			data.From = ""
		} else if createdAfter == nil || value.After(*createdAfter) {
			createdAfter = &value
		}
	}
	if data.To != "" {
		if value, err := time.Parse(time.DateOnly, data.To); err != nil {
			problems = append(problems, "To must be a date.")
			data.To = ""
		} else {
			value = value.Add(24 * time.Hour)
			createdBefore = &value
		}
	}
	var before *time.Time
	var beforeID *uuid.UUID
	if raw := strings.TrimSpace(q.Get("before")); raw != "" {
		if parsed, ok := parseAskSessionsCursor(raw); ok {
			before, beforeID = &parsed.Created, &parsed.ID
		} else {
			problems = append(problems, "That page link is not valid, so this is the newest matching page.")
		}
	}
	data.FilterError = strings.Join(problems, " ")

	reference, found, isReference, err := s.resolveAskReference(ctx, data.Query)
	if err != nil {
		return data, uuid.Nil, err
	}
	if found {
		return data, reference, nil
	}
	if data.Channels, err = s.askChannelOptions(ctx, data.Where); err != nil {
		return data, uuid.Nil, err
	}
	if data.Tools, err = s.askToolNames(ctx); err != nil {
		return data, uuid.Nil, err
	}
	if isReference {
		data.NoMatch = "No conversation has the ID " + data.Query + "."
		if reference == uuid.Nil {
			data.NoMatch = "No conversation started from, or answered in, that Slack message."
		}
		return data, uuid.Nil, nil
	}

	title := data.Query
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT s.id, s.created_at, s.last_turn_at,
			       COALESCE(s.owner_email, '') AS owner_email,
			       `+askSurfaceSQL+` AS surface,
			       COALESCE(sl.channel_id, '') AS channel_id,
			       COALESCE(ci.name, '') AS channel_name,
			       COALESCE(s.title, '') AS title,
			       st.state, st.run_error, st.incomplete_reason
			  FROM ask_sessions s
			  LEFT JOIN ask_slack_sessions sl ON sl.session_id = s.id
			  LEFT JOIN slack_channel_info ci ON ci.channel_id = sl.channel_id
			 CROSS JOIN LATERAL (`+askStateSQL+`) st
			 WHERE ($1 = '' OR strpos(lower(COALESCE(s.title, '')), lower($1)) > 0)
			   AND ($2 = '' OR strpos(lower(COALESCE(s.owner_email, '')), lower($2)) > 0)
			   AND ($3 = ''
			        OR ($3 = 'dm' AND sl.channel_id LIKE 'D%')
			        OR ($3 IN ('slack', 'http', 'scheduled', 'pagerduty') AND `+askSurfaceSQL+` = $3)
			        OR sl.channel_id = $3)
			   AND ($4 = '' OR st.state = $4)
			   AND ($5::timestamptz IS NULL OR s.created_at >= $5)
			   AND ($6::timestamptz IS NULL OR s.created_at < $6)
			   AND ($7 = '' OR EXISTS (
			       SELECT 1 FROM ask_messages tm
			        CROSS JOIN LATERAL jsonb_array_elements(tm.message->'content_blocks') tb
			        WHERE tm.session_id = s.id
			          AND tb->>'type' = 'function_tool_call'
			          AND tb->'function_tool_call'->>'name' = $7))
			   AND ($8::timestamptz IS NULL OR (s.created_at, s.id) < ($8, $9::uuid))
			 ORDER BY s.created_at DESC, s.id DESC
			 LIMIT $10
		)
		SELECT c.id, c.created_at, c.last_turn_at, c.owner_email, c.surface, c.channel_id, c.channel_name,
		       c.title, c.state, c.run_error, c.incomplete_reason,
		       runs.took_ms, COALESCE(final_a.preview, ''), COALESCE(report.name, '')
		  FROM candidates c
		  LEFT JOIN LATERAL (
			SELECT sum(r.duration_ms)::bigint AS took_ms FROM ask_runs r WHERE r.session_id = c.id
		  ) runs ON true
		  LEFT JOIN LATERAL (
			SELECT b->'assistant_gen_text'->>'text' AS preview
			  FROM ask_messages lm
			 CROSS JOIN LATERAL jsonb_array_elements(lm.message->'content_blocks') WITH ORDINALITY blocks(b, block_no)
			 WHERE lm.session_id = c.id AND lm.role = 'assistant'
			   AND lm.turn = (SELECT max(last_m.turn) FROM ask_messages last_m WHERE last_m.session_id = c.id)
			   AND (lm.incomplete_reason IS NULL OR lm.incomplete_reason = 'reference_unavailable')
			   AND NOT jsonb_path_exists(lm.message, '$.content_blocks[*] ? (@.type == "function_tool_call")')
			   AND b->>'type' = 'assistant_gen_text'
			   AND b->'assistant_gen_text'->>'text' <> ''
			 ORDER BY lm.turn DESC, block_no DESC LIMIT 1
		  ) final_a ON true
		  LEFT JOIN LATERAL (
			SELECT sr.name
			  FROM scheduled_report_runs rr
			  JOIN scheduled_reports sr ON sr.id = rr.report_id
			 WHERE c.surface = 'scheduled' AND rr.ask_session_id = c.id
			 ORDER BY rr.created_at DESC
			 LIMIT 1
		  ) report ON true
		 ORDER BY c.created_at DESC, c.id DESC
	`, title, data.Owner, data.Where, data.Status, createdAfter, createdBefore, data.Tool,
		before, beforeID, askPageSize+1)
	if err != nil {
		return data, uuid.Nil, err
	}
	defer rows.Close()
	var lastCursor string
	for rows.Next() {
		var id uuid.UUID
		var created, last time.Time
		var owner, surface, channelID, channelName, rowTitle, state, runError, incomplete, preview, report string
		var tookMS *int64
		if err := rows.Scan(&id, &created, &last, &owner, &surface, &channelID, &channelName,
			&rowTitle, &state, &runError, &incomplete, &tookMS, &preview, &report); err != nil {
			return data, uuid.Nil, err
		}
		if len(data.Sessions) == askPageSize {
			next := cloneURLValues(q)
			next.Set("before", lastCursor)
			data.NextURL = "?" + next.Encode()
			break
		}
		row := askSessionRow{
			ID: id.String(), Title: askTitle(rowTitle), Owner: owner, Created: created,
			Place: newAskPlace(surface, channelID, channelName, report),
			State: state, Tone: askStateTone(state), Preview: truncateRunes(plainAskText(preview), askPreviewLength),
			Reason: askStateReason(state, runError, incomplete),
		}
		if tookMS != nil {
			row.Took = time.Duration(*tookMS) * time.Millisecond
		} else {
			row.Took = last.Sub(created)
		}
		data.Sessions = append(data.Sessions, row)
		lastCursor = formatAskSessionsCursor(created, id)
	}
	return data, uuid.Nil, rows.Err()
}

// askChannelOptions lists the Slack channels Ask has answered in, by name,
// for the Where filter. selected stays listed when it is not among them.
func (s *Server) askChannelOptions(ctx context.Context, selected string) ([]askOption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sl.channel_id, COALESCE(max(ci.name), '')
		  FROM ask_slack_sessions sl
		  JOIN ask_sessions s ON s.id = sl.session_id
		  LEFT JOIN slack_channel_info ci ON ci.channel_id = sl.channel_id
		 WHERE sl.channel_id NOT LIKE 'D%'
		 GROUP BY sl.channel_id
		 ORDER BY max(s.last_turn_at) DESC
		 LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("ask channels: %w", err)
	}
	defer rows.Close()
	var options []askOption
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("ask channels scan: %w", err)
		}
		option := askOption{Value: id, Label: id}
		if name != "" {
			option.Label = "#" + name
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ask channels rows: %w", err)
	}
	isChannel := selected != "" && validSlackChannel(selected)
	if isChannel && !slices.ContainsFunc(options, func(o askOption) bool { return o.Value == selected }) {
		options = append(options, askOption{Value: selected, Label: selected})
	}
	slices.SortFunc(options, func(a, b askOption) int {
		// Named channels first, alphabetically; IDs without a name after.
		if aNamed, bNamed := strings.HasPrefix(a.Label, "#"), strings.HasPrefix(b.Label, "#"); aNamed != bNamed {
			if aNamed {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return options, nil
}

// askToolNames suggests tool names for the Tool filter, from the root
// agent's recorded tool calls: a narrow table, where reading them out of the
// transcripts would decompress every message body of 1,000 sessions.
func (s *Server) askToolNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH recent_sessions AS MATERIALIZED (
			SELECT id FROM ask_sessions ORDER BY created_at DESC, id DESC LIMIT 1000
		)
		SELECT DISTINCT c.tool_name
		  FROM recent_sessions rs
		  JOIN ask_runs r ON r.session_id = rs.id
		  JOIN ask_tool_calls c ON c.run_id = r.id
		 WHERE c.workload = 'root'
		 ORDER BY c.tool_name
		 LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("ask tool names: %w", err)
	}
	tools, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("ask tool names: %w", err)
	}
	return tools, nil
}

// resolveAskReference reads the search box as a conversation ID or a Slack
// message link. isReference reports whether it was one; found whether a
// conversation has it; id is that conversation, or the ID read when none
// has it, and nil for a link no conversation has. A link resolves through
// the thread it names (a
// reply's link carries its thread), an answer Diderot posted, or, in a DM,
// the DM's one conversation.
func (s *Server) resolveAskReference(ctx context.Context, query string) (id uuid.UUID, found, isReference bool, err error) {
	if query == "" {
		return uuid.Nil, false, false, nil
	}
	if parsed, parseErr := uuid.Parse(query); parseErr == nil {
		err = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ask_sessions WHERE id = $1)`, parsed).Scan(&found)
		if err != nil {
			return uuid.Nil, false, true, fmt.Errorf("ask session lookup: %w", err)
		}
		return parsed, found, true, nil
	}
	channel, messageTS, ok := adminapi.ParseSlackPermalink(query)
	if !ok {
		return uuid.Nil, false, false, nil
	}
	threadTS := messageTS
	if link, parseErr := url.Parse(query); parseErr == nil {
		if thread := link.Query().Get("thread_ts"); validSlackThreadTS(thread) {
			threadTS = thread
		}
	}
	err = s.pool.QueryRow(ctx, `
		SELECT session_id FROM (
			SELECT session_id, 1 AS rank FROM ask_slack_sessions WHERE channel_id = $1 AND thread_ts = $2
			UNION ALL
			SELECT session_id, 2 FROM ask_slack_sessions WHERE channel_id = $1 AND thread_ts = $3
			UNION ALL
			SELECT session_id, 3 FROM ask_answer_messages WHERE channel_id = $1 AND message_ts = $3
			UNION ALL
			SELECT session_id, 4 FROM ask_slack_sessions WHERE channel_id = $1 AND thread_ts = 'dm' AND $1 LIKE 'D%'
		) matches
		ORDER BY rank
		LIMIT 1`, channel, threadTS, messageTS).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, true, nil
	}
	if err != nil {
		return uuid.Nil, false, true, fmt.Errorf("ask slack link lookup: %w", err)
	}
	return id, true, true, nil
}

func askTitle(title string) string {
	if strings.TrimSpace(title) == "" {
		return untitledAsk
	}
	return title
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, value := range values {
		clone[key] = append([]string(nil), value...)
	}
	return clone
}

func formatAskSessionsCursor(created time.Time, id uuid.UUID) string {
	return created.UTC().Format(time.RFC3339Nano) + "|" + id.String()
}

func parseAskSessionsCursor(raw string) (askSessionsCursor, bool) {
	createdRaw, idRaw, ok := strings.Cut(raw, "|")
	if !ok {
		return askSessionsCursor{}, false
	}
	created, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return askSessionsCursor{}, false
	}
	id, err := uuid.Parse(idRaw)
	if err != nil {
		return askSessionsCursor{}, false
	}
	return askSessionsCursor{Created: created, ID: id}, true
}

var (
	plainMarkdownLink = regexp.MustCompile(`\[([^\]\n]+)\]\((?:[^)\s]+)\)`)
	plainSlackLink    = regexp.MustCompile(`<((?:https?|mailto):[^|>\s]+)(?:\|([^>\n]+))?>`)
	plainStrong       = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__|~~([^~\n]+)~~`)
	plainEmphasis     = regexp.MustCompile(`\*([^*\s][^*\n]*)\*|\b_([^_\n]+)_\b`)
	plainCode         = regexp.MustCompile("`+([^`\n]*)`+")
	plainLinePrefix   = regexp.MustCompile(`^(?:#{1,6}\s+|>\s*|[-*+]\s+|[0-9]+[.)]\s+)`)
)

// plainAskText is an answer's Markdown (Ask's subset, Slack links included)
// as one line of plain text: link labels for links, no emphasis markers,
// headings, quote or list markers, or code fences.
func plainAskText(markdown string) string {
	var words []string
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			continue
		}
		line = plainLinePrefix.ReplaceAllString(line, "")
		line = plainMarkdownLink.ReplaceAllString(line, "$1")
		line = plainSlackLink.ReplaceAllStringFunc(line, func(link string) string {
			parts := plainSlackLink.FindStringSubmatch(link)
			if parts[2] != "" {
				return parts[2]
			}
			return parts[1]
		})
		line = plainStrong.ReplaceAllString(line, "$1$2$3")
		line = plainEmphasis.ReplaceAllString(line, "$1$2")
		line = plainCode.ReplaceAllString(line, "$1")
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " ")
}

// truncateRunes cuts text to at most limit runes, at a word boundary where
// one is near, and marks the cut with an ellipsis.
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := string(runes[:limit-1])
	if space := strings.LastIndexByte(cut, ' '); space > len(cut)*3/4 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:.") + "…"
}

func slackAskURL(host, channel, threadTS string) string {
	host = strings.TrimSpace(host)
	if host == "" || !validSlackChannel(channel) || !validSlackThreadTS(threadTS) {
		return ""
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return "https://" + parsed.Host + "/archives/" + url.PathEscape(channel) + "/p" + strings.ReplaceAll(threadTS, ".", "")
}

func validSlackChannel(channel string) bool {
	if channel == "" {
		return false
	}
	for _, r := range channel {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validSlackThreadTS(threadTS string) bool {
	seconds, fraction, ok := strings.Cut(threadTS, ".")
	if !ok || seconds == "" || fraction == "" {
		return false
	}
	for _, part := range []string{seconds, fraction} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ImageReader reads a Slack image an Ask conversation was given
// (*adminapi.Server).
type ImageReader interface {
	AskInputImage(ctx context.Context, sourceImageID string) (data []byte, mediaType string, err error)
}

// handleAskImage serves one Slack image to the Ask conversation page that
// embeds it. An administrator, who can read the conversation's whole
// transcript, sees every image attached to it, whatever Slack channels the
// administrator is in; an image no conversation holds answers 404, as one
// Diderot never stored does.
func (s *Server) handleAskImage(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		http.NotFound(w, r)
		return
	}
	data, mediaType, err := s.images.AskInputImage(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, adminapi.ErrImageNotFound), errors.Is(err, adminapi.ErrImageNotMirrored):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logger.Warn("ask image read failed", "image", r.PathValue("id"), "err", err)
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	// Only an image type is served inline; anything else a stored row
	// claims is downloaded, never rendered in the admin origin.
	if !strings.HasPrefix(mediaType, "image/") || strings.HasPrefix(mediaType, "image/svg") {
		mediaType = "application/octet-stream"
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(data)
}
