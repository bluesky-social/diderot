package adminui

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

type recordingReembedLauncher struct {
	jobID       int64
	err         error
	requestedBy string
	sources     []string
	calls       int
}

func (l *recordingReembedLauncher) StartReembed(_ context.Context, requestedBy string, sources []string) (int64, error) {
	l.calls++
	l.requestedBy = requestedBy
	l.sources = append([]string(nil), sources...)
	return l.jobID, l.err
}

func TestJobWords(t *testing.T) {
	for status, want := range map[string][2]string{
		"queued": {"waiting to start", toneAttention}, "running": {"running", toneAttention},
		"succeeded": {"done", toneGood}, "failed": {"failed", toneBad}, "canceled": {"canceled", toneNeutral},
	} {
		word, tone := jobState(status, false)
		assert.Equal(t, want, [2]string{word, tone}, status)
	}
	word, tone := jobState("running", true)
	assert.Equal(t, [2]string{"stalled", toneBad}, [2]string{word, tone}, "whatever ran it stopped")
	word, _ = jobState("queued", true)
	assert.Equal(t, "waiting to start", word, "a queued job has nothing to go stale")

	assert.Equal(t, "Re-read Slack images", jobName("slack_image_replay"))
	assert.Equal(t, "Some new job", jobName("some_new_job"))
	assert.Equal(t, "#platform · 12 messages read · 10 rebuilt · 1 error",
		jobProgress(jobRow{Kind: "slack_image_replay", channelID: "CPLATFORM", progress: []byte(`{
			"phase":"complete","channel_id":"CPLATFORM","scanned":12,"reconciled":10,"errors":1}`)}, "platform", nil))
	sources := source.Infos{{EventSource: "slack", Title: "Slack"}}
	assert.Equal(t, "All sources · 6 of 12 items (50%) · 2 could not be rebuilt",
		jobProgress(jobRow{Kind: "reembed", progress: []byte(`{"total":12,"processed":6,"errors":2}`)}, "", sources))
	assert.Equal(t, "Slack, wiki", jobProgress(jobRow{Kind: "reembed", request: []byte(`{"sources":["slack","wiki"]}`), progress: []byte(`{}`)}, "", sources),
		"which sources, before it counts; a source no longer registered by its key")
	assert.Equal(t, "Re-read Slack documents", jobName("slack_document_rebuild"))
	assert.Equal(t, "40 messages with files read · 52 documents to read again · 1 error",
		jobProgress(jobRow{Kind: "slack_document_rebuild", progress: []byte(`{"scanned":40,"queued":38,"documents":52,"errors":1}`)}, "", nil))
	assert.Equal(t, "", jobProgress(jobRow{Kind: "slack_document_rebuild", progress: []byte(`{}`)}, "", nil))

	assert.Equal(t, "one two three", compactText(" one\n two\tthree ", 20))
	assert.Equal(t, "αβ…", compactText("αβγ", 2))
}

// statusServer is a server whose registry has Slack, Notion, the session
// feed, and an off Linear, with the summaries control given.
func statusServer(pool *pgxpool.Pool, summaries SessionSummaryControl) *Server {
	slack := testSource("slack", "Slack")
	slack.info.EventSource = "slack"
	notion := testSource("notion", "Notion")
	notion.info.EventSource = "notion"
	feed := reportingSource{namedSource: testSource("session_feed", "Session feed"),
		status: settings.Status{Waiting: "Sessions aren't summarized, so they aren't searchable."}}
	feed.info.EventSource = "agent_session"
	linear := testSource("linear", "Linear")
	return &Server{
		pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), summaryControl: summaries,
		registries: setup.Registries{Sources: source.NewRegistry(slack, notion, feed, linear)},
		connectors: &fakeConnectors{list: []settings.View{
			{Name: "slack", Enabled: true}, {Name: "notion", Enabled: true}, {Name: "session_feed", Enabled: true}, {Name: "linear"},
		}},
	}
}

func TestStatusSaysWhatTheBackgroundWorkAndSourcesAreDoing(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_operations"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events, admin_jobs, slack_channel_sync, slack_channel_config, notion_sync_runs,
		session_ingest_candidates, scheduled_reports RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	old := time.Now().Add(-2 * time.Hour)
	var eventID, deletedID int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, ingested_at, body, raw)
		VALUES ('slack', 'COPS:1.0', $1, $1, 'worker fixture', '{}') RETURNING id`, old).Scan(&eventID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw, deleted)
		VALUES ('slack', 'COPS:2.0', $1, 'deleted', '{}', true) RETURNING id`, old).Scan(&deletedID))
	// A chunk that waits with nothing indexed for two hours: indexing has
	// stalled. One that gave up, and a deleted event's, which is no one's
	// concern.
	_, err = pool.Exec(ctx, `INSERT INTO event_chunks (event_id, chunk_idx, content, occurred_at, failure_count, last_failure, queued_at)
		VALUES ($1, 0, 'waiting', $2, 0, NULL, $2), ($1, 1, 'gave up', $2, 5, 'embed: status 503: overloaded', $2),
		       ($3, 0, 'deleted', $2, 5, 'must stay hidden', $2)`, eventID, old, deletedID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, captioned_at)
		VALUES ($1, 'FDONE', 'image/png', $2, now())`, eventID, old)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, last_error, ingest_enabled)
		VALUES ('COPS', now(), 'not_in_channel', true), ('CNEW', NULL, NULL, true), ('COFF', now(), 'ignored', false)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO notion_sync_runs (run_id, run_type, started_at, status, attempted, failed_count)
		VALUES ('r1', 'reconcile', now(), 'partial', 12, 2)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, complete, first_pending_at)
		VALUES ('op@example.com', 'op@example.com', 'codex', 'waiting-session', 'rev-1', true, now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO admin_jobs (kind, status, requested_by, progress, started_at, heartbeat_at, stale_after_seconds)
		VALUES ('reembed', 'running', 'op@example.com', '{"total":10,"processed":4}', now(), now(), 30)`)
	require.NoError(t, err)

	s := statusServer(pool, fakeSummaries{ModelAvailable: true, Blocked: sessioningest.SummaryPaused,
		Reason: "Session summaries are paused: resume them on the Jobs and maintenance page, under Operations."})
	h, err := s.loadHealth(ctx)
	require.NoError(t, err)

	work := map[string]workRow{}
	for _, row := range h.Work {
		work[row.ID] = row
	}
	assert.Equal(t, []string{"index", "captions", "summaries", "jobs"}, workIDs(h.Work), "no documents yet, no report on")
	assert.Equal(t, "needs attention", work["index"].State)
	assert.Equal(t, "1 item waiting to be indexed, and none indexed in the last 15 minutes.", work["index"].Detail)
	assert.Equal(t, "1 item could not be indexed.", work["index"].Note, "the deleted event's chunk is left out")
	assert.Equal(t, "up to date", work["captions"].State)
	assert.Equal(t, "Every image has a caption.", work["captions"].Detail)

	drained := queueRow(time.Now(), "index", "Search index", queueState{GaveUp: 2}, queueWords{
		one: "item", many: "items", upToDate: "Every item is searchable.", drained: "Nothing is waiting to be indexed.", gaveUp: "could not be indexed",
	})
	assert.Equal(t, "Nothing is waiting to be indexed.", drained.Detail, "not every item is searchable while some gave up")
	assert.Equal(t, "2 items could not be indexed.", drained.Note)
	assert.Equal(t, "paused", work["summaries"].State)
	assert.Equal(t, toneAttention, work["summaries"].Tone, "a session waits on the pause")
	assert.Equal(t, "Re-embed: running, 40% done.", work["jobs"].Detail)

	require.Len(t, h.Sources, 4)
	assert.Equal(t, "1 of the 2 channels the bot is in can't be read.", h.Sources[0].Detail)
	assert.Equal(t, "needs attention", h.Sources[0].State)
	assert.Equal(t, "The last sync could not read 2 pages.", h.Sources[1].Detail)
	assert.Equal(t, "off", h.Sources[3].State)

	var attention []string
	for _, item := range h.Attention {
		attention = append(attention, item.Text)
	}
	assert.Equal(t, []string{
		"Search index has stalled: 1 item waiting to be indexed, and none indexed in the last 15 minutes.",
		"1 agent session waiting for a summary is not searchable: summaries are paused.",
		"Slack: 1 of the 2 channels the bot is in can't be read.",
		"Notion: the last sync could not read 2 pages.",
		"1 item stopped after 5 attempts for a reason that retrying may fix.",
	}, attention, "the session feed's own wait is the summaries row's")

	w := httptest.NewRecorder()
	s.handleOperations(w, httptest.NewRequest(http.MethodGet, "/admin/operations", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Background work")
	assert.Contains(t, body, `<a class="row-link" href="/admin/connectors/slack">Slack</a>`)
	assert.NotContains(t, body, ">Linear<", "an off source is counted, not listed")
	assert.Contains(t, body, "One more source is off.")
	for _, internal := range []string{"Postgres", "Qdrant", "cursor", "lease", "claimed", "stranded", "durable", "must stay hidden"} {
		assert.NotContains(t, body, internal)
	}
}

func workIDs(rows []workRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func TestOperationsReembedBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_operations_reembed"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, admin_jobs RESTART IDENTITY`)
	require.NoError(t, err)

	launcher := &recordingReembedLauncher{jobID: 42}
	slack, wiki := testSource("slack", "Slack"), testSource("wiki", "Wiki")
	slack.info.EventSource, wiki.info.EventSource = "slack", "wiki"
	connectors := &fakeConnectors{list: []settings.View{{Name: "slack", Enabled: true}, {Name: "wiki"}}}
	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		ReembedLauncher: launcher, ReembedSources: []string{"slack", "wiki"},
		Registries: setup.Registries{Sources: source.NewRegistry(slack, wiki, testSource("linear", "Linear"))}, Connectors: connectors,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), ">Start re-embed</button>")
	assert.Contains(t, page.Body.String(), `<input type="checkbox" name="source" value="slack" checked> <span>Slack</span>`, "a source that is on can be chosen")
	assert.Contains(t, page.Body.String(), `<input type="checkbox" name="source" value="wiki" disabled> <span>Wiki</span>`, "one that is off can't")
	assert.NotContains(t, page.Body.String(), `value="linear"`, "a source nothing rebuilds is not offered")
	assert.NotContains(t, page.Body.String(), "data-jobs-active")
	assert.Contains(t, page.Body.String(), "No jobs yet")
	assert.NotContains(t, page.Body.String(), "Session summaries", "nothing here uses agent sessions")
	var csrfToken string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.True(t, validCSRFToken(csrfToken))

	badCSRF := url.Values{"csrf_token": {"wrong"}, "confirm": {"reembed"}}
	rejected := browserRequest(t, mux, http.MethodPost,
		"https://diderot.test/admin/operations/reembed", badCSRF, csrfToken)
	assert.Equal(t, http.StatusForbidden, rejected.Code)
	assert.Zero(t, launcher.calls)

	missingConfirmation := url.Values{"csrf_token": {csrfToken}, "source": {"slack"}}
	rejected = browserRequest(t, mux, http.MethodPost,
		"https://diderot.test/admin/operations/reembed", missingConfirmation, csrfToken)
	require.Equal(t, http.StatusSeeOther, rejected.Code, "back to the page, which says why")
	assert.Contains(t, rejected.Header().Get("Location"), "/admin/operations/jobs?error=Nothing+was+started")
	assert.Zero(t, launcher.calls)

	for name, refused := range map[string]struct {
		sources []string
		why     string
	}{
		"none chosen":           {nil, "choose+at+least+one+source"},
		"one that is off":       {[]string{"slack", "wiki"}, "Wiki+is+off"},
		"one nothing rebuilds":  {[]string{"linear"}, "cannot+rebuild+linear"},
		"one that is not there": {[]string{"nope"}, "cannot+rebuild+nope"},
	} {
		rejected = browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/operations/reembed",
			url.Values{"csrf_token": {csrfToken}, "confirm": {"reembed"}, "source": refused.sources}, csrfToken)
		require.Equal(t, http.StatusSeeOther, rejected.Code, name)
		assert.Contains(t, rejected.Header().Get("Location"), "error=Nothing+was+started", name)
		assert.Contains(t, rejected.Header().Get("Location"), refused.why, name)
		assert.Zero(t, launcher.calls, name)
	}

	form := url.Values{"csrf_token": {csrfToken}, "confirm": {"reembed"}, "source": {"slack"}}
	started := browserRequest(t, mux, http.MethodPost,
		"https://diderot.test/admin/operations/reembed", form, csrfToken)
	require.Equal(t, http.StatusSeeOther, started.Code)
	assert.Equal(t, "/admin/operations/jobs?notice=Re-embed+requested.+It+starts+within+a+few+seconds.", started.Header().Get("Location"))
	assert.Equal(t, 1, launcher.calls)
	assert.Equal(t, "operator@example.com", launcher.requestedBy)
	assert.Equal(t, []string{"slack"}, launcher.sources, "one of the two sources a re-embed rebuilds")

	// With every source it rebuilds on and chosen, the request names none.
	connectors.list[1].Enabled = true
	all := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/operations/reembed",
		url.Values{"csrf_token": {csrfToken}, "confirm": {"reembed"}, "source": {"wiki", "slack"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, all.Code)
	assert.Equal(t, 2, launcher.calls)
	assert.Empty(t, launcher.sources)

	// With every source it rebuilds off, the page says why it offers nothing.
	connectors.list[0].Enabled, connectors.list[1].Enabled = false, false
	off := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, "")
	assert.NotContains(t, off.Body.String(), ">Start re-embed</button>")
	assert.Contains(t, off.Body.String(), "Slack, Wiki are off")
	connectors.list[0].Enabled = true

	var actor, action, target, status string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT actor_email, action, target, status
		  FROM admin_audit_log
		 ORDER BY id DESC LIMIT 1`).Scan(&actor, &action, &target, &status))
	assert.Equal(t, "operator@example.com", actor)
	assert.Equal(t, "operations.reembed.start", action)
	assert.Equal(t, "reembed", target)
	assert.Equal(t, "succeeded", status)

	launcher.err = adminjob.ErrActive
	active := browserRequest(t, mux, http.MethodPost,
		"https://diderot.test/admin/operations/reembed", form, csrfToken)
	require.Equal(t, http.StatusSeeOther, active.Code)
	assert.Contains(t, active.Header().Get("Location"), "notice=A+re-embed+is+already+waiting+or+running.")

	launcher.err = errors.New("database is down")
	failed := browserRequest(t, mux, http.MethodPost,
		"https://diderot.test/admin/operations/reembed", form, csrfToken)
	require.Equal(t, http.StatusSeeOther, failed.Code)
	assert.Contains(t, failed.Header().Get("Location"), "error=The+re-embed+was+not+requested%3A+database+is+down")
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT status FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestJobsPageListsJobsAndPages(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_operations_jobs"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_jobs, slack_channel_info RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name) VALUES ('CPLATFORM', 'platform')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_jobs (kind, status, requested_by, progress, requested_at, started_at, heartbeat_at,
		                        stale_after_seconds, finished_at, error)
		SELECT 'eval_retrieval', 'succeeded', 'op@example.com', '{}', now() - n * interval '1 hour',
		       now() - n * interval '1 hour', now(), 30, now() - n * interval '1 hour' + interval '90 seconds', NULL
		  FROM generate_series(1, 22) n`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_jobs (kind, status, requested_by, progress, started_at, heartbeat_at, stale_after_seconds)
		VALUES ('slack_image_replay', 'running', 'op@example.com',
		        '{"channel_id":"CPLATFORM","scanned":3,"reconciled":2}', now() - interval '5 minutes',
		        now() - interval '2 minutes', 30)`)
	require.NoError(t, err)

	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), summaryControl: fakeSummaries{}}
	w := httptest.NewRecorder()
	s.handleOperationsJobs(w, httptest.NewRequest(http.MethodGet, "/admin/operations/jobs", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "#platform · 3 messages read · 2 rebuilt")
	assert.Contains(t, body, `<span class="badge badge-bad">stalled</span>`, "its heartbeat is two minutes old")
	assert.NotContains(t, body, "data-jobs-active", "a stalled job isn't refreshed for")
	assert.Contains(t, body, "20 jobs, newest first")
	assert.Contains(t, body, `href="?before=`)

	jobs, more, err := s.loadJobs(ctx, nil)
	require.NoError(t, err)
	require.True(t, more)
	last := jobs[len(jobs)-1]
	older, more, err := s.loadJobs(ctx, &auditCursor{Created: last.RequestedAt, ID: last.ID})
	require.NoError(t, err)
	assert.False(t, more)
	assert.Len(t, older, 3)
	assert.Equal(t, 90*time.Second, older[0].Took)
}

func TestOperationsSessionSummarizationBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_session_summarization"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, session_ingest_control RESTART IDENTITY`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO session_ingest_control (id, summarization_enabled) VALUES (true, false)`)
	require.NoError(t, err)
	// The control shows once agent sessions reach the install: one that
	// already has its summary.
	_, err = pool.Exec(ctx, `TRUNCATE session_ingest_candidates CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, published_revision, complete)
		VALUES ('operator@example.com', 'operator@example.com', 'claude-code', 'summarized-session', 'rev-1', 'rev-1', true)`)
	require.NoError(t, err)
	control := sessioningest.NewSummaryControl(sessioningest.NewStore(pool), nil)
	server, err := New(Config{Authenticator: testBrowserAuth(), Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test", SessionSummaryControl: control})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "New sessions wait, unsearchable, until you resume.")
	require.Contains(t, page.Body.String(), `value="true">Resume summaries</button>`)
	var csrfToken string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.NotEmpty(t, csrfToken)

	badCSRF := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/operations/session-summarization", url.Values{"csrf_token": {"wrong"}, "enabled": {"true"}}, csrfToken)
	require.Equal(t, http.StatusForbidden, badCSRF.Code)

	enabled := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/operations/session-summarization", url.Values{"csrf_token": {csrfToken}, "enabled": {"true"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, enabled.Code)
	assert.Equal(t, "/admin/operations/jobs?notice=Session+summaries+resumed.", enabled.Header().Get("Location"))
	state, err := control.State(ctx)
	require.NoError(t, err)
	require.True(t, state.Enabled)
	var action, status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT action, status FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &status))
	require.Equal(t, "operations.session_summarization.set", action)
	require.Equal(t, "succeeded", status)

	page = browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "<p>Every finished session is summarized.</p>")
	require.Contains(t, page.Body.String(), `value="false">Pause summaries</button>`)
}

// TestJobsSaysWhySessionSummariesDontRun: while no model can write
// summaries, the page says so and offers no switch that can't apply.
func TestJobsSaysWhySessionSummariesDontRun(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_session_summarization_reason"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `UPDATE session_ingest_control SET summarization_enabled = true`)
	require.NoError(t, err)
	// A session waiting for its summary.
	_, err = pool.Exec(ctx, `TRUNCATE session_ingest_candidates CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, complete, first_pending_at)
		VALUES ('operator@example.com', 'operator@example.com', 'claude-code', 'waiting-session', 'rev-1', true, now())`)
	require.NoError(t, err)
	for _, tc := range []struct {
		control sessioningest.SummaryControl
		want    string
	}{
		{sessioningest.NewSummaryControl(sessioningest.NewStore(pool), func() string { return "No model can write session summaries: set ANTHROPIC_API_KEY." }),
			"No model can write session summaries: set ANTHROPIC_API_KEY."},
	} {
		server, err := New(Config{Authenticator: testBrowserAuth(), Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test", SessionSummaryControl: tc.control})
		require.NoError(t, err)
		mux := http.NewServeMux()
		require.NoError(t, server.Mount(mux))
		page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/jobs", nil, "")
		require.Equal(t, http.StatusOK, page.Code)
		assert.Contains(t, page.Body.String(), "<p>"+tc.want+"</p>")
		assert.Contains(t, page.Body.String(), "1 session is stored, and not searchable until summaries run.")
		assert.NotContains(t, page.Body.String(), "Pause summaries")
		assert.NotContains(t, page.Body.String(), "Resume summaries")

		status := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations", nil, "")
		require.Equal(t, http.StatusOK, status.Code)
		assert.Contains(t, status.Body.String(), tc.want, "the Status row says why too")
		assert.Contains(t, status.Body.String(), "1 agent session waiting, not searchable until summaries run.")
	}
}

// A queue has stalled when an item has waited in it, untried, with nothing
// finishing. The wait is the item's own: how old its event is says nothing,
// and an item that is failing and being retried is being worked on.
func TestStalledMeasuresAnItemsOwnWaitInItsQueue(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_operations"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	reset := func() int64 {
		t.Helper()
		_, err := pool.Exec(ctx, `TRUNCATE events, admin_jobs, slack_channel_sync, slack_channel_config, notion_sync_runs,
			session_ingest_candidates, scheduled_reports RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
		// A page Diderot has had for months.
		var eventID int64
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, ingested_at, body, raw)
			VALUES ('notion', 'page-1', now() - interval '3 years', now() - interval '4 months', 'an old page', '{}')
			RETURNING id`).Scan(&eventID))
		return eventID
	}
	// queue puts one item of each kind of work on the old page: queued so
	// long ago, having failed so many attempts.
	queue := func(eventID int64, queued string, failures int) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO event_chunks (event_id, chunk_idx, content, occurred_at, failure_count, queued_at)
			VALUES ($1, 0, 'text', now() - interval '3 years', $2, now() - $3::interval)`, eventID, failures, queued)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, queued_at)
			VALUES ($1, 'F1', 'image/png', now() - interval '3 years', $2, now() - $3::interval)`, eventID, failures, queued)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO event_documents (event_id, source_file_id, occurred_at, filename, media_type, failure_count, queued_at)
			VALUES ($1, 'D1', now() - interval '3 years', 'notes.docx', 'application/msword', $2, now() - $3::interval)`, eventID, failures, queued)
		require.NoError(t, err)
	}
	states := func() (map[string]string, string) {
		t.Helper()
		h, err := statusServer(pool, fakeSummaries{}).loadHealth(ctx)
		require.NoError(t, err)
		got := map[string]string{}
		for _, row := range h.Work {
			if row.ID == "index" || row.ID == "captions" || row.ID == "documents" {
				got[row.ID] = row.State
			}
		}
		var attention []string
		for _, item := range h.Attention {
			if strings.Contains(item.Text, "stalled") {
				attention = append(attention, item.Text)
			}
		}
		return got, strings.Join(attention, " ")
	}
	working := map[string]string{"index": "working", "captions": "working", "documents": "working"}

	queue(reset(), "5 seconds", 0)
	got, attention := states()
	assert.Equal(t, working, got, "something added to an old page a moment ago has waited a moment")
	assert.Empty(t, attention)

	queue(reset(), "14 minutes", 0)
	got, attention = states()
	assert.Equal(t, working, got, "a wait within what the page allows")
	assert.Empty(t, attention)

	queue(reset(), "2 hours", 2)
	got, attention = states()
	assert.Equal(t, working, got, "an item that keeps failing is being tried, and is listed with the failures")
	assert.Empty(t, attention)

	queue(reset(), "2 hours", 0)
	got, attention = states()
	assert.Equal(t, map[string]string{"index": "needs attention", "captions": "needs attention", "documents": "needs attention"},
		got, "two hours in the queue without an attempt")
	assert.Contains(t, attention, "Search index has stalled")
	assert.Contains(t, attention, "Image captions has stalled")
	assert.Contains(t, attention, "Document reading has stalled")

	// Put back by an administrator's retry, items that gave up wait from then.
	queue(reset(), "2 hours", 5)
	tables := map[string]string{
		failureKindIndex: "event_chunks", failureKindCaption: "event_images", failureKindDocument: "event_documents",
	}
	for kind, table := range tables {
		_, err := pool.Exec(ctx, `UPDATE `+table+` SET last_failure = 'caption F1: llmclient: status 503: overloaded'`)
		require.NoError(t, err)
		retried, err := statusServer(pool, fakeSummaries{}).failureStore().RetryFailures(ctx, kind, "provider-error")
		require.NoError(t, err)
		require.Equal(t, int64(1), retried, kind)
	}
	got, attention = states()
	assert.Equal(t, working, got, "a retried item has waited since the retry")
	assert.Empty(t, attention)
}
