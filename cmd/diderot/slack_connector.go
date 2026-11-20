package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// slackSource is the Slack source: the connector's definition, facts and
// expansion, and a Run that holds every Slack ingest worker and the Ask
// consumer of a leader term, with wiring and what the term lends it.
type slackSource struct {
	slackcx.Connector
	term   *settings.Live[leaderTerm]
	wiring *slackWiring
}

// slackWiring is what the Slack source's Run uses beyond source.Deps that
// lasts as long as the process.
type slackWiring struct {
	// processCtx ends at shutdown, which the consumer tells from a lost
	// leader term so it can drain accepted work.
	processCtx    context.Context
	cfg           *config.Config
	authorizer    *authorization.Authorizer
	publicScopes  *slackcx.PublicScopeResolver
	settingsStore *settings.Store
	// delivery is where the Run lends its client, consumer, resolvers and
	// rebuilder to work outside it, for as long as it runs.
	delivery *settings.Live[slackcx.Delivery]
}

func (s *slackSource) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	term, ok := s.term.Get()
	if !ok {
		return errNoLeaderTerm
	}
	return s.wiring.run(ctx, deps, resolved, term)
}

// run holds every Slack worker of term with resolved. The settings
// supervisor restarts it when they change.
func (c *slackWiring) run(ctx context.Context, deps source.Deps, resolved settings.Resolved, term leaderTerm) error {
	s := slackcx.SettingsFrom(resolved)
	logger, pool := deps.Logger, deps.Pool
	sup := worker.NewSupervisor(logger, deps.Metrics)

	// One client and one limiter for every Slack call, so all traffic
	// shares the workspace's rate limits. The limiter turns a 429 into a
	// degraded mode (1 req/min) until five calls in a row succeed.
	api := slack.New(s.BotToken, slack.OptionAppLevelToken(s.AppToken))
	limiter := slackcx.NewLimiter(slackcx.DefaultLimiterConfig(),
		logger.With("component", "slack-limiter"), deps.Metrics)
	dir := slackcx.NewDirectory(pool, api).WithLimiter(limiter)
	prefix := slackcx.NewPrefixBuilder(dir, pool)

	fileFetcher := slackcx.NewFileFetcher(api, limiter, s.BotToken, nil, logger.With("component", "slack-fetcher"))
	releaseImages := deps.Files.LendImages(ctx, fileFetcher)
	defer releaseImages()

	// The ingest blacklist is loaded before any ingest path can run, so the
	// first gate check sees it; it then follows the changes operators make
	// through any replica.
	blacklist := slackcx.NewIngestBlacklist(pool, logger.With("component", "ingest-blacklist"))
	if n, err := blacklist.Refresh(ctx); err != nil {
		logger.Warn("ingest blacklist: initial refresh failed; gates will be open until next tick", "err", err)
	} else {
		logger.Info("ingest blacklist loaded", "entries", n)
	}
	blacklist.StartRefresher(ctx)

	ingestMetrics := slackcx.NewIngestMetrics(deps.Metrics)
	bf := slackcx.NewBackfill(pool, deps.Sink, api, dir, prefix,
		logger.With("component", "backfill"), deps.Metrics, s.WorkspaceHost).
		WithLimiter(limiter).
		WithIngestMetrics(ingestMetrics).
		WithIngestBlacklist(blacklist)
	channelConfigs := slackcx.NewChannelConfigStore(pool, dir,
		&slackcx.SlackClientMembersAdapter{Client: api}, logger.With("component", "channel-config")).
		WithLimiter(limiter)
	// Backfill, sync, and thread ingest need the same per-channel document
	// opt-in as the live consumer; without it they would enqueue no
	// documents at all.
	bf.WithDocGate(channelConfigs)

	// The "Summarize thread" shortcuts are best effort: without a chat
	// provider they reply "not configured".
	var summarizer *slackcx.Summarizer
	if threadSummariesEnabled(term.models) {
		if sum, err := newThreadSummarizer(term.models.summarizer); err != nil {
			logger.Error("summarizer: init failed; message shortcuts will reply not-configured", "err", err)
		} else {
			summarizer = sum
		}
	}

	// SyncWorker drains slack_channel_sync and ThreadWorker drains
	// slack_thread_sync; both share the backfill engine, its limiter, and
	// the blacklist.
	syncWorker := slackcx.NewSyncWorker(pool, bf, logger.With("component", "sync-worker"), deps.Metrics).
		WithIngestBlacklist(blacklist).
		WithChannelMode(s.ChannelMode)
	threadWorker := slackcx.NewThreadWorker(pool, bf, logger.With("component", "thread-worker"), deps.Metrics)
	docBackfillWorker := slackcx.NewDocumentBackfillWorker(pool, logger.With("component", "doc-backfill"))

	channelAuthorizer := &slackcx.ChannelAuthorizer{Policy: c.authorizer, Directory: dir, Logger: logger, PublicScopes: c.publicScopes}
	cons := slackcx.NewConsumer(pool, deps.Sink, s.AppToken, s.BotToken, s.WorkspaceHost,
		logger.With("component", "slack"), deps.Metrics,
	).WithAPI(api).WithSyncWorker(syncWorker).
		WithChannelConfig(channelConfigs).
		WithChannelAuthorizer(channelAuthorizer).
		WithIngestBlacklist(blacklist).
		WithLimiter(limiter).
		WithIngestMetrics(ingestMetrics).
		WithAskSessionViewerURL(c.cfg.PublicURL).
		WithOrgName(c.settingsStore.OrgName).
		WithAskGate(c.settingsStore).
		WithSummarizer(summarizer).
		WithNotifyEmails(s.NotifyEmails).
		WithNotifyEvents(s.NotifyQuestions, s.NotifySearches, s.NotifyChannels).
		WithNotifyExcludeEmails(s.NotifyExcludeEmails).
		// A negative control for the Slack e2e test only: it reproduces the
		// answered-before-captioned bug on purpose. Never set in a
		// deployment.
		WithDisableCaptionInjection(os.Getenv("DIDEROT_DISABLE_CAPTION_INJECTION") == "1")
	// Backfill skips the bot's own messages on the tail-sweep path. The
	// consumer learns its user id from auth.test once it runs, so read it
	// lazily.
	bf.WithSelfUserIDFn(cons.SelfUserID)

	// Live DMs and scheduled DM reports share one per-identity resolver and
	// its five-minute membership cache, so their audience and revocation
	// match.
	dmScopes := slackcx.NewUserScopeResolver(api, limiter, pool, logger.With("component", "dm-scopes")).WithPublicScopes(c.publicScopes)
	if term.ask != nil {
		// Mentions and DMs go to Ask.
		cons.WithAsk(term.ask).WithDMScopeResolver(dmScopes)
	}
	// New channels get Ask on by default (never overriding operator
	// config), and private-channel joins DM the operators.
	syncWorker.WithNewChannelHook(cons.HandleNewChannel)
	// Leader workers outside this Run use it while it lasts: automations
	// and scheduled reports post through its client and consumer, the scope
	// reconcile refreshes channel privacy through its client, and
	// re-embedding rebuilds Slack events with its directory and prefix
	// builder. The admin Slack controls and the operator notifier are not
	// among them: every replica builds its own (slackAdmin).
	c.delivery.Set(slackcx.Delivery{
		Run: ctx, API: api, Limiter: limiter, Consumer: cons, DMScopes: dmScopes, Channels: channelAuthorizer,
		Rebuilder: slackcx.NewRebuilder(dir, prefix, s.WorkspaceHost),
	})
	defer c.delivery.Clear()

	if s.DocIngest {
		closeDocuments := deps.Files.AddDocumentWorker(sup, "doc-extract", channelConfigs, fileFetcher)
		defer closeDocuments()
	}

	// Discover channels and reconcile slack_channel_sync every five
	// minutes, and at start for channels added while Slack was stopped. In
	// all-public mode JoinSweep first joins public channels the bot isn't
	// in; membership stays the source of truth for what is indexed.
	sup.AddRunner(worker.Runner{
		Name: "slack-reconcile", Interval: 5 * time.Minute, BootTick: true,
		Logger: logger.With("component", "slack-reconcile"),
		Run: func(ctx context.Context) error {
			if s.ChannelMode == slackcx.ChannelModeAllPublic {
				joined, err := syncWorker.JoinSweep(ctx, api)
				if err != nil {
					return err
				}
				if joined > 0 {
					logger.Info("join sweep joined channels", "count", joined)
				}
			}
			inserted, err := syncWorker.Reconcile(ctx, api)
			if err != nil {
				return err
			}
			if inserted > 0 {
				logger.Info("sync reconcile inserted channels", "count", inserted)
			}
			return nil
		},
	})
	sup.AddFunc("slack-sync", syncWorker.Run)
	sup.AddFunc("slack-thread", threadWorker.Run)
	sup.AddRunner(worker.Runner{
		Name: "slack-doc-backfill", Interval: 2 * time.Second, BootTick: true,
		Logger: logger.With("component", "doc-backfill"), Run: docBackfillWorker.Sweep,
	})
	sup.AddFunc("slack-consumer", func(ctx context.Context) error {
		return cons.RunWithProcessContext(ctx, c.processCtx)
	})
	// Reconcile ask_feedback with live reactions on recent answers, which
	// catches reactions missed while the socket was down.
	sup.AddRunner(worker.Runner{
		Name: "ask-feedback-sweep", Interval: slackcx.FeedbackSweepInterval,
		Logger: logger.With("component", "feedback-sweep"), Run: cons.SweepFeedback,
	})
	logger.Info("slack connector running", "workspace", s.WorkspaceHost, "channel_mode", s.ChannelMode,
		"doc_ingest", s.DocIngest, "notify_emails", len(s.NotifyEmails), "summarizer", summarizer != nil,
		"invites", s.UserToken != "")
	sup.Run(ctx)
	return nil
}

// liveScopeResolver resolves callers' private-channel scopes on every
// replica with the Slack bot token currently saved. It builds a new
// resolver, with its own client and an empty membership cache, when the
// Slack settings change; with Slack off it serves the baseline scopes.
type liveScopeResolver struct {
	store        *settings.Store
	pool         *pgxpool.Pool
	publicScopes *slackcx.PublicScopeResolver
	logger       *slog.Logger

	mu       sync.Mutex
	version  int64
	resolver *slackcx.UserScopeResolver
}

func (l *liveScopeResolver) current() *slackcx.UserScopeResolver {
	resolved, enabled := l.store.Connector(slackcx.ConnectorName)
	version := int64(0)
	if enabled {
		version = resolved.Version
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resolver != nil && l.version == version {
		return l.resolver
	}
	var resolver *slackcx.UserScopeResolver
	if enabled {
		limiter := slackcx.NewLimiter(slackcx.DefaultLimiterConfig(),
			l.logger.With("component", "userscopes-limiter"), prometheus.DefaultRegisterer)
		resolver = slackcx.NewUserScopeResolver(slack.New(slackcx.SettingsFrom(resolved).BotToken), limiter, l.pool, l.logger)
	} else {
		resolver = slackcx.NewUserScopeResolver(nil, nil, l.pool, l.logger)
	}
	l.resolver, l.version = resolver.WithPublicScopes(l.publicScopes), version
	return l.resolver
}

func (l *liveScopeResolver) ScopesForEmail(ctx context.Context, email string) []string {
	return l.current().ScopesForEmail(ctx, email)
}

func (l *liveScopeResolver) AskerForEmail(ctx context.Context, email string) (*ask.AskerInfo, error) {
	return l.current().AskerForEmail(ctx, email)
}

// slackWorkspaceHost is the saved Slack workspace host, empty while Slack is
// off, for links built on request paths.
func slackWorkspaceHost(store *settings.Store) func() string {
	return func() string {
		resolved, ok := store.Connector(slackcx.ConnectorName)
		if !ok {
			return ""
		}
		return slackcx.SettingsFrom(resolved).WorkspaceHost
	}
}
