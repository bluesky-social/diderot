package adminaction_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/auth"
	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	linearcx "github.com/bluesky-social/diderot/internal/connector/linear"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// exempt are the mutating administrator routes that write no row through
// this package, and why.
var exempt = map[string]string{
	"POST /admin/connectors/{name}":                     "internal/settings records it in the write's transaction",
	"POST /admin/connectors/{name}/enable":              "internal/settings records it in the write's transaction",
	"POST /admin/connectors/{name}/disable":             "internal/settings records it in the write's transaction",
	"POST /admin/connectors/{name}/test":                "a connector test writes nothing",
	"POST /admin/connections":                           "internal/settings records it in the write's transaction",
	"POST /admin/connections/{name}":                    "internal/settings records it in the write's transaction",
	"POST /admin/connections/{name}/enable":             "internal/settings records it in the write's transaction",
	"POST /admin/connections/{name}/disable":            "internal/settings records it in the write's transaction",
	"POST /admin/connections/{name}/delete":             "internal/settings records it in the write's transaction",
	"POST /admin/connections/{name}/test":               "a connection test writes nothing",
	"POST /admin/settings/actions/{action}":             "internal/settings records it in the write's transaction",
	"POST /admin/operations/session-summaries/schedule": "internal/settings records it in the write's transaction",
	"POST /admin/settings/organization":                 "internal/settings records it in the write's transaction",
	"POST /admin/settings/models":                       "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/embedding":             "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/model":                 "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/model/remove":          "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/model/match":           "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/model/published":       "internal/settings records it in the write's transaction",
	"POST /admin/settings/models/facts":                 "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers":                    "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers/{name}":             "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers/{name}/enable":      "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers/{name}/disable":     "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers/{name}/delete":      "internal/settings records it in the write's transaction",
	"POST /admin/settings/providers/{name}/test":        "a provider test writes nothing",
}

const (
	channel        = "C0PARITY"
	discordChannel = "200000000000000001"
	reportID       = "00000000-0000-0000-0000-000000000001"
	csrf           = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// auditCase is one administrator action: the browser's request and the audit
// row it must write.
type auditCase struct {
	name string
	// pattern is the route the request reaches, as registered; path is the
	// request's path, when the pattern has wildcards.
	pattern, path  string
	form           url.Values
	action, target string
}

// TestEveryMutatingAdminRouteWritesOneAuditRow: each state-changing request
// the browser admin accepts writes exactly one admin_audit_log row, naming
// the administrator, the action and its target, and every POST route under
// /admin/ has a case here or a stated exemption.
func TestEveryMutatingAdminRouteWritesOneAuditRow(t *testing.T) {
	h := newAuditHarness(t)
	prompt := promptcatalog.MustEmbedded().Prompts[0].Key
	promptSHA := promptcatalog.MustEmbedded().Prompts[0].SHA256
	// The browser names the channel; the page resolves it to its ID.
	reportForm := url.Values{
		"name": {"Weekly"}, "prompt": {"Summarize the week."}, "cadence": {"daily"},
		"time": {"09:00"}, "late_hours": {"1"}, "enabled": {"true"}, "timezone": {"UTC"},
		"delivery": {"channel"}, "channel": {"#weekly"}, "owner": {"owner@example.com"},
	}
	slack := "/admin/connectors/slack/channels/" + channel
	cases := []auditCase{
		{
			name:    "session summarization",
			pattern: "POST /admin/operations/session-summarization", form: url.Values{"enabled": {"true"}},
			action: "operations.session_summarization.set", target: "session-summarization",
		},
		{
			name:    "re-embed",
			pattern: "POST /admin/operations/reembed", form: url.Values{"confirm": {"reembed"}, "source": {"linear"}},
			action: "operations.reembed.start", target: "reembed",
		},
		{
			name:    "failure retry",
			pattern: "POST /admin/operations/failures/retry", form: url.Values{"kind": {"caption"}, "cause": {"rate-limited"}},
			action: "operations.failures.retry", target: "caption",
		},
		{
			name:    "describe images again",
			pattern: "POST /admin/operations/images", form: url.Values{"confirm": {"images"}},
			action: "operations.images.describe_again", target: "image-captions",
		},
		{
			name:    "repair session summaries",
			pattern: "POST /admin/operations/session-summaries/repair", form: url.Values{"confirm": {"repair"}},
			action: "operations.session_summaries.repair", target: "session-summaries",
		},
		{
			name:    "slack documents rebuild",
			pattern: "POST /admin/operations/slack-documents", form: url.Values{"confirm": {"documents"}},
			action: "sources.slack.documents.rebuild", target: "all",
		},
		{
			name:    "slack invite by name",
			pattern: "POST /admin/connectors/slack/channels/invite", form: url.Values{"channel": {"#" + channel}},
			action: "sources.slack.invite", target: channel,
		},
		{
			name:    "slack ask",
			pattern: "POST /admin/connectors/slack/channels/{id}/config", path: slack + "/config", form: url.Values{"field": {"ask"}, "enabled": {"true"}},
			action: "sources.slack.config.ask", target: channel,
		},
		{
			name:    "slack documents",
			pattern: "POST /admin/connectors/slack/channels/{id}/config", path: slack + "/config", form: url.Values{"field": {"documents"}, "enabled": {"true"}},
			action: "sources.slack.config.documents", target: channel,
		},
		{
			name:    "slack external ask",
			pattern: "POST /admin/connectors/slack/channels/{id}/config", path: slack + "/config", form: url.Values{"field": {"external"}, "enabled": {"false"}},
			action: "sources.slack.config.external", target: channel,
		},
		{
			name:    "slack blacklist",
			pattern: "POST /admin/connectors/slack/channels/{id}/blacklist", path: slack + "/blacklist", form: url.Values{"enabled": {"true"}, "confirm": {"blacklist"}, "reason": {"noisy"}},
			action: "sources.slack.blacklist", target: channel,
		},
		{
			name:    "slack unblacklist",
			pattern: "POST /admin/connectors/slack/channels/{id}/blacklist", path: slack + "/blacklist", form: url.Values{"enabled": {"false"}, "confirm": {"unblacklist"}},
			action: "sources.slack.unblacklist", target: channel,
		},
		{
			name:    "slack invite",
			pattern: "POST /admin/connectors/slack/channels/{id}/membership", path: slack + "/membership", form: url.Values{"joined": {"true"}, "confirm": {"invite"}},
			action: "sources.slack.invite", target: channel,
		},
		{
			name:    "slack leave",
			pattern: "POST /admin/connectors/slack/channels/{id}/membership", path: slack + "/membership", form: url.Values{"joined": {"false"}, "confirm": {"leave"}},
			action: "sources.slack.leave", target: channel,
		},
		{
			name:    "slack message backfill",
			pattern: "POST /admin/connectors/slack/channels/{id}/backfill", path: slack + "/backfill", form: url.Values{"kind": {"messages"}, "confirm": {"backfill"}},
			action: "sources.slack.backfill", target: channel,
		},
		{
			name:    "slack image replay",
			pattern: "POST /admin/connectors/slack/channels/{id}/backfill", path: slack + "/backfill", form: url.Values{"kind": {"images"}, "confirm": {"images"}},
			action: "sources.slack.images.enqueue", target: channel,
		},
		{
			name:    "slack document reconciliation",
			pattern: "POST /admin/connectors/slack/channels/{id}/backfill", path: slack + "/backfill", form: url.Values{"kind": {"documents"}, "confirm": {"backfill"}},
			action: "sources.slack.backfill", target: channel,
		},
		{
			name:    "source backfill",
			pattern: "POST /admin/connectors/{name}/backfill", path: "/admin/connectors/linear/backfill", form: url.Values{"confirm": {"backfill"}, "field.project": {"Platform"}},
			action: "sources.linear.backfill", target: linearcx.ConnectorName,
		},
		{
			name:    "notion root add",
			pattern: "POST /admin/connectors/notion/roots", form: url.Values{"input": {"root-page"}, "label": {"Docs"}},
			action: "sources.notion.root.add", target: "root-page",
		},
		{
			name:    "notion root remove",
			pattern: "POST /admin/connectors/notion/roots/remove", form: url.Values{"input": {"root-page"}, "confirm": {"remove"}},
			action: "sources.notion.root.remove", target: "root-page",
		},
		{
			name:    "discord channel choose",
			pattern: "POST /admin/connectors/discord/channels", form: url.Values{"channel": {discordChannel}, "visibility": {"everyone"}},
			action: "sources.discord.channel.choose", target: discordChannel,
		},
		{
			name:    "discord channel reread",
			pattern: "POST /admin/connectors/discord/channels/reread", form: url.Values{"channel": {discordChannel}},
			action: "sources.discord.channel.reread", target: discordChannel,
		},
		{
			name:    "discord channel remove",
			pattern: "POST /admin/connectors/discord/channels/remove", form: url.Values{"channel": {discordChannel}, "confirm": {"remove"}},
			action: "sources.discord.channel.remove", target: discordChannel,
		},
		{
			name:    "discord ask guild",
			pattern: "POST /admin/connectors/discord/ask/guild", form: url.Values{"guild": {"100000000000000001"}, "on": {"yes"}},
			action: "sources.discord.ask.guild", target: "100000000000000001",
		},
		{
			name:    "discord ask channel",
			pattern: "POST /admin/connectors/discord/ask/channel", form: url.Values{"channel": {discordChannel}, "on": {"no"}},
			action: "sources.discord.ask.channel", target: discordChannel,
		},
		{
			name:    "discord trusted server",
			pattern: "POST /admin/connectors/discord/ask/trusted", form: url.Values{"guild": {"100000000000000001"}, "trusted": {"yes"}},
			action: "sources.discord.ask.trusted", target: "100000000000000001",
		},
		{
			name:    "discord ask direct messages",
			pattern: "POST /admin/connectors/discord/ask/dms", form: url.Values{"on": {"yes"}},
			action: "sources.discord.ask.direct_messages", target: "discord",
		},
		{
			name:    "report create",
			pattern: "POST /admin/reports", form: reportForm,
			action: "reports.create", target: "Weekly",
		},
		{
			name:    "report update",
			pattern: "POST /admin/reports/{id}", path: "/admin/reports/" + reportID, form: reportForm,
			action: "reports.update", target: reportID,
		},
		{
			name:    "report enable",
			pattern: "POST /admin/reports/{id}/enabled", path: "/admin/reports/" + reportID + "/enabled", form: url.Values{"enabled": {"true"}},
			action: "reports.enable", target: reportID,
		},
		{
			name:    "report run",
			pattern: "POST /admin/reports/{id}/run", path: "/admin/reports/" + reportID + "/run", form: url.Values{},
			action: "reports.run", target: reportID,
		},
		{
			name:    "report delete",
			pattern: "POST /admin/reports/{id}/delete", path: "/admin/reports/" + reportID + "/delete", form: url.Values{"confirm": {"delete"}},
			action: "reports.delete", target: reportID,
		},
		{
			name:    "prompt release activate",
			pattern: "POST /admin/prompts/versions/{id}/activate", path: "/admin/prompts/versions/1/activate", form: url.Values{"expected_release_id": {"1"}},
			action: "prompts.activate", target: "1",
		},
		{
			name:    "prompt draft save",
			pattern: "POST /admin/prompts/{key}", path: "/admin/prompts/" + prompt, form: url.Values{"live_sha256": {promptSHA}, "body": {"A replacement prompt."}},
			action: "prompts.draft.save", target: prompt,
		},
		{
			name:    "prompt draft discard",
			pattern: "POST /admin/prompts/{key}/discard", path: "/admin/prompts/" + prompt + "/discard", form: url.Values{"live_sha256": {promptSHA}},
			action: "prompts.draft.discard", target: prompt,
		},
		{
			name:    "prompt draft restore",
			pattern: "POST /admin/prompts/{key}/restore", path: "/admin/prompts/" + prompt + "/restore", form: url.Values{"live_sha256": {promptSHA}, "restore_release_id": {"1"}},
			action: "prompts.draft.restore", target: prompt,
		},
		{
			name:    "prompt take shipped",
			pattern: "POST /admin/prompts/shipped", form: url.Values{"take": {prompt}, "live_sha256." + prompt: {promptSHA}, "shipped_sha256." + prompt: {promptSHA}},
			action: "prompts.draft.take_shipped", target: "draft",
		},
		{
			name:    "prompt draft publish",
			pattern: "POST /admin/prompts/draft/publish", form: url.Values{"expected_release_id": {"1"}, "fingerprint": {promptcatalog.MustEmbedded().SHA256}},
			action: "prompts.draft.publish", target: "draft",
		},
		{
			name:    "prompt draft discard all",
			pattern: "POST /admin/prompts/draft/discard", form: url.Values{"confirm": {"discard"}, "fingerprint": {promptcatalog.MustEmbedded().SHA256}},
			action: "prompts.draft.discard_all", target: "draft",
		},
	}

	// Every mutating administrator route has a case or a stated exemption,
	// so a new route that writes no audit row fails here.
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.pattern] = true
	}
	mutating := h.mutatingRoutes()
	require.NotEmpty(t, mutating)
	for _, pattern := range mutating {
		if _, ok := exempt[pattern]; ok {
			assert.False(t, covered[pattern], "route %q is both exempt and covered", pattern)
			continue
		}
		assert.True(t, covered[pattern], "mutating administrator route %q has no audit case", pattern)
	}
	for pattern := range covered {
		assert.Contains(t, mutating, pattern, "case names a route that isn't registered")
	}
	for pattern := range exempt {
		assert.Contains(t, mutating, pattern, "exemption names a route that isn't registered")
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := h.do(t, c)
			assert.Equal(t, "operator@example.com", row.Actor)
			assert.Equal(t, c.action, row.Action)
			assert.Equal(t, c.target, row.Target)
			assert.Equal(t, "succeeded", row.Status)
		})
	}
}

type auditRow struct {
	Actor, Action, Target, Details, Status string
}

type auditHarness struct {
	pool      *pgxpool.Pool
	api       *adminapi.Server
	ui        *routes
	blacklist *fakeBlacklist
}

// routes records the patterns registered on a mux, and serves them.
type routes struct {
	*http.ServeMux
	patterns []string
}

func (r *routes) Handle(pattern string, handler http.Handler) {
	r.patterns = append(r.patterns, pattern)
	r.ServeMux.Handle(pattern, handler)
}

func (r *routes) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
	r.ServeMux.HandleFunc(pattern, handler)
}

// mutatingRoutes are the browser UI's POST routes under /admin/.
func (h *auditHarness) mutatingRoutes() []string {
	var out []string
	for _, pattern := range h.ui.patterns {
		if strings.HasPrefix(pattern, "POST /admin/") {
			out = append(out, pattern)
		}
	}
	return out
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminaction_parity"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &auditHarness{pool: pool, blacklist: &fakeBlacklist{}}

	sources := source.NewRegistry(linearcx.Connector{})
	deps := source.Deps{Pool: pool, Logger: logger}
	summary := sessioningest.NewSummaryControl(sessioningest.NewStore(pool), nil)
	reports := &fakeReports{}
	roots := &fakeRoots{}

	// The browser acts through the API server's in-process services.
	api := adminapi.NewServer(nil, logger)
	api.SetStatusPool(pool)
	h.api = api
	api.SetSlackControls(func() (adminapi.SlackControls, bool) {
		return adminapi.SlackControls{
			SyncWorker: fakeSyncWorker{}, ChannelConfigs: fakeChannelConfigs{},
			Blacklist: h.blacklist, Inviter: fakeInviter{},
		}, true
	})

	browserAuth, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs:       []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		EmailHeader: "X-Email", GroupsHeader: "X-Groups", GroupsDelimiter: "|", Logger: logger,
	})
	require.NoError(t, err)
	ui, err := adminui.New(adminui.Config{
		Pool: pool, Logger: logger, Authenticator: browserAuth,
		AdminGroups: []string{"diderot-admins"}, PublicURL: "https://diderot.test",
		SessionSummaryControl: summary, ReembedLauncher: api, ReembedSources: []string{"linear"}, ScheduledReportsStore: reports,
		CaptionModel:     func() string { return "caption-model" },
		NotionRootsStore: roots, SlackSourceControl: api, Connectors: connectorsOn{},
		Registries: setup.Registries{Sources: sources}, SourceDeps: deps,
	})
	require.NoError(t, err)
	h.ui = &routes{ServeMux: http.NewServeMux()}
	require.NoError(t, ui.Mount(h.ui))
	// The Discord channel the cases choose is one the bot can see.
	require.NoError(t, source.NewState(pool, discordcx.ConnectorName).Set(ctx, "directory", discordcx.Directory{
		ReadAt: time.Now(), Guilds: []discordcx.GuildEntry{{ID: "100000000000000001", Name: "Example Server",
			Channels: []discordcx.ChannelEntry{{ID: discordChannel, Name: "general", Readable: true}}}},
	}))
	return h
}

// reset puts the database and fakes in the state every case starts from.
func (h *auditHarness) reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `
		TRUNCATE admin_audit_log, admin_jobs, session_ingest_control,
		         prompt_releases, prompt_release_entries, prompt_active_release, prompt_release_activations,
		         prompt_draft_entries
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx, `INSERT INTO session_ingest_control (id, summarization_enabled) VALUES (true, false)`)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name) VALUES ('C0EXAMPLE01', 'weekly')
		ON CONFLICT (channel_id) DO NOTHING`)
	require.NoError(t, err)
	_, err = promptcatalog.NewStore(h.pool).EnsureSeed(ctx, promptcatalog.MustEmbedded(), "seed")
	require.NoError(t, err)
	// The document rebuild needs a channel that indexes documents.
	_, err = h.pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, doc_ingest_enabled) VALUES ($1, true)
		ON CONFLICT (channel_id) DO UPDATE SET doc_ingest_enabled = true`, channel)
	require.NoError(t, err)
	// The image replay acts only on a channel diderot has seen.
	_, err = h.pool.Exec(ctx, `
		INSERT INTO slack_channel_sync (channel_id, last_synced_at, ingest_enabled)
		VALUES ($1, now(), true) ON CONFLICT (channel_id) DO NOTHING`, channel)
	require.NoError(t, err)
	h.blacklist.reset(channel)
}

// do resets the state, sends c's request to the browser UI, and returns the
// one audit row it wrote.
func (h *auditHarness) do(t *testing.T, c auditCase) auditRow {
	t.Helper()
	h.reset(t)
	method, path, _ := strings.Cut(c.pattern, " ")
	if c.path != "" {
		path = c.path
	}
	form := url.Values{}
	for key, values := range c.form {
		form[key] = values
	}
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest(method, "https://diderot.test"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://diderot.test")
	req.Header.Set("X-Email", "operator@example.com")
	req.Header.Set("X-Groups", "diderot-admins")
	req.AddCookie(&http.Cookie{Name: "diderot_admin_csrf", Value: csrf})
	w := httptest.NewRecorder()
	h.ui.ServeHTTP(w, req)

	rows, err := h.pool.Query(context.Background(), `
		SELECT actor_email, action, target, details::text, status FROM admin_audit_log ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		require.NoError(t, rows.Scan(&row.Actor, &row.Action, &row.Target, &row.Details, &row.Status))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, out, 1, "%s answered %d: %s", c.pattern, w.Code, w.Body.String())
	return out[0]
}

// connectorsOn is the connector settings with every connector on: Linear,
// the one source the harness registers, and Slack, which reports post to.
// Nothing here saves or tests a connector.
type connectorsOn struct{ adminui.ConnectorSettings }

func (connectorsOn) List(context.Context) ([]settings.View, error) {
	return []settings.View{{Name: linearcx.ConnectorName, Enabled: true}}, nil
}

func (connectorsOn) View(_ context.Context, name string) (settings.View, error) {
	return settings.View{Name: name, Enabled: true}, nil
}

type fakeSyncWorker struct{}

func (fakeSyncWorker) RequestBackfill(context.Context, string) (bool, error) { return true, nil }

type fakeChannelConfigs struct{}

func (fakeChannelConfigs) SetAllowExternalAsk(context.Context, string, bool, string) error {
	return nil
}
func (fakeChannelConfigs) SetAskEnabled(context.Context, string, bool, string) error { return nil }
func (fakeChannelConfigs) SetDocIngestEnabled(context.Context, string, bool, string) error {
	return nil
}
func (fakeChannelConfigs) DocIngestEnabled(context.Context, string) (bool, error) { return true, nil }

// fakeBlacklist holds the channels currently blacklisted.
type fakeBlacklist struct{ channels []string }

func (f *fakeBlacklist) reset(channels ...string) { f.channels = slices.Clone(channels) }
func (f *fakeBlacklist) Add(_ context.Context, id, name, reason, addedBy string) (slackcx.BlacklistEntry, error) {
	f.channels = append(f.channels, id)
	return slackcx.BlacklistEntry{ChannelID: id, ChannelName: name, Reason: reason, AddedBy: addedBy}, nil
}
func (f *fakeBlacklist) RemoveByID(_ context.Context, id string) (bool, error) {
	before := len(f.channels)
	f.channels = slices.DeleteFunc(f.channels, func(c string) bool { return c == id })
	return len(f.channels) < before, nil
}

type fakeInviter struct{}

func (fakeInviter) ResolveChannel(_ context.Context, nameOrID string) (string, string, error) {
	return nameOrID, "parity", nil
}
func (fakeInviter) Invite(context.Context, string) error                 { return nil }
func (fakeInviter) IsBotInChannel(context.Context, string) (bool, error) { return true, nil }
func (fakeInviter) Kick(context.Context, string) error                   { return nil }

type fakeRoots struct{}

func (fakeRoots) List(context.Context) ([]notioncx.Root, error) { return nil, nil }
func (fakeRoots) ListDiscoveries(context.Context) ([]notioncx.DiscoveryStatus, error) {
	return nil, nil
}
func (fakeRoots) Add(_ context.Context, input, _, _ string) (string, error) { return input, nil }
func (fakeRoots) Remove(context.Context, string) (bool, error)              { return true, nil }

type fakeReports struct{}

func (fakeReports) report() *scheduledreport.Report {
	return &scheduledreport.Report{ID: uuid.MustParse(reportID), Name: "Weekly", NextRunAt: time.Now()}
}
func (f fakeReports) Create(context.Context, scheduledreport.CreateParams) (*scheduledreport.Report, error) {
	return f.report(), nil
}
func (f fakeReports) List(context.Context) ([]scheduledreport.Report, error) { return nil, nil }
func (f fakeReports) Get(context.Context, uuid.UUID) (*scheduledreport.Report, error) {
	return f.report(), nil
}
func (f fakeReports) Update(context.Context, uuid.UUID, scheduledreport.CreateParams) (*scheduledreport.Report, error) {
	return f.report(), nil
}
func (f fakeReports) SetEnabled(context.Context, uuid.UUID, bool) (*scheduledreport.Report, error) {
	return f.report(), nil
}
func (fakeReports) Delete(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (fakeReports) EnqueueNow(context.Context, uuid.UUID) (*scheduledreport.Run, error) {
	return &scheduledreport.Run{ID: uuid.New()}, nil
}
func (fakeReports) ListRuns(context.Context, uuid.UUID, int) ([]scheduledreport.Run, error) {
	return nil, nil
}
func (fakeReports) LatestRuns(context.Context) (map[uuid.UUID]scheduledreport.Run, error) {
	return nil, nil
}

// TestRebuildSlackDocumentsCoversEveryOptedInChannel: the rebuild deletes
// and requeues the documents of every channel with document indexing on, and
// of no other, as one audited job that reports what it read.
func TestRebuildSlackDocumentsCoversEveryOptedInChannel(t *testing.T) {
	h := newAuditHarness(t)
	h.reset(t)
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `TRUNCATE events, slack_channel_config RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	seed := func(channelID, fileID string) int64 {
		raw := `{"type":"message","subtype":"file_share","channel":"` + channelID + `","user":"U1","ts":"1700000000.000001",` +
			`"message":{"user":"U1","ts":"1700000000.000001","files":[{"id":"` + fileID +
			`","name":"contract.pdf","mimetype":"application/pdf","mode":"hosted"}]}}`
		var eventID int64
		require.NoError(t, h.pool.QueryRow(ctx, `
			INSERT INTO events (source, external_id, occurred_at, actor, body, raw, scope)
			VALUES ('slack', $1, now(), 'tester', 'file', $2::jsonb, 'public') RETURNING id`,
			channelID+":1700000000.000001", raw).Scan(&eventID))
		_, err := h.pool.Exec(ctx, `
			INSERT INTO slack_message_details (event_id, channel_id, user_id, has_files, occurred_at)
			VALUES ($1, $2, 'U1', true, now())`, eventID, channelID)
		require.NoError(t, err)
		return eventID
	}
	one, two, off := seed("CONE", "F1"), seed("CTWO", "F2"), seed("COFF", "F3")
	audit := adminaction.Audit{Pool: h.pool, Actor: adminaction.Actor{Email: "operator@example.com"}}

	_, err = adminaction.RebuildSlackDocuments(ctx, audit, h.api)
	require.ErrorIs(t, err, adminapi.ErrNoDocumentChannels, "nothing to rebuild while no channel indexes documents")

	configs := slackcx.NewChannelConfigStore(h.pool, nil, nil, nil)
	require.NoError(t, configs.SetDocIngestEnabled(ctx, "CONE", true, "operator@example.com"))
	require.NoError(t, configs.SetDocIngestEnabled(ctx, "CTWO", true, "operator@example.com"))
	// What was extracted before is deleted, not kept beside the new rows.
	_, err = h.pool.Exec(ctx, `
		INSERT INTO event_documents (event_id, source_file_id, filename, media_type, occurred_at, extracted_at)
		VALUES ($1, 'F_STALE', 'old.pdf', 'application/pdf', now(), now())`, one)
	require.NoError(t, err)

	jobID, err := adminaction.RebuildSlackDocuments(ctx, audit, h.api)
	require.NoError(t, err)
	var status, progress string
	require.Eventually(t, func() bool {
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT status, progress::text FROM admin_jobs WHERE id = $1`, jobID).Scan(&status, &progress))
		return status != "running"
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, "succeeded", status)
	assert.JSONEq(t, `{"cursor":`+strconv.FormatInt(two, 10)+`,"scanned":2,"queued":2,"documents":2,"errors":0}`, progress)

	rows, err := h.pool.Query(ctx, `SELECT event_id::text || ' ' || source_file_id FROM event_documents ORDER BY event_id`)
	require.NoError(t, err)
	queued, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{strconv.FormatInt(one, 10) + " F1", strconv.FormatInt(two, 10) + " F2"}, queued,
		"the opted-in channels' documents wait to be read again; the stale row and channel %d's are not there", off)

	rows, err = h.pool.Query(ctx, `SELECT action || ' ' || target || ' ' || status FROM admin_audit_log ORDER BY id`)
	require.NoError(t, err)
	audited, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"sources.slack.documents.rebuild all failed", "sources.slack.documents.rebuild all succeeded"}, audited)
}
