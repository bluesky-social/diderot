package adminui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

var overviewPage = mustPage("overview.html")

type overviewData struct {
	pageData
	// Attention is what needs an administrator; empty when nothing does.
	Attention []attentionItem
	Window    overviewWindow
	// WindowTabs switch between the windows.
	WindowTabs tabSet
	Ask        askActivity
	Chart      barChart
	// Feedback is how people rated recent answers, nil when nobody has.
	Feedback *feedbackSignal
	Recent   []recentConversation
	// Failures are the conversations begun in the window whose latest
	// question failed or got an incomplete answer: what the Ask list's
	// State filter finds, at FailedURL and IncompleteURL.
	Failures      []recentConversation
	FailedURL     string
	IncompleteURL string
	Sources       []overviewSource
	// CountedAt is when the sources' items were counted.
	CountedAt time.Time
}

// overviewWindow is a span of Ask activity the page can show.
type overviewWindow struct {
	Key   string
	Label string
	Span  time.Duration
	// Bucket is the chart's bar width.
	Bucket time.Duration
	// BucketName says one bucket in "Questions per hour".
	BucketName string
	// Start says where the chart begins: "24 hours ago".
	Start string
	// During ends a sentence about it: "in the last 24 hours".
	During string
}

var overviewWindows = []overviewWindow{
	{Key: "24h", Label: "Last 24 hours", Span: 24 * time.Hour, Bucket: time.Hour, BucketName: "hour", Start: "24 hours ago", During: "in the last 24 hours"},
	{Key: "7d", Label: "Last 7 days", Span: 7 * 24 * time.Hour, Bucket: 6 * time.Hour, BucketName: "6 hours", Start: "7 days ago", During: "in the last 7 days"},
	{Key: "30d", Label: "Last 30 days", Span: 30 * 24 * time.Hour, Bucket: 24 * time.Hour, BucketName: "day", Start: "30 days ago", During: "in the last 30 days"},
}

// askActivity is what Ask did in a window, from its record of each question.
type askActivity struct {
	Questions     int64
	Conversations int64
	Failed        int64
	Incomplete    int64
	// MedianAnswer and SlowAnswer are the 50th and 95th percentile time to
	// answer, nil without an answer.
	MedianAnswer *time.Duration
	SlowAnswer   *time.Duration
}

// PerConversation is the average number of questions in a conversation.
func (a askActivity) PerConversation() string {
	if a.Conversations == 0 {
		return "0"
	}
	return fmt.Sprintf("%.1f", float64(a.Questions)/float64(a.Conversations))
}

// FailedShare is the failed questions' share of all of them, in percent.
func (a askActivity) FailedShare() int {
	if a.Questions == 0 {
		return 0
	}
	return int((100*a.Failed + a.Questions/2) / a.Questions)
}

// recentConversation is a conversation in one of the Overview's two short
// lists, with the state the Ask pages give it (askStateSQL). Reason is set
// in the list of those that failed.
type recentConversation struct {
	ID      string
	Title   string
	Owner   string
	Surface string
	State   string
	Tone    string
	Reason  string
	At      time.Time
}

// overviewListSize is how many conversations each short list shows.
const overviewListSize = 6

// overviewSource is one source's row: registered, or only in the corpus.
type overviewSource struct {
	Title  string
	Href   string
	Tone   string
	State  string
	Detail string
	Items  int64
	Newest *time.Time
}

// handleOverview serves the Overview once the install is live; until then
// the setup checklist is the first page.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if s.setup != nil && !s.setup.Org().Live() {
		http.Redirect(w, r, settingsPath, http.StatusSeeOther)
		return
	}
	window := pickOverviewWindow(r.URL.Query().Get("range"))
	data, err := s.loadOverview(r.Context(), window, time.Now())
	if err != nil {
		s.logger.Error("admin overview query failed", "err", err)
		http.Error(w, "overview query failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, "Overview", navOverview)
	data.Stylesheet = "overview.css"
	s.render(w, overviewPage, data)
}

// pickOverviewWindow is the window key names, or the last 24 hours.
func pickOverviewWindow(key string) overviewWindow {
	for _, window := range overviewWindows {
		if window.Key == key {
			return window
		}
	}
	return overviewWindows[0]
}

func (s *Server) loadOverview(ctx context.Context, window overviewWindow, now time.Time) (overviewData, error) {
	data := overviewData{Window: window, WindowTabs: tabSet{Label: "Time window"}}
	for _, w := range overviewWindows {
		href := "?range=" + w.Key
		if w.Key == overviewWindows[0].Key {
			href = "/admin/"
		}
		data.WindowTabs.Tabs = append(data.WindowTabs.Tabs, navTab{Key: w.Key, Label: w.Label, Href: href, Current: w.Key == window.Key})
	}
	since := now.Add(-window.Span)
	var err error
	if data.Ask, err = s.loadAskActivity(ctx, since, now); err != nil {
		return data, err
	}
	if data.Chart, err = s.loadActivityChart(ctx, since, now, window.Bucket); err != nil {
		return data, err
	}
	data.Chart.Caption = "Questions per " + window.BucketName
	data.Chart.From, data.Chart.To = window.Start, "Now"
	data.Chart.Summary = fmt.Sprintf("Questions per %s, %s: %s in all, at most %d in one %s.",
		window.BucketName, strings.ToLower(window.Label), formatCount(data.Ask.Questions), data.Chart.Peak(), window.BucketName)
	if data.Feedback, err = s.loadFeedbackSignal(ctx, now); err != nil {
		return data, err
	}
	if data.Recent, err = s.loadRecentConversations(ctx, nil); err != nil {
		return data, err
	}
	if data.Failures, err = s.loadRecentConversations(ctx, &since); err != nil {
		return data, err
	}
	data.FailedURL = askListPath + "?status=" + askFailed + "&range=" + window.Key
	data.IncompleteURL = askListPath + "?status=" + askIncomplete + "&range=" + window.Key
	health, err := s.loadHealth(ctx)
	if err != nil {
		return data, err
	}
	data.Attention = health.Attention
	// Failed questions need a look when they are more than one in twenty of
	// the last day's.
	day := data.Ask
	if window.Span != 24*time.Hour {
		if day, err = s.loadAskActivity(ctx, now.Add(-24*time.Hour), now); err != nil {
			return data, err
		}
	}
	if day.Failed > 0 && day.Failed*20 >= day.Questions {
		data.Attention = append([]attentionItem{{
			Text:     fmt.Sprintf("%d of %s in the last 24 hours failed.", day.Failed, countOf(day.Questions, "question", "questions")),
			Href:     "#failures-heading",
			LinkText: "Recent failures",
		}}, data.Attention...)
	}
	var on []string
	for _, src := range health.Sources {
		if src.Enabled {
			on = append(on, src.EventSource)
		}
	}
	corpus, err := s.corpusFor(ctx, on)
	if err != nil {
		return data, err
	}
	data.CountedAt = corpus.At
	data.Sources = overviewSources(health.Sources, corpus)
	return data, nil
}

// overviewSources lists every source that is on or has items, then any
// items no registered source wrote.
func overviewSources(sources []sourceHealth, corpus corpusTotals) []overviewSource {
	var rows []overviewSource
	listed := map[string]bool{}
	for _, src := range sources {
		listed[src.EventSource] = true
		counted := corpus.Sources[src.EventSource]
		if !src.Enabled && counted.Items == 0 {
			continue
		}
		rows = append(rows, overviewSource{
			Title: src.Title, Href: src.Href, Tone: src.Tone, State: src.State, Detail: src.Detail,
			Items: counted.Items, Newest: counted.Newest,
		})
	}
	var others []string
	for eventSource, counted := range corpus.Sources {
		if !listed[eventSource] && counted.Items > 0 {
			others = append(others, eventSource)
		}
	}
	slices.Sort(others)
	for _, eventSource := range others {
		counted := corpus.Sources[eventSource]
		rows = append(rows, overviewSource{
			Title: eventSource, Tone: toneNeutral, State: "off",
			Detail: "No connector writes these any more.", Items: counted.Items, Newest: counted.Newest,
		})
	}
	return rows
}

func (s *Server) loadAskActivity(ctx context.Context, since, until time.Time) (askActivity, error) {
	var activity askActivity
	var median, slow *float64
	var answered int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(DISTINCT session_id),
		       count(*) FILTER (WHERE outcome = 'error'),
		       count(*) FILTER (WHERE outcome = 'incomplete'),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE outcome = 'answered'),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE outcome = 'answered'),
		       count(*) FILTER (WHERE outcome = 'answered')
		  FROM ask_runs
		 WHERE started_at >= $1 AND started_at < $2`, since, until).Scan(
		&activity.Questions, &activity.Conversations, &activity.Failed, &activity.Incomplete, &median, &slow, &answered)
	if err != nil {
		return activity, fmt.Errorf("ask activity: %w", err)
	}
	activity.MedianAnswer = milliseconds(median)
	// With few answers the slowest 5% is the slowest one or two, which says
	// nothing the median does not.
	if answered >= slowAnswerMinimum {
		activity.SlowAnswer = milliseconds(slow)
	}
	return activity, nil
}

// slowAnswerMinimum is how many answers a window needs before its 95th
// percentile is shown.
const slowAnswerMinimum = 20

func milliseconds(value *float64) *time.Duration {
	if value == nil {
		return nil
	}
	// Under ten seconds the page shows tenths, so keep them.
	d := time.Duration(*value * float64(time.Millisecond))
	if d >= 10*time.Second {
		d = d.Round(time.Second)
	} else {
		d = d.Round(100 * time.Millisecond)
	}
	return &d
}

func (s *Server) loadActivityChart(ctx context.Context, since, until time.Time, bucket time.Duration) (barChart, error) {
	var chart barChart
	rows, err := s.pool.Query(ctx, `
		WITH bins AS (
			SELECT generate_series(
			         date_bin(make_interval(secs => $3), $1::timestamptz, '2000-01-01'),
			         date_bin(make_interval(secs => $3), $2::timestamptz, '2000-01-01'),
			         make_interval(secs => $3)) AS bucket
		), counted AS (
			SELECT date_bin(make_interval(secs => $3), started_at, '2000-01-01') AS bucket, count(*) AS n
			  FROM ask_runs
			 WHERE started_at >= $1 AND started_at < $2
			 GROUP BY 1
		)
		SELECT b.bucket, COALESCE(c.n, 0)
		  FROM bins b LEFT JOIN counted c USING (bucket)
		 ORDER BY b.bucket`, since, until, bucket.Seconds())
	if err != nil {
		return chart, fmt.Errorf("ask activity chart: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bar chartBar
		if err := rows.Scan(&bar.Start, &bar.Count); err != nil {
			return chart, fmt.Errorf("ask activity chart scan: %w", err)
		}
		chart.Bars = append(chart.Bars, bar)
	}
	if err := rows.Err(); err != nil {
		return chart, fmt.Errorf("ask activity chart rows: %w", err)
	}
	return chart, nil
}

// loadRecentConversations lists the latest conversations by their last
// question. Given since, it lists instead the conversations begun from then
// on whose latest question failed or got an incomplete answer, newest
// first, each with the reason the Ask pages give.
func (s *Server) loadRecentConversations(ctx context.Context, since *time.Time) ([]recentConversation, error) {
	where, order := "true", "s.last_turn_at DESC, s.id DESC"
	if since != nil {
		where = "s.created_at >= $2 AND st.state IN ('" + askFailed + "', '" + askIncomplete + "')"
		order = "s.created_at DESC, s.id DESC"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, COALESCE(s.title, ''), COALESCE(s.owner_email, ''), `+askSurfaceSQL+`,
		       CASE WHEN $2::timestamptz IS NULL THEN s.last_turn_at ELSE s.created_at END,
		       st.state, st.run_error, st.incomplete_reason
		  FROM ask_sessions s
		  LEFT JOIN ask_slack_sessions sl ON sl.session_id = s.id
		 CROSS JOIN LATERAL (`+askStateSQL+`) st
		 WHERE `+where+`
		 ORDER BY `+order+`
		 LIMIT $1`, overviewListSize, since)
	if err != nil {
		return nil, fmt.Errorf("recent conversations: %w", err)
	}
	defer rows.Close()
	var recent []recentConversation
	for rows.Next() {
		var row recentConversation
		var surface, runError, incomplete string
		if err := rows.Scan(&row.ID, &row.Title, &row.Owner, &surface, &row.At, &row.State, &runError, &incomplete); err != nil {
			return nil, fmt.Errorf("recent conversations scan: %w", err)
		}
		row.Surface = askSurfaceLabel(surface)
		row.Tone = askStateTone(row.State)
		row.Reason = compactText(askStateReason(row.State, runError, incomplete), 160)
		recent = append(recent, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent conversations rows: %w", err)
	}
	return recent, nil
}
