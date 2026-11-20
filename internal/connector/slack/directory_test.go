package slack

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a slackAPI that returns canned data and counts calls
// so tests can assert cache behavior (no redundant API calls).
type fakeAPI struct {
	users     map[string]*slack.User
	userErrs  map[string]error
	bots      map[string]*slack.Bot
	botErrs   map[string]error
	channels  map[string]*slack.Channel
	groups    []slack.UserGroup
	userCalls atomic.Int32
	botCalls  atomic.Int32
	chanCalls atomic.Int32
	grpCalls  atomic.Int32
}

func (f *fakeAPI) GetBotInfoContext(_ context.Context, p slack.GetBotInfoParameters) (*slack.Bot, error) {
	f.botCalls.Add(1)
	if err := f.botErrs[p.Bot]; err != nil {
		return nil, err
	}
	if b, ok := f.bots[p.Bot]; ok {
		return b, nil
	}
	return nil, &slack.SlackErrorResponse{Err: "bot_not_found"}
}

func (f *fakeAPI) GetUserInfoContext(_ context.Context, id string, _ ...slack.GetUserInfoOption) (*slack.User, error) {
	f.userCalls.Add(1)
	if err := f.userErrs[id]; err != nil {
		return nil, err
	}
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, &slack.SlackErrorResponse{Err: "user_not_found"}
}

func (f *fakeAPI) GetConversationInfoContext(_ context.Context, in *slack.GetConversationInfoInput) (*slack.Channel, error) {
	f.chanCalls.Add(1)
	if c, ok := f.channels[in.ChannelID]; ok {
		return c, nil
	}
	return nil, &slack.SlackErrorResponse{Err: "channel_not_found"}
}

func (f *fakeAPI) GetUserGroupsContext(_ context.Context, _ ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	f.grpCalls.Add(1)
	return f.groups, nil
}

func (f *fakeAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	// Empty list by default; tests that exercise LookupChannelByName
	// either pre-seed slack_channel_info directly or extend this
	// fake. Keeps the existing tests passing without the channel
	// listing returning surprises.
	out := make([]slack.Channel, 0, len(f.channels))
	for _, ch := range f.channels {
		if ch != nil {
			out = append(out, *ch)
		}
	}
	return out, "", nil
}

func getDirDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func truncateDir(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE slack_users, slack_bots, slack_channel_info, slack_usergroups RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

func TestLookupBotHydratesAndCachesDisplayName(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{bots: map[string]*slack.Bot{
		"B0EXAMPLE01": {
			ID: "B0EXAMPLE01", Name: "Build Status bot",
			AppID: "A0EXAMPLE01", UserID: "U_BUILD_BOT",
		},
	}}
	d := newDirectoryWithAPI(pool, api)

	got, err := d.LookupBot(context.Background(), "B0EXAMPLE01")
	require.NoError(t, err)
	assert.Equal(t, "Build Status bot", got.Name)
	assert.Equal(t, "A0EXAMPLE01", got.AppID)

	got, err = d.LookupBot(context.Background(), "B0EXAMPLE01")
	require.NoError(t, err)
	assert.Equal(t, "Build Status bot", got.Name)
	assert.Equal(t, int32(1), api.botCalls.Load(), "second lookup should use the persistent cache")

	var name, appID string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT name, app_id FROM slack_bots WHERE bot_id = $1`, "B0EXAMPLE01").Scan(&name, &appID))
	assert.Equal(t, "Build Status bot", name)
	assert.Equal(t, "A0EXAMPLE01", appID)
}

func TestLookupBotPreservesStaleNameOnRefreshFailure(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slack_bots (bot_id, name, fetched_at)
		 VALUES ('B_STALE', 'Useful bot name', now() - interval '30 days')`)
	require.NoError(t, err)

	api := &fakeAPI{botErrs: map[string]error{
		"B_STALE": &slack.SlackErrorResponse{Err: "internal_error"},
	}}
	got, err := newDirectoryWithAPI(pool, api).LookupBot(context.Background(), "B_STALE")
	require.NoError(t, err)
	assert.Equal(t, "Useful bot name", got.Name)
}

func TestLookupBotCachesSuccessfulEmptyName(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)
	api := &fakeAPI{bots: map[string]*slack.Bot{
		"B_NAMELESS": {ID: "B_NAMELESS", AppID: "A_NAMELESS"},
	}}
	d := newDirectoryWithAPI(pool, api)

	for range 5 {
		got, err := d.LookupBot(context.Background(), "B_NAMELESS")
		require.NoError(t, err)
		assert.Empty(t, got.Name)
	}
	assert.Equal(t, int32(1), api.botCalls.Load(),
		"a successful empty-name result must still be cached")
}

func makeUser(id, name, display string) *slack.User {
	u := &slack.User{ID: id, Name: name}
	u.Profile.DisplayName = display
	return u
}

func TestResolve_UnescapesEntities(t *testing.T) {
	// Pure-text test: no refs, no directory lookups. The resolver
	// must still unescape &gt; &lt; &amp;.
	d := newDirectoryWithAPI(nil, &fakeAPI{})
	got := d.Resolve(context.Background(), "alpha-&gt;beta &amp; back &lt;-")
	assert.Equal(t, "alpha->beta & back <-", got)
}

func TestResolve_UserReference(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{users: map[string]*slack.User{
		"U0EXAMPLE02": makeUser("U0EXAMPLE02", "avery", "avery"),
	}}
	d := newDirectoryWithAPI(pool, api)
	got := d.Resolve(context.Background(), "cc <@U0EXAMPLE02> about this")
	assert.Equal(t, "cc @avery about this", got)

	// Second call must hit cache — fakeAPI call count should stay at 1.
	_ = d.Resolve(context.Background(), "again <@U0EXAMPLE02>")
	assert.Equal(t, int32(1), api.userCalls.Load(),
		"second resolve should hit cache, not API")
}

func TestResolve_UserReferenceWithDisplayNameFallback(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	// No display_name set; should fall back to username.
	api := &fakeAPI{users: map[string]*slack.User{
		"U01AAAAAAAA": makeUser("U01AAAAAAAA", "harper", ""),
	}}
	d := newDirectoryWithAPI(pool, api)
	got := d.Resolve(context.Background(), "<@U01AAAAAAAA> shipped it")
	assert.Equal(t, "@harper shipped it", got)
}

func TestResolve_EnterpriseGridUserReference(t *testing.T) {
	// Regression: Enterprise Grid user IDs start with W, not U.
	// Regex must match both or W-prefix users stay as raw IDs.
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{users: map[string]*slack.User{
		"W01ENTERPRISE": makeUser("W01ENTERPRISE", "enterprise-avery", "ent-avery"),
	}}
	d := newDirectoryWithAPI(pool, api)
	got := d.Resolve(context.Background(), "<@W01ENTERPRISE> owns this")
	assert.Equal(t, "@ent-avery owns this", got)
}

func TestResolve_NegativeCachingAvoidsAPIStorm(t *testing.T) {
	// Regression: a backfill encountering the same unresolvable
	// <@U...> id thousands of times must not fire thousands of
	// users.info calls — Slack's Tier 4 budget is 20 req/min.
	// After the first miss, the directory must write a negative
	// cache row so subsequent lookups hit the cache.
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{users: map[string]*slack.User{}} // nothing resolves
	d := newDirectoryWithAPI(pool, api)
	for i := 0; i < 50; i++ {
		_ = d.Resolve(context.Background(), "<@U01DEACTIVATED> was here")
	}
	assert.Equal(t, int32(1), api.userCalls.Load(),
		"50 mentions of the same unresolvable user should trigger exactly 1 API call")

	// Same pattern for channels.
	api.chanCalls.Store(0)
	for i := 0; i < 50; i++ {
		_ = d.Resolve(context.Background(), "<#C01DEAD> too")
	}
	assert.Equal(t, int32(1), api.chanCalls.Load(),
		"50 mentions of unresolvable channel should trigger 1 API call")
}

func TestResolve_TransientErrorNotNegativelyCached(t *testing.T) {
	// Regression: if Slack's API blips (503, network, rate-limit)
	// we must NOT write a negative cache entry — otherwise the
	// entity stays unresolvable for the full TTL.
	pool := getDirDB(t)
	truncateDir(t, pool)

	// transientAPI returns a generic non-"user_not_found" error,
	// simulating a 503 or network error surfacing through slack-go.
	api := &transientFailAPI{}
	d := newDirectoryWithAPI(pool, api)
	_ = d.Resolve(context.Background(), "<@U01FLAKY> here")
	_ = d.Resolve(context.Background(), "<@U01FLAKY> and again")

	// Both calls should have hit the API — no negative cache
	// was written because the error isn't "user_not_found".
	assert.Equal(t, int32(2), api.calls.Load(),
		"transient failure must not populate negative cache")
}

type transientFailAPI struct {
	calls atomic.Int32
}

func (f *transientFailAPI) GetUserInfoContext(_ context.Context, _ string, _ ...slack.GetUserInfoOption) (*slack.User, error) {
	f.calls.Add(1)
	return nil, &slack.SlackErrorResponse{Err: "internal_error"}
}
func (f *transientFailAPI) GetConversationInfoContext(_ context.Context, _ *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return nil, &slack.SlackErrorResponse{Err: "internal_error"}
}
func (f *transientFailAPI) GetUserGroupsContext(_ context.Context, _ ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	return nil, &slack.SlackErrorResponse{Err: "internal_error"}
}
func (f *transientFailAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return nil, "", &slack.SlackErrorResponse{Err: "internal_error"}
}

func TestResolve_StalePositiveRefreshesOnDefinitiveNotFound(t *testing.T) {
	// Scenario: a user was cached a long time ago; they've since
	// been deleted so Slack now returns user_not_found. Our
	// resolver should (a) keep returning the stale cached name so
	// the reader sees something useful, and (b) bump fetched_at
	// so the next 100 mentions don't each trigger another API
	// call. Without (b) the stale-positive path re-storms Slack.
	pool := getDirDB(t)
	truncateDir(t, pool)

	// Seed a stale positive entry (fetched long ago).
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slack_users (user_id, name, display_name, real_name, email, is_bot, fetched_at)
		 VALUES ('U01GONE', 'ghost', 'ghost', '', '', false, now() - interval '30 days')`)
	require.NoError(t, err)

	// API reports them as gone.
	api := &notFoundAPI{}
	d := newDirectoryWithAPI(pool, api)
	for i := 0; i < 10; i++ {
		got := d.Resolve(context.Background(), "<@U01GONE> saw it")
		assert.Equal(t, "@ghost saw it", got, "stale name must still be returned")
	}
	assert.Equal(t, int32(1), api.calls.Load(),
		"stale+404 path must only call the API once per TTL window")

	// And fetched_at should now be recent.
	var fetchedAt time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT fetched_at FROM slack_users WHERE user_id='U01GONE'`).Scan(&fetchedAt))
	assert.WithinDuration(t, time.Now(), fetchedAt, 10*time.Second,
		"fetched_at must be bumped so subsequent lookups hit cache")
}

// TestLookupUserInfo_StaleProfilelessBotSurvivesTransientError pins
// the stale-fallback path's use of the IsNegativeCache flag (not a
// name-emptiness check): a cached profile-less bot (empty names,
// is_bot=true) whose re-fetch fails transiently must fall back to the
// stale cached row, not bubble the error. A name-based fallback gate
// would treat the bot as "no usable cache" and return the error.
func TestLookupUserInfo_StaleProfilelessBotSurvivesTransientError(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	// Stale cached profile-less bot: all names empty, is_bot=true,
	// fetched long ago so the lookup re-fetches.
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slack_users (user_id, name, display_name, real_name, email, is_bot, fetched_at)
		 VALUES ('U0BOT', '', '', '', '', true, now() - interval '30 days')`)
	require.NoError(t, err)

	api := &transientFailAPI{}
	d := newDirectoryWithAPI(pool, api)
	ui, err := d.LookupUserInfo(context.Background(), "U0BOT")
	require.NoError(t, err, "transient re-fetch failure must fall back to the stale cached bot, not error out")
	assert.True(t, ui.IsBot, "fell back to the cached bot row")
	assert.Equal(t, "slack:U0BOT", SlackActorCanonical(ui),
		"stale bot keeps its canonical key on the fallback path")
}

func TestLookupUserInfoStrictRejectsStaleProfileOnRefreshFailure(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	// Ten minutes is fresh enough for the ordinary six-hour channel-gate
	// cache, but outside the five-minute DM authorization window.
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slack_users
		   (user_id, name, display_name, real_name, email, is_bot,
		    is_stranger, is_restricted, is_ultra_restricted, fetched_at)
		 VALUES
		   ('U0STALE', 'stale', 'stale', 'Stale User', 'stale@example.com', false,
		    false, false, false, now() - interval '10 minutes')`)
	require.NoError(t, err)

	api := &transientFailAPI{}
	d := newDirectoryWithAPI(pool, api)
	ui, err := d.LookupUserInfo(context.Background(), "U0STALE")
	require.NoError(t, err, "ordinary lookup may use its six-hour cache")
	assert.Equal(t, "stale@example.com", ui.Email)
	assert.Equal(t, int32(0), api.calls.Load(), "ordinary lookup should remain cached")

	_, err = d.LookupUserInfoStrict(context.Background(), "U0STALE")
	require.Error(t, err, "DM authorization must not reuse a profile older than five minutes")
	assert.Equal(t, int32(1), api.calls.Load(), "strict lookup should attempt a live refresh")
}

type notFoundAPI struct {
	calls atomic.Int32
}

func (f *notFoundAPI) GetUserInfoContext(_ context.Context, _ string, _ ...slack.GetUserInfoOption) (*slack.User, error) {
	f.calls.Add(1)
	return nil, &slack.SlackErrorResponse{Err: "user_not_found"}
}
func (f *notFoundAPI) GetConversationInfoContext(_ context.Context, _ *slack.GetConversationInfoInput) (*slack.Channel, error) {
	f.calls.Add(1)
	return nil, &slack.SlackErrorResponse{Err: "channel_not_found"}
}
func (f *notFoundAPI) GetUserGroupsContext(_ context.Context, _ ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	f.calls.Add(1)
	return nil, nil
}
func (f *notFoundAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	f.calls.Add(1)
	return nil, "", nil
}

func TestResolve_UnknownUserFallsBackToRawID(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{users: map[string]*slack.User{}}
	d := newDirectoryWithAPI(pool, api)
	// Unknown user — API returns user_not_found. Resolver keeps
	// the ID visible so the reader sees "@U01..." rather than
	// silently dropping the mention.
	got := d.Resolve(context.Background(), "<@U01ZZZZZZZZ> was here")
	assert.Equal(t, "@U01ZZZZZZZZ was here", got)
}

func TestResolve_ChannelReferenceUsesInlineNameOnAPIFailure(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{channels: map[string]*slack.Channel{}}
	d := newDirectoryWithAPI(pool, api)
	// When API fails but message carries inline name, use the
	// inline name rather than the raw ID.
	got := d.Resolve(context.Background(), "in <#C12345|ops> today")
	assert.Equal(t, "in #ops today", got)
}

func TestResolve_UsergroupReference(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{groups: []slack.UserGroup{
		{ID: "S01AAAA", Name: "Netops team", Handle: "netops"},
	}}
	d := newDirectoryWithAPI(pool, api)
	got := d.Resolve(context.Background(), "<!subteam^S01AAAA|netops> heads up")
	assert.Equal(t, "@netops heads up", got)
}

func TestResolve_URLWithAnchor(t *testing.T) {
	// No directory call needed. Pure text rewrite.
	d := newDirectoryWithAPI(nil, &fakeAPI{})
	got := d.Resolve(context.Background(),
		"check <http://grafana.example.com:3000/dash|Grafana> for more")
	assert.Equal(t, "check Grafana (http://grafana.example.com:3000/dash) for more", got)
}

func TestResolve_BareURL(t *testing.T) {
	d := newDirectoryWithAPI(nil, &fakeAPI{})
	got := d.Resolve(context.Background(), "see <https://example.com/foo>")
	assert.Equal(t, "see https://example.com/foo", got)
}

func TestResolve_MixedContent(t *testing.T) {
	// A message mixing user refs, a channel ref and escaped entities.
	pool := getDirDB(t)
	truncateDir(t, pool)

	api := &fakeAPI{
		users: map[string]*slack.User{
			"U0EXAMPLE02": makeUser("U0EXAMPLE02", "avery", "avery"),
			"U0EXAMPLE01": makeUser("U0EXAMPLE01", "jamie", "jamie"),
		},
		channels: map[string]*slack.Channel{
			"C0EXAMPLE04": {GroupConversation: slack.GroupConversation{Name: "ops-incidents"}},
		},
	}
	// Channel info returns a slack.Channel; the Name field is
	// nested on GroupConversation. Setting it inline above so
	// the fake returns the right shape.
	api.channels["C0EXAMPLE04"].ID = "C0EXAMPLE04"

	d := newDirectoryWithAPI(pool, api)
	got := d.Resolve(context.Background(),
		"<@U0EXAMPLE02> and <@U0EXAMPLE01> — see <#C0EXAMPLE04|incidents> and alpha-&gt;beta traffic")
	assert.Equal(t, "@avery and @jamie — see #ops-incidents and alpha->beta traffic", got)
}

func TestLookupChannelByName_CacheHit(t *testing.T) {
	// Cache pre-seeded with the channel; conversations.list never
	// gets called. Tests that the cache path short-circuits the
	// API walk.
	pool := getDirDB(t)
	truncateDir(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
		 VALUES ('C0123', 'eng-sandbox', false, false, now())`)
	require.NoError(t, err)

	api := &countingListAPI{fakeAPI: fakeAPI{}}
	d := newDirectoryWithAPI(pool, api)
	id, err := d.LookupChannelByName(context.Background(), "eng-sandbox")
	require.NoError(t, err)
	assert.Equal(t, "C0123", id)
	assert.Equal(t, int32(0), api.listCalls.Load(),
		"fresh cache hit must not call conversations.list")

	// Lookup with the leading `#` prefix should also work.
	id, err = d.LookupChannelByName(context.Background(), "#eng-sandbox")
	require.NoError(t, err)
	assert.Equal(t, "C0123", id)
}

func TestLookupChannelByName_FallsBackToConversationsList(t *testing.T) {
	// Cache is empty. Fake conversations.list returns a workspace
	// of 3 channels including the one we want.
	pool := getDirDB(t)
	truncateDir(t, pool)
	api := &countingListAPI{
		fakeAPI: fakeAPI{},
		channels: []slack.Channel{
			{GroupConversation: slack.GroupConversation{Name: "general"}},
			{GroupConversation: slack.GroupConversation{Name: "ops-incidents"}},
			{GroupConversation: slack.GroupConversation{Name: "eng-sandbox"}},
		},
	}
	api.channels[0].ID = "C001"
	api.channels[1].ID = "C002"
	api.channels[2].ID = "C003"
	d := newDirectoryWithAPI(pool, api)
	id, err := d.LookupChannelByName(context.Background(), "eng-sandbox")
	require.NoError(t, err)
	assert.Equal(t, "C003", id)
	assert.Equal(t, int32(1), api.listCalls.Load(),
		"miss should trigger exactly one conversations.list call")

	// Subsequent lookup must hit the cache populated by the walk.
	id, err = d.LookupChannelByName(context.Background(), "ops-incidents")
	require.NoError(t, err)
	assert.Equal(t, "C002", id)
	assert.Equal(t, int32(1), api.listCalls.Load(),
		"refresh-on-walk must populate the cache for unrelated channels too")
}

func TestLookupChannelByName_NotFound(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)
	api := &countingListAPI{
		fakeAPI: fakeAPI{},
		channels: []slack.Channel{
			{GroupConversation: slack.GroupConversation{Name: "general"}},
		},
	}
	api.channels[0].ID = "C001"
	d := newDirectoryWithAPI(pool, api)
	_, err := d.LookupChannelByName(context.Background(), "nonexistent")
	assert.ErrorIs(t, err, ErrChannelNotFound)
}

// countingListAPI extends fakeAPI with a list-call counter and a
// fixed channel slice for conversations.list. Used by the
// LookupChannelByName tests above.
type countingListAPI struct {
	fakeAPI
	channels  []slack.Channel
	listCalls atomic.Int32
}

func (c *countingListAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	c.listCalls.Add(1)
	// Return the full set in one page; production code handles
	// paging via nextCursor, but a single-page reply exercises
	// the same code path.
	return c.channels, "", nil
}

func TestSlackActorCanonical(t *testing.T) {
	// Pure unit test (no DB): SlackActorCanonical's three-way
	// behavior, including the explicit IsNegativeCache flag that
	// replaced the all-empty-fields sentinel.
	tests := []struct {
		name string
		ui   UserInfo
		want string
	}{
		{
			name: "email wins",
			ui:   UserInfo{UserID: "U01", Email: "a@example.com", Name: "alice"},
			want: "a@example.com",
		},
		{
			name: "no email falls back to slack:<id>",
			ui:   UserInfo{UserID: "U01", Name: "alice"},
			want: "slack:U01",
		},
		{
			name: "negative-cache row yields no canonical key",
			ui:   UserInfo{UserID: "U01", IsNegativeCache: true},
			want: "",
		},
		{
			name: "profile-less bot is NOT negative-cache: still gets slack:<id>",
			ui:   UserInfo{UserID: "U0BOT", IsBot: true},
			want: "slack:U0BOT",
		},
		{
			name: "empty everything yields empty",
			ui:   UserInfo{},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SlackActorCanonical(tc.ui))
		})
	}
}

func TestResolve_NoRefsLeavesTextAlone(t *testing.T) {
	d := newDirectoryWithAPI(nil, &fakeAPI{})
	got := d.Resolve(context.Background(), "plain text, no refs, no entities")
	assert.Equal(t, "plain text, no refs, no entities", got)
}

func TestUnescapeEntities_OrderMatters(t *testing.T) {
	// &amp;lt; should decode to &lt; not to < — the outer &
	// decodes last so we don't re-trigger the lt entity.
	got := unescapeEntities("&amp;lt;")
	assert.Equal(t, "&lt;", got)
	got = unescapeEntities("a &amp;&amp; b")
	assert.Equal(t, "a && b", got)
	// Empty + no entities are pass-through.
	assert.Equal(t, "", unescapeEntities(""))
	assert.Equal(t, "plain", unescapeEntities("plain"))
}

var _ = strings.Contains // keep imports tidy when editing
