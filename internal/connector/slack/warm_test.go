package slack

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

// warmFakeAPI is a minimal warmAPI for these tests — returns a canned
// slice of users and a two-page channel listing so we exercise the
// cursor loop.
type warmFakeAPI struct {
	users      []slack.User
	channelsP1 []slack.Channel
	channelsP2 []slack.Channel

	// rateLimitOnce injects a RateLimitedError on the first call so
	// we exercise the retry path.
	rateLimitOnce bool
	rlCalls       int
}

func (w *warmFakeAPI) GetUsersContext(_ context.Context, _ ...slack.GetUsersOption) ([]slack.User, error) {
	return w.users, nil
}

func (w *warmFakeAPI) GetConversationsContext(_ context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	if w.rateLimitOnce && w.rlCalls == 0 {
		w.rlCalls++
		return nil, "", &slack.RateLimitedError{RetryAfter: 10 * time.Millisecond}
	}
	if params.Cursor == "" {
		return w.channelsP1, "cur2", nil
	}
	return w.channelsP2, "", nil
}

func getWarmPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE slack_users, slack_bots, slack_channel_info, slack_usergroups
		 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func chanWith(id, name string, private, archived bool) slack.Channel {
	c := slack.Channel{}
	c.ID = id
	c.Name = name
	c.IsPrivate = private
	c.IsArchived = archived
	return c
}

func TestWarmUsersPopulatesCache(t *testing.T) {
	pool := getWarmPool(t)
	api := &warmFakeAPI{
		users: []slack.User{
			{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice", Email: "alice@example.com"}, RealName: "Alice Example"},
			{ID: "U2", Name: "bot-deploy", IsBot: true},
		},
	}
	n, err := WarmUsers(context.Background(), api, nil, pool)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wrote %d users, want 2", n)
	}

	// Directory.LookupUser should now hit the cache — fakeAPI from
	// directory_test.go is wired with empty users map so a call-through
	// would return user_not_found. A cache hit means it doesn't go.
	dirAPI := &fakeAPI{users: map[string]*slack.User{}}
	dir := newDirectoryWithAPI(pool, dirAPI)
	name, err := dir.LookupUser(context.Background(), "U1")
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if name != "Alice" {
		t.Errorf("LookupUser got %q, want Alice (from warm cache)", name)
	}
	if dirAPI.userCalls.Load() != 0 {
		t.Errorf("warm should mean zero API calls; got %d", dirAPI.userCalls.Load())
	}
}

func TestWarmChannelsPaginatesAndRateLimits(t *testing.T) {
	pool := getWarmPool(t)
	api := &warmFakeAPI{
		channelsP1: []slack.Channel{
			chanWith("C1", "ops-incidents", true, false),
			chanWith("C2", "team-eng", false, false),
		},
		channelsP2: []slack.Channel{
			chanWith("C3", "random", false, false),
		},
		rateLimitOnce: true,
	}
	n, err := WarmChannels(context.Background(), api, nil, pool)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("wrote %d channels, want 3 (across 2 pages + 1 rate-limit retry)", n)
	}

	// Spot-check directory hit.
	dirAPI := &fakeAPI{channels: map[string]*slack.Channel{}}
	dir := newDirectoryWithAPI(pool, dirAPI)
	name, err := dir.LookupChannel(context.Background(), "C1")
	if err != nil {
		t.Fatalf("LookupChannel: %v", err)
	}
	if name != "ops-incidents" {
		t.Errorf("got %q", name)
	}
	if dirAPI.chanCalls.Load() != 0 {
		t.Errorf("warm-then-lookup should be zero API calls; got %d", dirAPI.chanCalls.Load())
	}
}
