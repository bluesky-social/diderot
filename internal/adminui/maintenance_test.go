package adminui

import (
	"bytes"
	"context"
	"fmt"
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

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminjob"
	linearcx "github.com/bluesky-social/diderot/internal/connector/linear"
	markdowncx "github.com/bluesky-social/diderot/internal/connector/markdown"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	zendeskcx "github.com/bluesky-social/diderot/internal/connector/zendesk"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// adminBrowser is a signed-in administrator's browser on a mounted server.
type adminBrowser struct {
	t    *testing.T
	mux  *http.ServeMux
	csrf string
}

func newAdminBrowser(t *testing.T, cfg Config) *adminBrowser {
	t.Helper()
	cfg.Authenticator = testBrowserAuth()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.AdminGroups, cfg.PublicURL = []string{"admins"}, "https://diderot.test"
	server, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	csrf, err := issueCSRFToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://diderot.test/admin/", nil))
	require.NoError(t, err)
	return &adminBrowser{t: t, mux: mux, csrf: csrf}
}

func (b *adminBrowser) get(path string) string {
	b.t.Helper()
	w := browserRequest(b.t, b.mux, http.MethodGet, "https://diderot.test"+path, nil, b.csrf)
	require.Equal(b.t, http.StatusOK, w.Code, w.Body.String())
	return w.Body.String()
}

// post sends the form and returns the notice or error the page it
// redirects to shows, and whether it was an error.
func (b *adminBrowser) post(path string, form url.Values) (string, bool) {
	b.t.Helper()
	form.Set("csrf_token", b.csrf)
	w := browserRequest(b.t, b.mux, http.MethodPost, "https://diderot.test"+path, form, b.csrf)
	require.Equal(b.t, http.StatusSeeOther, w.Code, w.Body.String())
	return redirectMessage(b.t, w.Header().Get("Location"))
}

// audited are the audit rows written so far, as "action target status".
func audited(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT action || ' ' || target || ' ' || status FROM admin_audit_log ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestSlackDocumentRebuildBrowserWorkflow: Jobs and maintenance offers
// reading every channel's documents again while Slack is on and a channel
// indexes documents, asks for a confirmation, and starts it as one audited
// job.
func TestSlackDocumentRebuildBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_document_rebuild"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, admin_jobs, slack_channel_config RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	control := &recordingSourceControl{}
	b := newAdminBrowser(t, Config{
		Pool: pool, SlackSourceControl: control, Connectors: &everyConnector{},
		Registries: pageRegistries(), SourceDeps: source.Deps{Pool: pool},
	})

	page := b.get("/admin/operations/jobs")
	assert.Contains(t, page, "Read Slack documents again")
	assert.Contains(t, page, "No channel has document indexing on.")
	assert.NotContains(t, page, `action="/admin/operations/slack-documents"`)

	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_config (channel_id, doc_ingest_enabled) VALUES ('CONE', true), ('CTWO', true), ('COFF', false)`)
	require.NoError(t, err)
	page = b.get("/admin/operations/jobs")
	assert.Contains(t, page, `action="/admin/operations/slack-documents"`)
	assert.Contains(t, page, "Read the documents of 2 channels again")

	message, refused := b.post("/admin/operations/slack-documents", url.Values{})
	assert.True(t, refused)
	assert.Contains(t, message, "tick the box")
	assert.Zero(t, control.documentRebuilds)
	assert.Empty(t, audited(t, pool), "an unconfirmed request does nothing")

	message, refused = b.post("/admin/operations/slack-documents", url.Values{"confirm": {"documents"}})
	assert.False(t, refused, message)
	assert.Equal(t, "Reading every channel's documents again. Follow it under Jobs below.", message)
	assert.Equal(t, 1, control.documentRebuilds)
	assert.Equal(t, []string{"sources.slack.documents.rebuild all succeeded"}, audited(t, pool))

	for want, cause := range map[string]error{
		"already being read again":            adminjob.ErrActive,
		"Slack is off":                        adminapi.ErrSlackOff,
		"no channel has document indexing on": adminapi.ErrNoDocumentChannels,
		"the audit log has the details":       fmt.Errorf("pq: relation does not exist"),
	} {
		control.documentErr = cause
		message, _ = b.post("/admin/operations/slack-documents", url.Values{"confirm": {"documents"}})
		assert.Contains(t, message, want)
		assert.NotContains(t, message, "pq:", "an internal error stays in the audit log")
	}

	// A running rebuild replaces the form, and the job list words its progress.
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_jobs (kind, status, requested_by, request, progress, started_at, stale_after_seconds)
		VALUES ('slack_document_rebuild', 'running', 'operator@example.com', '{}', '{"scanned":40,"queued":38,"documents":52}', now(), 120)`)
	require.NoError(t, err)
	page = b.get("/admin/operations/jobs")
	assert.NotContains(t, page, `action="/admin/operations/slack-documents"`)
	assert.Contains(t, page, "Re-read Slack documents")
	assert.Contains(t, page, "40 messages with files read · 52 documents to read again")
	assert.Contains(t, page, "data-jobs-active", "the page refreshes itself while it runs")

	control.off = true
	_, err = pool.Exec(ctx, `TRUNCATE admin_jobs`)
	require.NoError(t, err)
	assert.Contains(t, b.get("/admin/operations/jobs"), "Documents can be read again only while the")
	control.off = false
	assert.Contains(t, b.get("/admin/connectors/slack"), `href="/admin/operations/jobs#slack-documents"`, "the Slack connector links to it")
}

// TestSlackChannelInviteBrowserWorkflow: the channel list adds the bot to a
// channel Diderot does not list yet, named by its name, ID or link, as one
// audited action, and says what it needs when it can't.
func TestSlackChannelInviteBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_channel_invite"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	control := &recordingSourceControl{resolved: map[string][2]string{
		"platform": {"CPLATFORM", "platform"}, "CNEW00001": {"CNEW00001", "launch"},
	}}
	b := newAdminBrowser(t, Config{Pool: pool, SlackSourceControl: control, Registries: pageRegistries(), SourceDeps: source.Deps{Pool: pool}})

	page := b.get("/admin/connectors/slack/channels")
	assert.Contains(t, page, "Add the bot to a channel")
	assert.Contains(t, page, `action="/admin/connectors/slack/channels/invite"`)

	message, refused := b.post("/admin/connectors/slack/channels/invite", url.Values{"channel": {" #platform "}})
	assert.False(t, refused, message)
	assert.Equal(t, "The bot is in #platform now. Diderot starts reading it within a few minutes, and lists it here then.", message)
	message, refused = b.post("/admin/connectors/slack/channels/invite", url.Values{"channel": {"https://example.slack.com/archives/CNEW00001/p1700000000351689"}})
	assert.False(t, refused, message)
	assert.Contains(t, message, "The bot is in #launch now.")
	assert.Equal(t, []string{"CPLATFORM", "CNEW00001"}, control.invited)
	assert.Equal(t, []string{"sources.slack.invite CPLATFORM succeeded", "sources.slack.invite CNEW00001 succeeded"}, audited(t, pool))

	for channel, want := range map[string]string{
		"":         "enter the channel's name, ID or link",
		"#nowhere": "Slack has no channel nowhere that the user token can see",
	} {
		message, refused = b.post("/admin/connectors/slack/channels/invite", url.Values{"channel": {channel}})
		assert.True(t, refused)
		assert.Contains(t, message, want)
	}
	for want, cause := range map[string]error{
		"Slack is off":                     adminapi.ErrSlackOff,
		"needs a Slack user token":         adminapi.ErrNoSlackUserToken,
		"Slack could not look the channel": fmt.Errorf("slack: ratelimited"),
	} {
		control.resolveErr = cause
		message, refused = b.post("/admin/connectors/slack/channels/invite", url.Values{"channel": {"platform"}})
		assert.True(t, refused)
		assert.Contains(t, message, want)
	}
	assert.Len(t, control.invited, 2, "a channel Slack could not name is not invited")
	assert.Len(t, audited(t, pool), 2, "and nothing was done to record")

	control.noToken = true
	page = b.get("/admin/connectors/slack/channels")
	assert.NotContains(t, page, `action="/admin/connectors/slack/channels/invite"`)
	assert.Contains(t, page, "needs a Slack user token, and none is set")
	control.off = true
	assert.NotContains(t, b.get("/admin/connectors/slack/channels"), `id="add-heading"`, "nothing can be added while Slack is off")
}

// manyDiscoveries is a Notion store that knows count shared pages, the last
// third of them under a page Diderot indexes; like the real store, it lists
// those not indexed first.
type manyDiscoveries struct {
	count int
	added []string
}

func (*manyDiscoveries) List(context.Context) ([]notioncx.Root, error) { return nil, nil }
func (m *manyDiscoveries) ListDiscoveries(context.Context) ([]notioncx.DiscoveryStatus, error) {
	var out []notioncx.DiscoveryStatus
	for i := range m.count {
		out = append(out, notioncx.DiscoveryStatus{
			ObjectType: "page", ObjectID: fmt.Sprintf("page-%03d", i), Title: fmt.Sprintf("Shared page %03d", i), LastSeen: time.Now(),
			Covered: i >= m.count*2/3,
		})
	}
	return out, nil
}
func (m *manyDiscoveries) Add(_ context.Context, input, label, _ string) (string, error) {
	m.added = append(m.added, input+" "+label)
	return input, nil
}
func (*manyDiscoveries) Remove(context.Context, string) (bool, error) { return true, nil }

// TestNotionSharedPagesArePagedSearchedAndAddable: every page shared with
// the integration is reachable on the Notion connector's page, a page at a
// time or by searching, and each one Diderot does not index has its Add.
func TestNotionSharedPagesArePagedSearchedAndAddable(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_notion_shared"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	store := &manyDiscoveries{count: 60}
	b := newAdminBrowser(t, Config{
		Pool: pool, NotionRootsStore: store, Connectors: &everyConnector{},
		Registries: pageRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	rows := func(body string) int { return strings.Count(body, `<span class="id">page-`) }
	adds := func(body string) int { return strings.Count(body, `name="input" value="page-`) }

	first := b.get("/admin/connectors/notion")
	assert.Contains(t, first, "Pages shared with the integration: 60, 40 not indexed")
	assert.Equal(t, 25, rows(first))
	assert.Equal(t, 25, adds(first), "the pages not indexed come first, each with its Add")
	assert.Contains(t, first, "1–25 of 60 pages")
	assert.Contains(t, first, `href="?shared_page=2#notion-shared"`)
	assert.NotContains(t, first, `<details class="disclosure" id="notion-shared" open>`, "closed until someone looks")

	last := b.get("/admin/connectors/notion?shared_page=3")
	assert.Equal(t, 10, rows(last))
	assert.Zero(t, adds(last), "an indexed page has no Add")
	assert.Contains(t, last, "51–60 of 60 pages")
	assert.Contains(t, last, `<details class="disclosure" id="notion-shared" open>`)
	assert.Contains(t, b.get("/admin/connectors/notion?shared_page=99"), "51–60 of 60 pages", "a page past the last shows the last")

	found := b.get("/admin/connectors/notion?shared=PAGE+01")
	assert.Equal(t, 10, rows(found), "Shared page 010 to 019")
	assert.Contains(t, found, `name="shared" value="PAGE 01"`)
	assert.Contains(t, b.get("/admin/connectors/notion?shared=nothing-like-it"), "No shared page matches")

	message, refused := b.post("/admin/connectors/notion/roots", url.Values{"input": {"page-001"}, "label": {"Shared page 001"}})
	assert.False(t, refused, message)
	assert.Equal(t, []string{"page-001 Shared page 001"}, store.added)
	assert.Equal(t, []string{"sources.notion.root.add page-001 succeeded"}, audited(t, pool))
}

// TestConnectorSlackChannelFieldTakesANameIDOrLink: a connector field that
// holds a Slack channel takes its name, its ID or a link to it, saves the
// ID, and shows the name.
func TestConnectorSlackChannelFieldTakesANameIDOrLink(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_channel_field"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name) VALUES ('C0EXAMPLE102', 'alerts')
		ON CONFLICT (channel_id) DO UPDATE SET name = EXCLUDED.name, is_archived = false`)
	require.NoError(t, err)
	view := exampleConnectorView()
	view.Fields = append(view.Fields, settings.FieldView{Key: "channel", Label: "Alerts channel", Kind: settings.KindText, SlackChannel: true})
	view.Settings["channel"] = "C0EXAMPLE102"
	store := &fakeConnectors{view: view, record: passingRecord()}
	mux, csrf := connectorTestServer(t, store, pool)
	post := connectorPoster(t, mux, csrf)

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, page, `name="field.channel" value="#alerts"`, "the saved ID shows as the channel's name")

	for _, typed := range []string{"#alerts", "Alerts", "C0EXAMPLE102", "https://example.slack.com/archives/C0EXAMPLE102"} {
		store.saved = nil
		post("/admin/connectors/example", url.Values{"version": {"4"}, "field.mode": {"one"}, "field.channel": {typed}})
		assert.Equal(t, "C0EXAMPLE102", store.saved["channel"], typed)
	}
	store.saved = nil
	post("/admin/connectors/example", url.Values{"version": {"4"}, "field.mode": {"one"}, "field.channel": {"C0EXAMPLE103"}})
	assert.Equal(t, "C0EXAMPLE103", store.saved["channel"], "a well-formed ID Diderot has not seen is kept")

	store.saved = nil
	form := url.Values{"version": {"4"}, "field.mode": {"one"}, "field.channel": {"#nowhere"}, "csrf_token": {csrf}}
	refused := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/connectors/example", form, csrf)
	require.Equal(t, http.StatusBadRequest, refused.Code)
	assert.Contains(t, refused.Body.String(), "Diderot does not know a channel named #nowhere.")
	assert.Contains(t, refused.Body.String(), `name="field.channel" value="#nowhere"`, "what was typed stays in the form")
	assert.Nil(t, store.saved)

	tested := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/connectors/example/test",
		url.Values{"version": {"4"}, "field.mode": {"one"}, "field.channel": {"alerts"}, "csrf_token": {csrf}}, csrf)
	require.Equal(t, http.StatusOK, tested.Code, tested.Body.String())
	require.NotNil(t, store.draft)
	assert.Equal(t, "C0EXAMPLE102", store.draft.Settings["channel"], "a test runs on the channel's ID too")
}

// TestConnectorPageWordsItsChoicesAndFacts: an enum's options show under
// the words its definition gives them, and a discovered fact's identifier
// sits beside its name.
func TestConnectorPageWordsItsChoicesAndFacts(t *testing.T) {
	view := exampleConnectorView()
	view.Enabled = true
	view.Fields[2].OptionLabels = []string{"Only the first", "Both of them"}
	record := passingRecord()
	record.Facts = append(record.Facts, settings.Fact{Key: "bot_user", Label: "Bot user", Value: "diderot", ID: "U0BOT"})
	view.LastTest = &record
	mux, _ := connectorTestServer(t, &fakeConnectors{view: view})
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/example", nil, "").Body.String()
	assert.Contains(t, page, `<option value="one">Only the first</option><option value="two" selected>Both of them</option>`)
	assert.Contains(t, page, `<dt>Bot user</dt><dd>diderot <span class="id">U0BOT</span></dd>`)
}

// TestOverviewAndStatusCountWhatEverySourceIndexed: an item a source
// indexed is counted on the Overview and dated on Operations > Status,
// for every kind of source, and a count taken before a source's first
// items is taken again once it is a few seconds old.
func TestOverviewAndStatusCountWhatEverySourceIndexed(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_source_counts"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions, events, admin_jobs, slack_channel_sync, notion_sync_runs RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	feed := testSource("session_feed", "Session feed")
	feed.info.EventSource = "agent_session"
	sources := source.NewRegistry(slackSource{}, feed, notioncx.Connector{}, linearcx.Connector{}, zendeskcx.Connector{}, markdowncx.Connector{})
	var views []settings.View
	for _, src := range sources.Sources() {
		views = append(views, settings.View{Name: src.Name(), Enabled: true})
	}
	s := &Server{
		pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		registries: setup.Registries{Sources: sources}, sourceDeps: source.Deps{Pool: pool},
		connectors: &fakeConnectors{list: views},
	}
	overview := func() map[string]overviewSource {
		data, err := s.loadOverview(ctx, pickOverviewWindow("24h"), time.Now())
		require.NoError(t, err)
		byTitle := map[string]overviewSource{}
		for _, row := range data.Sources {
			byTitle[row.Title] = row
		}
		return byTitle
	}

	// A fresh install: every source is on and nothing is indexed yet.
	for title, row := range overview() {
		assert.Zero(t, row.Items, title)
	}
	for _, info := range sources.Infos() {
		_, err = pool.Exec(ctx, `
			INSERT INTO events (source, external_id, occurred_at, body, raw) VALUES
			  ($1, $1 || ':1', now() - interval '3 minutes', 'one', '{}'),
			  ($1, $1 || ':2', now() - interval '2 hours', 'two', '{}')`, info.EventSource)
		require.NoError(t, err)
	}
	for title, row := range overview() {
		assert.Zero(t, row.Items, "%s: the count is a few seconds old at most, so it is served as it is", title)
	}
	s.overview.mu.Lock()
	s.overview.totals.At = time.Now().Add(-overviewEmptyMaxAge - time.Second)
	s.overview.mu.Unlock()
	rows := overview()
	for _, info := range sources.Infos() {
		row, listed := rows[info.Title]
		require.True(t, listed, info.Title)
		assert.Equal(t, int64(2), row.Items, info.Title)
		require.NotNil(t, row.Newest, info.Title)
		assert.WithinDuration(t, time.Now().Add(-3*time.Minute), *row.Newest, time.Minute, info.Title)
	}

	// Status reads each source's newest item as it is now, whatever the
	// Overview last counted.
	_, err = pool.Exec(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw) VALUES ('markdown', 'markdown:3', now(), 'three', '{}')`)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.handleOperations(w, httptest.NewRequest(http.MethodGet, "/admin/operations", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	status := w.Body.String()
	for _, info := range sources.Infos() {
		assert.Contains(t, status, ">"+info.Title+"</a>")
	}
	assert.NotContains(t, status, "Nothing yet", "every source has a newest item")
	assert.Equal(t, 1, strings.Count(status, ">just now<")+strings.Count(status, " seconds ago<"), "the Markdown folder's newest item is the one just indexed")
}

// TestAskActivityAnswerTimes: a quick answer keeps its tenths, and the
// slowest 5% is said only once there are enough answers for it to mean
// something.
func TestAskActivityAnswerTimes(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_answer_times"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE ask_sessions RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	session := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO ask_sessions (id, surface) VALUES ($1, 'http')`, session)
	require.NoError(t, err)
	answer := func(ms int64) {
		_, err := pool.Exec(ctx, `INSERT INTO ask_runs (id, session_id, surface, model_id, started_at, completed_at, duration_ms, outcome)
			VALUES ($1, $2, 'http', 'model', now() - interval '1 minute', now(), $3, 'answered')`, uuid.New(), session, ms)
		require.NoError(t, err)
	}
	now := time.Now()
	answer(340)
	activity, err := s.loadAskActivity(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, activity.MedianAnswer)
	assert.Equal(t, 300*time.Millisecond, *activity.MedianAnswer)
	assert.Equal(t, "300ms", formatDuration(*activity.MedianAnswer), "not 0s")
	assert.Nil(t, activity.SlowAnswer, "one answer has no slowest 5%")

	var page bytes.Buffer
	require.NoError(t, overviewPage.ExecuteTemplate(&page, "layout", overviewData{Ask: activity}))
	assert.Contains(t, page.String(), `<span class="stat-value">300ms</span><span class="stat-note">Median</span>`)

	for range slowAnswerMinimum - 1 {
		answer(4_200)
	}
	activity, err = s.loadAskActivity(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, activity.SlowAnswer)
	assert.Equal(t, "4.2s", formatDuration(*activity.MedianAnswer))
	assert.Equal(t, "4.2s", formatDuration(*activity.SlowAnswer))
}

// TestAccessPageSaysHowCallersSignInAndWhetherSlackIsConnected: the API
// sign-in reads in words beside the deployment's value, and the Ask in
// Slack section tells a Slack that is off from one where no channel has Ask.
func TestAccessPageSaysHowCallersSignInAndWhetherSlackIsConnected(t *testing.T) {
	for mode, words := range map[string]string{
		"static": "An API token", "oidc": "Your identity provider (OIDC)", "none": "No sign-in: anyone who can reach the server",
	} {
		assert.Equal(t, words, accessData{Summary: setup.AccessSummary{Access: setup.Access{AuthMode: mode}}}.APISignIn())
	}
	render := func(data accessData) string {
		var page bytes.Buffer
		require.NoError(t, accessPage.ExecuteTemplate(&page, "layout", data))
		return page.String()
	}
	data := accessData{Summary: setup.AccessSummary{Access: setup.Access{AuthMode: "static"}}}
	off := render(data)
	assert.Contains(t, off, `<dt>API and MCP sign-in<span class="sub"><code>DIDEROT_AUTH_MODE</code></span></dt><dd>An API token <span class="id">static</span></dd>`)
	assert.Contains(t, off, "Slack is not connected")
	assert.Contains(t, off, `href="/admin/connectors/slack">Set up Slack</a>`)
	assert.NotContains(t, off, "Ask is not on in any Slack channel")
	data.SlackOn = true
	on := render(data)
	assert.Contains(t, on, "Ask is not on in any Slack channel")
	assert.NotContains(t, on, "Slack is not connected")
}
