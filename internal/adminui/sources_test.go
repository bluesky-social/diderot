package adminui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/adminaction"
	linearcx "github.com/bluesky-social/diderot/internal/connector/linear"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSourceControl struct {
	configField        string
	enabled            bool
	actor              string
	blacklist          bool
	reason             string
	joined             bool
	backfills          int
	documentBackfills  int
	imageReplayChannel string
	imageReplayActor   string
	documentRebuilds   int
	documentErr        error
	// resolved are the channels ResolveSlackChannel knows, by name or ID;
	// resolveErr fails it.
	resolved   map[string][2]string
	resolveErr error
	invited    []string
	noToken    bool
	off        bool
}

func (f *recordingSourceControl) SlackOn() bool           { return !f.off }
func (f *recordingSourceControl) SlackUserTokenSet() bool { return !f.noToken }
func (f *recordingSourceControl) StartSlackDocumentRebuild(context.Context, string) (int64, error) {
	if f.documentErr != nil {
		return 0, f.documentErr
	}
	f.documentRebuilds++
	return 78, nil
}
func (f *recordingSourceControl) ResolveSlackChannel(_ context.Context, key string) (string, string, error) {
	if f.resolveErr != nil {
		return "", "", f.resolveErr
	}
	if found, ok := f.resolved[key]; ok {
		return found[0], found[1], nil
	}
	return "", "", slackcx.ErrChannelNotFound
}
func (f *recordingSourceControl) SetSlackChannelConfig(_ context.Context, _, field string, enabled bool, actor string) error {
	f.configField, f.enabled, f.actor = field, enabled, actor
	return nil
}
func (f *recordingSourceControl) AddSlackBlacklist(_ context.Context, _, _, reason, _ string) (slackcx.BlacklistEntry, error) {
	f.blacklist, f.reason = true, reason
	return slackcx.BlacklistEntry{}, nil
}
func (f *recordingSourceControl) RemoveSlackBlacklist(context.Context, string) (bool, error) {
	f.blacklist = false
	return true, nil
}
func (f *recordingSourceControl) SetSlackMembership(_ context.Context, id string, joined bool) (bool, error) {
	f.joined = joined
	if joined {
		f.invited = append(f.invited, id)
	}
	return false, nil
}
func (f *recordingSourceControl) RequestSlackBackfill(context.Context, string) (bool, error) {
	f.backfills++
	return false, nil
}
func (f *recordingSourceControl) RequestSlackDocumentBackfill(context.Context, string, string) error {
	f.documentBackfills++
	return nil
}
func (f *recordingSourceControl) StartSlackImageReplay(_ context.Context, actor, channel string) (int64, error) {
	f.imageReplayActor, f.imageReplayChannel = actor, channel
	return 77, nil
}

// everyConnector serves an enabled view of any connector name, so each
// connector's page renders with its own sections.
type everyConnector struct{ fakeConnectors }

func (*everyConnector) View(_ context.Context, name string) (settings.View, error) {
	return settings.View{Name: name, Version: 1, Enabled: true}, nil
}

// TestSlackChannelPages: the channel list shows every channel Diderot
// knows in pages of 50, with a count for each view, and the views, search,
// and paging narrow it; each channel's page says where it stands and offers
// what applies to it. The Slack, Notion, and Linear pages show their own
// sections.
func TestSlackChannelPages(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_sources"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		TRUNCATE slack_ingest_blacklist, slack_channel_config, slack_channel_sync,
		         slack_channel_info, slack_users, ask_sessions, events, notion_roots,
		         notion_discovered_resources, notion_page_state, notion_sync_runs, admin_jobs
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
		VALUES ('CPLATFORM', 'platform', true, false, $1), ('CBLOCKED', 'blocked', false, false, $1),
		       ('CRANDOM', 'random', false, false, $1), ('CLEFT', 'left', false, false, $1),
		       ('CBROKEN', 'broken', false, false, $1), ('COLD', 'old', false, true, $1)`, now)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled, last_error)
		VALUES ('CPLATFORM', $1, true, NULL), ('CBLOCKED', NULL, false, 'blacklisted; ingest_enabled flipped'),
		       ('CLEFT', $1, false, NULL), ('CBROKEN', $1, true, 'not_in_channel'), ('COLD', $1, false, NULL)`, now)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_config
		       (channel_id, ask_enabled, doc_ingest_enabled, has_external_members,
		        allow_external_ask, external_members, updated_by, updated_at)
		VALUES ('CPLATFORM', true, true, true, false, ARRAY['UEXTERNAL', 'UNKNOWN'], 'operator@example.com', $1)`, now)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_users (user_id, real_name) VALUES ('UEXTERNAL', 'Pat Partner')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_ingest_blacklist (channel_id, channel_name, reason, added_by)
		VALUES ('CBLOCKED', 'blocked', 'sensitive source', 'operator@example.com')`)
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('slack', 'CPLATFORM:1.0', $1, 'platform update', '{}', 'public') RETURNING id`, now).Scan(&eventID))
	_, err = pool.Exec(ctx, `INSERT INTO slack_message_details (event_id, channel_id, occurred_at)
		VALUES ($1, 'CPLATFORM', $2)`, eventID, now)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('slack', 'CLEFT:1.0', $1, 'left behind', '{}', 'public') RETURNING id`, now).Scan(&eventID))
	_, err = pool.Exec(ctx, `INSERT INTO slack_message_details (event_id, channel_id, occurred_at)
		VALUES ($1, 'CLEFT', $2)`, eventID, now)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_jobs
		       (kind, status, requested_by, request, progress, started_at, finished_at, stale_after_seconds)
		VALUES ('slack_image_replay', 'succeeded', 'operator@example.com',
		        '{"channel_id":"CPLATFORM"}',
		        '{"phase":"complete","channel_id":"CPLATFORM","scanned":3,"reconciled":2,"done":true}',
		        now(), now(), 120)`)
	require.NoError(t, err)
	var sessionID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id::text`).Scan(&sessionID))
	_, err = pool.Exec(ctx, `INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id)
		VALUES ('CPLATFORM', '1.0', $1::uuid)`, sessionID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO notion_roots (object_type, object_id, label, added_by, last_walk_status)
		VALUES ('page', '11111111-1111-1111-1111-111111111111', 'Engineering', 'operator@example.com', 'complete')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO notion_discovered_resources (object_type, object_id, title, url)
		VALUES ('data_source', '22222222-2222-2222-2222-222222222222', 'Projects', 'https://notion.so/projects')`)
	require.NoError(t, err)

	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		NotionRootsStore:   notioncx.NewRootsStore(pool),
		Connectors:         &everyConnector{},
		SlackSourceControl: &recordingSourceControl{},
		Registries:         pageRegistries(),
		SourceDeps:         source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	get := func(path string) string {
		t.Helper()
		page := browserRequest(t, mux, http.MethodGet, "https://diderot.test"+path, nil, "")
		require.Equal(t, http.StatusOK, page.Code, page.Body.String())
		return page.Body.String()
	}
	channelsIn := func(body string) []string {
		var names []string
		for _, match := range regexp.MustCompile(`class="row-link" href="/admin/connectors/slack/channels/\w+">#([\w-]+)</a>`).FindAllStringSubmatch(body, -1) {
			names = append(names, match[1])
		}
		return names
	}

	list := get("/admin/connectors/slack/channels")
	assert.Equal(t, []string{"blocked", "broken", "left", "old", "platform", "random"}, channelsIn(list))
	for _, tab := range []string{
		`>All<span class="tab-count">6</span>`, `>Ask on<span class="tab-count">1</span>`,
		`>Bot not in channel<span class="tab-count">2</span>`, `>Can&#39;t be read<span class="tab-count">1</span>`,
		`>Blocked<span class="tab-count">1</span>`, `>Archived<span class="tab-count">1</span>`,
	} {
		assert.Contains(t, list, tab)
	}
	for _, state := range []string{"ok", "blocked", "not in channel", "error", "archived"} {
		assert.Contains(t, list, `">`+state+`</span>`, state)
	}
	assert.Contains(t, list, "not_in_channel", "a channel the bot can't read says why")
	assert.NotContains(t, list, "ingest_enabled", "what blocking did is no error of the channel's")
	assert.Equal(t, []string{"platform"}, channelsIn(get("/admin/connectors/slack/channels?filter=ask")),
		"the view the Access page links to")
	assert.Equal(t, []string{"left", "random"}, channelsIn(get("/admin/connectors/slack/channels?filter=not-member")))
	assert.Equal(t, []string{"broken"}, channelsIn(get("/admin/connectors/slack/channels?filter=errors")))
	assert.Equal(t, []string{"blocked"}, channelsIn(get("/admin/connectors/slack/channels?filter=blocked")))
	assert.Equal(t, []string{"platform"}, channelsIn(get("/admin/connectors/slack/channels?q=PLAT")))
	assert.Equal(t, []string{"platform"}, channelsIn(get("/admin/connectors/slack/channels?q=cplatform&filter=ask")))
	noMatch := get("/admin/connectors/slack/channels?q=nothing-like-it")
	assert.Empty(t, channelsIn(noMatch))
	assert.Contains(t, noMatch, "No channels match", "a search that finds nothing keeps the search")
	assert.Contains(t, noMatch, `name="q" value="nothing-like-it"`)
	assert.NotContains(t, noMatch, "No channels yet")
	assert.Len(t, channelsIn(get("/admin/connectors/slack/channels?filter=surprise")), 6, "an unknown view shows every channel")

	slack := get("/admin/connectors/slack")
	assert.Contains(t, slack, `href="/admin/connectors/slack/channels"`)
	assert.Contains(t, slack, `<span class="stat-label">The bot is in</span><span class="stat-value">2</span>`)
	assert.Contains(t, slack, "channels the bot is in can&#39;t be read.")
	assert.NotContains(t, slack, "#platform", "the channels have their own page")
	assert.Contains(t, slack, `<button class="button button-primary" type="submit">Save</button>`,
		"a connector with nothing to set can still be saved and tested")
	assert.Contains(t, slack, `formaction="/admin/connectors/slack/test">Test</button>`)

	detail := get("/admin/connectors/slack/channels/CPLATFORM")
	assert.Contains(t, detail, `<div class="page-title"><h1>#platform</h1><span class="badge badge-good">ok</span></div>`)
	assert.Contains(t, detail, `<p class="page-meta"><span>Private channel, shared with other organizations</span>`)
	assert.Contains(t, detail, "Pat Partner <span class=\"id\">UEXTERNAL</span>, <span class=\"id\">UNKNOWN</span>")
	assert.Contains(t, detail, `<span class="stat-label">Messages indexed</span><span class="stat-value">1</span>`)
	assert.Contains(t, detail, `<span class="stat-label">Ask conversations</span><span class="stat-value">1</span>`)
	assert.Contains(t, detail, "Messages checked: 3. Images rebuilt: 2.")
	assert.Contains(t, detail, ">Remove the bot</button>")
	assert.NotContains(t, detail, "Invite the bot", "the bot is in the channel")
	assert.Contains(t, detail, ">Read history again</button>")
	assert.Contains(t, detail, ">Block this channel</button>")
	assert.NotContains(t, detail, "data-reload-while-running")

	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET status='running', finished_at=NULL, heartbeat_at=now()
		WHERE kind='slack_image_replay'`)
	require.NoError(t, err)
	detail = get("/admin/connectors/slack/channels/CPLATFORM")
	assert.Contains(t, detail, "data-reload-while-running", "the page follows its own rebuild")
	assert.NotContains(t, detail, ">Rebuild descriptions</button>")
	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET request='{"channel_id":"CBLOCKED"}'
		WHERE kind='slack_image_replay'`)
	require.NoError(t, err)
	detail = get("/admin/connectors/slack/channels/CPLATFORM")
	assert.Contains(t, detail, "Another channel's image descriptions are being rebuilt.")
	assert.NotContains(t, detail, "data-reload-while-running")

	blocked := get("/admin/connectors/slack/channels/CBLOCKED")
	assert.Contains(t, blocked, "sensitive source")
	assert.Contains(t, blocked, `<span class="badge badge-neutral">blocked</span>`)
	assert.Contains(t, blocked, ">Unblock</button>")
	assert.NotContains(t, blocked, "Invite the bot")
	left := get("/admin/connectors/slack/channels/CLEFT")
	assert.Contains(t, left, `<span class="badge badge-neutral">not in channel</span>`)
	assert.Contains(t, left, "Once the bot is back in the channel, Diderot picks up where it stopped", "reading resumes by itself")
	assert.NotContains(t, left, "Start reading again")
	assert.Contains(t, left, ">Invite the bot</button>")
	assert.NotContains(t, left, "Read history again", "maintenance is for a channel Diderot reads")
	assert.Contains(t, left, "<h3>Rebuild image descriptions</h3>", "but its stored messages' images can be rebuilt")
	random := get("/admin/connectors/slack/channels/CRANDOM")
	assert.Contains(t, random, ">Invite the bot</button>")
	assert.NotContains(t, random, "picks up where it stopped", "it was never read")
	assert.NotContains(t, get("/admin/connectors/slack/channels/COLD"), "Invite the bot", "an archived channel can't be joined")

	for _, id := range []string{"not-valid", "CUNKNOWN"} {
		missing := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/connectors/slack/channels/"+id, nil, "")
		assert.Equal(t, http.StatusNotFound, missing.Code, id)
		assert.Contains(t, missing.Body.String(), "<h1>Channel not found</h1>", id)
		assert.Contains(t, missing.Body.String(), `<a class="button" href="/admin/connectors/slack/channels">All channels</a>`, id)
	}

	// The old Sources page's sections live on their connectors' pages.
	notion := get("/admin/connectors/notion")
	assert.Contains(t, notion, "Pages Diderot indexes")
	assert.Contains(t, notion, "Engineering")
	assert.Contains(t, notion, "Projects")
	assert.Contains(t, notion, `action="/admin/connectors/notion/roots"`)
	assert.Contains(t, notion, `<form class="form" method="post" action="/admin/connectors/notion/backfill">`)
	assert.Contains(t, notion, "Read every page listed above now instead of waiting for the daily read.")
	assert.Contains(t, notion, `type="submit">Read now</button>`)
	assert.NotContains(t, notion, "#platform")
	linear := get("/admin/connectors/linear")
	assert.Contains(t, linear, `action="/admin/connectors/linear/backfill"`)
	assert.NotContains(t, linear, "Pages Diderot indexes")
	assert.NotContains(t, get("/admin/connectors/zendesk"), "Pages Diderot indexes")
	gone := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/sources", nil, "")
	assert.Equal(t, http.StatusNotFound, gone.Code)

	// A long list comes in pages.
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id, name)
		SELECT 'CZ' || n, 'zz-' || lpad(n::text, 3, '0') FROM generate_series(1, 50) AS n`)
	require.NoError(t, err)
	first := get("/admin/connectors/slack/channels")
	assert.Len(t, channelsIn(first), slackChannelPageSize)
	assert.Contains(t, first, "1–50 of 56 channels")
	assert.Contains(t, first, `href="/admin/connectors/slack/channels?page=2">Next</a>`)
	second := get("/admin/connectors/slack/channels?page=2")
	assert.Equal(t, []string{"zz-045", "zz-046", "zz-047", "zz-048", "zz-049", "zz-050"}, channelsIn(second))
	assert.Contains(t, second, "51–56 of 56 channels")
	assert.Contains(t, second, `href="/admin/connectors/slack/channels">Previous</a>`)
	assert.NotContains(t, second, ">Next</a>")
	assert.Equal(t, channelsIn(second), channelsIn(get("/admin/connectors/slack/channels?page=9223372036854775807")),
		"a page past the last shows the last")
}

func TestSlackChannelFilterAndID(t *testing.T) {
	_, ok := slackFilter("blocked")
	assert.True(t, ok)
	_, ok = slackFilter("surprise")
	assert.False(t, ok)
	assert.True(t, validSlackChannelID("C012ABC"))
	assert.False(t, validSlackChannelID("Cbad"))
	assert.False(t, validSlackChannelID("platform"))
	assert.Equal(t, "something went wrong; the audit log has the details",
		publicSourceError(errors.New("notion roots: insert: password=secret")))
	assert.Equal(t, "this channel is not blocked", publicSourceError(fmt.Errorf("wrapped: %w", adminaction.ErrNotBlacklisted)))
}

func TestSourcesBrowserMutationWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_source_mutations"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, admin_jobs, notion_roots RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE notion_sync SET backfill_requested_at=NULL, backfill_cursor=NULL WHERE id=1`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE linear_sync SET backfill_requested_at=NULL, backfill_cursor=NULL,
		backfill_project=NULL, backfill_project_id=NULL WHERE id=1`)
	require.NoError(t, err)

	control := &recordingSourceControl{}
	rootStore := notioncx.NewRootsStore(pool)
	server, err := New(Config{
		Authenticator: testBrowserAuth(),
		Pool:          pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		NotionRootsStore: rootStore, SlackSourceControl: control,
		Registries: pageRegistries(), SourceDeps: source.Deps{Pool: pool},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	csrfToken, err := issueCSRFToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://diderot.test/admin/", nil))
	require.NoError(t, err)
	require.True(t, validCSRFToken(csrfToken))

	post := func(path string, values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("csrf_token", csrfToken)
		return browserRequest(t, mux, http.MethodPost, "https://diderot.test"+path, values, csrfToken)
	}
	said := func(result *httptest.ResponseRecorder) string {
		t.Helper()
		require.Equal(t, http.StatusSeeOther, result.Code, result.Body.String())
		message, _ := redirectMessage(t, result.Header().Get("Location"))
		return message
	}
	const channel = "/admin/connectors/slack/channels/CPLATFORM"
	assert.Equal(t, "Ask is on in this channel: Diderot answers when someone mentions it here.",
		said(post(channel+"/config", url.Values{"field": {"ask"}, "enabled": {"true"}})))
	assert.Equal(t, "ask", control.configField)
	assert.True(t, control.enabled)
	assert.Equal(t, "operator@example.com", control.actor)

	refusal := func(result *httptest.ResponseRecorder) string {
		t.Helper()
		require.Equal(t, http.StatusSeeOther, result.Code, result.Body.String())
		message, refused := redirectMessage(t, result.Header().Get("Location"))
		require.True(t, refused, message)
		return message
	}
	assert.Equal(t, "Nothing was changed: tick the box to confirm.",
		refusal(post(channel+"/config", url.Values{"field": {"external"}, "enabled": {"true"}})), "widening who may ask needs its confirmation")
	assert.Equal(t, "ask", control.configField)
	assert.Equal(t, "People outside your organization may now ask in this channel.",
		said(post(channel+"/config", url.Values{"field": {"external"}, "enabled": {"true"}, "confirm": {"external"}})))
	assert.Equal(t, "external", control.configField)

	refused := post(channel+"/blacklist", url.Values{"enabled": {"true"}, "reason": {" "}})
	message, wasRefused := redirectMessage(t, refused.Header().Get("Location"))
	assert.Equal(t, "Say why you are blocking this channel.", message)
	assert.True(t, wasRefused)
	assert.False(t, control.blacklist)
	assert.Equal(t, "This channel is blocked. Diderot reads nothing new in it.",
		said(post(channel+"/blacklist", url.Values{"enabled": {"true"}, "reason": {"restricted"}})))
	assert.True(t, control.blacklist)
	assert.Equal(t, "restricted", control.reason)
	assert.Contains(t, said(post(channel+"/backfill", url.Values{})), "Reading this channel's history again.")
	assert.Equal(t, 1, control.backfills)
	assert.Equal(t, "Reading this channel's documents again.", said(post(channel+"/backfill", url.Values{"kind": {"documents"}})))
	assert.Equal(t, 1, control.documentBackfills)
	assert.Equal(t, "Nothing was started: tick the box to confirm.",
		refusal(post(channel+"/backfill", url.Values{"kind": {"images"}, "confirm": {"backfill"}})), "rebuilding image descriptions needs its confirmation")
	assert.Equal(t, "Rebuilding this channel's image descriptions.",
		said(post(channel+"/backfill", url.Values{"kind": {"images"}, "confirm": {"images"}})))
	assert.Equal(t, "operator@example.com", control.imageReplayActor)
	assert.Equal(t, "CPLATFORM", control.imageReplayChannel)
	assert.Contains(t, said(post(channel+"/membership", url.Values{"joined": {"false"}})), "The bot left this channel.")
	assert.False(t, control.joined)

	rootID := "11111111-1111-1111-1111-111111111111"
	result := post("/admin/connectors/notion/roots", url.Values{"input": {rootID}, "label": {"Engineering"}})
	assert.True(t, strings.HasPrefix(said(result), "Page added."), result.Header().Get("Location"))
	roots, err := rootStore.List(ctx)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "operator@example.com", roots[0].AddedBy)
	said(post("/admin/connectors/notion/roots/remove", url.Values{"input": {rootID}, "confirm": {"remove"}}))
	roots, err = rootStore.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, roots)

	said(post("/admin/connectors/notion/backfill", url.Values{"confirm": {"backfill"}}))
	result = post("/admin/connectors/linear/backfill", url.Values{"field.project": {" platform "}, "confirm": {"backfill"}})
	assert.True(t, strings.HasPrefix(result.Header().Get("Location"), "/admin/connectors/linear?notice=Re-read+requested"), result.Header().Get("Location"))
	var project string
	require.NoError(t, pool.QueryRow(ctx, `SELECT backfill_project FROM linear_sync WHERE id = 1`).Scan(&project))
	assert.Equal(t, "platform", project)
	var target, details string
	require.NoError(t, pool.QueryRow(ctx, `SELECT target, details::text FROM admin_audit_log
		WHERE action = 'sources.linear.backfill'`).Scan(&target, &details))
	assert.Equal(t, "linear", target)
	assert.JSONEq(t, `{"project": "platform"}`, details)
	rejected := post("/admin/connectors/slack/backfill", url.Values{"confirm": {"backfill"}})
	assert.Equal(t, http.StatusNotFound, rejected.Code, "Slack has no source backfill")
	assert.Equal(t, "Nothing was started: tick the box to confirm.",
		refusal(post("/admin/connectors/linear/backfill", url.Values{})), "a re-read of everything needs its confirmation")
	assert.Equal(t, "Nothing was removed: tick the box to confirm.",
		refusal(post("/admin/connectors/notion/roots/remove", url.Values{"input": {rootID}})), "removing a page needs its confirmation")

	var succeeded, started int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='succeeded'),
		count(*) FILTER (WHERE status='started') FROM admin_audit_log`).Scan(&succeeded, &started))
	assert.Equal(t, 11, succeeded)
	assert.Zero(t, started)
}

// pageRegistries are Slack, Notion and Linear, the sources whose sections and
// backfills these tests use.
func pageRegistries() setup.Registries {
	return setup.Registries{Sources: source.NewRegistry(slackSource{}, notioncx.Connector{}, linearcx.Connector{})}
}

// slackSource is the Slack source as its pages see it: its Run is the
// server's.
type slackSource struct{ slackcx.Connector }

func (slackSource) Run(context.Context, source.Deps, settings.Resolved) error { return nil }
