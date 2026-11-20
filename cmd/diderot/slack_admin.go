package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/adminapi"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
)

// liveSlackAdmin is what requests on every replica act on Slack through,
// apart from the Slack connector the leader runs: the channel controls of
// the admin API and pages, and the notifier that DMs the operators about
// browser admin, HTTP and MCP use. They write the database, which the leader's
// workers read, and call Slack with the saved tokens through a client and
// limiter of their own. It builds them anew when the saved Slack settings
// change, and has none while Slack is off.
type liveSlackAdmin struct {
	store *settings.Store
	pool  *pgxpool.Pool
	// publicURL is where the notifier's session links point.
	publicURL string
	logger    *slog.Logger
	// apiURL is Slack's Web API base with a trailing slash; empty is
	// slack.APIURL. Tests point it at a fake.
	apiURL string

	mu      sync.Mutex
	version int64
	admin   *slackAdmin
}

type slackAdmin struct {
	controls adminapi.SlackControls
	// notifier is a consumer that is never run: it only DMs the operators.
	notifier *slackcx.Consumer
}

// current is the Slack admin for the settings saved now, nil while Slack is
// off.
func (l *liveSlackAdmin) current() *slackAdmin {
	resolved, enabled := l.store.Connector(slackcx.ConnectorName)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !enabled {
		l.admin, l.version = nil, 0
		return nil
	}
	if l.admin == nil || l.version != resolved.Version {
		l.admin, l.version = l.build(slackcx.SettingsFrom(resolved)), resolved.Version
	}
	return l.admin
}

func (l *liveSlackAdmin) build(s slackcx.Settings) *slackAdmin {
	var options []slack.Option
	if l.apiURL != "" {
		options = append(options, slack.OptionAPIURL(l.apiURL))
	}
	api := slack.New(s.BotToken, options...)
	limiter := slackcx.NewLimiter(slackcx.DefaultLimiterConfig(),
		l.logger.With("component", "slack-admin-limiter"), prometheus.DefaultRegisterer)
	dir := slackcx.NewDirectory(l.pool, api).WithLimiter(limiter)
	controls := adminapi.SlackControls{
		SyncWorker: slackcx.NewChannelSync(l.pool),
		ChannelConfigs: slackcx.NewChannelConfigStore(l.pool, dir,
			&slackcx.SlackClientMembersAdapter{Client: api}, l.logger.With("component", "channel-config")).
			WithLimiter(limiter),
		Blacklist: slackcx.NewIngestBlacklist(l.pool, l.logger.With("component", "ingest-blacklist")),
	}
	// Invites act as the user token's owner; a nil *Inviter in the
	// interface would read as having one.
	if s.UserToken != "" {
		controls.Inviter = slackcx.NewInviter(dir, api, slack.New(s.UserToken, options...))
	}
	notifier := slackcx.NewConsumer(l.pool, nil, s.AppToken, s.BotToken, s.WorkspaceHost,
		l.logger.With("component", "slack-notifier"), nil).
		WithAPI(api).WithLimiter(limiter).
		WithAskSessionViewerURL(l.publicURL).
		WithNotifyEmails(s.NotifyEmails).
		WithNotifyEvents(s.NotifyQuestions, s.NotifySearches, s.NotifyChannels).
		WithNotifyExcludeEmails(s.NotifyExcludeEmails)
	return &slackAdmin{controls: controls, notifier: notifier}
}

// Controls is what the admin API's Slack routes act through, false while
// Slack is off.
func (l *liveSlackAdmin) Controls() (adminapi.SlackControls, bool) {
	admin := l.current()
	if admin == nil {
		return adminapi.SlackControls{}, false
	}
	return admin.controls, true
}

// NotifyAskInvocation DMs the operators about an HTTP question. While
// Slack is off it says nothing.
func (l *liveSlackAdmin) NotifyAskInvocation(ctx context.Context, ownerEmail, sessionID, question string) error {
	admin := l.current()
	if admin == nil {
		return nil
	}
	return admin.notifier.NotifyAskInvocation(ctx, ownerEmail, sessionID, question)
}

// NotifyMCPInvocation DMs the operators about an MCP tool call. While Slack
// is off it says nothing.
func (l *liveSlackAdmin) NotifyMCPInvocation(ctx context.Context, ownerEmail, toolName, sessionID, input string) error {
	admin := l.current()
	if admin == nil {
		return nil
	}
	return admin.notifier.NotifyMCPInvocation(ctx, ownerEmail, toolName, sessionID, input)
}
