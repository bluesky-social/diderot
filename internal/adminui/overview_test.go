package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestOverviewFormatting(t *testing.T) {
	assert.Equal(t, "999", formatCount(999))
	assert.Equal(t, "1.2K", formatCount(1_234))
	assert.Equal(t, "1.2 MiB", formatBytes(1_258_291))
	assert.Equal(t, "1 item", countOf(1, "item", "items"))
	assert.Equal(t, "12.4K items", countOf(12_400, "item", "items"))
}

func TestFormatDuration(t *testing.T) {
	for want, d := range map[string]time.Duration{
		"0s": -3 * time.Second, "83ms": 83 * time.Millisecond, "31s": 31 * time.Second,
		"8m 20s": 8*time.Minute + 20*time.Second, "5m": 5 * time.Minute, "1h 30m": 90 * time.Minute,
		"2h 5m": 2*time.Hour + 5*time.Minute, "3d 4h": 76 * time.Hour, "110d": 110 * 24 * time.Hour,
	} {
		assert.Equal(t, want, formatDuration(d))
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		at       time.Time
		want     string
		relative bool
	}{
		{now.Add(-20 * time.Second), "just now", true},
		{now.Add(-3*time.Minute - 30*time.Second), "3 min ago", true},
		{now.Add(-time.Hour), "1 hour ago", true},
		{now.Add(-5 * time.Hour), "5 hours ago", true},
		{now.Add(-26 * time.Hour), "1 day ago", true},
		{now.Add(-29 * 24 * time.Hour), "29 days ago", true},
		{now.Add(4 * time.Hour), "in 4 hours", true},
		{now.Add(-110 * 24 * time.Hour), "Jul 30", false},
		{now.Add(-1281 * 24 * time.Hour), "May 16, 2023", false},
	} {
		got, relative := relativeTime(now, test.at)
		assert.Equal(t, test.want, got)
		assert.Equal(t, test.relative, relative)
	}
	assert.Equal(t, "—", formatAge(nil))
}

func TestTimeTag(t *testing.T) {
	at := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	want := `<time datetime="2020-03-04T05:06:07Z" title="2020-03-04 05:06:07 UTC" data-time="date">Mar 4, 2020</time>`
	assert.Equal(t, want, string(timeTag(at, false)))
	assert.Equal(t, want, string(timeTag(&at, false)))
	assert.Contains(t, string(timeTag(time.Now().Add(-90*time.Second), false)), `data-time="relative">1 min ago</time>`)
	var never *time.Time
	assert.Equal(t, `<span class="muted">—</span>`, string(timeTag(never, false)))
	assert.Equal(t, `<span class="muted">—</span>`, string(timeTag(time.Time{}, false)))

	// A log's rows share days, so a date there carries its time of day.
	assert.Equal(t, `<time datetime="2020-03-04T05:06:07Z" title="2020-03-04 05:06:07 UTC" data-time="datetime">Mar 4, 2020, 5:06 AM UTC</time>`,
		string(timeTag(at, true)))
	assert.Contains(t, string(timeTag(time.Now().Add(-90*time.Second), true)), `data-time="relative">1 min ago</time>`)
}

func TestActorName(t *testing.T) {
	assert.Equal(t, "admin (bootstrap)", actorName("admin@bootstrap.invalid"))
	assert.Equal(t, "Diderot", actorName("diderot:migration"))
	assert.Equal(t, "op@example.com", actorName("op@example.com"))
	assert.Equal(t, `<span class="badge badge-good">on</span>`, string(badge(toneGood, "On")))
}

func TestPickOverviewWindow(t *testing.T) {
	assert.Equal(t, "24h", pickOverviewWindow("").Key)
	assert.Equal(t, "24h", pickOverviewWindow("custom").Key)
	assert.Equal(t, 7*24*time.Hour, pickOverviewWindow("7d").Span)
	assert.Equal(t, 24*time.Hour, pickOverviewWindow("30d").Bucket)
}

func TestActivityChartScalesToRoundGridlines(t *testing.T) {
	chart := func(counts ...int64) barChart {
		var c barChart
		for _, n := range counts {
			c.Bars = append(c.Bars, chartBar{Count: n})
		}
		return c
	}
	for peak, top := range map[int64]int64{0: 2, 3: 4, 13: 20, 56: 60, 100: 100, 101: 200, 2500: 4000} {
		assert.Equal(t, top, chart(0, peak).Top(), "peak %d", peak)
	}
	drawn := chart(0, 15, 30).Drawn()
	assert.Equal(t, 40, int(chart(0, 15, 30).Top()))
	assert.InDelta(t, 2.15, drawn[2].X, 0.001)
	assert.InDelta(t, 75, drawn[2].Height, 0.001)
	assert.InDelta(t, 25, drawn[2].Y, 0.001)
	assert.Zero(t, drawn[0].Height)
	assert.Equal(t, 3, chart(1, 2, 3).Width())
}

func TestOverviewReadsAskAndTheSources(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_overview"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions, events, admin_jobs, scheduled_reports, slack_channel_sync, session_ingest_candidates RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	now := time.Now()
	answered, failing := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO ask_sessions (id, title, owner_email, surface, last_turn_at)
		VALUES ($1, 'Who owns billing?', 'op@example.com', 'slack', $3), ($2, NULL, NULL, 'http', $4)`,
		answered, failing, now.Add(-time.Hour), now.Add(-2*time.Hour))
	require.NoError(t, err)
	run := func(session uuid.UUID, at time.Time, ms int64, outcome, runErr string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO ask_runs (id, session_id, surface, model_id, started_at, completed_at, duration_ms, outcome, error)
			VALUES ($1, $2, 'slack', 'model', $3, $3, $4, $5, NULLIF($6, ''))`, uuid.New(), session, at, ms, outcome, runErr)
		require.NoError(t, err)
	}
	run(answered, now.Add(-90*time.Minute), 10_000, "answered", "")
	run(answered, now.Add(-80*time.Minute), 30_000, "answered", "")
	run(failing, now.Add(-2*time.Hour), 4_000, "error", "model call: llmclient: status 429: rate limited")
	run(answered, now.Add(-3*24*time.Hour), 20_000, "answered", "")

	_, err = pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, deleted) VALUES
		  ('alpha_events', 'a1', now() - interval '1 hour', 'one', '{}', false),
		  ('alpha_events', 'a2', now() - interval '2 hours', 'two', '{}', false),
		  ('alpha_events', 'a3', now(), 'gone', '{}', true),
		  ('retired', 'r1', now() - interval '400 days', 'old', '{}', false)`)
	require.NoError(t, err)

	s := &Server{
		pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		summaryControl: fakeSummaries{EffectiveEnabled: true},
		registries: setup.Registries{Sources: source.NewRegistry(
			reportingSource{namedSource: testSource("alpha", "Alpha"), status: settings.Status{Waiting: "Alpha needs a token."}},
			testSource("beta", "Beta"),
		)},
		connectors: &fakeConnectors{list: []settings.View{{Name: "alpha", Enabled: true}, {Name: "beta"}}},
	}
	data, err := s.loadOverview(ctx, pickOverviewWindow("24h"), now)
	require.NoError(t, err)

	assert.Equal(t, int64(3), data.Ask.Questions, "the last 24 hours' questions")
	assert.Equal(t, int64(2), data.Ask.Conversations)
	assert.Equal(t, int64(1), data.Ask.Failed)
	assert.Zero(t, data.Ask.Incomplete)
	assert.Equal(t, 33, data.Ask.FailedShare())
	require.NotNil(t, data.Ask.MedianAnswer)
	assert.Equal(t, 20*time.Second, *data.Ask.MedianAnswer, "the median of the answered questions")
	var charted int64
	for _, bar := range data.Chart.Bars {
		charted += bar.Count
	}
	assert.Equal(t, int64(3), charted)
	assert.Len(t, data.Chart.Bars, 25, "an hour a bar, both ends partial")

	require.Len(t, data.Recent, 2)
	assert.Equal(t, "Who owns billing?", data.Recent[0].Title)
	assert.Equal(t, askAnswered, data.Recent[0].State)
	assert.Equal(t, toneGood, data.Recent[0].Tone)
	require.Len(t, data.Failures, 1, "the conversations the Ask list's State filter finds")
	assert.Equal(t, failing.String(), data.Failures[0].ID)
	assert.Equal(t, askFailed, data.Failures[0].State)
	assert.Equal(t, "The model provider limited the request rate.", data.Failures[0].Reason)
	assert.Equal(t, "/admin/ask/sessions?status=failed&range=24h", data.FailedURL)
	listed, _, err := s.loadAskSessions(ctx, httptest.NewRequest(http.MethodGet, data.FailedURL, nil))
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1, "the link finds the conversation the Overview lists")
	assert.Equal(t, failing.String(), listed.Sessions[0].ID)
	assert.Equal(t, data.Failures[0].Reason, listed.Sessions[0].Reason, "and gives the same reason")

	require.Len(t, data.Attention, 2)
	assert.Equal(t, "1 of 3 questions in the last 24 hours failed.", data.Attention[0].Text)
	assert.Equal(t, "Alpha: alpha needs a token.", data.Attention[1].Text)
	assert.Equal(t, "/admin/connectors/alpha", data.Attention[1].Href)

	require.Len(t, data.Sources, 2, "Beta is off and has nothing, so it isn't listed")
	assert.Equal(t, overviewSource{Title: "Alpha", Href: "/admin/connectors/alpha", Tone: toneAttention, State: "needs attention",
		Detail: "Alpha needs a token.", Items: 2, Newest: data.Sources[0].Newest}, data.Sources[0])
	assert.Equal(t, "retired", data.Sources[1].Title, "items no connector writes any more")
	assert.Equal(t, int64(1), data.Sources[1].Items)

	var out bytes.Buffer
	data.pageData = pageData{Title: "Overview", CurrentNav: navOverview}
	require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", data))
	html := out.String()
	assert.Contains(t, html, "Needs attention")
	assert.Contains(t, html, "Questions per hour")
	assert.Contains(t, html, `<a class="row-link" href="/admin/ask/sessions/`+failing.String()+`">Untitled conversation</a>`)
	for _, internal := range []string{"Postgres", "Qdrant", "cursor", "lease", "claimed", "genuine", "persisted"} {
		assert.NotContains(t, html, internal)
	}

	// A quiet install: nothing asked, and only Beta on, with nothing yet.
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions, events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	s.overview = overviewCache{}
	s.connectors = &fakeConnectors{list: []settings.View{{Name: "alpha"}, {Name: "beta", Enabled: true}}}
	data, err = s.loadOverview(ctx, pickOverviewWindow("7d"), now)
	require.NoError(t, err)
	assert.Empty(t, data.Attention)
	require.Len(t, data.Sources, 1)
	assert.Equal(t, "Beta", data.Sources[0].Title)
	out.Reset()
	data.pageData = pageData{Title: "Overview", CurrentNav: navOverview}
	require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", data))
	assert.Contains(t, out.String(), "Nothing needs your attention right now.")
	assert.Contains(t, out.String(), "No questions in the last 7 days")
	assert.NotContains(t, out.String(), "Recent conversations", "nothing to list yet")

	// Every source off: nothing new reaches the index.
	s.connectors = &fakeConnectors{list: []settings.View{{Name: "alpha"}, {Name: "beta"}}}
	data, err = s.loadOverview(ctx, pickOverviewWindow("7d"), now)
	require.NoError(t, err)
	require.Len(t, data.Attention, 1)
	assert.Equal(t, "No source is on, so Diderot has nothing to index.", data.Attention[0].Text)
	assert.Empty(t, data.Sources)

	// Pushed sessions arrive with every source off.
	_, err = pool.Exec(ctx, `INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, complete, first_pending_at)
		VALUES ('op@example.com', 'op@example.com', 'codex', 'pushed', 'rev-1', true, now())`)
	require.NoError(t, err)
	data, err = s.loadOverview(ctx, pickOverviewWindow("7d"), now)
	require.NoError(t, err)
	for _, item := range data.Attention {
		assert.NotContains(t, item.Text, "No source is on")
	}
}
