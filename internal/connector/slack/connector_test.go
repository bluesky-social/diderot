package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/bluesky-social/diderot/internal/settings"
)

const (
	fakeBotToken  = "xoxb-fake-bot-token"
	fakeAppToken  = "xapp-fake-app-token"
	fakeUserToken = "xoxp-fake-user-token"
	fakeOtherUser = "xoxp-other-workspace"
	fakeMessage   = "a private message nobody should see in a report"
)

// fakeSlack serves the Web API calls Test makes and a Socket Mode
// endpoint that says hello.
type fakeSlack struct {
	server *httptest.Server
	scopes []string
	calls  []string
	// members maps the emails users.lookupByEmail knows to user IDs.
	members map[string]string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	fake := &fakeSlack{scopes: append(slices.Clone(requiredBotScopes), "channels:join")}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	token := func(r *http.Request) string {
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			return bearer
		}
		return r.FormValue("token")
	}
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "auth.test")
		switch token(r) {
		case fakeBotToken:
			w.Header().Set("X-OAuth-Scopes", strings.Join(fake.scopes, ","))
			reply(w, map[string]any{"ok": true, "url": "https://example.slack.com/", "team": "Example", "team_id": "T1",
				"user": "diderot", "user_id": "UBOT", "bot_id": "B1"})
		case fakeUserToken:
			reply(w, map[string]any{"ok": true, "url": "https://example.slack.com/", "team": "Example", "team_id": "T1",
				"user": "alice", "user_id": "UALICE"})
		case fakeOtherUser:
			reply(w, map[string]any{"ok": true, "team_id": "T2", "user": "mallory", "user_id": "UM"})
		default:
			reply(w, map[string]any{"ok": false, "error": "invalid_auth"})
		}
	})
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "apps.connections.open")
		if token(r) != fakeAppToken {
			reply(w, map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		reply(w, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(fake.server.URL, "http") + "/socket?ticket=t"})
	})
	upgrader := websocket.Upgrader{}
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "socket")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteJSON(map[string]any{"type": "hello"})
		_, _, _ = conn.ReadMessage()
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "conversations.list")
		if token(r) != fakeBotToken {
			reply(w, map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		reply(w, map[string]any{"ok": true, "channels": []map[string]any{
			{"id": "COTHER", "name": "other", "is_member": false},
			{"id": "CJOINED", "name": "joined", "is_member": true},
		}})
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "conversations.history "+r.FormValue("channel")+" "+r.FormValue("limit"))
		reply(w, map[string]any{"ok": true, "messages": []map[string]any{
			{"type": "message", "user": "U1", "text": fakeMessage, "ts": "1700000000.000100"},
			{"type": "message", "user": "U2", "text": "<@U1> hi", "ts": "1700000001.000100", "thread_ts": "1700000000.000100"},
			{"type": "message", "subtype": "channel_join", "user": "U3", "text": "joined", "ts": "1700000002.000100"},
		}})
	})
	mux.HandleFunc("/users.lookupByEmail", func(w http.ResponseWriter, r *http.Request) {
		fake.calls = append(fake.calls, "users.lookupByEmail "+r.FormValue("email"))
		if id, ok := fake.members[r.FormValue("email")]; ok {
			reply(w, map[string]any{"ok": true, "user": map[string]any{"id": id, "name": "member-" + id}})
			return
		}
		reply(w, map[string]any{"ok": false, "error": "users_not_found"})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeSlack) connector() Connector {
	return Connector{APIURL: f.server.URL + "/", HTTP: f.server.Client()}
}

func slackDraft(bot, app, user, mode string) settings.Resolved {
	secrets := map[string]string{FieldBotToken: bot, FieldAppToken: app}
	if user != "" {
		secrets[FieldUserToken] = user
	}
	return settings.NewResolved(ConnectorName, settings.Values{FieldChannelMode: mode}, secrets)
}

func checks(report settings.TestReport) map[string]settings.Check {
	out := map[string]settings.Check{}
	for _, check := range report.Checks {
		out[check.Name] = check
	}
	return out
}

func TestSlackConnectorTestPassesAgainstAFake(t *testing.T) {
	fake := newFakeSlack(t)
	report := fake.connector().Test(context.Background(), slackDraft(fakeBotToken, fakeAppToken, fakeUserToken, ChannelModeMember))
	require.True(t, report.Passed(), "%+v", report)
	got := checks(report)
	assert.Len(t, got, 7)
	assert.Equal(t, "Not set: no one gets a DM.", got["Operators"].Detail)
	assert.Contains(t, got["Bot token"].Detail, "Workspace Example (example.slack.com)")
	assert.Equal(t, "Parsed the last 3 messages of a joined channel without writing them.", got["Message parsing"].Detail)
	assert.Contains(t, got["Channels"].Detail, "sees 2 channels")
	assert.Contains(t, got["Channels"].Detail, "belongs to 1")
	assert.Contains(t, got["User token"].Detail, "as alice")
	name, _ := report.Fact(FieldWorkspaceName)
	host, _ := report.Fact(FieldWorkspaceHost)
	assert.Equal(t, "Example", name)
	assert.Equal(t, "example.slack.com", host)
	assert.Contains(t, fake.calls, "socket", "Socket Mode connects, then closes")
	assert.Contains(t, fake.calls, "conversations.history CJOINED 20")

	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, secret := range []string{fakeBotToken, fakeAppToken, fakeUserToken, fakeMessage, "ticket"} {
		assert.NotContains(t, string(encoded), secret, "the report carries counts, never tokens or message text")
	}
}

func TestSlackConnectorTestFailures(t *testing.T) {
	fake := newFakeSlack(t)
	connector := fake.connector()

	report := connector.Test(context.Background(), slackDraft("xoxb-revoked", "xapp-revoked", "", ChannelModeMember))
	got := checks(report)
	assert.False(t, report.Passed())
	assert.Equal(t, "auth.test: Slack answered invalid_auth", got["Bot token"].Detail)
	assert.Equal(t, "Paste the bot token (xoxb-) from OAuth & Permissions, and reinstall the app if it was revoked.", got["Bot token"].Remedy)
	assert.Contains(t, got["App token and Socket Mode"].Detail, "invalid_auth")
	fromEnv := checks(connector.Test(context.Background(), slackDraft("xoxb-revoked", "xapp-revoked", "", ChannelModeMember).
		WithSecretEnv(FieldBotToken, "DIDEROT_SLACK_BOT_TOKEN").WithSecretEnv(FieldAppToken, "DIDEROT_SLACK_APP_TOKEN")))
	assert.Equal(t, "Set DIDEROT_SLACK_BOT_TOKEN in the deployment to the bot token (xoxb-) from OAuth & Permissions, "+
		"and reinstall the app if it was revoked.", fromEnv["Bot token"].Remedy, "the remedy says where the token comes from")
	assert.Equal(t, "Set DIDEROT_SLACK_APP_TOKEN in the deployment to an app-level token (xapp-) with connections:write, "+
		"and turn on Socket Mode in the app settings.", fromEnv["App token and Socket Mode"].Remedy)
	assert.False(t, got["Channels"].Passed)
	assert.Equal(t, "Not checked: the bot token did not authenticate.", got["Channels"].Detail,
		"a bad token must not be reported as missing scopes")
	assert.NotContains(t, got, "Message parsing", "nothing to parse without channels")
	assert.True(t, got["User token"].Passed, "an unset user token only turns invites off")

	fake.scopes = slices.DeleteFunc(slices.Clone(requiredBotScopes), func(scope string) bool { return scope == "reactions:write" })
	report = connector.Test(context.Background(), slackDraft(fakeBotToken, fakeAppToken, fakeOtherUser, ChannelModeAllPublic))
	got = checks(report)
	assert.Equal(t, "Missing reactions:write, channels:join.", got["Bot scopes"].Detail)
	assert.False(t, got["User token"].Passed)
	assert.Contains(t, got["User token"].Detail, "another workspace")

	report = connector.Test(context.Background(), slackDraft(fakeUserToken, fakeAppToken, fakeBotToken, ChannelModeMember))
	got = checks(report)
	assert.Equal(t, "Slack says this is not a bot token.", got["Bot token"].Detail)
	assert.Equal(t, "Slack says this is a bot token.", got["User token"].Detail)
}

// Index documents keeps every attachment in file storage; on a deployment
// without it the setting would pass its Test and extract nothing, so the Test
// fails it.
func TestSlackConnectorTestChecksDocumentIngestStorage(t *testing.T) {
	fake := newFakeSlack(t)
	connector := fake.connector()
	draft := func(docIngest bool) settings.Resolved {
		return settings.NewResolved(ConnectorName,
			settings.Values{FieldChannelMode: ChannelModeMember, FieldDocIngest: docIngest},
			map[string]string{FieldBotToken: fakeBotToken, FieldAppToken: fakeAppToken})
	}
	report := connector.Test(context.Background(), draft(true))
	assert.False(t, report.Passed())
	docs := checks(report)["Index documents"]
	assert.False(t, docs.Passed)
	assert.Equal(t, "Indexing documents needs file storage, and this deployment has none.", docs.Detail)
	assert.Contains(t, docs.Remedy, "DIDEROT_IMAGES_BUCKET")

	connector.FileStorage = true
	report = connector.Test(context.Background(), draft(true))
	require.True(t, report.Passed(), "%+v", report)
	assert.True(t, checks(report)["Index documents"].Passed)

	connector.FileStorage = false
	report = connector.Test(context.Background(), draft(false))
	require.True(t, report.Passed(), "%+v", report)
	assert.NotContains(t, checks(report), "Index documents", "off, it needs nothing")
}

// TestManifestScopesAreTheOnesUsed keeps slack-app-manifest.yaml to the
// scopes some code path needs: the bot scopes Test requires, and the user
// scopes of the Inviter's calls (conversations.invite, .join, .leave, and
// .kick), the user token's only use. A scope nothing uses only widens what a
// leaked token can do; one the code uses and the manifest lacks fails that
// call on every install.
func TestManifestScopesAreTheOnesUsed(t *testing.T) {
	raw, err := os.ReadFile("../../../slack-app-manifest.yaml")
	require.NoError(t, err)
	var manifest struct {
		OAuth struct {
			Scopes struct {
				Bot  []string `yaml:"bot"`
				User []string `yaml:"user"`
			} `yaml:"scopes"`
		} `yaml:"oauth_config"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &manifest))
	want := slices.Clone(manifest.OAuth.Scopes.Bot)
	got := append(slices.Clone(requiredBotScopes), "channels:join")
	slices.Sort(want)
	slices.Sort(got)
	assert.Equal(t, want, got)
	// The Directory names mentioned user groups through usergroups.list.
	assert.Contains(t, manifest.OAuth.Scopes.Bot, "usergroups:read")
	assert.ElementsMatch(t, []string{"channels:write", "groups:write"}, manifest.OAuth.Scopes.User)
}

// TestManifestFeaturesAreTheOnesHandled keeps the manifest's events and
// shortcuts to what the consumer handles, within the limits Slack's manifest
// reference sets, so the file pastes into Create app from manifest.
func TestManifestFeaturesAreTheOnesHandled(t *testing.T) {
	raw, err := os.ReadFile("../../../slack-app-manifest.yaml")
	require.NoError(t, err)
	var manifest struct {
		Display struct {
			Name            string `yaml:"name"`
			Description     string `yaml:"description"`
			LongDescription string `yaml:"long_description"`
		} `yaml:"display_information"`
		Features struct {
			AppHome struct {
				Messages bool  `yaml:"messages_tab_enabled"`
				Tasks    *bool `yaml:"agent_tasks_enabled"`
			} `yaml:"app_home"`
			AgentView struct {
				Description string `yaml:"agent_description"`
				Prompts     []struct {
					Title   string `yaml:"title"`
					Message string `yaml:"message"`
				} `yaml:"suggested_prompts"`
			} `yaml:"agent_view"`
			Shortcuts []struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
				Type        string `yaml:"type"`
				CallbackID  string `yaml:"callback_id"`
			} `yaml:"shortcuts"`
		} `yaml:"features"`
		Settings struct {
			Events struct {
				Bot []string `yaml:"bot_events"`
			} `yaml:"event_subscriptions"`
			SocketMode bool `yaml:"socket_mode_enabled"`
		} `yaml:"settings"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &manifest))
	assert.True(t, manifest.Settings.SocketMode)
	// handleEventsAPIWithRetry's cases: messages of every conversation
	// type, reactions (feedback), membership changes, the stop button, and
	// mentions (for the one a channel's messages don't carry). The
	// subscription to app_context_changed is what puts app_context on a
	// direct message; the event itself needs no handling.
	assert.ElementsMatch(t, []string{
		"message.channels", "message.groups", "message.im", "message.mpim",
		"reaction_added", "reaction_removed", "member_joined_channel", "member_left_channel",
		"agent_session_stopped", "app_mention", "app_context_changed",
	}, manifest.Settings.Events.Bot)
	var callbacks []string
	for _, shortcut := range manifest.Features.Shortcuts {
		assert.Equal(t, "message", shortcut.Type, shortcut.CallbackID)
		assert.LessOrEqual(t, len([]rune(shortcut.Name)), 24, shortcut.Name)
		assert.LessOrEqual(t, len([]rune(shortcut.Description)), 150, shortcut.CallbackID)
		callbacks = append(callbacks, shortcut.CallbackID)
	}
	assert.ElementsMatch(t, []string{summarizeCallbackID, summarizePrivateCallbackID}, callbacks)
	assert.NotEmpty(t, manifest.Display.Name)
	assert.LessOrEqual(t, len([]rune(manifest.Display.Name)), 35)
	assert.LessOrEqual(t, len([]rune(manifest.Display.Description)), 140)
	assert.GreaterOrEqual(t, len([]rune(manifest.Display.LongDescription)), 175)
	assert.NotEmpty(t, manifest.Features.AgentView.Description, "Slack's manifest reference requires it with agent_view")
	assert.LessOrEqual(t, len([]rune(manifest.Features.AgentView.Description)), 300)
	// Slack takes up to four suggested prompts; choosing one sends its
	// message as written, so each is a whole question.
	assert.NotEmpty(t, manifest.Features.AgentView.Prompts)
	assert.LessOrEqual(t, len(manifest.Features.AgentView.Prompts), 4)
	for _, prompt := range manifest.Features.AgentView.Prompts {
		assert.NotEmpty(t, prompt.Title)
		assert.True(t, strings.HasSuffix(prompt.Message, "?") || strings.HasSuffix(prompt.Message, "."), prompt.Message)
	}
	// Direct messages to the bot arrive through its Messages tab.
	assert.True(t, manifest.Features.AppHome.Messages)
	// Slack turns an agent's Tasks tab on unless the manifest says false, and
	// nothing in Diderot takes the recurring work people would give it there.
	require.NotNil(t, manifest.Features.AppHome.Tasks, "omitting agent_tasks_enabled turns the Tasks tab on")
	assert.False(t, *manifest.Features.AppHome.Tasks)
}

func TestSlackConnectorFieldsAndValidation(t *testing.T) {
	require.NotPanics(t, func() { settings.NewRegistry(Connector{}) })
	values := settings.Values{
		FieldNotifyEmails:        []string{" Ops@Example.COM ", "ops@example.com", "oncall@example.co.uk"},
		FieldNotifyExcludeEmails: []string{"Watcher@Example.com"},
	}
	require.NoError(t, Connector{}.Validate(values))
	assert.Equal(t, []string{"ops@example.com", "oncall@example.co.uk"}, values[FieldNotifyEmails], "lowercased, trimmed, deduplicated")
	assert.Equal(t, []string{"watcher@example.com"}, values[FieldNotifyExcludeEmails])
	for _, bad := range []string{"U0123ABC", "alice", "alice@example", "a b@example.com", "Alice <alice@example.com>", "a@b@example.com"} {
		err := Connector{}.Validate(settings.Values{FieldNotifyEmails: []string{"ok@example.com", bad}})
		var fieldErrs settings.FieldErrors
		require.ErrorAs(t, err, &fieldErrs, bad)
		assert.Equal(t, fmt.Sprintf("%q is not an email address", bad), fieldErrs[FieldNotifyEmails], "named as entered")
	}
	err := Connector{}.Validate(settings.Values{FieldNotifyExcludeEmails: []string{"nope"}})
	var fieldErrs settings.FieldErrors
	require.ErrorAs(t, err, &fieldErrs)
	assert.Contains(t, fieldErrs[FieldNotifyExcludeEmails], `"nope"`)

	s := SettingsFrom(settings.NewResolved(ConnectorName, settings.Values{
		FieldChannelMode: ChannelModeAllPublic, FieldDocIngest: true, FieldWorkspaceHost: "example.slack.com",
		FieldNotifyEmails: []string{"ops@example.com"},
	}, map[string]string{FieldBotToken: fakeBotToken, FieldAppToken: fakeAppToken}))
	assert.Equal(t, fakeBotToken, s.BotToken)
	assert.True(t, s.DocIngest)
	assert.Equal(t, []string{"ops@example.com"}, s.NotifyEmails)
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		assert.NotContains(t, fmt.Sprintf(verb, s), "xoxb", verb)
	}
}
