package adminui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var askFeedbackPage = mustPage("ask_feedback.html")

const (
	askFeedbackPath = "/admin/ask/feedback"
	// feedbackRateMinimum is how many ratings a span needs before the page
	// gives the share that were helpful: below it, one rating moves the
	// share by five points or more, and a rate reads as more than it is.
	feedbackRateMinimum = 20
	feedbackPageSize    = 50
	// feedbackReactionsShown is how many kinds of other reaction are named.
	feedbackReactionsShown = 8
)

// The rating filter's values.
const (
	ratingHelpful    = "up"
	ratingNotHelpful = "down"
)

// feedbackRange is a span of answers the Feedback page can show: answers
// given in it, with their ratings. The spans are whole weeks, so the chart's
// weekly bars each cover seven days and add up to the counts above them.
type feedbackRange struct {
	Key   string
	Label string
	// Days is the span's length; zero for all time.
	Days int
	// BarDays is the length of the chart's bars; zero for calendar months.
	BarDays int
	// BarName says one bar in "Ratings per week".
	BarName string
	// During ends a sentence: "in the last 4 weeks".
	During string
	// Before names the span just before this one: "the 4 weeks before".
	Before string
}

var feedbackRanges = []feedbackRange{
	{Key: "7d", Label: "7 days", Days: 7, BarDays: 1, BarName: "day", During: "in the last 7 days", Before: "the 7 days before"},
	{Key: "4w", Label: "4 weeks", Days: 28, BarDays: 7, BarName: "week", During: "in the last 4 weeks", Before: "the 4 weeks before"},
	{Key: "13w", Label: "13 weeks", Days: 91, BarDays: 7, BarName: "week", During: "in the last 13 weeks", Before: "the 13 weeks before"},
	{Key: "all", Label: "All time", BarName: "month", During: "since the first rating"},
}

// defaultFeedbackRange is a quarter: at a few ratings a week, a shorter span
// holds too few to say much.
const defaultFeedbackRange = "13w"

func pickFeedbackRange(key string) feedbackRange {
	for _, r := range feedbackRanges {
		if r.Key == key {
			return r
		}
	}
	return pickFeedbackRange(defaultFeedbackRange)
}

// span is the range's start before until, nil for all time.
func (r feedbackRange) span(until time.Time) *time.Time {
	if r.Days == 0 {
		return nil
	}
	since := until.AddDate(0, 0, -r.Days)
	return &since
}

// feedbackTally counts the ratings of the answers given in a span. A rating
// is one person's verdict on one answer: their latest thumbs up or down on
// it, by feedback button or by reaction, on any of the messages the answer
// was posted as.
type feedbackTally struct {
	Helpful    int64
	NotHelpful int64
	// RatedAnswers have at least one rating; Answers are every answer
	// posted in Slack in the span, which people could rate.
	RatedAnswers int64
	Answers      int64
}

// Ratings is how many ratings the span holds.
func (t feedbackTally) Ratings() int64 { return t.Helpful + t.NotHelpful }

// Enough reports whether the span holds enough ratings for a rate.
func (t feedbackTally) Enough() bool { return t.Ratings() >= feedbackRateMinimum }

// HelpfulShare is the helpful ratings' share of all of them, in percent.
func (t feedbackTally) HelpfulShare() int64 {
	if t.Ratings() == 0 {
		return 0
	}
	return (100*t.Helpful + t.Ratings()/2) / t.Ratings()
}

// RatedShare is the share of answers that were rated, as the page writes it.
func (t feedbackTally) RatedShare() string {
	switch {
	case t.Answers == 0:
		return ""
	case t.RatedAnswers > 0 && 100*t.RatedAnswers < t.Answers:
		return "under 1%"
	}
	return fmt.Sprintf("%d%%", (100*t.RatedAnswers+t.Answers/2)/t.Answers)
}

// feedbackPlace is the ratings of the answers given in one kind of place.
type feedbackPlace struct {
	Label      string
	Helpful    int64
	NotHelpful int64
}

// feedbackReaction is how often one other reaction was left on answers.
type feedbackReaction struct {
	Emoji string
	Count int64
}

// ratedAnswer is one answer that people rated, with its ratings.
type ratedAnswer struct {
	SessionID string
	Turn      int
	Title     string
	Place     askPlace
	Answered  time.Time
	// Preview is the start of the answer as plain text.
	Preview    string
	Helpful    int64
	NotHelpful int64
	Ratings    []answerRating
}

// Href opens the conversation at the answer.
func (a ratedAnswer) Href() string {
	return askAnswerHref(a.SessionID, a.Turn)
}

// Verdict is the answer's ratings in one badge: helpful, not helpful, or
// mixed when people disagreed.
func (a ratedAnswer) Verdict() (tone, word string) {
	switch {
	case a.NotHelpful > 0 && a.Helpful > 0:
		return toneAttention, "mixed"
	case a.NotHelpful > 0:
		return toneBad, "not helpful"
	}
	return toneGood, "helpful"
}

// Tone and Word are Verdict's two halves, for the template.
func (a ratedAnswer) Tone() string { tone, _ := a.Verdict(); return tone }
func (a ratedAnswer) Word() string { _, word := a.Verdict(); return word }

// answerRating is one person's rating of an answer.
type answerRating struct {
	Who     string
	Helpful bool
	At      time.Time
}

// askAnswerHref opens a conversation at one of its answers, in the window
// of its transcript that holds it.
func askAnswerHref(sessionID string, turn int) string {
	href := askListPath + "/" + sessionID
	if turn >= askTranscriptWindow {
		from := turn - askTranscriptWindow/2
		href += fmt.Sprintf("?from=%d&to=%d", from, from+askTranscriptWindow-1)
	}
	return href + fmt.Sprintf("#turn-%d", turn)
}

type askFeedbackData struct {
	pageData
	Range     feedbackRange
	RangeTabs tabSet
	// Tally is the span's; Previous the span's just before it, nil for all
	// time.
	Tally    feedbackTally
	Previous *feedbackTally
	Chart    barChart
	// Places split the ratings by where the answers were given, when more
	// than one kind of place has any.
	Places []feedbackPlace
	// Reactions are the other reactions left on the span's answers, most
	// left first, and OtherReactions how many there were in all.
	Reactions      []feedbackReaction
	OtherReactions int64
	// Rating is the list's filter: "", ratingHelpful or ratingNotHelpful.
	Rating     string
	RatingTabs tabSet
	Answers    []ratedAnswer
	NextURL    string
	// FilterError says a page link could not be read.
	FilterError string
}

// RatingHref lists the span's answers with a helpful ("up") or not-helpful
// ("down") rating.
func (d askFeedbackData) RatingHref(rating string) string {
	return "?range=" + d.Range.Key + "&rating=" + rating + "#answers-heading"
}

// RateNote says what the helpful share is of, or why there is none.
func (d askFeedbackData) RateNote() string {
	if !d.Tally.Enough() {
		return fmt.Sprintf("Not enough ratings for a rate: %d of the %d it takes", d.Tally.Ratings(), feedbackRateMinimum)
	}
	note := "Of " + countOf(d.Tally.Ratings(), "rating", "ratings")
	if d.Previous != nil && d.Previous.Enough() {
		note += fmt.Sprintf("; %d%% in %s", d.Previous.HelpfulShare(), d.Range.Before)
	}
	return note
}

// AnswersGiven is how many answers were given in Slack in the span, in
// words.
func (d askFeedbackData) AnswersGiven() string {
	return countOf(d.Tally.Answers, "answer", "answers")
}

// MoreReactions is how many other reactions the named kinds leave out.
func (d askFeedbackData) MoreReactions() int64 {
	more := d.OtherReactions
	for _, reaction := range d.Reactions {
		more -= reaction.Count
	}
	return more
}

// Pages is the pagination row under the list.
func (d askFeedbackData) Pages() pagination {
	return pagination{
		Summary:    fmt.Sprintf("%s shown, newest first", plural(len(d.Answers), "rated answer")),
		ForwardURL: d.NextURL,
	}
}

func (s *Server) handleAskFeedback(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadAskFeedback(r.Context(), r.URL.Query(), time.Now())
	if err != nil {
		s.logger.Error("admin ask feedback query failed", "err", err)
		http.Error(w, "ask feedback query failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, "Feedback", navAsk)
	data.CurrentTab = tabAskFeedback
	data.Stylesheet = "asks.css"
	s.render(w, askFeedbackPage, data)
}

func (s *Server) loadAskFeedback(ctx context.Context, q url.Values, now time.Time) (askFeedbackData, error) {
	get := func(key string) string { return strings.TrimSpace(q.Get(key)) }
	data := askFeedbackData{Range: pickFeedbackRange(get("range")), Rating: get("rating")}
	if data.Rating != ratingHelpful && data.Rating != ratingNotHelpful {
		data.Rating = ""
	}
	since := data.Range.span(now)

	votes, err := s.loadFeedbackVotes(ctx, since, now)
	if err != nil {
		return data, err
	}
	data.Tally = tallyFeedback(votes)
	if data.Tally.Answers, err = s.countSlackAnswers(ctx, since, now); err != nil {
		return data, err
	}
	if since != nil {
		before := since.AddDate(0, 0, -data.Range.Days)
		previousVotes, err := s.loadFeedbackVotes(ctx, &before, *since)
		if err != nil {
			return data, err
		}
		previous := tallyFeedback(previousVotes)
		if previous.Answers, err = s.countSlackAnswers(ctx, &before, *since); err != nil {
			return data, err
		}
		data.Previous = &previous
	}
	data.Chart = feedbackChart(data.Range, votes, since, now)
	data.Places = feedbackPlaces(votes)
	if data.Reactions, data.OtherReactions, err = s.loadOtherReactions(ctx, since, now); err != nil {
		return data, err
	}

	base := "?range=" + data.Range.Key
	var up, down int64
	for _, answer := range answerVerdicts(votes) {
		if answer.helpful > 0 {
			up++
		}
		if answer.notHelpful > 0 {
			down++
		}
	}
	data.RatingTabs = tabSet{Label: "Ratings shown", Tabs: []navTab{
		{Label: "All", Href: base + "#answers-heading", Current: data.Rating == "", Count: data.Tally.RatedAnswers, Counted: true},
		{Label: "Not helpful", Href: base + "&rating=" + ratingNotHelpful + "#answers-heading", Current: data.Rating == ratingNotHelpful, Count: down, Counted: true},
		{Label: "Helpful", Href: base + "&rating=" + ratingHelpful + "#answers-heading", Current: data.Rating == ratingHelpful, Count: up, Counted: true},
	}}
	for _, r := range feedbackRanges {
		href := "?range=" + r.Key
		if data.Rating != "" {
			href += "&rating=" + data.Rating
		}
		data.RangeTabs.Tabs = append(data.RangeTabs.Tabs, navTab{Label: r.Label, Href: href, Current: r.Key == data.Range.Key})
	}
	data.RangeTabs.Label = "Answers given"

	var after *feedbackCursor
	if raw := get("before"); raw != "" {
		if parsed, ok := parseFeedbackCursor(raw); ok {
			after = &parsed
		} else {
			data.FilterError = "That page link is not valid, so this is the newest page."
		}
	}
	answers, more, err := s.loadRatedAnswers(ctx, since, now, data.Rating, after)
	if err != nil {
		return data, err
	}
	data.Answers = answers
	if more {
		last := answers[len(answers)-1]
		next := base
		if data.Rating != "" {
			next += "&rating=" + data.Rating
		}
		data.NextURL = next + "&before=" + formatFeedbackCursor(last) + "#answers-heading"
	}
	return data, nil
}

// feedbackVote is one rating, with the answer it rates.
type feedbackVote struct {
	SessionID uuid.UUID
	Turn      int
	Answered  time.Time
	Helpful   bool
	Surface   string
}

// feedbackAnswersSQL is the answers posted in Slack from $1 (none: from the
// first) until $2, each by when its first message was posted. Ratings and
// spans go by when an answer was given, not by when it was rated: a reaction
// is read again for days after it is left, and its time with it. The span
// filters the first message's time, so an answer whose messages straddle a
// span's edge belongs to one span only.
const feedbackAnswersSQL = `
	SELECT session_id, turn, min(created_at) AS answered_at
	  FROM ask_answer_messages
	 GROUP BY session_id, turn
	HAVING ($1::timestamptz IS NULL OR min(created_at) >= $1) AND min(created_at) < $2`

// feedbackVotesSQL is the ratings of the answers feedbackAnswersSQL names as
// "answers": each person's latest thumbs up or down on an answer still in
// place, whether a feedback button or a reaction, on any of its messages.
const feedbackVotesSQL = `
	SELECT DISTINCT ON (f.session_id, f.turn, f.reactor)
	       f.session_id, f.turn, f.reactor, f.verdict = 'up' AS helpful, f.added_at, a.answered_at
	  FROM answers a
	  JOIN ask_feedback f ON f.session_id = a.session_id AND f.turn = a.turn
	 WHERE f.removed_at IS NULL AND f.verdict IN ('up', 'down')
	 ORDER BY f.session_id, f.turn, f.reactor, f.added_at DESC`

// loadFeedbackVotes reads the ratings of the answers given from since (nil:
// from the first) until until.
func (s *Server) loadFeedbackVotes(ctx context.Context, since *time.Time, until time.Time) ([]feedbackVote, error) {
	rows, err := s.pool.Query(ctx, `
		WITH answers AS MATERIALIZED (`+feedbackAnswersSQL+`),
		votes AS (`+feedbackVotesSQL+`)
		SELECT v.session_id, v.turn, v.answered_at, v.helpful, `+askSurfaceSQL+`
		  FROM votes v
		  JOIN ask_sessions s ON s.id = v.session_id
		  LEFT JOIN LATERAL (
			SELECT session_id FROM ask_slack_sessions WHERE session_id = s.id LIMIT 1
		  ) sl ON true
		 ORDER BY v.answered_at, v.session_id, v.turn`, since, until)
	if err != nil {
		return nil, fmt.Errorf("ask feedback votes: %w", err)
	}
	defer rows.Close()
	var votes []feedbackVote
	for rows.Next() {
		var vote feedbackVote
		if err := rows.Scan(&vote.SessionID, &vote.Turn, &vote.Answered, &vote.Helpful, &vote.Surface); err != nil {
			return nil, fmt.Errorf("ask feedback votes scan: %w", err)
		}
		votes = append(votes, vote)
	}
	return votes, rows.Err()
}

// countSlackAnswers counts the answers posted in Slack from since until
// until: the answers people could rate.
func (s *Server) countSlackAnswers(ctx context.Context, since *time.Time, until time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+feedbackAnswersSQL+`) answers`, since, until).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ask feedback answers: %w", err)
	}
	return n, nil
}

type answerKey struct {
	session uuid.UUID
	turn    int
}

type answerCounts struct {
	helpful, notHelpful int64
}

// answerVerdicts counts each rated answer's ratings.
func answerVerdicts(votes []feedbackVote) map[answerKey]answerCounts {
	answers := map[answerKey]answerCounts{}
	for _, vote := range votes {
		key := answerKey{vote.SessionID, vote.Turn}
		counts := answers[key]
		if vote.Helpful {
			counts.helpful++
		} else {
			counts.notHelpful++
		}
		answers[key] = counts
	}
	return answers
}

func tallyFeedback(votes []feedbackVote) feedbackTally {
	var tally feedbackTally
	for _, vote := range votes {
		if vote.Helpful {
			tally.Helpful++
		} else {
			tally.NotHelpful++
		}
	}
	tally.RatedAnswers = int64(len(answerVerdicts(votes)))
	return tally
}

// feedbackPlaces splits the ratings by where the answers were given, only
// when they were given in more than one kind of place.
func feedbackPlaces(votes []feedbackVote) []feedbackPlace {
	byLabel := map[string]*feedbackPlace{}
	var places []*feedbackPlace
	for _, vote := range votes {
		label := askSurfaceLabel(vote.Surface)
		if vote.Surface == "slack" {
			label = "Conversations in Slack"
		}
		place, ok := byLabel[label]
		if !ok {
			place = &feedbackPlace{Label: label}
			byLabel[label] = place
			places = append(places, place)
		}
		if vote.Helpful {
			place.Helpful++
		} else {
			place.NotHelpful++
		}
	}
	if len(places) < 2 {
		return nil
	}
	out := make([]feedbackPlace, len(places))
	for i, place := range places {
		out[i] = *place
	}
	slices.SortFunc(out, func(a, b feedbackPlace) int {
		return int((b.Helpful + b.NotHelpful) - (a.Helpful + a.NotHelpful))
	})
	return out
}

// feedbackChart draws the span's ratings in bars of BarDays ending at until,
// or, for all time, in calendar months from the first rated answer's. Each
// bar's bad part is its not-helpful ratings.
func feedbackChart(r feedbackRange, votes []feedbackVote, since *time.Time, until time.Time) barChart {
	chart := barChart{
		Caption: "Ratings per " + r.BarName, BadLabel: "Not helpful", RestLabel: "Helpful",
		To: "Now",
	}
	var starts []time.Time
	if r.BarDays > 0 {
		bars := r.Days / r.BarDays
		for i := range bars {
			starts = append(starts, until.AddDate(0, 0, -(bars-i)*r.BarDays))
		}
		chart.From = plural(r.Days/r.BarDays, r.BarName) + " ago"
	} else {
		if len(votes) == 0 {
			return chart
		}
		first := votes[0].Answered.UTC()
		for month := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC); !month.After(until); month = month.AddDate(0, 1, 0) {
			starts = append(starts, month)
		}
		chart.From = starts[0].Format("Jan 2006")
	}
	chart.Bars = make([]chartBar, len(starts))
	for i, start := range starts {
		chart.Bars[i].Start = start
	}
	for _, vote := range votes {
		i, found := slices.BinarySearchFunc(starts, vote.Answered, func(start, at time.Time) int {
			return start.Compare(at)
		})
		if !found {
			i--
		}
		if i < 0 || (since != nil && vote.Answered.Before(*since)) {
			continue
		}
		chart.Bars[i].Count++
		if !vote.Helpful {
			chart.Bars[i].Bad++
		}
	}
	for i := range chart.Bars {
		bar := &chart.Bars[i]
		end := until
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		bar.Label = fmt.Sprintf("%s: %d helpful, %d not helpful", feedbackBarName(r, bar.Start, end), bar.Count-bar.Bad, bar.Bad)
	}
	var total, bad int64
	for _, bar := range chart.Bars {
		total += bar.Count
		bad += bar.Bad
	}
	chart.Summary = fmt.Sprintf("Ratings per %s of the answers given %s: %d helpful and %d not helpful in all, at most %d in one %s.",
		r.BarName, r.During, total-bad, bad, chart.Peak(), r.BarName)
	if total < feedbackRateMinimum {
		chart.Note = "Too few ratings to show a trend: read these as counts, not as a direction."
	}
	return chart
}

// feedbackBarName names the span of one bar: a day, a week's first and last
// days, or a month, in UTC.
func feedbackBarName(r feedbackRange, start, end time.Time) string {
	start, end = start.UTC(), end.UTC()
	switch r.BarDays {
	case 0:
		return start.Format("January 2006")
	case 1:
		return start.Format("Jan 2, 15:04") + " to " + end.Format("Jan 2, 15:04 UTC")
	}
	return start.Format("Jan 2") + " to " + end.Add(-time.Second).Format("Jan 2")
}

// loadOtherReactions counts the reactions other than thumbs left on the
// answers given in the span: the kinds left most, and how many in all. One
// person's emoji on an answer counts once, on however many of its messages,
// as the conversation page counts it.
func (s *Server) loadOtherReactions(ctx context.Context, since *time.Time, until time.Time) ([]feedbackReaction, int64, error) {
	rows, err := s.pool.Query(ctx, `
		WITH answers AS MATERIALIZED (`+feedbackAnswersSQL+`)
		SELECT f.emoji, count(DISTINCT (f.session_id, f.turn, f.reactor)) AS n
		  FROM answers a
		  JOIN ask_feedback f ON f.session_id = a.session_id AND f.turn = a.turn
		 WHERE f.removed_at IS NULL AND f.verdict = 'other'
		 GROUP BY f.emoji
		 ORDER BY n DESC, f.emoji`, since, until)
	if err != nil {
		return nil, 0, fmt.Errorf("ask feedback reactions: %w", err)
	}
	defer rows.Close()
	var reactions []feedbackReaction
	var total int64
	for rows.Next() {
		var reaction feedbackReaction
		if err := rows.Scan(&reaction.Emoji, &reaction.Count); err != nil {
			return nil, 0, fmt.Errorf("ask feedback reactions scan: %w", err)
		}
		total += reaction.Count
		if len(reactions) < feedbackReactionsShown {
			reactions = append(reactions, reaction)
		}
	}
	return reactions, total, rows.Err()
}

type feedbackCursor struct {
	Answered time.Time
	Session  uuid.UUID
	Turn     int
}

func formatFeedbackCursor(a ratedAnswer) string {
	return a.Answered.UTC().Format(time.RFC3339Nano) + "|" + a.SessionID + "|" + strconv.Itoa(a.Turn)
}

func parseFeedbackCursor(raw string) (feedbackCursor, bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 3 {
		return feedbackCursor{}, false
	}
	answered, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return feedbackCursor{}, false
	}
	session, err := uuid.Parse(parts[1])
	if err != nil {
		return feedbackCursor{}, false
	}
	// A turn is a Postgres integer.
	turn, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || turn < 0 {
		return feedbackCursor{}, false
	}
	return feedbackCursor{Answered: answered, Session: session, Turn: int(turn)}, true
}

// loadRatedAnswers lists the rated answers given in the span, newest first,
// after the cursor; rating narrows it to those with a helpful or a
// not-helpful rating. more reports a further page.
func (s *Server) loadRatedAnswers(ctx context.Context, since *time.Time, until time.Time, rating string, after *feedbackCursor) ([]ratedAnswer, bool, error) {
	var afterAt *time.Time
	var afterSession *uuid.UUID
	var afterTurn *int
	if after != nil {
		afterAt, afterSession, afterTurn = &after.Answered, &after.Session, &after.Turn
	}
	rows, err := s.pool.Query(ctx, `
		WITH answers AS MATERIALIZED (`+feedbackAnswersSQL+`),
		votes AS (`+feedbackVotesSQL+`),
		rated AS MATERIALIZED (
			SELECT v.session_id, v.turn, v.answered_at,
			       count(*) FILTER (WHERE v.helpful) AS helpful,
			       count(*) FILTER (WHERE NOT v.helpful) AS not_helpful,
			       jsonb_agg(jsonb_build_object(
			         'who', COALESCE(NULLIF((SELECT `+slackPersonName+` FROM slack_users WHERE user_id = v.reactor), ''), v.reactor),
			         'helpful', v.helpful, 'at', v.added_at) ORDER BY v.added_at) AS ratings
			  FROM votes v
			 GROUP BY v.session_id, v.turn, v.answered_at
			HAVING $3 = ''
			    OR ($3 = 'up' AND count(*) FILTER (WHERE v.helpful) > 0)
			    OR ($3 = 'down' AND count(*) FILTER (WHERE NOT v.helpful) > 0)
		), page AS MATERIALIZED (
			SELECT * FROM rated r
			 WHERE $4::timestamptz IS NULL OR (r.answered_at, r.session_id, r.turn) < ($4, $5::uuid, $6::int)
			 ORDER BY r.answered_at DESC, r.session_id DESC, r.turn DESC
			 LIMIT $7
		)
		SELECT p.session_id, p.turn, p.answered_at, p.helpful, p.not_helpful, p.ratings,
		       COALESCE(s.title, ''), `+askSurfaceSQL+`, COALESCE(sl.channel_id, ''), COALESCE(ci.name, ''),
		       COALESCE(answer.text, '')
		  FROM page p
		  JOIN ask_sessions s ON s.id = p.session_id
		  LEFT JOIN LATERAL (
			SELECT session_id, channel_id FROM ask_slack_sessions WHERE session_id = s.id LIMIT 1
		  ) sl ON true
		  LEFT JOIN slack_channel_info ci ON ci.channel_id = sl.channel_id
		  LEFT JOIN LATERAL (
			SELECT string_agg(b->'assistant_gen_text'->>'text', E'\n' ORDER BY block_no) AS text
			  FROM ask_messages m
			 CROSS JOIN LATERAL jsonb_array_elements(m.message->'content_blocks') WITH ORDINALITY blocks(b, block_no)
			 WHERE m.session_id = p.session_id AND m.turn = p.turn
			   AND b->>'type' = 'assistant_gen_text' AND b->'assistant_gen_text'->>'text' <> ''
		  ) answer ON true
		 ORDER BY p.answered_at DESC, p.session_id DESC, p.turn DESC`,
		since, until, rating, afterAt, afterSession, afterTurn, feedbackPageSize+1)
	if err != nil {
		return nil, false, fmt.Errorf("ask rated answers: %w", err)
	}
	defer rows.Close()
	var answers []ratedAnswer
	more := false
	for rows.Next() {
		var answer ratedAnswer
		var session uuid.UUID
		var ratings []byte
		var title, surface, channelID, channelName, text string
		if err := rows.Scan(&session, &answer.Turn, &answer.Answered, &answer.Helpful, &answer.NotHelpful, &ratings,
			&title, &surface, &channelID, &channelName, &text); err != nil {
			return nil, false, fmt.Errorf("ask rated answers scan: %w", err)
		}
		if len(answers) == feedbackPageSize {
			more = true
			break
		}
		answer.SessionID = session.String()
		answer.Title = askTitle(title)
		answer.Place = newAskPlace(surface, channelID, channelName, "")
		answer.Preview = truncateRunes(plainAskText(text), askPreviewLength)
		if answer.Ratings, err = decodeAnswerRatings(ratings); err != nil {
			return nil, false, err
		}
		answers = append(answers, answer)
	}
	return answers, more, rows.Err()
}

func decodeAnswerRatings(raw []byte) ([]answerRating, error) {
	var stored []struct {
		Who     string    `json:"who"`
		Helpful bool      `json:"helpful"`
		At      time.Time `json:"at"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("ask answer ratings: %w", err)
	}
	ratings := make([]answerRating, len(stored))
	for i, rating := range stored {
		ratings[i] = answerRating(rating)
	}
	return ratings, nil
}

// askSessionRatings are a conversation's ratings and other reactions, by
// the turn of the answer they were left on.
func (s *Server) askSessionRatings(ctx context.Context, id uuid.UUID) (map[int][]answerRating, map[int][]feedbackReaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (f.turn, f.reactor, f.verdict = 'other', CASE WHEN f.verdict = 'other' THEN f.emoji END)
		       f.turn, f.verdict, f.emoji, f.added_at,
		       COALESCE(NULLIF((SELECT `+slackPersonName+` FROM slack_users WHERE user_id = f.reactor), ''), f.reactor)
		  FROM ask_feedback f
		 WHERE f.session_id = $1 AND f.removed_at IS NULL
		 ORDER BY f.turn, f.reactor, f.verdict = 'other', CASE WHEN f.verdict = 'other' THEN f.emoji END, f.added_at DESC`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("ask session ratings: %w", err)
	}
	defer rows.Close()
	ratings := map[int][]answerRating{}
	reactions := map[int][]feedbackReaction{}
	for rows.Next() {
		var turn int
		var verdict, emoji, who string
		var at time.Time
		if err := rows.Scan(&turn, &verdict, &emoji, &at, &who); err != nil {
			return nil, nil, fmt.Errorf("ask session ratings scan: %w", err)
		}
		if verdict == "other" {
			index := slices.IndexFunc(reactions[turn], func(r feedbackReaction) bool { return r.Emoji == emoji })
			if index < 0 {
				reactions[turn] = append(reactions[turn], feedbackReaction{Emoji: emoji})
				index = len(reactions[turn]) - 1
			}
			reactions[turn][index].Count++
			continue
		}
		ratings[turn] = append(ratings[turn], answerRating{Who: who, Helpful: verdict == "up", At: at})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for turn := range ratings {
		slices.SortFunc(ratings[turn], func(a, b answerRating) int { return a.At.Compare(b.At) })
	}
	for turn := range reactions {
		slices.SortStableFunc(reactions[turn], func(a, b feedbackReaction) int { return int(b.Count - a.Count) })
	}
	return ratings, reactions, nil
}

// feedbackSignal is the Overview's line about ratings: those of the answers
// given in the shortest span that has any, nil when none has.
type feedbackSignal struct {
	Helpful    int64
	NotHelpful int64
	During     string
	Href       string
}

// loadFeedbackSignal reads the Overview's line: the last 7 days' ratings, or
// when there are none the last 4 weeks'.
func (s *Server) loadFeedbackSignal(ctx context.Context, now time.Time) (*feedbackSignal, error) {
	for _, key := range []string{"7d", "4w"} {
		r := pickFeedbackRange(key)
		votes, err := s.loadFeedbackVotes(ctx, r.span(now), now)
		if err != nil {
			return nil, err
		}
		tally := tallyFeedback(votes)
		if tally.Ratings() == 0 {
			continue
		}
		signal := &feedbackSignal{Helpful: tally.Helpful, NotHelpful: tally.NotHelpful, During: r.During, Href: askFeedbackPath + "?range=" + r.Key}
		if tally.NotHelpful > 0 {
			signal.Href += "&rating=" + ratingNotHelpful + "#answers-heading"
		}
		return signal, nil
	}
	return nil, nil
}
