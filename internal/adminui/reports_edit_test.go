package adminui

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestReportEditKeepsATargetItCannotResolve: a report whose channel was
// archived or forgotten, or whose owner left Slack, can still have its
// name, schedule, and instructions changed, as long as the form leaves its
// delivery target as it is. A target the form changes is looked up as on a
// new report.
func TestReportEditKeepsATargetItCannotResolve(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_reports"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, scheduled_reports, slack_channel_info, slack_users RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name, is_archived) VALUES ('CGONE01', 'gone', true)`)
	require.NoError(t, err)

	store := scheduledreport.NewStore(pool)
	on := true
	base := scheduledreport.CreateParams{
		Prompt: "Summarize the week.", Cadence: scheduledreport.CadenceDaily, ScheduleHour: 9, Timezone: "UTC",
		OwnerEmail: "left@example.com", CatchupSeconds: 3600, Enabled: &on, CreatedBy: "operator@example.com",
	}
	channel, dm := base, base
	channel.Name, channel.DeliveryKind, channel.SlackTarget = "To a channel", scheduledreport.DeliverySlackChannel, "CGONE01"
	dm.Name, dm.DeliveryKind, dm.SlackTarget = "To a person", scheduledreport.DeliverySlackDM, "ULEFT001"
	channelReport, err := store.Create(ctx, channel)
	require.NoError(t, err)
	dmReport, err := store.Create(ctx, dm)
	require.NoError(t, err)

	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		ScheduledReportsStore: store,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	edit := reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+channelReport.ID.String()+"/edit", nil, "")
	require.Equal(t, http.StatusOK, edit.Code)
	assert.Contains(t, edit.Body.String(), `name="channel" value="#gone"`)
	csrfToken := ""
	for _, cookie := range edit.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.True(t, validCSRFToken(csrfToken))
	save := func(report *scheduledreport.Report, changes url.Values) (int, string) {
		t.Helper()
		form := reportTestForm(csrfToken, url.Values{"owner": {"left@example.com"}, "timezone": {"UTC"}})
		for key, values := range changes {
			form[key] = values
		}
		response := reportBrowserRequest(t, mux, http.MethodPost, "/admin/reports/"+report.ID.String(), form, csrfToken)
		return response.Code, html.UnescapeString(response.Body.String())
	}

	// The archived channel, as the edit form shows it.
	code, body := save(channelReport, url.Values{"name": {"Renamed"}, "channel": {"#gone"}})
	require.Equal(t, http.StatusSeeOther, code, body)
	saved, err := store.Get(ctx, channelReport.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", saved.Name)
	assert.Equal(t, "CGONE01", saved.SlackTarget)

	// A channel Diderot no longer knows is shown, and kept, by its ID.
	_, err = pool.Exec(ctx, `DELETE FROM slack_channel_info WHERE channel_id = 'CGONE01'`)
	require.NoError(t, err)
	edit = reportBrowserRequest(t, mux, http.MethodGet, "/admin/reports/"+channelReport.ID.String()+"/edit", nil, csrfToken)
	assert.Contains(t, edit.Body.String(), `name="channel" value="CGONE01"`)
	code, body = save(channelReport, url.Values{"name": {"Renamed again"}, "channel": {"CGONE01"}})
	require.Equal(t, http.StatusSeeOther, code, body)

	// A changed target is looked up.
	code, body = save(channelReport, url.Values{"name": {"Renamed again"}, "channel": {"#elsewhere"}})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "Diderot does not know a channel named #elsewhere.")
	code, body = save(channelReport, url.Values{"name": {"Renamed again"}, "delivery": {"dm"}})
	require.Equal(t, http.StatusUnprocessableEntity, code, "the same owner, but a direct message is a new target")
	assert.Contains(t, body, "No one in Slack has this email")

	// A direct message to someone Slack no longer lists.
	code, body = save(dmReport, url.Values{"name": {"To them, later"}, "delivery": {"dm"}, "time": {"10:30"}})
	require.Equal(t, http.StatusSeeOther, code, body)
	saved, err = store.Get(ctx, dmReport.ID)
	require.NoError(t, err)
	assert.Equal(t, "ULEFT001", saved.SlackTarget)
	assert.Equal(t, 10, saved.ScheduleHour)
	code, body = save(dmReport, url.Values{"name": {"To them, later"}, "delivery": {"dm"}, "owner": {"nobody@example.com"}})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "No one in Slack has this email")
}

func TestReportNextNote(t *testing.T) {
	soon, past := time.Now().Add(time.Hour), time.Now().Add(-3*time.Hour)
	assert.Empty(t, reportNextNote(soon, true, false))
	assert.Equal(t, "Waiting for Slack", reportNextNote(past, false, false))
	assert.Equal(t, "Waiting for Slack", reportNextNote(soon, false, true), "runs wait, whatever the schedule says")
	assert.Equal(t, "Waiting for setup", reportNextNote(past, true, true))
	assert.Equal(t, "Due now", reportNextNote(past, true, false), "a time already past is never shown as the next run")

	assert.True(t, sameReportChannel(" #Gone ", "#gone", "C1"))
	assert.True(t, sameReportChannel("C1", "#gone", "C1"))
	assert.False(t, sameReportChannel("#other", "#gone", "C1"))
	assert.False(t, sameReportChannel("", "", "C1"), "an empty channel is not the saved one")
}
