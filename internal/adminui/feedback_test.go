package adminui

import (
	"bytes"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestFeedbackTally(t *testing.T) {
	few := feedbackTally{Helpful: 6, NotHelpful: 1, RatedAnswers: 6, Answers: 900}
	assert.False(t, few.Enough(), "seven ratings make no rate")
	assert.Equal(t, "under 1%", few.RatedShare())
	enough := feedbackTally{Helpful: 17, NotHelpful: 3, RatedAnswers: 18, Answers: 400}
	assert.True(t, enough.Enough())
	assert.Equal(t, int64(85), enough.HelpfulShare())
	assert.Equal(t, "5%", enough.RatedShare())
	assert.Empty(t, feedbackTally{}.RatedShare())

	data := askFeedbackData{Range: pickFeedbackRange("4w"), Tally: few}
	assert.Equal(t, "Not enough ratings for a rate: 7 of the 20 it takes", data.RateNote())
	data.Tally, data.Previous = enough, &enough
	assert.Equal(t, "Of 20 ratings; 85% in the 4 weeks before", data.RateNote())

	assert.Equal(t, "13w", pickFeedbackRange("").Key)
	assert.Equal(t, "13w", pickFeedbackRange("90d").Key)
	assert.Nil(t, pickFeedbackRange("all").span(time.Now()))
}

func TestFeedbackChartBarsAddUpToTheSpan(t *testing.T) {
	until := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	r := pickFeedbackRange("4w")
	since := r.span(until)
	vote := func(daysAgo float64, helpful bool) feedbackVote {
		return feedbackVote{Answered: until.Add(-time.Duration(daysAgo * 24 * float64(time.Hour))), Helpful: helpful}
	}
	votes := []feedbackVote{vote(27.9, true), vote(20, false), vote(14, true), vote(13.9, true), vote(0.1, false), vote(0.2, true)}
	chart := feedbackChart(r, votes, since, until)
	require.Len(t, chart.Bars, 4, "four weeks, a week a bar")
	counts := make([]int64, 4)
	bad := make([]int64, 4)
	for i, bar := range chart.Bars {
		counts[i], bad[i] = bar.Count, bar.Bad
	}
	assert.Equal(t, []int64{1, 1, 2, 2}, counts, "a vote exactly at a bar's start belongs to that bar")
	assert.Equal(t, []int64{0, 1, 0, 1}, bad)
	assert.Equal(t, "Oct 21 to Oct 28: 1 helpful, 0 not helpful", chart.Bars[0].Label)
	assert.Equal(t, "4 weeks ago", chart.From)
	assert.NotEmpty(t, chart.Note, "six ratings show no trend")

	months := feedbackChart(pickFeedbackRange("all"), votes, nil, until)
	require.Len(t, months.Bars, 2, "October and November")
	assert.Equal(t, "Oct 2026", months.From)
	assert.Equal(t, int64(2), months.Bars[0].Count)
	assert.Equal(t, "November 2026: 3 helpful, 1 not helpful", months.Bars[1].Label)
	assert.Empty(t, feedbackChart(pickFeedbackRange("all"), nil, nil, until).Bars)
}

func TestFeedbackPlacesOnlyWhenTheyDiffer(t *testing.T) {
	assert.Nil(t, feedbackPlaces([]feedbackVote{{Surface: "slack", Helpful: true}, {Surface: "slack"}}))
	places := feedbackPlaces([]feedbackVote{{Surface: "pagerduty", Helpful: true}, {Surface: "slack", Helpful: true}, {Surface: "slack"}})
	assert.Equal(t, []feedbackPlace{
		{Label: "Conversations in Slack", Helpful: 1, NotHelpful: 1},
		{Label: "PagerDuty alert", Helpful: 1},
	}, places)
}

func TestAskAnswerHrefOpensTheWindowHoldingTheAnswer(t *testing.T) {
	assert.Equal(t, "/admin/ask/sessions/abc#turn-7", askAnswerHref("abc", 7))
	assert.Equal(t, "/admin/ask/sessions/abc?from=90&to=189#turn-140", askAnswerHref("abc", 140))
	from, to, err := parseAskTranscriptWindow(url.Values{"from": {"90"}, "to": {"189"}})
	require.NoError(t, err)
	assert.True(t, from <= 140 && 140 <= to)
}

func TestFeedbackCursorRoundTrip(t *testing.T) {
	answer := ratedAnswer{SessionID: uuid.NewString(), Turn: 9, Answered: time.Date(2026, 11, 2, 3, 4, 5, 600, time.UTC)}
	cursor, ok := parseFeedbackCursor(formatFeedbackCursor(answer))
	require.True(t, ok)
	assert.Equal(t, answer.SessionID, cursor.Session.String())
	assert.Equal(t, 9, cursor.Turn)
	assert.True(t, answer.Answered.Equal(cursor.Answered))
	for _, bad := range []string{"", "x|y", "2026-11-02T03:04:05Z|not-a-uuid|1", "2026-11-02T03:04:05Z|" + answer.SessionID + "|-1", "2026-11-02T03:04:05Z|" + answer.SessionID + "|2147483648"} {
		_, ok := parseFeedbackCursor(bad)
		assert.False(t, ok, bad)
	}
}

// feedbackFixture is a database of four rated answers, and one rated too
// long ago for the spans the test reads.
type feedbackFixture struct {
	server               *Server
	now                  time.Time
	platform, dm, pageme uuid.UUID
}

func newFeedbackFixture(t *testing.T) feedbackFixture {
	t.Helper()
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_feedback"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions, slack_users, slack_channel_info RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	f := feedbackFixture{
		server: &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		now:    time.Now(), platform: uuid.New(), dm: uuid.New(), pageme: uuid.New(),
	}
	old := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO ask_sessions (id, title, surface) VALUES
		($1, 'Who owns the deploy pipeline?', 'slack'), ($2, NULL, 'slack'),
		($3, 'Why did the latency alert fire?', 'pagerduty'), ($4, 'An old question', 'slack')`,
		f.platform, f.dm, f.pageme, old)
	exec(`INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id) VALUES
		('C0EXAMPLE1', '1700000000.000100', $1), ('D0EXAMPLE2', 'dm', $2), ('C0EXAMPLE3', '1700000000.000300', $3),
		('C0EXAMPLE1', '1600000000.000100', $4)`, f.platform, f.dm, f.pageme, old)
	exec(`INSERT INTO slack_channel_info (channel_id, name) VALUES ('C0EXAMPLE1', 'platform')`)
	exec(`INSERT INTO slack_users (user_id, real_name) VALUES ('U0EXAMPLE1', 'Ada Example'), ('U0EXAMPLE2', 'Grace Example')`)
	for _, session := range []uuid.UUID{f.platform, f.dm, f.pageme, old} {
		exec(`INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message) VALUES
			($1, 0, 'user', 1, '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"A synthetic question"}}]}'),
			($1, 1, 'assistant', 1, '{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"The **pipeline** belongs"}},{"type":"assistant_gen_text","assistant_gen_text":{"text":"to the platform team."}}]}')`, session)
	}
	// The platform answer was posted as two messages, an hour ago; the DM's
	// two days ago; the alert's ten days ago; the old one a year ago. One
	// more answer, in the DM, was never rated.
	exec(`INSERT INTO ask_answer_messages (channel_id, message_ts, session_id, turn, chunk_index, created_at) VALUES
		('C0EXAMPLE1', '1700000001.000001', $1, 1, 0, $5::timestamptz - interval '1 hour'),
		('C0EXAMPLE1', '1700000001.000002', $1, 1, 1, $5::timestamptz - interval '1 hour' + interval '1 second'),
		('D0EXAMPLE2', '1700000002.000001', $2, 1, 0, $5::timestamptz - interval '2 days'),
		('D0EXAMPLE2', '1700000002.000009', $2, 3, 0, $5::timestamptz - interval '3 days'),
		('D0EXAMPLE2', '1700000002.000005', $2, 5, 0, $5::timestamptz - interval '7 days' - interval '1 minute'),
		('D0EXAMPLE2', '1700000002.000006', $2, 5, 1, $5::timestamptz - interval '7 days' + interval '1 minute'),
		('C0EXAMPLE3', '1700000003.000001', $3, 1, 0, $5::timestamptz - interval '10 days'),
		('C0EXAMPLE1', '1600000001.000001', $4, 1, 0, $5::timestamptz - interval '400 days')`,
		f.platform, f.dm, f.pageme, old, f.now)
	exec(`INSERT INTO ask_feedback (session_id, turn, channel_id, message_ts, reactor, emoji, verdict, added_at, removed_at) VALUES
		-- Ada clicked helpful and reacted thumbs-up on both messages: one rating.
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE1', 'button:feedback', 'up', $5::timestamptz - interval '50 minutes', NULL),
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE1', '+1', 'up', $5::timestamptz - interval '55 minutes', NULL),
		($1, 1, 'C0EXAMPLE1', '1700000001.000002', 'U0EXAMPLE1', 'thumbsup', 'up', $5::timestamptz - interval '54 minutes', NULL),
		-- Grace thought it not helpful; someone took their thumbs-down back.
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE2', 'button:feedback', 'down', $5::timestamptz - interval '40 minutes', NULL),
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE3', '-1', 'down', $5::timestamptz - interval '45 minutes', $5::timestamptz - interval '30 minutes'),
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE3', 'fire', 'other', $5::timestamptz - interval '45 minutes', NULL),
		($1, 1, 'C0EXAMPLE1', '1700000001.000002', 'U0EXAMPLE2', 'fire', 'other', $5::timestamptz - interval '45 minutes', NULL),
		($1, 1, 'C0EXAMPLE1', '1700000001.000001', 'U0EXAMPLE2', 'fire', 'other', $5::timestamptz - interval '44 minutes', NULL),
		-- Ada changed her mind on the DM answer: the latest counts.
		($2, 1, 'D0EXAMPLE2', '1700000002.000001', 'U0EXAMPLE1', '+1', 'up', $5::timestamptz - interval '47 hours', NULL),
		($2, 1, 'D0EXAMPLE2', '1700000002.000001', 'U0EXAMPLE1', 'button:feedback', 'down', $5::timestamptz - interval '46 hours', NULL),
		-- An answer posted as two messages either side of the 7-day edge.
		($2, 5, 'D0EXAMPLE2', '1700000002.000006', 'U0EXAMPLE2', '-1', 'down', $5::timestamptz - interval '6 days', NULL),
		($3, 1, 'C0EXAMPLE3', '1700000003.000001', 'U0EXAMPLE9', 'button:feedback', 'up', $5::timestamptz - interval '9 days', NULL),
		($4, 1, 'C0EXAMPLE1', '1600000001.000001', 'U0EXAMPLE1', 'button:feedback', 'up', $5::timestamptz - interval '399 days', NULL)`,
		f.platform, f.dm, f.pageme, old, f.now)
	return f
}

func TestAskFeedbackReadsRatings(t *testing.T) {
	f := newFeedbackFixture(t)
	ctx := t.Context()

	week, err := f.server.loadAskFeedback(ctx, url.Values{"range": {"7d"}}, f.now)
	require.NoError(t, err)
	assert.Equal(t, feedbackTally{Helpful: 1, NotHelpful: 2, RatedAnswers: 2, Answers: 3}, week.Tally,
		"one person's thumbs on one answer is one rating, however many times and ways they gave it")
	require.NotNil(t, week.Previous)
	assert.Equal(t, feedbackTally{Helpful: 1, NotHelpful: 1, RatedAnswers: 2, Answers: 2}, *week.Previous,
		"the alert's answer, ten days ago, and the one whose first message is just over seven days old")
	assert.Nil(t, week.Places, "the span's answers were all given in Slack conversations")
	assert.Equal(t, []feedbackReaction{{Emoji: "fire", Count: 2}}, week.Reactions)
	assert.Equal(t, int64(2), week.OtherReactions)
	var charted int64
	for _, bar := range week.Chart.Bars {
		charted += bar.Count
	}
	assert.Equal(t, week.Tally.Ratings(), charted)
	assert.Len(t, week.Chart.Bars, 7)

	require.Len(t, week.Answers, 2, "newest answer first")
	platform := week.Answers[0]
	assert.Equal(t, f.platform.String(), platform.SessionID)
	assert.Equal(t, "Who owns the deploy pipeline?", platform.Title)
	assert.Equal(t, "#platform", platform.Place.Label)
	assert.Equal(t, "The pipeline belongs to the platform team.", platform.Preview)
	assert.Equal(t, "mixed", platform.Word())
	require.Len(t, platform.Ratings, 2)
	assert.Equal(t, answerRating{Who: "Ada Example", Helpful: true, At: platform.Ratings[0].At}, platform.Ratings[0])
	assert.Equal(t, "Grace Example", platform.Ratings[1].Who)
	assert.Equal(t, "/admin/ask/sessions/"+f.platform.String()+"#turn-1", platform.Href())
	dm := week.Answers[1]
	assert.Equal(t, untitledAsk, dm.Title)
	assert.Equal(t, "Slack DM", dm.Place.Label)
	assert.Equal(t, "not helpful", dm.Word())

	down, err := f.server.loadAskFeedback(ctx, url.Values{"range": {"7d"}, "rating": {"down"}}, f.now)
	require.NoError(t, err)
	assert.Len(t, down.Answers, 2)
	up, err := f.server.loadAskFeedback(ctx, url.Values{"range": {"7d"}, "rating": {"up"}}, f.now)
	require.NoError(t, err)
	require.Len(t, up.Answers, 1)
	assert.Equal(t, f.platform.String(), up.Answers[0].SessionID)
	assert.Equal(t, []int64{2, 2, 1}, []int64{up.RatingTabs.Tabs[0].Count, up.RatingTabs.Tabs[1].Count, up.RatingTabs.Tabs[2].Count})
	assert.True(t, up.RatingTabs.Tabs[2].Current)

	// A page link carries on after the answer it names.
	after, err := f.server.loadAskFeedback(ctx, url.Values{"range": {"7d"}, "before": {formatFeedbackCursor(platform)}}, f.now)
	require.NoError(t, err)
	require.Len(t, after.Answers, 1)
	assert.Equal(t, f.dm.String(), after.Answers[0].SessionID)
	broken, err := f.server.loadAskFeedback(ctx, url.Values{"before": {"nonsense"}}, f.now)
	require.NoError(t, err)
	assert.NotEmpty(t, broken.FilterError)

	quarter, err := f.server.loadAskFeedback(ctx, url.Values{}, f.now)
	require.NoError(t, err)
	assert.Equal(t, "13w", quarter.Range.Key)
	assert.Equal(t, int64(5), quarter.Tally.Answers)
	assert.Equal(t, []feedbackPlace{{Label: "Conversations in Slack", Helpful: 1, NotHelpful: 3}, {Label: "PagerDuty alert", Helpful: 1}}, quarter.Places)

	all, err := f.server.loadAskFeedback(ctx, url.Values{"range": {"all"}}, f.now)
	require.NoError(t, err)
	assert.Nil(t, all.Previous)
	assert.Equal(t, int64(3), all.Tally.Helpful)
	assert.Equal(t, int64(5), all.Tally.RatedAnswers)

	var out bytes.Buffer
	week.pageData = pageData{Title: "Feedback", CurrentNav: navAsk, CurrentTab: tabAskFeedback}
	require.NoError(t, askFeedbackPage.ExecuteTemplate(&out, "layout", week))
	html := out.String()
	assert.Contains(t, html, "Ratings come from the thumbs on Slack answers")
	assert.Contains(t, html, "Not enough ratings for a rate: 3 of the 20 it takes")
	assert.Contains(t, html, `<a href="/admin/ask/feedback" aria-current="page">Feedback</a>`)
	assert.Contains(t, html, `href="/admin/ask/sessions/`+f.platform.String()+`#turn-1"`)
	assert.Contains(t, html, "Too few ratings to show a trend")
	for _, internal := range []string{"ask_feedback", "verdict", "reactor", "button:feedback", "U0EXAMPLE1", "U0EXAMPLE2"} {
		assert.NotContains(t, html, internal)
	}
}

func TestAskFeedbackEmptyPage(t *testing.T) {
	var out bytes.Buffer
	data := askFeedbackData{Range: pickFeedbackRange("7d"), Tally: feedbackTally{Answers: 12}}
	data.pageData = pageData{Title: "Feedback", CurrentNav: navAsk, CurrentTab: tabAskFeedback}
	require.NoError(t, askFeedbackPage.ExecuteTemplate(&out, "layout", data))
	html := out.String()
	assert.Contains(t, html, "No ratings in the last 7 days")
	assert.Contains(t, html, "Diderot gave 12 answers in Slack in the last 7 days, and nobody rated one.")
	assert.NotContains(t, html, "Other reactions")
	assert.NotContains(t, html, "Rated answers", "the empty span says it once")
}

func TestAskSessionShowsItsRatings(t *testing.T) {
	f := newFeedbackFixture(t)
	data, err := f.server.loadAskSession(t.Context(), f.platform)
	require.NoError(t, err)
	assert.Equal(t, int64(1), data.Helpful)
	assert.Equal(t, int64(1), data.NotHelpful)
	require.Len(t, data.Exchanges, 1)
	require.Len(t, data.Exchanges[0].Answers, 1)
	answer := data.Exchanges[0].Answers[0]
	require.Len(t, answer.Ratings, 2)
	assert.Equal(t, "Ada Example", answer.Ratings[0].Who)
	assert.True(t, answer.Ratings[0].Helpful)
	assert.Equal(t, "Grace Example", answer.Ratings[1].Who)
	assert.False(t, answer.Ratings[1].Helpful)
	assert.Equal(t, []feedbackReaction{{Emoji: "fire", Count: 2}}, answer.Reactions)

	var out bytes.Buffer
	data.pageData = pageData{Title: data.Title, CurrentNav: navAsk}
	require.NoError(t, askSessionPage.ExecuteTemplate(&out, "layout", data))
	html := out.String()
	assert.Contains(t, html, "Ratings in Slack")
	assert.Contains(t, html, `<span class="badge badge-bad">not helpful</span> <span>Grace Example</span>`)
	assert.Contains(t, html, "<code>:fire:</code> ×2")
}

func TestOverviewFeedbackSignal(t *testing.T) {
	f := newFeedbackFixture(t)
	signal, err := f.server.loadFeedbackSignal(t.Context(), f.now)
	require.NoError(t, err)
	require.NotNil(t, signal)
	assert.Equal(t, feedbackSignal{Helpful: 1, NotHelpful: 2, During: "in the last 7 days",
		Href: "/admin/ask/feedback?range=7d&rating=down#answers-heading"}, *signal)

	// A quiet week falls back to the last four.
	later := f.now.Add(8 * 24 * time.Hour)
	signal, err = f.server.loadFeedbackSignal(t.Context(), later)
	require.NoError(t, err)
	require.NotNil(t, signal)
	assert.Equal(t, "in the last 4 weeks", signal.During)
	assert.Equal(t, int64(2), signal.Helpful)

	signal, err = f.server.loadFeedbackSignal(t.Context(), f.now.Add(60*24*time.Hour))
	require.NoError(t, err)
	assert.Nil(t, signal, "nothing rated recently: the Overview says nothing")

	var out bytes.Buffer
	data := overviewData{Feedback: &feedbackSignal{Helpful: 6, NotHelpful: 1, During: "in the last 7 days", Href: "/admin/ask/feedback?range=7d"}}
	data.pageData = pageData{Title: "Overview", CurrentNav: navOverview}
	require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", data))
	assert.Contains(t, out.String(), "Ratings of the answers given in the last 7 days: <strong>6 helpful</strong>, <strong>1 not helpful</strong>.")
	assert.Contains(t, out.String(), "Read the one rated not helpful")
	out.Reset()
	data.Feedback = nil
	require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", data))
	assert.False(t, strings.Contains(out.String(), "overview-feedback"))
}
