package adminui

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestReportsBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_reports"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, scheduled_reports, slack_channel_info, slack_users RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_archived) VALUES
		  ('CPLATFORM', 'platform', false), ('COLDTEAM', 'old-team', true)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_users (user_id, name, real_name, email, tz) VALUES
		  ('UOPERATOR', 'operator', 'Ana Lopez', 'Operator@example.com', 'America/New_York'),
		  ('USHARED1', 'one', 'One', 'shared@example.com', NULL),
		  ('USHARED2', 'two', 'Two', 'shared@example.com', NULL)`)
	require.NoError(t, err)

	store := scheduledreport.NewStore(pool)
	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		ScheduledReportsStore: store,
		SlackWorkspaceHost:    func() string { return "example.slack.com" },
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	list := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports", nil, "")
	require.Equal(t, http.StatusOK, list.Code)
	assert.Contains(t, list.Body.String(), "No reports yet")
	assert.Contains(t, list.Body.String(), `href="/admin/reports/new"`)
	csrfToken := ""
	for _, cookie := range list.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	if csrfToken == "" {
		form := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/new", nil, "")
		for _, cookie := range form.Result().Cookies() {
			if cookie.Name == csrfCookieName {
				csrfToken = cookie.Value
			}
		}
	}
	require.True(t, validCSRFToken(csrfToken))

	// A new report starts in the operator's Slack time zone, owned by them.
	blank := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/new", nil, csrfToken)
	require.Equal(t, http.StatusOK, blank.Code)
	assert.Contains(t, blank.Body.String(), `value="America/New_York"`)
	assert.Contains(t, blank.Body.String(), `value="operator@example.com"`)
	assert.Contains(t, blank.Body.String(), "Daily standup")
	example := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/new?template=team-weekly", nil, csrfToken)
	require.Equal(t, http.StatusOK, example.Code)
	weekly, err := scheduledreport.Template("team-weekly")
	require.NoError(t, err)
	assert.Contains(t, html.UnescapeString(example.Body.String()), ">"+weekly+"</textarea>")
	assert.Contains(t, example.Body.String(), `value="Weekly team update"`)

	// What cannot be resolved is refused next to its field, keeping what
	// was typed.
	refused := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, url.Values{
		"channel": {"#nope"}, "late_hours": {"0"}, "timezone": {"Mars/Base"},
	}), csrfToken)
	require.Equal(t, http.StatusUnprocessableEntity, refused.Code)
	body := html.UnescapeString(refused.Body.String())
	assert.Contains(t, body, "Diderot does not know a channel named #nope.")
	assert.Contains(t, body, "Enter a number of hours above zero, at most 720.")
	assert.Contains(t, body, "Enter a time zone such as America/New_York.")
	assert.Contains(t, body, `value="Platform briefing"`)
	assert.Contains(t, body, `value="#nope"`)
	archived := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, url.Values{"channel": {"old-team"}}), csrfToken)
	require.Equal(t, http.StatusUnprocessableEntity, archived.Code)
	assert.Contains(t, archived.Body.String(), "#old-team is archived.")
	for owner, problem := range map[string]string{
		"nobody@example.com": "No one in Slack has this email",
		"shared@example.com": "More than one Slack account has this email",
		"UNOBODY1":           "Diderot does not know the Slack member UNOBODY1.",
	} {
		dm := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, url.Values{
			"delivery": {"dm"}, "owner": {owner},
		}), csrfToken)
		require.Equal(t, http.StatusUnprocessableEntity, dm.Code, owner)
		assert.Contains(t, html.UnescapeString(dm.Body.String()), problem, owner)
	}
	reports, err := store.List(ctx)
	require.NoError(t, err)
	require.Empty(t, reports)

	// A channel by name, and a direct message to an owner given by Slack ID.
	created := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, nil), csrfToken)
	require.Equal(t, http.StatusSeeOther, created.Code)
	assert.Contains(t, created.Header().Get("Location"), "/admin/reports/")
	dm := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, url.Values{
		"name": {"Standup"}, "delivery": {"dm"}, "owner": {"UOPERATOR"}, "channel": {""}, "enabled": nil,
	}), csrfToken)
	require.Equal(t, http.StatusSeeOther, dm.Code, dm.Body.String())
	duplicate := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, nil), csrfToken)
	require.Equal(t, http.StatusUnprocessableEntity, duplicate.Code)
	assert.Contains(t, duplicate.Body.String(), "Another report already has this name.")
	reports, err = store.List(ctx)
	require.NoError(t, err)
	require.Len(t, reports, 2)
	channelReport, dmReport := reports[0], reports[1]
	require.Equal(t, "Platform briefing", channelReport.Name)
	assert.Equal(t, scheduledreport.DeliverySlackChannel, channelReport.DeliveryKind)
	assert.Equal(t, "CPLATFORM", channelReport.SlackTarget)
	assert.Equal(t, 90*time.Minute, channelReport.CatchupWindow)
	assert.Equal(t, "operator@example.com", channelReport.CreatedBy)
	assert.True(t, channelReport.Enabled)
	assert.Equal(t, scheduledreport.DeliverySlackDM, dmReport.DeliveryKind)
	assert.Equal(t, "UOPERATOR", dmReport.SlackTarget)
	assert.Equal(t, "operator@example.com", dmReport.OwnerEmail)
	assert.False(t, dmReport.Enabled)
	id := channelReport.ID.String()

	list = reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports", nil, csrfToken)
	require.Equal(t, http.StatusOK, list.Code)
	assert.Contains(t, list.Body.String(), "#platform")
	assert.Contains(t, list.Body.String(), "Ana Lopez")
	assert.Contains(t, list.Body.String(), "Every day at 9:00 AM Pacific time")
	assert.Contains(t, list.Body.String(), "Not run yet")

	detail := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+id, nil, csrfToken)
	require.Equal(t, http.StatusOK, detail.Code)
	assert.Contains(t, detail.Body.String(), "<span>Every day at 9:00 AM Pacific time</span><span>Posted in #platform</span>")
	assert.Contains(t, detail.Body.String(), "Run now")
	assert.Contains(t, detail.Body.String(), "No runs yet")

	edit := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+id+"/edit", nil, csrfToken)
	require.Equal(t, http.StatusOK, edit.Code)
	assert.Contains(t, edit.Body.String(), `value="#platform"`)
	assert.Contains(t, edit.Body.String(), `value="1.5"`)

	update := reportTestForm(csrfToken, url.Values{
		"name": {"Platform weekly"}, "cadence": {"weekly"}, "weekday": {"5"}, "time": {"10:15 am"},
		"channel": {"CPLATFORM"}, "prompt": {"Summarize changes safely."},
	})
	updated := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id, update, csrfToken)
	require.Equal(t, http.StatusSeeOther, updated.Code, updated.Body.String())
	got, err := store.Get(ctx, channelReport.ID)
	require.NoError(t, err)
	assert.Equal(t, "Platform weekly", got.Name)
	assert.Equal(t, scheduledreport.CadenceWeekly, got.Cadence)
	require.NotNil(t, got.Weekday)
	assert.Equal(t, 5, *got.Weekday)
	assert.Equal(t, 10, got.ScheduleHour)
	assert.Equal(t, 15, got.ScheduleMinute)
	assert.True(t, got.Enabled)
	invalid := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id, reportTestForm(csrfToken, url.Values{"name": {""}}), csrfToken)
	require.Equal(t, http.StatusUnprocessableEntity, invalid.Code)
	assert.Contains(t, invalid.Body.String(), "Enter a name.")

	disabled := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id+"/enabled",
		url.Values{"csrf_token": {csrfToken}, "enabled": {"false"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, disabled.Code)
	got, err = store.Get(ctx, channelReport.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled)

	// Run now needs no confirmation, and the run shows as queued.
	queued := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id+"/run", url.Values{"csrf_token": {csrfToken}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, queued.Code)
	assert.Contains(t, queued.Header().Get("Location"), "notice=Run+queued")
	runs, err := store.ListRuns(ctx, channelReport.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "manual", runs[0].Trigger)
	detail = reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+id, nil, csrfToken)
	assert.Contains(t, detail.Body.String(), `<span class="badge badge-attention">queued</span>`)
	assert.Contains(t, detail.Body.String(), `data-refresh="15"`)

	// A delivered run links to its Slack message; a failed one says why.
	_, err = pool.Exec(ctx, `
		UPDATE scheduled_report_runs
		   SET status='succeeded', attempts=1, finished_at=created_at + interval '94 seconds',
		       delivery_kind='slack_channel', slack_target='CPLATFORM', delivery_ts='1763312494.000100'
		 WHERE id=$1`, runs[0].ID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO scheduled_report_runs
		       (report_id, trigger, scheduled_for, window_start, window_end, prompt_snapshot, status, attempts, finished_at, error)
		VALUES ($1, 'scheduled', now() - interval '1 day', now() - interval '2 days', now() - interval '1 day', 'p', 'failed', 3, now() - interval '1 day',
		        'scheduled report: deliver: post scheduled report chunk 1/1: not_in_channel')`, channelReport.ID)
	require.NoError(t, err)
	detail = reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+id, nil, csrfToken)
	body = html.UnescapeString(detail.Body.String())
	assert.Contains(t, body, `<span class="badge badge-good">delivered</span>`)
	assert.Contains(t, body, `href="https://example.slack.com/archives/CPLATFORM/p1763312494000100"`)
	assert.Contains(t, body, "1m 34s")
	assert.Contains(t, body, "Diderot is not in the channel. Add it to the channel in Slack.")
	assert.Contains(t, body, "3 tries")
	assert.NotContains(t, body, "data-refresh")
	list = reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports", nil, csrfToken)
	assert.Contains(t, list.Body.String(), `<span class="badge badge-good">delivered</span>`)

	rejected := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id+"/delete", url.Values{"csrf_token": {csrfToken}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, rejected.Code, "a delete that does not confirm goes back to the page, with why")
	assert.Contains(t, rejected.Header().Get("Location"), "/admin/reports/"+id+"/edit?error=The+report+was+not+deleted")
	deleted := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id+"/delete",
		url.Values{"csrf_token": {csrfToken}, "confirm": {"delete"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, deleted.Code)
	assert.Contains(t, deleted.Header().Get("Location"), "Platform+weekly")
	_, err = store.Get(ctx, channelReport.ID)
	require.Error(t, err)
	gone := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+id, nil, csrfToken)
	require.Equal(t, http.StatusNotFound, gone.Code)
	assert.Contains(t, gone.Body.String(), "<h1>Report not found</h1>")
	assert.Contains(t, gone.Body.String(), `<a class="button" href="/admin/reports">All reports</a>`)
	stale := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+id,
		url.Values{"csrf_token": {csrfToken}, "name": {"Renamed"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, stale.Code, "a form for a deleted report goes back to the list")
	assert.Contains(t, stale.Header().Get("Location"), "error=That+report+no+longer+exists.")

	// Two creates, an update, a disable, a run, and a delete; the store
	// refused the duplicate name, and forms refused before it write nothing.
	var succeeded, failed, other int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status='succeeded'), count(*) FILTER (WHERE status='failed'),
		       count(*) FILTER (WHERE status NOT IN ('succeeded', 'failed'))
		  FROM admin_audit_log`).Scan(&succeeded, &failed, &other))
	assert.Equal(t, 6, succeeded)
	assert.Equal(t, 1, failed)
	assert.Zero(t, other)
}

func TestReportsNeedSlack(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_reports"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE scheduled_reports RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		ScheduledReportsStore: scheduledreport.NewStore(pool),
		Connectors:            slackOffConnectors{},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	for _, path := range []string{"/admin/reports", "/admin/reports/new"} {
		page := reportBrowserRequest(t, mux, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusOK, page.Code, path)
		assert.Contains(t, page.Body.String(), "Reports need Slack", path)
		assert.Contains(t, page.Body.String(), `href="/admin/connectors/slack"`, path)
		assert.NotContains(t, page.Body.String(), `<form class="form"`, path)
		assert.NotContains(t, page.Body.String(), `href="/admin/reports/new"`, path)
	}

	// The page offers no form, and one posted anyway creates nothing.
	csrfToken, err := issueCSRFToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://diderot.test/admin/reports", nil))
	require.NoError(t, err)
	refused := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports", reportTestForm(csrfToken, nil), csrfToken)
	require.Equal(t, http.StatusSeeOther, refused.Code, refused.Body.String())
	message, isError := redirectMessage(t, refused.Header().Get("Location"))
	assert.True(t, isError)
	assert.Equal(t, "The report was not created: reports are delivered in Slack, and the Slack connector is off. Turn it on first.", message)
	var reports int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_reports`).Scan(&reports))
	assert.Zero(t, reports)

	// A report made while Slack was on waits: its next run is not shown as
	// a time, least of all one already past.
	on := true
	waiting, err := scheduledreport.NewStore(pool).Create(ctx, scheduledreport.CreateParams{
		Name: "Waiting", Prompt: "Summarize.", Cadence: scheduledreport.CadenceDaily, ScheduleHour: 9, Timezone: "UTC",
		DeliveryKind: scheduledreport.DeliverySlackChannel, SlackTarget: "CPLATFORM", OwnerEmail: "op@example.com",
		CatchupSeconds: 3600, Enabled: &on, CreatedBy: "op@example.com",
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE scheduled_reports SET next_run_at = now() - interval '3 hours' WHERE id = $1`, waiting.ID)
	require.NoError(t, err)
	for _, path := range []string{"/admin/reports", "/admin/reports/" + waiting.ID.String()} {
		body := reportBrowserRequest(t, mux, http.MethodGet, path, nil, "").Body.String()
		assert.Contains(t, body, "Waiting for Slack", path)
		assert.NotContains(t, body, "3 hours ago", path)
	}
}

func TestScheduleWords(t *testing.T) {
	friday := 5
	assert.Equal(t, "Weekdays at 9:30 AM Pacific time", scheduleWords("weekdays", nil, 9, 30, "America/Los_Angeles"))
	assert.Equal(t, "Every day at 12:00 AM UTC", scheduleWords("daily", nil, 0, 0, "UTC"))
	assert.Equal(t, "Every Friday at 4:05 PM Lisbon time", scheduleWords("weekly", &friday, 16, 5, "Europe/Lisbon"))
	assert.Equal(t, "Every day at 8:00 AM Buenos Aires time", scheduleWords("daily", nil, 8, 0, "America/Argentina/Buenos_Aires"))
	assert.Equal(t, "6 hours", lateWords(6*time.Hour))
	assert.Equal(t, "90 minutes", lateWords(90*time.Minute))
}

func TestReportFormReadsTimesAndZones(t *testing.T) {
	for raw, want := range map[string][2]int{"09:30": {9, 30}, "9:30": {9, 30}, "9:30 pm": {21, 30}, "12 AM": {0, 0}, "17:45:00": {17, 45}} {
		hour, minute, ok := parseReportTime(raw)
		require.True(t, ok, raw)
		assert.Equal(t, want, [2]int{hour, minute}, raw)
	}
	for _, raw := range []string{"", "25:00", "noon", "9.30"} {
		_, _, ok := parseReportTime(raw)
		assert.False(t, ok, raw)
	}
	for raw, want := range map[string]string{
		"America/Chicago": "America/Chicago", "Pacific": "America/Los_Angeles", "eastern time": "America/New_York",
		"europe/berlin": "Europe/Berlin", "UTC": "UTC",
	} {
		zone, ok := resolveReportZone(raw)
		require.True(t, ok, raw)
		assert.Equal(t, want, zone, raw)
	}
	for _, raw := range []string{"", "Local", "Mars/Base"} {
		_, ok := resolveReportZone(raw)
		assert.False(t, ok, raw)
	}
	// A saved limit reads back as the same seconds.
	for _, seconds := range []int{60, 72, 90 * 60, 6 * 3600, 3601, 30 * 24 * 3600} {
		text := formatLateHours(time.Duration(seconds) * time.Second)
		parsed, ok := parseLateHours(text)
		require.True(t, ok, text)
		assert.Equal(t, seconds, parsed, text)
	}
	assert.Equal(t, "6", formatLateHours(6*time.Hour))
	assert.Equal(t, "1.5", formatLateHours(90*time.Minute))
	for _, raw := range []string{"", "0", "-1", "0.01", "721", "NaN", "Inf", "six"} {
		_, ok := parseLateHours(raw)
		assert.False(t, ok, raw)
	}
}

func TestReportRunState(t *testing.T) {
	text := func(s string) *string { return &s }
	cases := []struct {
		run                    scheduledreport.Run
		label, tone, reason    string
		keepsRecord, hasReason bool
	}{
		{run: scheduledreport.Run{Status: "pending"}, label: "queued", tone: toneAttention},
		{run: scheduledreport.Run{Status: "pending", Error: text("post: not_in_channel")}, label: "retrying", tone: toneAttention,
			reason: "Diderot is not in the channel. Add it to the channel in Slack.", keepsRecord: true},
		{run: scheduledreport.Run{Status: "running"}, label: "running", tone: toneAttention},
		{run: scheduledreport.Run{Status: "generated"}, label: "sending", tone: toneAttention},
		{run: scheduledreport.Run{Status: "succeeded"}, label: "delivered", tone: toneGood},
		{run: scheduledreport.Run{Status: "skipped", Error: text("outside catch-up window")}, label: "skipped", tone: toneNeutral,
			reason: "Diderot was down for longer than the report may be sent late, so this run was skipped."},
		{run: scheduledreport.Run{Status: "failed", Error: text("model gateway: 503 upstream unavailable\nbody")}, label: "failed", tone: toneBad,
			reason: "model gateway: 503 upstream unavailable"},
	}
	for _, c := range cases {
		label, tone, reason, recorded := reportRunState(c.run)
		assert.Equal(t, c.label, label, c.run.Status)
		assert.Equal(t, c.tone, tone, c.run.Status)
		assert.Equal(t, c.reason, reason, c.run.Status)
		assert.Equal(t, c.keepsRecord, recorded != "", c.run.Status)
	}
}

func TestReportRunViewLinksChannelMessagesOnly(t *testing.T) {
	ts, kind, channel := "1763312494.000100", scheduledreport.DeliverySlackChannel, "CPLATFORM"
	session := uuid.New()
	run := scheduledreport.Run{Status: "succeeded", DeliveryTS: &ts, DeliveryKind: &kind, SlackTarget: &channel, AskSessionID: &session}
	view := newReportRunView(run, scheduledreport.Report{}, "example.slack.com")
	assert.Equal(t, "https://example.slack.com/archives/CPLATFORM/p1763312494000100", view.SlackURL)
	assert.Equal(t, "/admin/ask/sessions/"+session.String(), view.SessionURL)

	dm, user := scheduledreport.DeliverySlackDM, "UOWNER"
	run.DeliveryKind, run.SlackTarget = &dm, &user
	assert.Empty(t, newReportRunView(run, scheduledreport.Report{}, "example.slack.com").SlackURL)
	run.DeliveryKind, run.SlackTarget = &kind, &channel
	assert.Empty(t, newReportRunView(run, scheduledreport.Report{}, "").SlackURL)
}

// slackOffConnectors is connector settings with the Slack connector off.
type slackOffConnectors struct{ ConnectorSettings }

func (slackOffConnectors) View(context.Context, string) (settings.View, error) {
	return settings.View{Name: "slack"}, nil
}

// reportTestForm is a valid report form, with changes; a nil value removes
// a field.
func reportTestForm(csrfToken string, changes url.Values) url.Values {
	form := url.Values{
		"csrf_token": {csrfToken}, "name": {"Platform briefing"},
		"prompt": {"Summarize the important platform work."}, "cadence": {"daily"},
		"weekday": {"1"}, "time": {"09:00"}, "timezone": {"America/Los_Angeles"},
		"delivery": {"channel"}, "channel": {"#platform"},
		"owner": {"platform@example.com"}, "late_hours": {"1.5"}, "enabled": {"true"},
	}
	for key, values := range changes {
		if values == nil {
			form.Del(key)
		} else {
			form[key] = values
		}
	}
	return form
}

func reportBrowserRequest(t *testing.T, mux *http.ServeMux, method, path string, form url.Values, csrfCookie string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "https://diderot.test"+path, body)
	r.Header.Set(proxyEmailHeader, "operator@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
	if csrfCookie != "" {
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://diderot.test")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
