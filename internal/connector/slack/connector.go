package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/safehttp"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// ConnectorName is the Slack connector's connector_settings row.
const ConnectorName = "slack"

// Slack connector fields.
const (
	FieldBotToken            = "bot_token"
	FieldAppToken            = "app_token"
	FieldUserToken           = "user_token"
	FieldChannelMode         = "channel_mode"
	FieldNotifyEmails        = "notify_emails"
	FieldNotifyQuestions     = "notify_questions"
	FieldNotifySearches      = "notify_searches"
	FieldNotifyChannels      = "notify_channels"
	FieldNotifyExcludeEmails = "notify_exclude_emails"
	FieldDocIngest           = "doc_ingest"
	FieldWorkspaceName       = "workspace_name"
	FieldWorkspaceHost       = "workspace_host"
)

// Channel modes.
const (
	ChannelModeMember    = "member"
	ChannelModeAllPublic = "all-public"
)

// testSampleSize is how many recent messages Test parses.
const testSampleSize = 20

// requiredBotScopes are the bot scopes the code uses: slack-app-manifest.yaml
// grants them (a test keeps the two in step). channels:join is needed only
// in all-public mode. usergroups:read is usergroups.list, which names the
// user groups that messages mention (Directory.LookupUsergroup). commands
// is what Slack requires of an app with shortcuts, the thread summarizer's.
// assistant:write is what Slack grants an app whose manifest declares it an
// agent (agent_view). app_mentions:read is the app_mention event's, the one
// delivery of a mention in a channel the bot is added to by that mention.
// There is no im:read: no call reads a direct message's conversation info
// (stampScope scopes DMs by their author), and message.im needs only
// im:history.
var requiredBotScopes = []string{
	"app_mentions:read", "assistant:write", "channels:history", "channels:read", "chat:write", "commands", "files:read", "files:write",
	"groups:history", "groups:read", "im:history", "im:write", "mpim:history", "mpim:read",
	"reactions:read", "reactions:write", "usergroups:read", "users:read", "users:read.email",
}

// plausibleEmail is one address with no display name: a local part, an
// @, and a domain with a dot.
var plausibleEmail = regexp.MustCompile(`^[^\s@,;<>"]+@[^\s@,;<>"]+\.[^\s@,;<>".]+$`)

// Settings is the Slack connector's resolved settings.
type Settings struct {
	BotToken, AppToken, UserToken string
	ChannelMode                   string
	NotifyEmails                  []string
	NotifyExcludeEmails           []string
	// What the operators are DMed about.
	NotifyQuestions, NotifySearches, NotifyChannels bool
	DocIngest                                       bool
	WorkspaceName, WorkspaceHost                    string
}

// SettingsFrom reads the Slack fields of resolved settings.
func SettingsFrom(r settings.Resolved) Settings {
	return Settings{
		BotToken: r.Secret(FieldBotToken), AppToken: r.Secret(FieldAppToken), UserToken: r.Secret(FieldUserToken),
		ChannelMode:         r.String(FieldChannelMode),
		NotifyEmails:        r.List(FieldNotifyEmails),
		NotifyExcludeEmails: r.List(FieldNotifyExcludeEmails),
		NotifyQuestions:     r.Bool(FieldNotifyQuestions),
		NotifySearches:      r.Bool(FieldNotifySearches),
		NotifyChannels:      r.Bool(FieldNotifyChannels),
		DocIngest:           r.Bool(FieldDocIngest),
		WorkspaceName:       r.String(FieldWorkspaceName),
		WorkspaceHost:       r.String(FieldWorkspaceHost),
	}
}

// Format keeps the tokens out of every fmt verb.
func (s Settings) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "slack.Settings{%s}", s.WorkspaceHost)
}

// Connector is the Slack connector's definition: its page's fields, their
// validation, the pre-flight Test, and, in source.go, its facts and its
// expansion. The leader's workers are wired in cmd/diderot.
type Connector struct {
	// APIURL is Slack's Web API base with a trailing slash; empty is
	// slack.APIURL. Tests point it at a fake.
	APIURL string
	// HTTP makes Test's requests; nil uses a client without a timeout of
	// its own (Test's context bounds it).
	HTTP *http.Client
	// Pool reads the slack_users directory when Test resolves the
	// operators to notify; nil asks Slack for each.
	Pool *pgxpool.Pool
	// FileStorage says the deployment has the file storage Index documents
	// needs (DIDEROT_IMAGES_BUCKET).
	FileStorage bool
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldBotToken, Label: "Bot token", Kind: settings.KindSecret, Required: true,
			Help: "Starts with xoxb-. In the Slack app's settings under OAuth & Permissions."},
		{Key: FieldAppToken, Label: "App token", Kind: settings.KindSecret, Required: true,
			Help: "Starts with xapp-. An app-level token with connections:write, under Basic Information."},
		{Key: FieldUserToken, Label: "User token", Kind: settings.KindSecret,
			Help: "Starts with xoxp-. Diderot adds the bot to channels, and removes it, as the person this token belongs to."},
		{Key: FieldChannelMode, Label: "Channel mode", Kind: settings.KindEnum, Required: true,
			Options: []string{ChannelModeMember, ChannelModeAllPublic}, Default: ChannelModeMember,
			OptionLabels: []string{"Only the channels the bot is in", "Every public channel"},
			Help:         "Which channels Diderot indexes. With every public channel, the bot also joins each public channel that isn't blocked."},
		{Key: FieldNotifyEmails, Label: "Operators", Kind: settings.KindList,
			Help: "Emails of the people who look after Diderot. They get the DMs chosen below, and Diderot names the first one when it tells someone who to ask for help."},
		{Key: FieldNotifyQuestions, Label: "DM operators about every question", Kind: settings.KindBool,
			Help: "Each question asked in Slack, through the API, or by an agent, with who asked and a link to the session."},
		{Key: FieldNotifySearches, Label: "DM operators about agent searches", Kind: settings.KindBool,
			Help: "Each search an agent runs through MCP without asking a question. This can be a lot of messages."},
		{Key: FieldNotifyChannels, Label: "DM operators about channel changes", Kind: settings.KindBool, Default: true,
			Help: "When Diderot is added to a private channel, or someone turns Ask on in a channel."},
		{Key: FieldNotifyExcludeEmails, Label: "People whose activity never sends a DM", Kind: settings.KindList,
			Help: "Emails of people, such as the operators themselves, whose questions and searches aren't reported."},
		{Key: FieldDocIngest, Label: "Index documents", Kind: settings.KindBool,
			Help: "Index PDF, Word, and Excel attachments in channels that allow it."},
		{Key: FieldWorkspaceName, Label: "Workspace name", Kind: settings.KindText, Discovered: true,
			PromptVar: promptcatalog.VarSlackWorkspaceName, Help: "Filled in when you save."},
		{Key: FieldWorkspaceHost, Label: "Workspace host", Kind: settings.KindText, Discovered: true,
			Help: "Filled in when you save."},
	}
}

// Validate checks that both email lists hold plausible addresses, and
// lowercases them in values.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	for _, field := range []string{FieldNotifyEmails, FieldNotifyExcludeEmails} {
		emails, _ := values[field].([]string)
		lowered := make([]string, 0, len(emails))
		for _, entered := range emails {
			email := strings.ToLower(strings.TrimSpace(entered))
			if !plausibleEmail.MatchString(email) {
				errs[field] = fmt.Sprintf("%q is not an email address", strings.TrimSpace(entered))
			}
			if !slices.Contains(lowered, email) {
				lowered = append(lowered, email)
			}
		}
		if emails != nil {
			values[field] = lowered
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (c Connector) apiURL() string {
	if c.APIURL != "" {
		return c.APIURL
	}
	return slack.APIURL
}

func (c Connector) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return safehttp.NewClient(safehttp.Options{})
}

// Test runs the connector's checks. Every call is read-only; the report
// carries counts, Slack's error codes, and the emails the admin entered,
// never a response body or message text.
func (c Connector) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	s := SettingsFrom(r)
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }

	bot, botScopes, problem := c.authTest(ctx, s.BotToken)
	switch {
	case problem != "":
		add(failed("Bot token", "auth.test: "+problem,
			r.SupplySecret(FieldBotToken, "the bot token (xoxb-) from OAuth & Permissions")+", and reinstall the app if it was revoked."))
	case bot.BotID == "":
		add(failed("Bot token", "Slack says this is not a bot token.", "Use the Bot User OAuth Token (xoxb-), not a user or app token."))
	default:
		host := workspaceHost(bot.URL)
		add(settings.Check{Name: "Bot token", Passed: true,
			Detail: fmt.Sprintf("Workspace %s (%s), bot user %s.", bot.Team, host, bot.User)})
		report.Facts = append(report.Facts,
			settings.Fact{Key: FieldWorkspaceName, Label: "Workspace name", Value: bot.Team},
			settings.Fact{Key: FieldWorkspaceHost, Label: "Workspace host", Value: host},
			settings.Fact{Key: "bot_user", Label: "Bot user", Value: bot.User, ID: bot.UserID},
		)
		add(scopeCheck(botScopes, s.ChannelMode))
	}

	add(c.socketCheck(ctx, s.AppToken, r.SupplySecret(FieldAppToken, "an app-level token (xapp-) with connections:write")+
		", and turn on Socket Mode in the app settings."))

	var api *slack.Client
	if problem != "" {
		// Every channel call would fail the same way and blame scopes.
		add(failed("Channels", "Not checked: the bot token did not authenticate.", "Fix the bot token, then test again."))
	} else {
		api = slack.New(s.BotToken, slack.OptionAPIURL(c.apiURL()), slack.OptionHTTPClient(c.httpClient()))
		joined, check := channelCheck(ctx, api)
		add(check)
		if check.Passed {
			add(parseCheck(ctx, api, joined, workspaceHost(bot.URL)))
		}
	}
	add(c.operatorsCheck(ctx, api, s.NotifyEmails))
	if s.DocIngest {
		add(source.DocumentIngestCheck(c.FileStorage))
	}

	if s.UserToken == "" {
		add(settings.Check{Name: "User token", Passed: true, Detail: "Not set: inviting the bot into private channels is off."})
	} else if user, _, problem := c.authTest(ctx, s.UserToken); problem != "" {
		add(failed("User token", "auth.test: "+problem,
			r.SupplySecret(FieldUserToken, "a user token (xoxp-) from the app's OAuth & Permissions page")+", or remove it."))
	} else if user.BotID != "" {
		add(failed("User token", "Slack says this is a bot token.", "Use the User OAuth Token (xoxp-)."))
	} else if bot.TeamID != "" && user.TeamID != bot.TeamID {
		add(failed("User token", "The user token belongs to another workspace than the bot token.", "Install the app and take both tokens from the same workspace."))
	} else {
		add(settings.Check{Name: "User token", Passed: true, Detail: "Valid in the same workspace, as " + user.User + "."})
	}
	return report
}

func failed(name, detail, remedy string) settings.Check {
	return settings.Check{Name: name, Detail: detail, Remedy: remedy}
}

// authIdentity is the part of auth.test the report uses.
type authIdentity struct {
	URL    string `json:"url"`
	Team   string `json:"team"`
	TeamID string `json:"team_id"`
	User   string `json:"user"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// authTest calls auth.test directly rather than through slack-go, which
// drops the x-oauth-scopes header that lists the token's scopes. A failure
// is described for the report, without Slack's response body.
func (c Connector) authTest(ctx context.Context, token string) (authIdentity, []string, string) {
	var identity authIdentity
	if token == "" {
		return identity, nil, "no token"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL()+"auth.test", nil)
	if err != nil {
		return identity, nil, "the Slack API URL is invalid"
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return identity, nil, "Slack is unreachable"
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return identity, nil, fmt.Sprintf("Slack answered HTTP %d", response.StatusCode)
	}
	var body struct {
		authIdentity
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return identity, nil, "Slack's answer was not JSON"
	}
	if !body.OK {
		return identity, nil, "Slack answered " + slackCode(body.Error)
	}
	var scopes []string
	for _, scope := range strings.Split(response.Header.Get("X-OAuth-Scopes"), ",") {
		if scope = strings.TrimSpace(scope); scope != "" {
			scopes = append(scopes, scope)
		}
	}
	return body.authIdentity, scopes, ""
}

func scopeCheck(granted []string, channelMode string) settings.Check {
	required := slices.Clone(requiredBotScopes)
	if channelMode == ChannelModeAllPublic {
		required = append(required, "channels:join")
	}
	var missing []string
	for _, scope := range required {
		if !slices.Contains(granted, scope) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return failed("Bot scopes", "Missing "+strings.Join(missing, ", ")+".",
			"Add them to the app's bot scopes (slack-app-manifest.yaml lists them all) and reinstall the app.")
	}
	return settings.Check{Name: "Bot scopes", Passed: true, Detail: fmt.Sprintf("All %d required scopes are granted.", len(required))}
}

// socketCheck opens a Socket Mode connection with the app token, waits for
// Slack's hello, and closes it. remedy says how to fix a token that fails.
func (c Connector) socketCheck(ctx context.Context, appToken, remedy string) settings.Check {
	const name = "App token and Socket Mode"
	if appToken == "" {
		return failed(name, "No app token.", remedy)
	}
	api := slack.New("", slack.OptionAppLevelToken(appToken), slack.OptionAPIURL(c.apiURL()), slack.OptionHTTPClient(c.httpClient()))
	_, wsURL, err := api.StartSocketModeContext(ctx)
	if err != nil {
		return failed(name, "apps.connections.open: "+describeSlackError(err), remedy)
	}
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return failed(name, "Couldn't connect to Slack's Socket Mode endpoint.", "Check that this server may open outbound WebSocket connections to Slack (wss://wss-primary.slack.com).")
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	var hello struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != "hello" {
		return failed(name, "Slack's Socket Mode endpoint didn't say hello.", remedy)
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return settings.Check{Name: name, Passed: true, Detail: "Opened a Socket Mode connection and closed it."}
}

// channelCheck reads one page of conversations.list and returns a channel
// the bot belongs to, if any.
func channelCheck(ctx context.Context, api *slack.Client) (string, settings.Check) {
	const name = "Channels"
	channels, _, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
		Types: []string{"public_channel", "private_channel"}, ExcludeArchived: true, Limit: 200,
	})
	if err != nil {
		return "", failed(name, "conversations.list: "+describeSlackError(err), "Grant channels:read and groups:read and reinstall the app.")
	}
	joined := ""
	members := 0
	for _, channel := range channels {
		if channel.IsMember {
			members++
			if joined == "" {
				joined = channel.ID
			}
		}
	}
	return joined, settings.Check{Name: name, Passed: true,
		Detail: fmt.Sprintf("The bot sees %d channels on the first page and belongs to %d of them.", len(channels), members)}
}

// parseCheck runs the latest messages of one joined channel through the
// ingest parser without writing anything, and reports only counts.
func parseCheck(ctx context.Context, api *slack.Client, channelID, host string) settings.Check {
	const name = "Message parsing"
	if channelID == "" {
		return settings.Check{Name: name, Passed: true,
			Detail: "Skipped: the bot belongs to no channel yet. Invite it with /invite @diderot."}
	}
	history, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channelID, Limit: testSampleSize,
	})
	if err != nil {
		return failed(name, "conversations.history: "+describeSlackError(err), "Grant channels:history and groups:history and reinstall the app.")
	}
	parsed, failures := 0, 0
	for _, message := range history.Messages {
		raw, err := json.Marshal(msgEnvelope{Type: "message", Channel: channelID, Msg: message.Msg})
		if err != nil {
			failures++
			continue
		}
		if _, err := ParseMessage(ctx, raw, host, nil); err != nil {
			failures++
			continue
		}
		parsed++
	}
	if failures > 0 {
		return failed(name, fmt.Sprintf("%d of the last %d messages in a joined channel didn't parse.", failures, len(history.Messages)),
			"Report this as a Diderot bug; the parser rejected messages Slack returned.")
	}
	return settings.Check{Name: name, Passed: true,
		Detail: fmt.Sprintf("Parsed the last %d messages of a joined channel without writing them.", parsed)}
}

// operatorsCheck resolves each operator email the way notifications do. A
// nil api means the bot token did not authenticate. The report names only
// emails the admin entered.
func (c Connector) operatorsCheck(ctx context.Context, api *slack.Client, emails []string) settings.Check {
	const name = "Operators"
	if len(emails) == 0 {
		return settings.Check{Name: name, Passed: true, Detail: "Not set: no one gets a DM."}
	}
	if api == nil {
		return failed(name, "Not checked: the bot token did not authenticate.", "Fix the bot token, then test again.")
	}
	var missing []string
	for _, email := range emails {
		_, err := operatorUserID(ctx, c.Pool, api.GetUserByEmailContext, email)
		switch {
		case errors.Is(err, errNoMember):
			missing = append(missing, email)
		case err != nil:
			return failed(name, "Couldn't look up "+email+": "+describeSlackError(err)+".",
				"Test again. If it keeps failing, check the bot token and scopes above.")
		}
	}
	if len(missing) > 0 {
		return failed(name, "No member of this workspace has "+strings.Join(missing, ", ")+".",
			"Use the email of a member of this workspace.")
	}
	return settings.Check{Name: name, Passed: true, Detail: "Every email belongs to a member of this workspace."}
}

// describeSlackError is a short, body-free description of a slack-go error.
func describeSlackError(err error) string {
	var rateLimited *slack.RateLimitedError
	var response slack.SlackErrorResponse
	var status slack.StatusCodeError
	switch {
	case errors.As(err, &rateLimited):
		return "Slack is rate limiting this app; try again in a minute"
	case errors.As(err, &response):
		return "Slack answered " + slackCode(response.Err)
	case errors.As(err, &status):
		return fmt.Sprintf("Slack answered HTTP %d", status.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	if code := err.Error(); slackCodePattern.MatchString(code) {
		return "Slack answered " + code
	}
	return "Slack is unreachable"
}

var slackCodePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// slackCode passes Slack's machine-readable error codes and nothing else.
func slackCode(code string) string {
	if slackCodePattern.MatchString(code) {
		return code
	}
	return "an error"
}

// workspaceHost is the host of auth.test's workspace URL
// (https://example.slack.com/ -> example.slack.com).
func workspaceHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
