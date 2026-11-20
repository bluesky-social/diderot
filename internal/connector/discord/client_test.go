package discord

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

func TestRouteKeyKeepsOnlyTheMajorID(t *testing.T) {
	assert.Equal(t, "GET /channels/200000000000000001/messages/:id",
		routeKey("GET", "/channels/200000000000000001/messages/300000000000000001"))
	assert.Equal(t, "GET /users/@me/guilds", routeKey("GET", "/users/@me/guilds"))
}

func TestClientWaitsOutARateLimit(t *testing.T) {
	f := newFakeDiscord(t)
	f.mu.Lock()
	f.limited["/channels/"+fakeText+"/messages"] = 2
	f.mu.Unlock()
	client := NewClient(f.apiBase(), "test-token")
	start := time.Now()
	var pages int
	err := client.messagesBefore(t.Context(), fakeText, "", func([]message) (bool, error) { pages++; return false, nil })
	require.NoError(t, err)
	assert.Equal(t, 1, pages)
	assert.GreaterOrEqual(t, time.Since(start), 90*time.Millisecond, "two 429s of 50ms each are waited out")
	assert.Equal(t, 3, f.requestCount("/channels/"+fakeText+"/messages"))
}

func TestClientGivesUpOnPersistentRateLimit(t *testing.T) {
	f := newFakeDiscord(t)
	f.mu.Lock()
	f.limited["/users/@me"] = maxAttempts + 1
	f.mu.Unlock()
	_, err := NewClient(f.apiBase(), "test-token").currentUser(t.Context())
	require.Error(t, err)
	assert.True(t, isStatus(err, 429))
}

func TestClientReportsStatusWithoutBody(t *testing.T) {
	f := newFakeDiscord(t)
	_, err := NewClient(f.apiBase(), "wrong-token").currentUser(t.Context())
	require.Error(t, err)
	assert.True(t, isStatus(err, 401))
	assert.NotContains(t, err.Error(), "Unauthorized")
	assert.NotContains(t, err.Error(), "wrong-token")
}

func TestClientRetriesServerErrors(t *testing.T) {
	f := newFakeDiscord(t)
	f.fail("/users/@me", 502)
	u, err := NewClient(f.apiBase(), "test-token").currentUser(t.Context())
	require.NoError(t, err)
	assert.Equal(t, fakeBot, u.ID)
}

func TestClientHonoursAnEmptyBucket(t *testing.T) {
	c := NewClient("http://unused.invalid", "t")
	c.observe("GET /x", map[string][]string{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"0.2"}})
	start := time.Now()
	require.NoError(t, c.waitRoute(t.Context(), "GET /x"))
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.blockRoute("GET /y", time.Now().Add(time.Hour))
	assert.ErrorIs(t, c.waitRoute(ctx, "GET /y"), context.Canceled)
}

func TestMessagesAfterReadsOldestFirst(t *testing.T) {
	f := newFakeDiscord(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 250 {
		f.addMessage(fakeText, msg(fakeID(base.Add(time.Duration(i)*time.Minute), 0), alice, base, "m"+strconv.Itoa(i)))
	}
	var got []string
	err := NewClient(f.apiBase(), "test-token").messagesAfter(t.Context(), fakeText, fakeID(base.Add(99*time.Minute), 0), func(page []message) error {
		for _, m := range page {
			got = append(got, m.Content)
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, got, 150)
	assert.Equal(t, "m100", got[0])
	assert.Equal(t, "m249", got[149])
}

func TestChannelPermissions(t *testing.T) {
	roles := map[string]int64{fakeEveryone: permViewChannel | permReadMessageHistory, fakeRole: 0}
	hide := []overwrite{{ID: fakeEveryone, Type: 0, Deny: strconv.FormatInt(permViewChannel, 10), Allow: "0"}}
	assert.NotZero(t, channelPermissions(fakeGuild, roles, nil, "", nil)&permViewChannel)
	assert.Zero(t, channelPermissions(fakeGuild, roles, nil, "", hide)&permViewChannel)
	// A role allow beats an @everyone deny; a member deny beats a role allow.
	withRole := append(hide, overwrite{ID: fakeRole, Type: 0, Allow: strconv.FormatInt(permViewChannel, 10), Deny: "0"})
	assert.NotZero(t, channelPermissions(fakeGuild, roles, []string{fakeRole}, fakeBot, withRole)&permViewChannel)
	memberDeny := append(withRole, overwrite{ID: fakeBot, Type: 1, Deny: strconv.FormatInt(permViewChannel, 10), Allow: "0"})
	assert.Zero(t, channelPermissions(fakeGuild, roles, []string{fakeRole}, fakeBot, memberDeny)&permViewChannel)
	// Administrator sees everything.
	roles[fakeRole] = permAdministrator
	assert.NotZero(t, channelPermissions(fakeGuild, roles, []string{fakeRole}, fakeBot, memberDeny)&permViewChannel)
}

func TestReadDirectory(t *testing.T) {
	f := newFakeDiscord(t)
	dir, err := readDirectory(t.Context(), NewClient(f.apiBase(), "test-token"))
	require.NoError(t, err)
	assert.Equal(t, "diderot-test", dir.Bot)
	require.Len(t, dir.Guilds, 1)
	g := dir.Guilds[0]
	assert.Equal(t, "Example Server", g.Name)
	assert.Equal(t, "maintainers", g.Roles[fakeRole])
	var names []string
	for _, c := range g.Channels {
		names = append(names, c.Name)
	}
	assert.ElementsMatch(t, []string{"general", "help", "staff"}, names, "voice channels and categories are not listed")
	_, general, _ := dir.channel(fakeText)
	assert.Equal(t, "Projects", general.Category)
	assert.False(t, general.Private)
	assert.True(t, general.Readable)
	_, staff, _ := dir.channel(fakePrivate)
	assert.True(t, staff.Private)
	assert.True(t, staff.Readable, "a member overwrite lets the bot in")
	_, help, _ := dir.channel(fakeForum)
	assert.Equal(t, "forum", help.Kind())
}

func TestConnectorTest(t *testing.T) {
	f := newFakeDiscord(t)
	c := Connector{APIBase: f.apiBase()}
	resolve := func(token string, docs bool) settings.Resolved {
		return settings.NewResolved(ConnectorName, settings.Values{FieldHistoryDays: int64(0), FieldDocIngest: docs}, map[string]string{FieldBotToken: token})
	}
	report := c.Test(t.Context(), resolve("test-token", false))
	require.True(t, report.Passed(), "%+v", report)
	assert.Equal(t, "Signed in as diderot-test.", report.Checks[0].Detail)
	assert.Equal(t, "The bot is in 1 server: Example Server.", report.Checks[2].Detail)

	report = c.Test(t.Context(), resolve("bad-token", false))
	assert.False(t, report.Passed())
	assert.NotContains(t, report.Checks[0].Detail, "bad-token")

	f.mu.Lock()
	f.intentOK = false
	f.mu.Unlock()
	report = c.Test(t.Context(), resolve("test-token", false))
	assert.False(t, report.Passed())
	assert.Contains(t, report.Checks[1].Remedy, "Message Content Intent")
	f.mu.Lock()
	f.intentOK = true
	f.mu.Unlock()

	report = c.Test(t.Context(), resolve("test-token", true))
	assert.False(t, report.Passed(), "documents need file storage")
	report = Connector{APIBase: f.apiBase(), FileStorage: true}.Test(t.Context(), resolve("test-token", true))
	assert.True(t, report.Passed())

	report = c.Test(t.Context(), resolve("", false))
	assert.False(t, report.Passed())
}

func TestValidate(t *testing.T) {
	c := Connector{}
	assert.NoError(t, c.Validate(settings.Values{FieldHistoryDays: int64(30)}))
	assert.Error(t, c.Validate(settings.Values{FieldHistoryDays: int64(-1)}))
	assert.Error(t, c.Validate(settings.Values{FieldHistoryDays: int64(maxHistoryDays + 1)}))
}
