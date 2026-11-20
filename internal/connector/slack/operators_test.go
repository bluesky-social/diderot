package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	slackgo "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

// operatorSlack answers users.lookupByEmail from members and records the
// lookups and the users each DM went to.
type operatorSlack struct {
	mu      sync.Mutex
	members map[string]string
	lookups []string
	dms     []string
}

func newOperatorSlack(t *testing.T, members map[string]string) (*operatorSlack, *slackgo.Client) {
	t.Helper()
	fake := &operatorSlack{members: members}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fake.mu.Lock()
		defer fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.lookupByEmail":
			email := r.PostForm.Get("email")
			fake.lookups = append(fake.lookups, email)
			if id, ok := fake.members[email]; ok {
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"` + id + `"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":false,"error":"users_not_found"}`))
		case "/conversations.open":
			fake.dms = append(fake.dms, r.PostForm.Get("users"))
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D1"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1700000000.000100"}`))
		}
	}))
	t.Cleanup(server.Close)
	return fake, slackgo.New("xoxb-test", slackgo.OptionAPIURL(server.URL+"/"))
}

func (f *operatorSlack) calls() (lookups, dms []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lookups...), append([]string(nil), f.dms...)
}

func operatorConsumer(api *slackgo.Client, logs *bytes.Buffer, emails ...string) *Consumer {
	c := &Consumer{api: api, logger: slog.New(slog.NewTextHandler(logs, nil))}
	return c.WithNotifyEmails(emails).WithNotifyEvents(true, true, true)
}

func insertSlackUsers(t *testing.T, pool *pgxpool.Pool, rows ...[3]any) {
	t.Helper()
	for _, row := range rows {
		_, err := pool.Exec(context.Background(), `INSERT INTO slack_users (user_id, email, is_bot) VALUES ($1, $2, $3)`, row[0], row[1], row[2])
		require.NoError(t, err)
	}
}

// TestOperatorIDsUseTheDirectoryThenSlack: a directory hit costs no Slack
// call, a miss asks users.lookupByEmail, and an email no member has is
// skipped with one warning per run while the others still resolve.
func TestOperatorIDsUseTheDirectoryThenSlack(t *testing.T) {
	ctx := context.Background()
	pool := getDirDB(t)
	truncateDir(t, pool)
	insertSlackUsers(t, pool,
		[3]any{"U_DIR", "Dir@Example.com", false},
		[3]any{"U_BOT", "bot@example.com", true})
	fake, api := newOperatorSlack(t, map[string]string{"api@example.com": "U_API"})
	var logs bytes.Buffer
	c := operatorConsumer(api, &logs, "dir@example.com", "gone@example.com", "api@example.com", "bot@example.com")
	c.pool = pool

	assert.Equal(t, []string{"U_DIR", "U_API"}, c.operatorIDs(ctx))
	assert.Equal(t, []string{"U_DIR", "U_API"}, c.operatorIDs(ctx))
	lookups, _ := fake.calls()
	assert.NotContains(t, lookups, "dir@example.com", "a directory hit needs no Slack call")
	assert.Contains(t, lookups, "api@example.com")
	assert.Contains(t, lookups, "bot@example.com", "a bot in the directory isn't an operator")
	assert.Equal(t, 1, strings.Count(logs.String(), "email=gone@example.com"), logs.String())
	assert.Equal(t, 1, strings.Count(logs.String(), "email=bot@example.com"), logs.String())
	assert.Equal(t, 2, strings.Count(logs.String(), "no member of this workspace has this email"))
}

// TestSendAskNotificationSkipsUnresolvableOperators: one email nobody in
// the workspace has doesn't stop the DM to the operator who is a member.
func TestSendAskNotificationSkipsUnresolvableOperators(t *testing.T) {
	fake, api := newOperatorSlack(t, map[string]string{"ops@example.com": "U_OPS"})
	var logs bytes.Buffer
	c := operatorConsumer(api, &logs, "gone@example.com", "ops@example.com")

	require.NoError(t, c.sendAskNotification(context.Background(), askNotification{
		Source: "HTTP", AskerEmail: "alice@example.com", Question: "status?",
	}, ""))
	lookups, dms := fake.calls()
	assert.Equal(t, []string{"gone@example.com", "ops@example.com"}, lookups)
	assert.Equal(t, []string{"U_OPS"}, dms)
	assert.Contains(t, logs.String(), "email=gone@example.com")
}

func TestSendAskNotificationSkipsExcludedAskersAndWatcherSelfDMs(t *testing.T) {
	fake, api := newOperatorSlack(t, map[string]string{"harper@example.com": "U_HARPER", "ops@example.com": "U_OPERATOR"})
	var logs bytes.Buffer
	c := operatorConsumer(api, &logs, "ops@example.com").WithNotifyExcludeEmails([]string{"watcher@example.com"})

	require.NoError(t, c.sendAskNotification(context.Background(), askNotification{
		Source: "Slack", AskerEmail: "Watcher@Example.com", Question: "status?",
	}, "U_WATCHER"))
	lookups, dms := fake.calls()
	assert.Empty(t, lookups, "an excluded asker needs no lookup")
	assert.Empty(t, dms)

	c.notifyEmails = []string{"harper@example.com"}
	require.NoError(t, c.sendAskNotification(context.Background(), askNotification{
		Source: "Slack", AskerEmail: "harper@example.com", Question: "status?",
	}, "U_HARPER"))
	_, dms = fake.calls()
	assert.Empty(t, dms, "the asker isn't told about their own question")

	// Anyone else still notifies the watchers.
	c.notifyEmails = []string{"ops@example.com"}
	require.NoError(t, c.sendAskNotification(context.Background(), askNotification{
		Source: "HTTP", AskerEmail: "alice@example.com", Question: "status?",
	}, ""))
	_, dms = fake.calls()
	assert.Equal(t, []string{"U_OPERATOR"}, dms)
}

func operatorsDraft(emails ...string) settings.Resolved {
	return settings.NewResolved(ConnectorName,
		settings.Values{FieldChannelMode: ChannelModeMember, FieldNotifyEmails: emails},
		map[string]string{FieldBotToken: fakeBotToken, FieldAppToken: fakeAppToken})
}

// TestSlackConnectorTestChecksOperators: the check passes when every email
// belongs to a member, through the directory or users.lookupByEmail, and
// otherwise names the emails that don't. It prints only those emails.
func TestSlackConnectorTestChecksOperators(t *testing.T) {
	ctx := context.Background()
	pool := getDirDB(t)
	truncateDir(t, pool)
	insertSlackUsers(t, pool, [3]any{"U_DIR", "dir@example.com", false})
	fake := newFakeSlack(t)
	fake.members = map[string]string{"api@example.com": "U_API"}
	connector := fake.connector()
	connector.Pool = pool

	report := connector.Test(ctx, operatorsDraft("dir@example.com", "api@example.com"))
	check := checks(report)["Operators"]
	assert.True(t, check.Passed, "%+v", check)
	assert.Equal(t, "Every email belongs to a member of this workspace.", check.Detail)
	assert.NotContains(t, fake.calls, "users.lookupByEmail dir@example.com")
	assert.Contains(t, fake.calls, "users.lookupByEmail api@example.com")

	report = connector.Test(ctx, operatorsDraft("dir@example.com", "gone@example.com", "api@example.com", "left@example.com"))
	check = checks(report)["Operators"]
	assert.False(t, check.Passed)
	assert.Equal(t, "No member of this workspace has gone@example.com, left@example.com.", check.Detail)
	assert.Equal(t, "Use the email of a member of this workspace.", check.Remedy)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, leak := range []string{"U_DIR", "U_API", "member-"} {
		assert.NotContains(t, string(encoded), leak, "the report names only the emails entered")
	}

	report = connector.Test(ctx, settings.NewResolved(ConnectorName,
		settings.Values{FieldChannelMode: ChannelModeMember, FieldNotifyEmails: []string{"dir@example.com"}},
		map[string]string{FieldBotToken: "xoxb-revoked", FieldAppToken: fakeAppToken}))
	check = checks(report)["Operators"]
	assert.False(t, check.Passed)
	assert.Equal(t, "Not checked: the bot token did not authenticate.", check.Detail)
}

func TestNotifyEventsChooseWhichDMsAreSent(t *testing.T) {
	question := askNotification{Source: "HTTP", Operation: "ask", AskerEmail: "alice@example.com", Question: "status?"}
	mcpAsk := askNotification{Source: "MCP", Operation: "ask", AskerEmail: "alice@example.com", Question: "status?"}
	search := askNotification{Source: "MCP", Operation: "retrieve", AskerEmail: "alice@example.com", Question: "status"}
	for name, tc := range map[string]struct {
		questions, searches bool
		n                   askNotification
		want                int
	}{
		"question on":           {true, false, question, 1},
		"question off":          {false, true, question, 0},
		"agent ask is question": {true, false, mcpAsk, 1},
		"search on":             {false, true, search, 1},
		"search off":            {true, false, search, 0},
	} {
		t.Run(name, func(t *testing.T) {
			fake, api := newOperatorSlack(t, map[string]string{"ops@example.com": "U_OPERATOR"})
			var logs bytes.Buffer
			c := operatorConsumer(api, &logs, "ops@example.com").WithNotifyEvents(tc.questions, tc.searches, true)
			require.NoError(t, c.sendAskNotification(context.Background(), tc.n, ""))
			_, dms := fake.calls()
			assert.Len(t, dms, tc.want)
		})
	}
}
