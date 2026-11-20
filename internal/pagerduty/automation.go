package pagerduty

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/automation"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// errSlackOff is a delivery attempted while no Slack connector runs.
var errSlackOff = errors.New("the Slack connector is off")

func (Connector) Info() automation.Info {
	return automation.Info{Title: "PagerDuty", Step: "pagerduty",
		Summary: "Investigates PagerDuty incidents as they trigger and replies in Slack."}
}

// Status says why investigations wait in the queue: Ask can't investigate
// in this deployment, or Slack, which delivers them, is off.
func (c Connector) Status(context.Context) (settings.Status, error) {
	var status settings.Status
	switch {
	case !c.Ask:
		status.Waiting = "Waiting for Ask, which is off in this deployment. Alerts wait in the queue until then."
	case c.Grafana == nil || !c.Grafana():
		status.Waiting = "Waiting for a Grafana connection for Ask to investigate with. Alerts wait in the queue until then."
	case c.Slack == nil || !c.Slack():
		status.Waiting = "Waiting for Slack to be enabled. Alerts wait in the queue until then."
	}
	return status, nil
}

// Run investigates queued pages, two at a time, until ctx ends. It holds no
// Slack connection: each investigation posts through what the running Slack
// source lends when it posts. While Slack is off, the install is in setup,
// or Ask has no observability investigator with a Grafana connection,
// pages wait in the queue.
func (c Connector) Run(ctx context.Context, deps automation.Deps, r settings.Resolved) error {
	if deps.Ask == nil {
		deps.Logger.Error("PagerDuty investigations cannot start: Ask is off; pages remain queued")
		<-ctx.Done()
		return nil
	}
	logger := deps.Logger.With("component", "pagerduty-investigations")
	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	for index := range 2 {
		sup.AddRunner(worker.Runner{
			Name: fmt.Sprintf("pagerduty-investigations-%d", index), Interval: 5 * time.Second, BootTick: true,
			MinBackoff: 5 * time.Second, MaxBackoff: time.Minute,
			Logger: logger, Run: c.investigator(deps, r).tick,
		})
	}
	sup.Run(ctx)
	return nil
}

// investigator takes one queued page per tick and investigates it.
type investigator struct {
	runner *Runner
	deps   automation.Deps
}

func (c Connector) investigator(deps automation.Deps, r settings.Resolved) investigator {
	s := SettingsFrom(r)
	queue := NewStore(deps.Pool)
	return investigator{deps: deps, runner: &Runner{
		Queue: queue, Agent: deps.Ask, Pager: c.client(s),
		Slack: &lentSlack{live: deps.Slack, delivery: SlackDelivery{
			Store: queue, ChannelID: s.SlackChannelID, ChannelName: s.SlackChannelName,
			Artifacts: deps.Ask, Scopes: s.AllowedScopes,
		}},
		PublicURL: c.PublicURL, AllowedScopes: s.AllowedScopes,
		Logger: deps.Logger.With("component", "pagerduty-investigations"),
	}}
}

// tick investigates the next queued page, leaving it queued while the
// install is in setup, Ask has no Grafana connection to investigate with,
// or Slack is off.
func (i investigator) tick(ctx context.Context) error {
	if !i.deps.AskAllowed() || !i.deps.Observability() {
		return nil
	}
	if slack, ok := i.deps.Slack.Get(); !ok || !slack.Running() {
		return nil
	}
	return i.runner.Tick(ctx)
}

// lentSlack delivers an investigation through what the Slack source running
// when it posts lends, holding it lent for the delivery so that run doesn't
// end mid-post, and stopping the delivery when that run starts to stop.
type lentSlack struct {
	live     *settings.Live[slackcx.Delivery]
	delivery SlackDelivery
}

func (l *lentSlack) Deliver(ctx context.Context, job *Job, report string) (messageTS string, err error) {
	if !l.live.With(func(slack slackcx.Delivery) {
		ctx, done := slack.Within(ctx)
		defer done()
		delivery := l.delivery
		delivery.API, delivery.Limiter, delivery.Answers = slack.API, slack.Limiter, slack.Consumer
		messageTS, err = delivery.Deliver(ctx, job, report)
	}) {
		return "", errSlackOff
	}
	return messageTS, err
}
