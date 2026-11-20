package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

// connectorSet is every connector this binary knows, the settings store
// that caches their saved settings on this replica, and the handles through
// which work that runs on the leader reaches code outside it.
type connectorSet struct {
	// store caches the organization settings, the setup state, and every
	// connector's settings for request paths (the Ask gate, organization
	// wording, Slack scopes and links), polling the database for changes.
	store      *settings.Store
	registries setup.Registries
	// pushHandler serves push ingest, nil unless it is on.
	pushHandler *sessioningest.PushHandler
	// summaries gates agent-session summaries and says what stops them.
	summaries sessioningest.SummaryControl
	// sessionFeed is the session feed connector's client, through which every
	// replica expands feed sessions and reads Ask usage accounting.
	sessionFeed *liveSessionFeed
	// publicScopes resolves workspace-public Slack channels on every
	// replica.
	publicScopes *slackcx.PublicScopeResolver
	// term is what each leader term lends the sources whose Run needs more
	// than source.Deps.
	term *settings.Live[leaderTerm]
	// slack is what the running Slack source lends the leader's other work.
	slack *settings.Live[slackcx.Delivery]
}

// loadConnectors builds every connector and the settings store, loads the
// saved settings into the store's cache, and starts polling them until ctx
// ends. The connectors read the store's cache and the store decodes their
// settings, so the store is built first and learns them before it loads.
// noSummaryModel says what the catalog says lets a model write session
// summaries, empty when one can.
func loadConnectors(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, authorizer *authorization.Authorizer,
	noSummaryModel func() string, logger *slog.Logger,
) (connectorSet, error) {
	keys, err := settings.NewKeyring(cfg.ConfigKeys)
	if err != nil {
		return connectorSet{}, err
	}
	store := settings.NewStore(pool, logger.With("component", "settings"))
	pushHandler, err := newSessionPushHandler(cfg, pool, logger)
	if err != nil {
		return connectorSet{}, fmt.Errorf("session push ingest: %w", err)
	}
	var pushStore *sessioningest.PushStore
	if pushHandler != nil {
		pushStore = pushHandler.Store()
	}
	summaries := sessioningest.NewSummaryControl(sessioningest.NewStore(pool), noSummaryModel)
	c := connectorSet{
		store: store, pushHandler: pushHandler, summaries: summaries, sessionFeed: &liveSessionFeed{store: store},
		publicScopes: slackcx.NewPublicScopeResolver(pool, logger.With("component", "public-scopes")),
		term:         &settings.Live[leaderTerm]{}, slack: &settings.Live[slackcx.Delivery]{},
	}
	slack := &slackWiring{
		processCtx: ctx, cfg: cfg, authorizer: authorizer, publicScopes: c.publicScopes,
		settingsStore: store, delivery: c.slack,
	}
	c.registries = setup.Registries{
		Sources:     sourceRegistry(pool, c.term, slack, c.sessionFeed.Client, pushStore, &summaries, cfg.ImagesBucket != ""),
		Tools:       toolRegistry(),
		Automations: automationRegistry(cfg, store),
	}
	registry := connectorRegistry(c.registries.Sources, c.registries.Tools, c.registries.Automations)
	env := settings.ReadEnv(registry, os.Getenv)
	maps.Copy(env, askconn.ReadEnv(os.Environ()))
	maps.Copy(env, modelprovider.ReadEnv(os.Environ()))
	standalone := cfg.Deployment == config.DeploymentStandalone
	store.WithConnectors(registry, settings.Secrets{Keys: keys, Env: env}).
		WithFamily(askconn.Definitions{Standalone: standalone, Images: cfg.ImagesBucket != ""}.Family()).
		WithFamily(modelprovider.Definitions{Standalone: standalone}.Family())
	if err := store.Load(ctx); err != nil {
		return connectorSet{}, fmt.Errorf("load settings: %w", err)
	}
	go store.Run(ctx, settings.PollInterval)
	return c, nil
}

// startScopeInvalidations listens for the database's notices that
// workspace-public Slack channels changed, so this replica's resolver drops
// what it cached. The returned stop ends the listener and waits for it.
func startScopeInvalidations(ctx context.Context, publicScopes *slackcx.PublicScopeResolver) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		publicScopes.RunInvalidationListener(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}
