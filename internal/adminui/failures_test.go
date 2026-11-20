package adminui

import (
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/testdb"
)

const (
	rateLimited = `caption F0: gateway captioner: request: llmclient: status 429: {"error":{"message":"Rate limit reached"}}`
	badImage    = `caption F0: gateway captioner: request: llmclient: status 400: {"error":{"message":"Could not process image"}}`
)

// seedFailures stores failing work: images that gave up for a rate limit
// and for a bad file, one still being retried, a document with no text,
// and a chunk of a deleted event.
func seedFailures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `TRUNCATE events, slack_channel_info, admin_audit_log RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name) VALUES ('CPLATFORM', 'platform')`)
	require.NoError(t, err)
	var eventID, deletedID int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw, url)
		VALUES ('slack', 'CPLATFORM:1.0', now() - interval '1 hour', 'the dashboard after the deploy', '{}',
		        'https://example.slack.com/archives/CPLATFORM/p10') RETURNING id`).Scan(&eventID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw, deleted)
		VALUES ('slack', 'CPLATFORM:2.0', now(), 'gone', '{}', true) RETURNING id`).Scan(&deletedID))
	for i := range 30 {
		_, err := pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, last_failure, last_failed_at, claimed_at)
			VALUES ($1, $2, 'image/png', now() - $3 * interval '1 minute', 5, $4, now() - $3 * interval '1 second', now())`,
			eventID, fmt.Sprintf("FRATE%02d", i), i, rateLimited)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, last_failure)
		VALUES ($1, 'FBAD', 'image/png', now() - interval '2 hours', 5, $2),
		       ($1, 'FSOON', 'image/png', now() - interval '3 hours', 2, $2)`, eventID, badImage)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO event_documents (event_id, source_file_id, occurred_at, filename, media_type, failure_count, last_failure)
		VALUES ($1, 'DEMPTY', now() - interval '4 hours', 'blank.docx', 'application/msword', 5, 'extract DEMPTY: permanent: docs: empty document')`, eventID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO event_chunks (event_id, chunk_idx, content, occurred_at, failure_count, last_failure)
		VALUES ($1, 0, 'gone', now(), 5, 'must stay hidden')`, deletedID)
	require.NoError(t, err)
}

func TestFailuresAreGroupedByCause(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seedFailures(t, pool)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	items, truncated, err := s.failureStore().load(ctx, false)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, items, 33, "the deleted event's chunk is no one's concern")
	assert.Equal(t, "the dashboard after the deploy", items[0].Label)
	assert.Equal(t, "#platform", items[0].Where)
	assert.Equal(t, "https://example.slack.com/archives/CPLATFORM/p10", items[0].URL)
	assert.Equal(t, "blank.docx", items[32].Label)

	groups := groupFailures(items)
	require.Len(t, groups, 3)
	assert.Equal(t, "rate-limited", groups[0].Cause.Key, "what can be retried comes first")
	assert.Equal(t, 30, groups[0].GaveUp)
	assert.True(t, groups[0].CanRetry())
	assert.Equal(t, "malformed", groups[1].Cause.Key)
	assert.Equal(t, [2]int{1, 1}, [2]int{groups[1].GaveUp, groups[1].Retrying})
	assert.False(t, groups[1].CanRetry(), "a bad file fails the same way every time")
	assert.Equal(t, failureKindDocument, groups[2].Kind)

	w := httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures?kind=caption&cause=rate-limited&page=2", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Image captions: The model provider limited the request rate")
	assert.Contains(t, body, "26–30 of 30 items, most recently failed first")
	assert.Contains(t, body, `href="?cause=rate-limited&amp;kind=caption#items"`, "the newer page keeps the filter")
	assert.Equal(t, 1, strings.Count(body, ">Retry 30<"), "only the rate limit can be retried")
	assert.NotContains(t, body, "must stay hidden")

	w = httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures?kind=caption&cause=gone", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Nothing fails for that cause any more")
	assert.Contains(t, w.Body.String(), "Every failing item")
}

func TestFailuresOfAnUnknownCauseAreNamedPlainlyAndKeptApart(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seedFailures(t, pool)
	const (
		unparsed = "caption F0: gateway captioner: parse response: invalid character '<' looking for beginning of value"
		internal = "Vertex caption: vertex returned a part of kind 7"
		codec    = `caption F0: gateway captioner: request: llmclient: status 400: {"error":{"message":"The provided file uses a format or codec that is not supported."}}`
	)
	_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, last_failure)
		SELECT e.id, v.id, 'image/png', now() - interval '5 hours', 5, v.failure
		  FROM events e, (VALUES ('FOTHER1', $1), ('FOTHER2', $1), ('FOTHER3', $2), ('FCODEC', $3)) AS v (id, failure)
		 WHERE NOT e.deleted`, unparsed, internal, codec)
	require.NoError(t, err)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	items, _, err := s.failureStore().load(ctx, false)
	require.NoError(t, err)
	byCause := map[string]failureGroup{}
	for _, group := range groupFailures(items) {
		if group.Kind == failureKindCaption {
			byCause[group.Cause.Key] = group
		}
	}
	first, second := byCause[classifyFailure(unparsed).Key], byCause[classifyFailure(internal).Key]
	assert.Equal(t, [2]int{2, 1}, [2]int{first.GaveUp, second.GaveUp}, "unrelated unknown errors are retried apart")
	assert.True(t, first.CanRetry())
	assert.False(t, byCause["unsupported"].CanRetry(), "a format the model can't read fails the same way every time")
	assert.Equal(t, 1, byCause["unsupported"].GaveUp)

	w := httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures?kind=caption&cause="+second.Cause.Key, nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Image captions: Another error")
	assert.Equal(t, 2, strings.Count(body, "#items\">Another error</a>"), "each unknown cause is its own group")
	assert.Equal(t, 1, strings.Count(body, "Vertex caption"), "what the failure said is shown once, with its item")
	assert.Contains(t, body, `<pre aria-label="The error as recorded">Vertex caption: vertex returned a part of kind 7</pre>`)
	assert.NotContains(t, body, "format or codec", "the item list is narrowed to the cause")
}

// An item failing today can be years old at its source. The page orders and
// dates failures by when the work last failed, and keeps the item's own date
// beside it, labelled as the item's.
func TestFailuresSayWhenEachItemLastFailed(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events, slack_channel_info, admin_audit_log RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	var oldPage, newPage int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('notion', 'page-1', '2023-03-21 15:48:00+00', 'an old page', '{}') RETURNING id`).Scan(&oldPage))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('notion', 'page-2', now() - interval '1 day', 'a new page', '{}') RETURNING id`).Scan(&newPage))
	// The old page's image failed two hours ago; the new page's failed three
	// days ago, though the page itself is newer.
	_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, last_failure, last_failed_at)
		VALUES ($1, 'FOLD', 'image/png', '2023-03-21 15:48:00+00', 5, $3, date_trunc('second', now() - interval '2 hours')),
		       ($2, 'FNEW', 'image/png', now() - interval '1 day', 5, $3, now() - interval '3 days')`, oldPage, newPage, badImage)
	require.NoError(t, err)
	var failedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT last_failed_at FROM event_images WHERE source_image_id = 'FOLD'`).Scan(&failedAt))
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	items, _, err := s.failureStore().load(ctx, false)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, []string{"an old page", "a new page"}, []string{items[0].Label, items[1].Label},
		"the most recently failed comes first, whatever the items' own dates")
	groups := groupFailures(items)
	require.Len(t, groups, 1)
	require.NotNil(t, groups[0].LastFailedAt)
	assert.True(t, groups[0].LastFailedAt.Equal(failedAt), "a cause last failed when its latest item did")

	w := httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	failed := failedAt.UTC().Format(time.RFC3339)
	assert.Contains(t, body, `last failed <time datetime="`+failed+`"`, "the cause says when it last failed")
	assert.Contains(t, body, ">2 hours ago</time>")
	assert.NotContains(t, body, "newest item dated", "a cause with a failure time is not dated by its items")
	assert.Contains(t, body, `<span class="sub">item dated <time datetime="2023-03-21T15:48:00Z"`,
		"the item's own date is still shown, labelled as the item's")
	assert.Contains(t, body, "2 items, most recently failed first")
}

// A failure recorded before failures were timed has no time. The page says so
// rather than passing off another date as one, and lists it after the timed
// ones.
func TestFailuresWithoutARecordedTimeShowNone(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events, slack_channel_info, admin_audit_log RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	var eventID int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO events (source, external_id, occurred_at, ingested_at, updated_at, body, raw)
		VALUES ('notion', 'page-1', '2023-03-21 15:48:00+00', '2026-07-09 10:00:00+00', '2026-08-20 19:39:00+00', 'an old page', '{}')
		RETURNING id`).Scan(&eventID))
	// The untimed image is the newer item, and was claimed by a worker once:
	// neither its date nor any other time in its row is a failure time.
	_, err = pool.Exec(ctx, `INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, failure_count, last_failure, last_failed_at, claimed_at)
		VALUES ($1, 'FUNTIMED', 'image/png', '2024-05-06 07:08:09+00', 5, $2, NULL, '2026-07-09 11:12:13+00'),
		       ($1, 'FTIMED', 'image/png', '2023-03-21 15:48:00+00', 5, $3, now() - interval '5 days', NULL)`,
		eventID, badImage, rateLimited)
	require.NoError(t, err)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	items, _, err := s.failureStore().load(ctx, false)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.NotNil(t, items[0].FailedAt, "a failure with a time comes before one without")
	assert.Nil(t, items[1].FailedAt)
	var untimed failureGroup
	for _, group := range groupFailures(items) {
		if group.Cause.Key == "malformed" {
			untimed = group
		}
	}
	assert.Nil(t, untimed.LastFailedAt, "a cause whose failures have no time has none either")

	w := httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures?kind=caption&cause=malformed", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	_, list, found := strings.Cut(body, `<ol aria-label="Failing items">`)
	require.True(t, found)
	assert.Contains(t, list, `<span class="muted">not recorded</span><span class="sub">item dated <time datetime="2024-05-06T07:08:09Z"`)
	assert.Contains(t, list, "Not recorded: it last failed before Diderot kept the time of a failure.")
	assert.Equal(t, 2, strings.Count(list, "<time "), "the only times shown for it are the item's own date, in its row and its details")
	assert.NotContains(t, list, "2026-07-09", "when the item arrived or was last tried is not when it failed")
	assert.NotContains(t, list, "2026-08-20")
	assert.Contains(t, body, `newest item dated <time datetime="2024-05-06T07:08:09Z"`, "its cause falls back to the items' own dates, said as such")
}

func TestRetryingAFailureCausePutsItsItemsBack(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures_retry"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seedFailures(t, pool)
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/operations/failures", nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	var csrfToken string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.NotEmpty(t, csrfToken)
	retry := func(kind, cause string) string {
		t.Helper()
		w := browserRequest(t, mux, http.MethodPost, "https://diderot.test/admin/operations/failures/retry",
			url.Values{"csrf_token": {csrfToken}, "kind": {kind}, "cause": {cause}}, csrfToken)
		require.Equal(t, http.StatusSeeOther, w.Code)
		location, err := url.Parse(w.Header().Get("Location"))
		require.NoError(t, err)
		return location.Query().Get("notice") + location.Query().Get("error")
	}

	_, err = pool.Exec(ctx, `UPDATE event_images SET queued_at = now() - interval '3 days'`)
	require.NoError(t, err)
	assert.Equal(t, "Retrying 30 items: they are back in the queue and are tried again within a few minutes.", retry("caption", "rate-limited"))
	var requeued int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_images WHERE queued_at > now() - interval '1 minute'`).Scan(&requeued))
	assert.Equal(t, 30, requeued, "a retried item waits from now; the others' waits are untouched")
	var reset, claimed int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE failure_count = 0 AND last_failure IS NULL AND last_failed_at IS NULL),
		count(*) FILTER (WHERE claimed_at IS NOT NULL) FROM event_images WHERE source_image_id LIKE 'FRATE%'`).Scan(&reset, &claimed))
	assert.Equal(t, 30, reset, "a retried item is no longer a failure: its error and when it failed go together")
	assert.Zero(t, claimed)
	var action, target, status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT action, target, status FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &target, &status))
	assert.Equal(t, [3]string{"operations.failures.retry", "caption", "succeeded"}, [3]string{action, target, status})

	assert.Equal(t, "Nothing was waiting to be retried for that cause.", retry("caption", "rate-limited"))
	assert.Equal(t, "Nothing was retried: The file is malformed or could not be decoded: retrying would fail the same way",
		retry("caption", "malformed"))
	var failures int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_images WHERE source_image_id = 'FBAD' AND failure_count = 5`).Scan(&failures))
	assert.Equal(t, 1, failures, "a bad file is left as it is")
	assert.Contains(t, retry("bogus", "x"), "unknown kind of work")
}

// seedSessionSummaries stores agent sessions in each state a summary can be
// in: parked after every attempt, parked at once, failing and still retried,
// waiting untried, and three that are no one's concern.
func seedSessionSummaries(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `TRUNCATE events, session_ingest_candidates, admin_audit_log, admin_jobs, slack_channel_sync,
		slack_channel_config, notion_sync_runs, scheduled_reports RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, complete, source_started_at, source_updated_at,
		        index_failure_revision, index_failure_count, index_last_error, index_last_failed_at,
		        index_retry_at, index_quarantined_at, excluded_at)
		VALUES
		  ('ada@example.com', 'ada@example.com', 'codex', 'ses-outage', 'r1', true, '2026-10-01 10:00+00', '2026-10-01 11:00+00',
		   'r1', 5, 'sessioningest: summarize ses-outage: sessioningest: reduce: converse: sessionsummary gateway: status 503: upstream call failed',
		   now() - interval '1 hour', NULL, now() - interval '1 hour', NULL),
		  ('ada@example.com', 'ada@example.com', 'claude_code', 'ses-empty', 'r1', true, '2026-10-02 10:00+00', '2026-10-02 10:00+00',
		   'r1', 1, 'revision r1 for ses-empty: session revision has no records',
		   now() - interval '2 days', NULL, now() - interval '2 days', NULL),
		  ('bo@example.com', 'bo@example.com', 'codex', 'ses-retrying', 'r1', true, '2026-10-03 10:00+00', '2026-10-03 12:00+00',
		   'r1', 2, 'sessioningest: summarize ses-retrying: sessioningest: reduce: reduce returned empty summary (narrative 0 chars)',
		   now() - interval '5 minutes', now() + interval '15 minutes', NULL, NULL),
		  ('bo@example.com', 'bo@example.com', 'codex', 'ses-waiting', 'r1', true, now(), now(),
		   '', 0, NULL, NULL, NULL, NULL, NULL),
		  ('bo@example.com', 'bo@example.com', 'codex', 'ses-changed', 'r2', true, now(), now(),
		   'r1', 5, 'an earlier revision failed', now() - interval '3 days', NULL, now() - interval '3 days', NULL),
		  ('bo@example.com', 'bo@example.com', 'codex', 'ses-excluded', 'r1', true, now(), now(),
		   'r1', 5, 'must stay hidden', now(), NULL, now(), now())`)
	require.NoError(t, err)
	// The feed gives every session it reports a time.
	_, err = pool.Exec(ctx, `UPDATE session_ingest_candidates SET updated_at = source_updated_at`)
	require.NoError(t, err)
}

// A session whose summary keeps failing is on the Failures page like an
// image or a document: with its cause, when it last failed, and whether it
// is still being tried.
func TestFailingSessionSummariesAreListedByCause(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seedSessionSummaries(t, pool)
	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		viewerBaseURL: func() string { return "https://sessions.example.test" }}

	items, _, err := s.failureStore().load(ctx, false)
	require.NoError(t, err)
	byRecord := map[string]failureItem{}
	var order []string
	for _, item := range items {
		require.Equal(t, failureKindSummary, item.Kind)
		byRecord[item.Record] = item
		order = append(order, item.Record)
	}
	assert.Equal(t, []string{"session ses-retrying", "session ses-outage", "session ses-empty"}, order,
		"the most recently failed first; a session that changed, was excluded, or never failed is not failing")

	outage := byRecord["session ses-outage"]
	assert.Equal(t, "Session summaries", outage.KindName)
	assert.Equal(t, "codex session", outage.Label)
	assert.Equal(t, "ada@example.com", outage.Where)
	assert.Equal(t, "https://sessions.example.test/sessions/ses-outage", outage.URL)
	assert.Equal(t, "provider-error", outage.Cause.Key)
	assert.True(t, outage.GaveUp, "every attempt failed")
	assert.Equal(t, 5, outage.Attempts)
	require.NotNil(t, outage.FailedAt)
	assert.WithinDuration(t, time.Now().Add(-time.Hour), *outage.FailedAt, time.Minute)
	assert.Equal(t, time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC), outage.ItemAt.UTC(), "the item's date is the session's own")

	empty := byRecord["session ses-empty"]
	assert.Equal(t, "session-empty", empty.Cause.Key)
	assert.True(t, empty.GaveUp, "parked at the first failure: no retry can get past it")
	retrying := byRecord["session ses-retrying"]
	assert.Equal(t, "no-output", retrying.Cause.Key)
	assert.False(t, retrying.GaveUp, "it has attempts left")

	gaveUp, _, err := s.failureStore().load(ctx, true, failureKindSummary)
	require.NoError(t, err)
	assert.Len(t, gaveUp, 2, "only the parked ones gave up")

	w := httptest.NewRecorder()
	s.handleFailures(w, httptest.NewRequest(http.MethodGet, "/admin/operations/failures", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Session summaries · 1 no longer retried · last failed")
	assert.Contains(t, body, "Session summaries · 1 still being retried · last failed")
	assert.Contains(t, body, "The session has nothing in it to summarize")
	assert.Equal(t, 1, strings.Count(body, ">Retry 1<"), "only the outage can be retried")
	assert.Contains(t, body, `<span class="id">session ses-outage</span>`)
	assert.NotContains(t, body, "must stay hidden")
}

// Retry puts a parked session back for the session worker, and a parked
// session is not counted as waiting on any page.
func TestParkedSessionSummariesAreRetriedAndNotCountedAsWaiting(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_failures"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seedSessionSummaries(t, pool)
	s := statusServer(pool, fakeSummaries{ModelAvailable: true, EffectiveEnabled: true})

	summaries := func() (workRow, []string) {
		t.Helper()
		h, err := s.loadHealth(ctx)
		require.NoError(t, err)
		var row workRow
		for _, work := range h.Work {
			if work.ID == "summaries" {
				row = work
			}
		}
		var attention []string
		for _, item := range h.Attention {
			attention = append(attention, item.Text)
		}
		return row, attention
	}
	row, attention := summaries()
	assert.Equal(t, "3 agent sessions waiting for a summary.", row.Detail,
		"the one being retried, the untried one and the one that changed wait; the two parked ones do not")
	assert.Equal(t, "2 agent sessions could not be summarized, and are listed under Failures.", row.Note)
	assert.Contains(t, attention, "1 item stopped after 5 attempts for a reason that retrying may fix.")

	_, err = s.failureStore().RetryFailures(ctx, failureKindSummary, "session-empty")
	require.ErrorContains(t, err, "The session has nothing in it to summarize: retrying would fail the same way")
	retried, err := s.failureStore().RetryFailures(ctx, failureKindSummary, "provider-error")
	require.NoError(t, err)
	assert.Equal(t, int64(1), retried)

	var parked bool
	var failures int
	var lastError *string
	var failedAt, retryAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT index_quarantined_at IS NOT NULL, index_failure_count, index_last_error,
		index_last_failed_at, index_retry_at FROM session_ingest_candidates WHERE session_id = 'ses-outage'`).Scan(
		&parked, &failures, &lastError, &failedAt, &retryAt))
	assert.False(t, parked)
	assert.Zero(t, failures, "its attempts start again from none")
	assert.Nil(t, lastError)
	assert.Nil(t, failedAt)
	assert.Nil(t, retryAt, "nothing holds it back from the next pass")
	pending, err := sessioningest.NewStore(pool).FindPendingCandidates(ctx, 10)
	require.NoError(t, err)
	var selectable []string
	for _, candidate := range pending {
		selectable = append(selectable, candidate.SessionID)
	}
	assert.Contains(t, selectable, "ses-outage", "the session worker takes it on its next pass")
	assert.NotContains(t, selectable, "ses-empty", "a parked session is left alone")

	row, attention = summaries()
	assert.Equal(t, "4 agent sessions waiting for a summary.", row.Detail)
	assert.Equal(t, "1 agent session could not be summarized, and is listed under Failures.", row.Note)
	assert.NotContains(t, attention, "1 item stopped after 5 attempts for a reason that retrying may fix.")

	retried, err = s.failureStore().RetryFailures(ctx, failureKindSummary, "provider-error")
	require.NoError(t, err)
	assert.Zero(t, retried, "nothing is parked for that cause any more")

	// With nothing waiting, a parked session still means not every session
	// is searchable, and the row must not say they all are.
	on := sessioningest.SummaryControlState{ModelAvailable: true, EffectiveEnabled: true}
	drained := summaryRow(summaryState{SummaryControlState: on, Seen: 3, GaveUp: 1})
	assert.Equal(t, "No agent session is waiting for a summary.", drained.Detail)
	assert.Equal(t, "1 agent session could not be summarized, and is listed under Failures.", drained.Note)
	assert.Equal(t, "Every finished agent session is summarized and searchable.",
		summaryRow(summaryState{SummaryControlState: on, Seen: 3}).Detail)
}
