package adminui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

var askSessionPage = mustPage("ask_session.html")

const (
	askTranscriptWindow    = 100
	askTranscriptWindowMax = 200
	askMaxTurn             = 1<<31 - 1
	askUsageTimeout        = 2 * time.Second
)

// askSessionData is one conversation: who asked what, where and when, what
// it cost, and the transcript as a sequence of exchanges.
type askSessionData struct {
	pageData
	ID       string
	Title    string
	Owner    string
	Place    askPlace
	Created  time.Time
	LastTurn time.Time
	// Took is the time spent answering, zero when unknown.
	Took  time.Duration
	State string
	Tone  string
	// Reason says why the latest question got no full answer, and
	// ReasonAnchor where on this page that happened, when it is shown.
	Reason       string
	ReasonAnchor string
	Model        string
	Questions    int64
	ToolCalls    int64
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	// Cost is the metered cost rounded for reading, CostExact as metered,
	// and CostNote how complete it is; all empty when unknown.
	Cost      string
	CostExact string
	CostNote  string
	// Messages is how many messages the conversation has stored.
	Messages   int
	ChannelID  string
	ThreadTS   string
	Release    *askReleaseView
	TitleModel string
	Incident   string
	Images     []askInputImageView
	Exchanges  []askExchangeView
	// Helpful and NotHelpful count the ratings people gave its answers in
	// Slack.
	Helpful    int64
	NotHelpful int64
	// Window is the part of a long conversation shown, nil when all of it
	// is.
	Window *askWindow
}

// askReleaseView is the prompt release the conversation's first turn used.
type askReleaseView struct {
	ID      int64
	Legacy  bool
	Created *time.Time
	By      string
	Note    string
}

// askWindow is a range of a long conversation's messages, by turn.
type askWindow struct {
	From, To, Total int
	EarlierURL      string
	LaterURL        string
}

// Pages is the pagination row of a window.
func (d askSessionData) Pages() pagination {
	if d.Window == nil {
		return pagination{}
	}
	return pagination{
		Summary: fmt.Sprintf("Messages %d–%d of %d", d.Window.From+1, d.Window.To+1, d.Window.Total),
		BackURL: d.Window.EarlierURL, BackLabel: "Earlier",
		ForwardURL: d.Window.LaterURL, ForwardLabel: "Later",
	}
}

type askInputImageView struct {
	ID        string
	Filename  string
	Permalink string
	ProxyURL  string
	// Described reports whether a description of the image reached the
	// model.
	Described     bool
	FailureReason string
	// Caption is the description the model read; CurrentCaption the one
	// stored now, when it has changed since.
	Caption        string
	CurrentCaption string
}

// askExchangeView is one question and what followed it: what Diderot did,
// then its answer, or why there is none.
type askExchangeView struct {
	Anchor string
	// Continued marks a window that starts after this exchange's question.
	Continued bool
	Asked     time.Time
	Asker     string
	Question  template.HTML
	// Context is the identity and time sent with the question, Thread the
	// earlier Slack messages sent as background, References the linked
	// items read for it, and Other any input outside those.
	Context    string
	Thread     string
	References string
	Other      string
	Steps      []askStepView
	// Took is how long Diderot worked on the question, zero when unknown.
	Took time.Duration
	// Answers are how the question ended, in order: usually one answer.
	Answers []askAnswerView
	// Running marks the question Diderot is working on now; Continues one
	// whose answer is in the next window.
	Running   bool
	Continues bool
}

// stopped is the exchange's last ending when that ending is not an answer.
func (e askExchangeView) stopped() *askAnswerView {
	if len(e.Answers) == 0 || e.Answers[len(e.Answers)-1].Stopped == "" {
		return nil
	}
	return &e.Answers[len(e.Answers)-1]
}

// askStepView is one thing Diderot did while answering: a tool call (Kind
// "tool"), its reasoning ("reasoning"), a remark it made between calls
// ("note"), or a block this page does not know ("unknown").
type askStepView struct {
	Anchor string
	Kind   string
	Tool   string
	CallID string
	// Asked is what the call was for, Found what came back.
	Asked  string
	Found  string
	Took   time.Duration
	Failed bool
	Error  string
	Input  string
	// Returned reports whether the call's result is stored: Output, of
	// OutputSize bytes.
	Returned   bool
	Output     string
	OutputSize int
	Text       template.HTML
	Raw        string
	input      json.RawMessage
}

// askAnswerView is how a question ended: an answer as delivered, or, when
// Stopped says why, none, with what the model wrote before it stopped in
// Body.
type askAnswerView struct {
	Anchor       string
	At           time.Time
	Body         template.HTML
	InputTokens  int64
	OutputTokens int64
	ResponseID   string
	// Notice qualifies an answer delivered with a reason, such as a linked
	// item it could not read.
	Notice  string
	Stopped string
	// Chosen names an ending that whoever asked chose, which is not a
	// failure: "Stopped" (the stop button) or "Cancelled" (an API or MCP
	// caller that went away).
	Chosen string
	// Ratings are the thumbs people gave the answer in Slack, and
	// Reactions the other reactions left on it.
	Ratings   []answerRating
	Reactions []feedbackReaction
}

// askToolTiming is a tool call's recorded latency and outcome.
type askToolTiming struct {
	Took  time.Duration
	Error string
	// Failed reports a call recorded as an error.
	Failed bool
}

// askRunView is one recorded turn: how long it took and how it ended.
type askRunView struct {
	Took    time.Duration
	Outcome string
	Error   string
	Model   string
}

func (s *Server) handleAskSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderNotFound(w, r, missingConversation)
		return
	}
	fromTurn, toTurn, err := parseAskTranscriptWindow(r.URL.Query())
	if err != nil {
		// A window that cannot be shown: show the conversation from its start.
		http.Redirect(w, r, askListPath+"/"+id.String(), http.StatusSeeOther)
		return
	}
	data, err := s.loadAskSessionWindow(r.Context(), id, fromTurn, toTurn)
	if errors.Is(err, ask.ErrSessionNotFound) {
		s.renderNotFound(w, r, missingConversation)
		return
	}
	if err != nil {
		s.logger.Error("admin ask transcript query failed", "session_id", id, "err", err)
		http.Error(w, "ask transcript query failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, data.Title, navAsk)
	data.Stylesheet = "asks.css"
	s.render(w, askSessionPage, data)
}

func (s *Server) loadAskSession(ctx context.Context, id uuid.UUID) (askSessionData, error) {
	return s.loadAskSessionWindow(ctx, id, 0, askTranscriptWindow-1)
}

func parseAskTranscriptWindow(q url.Values) (int, int, error) {
	fromRaw, toRaw := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	from, to := 0, askTranscriptWindow-1
	var err error
	if fromRaw != "" {
		from, err = strconv.Atoi(fromRaw)
		if err != nil || from < 0 || from > askMaxTurn-askTranscriptWindow+1 {
			return 0, 0, fmt.Errorf("from must be a turn number between 0 and %d", askMaxTurn-askTranscriptWindow+1)
		}
		to = from + askTranscriptWindow - 1
	}
	if toRaw != "" {
		to, err = strconv.Atoi(toRaw)
		if err != nil || to < 0 || to > askMaxTurn {
			return 0, 0, fmt.Errorf("to must be a turn number between 0 and %d", askMaxTurn)
		}
		if fromRaw == "" {
			from = max(0, to-askTranscriptWindow+1)
		}
	}
	if to < from {
		return 0, 0, errors.New("to must be greater than or equal to from")
	}
	if to-from+1 > askTranscriptWindowMax {
		return 0, 0, fmt.Errorf("turn window cannot exceed %d messages", askTranscriptWindowMax)
	}
	return from, to, nil
}

func (s *Server) loadAskSessionWindow(ctx context.Context, id uuid.UUID, fromTurn, toTurn int) (askSessionData, error) {
	manager := ask.NewManager(s.pool, s.logger)
	session, messages, totalMessages, err := manager.InspectWindow(ctx, id, &fromTurn, &toTurn)
	if err != nil {
		return askSessionData{}, err
	}
	if totalMessages > 0 && fromTurn >= totalMessages {
		fromTurn = max(0, totalMessages-askTranscriptWindow)
		toTurn = totalMessages - 1
		session, messages, totalMessages, err = manager.InspectWindow(ctx, id, &fromTurn, &toTurn)
		if err != nil {
			return askSessionData{}, err
		}
	}
	data := askSessionData{
		ID: id.String(), Created: session.CreatedAt, LastTurn: session.LastTurnAt, Messages: totalMessages,
	}
	if fromTurn > 0 || toTurn < totalMessages-1 {
		window := &askWindow{From: fromTurn, To: min(toTurn, max(totalMessages-1, 0)), Total: totalMessages}
		if fromTurn > 0 {
			previousTo := fromTurn - 1
			window.EarlierURL = fmt.Sprintf("?from=%d&to=%d", max(0, previousTo-askTranscriptWindow+1), previousTo)
		}
		if toTurn < totalMessages-1 {
			nextFrom := toTurn + 1
			window.LaterURL = fmt.Sprintf("?from=%d&to=%d", nextFrom, min(totalMessages-1, nextFrom+askTranscriptWindow-1))
		}
		data.Window = window
	}

	var surface, channelName, title, runError, incomplete, reportID, reportName string
	var releaseID *int64
	var releaseCreated *time.Time
	var releaseBy, releaseNote string
	var releaseLegacy bool
	var tookMS *int64
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(s.owner_email, ''), COALESCE(sl.channel_id, ''), COALESCE(ci.name, ''),
		       COALESCE(sl.thread_ts, ''), COALESCE(s.title, ''), `+askSurfaceSQL+`,
		       COALESCE(s.initial_model_id, ''), COALESCE(s.prompt_release_id, lsp.legacy_prompt_id),
		       COALESCE(p.created_at, lp.created_at),
		       COALESCE(p.created_by, lp.created_by, ''), COALESCE(p.note, lp.note, ''),
		       (s.prompt_release_id IS NULL AND lsp.legacy_prompt_id IS NOT NULL),
		       COALESCE(s.title_model, ''),
		       st.state, st.run_error, st.incomplete_reason,
		       totals.questions, totals.tools, totals.input_tokens, totals.output_tokens,
		       totals.cache_read, totals.cache_write, runs.took_ms,
		       COALESCE(report.id::text, ''), COALESCE(report.name, ''), COALESCE(incident.incident_id, '')
		  FROM ask_sessions s
		  LEFT JOIN ask_slack_sessions sl ON sl.session_id = s.id
		  LEFT JOIN slack_channel_info ci ON ci.channel_id = sl.channel_id
		  LEFT JOIN prompt_releases p ON p.id = s.prompt_release_id
		  LEFT JOIN legacy_ask_session_prompts lsp ON lsp.session_id = s.id
		  LEFT JOIN legacy_ask_prompts lp ON lp.id = lsp.legacy_prompt_id
		 CROSS JOIN LATERAL (`+askStateSQL+`) st
		 CROSS JOIN LATERAL (
			SELECT COALESCE(sum(jsonb_array_length(jsonb_path_query_array(
			         m.message, '$.content_blocks[*] ? (@.type == "user_input_text")'))), 0)::bigint AS questions,
			       COALESCE(sum(jsonb_array_length(jsonb_path_query_array(
			         m.message, '$.content_blocks[*] ? (@.type == "function_tool_call")'))), 0)::bigint AS tools,
			       COALESCE(sum(m.input_tokens), 0)::bigint AS input_tokens,
			       COALESCE(sum(m.output_tokens), 0)::bigint AS output_tokens,
			       COALESCE(sum(m.cache_read_tokens), 0)::bigint AS cache_read,
			       COALESCE(sum(m.cache_write_tokens), 0)::bigint AS cache_write
			  FROM ask_messages m WHERE m.session_id = s.id
		  ) totals
		  LEFT JOIN LATERAL (
			SELECT sum(r.duration_ms)::bigint AS took_ms FROM ask_runs r WHERE r.session_id = s.id
		  ) runs ON true
		  LEFT JOIN LATERAL (
			SELECT sr.id, sr.name
			  FROM scheduled_report_runs rr
			  JOIN scheduled_reports sr ON sr.id = rr.report_id
			 WHERE s.surface = 'scheduled' AND rr.ask_session_id = s.id
			 ORDER BY rr.created_at DESC
			 LIMIT 1
		  ) report ON true
		  LEFT JOIN LATERAL (
			SELECT pi.incident_id FROM pagerduty_investigations pi
			 WHERE s.surface = 'pagerduty' AND pi.session_id = s.id
			 LIMIT 1
		  ) incident ON true
		 WHERE s.id = $1`, id).Scan(&data.Owner, &data.ChannelID, &channelName, &data.ThreadTS, &title, &surface,
		&data.Model, &releaseID, &releaseCreated, &releaseBy, &releaseNote, &releaseLegacy,
		&data.TitleModel, &data.State, &runError, &incomplete,
		&data.Questions, &data.ToolCalls, &data.InputTokens, &data.OutputTokens, &data.CacheRead, &data.CacheWrite,
		&tookMS, &reportID, &reportName, &data.Incident)
	if err != nil {
		return data, err
	}
	data.Title = askTitle(title)
	data.Tone = askStateTone(data.State)
	data.Reason = askStateReason(data.State, runError, incomplete)
	data.Place = newAskPlace(surface, data.ChannelID, channelName, reportName)
	switch {
	case data.Place.Kind == "report" && reportID != "":
		data.Place.Href = "/admin/reports/" + reportID
	case data.ThreadTS != "":
		data.Place.Href = slackAskURL(s.slackWorkspaceHost(), data.ChannelID, data.ThreadTS)
	}
	if releaseID != nil {
		data.Release = &askReleaseView{ID: *releaseID, Legacy: releaseLegacy, Created: releaseCreated, By: releaseBy, Note: releaseNote}
	}
	if tookMS != nil {
		data.Took = time.Duration(*tookMS) * time.Millisecond
	} else {
		data.Took = session.LastTurnAt.Sub(session.CreatedAt)
	}

	runs, err := s.askRuns(ctx, id)
	if err != nil {
		return data, err
	}
	if data.Model == "" {
		// A session from before the first model was stored: the first
		// recorded turn's.
		first := askMaxTurn
		for turn, run := range runs {
			if turn < first {
				first, data.Model = turn, run.Model
			}
		}
	}
	timings, err := s.askToolTimings(ctx, id)
	if err != nil {
		return data, err
	}
	if data.Images, err = s.askInputImages(ctx, id); err != nil {
		return data, err
	}
	more := data.Window != nil && data.Window.LaterURL != ""
	data.Exchanges = buildAskExchanges(messages, data.ChannelID != "", timings, runs, more, data.State == askRunning)
	ratings, reactions, err := s.askSessionRatings(ctx, id)
	if err != nil {
		return data, err
	}
	for _, turnRatings := range ratings {
		for _, rating := range turnRatings {
			if rating.Helpful {
				data.Helpful++
			} else {
				data.NotHelpful++
			}
		}
	}
	for e := range data.Exchanges {
		for a := range data.Exchanges[e].Answers {
			answer := &data.Exchanges[e].Answers[a]
			turn, ok := strings.CutPrefix(answer.Anchor, "turn-")
			if !ok {
				continue
			}
			if n, err := strconv.Atoi(turn); err == nil {
				answer.Ratings, answer.Reactions = ratings[n], reactions[n]
			}
		}
	}
	if data.Reason != "" && !more && len(data.Exchanges) > 0 {
		last := data.Exchanges[len(data.Exchanges)-1]
		if stop := last.stopped(); stop != nil {
			data.ReasonAnchor = cmp.Or(stop.Anchor, last.Anchor)
		}
	}

	if s.usageClient != nil {
		usageCtx, cancel := context.WithTimeout(ctx, askUsageTimeout)
		usage, usageErr := s.usageClient.GetWorkUsage(usageCtx, sessionfeed.DiderotAskWorkNamespace, id.String())
		cancel()
		if usageErr != nil {
			s.logger.Warn("admin ask session usage unavailable", "session_id", id, "err", usageErr)
		} else {
			data.Cost, data.CostExact, data.CostNote = formatAskCost(usage)
		}
	}
	return data, nil
}

// askRuns are the conversation's recorded turns by the turn of the question
// each answered; a retried question keeps its latest run.
func (s *Server) askRuns(ctx context.Context, id uuid.UUID) (map[int]askRunView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_turn, duration_ms, outcome, COALESCE(error, ''), model_id
		  FROM ask_runs
		 WHERE session_id = $1 AND user_turn IS NOT NULL
		 ORDER BY started_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("ask runs: %w", err)
	}
	defer rows.Close()
	runs := map[int]askRunView{}
	for rows.Next() {
		var turn int
		var tookMS int64
		var run askRunView
		if err := rows.Scan(&turn, &tookMS, &run.Outcome, &run.Error, &run.Model); err != nil {
			return nil, fmt.Errorf("ask runs scan: %w", err)
		}
		run.Took = time.Duration(tookMS) * time.Millisecond
		runs[turn] = run
	}
	return runs, rows.Err()
}

// askToolTimings are the conversation's recorded tool calls, by call ID.
func (s *Server) askToolTimings(ctx context.Context, id uuid.UUID) (map[string]askToolTiming, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.call_id, c.duration_ms, c.outcome, COALESCE(c.error, '')
		  FROM ask_runs r
		  JOIN ask_tool_calls c ON c.run_id = r.id
		 WHERE r.session_id = $1 AND c.workload = 'root' AND c.call_id IS NOT NULL`, id)
	if err != nil {
		return nil, fmt.Errorf("ask tool calls: %w", err)
	}
	defer rows.Close()
	timings := map[string]askToolTiming{}
	for rows.Next() {
		var callID, outcome, callErr string
		var tookMS int64
		if err := rows.Scan(&callID, &tookMS, &outcome, &callErr); err != nil {
			return nil, fmt.Errorf("ask tool calls scan: %w", err)
		}
		timings[callID] = askToolTiming{Took: time.Duration(tookMS) * time.Millisecond, Failed: outcome == "error", Error: callErr}
	}
	return timings, rows.Err()
}

func (s *Server) askInputImages(ctx context.Context, id uuid.UUID) ([]askInputImageView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.source, a.source_image_id, COALESCE(a.filename, ''),
		       COALESCE(a.permalink, ''),
		       COALESCE(a.caption_snapshot, ''), a.delivery_status,
		       COALESCE(a.failure_reason, ''), COALESCE(ei.storage_key, ''),
		       COALESCE(ec.content, '')
		  FROM ask_input_images a
		  LEFT JOIN event_images ei
		    ON ei.event_id = a.source_event_id
		   AND ei.source_image_id = a.source_image_id
		  LEFT JOIN event_chunks ec
		    ON ec.id = ei.chunk_id AND ec.kind = 'image_caption'
		 WHERE a.session_id = $1
		 ORDER BY a.created_at, a.source_turn_id, a.source_image_id`, id)
	if err != nil {
		return nil, fmt.Errorf("ask input images: %w", err)
	}
	defer rows.Close()
	var images []askInputImageView
	for rows.Next() {
		var source, delivery, storageKey, currentCaption string
		var image askInputImageView
		if err := rows.Scan(&source, &image.ID, &image.Filename, &image.Permalink,
			&image.Caption, &delivery, &image.FailureReason, &storageKey, &currentCaption); err != nil {
			return nil, fmt.Errorf("ask input images scan: %w", err)
		}
		if source == "slack" && storageKey != "" {
			image.ProxyURL = "/admin/ask/images/" + url.PathEscape(image.ID)
		}
		image.Described = delivery == "captioned"
		if currentCaption != image.Caption {
			image.CurrentCaption = currentCaption
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

// buildAskExchanges groups a transcript's messages into exchanges: each
// question starts one, and the tool calls, remarks and answer after it
// belong to it. A tool result joins its call. Nothing is left out: a block
// this page does not know is shown raw. more reports that later messages
// follow these; running that Diderot is working on the last question now.
func buildAskExchanges(messages []ask.StoredMessage, fromSlack bool, timings map[string]askToolTiming, runs map[int]askRunView, more, running bool) []askExchangeView {
	var exchanges []askExchangeView
	type position struct{ exchange, step int }
	calls := map[string]position{}
	questionTurns := map[int]int{}
	current := func() *askExchangeView {
		if len(exchanges) == 0 {
			exchanges = append(exchanges, askExchangeView{Continued: true})
		}
		return &exchanges[len(exchanges)-1]
	}
	for _, stored := range messages {
		message := stored.Message
		turnAnchor := fmt.Sprintf("turn-%d", stored.Turn)
		if message == nil {
			exchange := current()
			exchange.Steps = append(exchange.Steps, askStepView{
				Anchor: turnAnchor, Kind: "unknown", Asked: "A message that could not be read", Failed: true,
			})
			continue
		}
		if message.Role == schema.AgenticRoleTypeUser && ask.AgenticMessageText(message) != "" {
			exchange := newAskQuestion(ask.AgenticMessageText(message), fromSlack)
			exchange.Anchor, exchange.Asked = turnAnchor, stored.CreatedAt
			questionTurns[len(exchanges)] = stored.Turn
			exchanges = append(exchanges, exchange)
		}
		exchange := current()
		hasCalls := len(ask.AgenticMessageToolCalls(message)) > 0
		for index, block := range message.ContentBlocks {
			anchor := fmt.Sprintf("turn-%d-block-%d", stored.Turn, index)
			if block == nil {
				exchange.Steps = append(exchange.Steps, askStepView{Anchor: anchor, Kind: "unknown", Asked: "An empty block", Failed: true})
				continue
			}
			switch block.Type {
			case schema.ContentBlockTypeUserInputText:
				// The question itself.
			case schema.ContentBlockTypeAssistantGenText:
				if hasCalls && block.AssistantGenText != nil && strings.TrimSpace(block.AssistantGenText.Text) != "" {
					exchange.Steps = append(exchange.Steps, askStepView{
						Anchor: anchor, Kind: "note", Text: renderAskMarkdown(block.AssistantGenText.Text, fromSlack),
					})
				}
			case schema.ContentBlockTypeReasoning:
				if block.Reasoning != nil && strings.TrimSpace(block.Reasoning.Text) != "" {
					exchange.Steps = append(exchange.Steps, askStepView{
						Anchor: anchor, Kind: "reasoning", Asked: truncateRunes(plainAskText(block.Reasoning.Text), 120),
						Text: renderAskMarkdown(block.Reasoning.Text, false),
					})
				}
			case schema.ContentBlockTypeFunctionToolCall:
				step := newAskToolStep(block.FunctionToolCall)
				step.Anchor = anchor
				exchange.Steps = append(exchange.Steps, step)
				if step.CallID != "" {
					calls[step.CallID] = position{len(exchanges) - 1, len(exchange.Steps) - 1}
				}
			case schema.ContentBlockTypeFunctionToolResult:
				result := block.FunctionToolResult
				if result == nil {
					continue
				}
				if at, ok := calls[result.CallID]; ok && result.CallID != "" {
					fillAskToolResult(&exchanges[at.exchange].Steps[at.step], result)
					continue
				}
				// The call is in an earlier window.
				step := askStepView{Anchor: anchor, Kind: "tool", Tool: result.Name, CallID: result.CallID, Asked: "Called in earlier messages"}
				fillAskToolResult(&step, result)
				exchange.Steps = append(exchange.Steps, step)
			default:
				raw, _ := json.Marshal(block)
				exchange.Steps = append(exchange.Steps, askStepView{
					Anchor: anchor, Kind: "unknown", Asked: "A block of type " + string(block.Type), Raw: prettyJSON(raw),
				})
			}
		}
		text := ask.AgenticMessageText(message)
		// Only text ends an exchange: a message of reasoning or unknown
		// blocks alone is a step, shown above.
		if message.Role != schema.AgenticRoleTypeAssistant || hasCalls ||
			(strings.TrimSpace(text) == "" && stored.IncompleteReason == "") {
			continue
		}
		answer := askAnswerView{Anchor: turnAnchor, At: stored.CreatedAt, ResponseID: askResponseID(message)}
		if strings.TrimSpace(text) != "" {
			answer.Body = renderAskMarkdown(text, fromSlack)
		}
		if usage := ask.AgenticMessageUsage(message); usage != nil {
			answer.InputTokens, answer.OutputTokens = int64(usage.PromptTokens), int64(usage.CompletionTokens)
		}
		switch reason := ask.IncompleteReason(stored.IncompleteReason); reason {
		case "", ask.IncompleteReferenceUnavailable:
			answer.Notice = sentence(ask.IncompleteNotice(reason))
		default:
			answer.Stopped = sentence(ask.IncompleteNotice(reason))
		}
		exchange.Answers = append(exchange.Answers, answer)
	}

	for index := range exchanges {
		exchange := &exchanges[index]
		last := index == len(exchanges)-1
		turn, ok := questionTurns[index]
		if !ok && exchange.Continued && len(messages) > 0 {
			// The question is in an earlier window: its run is the latest
			// one recorded at or before this window's first turn.
			for candidate := range runs {
				if candidate <= messages[0].Turn && (!ok || candidate > turn) {
					turn, ok = candidate, true
				}
			}
		}
		if ok {
			// A recorded error decides how the question ended, even after
			// the model's answer was stored, unless the question is still
			// being worked on or continues past this window.
			if run, ok := runs[turn]; ok {
				exchange.Took = run.Took
				unfinished := last && (more || running)
				ended, chosen := "", ""
				switch {
				case unfinished:
				case run.Outcome == "error" && run.Error != "":
					ended = compactText(run.Error, 600)
				case run.Outcome == "stopped":
					ended, chosen = "The person who asked stopped this answer.", "Stopped"
				case run.Outcome == "cancelled":
					ended, chosen = "The caller cancelled this question before it was answered.", "Cancelled"
				}
				if ended != "" {
					if len(exchange.Answers) == 0 {
						exchange.Answers = append(exchange.Answers, askAnswerView{Stopped: ended, Chosen: chosen})
					} else if answer := &exchange.Answers[len(exchange.Answers)-1]; answer.Stopped == "" {
						answer.Stopped, answer.Chosen = ended, chosen
					}
				}
			}
		}
		for step := range exchange.Steps {
			if timing, ok := timings[exchange.Steps[step].CallID]; ok && exchange.Steps[step].CallID != "" {
				exchange.Steps[step].Took = timing.Took
				if timing.Failed {
					exchange.Steps[step].Failed = true
					if exchange.Steps[step].Error == "" {
						exchange.Steps[step].Error = timing.Error
					}
				}
			}
		}
		if len(exchange.Answers) > 0 {
			continue
		}
		switch {
		case last && more:
			exchange.Continues = true
		case last && running:
			exchange.Running = true
		default:
			exchange.Answers = append(exchange.Answers, askAnswerView{Stopped: "No answer was recorded."})
		}
	}
	return exchanges
}

// newAskQuestion reads a question from the input Diderot stored for it: the
// envelope of context, thread background, question and linked items each
// turn wraps the asker's words in, or the bare words of an older turn.
func newAskQuestion(input string, fromSlack bool) askExchangeView {
	var exchange askExchangeView
	sections, ok := parseAskPromptEnvelope(input)
	if !ok {
		exchange.Question = renderAskMarkdown(input, fromSlack)
		return exchange
	}
	var other []string
	for _, section := range sections {
		switch section.Kind {
		case "context":
			exchange.Context = prettyJSON(json.RawMessage(section.Text))
			exchange.Asker = askAskerName(section.Text)
		case "thread_context":
			exchange.Thread = section.Text
		case "question":
			exchange.Question = renderAskMarkdown(section.Text, fromSlack)
		case "exact_references":
			exchange.References = section.Text
		default:
			other = append(other, section.Text)
		}
	}
	exchange.Other = strings.Join(other, "\n\n")
	if exchange.Question == "" && exchange.Other != "" {
		exchange.Question, exchange.Other = renderAskMarkdown(exchange.Other, fromSlack), ""
	}
	return exchange
}

// askAskerName is the asker the per-turn context names: their name, else
// their email, else their Slack handle.
func askAskerName(context string) string {
	var value struct {
		Asker *struct {
			DisplayName string `json:"display_name"`
			Email       string `json:"email"`
			SlackHandle string `json:"slack_handle"`
		} `json:"asker"`
	}
	if json.Unmarshal([]byte(context), &value) != nil || value.Asker == nil {
		return ""
	}
	for _, name := range []string{value.Asker.DisplayName, value.Asker.Email, value.Asker.SlackHandle} {
		if strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

type askPromptSection struct {
	Kind string
	Text string
}

// askPromptEnvelopeTags are the top-level blocks a turn's input may hold.
var askPromptEnvelopeTags = []string{"context", "thread_context", "question", "exact_references"}

// parseAskPromptEnvelope separates the known, server-generated top-level
// blocks without parsing the nested XML-like reference payload. Any bytes
// outside known blocks stay visible as additional input. A malformed known
// block fails closed to the caller's whole-input fallback.
func parseAskPromptEnvelope(input string) ([]askPromptSection, bool) {
	var sections []askPromptSection
	remaining := input
	found := false
	for len(remaining) > 0 {
		best, selected := -1, ""
		for _, tag := range askPromptEnvelopeTags {
			index := strings.Index(remaining, "<"+tag+">")
			if index >= 0 && (best < 0 || index < best) {
				best, selected = index, tag
			}
		}
		if best < 0 {
			if text := strings.TrimSpace(remaining); text != "" {
				sections = append(sections, askPromptSection{Kind: "additional", Text: text})
			}
			break
		}
		if text := strings.TrimSpace(remaining[:best]); text != "" {
			sections = append(sections, askPromptSection{Kind: "additional", Text: text})
		}
		open, closing := "<"+selected+">", "</"+selected+">"
		bodyStart := best + len(open)
		bodyEnd := strings.Index(remaining[bodyStart:], closing)
		if bodyEnd < 0 {
			return nil, false
		}
		bodyEnd += bodyStart
		sections = append(sections, askPromptSection{Kind: selected, Text: strings.TrimSpace(remaining[bodyStart:bodyEnd])})
		found = true
		remaining = remaining[bodyEnd+len(closing):]
	}
	return sections, found
}

func newAskToolStep(call *schema.FunctionToolCall) askStepView {
	if call == nil {
		return askStepView{Kind: "unknown", Asked: "An empty tool call", Failed: true}
	}
	input := json.RawMessage(call.Arguments)
	asked, _ := ask.DescribeToolCall(call.Name, input, nil)
	if asked == "" {
		asked = askGenericAsked(input)
	}
	return askStepView{Kind: "tool", Tool: call.Name, CallID: call.CallID, Asked: asked, Input: prettyJSON(input), input: input}
}

func fillAskToolResult(step *askStepView, result *schema.FunctionToolResult) {
	var text strings.Builder
	for _, content := range result.Content {
		if content != nil && content.Type == schema.FunctionToolResultContentBlockTypeText && content.Text != nil {
			text.WriteString(content.Text.Text)
		}
	}
	output := text.String()
	step.Returned, step.OutputSize = true, len(output)
	if output != "" {
		step.Output = prettyJSON(json.RawMessage(output))
	}
	if ask.IsRecoverableToolErrorPayload(output) {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(output), &payload)
		step.Failed, step.Error = true, payload.Error
		return
	}
	if _, found := ask.DescribeToolCall(step.Tool, step.input, json.RawMessage(output)); found != "" {
		step.Found = found
		return
	}
	step.Found = askGenericFound(output)
}

// askGenericAsked describes a call of a tool DescribeToolCall does not know,
// by the first of its common arguments, else by its compact input.
func askGenericAsked(input json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) != nil {
		return truncateRunes(compactText(string(input), 200), 100)
	}
	for _, key := range []string{"question", "query", "q", "request", "url", "path", "event_id", "id", "name", "title"} {
		if value, ok := fields[key]; ok {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				return truncateRunes(compactText(text, 200), 100)
			}
		}
	}
	if len(fields) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) != nil {
		return ""
	}
	return truncateRunes(compact.String(), 100)
}

// askGenericFound describes a result DescribeToolCall does not: by the
// length of its one list, else by its size.
func askGenericFound(output string) string {
	if output == "" {
		return "Returned nothing"
	}
	var value any
	if json.Unmarshal([]byte(output), &value) == nil {
		switch typed := value.(type) {
		case []any:
			return fmt.Sprintf("Returned %s", plural(len(typed), "item"))
		case map[string]any:
			for _, key := range []string{"hits", "results", "items", "events", "matches", "issues", "data"} {
				if list, ok := typed[key].([]any); ok {
					return fmt.Sprintf("Returned %s", plural(len(list), "item"))
				}
			}
		}
	}
	return "Returned " + formatBytes(int64(len(output)))
}

// askResponseID is the provider's ID for the response a message holds.
func askResponseID(message *schema.AgenticMessage) string {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.OpenAIExtension == nil {
		return ""
	}
	return message.ResponseMeta.OpenAIExtension.ID
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

// formatAskCost is a metered cost rounded for reading, as metered, and a
// note on how complete it is. Diderot never derives a price from token
// counts: when the meter's coverage is incomplete the note says so rather
// than the gap reading as free.
func formatAskCost(usage *sessionfeed.Usage) (rounded, exact, note string) {
	if usage == nil {
		return "", "", ""
	}
	if usage.MeteredCostUSD != "" {
		symbol := strings.ToUpper(usage.Currency)
		if symbol == "" || symbol == "USD" {
			symbol = "$"
		} else {
			symbol += " "
		}
		exact = symbol + usage.MeteredCostUSD
		rounded = exact
		if amount, err := strconv.ParseFloat(usage.MeteredCostUSD, 64); err == nil {
			switch {
			case amount >= 100:
				rounded = fmt.Sprintf("%s%.0f", symbol, amount)
			case amount >= 0.01 || amount == 0:
				rounded = fmt.Sprintf("%s%.2f", symbol, amount)
			default:
				rounded = "<" + symbol + "0.01"
			}
		}
	}
	return rounded, exact, askCoverageNote(usage)
}

func askCoverageNote(usage *sessionfeed.Usage) string {
	if usage.CoverageComplete {
		if usage.HasEstimates {
			return "partly estimated"
		}
		return ""
	}
	var gaps []string
	if n := usage.UnknownBillingAttemptCount; n > 0 {
		gaps = append(gaps, plural(int(n), "call")+" with unknown billing")
	}
	if n := usage.UnpricedMeteredAttemptCount; n > 0 {
		gaps = append(gaps, plural(int(n), "call")+" without a price")
	}
	if usage.HasEstimates {
		gaps = append(gaps, "partly estimated")
	}
	if len(gaps) == 0 {
		return "incomplete"
	}
	return "incomplete: " + strings.Join(gaps, ", ")
}
