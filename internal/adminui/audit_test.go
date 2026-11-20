package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 8, 28, 10, 2, 3, 456, time.FixedZone("test", -7*60*60))
	cursor, ok := parseAuditCursor(formatAuditCursor(created, 42))
	require.True(t, ok)
	assert.True(t, created.Equal(cursor.Created))
	assert.Equal(t, int64(42), cursor.ID)

	for _, raw := range []string{"", "2026-08-28T10:02:03Z", "bad|42", "2026-08-28T10:02:03Z|0", "2026-08-28T10:02:03Z|nope"} {
		_, ok = parseAuditCursor(raw)
		assert.False(t, ok, raw)
	}
}

func TestPrettyAuditDetails(t *testing.T) {
	assert.Empty(t, prettyAuditDetails([]byte(`{}`)))
	assert.Equal(t, "{\n  \"enabled\": true\n}", prettyAuditDetails([]byte(`{"enabled":true}`)))
	assert.Equal(t, "not json", prettyAuditDetails([]byte(`not json`)))
}

func TestBuildAuditQueryUsesIndexablePredicates(t *testing.T) {
	created := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	statement, args := buildAuditQuery(auditData{
		Actor: "operator@example.com", Action: "reports.delete", Target: "report:42",
		Status: "failed", RequestID: "request-42",
	}, &created, &auditCursor{Created: created.Add(-time.Minute), ID: 42})

	assert.NotContains(t, statement, " OR ")
	assert.Contains(t, statement, "actor_email = $1")
	assert.Contains(t, statement, "action = $2")
	assert.Contains(t, statement, "target = $3")
	assert.Contains(t, statement, "status = $4")
	assert.Contains(t, statement, "request_id = $5")
	assert.Contains(t, statement, "created_at >= $6")
	assert.Contains(t, statement, "(created_at, id) < ($7, $8)")
	assert.Contains(t, statement, "LIMIT $9")
	assert.Len(t, args, 9)

	statement, args = buildAuditQuery(auditData{}, nil, nil)
	assert.NotContains(t, statement, "WHERE")
	assert.Contains(t, statement, "LIMIT $1")
	assert.Equal(t, []any{auditPageSize + 1}, args)
}

func TestAuditTemplateEscapesDetailsAndPreservesCursor(t *testing.T) {
	data := auditData{
		pageData: pageData{Title: "Audit log"},
		Range:    "all",
		Records: []auditRecord{{
			ID: 7, Action: "sources.slack.config", ActionLabel: "Changed a Slack channel's settings",
			Target: "<target>", Actor: "operator@example.com",
			Status: "failed", Result: "failed", Tone: toneBad, Details: `{"value":"<script>alert(1)</script>"}`,
			Error: "failed <unsafe>", CreatedAt: time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC),
		}},
		Truncated: true,
		NextURL:   "?before=2026-08-28T10%3A00%3A00Z%7C7",
	}
	var out bytes.Buffer
	require.NoError(t, auditPage.ExecuteTemplate(&out, "layout", data))
	html := out.String()
	assert.Contains(t, html, "&lt;script&gt;alert(1)&lt;/script&gt;")
	assert.NotContains(t, html, "<script>alert(1)</script>")
	assert.Contains(t, html, "failed &lt;unsafe&gt;")
	assert.Contains(t, strings.ToLower(html), "%7c7")
	assert.NotContains(t, strings.ToLower(html), "%257c7")
	assert.Contains(t, html, `<time datetime="2026-08-28T10:00:00Z" title="2026-08-28 10:00:00 UTC"`)
	assert.Contains(t, html, "Changed a Slack channel&#39;s settings")
	assert.Contains(t, html, `<span class="badge badge-bad">failed</span>`)
	for _, word := range []string{"ledger", "mutation", "durable"} {
		assert.NotContains(t, strings.ToLower(html), word)
	}
}

func TestAuditActionLabel(t *testing.T) {
	s := &Server{}
	assert.Equal(t, "Went live", s.auditActionLabel("setup.go_live"))
	assert.Equal(t, "Turned on Notion", s.auditActionLabel("connectors.notion.enable"))
	assert.Equal(t, "Saved Slack settings", s.auditActionLabel("connectors.slack.save"))
	assert.Equal(t, "Started re-reading Zendesk", s.auditActionLabel("sources.zendesk.backfill"), "a source's re-read, by its title")
	assert.Equal(t, "Started re-reading a Slack channel's history or documents", s.auditActionLabel("sources.slack.backfill"))
	assert.Equal(t, "Made a version of the prompts live", s.auditActionLabel("prompts.activate"))
	assert.Equal(t, "Some new thing done", s.auditActionLabel("some.new_thing.done"), "an action with no words yet still reads")
	for key, label := range auditActionLabels {
		for _, word := range []string{"walk", "root", "release", "blacklist", "ingest", "Queued image captions"} {
			assert.NotContains(t, label, word, key)
		}
	}
}

func TestAuditNamesSlackChannelsAndReports(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_audit_names"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name) VALUES ('CAUDITNAME', 'platform')
		ON CONFLICT (channel_id) DO UPDATE SET name = EXCLUDED.name`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_audit_log (actor_email, action, target, status, details)
		VALUES ('operator@example.com', 'sources.slack.config.ask', 'CAUDITNAME', 'succeeded', '{}'),
		       ('operator@example.com', 'setup.go_live', 'CAUDITNAME', 'started', '{}')`)
	require.NoError(t, err)

	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	data, err := s.loadAudit(ctx, httptest.NewRequest(http.MethodGet, auditPath, nil))
	require.NoError(t, err)
	require.Len(t, data.Records, 2)
	byAction := map[string]auditRecord{}
	for _, record := range data.Records {
		byAction[record.Action] = record
	}
	assert.Equal(t, "#platform", byAction["sources.slack.config.ask"].TargetName)
	assert.Equal(t, "ok", byAction["sources.slack.config.ask"].Result)
	assert.Empty(t, byAction["setup.go_live"].TargetName, "only a Slack action's target is a channel")
	assert.Equal(t, "started", byAction["setup.go_live"].Result)
	assert.Empty(t, byAction["setup.go_live"].Duration)
	assert.Equal(t, []auditAction{
		{Key: "sources.slack.config.ask", Label: "Turned Ask on or off in a Slack channel"},
		{Key: "setup.go_live", Label: "Went live"},
	}, data.Actions)
}

func TestAuditLedgerFiltersAndPaginates(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_audit"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)

	created := time.Now().UTC().Truncate(time.Second)
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_audit_log
		       (created_at, finished_at, actor_email, action, target, request_id, status, details, error)
		SELECT $1::timestamptz, $1::timestamptz + interval '1 second',
		       CASE WHEN n = 51 THEN 'other@example.com' ELSE 'operator@example.com' END,
		       CASE WHEN n = 51 THEN 'reports.delete' ELSE 'sources.slack.config.ask' END,
		       'channel:' || n, 'request-' || n,
		       CASE WHEN n = 50 THEN 'failed' ELSE 'succeeded' END,
		       jsonb_build_object('sequence', n),
		       CASE WHEN n = 50 THEN 'fixture failure' ELSE NULL END
		  FROM generate_series(1, 51) n`, created)
	require.NoError(t, err)

	s := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	first, err := s.loadAudit(ctx, httptest.NewRequest(http.MethodGet, "/admin/operations/audit", nil))
	require.NoError(t, err)
	require.Len(t, first.Records, auditPageSize)
	assert.True(t, first.Truncated)
	require.NotEmpty(t, first.NextURL)

	second, err := s.loadAudit(ctx, httptest.NewRequest(http.MethodGet, "/admin/operations/audit"+first.NextURL, nil))
	require.NoError(t, err)
	require.Len(t, second.Records, 1)
	assert.False(t, second.Truncated)
	seen := make(map[int64]struct{}, len(first.Records))
	for _, record := range first.Records {
		seen[record.ID] = struct{}{}
	}
	_, duplicated := seen[second.Records[0].ID]
	assert.False(t, duplicated)

	filteredURL := "/admin/operations/audit?actor=OPERATOR%40EXAMPLE.COM&action=sources.slack.config.ask&status=failed&target=channel%3A50&request_id=request-50"
	filtered, err := s.loadAudit(ctx, httptest.NewRequest(http.MethodGet, filteredURL, nil))
	require.NoError(t, err)
	require.Len(t, filtered.Records, 1)
	assert.Equal(t, "fixture failure", filtered.Records[0].Error)
	assert.Contains(t, filtered.Records[0].Details, `"sequence": 50`)

	invalid, err := s.loadAudit(ctx, httptest.NewRequest(http.MethodGet, "/admin/operations/audit?status=wat&before=bad", nil))
	require.NoError(t, err)
	assert.Contains(t, invalid.FilterError, "Result must be")
	assert.Contains(t, invalid.FilterError, "page link is not valid")
}

func TestAuditRouteIsAdministratorGated(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_audit_route"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)

	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))

	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/operations/audit", nil))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	request := httptest.NewRequest(http.MethodGet, "https://diderot.test/admin/operations/audit?actor="+url.QueryEscape("operator@example.com"), nil)
	request.Header.Set(proxyEmailHeader, "operator@example.com")
	request.Header.Set(proxyGroupsHeader, "admins")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "<h1>Audit log</h1>")
	assert.Contains(t, response.Body.String(), `<a href="/admin/operations/audit" aria-current="page">Audit log</a>`)
	assert.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
}
