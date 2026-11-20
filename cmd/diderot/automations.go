package main

import (
	"context"

	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/pagerduty"
	"github.com/bluesky-social/diderot/internal/settings"
)

// automationRegistry is every automation this binary runs, in the order pages
// list them after the tools. PagerDuty's Test and page say whether Slack and
// Ask with a Grafana connection, which investigations need, are on, from
// store's cache and cfg.
func automationRegistry(cfg *config.Config, store connectorCache) automation.Registry {
	return automation.NewRegistry(
		pagerduty.Connector{
			Slack: func() bool {
				_, enabled := store.Connector(slackcx.ConnectorName)
				return enabled
			},
			Ask:       cfg.AskEnabled,
			Grafana:   grafanaConnected(store),
			PublicURL: cfg.PublicURL,
		},
	)
}

// automationRunner runs an automation under the settings supervisor with its
// deps, starting its base metrics and counting a run that ends with an
// error.
type automationRunner struct {
	automation.Automation
	deps automation.Deps
}

func (r automationRunner) Run(ctx context.Context, s settings.Resolved) error {
	r.deps.Metrics.Started()
	err := r.Automation.Run(ctx, r.deps, s)
	if err != nil && ctx.Err() == nil {
		r.deps.Metrics.Failed()
	}
	return err
}
