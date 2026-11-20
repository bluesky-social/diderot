package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// errSlackOff is a post attempted while no Slack connector runs.
var errSlackOff = errors.New("the Slack connector is off")

// scheduledReports is the leader worker that runs due scheduled reports with
// agent and posts each through the Slack connector running when it posts.
// While Slack is off, due reports wait in the queue, and it says so once.
func scheduledReports(store *scheduledreport.Store, agent scheduledreport.Agent, live *settings.Live[slackcx.Delivery], gate *settings.Store, logger *slog.Logger) worker.Runner {
	logger = logger.With("component", "scheduled-reports")
	slack := slackReports{live: live}
	var waiting atomic.Bool
	// Channel deliveries are scoped to their destination by the runner.
	reports := &scheduledreport.Runner{
		Store: store, Agent: agent, Delivery: slack, ScopeResolver: slack, ChannelAuthorizer: slack,
		Ready: func() bool {
			slack, lent := live.Get()
			running := lent && slack.Running()
			if !running && !waiting.Swap(true) {
				logger.Info("scheduled reports wait in the queue until the Slack connector runs")
			}
			if running {
				waiting.Store(false)
			}
			return running
		},
		Logger: logger,
	}
	return worker.Runner{
		Name: "scheduled-reports", Interval: time.Minute, BootTick: true,
		MinBackoff: 15 * time.Second, MaxBackoff: 5 * time.Minute,
		Logger: logger, Run: whileLive(gate, reports.Tick),
	}
}

// whileLive runs tick only once the install is live: scheduled reports are
// Ask for an audience no administrator chose, so the setup gate refuses them.
// Their work stays queued until then. Automations apply the same gate
// through automation.Deps.AskAllowed.
func whileLive(gate *settings.Store, tick func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if !gate.AskAllowed(false) {
			return nil
		}
		return tick(ctx)
	}
}

// The adapters below act through what the Slack connector running at each
// call lends, inside live.With, so a Slack run doesn't end while one of
// them is still using it. Each use is cancelled as that run starts to stop
// (Delivery.Within), as it was when this work ran inside it.

// slackRebuilder rebuilds Slack events for the re-embed driver through the
// running Slack connector's rebuilder. While none runs, it reports the
// source off, which ends re-embedding Slack.
type slackRebuilder struct {
	live *settings.Live[slackcx.Delivery]
}

func (slackRebuilder) Source() string { return "slack" }

func (r slackRebuilder) Rebuild(ctx context.Context, row reembed.StoredRow) (event *events.Event, err error) {
	if !r.live.With(func(slack slackcx.Delivery) {
		ctx, done := slack.Within(ctx)
		defer done()
		event, err = slack.Rebuilder.Rebuild(ctx, row)
		// A rebuild cut short by the run stopping can come back without an
		// error but with lookups it gave up on, so it is not kept.
		if ctx.Err() != nil || !slack.Running() {
			event, err = nil, fmt.Errorf("slack connector stopped: %w", context.Canceled)
		}
	}) {
		return nil, reembed.ErrSourceOff
	}
	return event, err
}

// slackChannelRefresh re-reads channel privacy for the scope reconcile
// through the running Slack connector: one Tier-2 conversations.list pass
// keeps the hourly reconcile's revocation window at about an hour instead
// of the directory's 7-day TTL. Without one it does nothing and the
// reconcile uses cached channel info.
func slackChannelRefresh(live *settings.Live[slackcx.Delivery], pool *pgxpool.Pool) func(context.Context) (int, error) {
	return func(ctx context.Context) (refreshed int, err error) {
		live.With(func(slack slackcx.Delivery) {
			ctx, done := slack.Within(ctx)
			defer done()
			refreshed, err = slackcx.WarmChannels(ctx, slack.API, slack.Limiter, pool)
		})
		return refreshed, err
	}
}

// slackReports posts scheduled reports, and resolves their audiences,
// through the running Slack connector.
type slackReports struct {
	live *settings.Live[slackcx.Delivery]
}

func (r slackReports) DeliverScheduledReport(ctx context.Context, kind, target string, runID uuid.UUID, body string) (ts string, err error) {
	if !r.with(ctx, func(ctx context.Context, slack slackcx.Delivery) {
		ts, err = slack.Consumer.DeliverScheduledReport(ctx, kind, target, runID, body)
	}) {
		return "", errSlackOff
	}
	return ts, err
}

func (r slackReports) DeliverScheduledArtifacts(ctx context.Context, kind, target, deliveryTS string, sessionID uuid.UUID, assistantTurn int, artifacts []ask.ArtifactDescriptor) (err error) {
	if !r.with(ctx, func(ctx context.Context, slack slackcx.Delivery) {
		err = slack.Consumer.DeliverScheduledArtifacts(ctx, kind, target, deliveryTS, sessionID, assistantTurn, artifacts)
	}) {
		return errSlackOff
	}
	return err
}

func (r slackReports) ScopesForEmail(ctx context.Context, email string) (scopes []string) {
	r.with(ctx, func(ctx context.Context, slack slackcx.Delivery) { scopes = slack.DMScopes.ScopesForEmail(ctx, email) })
	return scopes
}

func (r slackReports) UserIDForEmail(ctx context.Context, email string) (userID string, ok bool) {
	r.with(ctx, func(ctx context.Context, slack slackcx.Delivery) {
		userID, ok = slack.DMScopes.UserIDForEmail(ctx, email)
	})
	return userID, ok
}

func (r slackReports) ResolveChannel(ctx context.Context, channelID string) (access authorization.ChannelAccess) {
	r.with(ctx, func(ctx context.Context, slack slackcx.Delivery) {
		access = slack.Channels.ResolveChannel(ctx, channelID)
	})
	return access
}

// with calls use inside live.With, with ctx ended also when the Slack run
// starts to stop.
func (r slackReports) with(ctx context.Context, use func(context.Context, slackcx.Delivery)) bool {
	return r.live.With(func(slack slackcx.Delivery) {
		ctx, done := slack.Within(ctx)
		defer done()
		use(ctx, slack)
	})
}

// reembedRebuilders are the re-embed driver's rebuilders, one for each
// source a re-embed can rebuild.
func reembedRebuilders(live *settings.Live[slackcx.Delivery]) []reembed.SourceRebuilder {
	return []reembed.SourceRebuilder{slackRebuilder{live: live}}
}

// reembedSources are the event sources a re-embed can rebuild, which the
// Jobs and maintenance page offers.
func reembedSources() []string {
	var sources []string
	for _, rebuilder := range reembedRebuilders(nil) {
		sources = append(sources, rebuilder.Source())
	}
	return sources
}

// reembedJobs is the leader worker that runs the re-embed any replica
// recorded in admin_jobs. A job cut short when the term ends is recorded as
// canceled. Re-embedding rebuilds Slack's events through the running Slack
// connector, so while Slack is on but its run has not started, as at the
// start of a term, a requested job waits rather than fail on a source that
// is about to run.
func reembedJobs(pool *pgxpool.Pool, sink *events.DBSink, live *settings.Live[slackcx.Delivery], store *settings.Store, logger *slog.Logger) worker.Runner {
	logger = logger.With("component", "reembed")
	jobs := &reembed.JobWorker{
		Jobs:   adminjob.NewStore(pool),
		Driver: reembed.NewDriver(pool, sink, logger, reembedRebuilders(live)...),
		Logger: logger,
	}
	return worker.Runner{
		Name: "reembed", Interval: 2 * time.Second, BootTick: true, Logger: logger,
		Run: func(ctx context.Context) error {
			if _, on := store.Connector(slackcx.ConnectorName); on {
				if slack, lent := live.Get(); !lent || !slack.Running() {
					return nil
				}
			}
			return jobs.Tick(ctx)
		},
	}
}
