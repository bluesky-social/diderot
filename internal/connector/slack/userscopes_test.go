package slack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

// userScopePool returns a fresh pool against the slack package's
// isolated test DB, truncating slack_users so each test starts clean.
func userScopePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE slack_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedSlackUser(t *testing.T, pool *pgxpool.Pool, userID, email string, isBot bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO slack_users (user_id, email, is_bot)
		VALUES ($1, $2, $3)
	`, userID, email, isBot); err != nil {
		t.Fatalf("seed slack_users: %v", err)
	}
}

// fakeUserConvAPI is a page-driven fake for users.conversations with a
// call counter so tests can prove the cache short-circuits the API.
type fakeUserConvAPI struct {
	mu    sync.Mutex
	calls int

	// pages keyed by inbound cursor ("" = first page). Each entry is
	// (channels, nextCursor). Missing key with a non-nil err triggers
	// the error path on that cursor.
	pages map[string]fakeConvPage
	err   map[string]error // cursor -> error to return on that page
}

type fakeConvPage struct {
	channels []slack.Channel
	next     string
}

func (f *fakeUserConvAPI) GetConversationsForUserContext(_ context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if e, ok := f.err[params.Cursor]; ok && e != nil {
		return nil, "", e
	}
	p := f.pages[params.Cursor]
	return p.channels, p.next, nil
}

func (f *fakeUserConvAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestResolver(t *testing.T, pool *pgxpool.Pool, api userConvAPI) *UserScopeResolver {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// nil limiter: WithSlackRetry no-ops the pacing so tests don't pay
	// the tier-3 interval.
	return NewUserScopeResolver(api, nil, pool, logger)
}

func wantBaseline(t *testing.T, got []string) {
	t.Helper()
	want := []string{events.ScopePublic, events.ScopeInternal}
	if !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want baseline %v", got, want)
	}
}

func TestDMScopesAddsOwnerWithoutMutatingResolverCache(t *testing.T) {
	cached := []string{
		events.ScopePublic,
		events.ScopeInternal,
		events.ScopeChannel("G_PRIVATE"),
	}
	resolver := &fakeDMScopeResolver{scopes: cached}
	c := &Consumer{dmScopes: resolver}

	first := c.scopesForDM(context.Background(), " Avery@Example.com ")
	second := c.scopesForDM(context.Background(), "jane@example.com")

	wantFirst := append(append([]string(nil), cached...), events.ScopeOwner("avery@example.com"))
	wantSecond := append(append([]string(nil), cached...), events.ScopeOwner("jane@example.com"))
	if !slices.Equal(first, wantFirst) {
		t.Fatalf("first scopes = %v, want %v", first, wantFirst)
	}
	if !slices.Equal(second, wantSecond) {
		t.Fatalf("second scopes = %v, want %v", second, wantSecond)
	}
	if !slices.Equal(cached, []string{
		events.ScopePublic,
		events.ScopeInternal,
		events.ScopeChannel("G_PRIVATE"),
	}) {
		t.Fatalf("resolver cache mutated: %v", cached)
	}
}

// TestUserScopesEmptyEmail: empty email short-circuits to baseline
// without touching the DB or the API.
func TestUserScopesEmptyEmail(t *testing.T) {
	pool := userScopePool(t)
	api := &fakeUserConvAPI{}
	r := newTestResolver(t, pool, api)

	wantBaseline(t, r.ScopesForEmail(context.Background(), ""))
	wantBaseline(t, r.ScopesForEmail(context.Background(), "   "))
	if api.callCount() != 0 {
		t.Fatalf("empty email should not call the slack API; calls=%d", api.callCount())
	}
}

// TestUserScopesEmailMiss: no slack_users row for the email → baseline.
func TestUserScopesEmailMiss(t *testing.T) {
	pool := userScopePool(t)
	api := &fakeUserConvAPI{}
	r := newTestResolver(t, pool, api)

	wantBaseline(t, r.ScopesForEmail(context.Background(), "ghost@example.com"))
	if api.callCount() != 0 {
		t.Fatalf("email miss should not call the slack API; calls=%d", api.callCount())
	}
}

func TestUserScopesAskerForEmailReturnsStructuredSlackProfile(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UAVERY", "Avery@Example.com", false)
	if _, err := pool.Exec(context.Background(), `
		UPDATE slack_users
		   SET name='avery', display_name='Avery', real_name='Alex Rivera',
		       tz='America/Los_Angeles', tz_label='Pacific Daylight Time'
		 WHERE user_id='UAVERY'`); err != nil {
		t.Fatal(err)
	}
	r := newTestResolver(t, pool, &fakeUserConvAPI{})

	got, err := r.AskerForEmail(context.Background(), " avery@example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.DisplayName != "Alex Rivera" || got.Handle != "Avery" ||
		got.Email != "Avery@Example.com" || got.SlackUserID != "UAVERY" ||
		got.TZ != "America/Los_Angeles" || got.TZLabel != "Pacific Daylight Time" ||
		got.LocalTime.IsZero() {
		t.Fatalf("asker = %#v", got)
	}
}

func TestUserScopesAskerForEmailDoesNotGuessAmbiguousIdentity(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "U1", "dupe@example.com", false)
	seedSlackUser(t, pool, "U2", "dupe@example.com", false)
	r := newTestResolver(t, pool, &fakeUserConvAPI{})

	got, err := r.AskerForEmail(context.Background(), "dupe@example.com")
	if err == nil || got != nil {
		t.Fatalf("asker = %#v, err = %v; want ambiguity error", got, err)
	}
}

// TestUserScopesMultiRow: two slack_users rows share an email (a data
// bug) → ambiguous → baseline, no API call.
func TestUserScopesMultiRow(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "U1", "dupe@example.com", false)
	seedSlackUser(t, pool, "U2", "dupe@example.com", false)
	api := &fakeUserConvAPI{}
	r := newTestResolver(t, pool, api)

	wantBaseline(t, r.ScopesForEmail(context.Background(), "dupe@example.com"))
	if api.callCount() != 0 {
		t.Fatalf("ambiguous email should not call the slack API; calls=%d", api.callCount())
	}
}

// TestUserScopesBotRowIgnored: the only matching row is a bot →
// treated as no match → baseline.
func TestUserScopesBotRowIgnored(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UBOT", "bot@example.com", true)
	api := &fakeUserConvAPI{}
	r := newTestResolver(t, pool, api)

	wantBaseline(t, r.ScopesForEmail(context.Background(), "bot@example.com"))
}

// TestUserScopesCaseInsensitiveDBEmail: the slack_users row stores a
// MIXED-CASE email (as the ingest paths write it verbatim), while the
// OIDC caller presents any casing. lower(email) matching must still
// resolve the user and grant their private channels. A byte-exact
// compare would silently drop private scopes.
func TestUserScopesCaseInsensitiveDBEmail(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UMIXED", "Alex.Rivera@Example.com", false)
	api := &fakeUserConvAPI{
		pages: map[string]fakeConvPage{
			"": {channels: []slack.Channel{chanID("CPRIV1")}, next: ""},
		},
	}
	r := newTestResolver(t, pool, api)

	// Caller email is lowercased upstream; DB value is mixed case.
	got := r.ScopesForEmail(context.Background(), "alex.rivera@example.com")
	if !slices.Contains(got, events.ScopeChannel("CPRIV1")) {
		t.Fatalf("mixed-case DB email should still resolve to the user's private channel; got %v", got)
	}
}

// TestUserScopesHappyPathTwoPages: a real user with two pages of
// private channels → baseline ∪ {channel:<id>...}, cursor followed to
// exhaustion.
func TestUserScopesHappyPathTwoPages(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UAVERY", "avery@example.com", false)
	api := &fakeUserConvAPI{
		pages: map[string]fakeConvPage{
			"":     {channels: []slack.Channel{chanID("CPRIV1"), chanID("CPRIV2")}, next: "cur2"},
			"cur2": {channels: []slack.Channel{chanID("CPRIV3")}, next: ""},
		},
	}
	r := newTestResolver(t, pool, api)

	got := r.ScopesForEmail(context.Background(), "avery@example.com")
	want := []string{
		events.ScopePublic, events.ScopeInternal,
		events.ScopeChannel("CPRIV1"), events.ScopeChannel("CPRIV2"),
		events.ScopeChannel("CPRIV3"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	if api.callCount() != 2 {
		t.Fatalf("two pages => 2 API calls; got %d", api.callCount())
	}
}

// TestUserScopesMidPaginationError: an error on page 2 discards the
// whole walk → baseline, and NOTHING is cached (the next call retries
// the API rather than serving a pinned under-grant).
func TestUserScopesMidPaginationError(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UAVERY", "avery@example.com", false)
	api := &fakeUserConvAPI{
		pages: map[string]fakeConvPage{
			"": {channels: []slack.Channel{chanID("CPRIV1")}, next: "cur2"},
		},
		err: map[string]error{"cur2": errors.New("slack 500")},
	}
	r := newTestResolver(t, pool, api)

	wantBaseline(t, r.ScopesForEmail(context.Background(), "avery@example.com"))
	callsAfterFirst := api.callCount()
	if callsAfterFirst < 2 {
		t.Fatalf("expected page1 + page2 attempts; got %d", callsAfterFirst)
	}
	// Nothing cached: a second call hits the API again (would be a
	// cache hit with the same call count otherwise).
	wantBaseline(t, r.ScopesForEmail(context.Background(), "avery@example.com"))
	if api.callCount() <= callsAfterFirst {
		t.Fatalf("mid-pagination failure must NOT cache; second call should retry API (calls %d -> %d)",
			callsAfterFirst, api.callCount())
	}
}

// TestUserScopesCacheHit: a successful resolution is cached for TTL —
// a second call within the window makes no additional API calls.
func TestUserScopesCacheHit(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UAVERY", "avery@example.com", false)
	api := &fakeUserConvAPI{
		pages: map[string]fakeConvPage{
			"": {channels: []slack.Channel{chanID("CPRIV1")}, next: ""},
		},
	}
	r := newTestResolver(t, pool, api)

	first := r.ScopesForEmail(context.Background(), "avery@example.com")
	if !slices.Contains(first, events.ScopeChannel("CPRIV1")) {
		t.Fatalf("first call missing CPRIV1: %v", first)
	}
	callsAfterFirst := api.callCount()
	// Second call within TTL: cache hit, no new API call.
	second := r.ScopesForEmail(context.Background(), "avery@example.com")
	if !slices.Equal(first, second) {
		t.Fatalf("cache returned a different set: %v vs %v", first, second)
	}
	if api.callCount() != callsAfterFirst {
		t.Fatalf("cache hit should make no API call; calls %d -> %d", callsAfterFirst, api.callCount())
	}
	// Case-insensitive cache key: mixed-case email hits the same entry.
	_ = r.ScopesForEmail(context.Background(), "Avery@Example.com")
	if api.callCount() != callsAfterFirst {
		t.Fatalf("mixed-case email should hit the same cache entry; calls %d -> %d",
			callsAfterFirst, api.callCount())
	}
}

// TestUserScopesExpiredThenAPIError: once the cached entry expires, a
// refresh that fails returns baseline — the stale private grant is NOT
// re-served (revocation path; no stale-serving).
func TestUserScopesExpiredThenAPIError(t *testing.T) {
	pool := userScopePool(t)
	seedSlackUser(t, pool, "UAVERY", "avery@example.com", false)
	api := &fakeUserConvAPI{
		pages: map[string]fakeConvPage{
			"": {channels: []slack.Channel{chanID("CPRIV1")}, next: ""},
		},
	}
	r := newTestResolver(t, pool, api)

	// Drive the clock manually.
	base := time.Unix(1_700_000_000, 0)
	current := base
	r.now = func() time.Time { return current }

	first := r.ScopesForEmail(context.Background(), "avery@example.com")
	if !slices.Contains(first, events.ScopeChannel("CPRIV1")) {
		t.Fatalf("first call missing CPRIV1: %v", first)
	}

	// Advance past TTL and break the API. The expired entry must not be
	// served; the failed refresh degrades to baseline.
	current = base.Add(userScopeTTL + time.Second)
	api.mu.Lock()
	api.err = map[string]error{"": errors.New("slack down")}
	api.mu.Unlock()

	wantBaseline(t, r.ScopesForEmail(context.Background(), "avery@example.com"))
}

// chanID builds a slack.Channel with just the id set — all the
// resolver reads.
func chanID(id string) slack.Channel {
	var c slack.Channel
	c.ID = id
	return c
}
