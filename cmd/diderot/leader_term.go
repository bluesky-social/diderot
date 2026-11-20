package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/embedder"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/scopesync"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/sessiontitle"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// leaderTerm is what each leader term builds anew and lends the sources
// whose Run needs it beyond source.Deps.
type leaderTerm struct {
	// models are the models the roles use this term: Slack's thread
	// summaries use one.
	models *inferenceModels
	// ask is the replica's Ask, nil when Ask is off: Slack routes mentions
	// and DMs to it.
	ask *termAsk
	// ingest is the session ingest the feed drives.
	ingest *sessionIngest
}

// errNoLeaderTerm is a Run outside a leader term, which the settings
// supervisor never starts.
var errNoLeaderTerm = errors.New("no leader term is running")

// leaderWork is what serve builds once and every leader term builds its
// work from.
type leaderWork struct {
	cfg    *config.Config
	logger *slog.Logger
	pool   *pgxpool.Pool
	// vectors opens the embedding model's collection for the term.
	vectors *vectorStores
	// catalog is what the server knows about models, for the leader to
	// watch the ones in use; nil in tests that need none.
	catalog *modelCatalog
	// readers.sink is what the term writes events through.
	readers corpusReaders
	// mirror is object storage, nil without it.
	mirror     files.S3Mirror
	prompts    *promptcatalog.Store
	reports    *scheduledreport.Store
	settings   *settings.Store
	registries setup.Registries
	// ask is the replica's Ask, nil without it; a term lends its service.
	ask *replicaAsk
	// summaries gates agent-session summaries and titles.
	summaries sessioningest.SummaryControl
	// term is where a term lends itself to the sources while it runs.
	term *settings.Live[leaderTerm]
	// slack is what the running Slack source lends; work that posts to
	// Slack takes it at each use, so that work runs whether or not Slack
	// does.
	slack *settings.Live[slackcx.Delivery]
}

// leaderTerms is what a replica runs while it leads: leader terms, one at a
// time. Each builds every client from the models the roles use as it
// starts, so a saved change to them ends the term and starts another, as a
// new leadership would.
func leaderTerms(work leaderWork, liveModels func() *inferenceModels) func(context.Context) {
	return func(ctx context.Context) {
		runPerModels(ctx, work.logger, "leader work", liveModels, sameRoleModels, func(termCtx context.Context, models *inferenceModels) {
			runLeaderTerm(termCtx, work, models)
		})
	}
}

// runLeaderTerm builds one leader term's work with models and runs it until
// ctx ends: at shutdown, when leadership is lost, or when a saved change
// alters the models. Every worker runs under one worker.Supervisor, which
// counts each one's runs and recovers its panics; a worker that fails is
// logged and counted, never fatal. The term takes the replica's Ask once
// the replica has tried to build it; each turn the term's work starts runs
// on the replica's build current then. While the workers run, the term
// lends itself, with that Ask, to the sources. It returns once every worker
// has stopped and then every lend has ended, in the reverse of the order
// they began, so the next term starts from nothing.
func runLeaderTerm(ctx context.Context, w leaderWork, models *inferenceModels) {
	cfg, logger := w.cfg, w.logger
	sup := worker.NewSupervisor(logger, prometheus.DefaultRegisterer)
	pushStore, pushErr := addSessionPushRetention(sup, cfg, w.pool, logger)

	var asked *termAsk
	if t, ok := w.ask.forTerm(ctx); ok {
		asked = &t
	}

	addAskTitleWorker(sup, cfg, models, w.pool, logger)
	addVectorWork(sup, w, models, logger)
	images := addCaptionWorker(sup, cfg, models, w.pool, w.mirror, w.registries.Sources, logger)
	ingest := addPushIngest(sup, cfg, models, w.pool, w.readers.sink, w.prompts, w.summaries, summaryThresholds(w.settings), pushStore, pushErr, logger)
	w.term.Set(leaderTerm{models: models, ask: asked, ingest: ingest})
	defer w.term.Clear()

	// Scheduled reports run with Ask on every term and wait in the queue
	// while Slack, which delivers them, is off.
	if asked != nil {
		sup.AddRunner(scheduledReports(w.reports, asked, w.slack, w.settings, logger))
	}
	sup.AddRunner(reembedJobs(w.pool, w.readers.sink, w.slack, w.settings, logger))
	sup.AddRunner(connectionChecks(w.settings, w.pool, logger))
	sup.AddRunner(modelFactsRunner(w.settings, w.pool, logger))
	if w.catalog != nil {
		sup.AddRunner(modelAttentionRunner(w.catalog, w.settings, logger))
	}

	connectorMetrics := settings.NewConnectorMetrics(prometheus.DefaultRegisterer)
	runners := sourceRunners(w.registries.Sources, connectorMetrics, cfg, models, w.pool, w.readers.sink, w.mirror, images, logger)
	runners = append(runners, automationRunners(w.registries.Automations, connectorMetrics, w.pool, asked, w.slack, w.settings, logger)...)
	connectors := settings.NewSupervisor(w.settings, logger.With("component", "connectors"), runners...)
	sup.AddFunc("connectors", func(ctx context.Context) error {
		connectors.Run(ctx)
		return nil
	})
	sup.Run(ctx)
}

// addVectorWork adds the term's work that needs the vector collection: the
// embedder, and Qdrant's upkeep. It writes to the collection being filled
// when an embedding model is replacing another, and otherwise to the one
// search uses. The work opens the collection itself, trying again every
// vectorRetry while Qdrant can't, so everything else the term runs (the
// sources, session ingest, captions, reports) never waits for it. While no
// embedding model is chosen there is none.
func addVectorWork(sup *worker.Supervisor, w leaderWork, models *inferenceModels, logger *slog.Logger) {
	model, embedding := models.vectorTarget()
	if model.ID == "" {
		logger.Info("embedder worker off: no embedding model is chosen; new chunks wait for one")
		return
	}
	filling := models.fill.ID != ""
	sup.AddFunc("vector-work", func(ctx context.Context) error {
		store := openTermVectors(ctx, w, embedding, filling, logger)
		if store == nil {
			return nil
		}
		inner := worker.NewSupervisor(logger, prometheus.DefaultRegisterer)
		addEmbedderWorker(inner, w.cfg, model, embedding, w.pool, store, logger)
		addScopeSync(inner, w.pool, store, w.slack, logger)
		if filling {
			inner.AddRunner(finishFilling(w.settings, w.pool, embedding, logger))
		}
		inner.Run(ctx)
		return nil
	})
}

// openTermVectors opens the collection the term's embedder writes to and,
// when it is being filled, marks the chunks it lacks. While Qdrant can't
// open it, it tries again every vectorRetry; it returns nil once ctx ends.
func openTermVectors(ctx context.Context, w leaderWork, embedding settings.Embedding, filling bool, logger *slog.Logger) *qdrantstore.Store {
	for {
		store, err := w.vectors.open(ctx, embedding)
		if err == nil && filling {
			err = fenceFilling(ctx, w.pool, embedding.Collection, logger)
		}
		if err == nil {
			return store
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Error("the embedder and vector upkeep wait: the embedding collection could not be opened; retrying",
			"collection", embedding.Collection, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(vectorRetry):
		}
	}
}

// addSessionPushRetention builds the term's push store, nil unless push
// ingest is on, and adds the worker that prunes superseded revisions when a
// retention is set. Serve refused to start on an invalid push
// configuration, so an error here is logged and returned for the session
// ingest to fail closed on.
func addSessionPushRetention(sup *worker.Supervisor, cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (*sessioningest.PushStore, error) {
	store, err := newSessionPushStore(cfg, pool)
	if err != nil {
		logger.Error("session push store", "err", err)
	}
	if retention := cfg.SessionPush.SupersededRetention; store != nil && retention > 0 {
		sup.AddRunner(sessionPushRetentionRunner(store, retention, logger))
	}
	return store, err
}

// addAskTitleWorker titles Ask sessions apart from their answers: a session
// becomes eligible once it has a successful answer, and each title is tried
// once, so titles never add to an answer's latency.
func addAskTitleWorker(sup *worker.Supervisor, cfg *config.Config, models *inferenceModels, pool *pgxpool.Pool, logger *slog.Logger) {
	if !cfg.AskEnabled {
		return
	}
	if !sessionTitlesEnabled(models) {
		logger.Info("session title worker disabled (needs a configured model registry provider serving responses)")
		return
	}
	titleGenerator, err := newTitleGenerator(models.title, promptcatalog.AskTitle)
	if err != nil {
		logger.Error("session title generator init failed; disabled", "err", err)
		return
	}
	titleWorker := &sessiontitle.Worker{
		Pool: pool, Generator: titleGenerator, BatchSize: 10,
		Logger: logger.With("component", "session-title"),
	}
	sup.AddRunner(worker.Runner{
		Name: "session-title", Interval: time.Minute, BootTick: true,
		MinBackoff: 15 * time.Second, MaxBackoff: 5 * time.Minute,
		Logger: titleWorker.Logger, Run: titleWorker.Tick,
	})
	logger.Info("session title worker enabled", "model", models.title.ID)
}

// addEmbedderWorker embeds new chunks into Qdrant. Without it chunk rows
// pile up with embedded_at NULL and retrieval finds nothing, so running
// without an embeddings model is a warning. The worker owns its fetch and
// embed loop; the supervisor only notices if it returns early.
func addEmbedderWorker(sup *worker.Supervisor, cfg *config.Config, model boundModel, embedding settings.Embedding, pool *pgxpool.Pool,
	qdrant *qdrantstore.Store, logger *slog.Logger) {
	emb, err := newEmbedder(model, embedding.Dimensions)
	if err != nil {
		logger.Error("embedder worker disabled: client init failed", "err", err)
		return
	}
	w := &embedder.Worker{
		Pool:     pool,
		Embedder: emb,
		Store:    qdrant,
		Logger:   logger.With("component", "embedder"),
		Registry: prometheus.DefaultRegisterer,
		Config: embedder.WorkerConfig{
			Concurrency: cfg.EmbedWorkers,
			BatchSize:   cfg.EmbedBatch,
		},
	}
	logger.Info("embedder worker enabled",
		"model", model.ID, "collection", qdrant.Collection(), "workers", cfg.EmbedWorkers, "batch", cfg.EmbedBatch)
	sup.AddFunc("embedder", w.Run)
}

// addCaptionWorker gives each source a slot it lends the caption worker its
// image fetcher through while it runs, adds the caption worker, which reads
// every slot, and returns the slots.
func addCaptionWorker(sup *worker.Supervisor, cfg *config.Config, models *inferenceModels, pool *pgxpool.Pool,
	mirror files.S3Mirror, sources source.Registry, logger *slog.Logger,
) map[string]*lentImages {
	images := map[string]*lentImages{}
	fetchers := map[string]files.SourceFetcher{}
	for _, info := range sources.Infos() {
		images[info.EventSource] = &lentImages{}
		fetchers[info.EventSource] = images[info.EventSource]
	}
	registerCaptionWorker(sup, cfg, models, pool, mirror, logger, fetchers)
	return images
}

// addPushIngest builds the term's session ingest and, with push ingest on,
// adds its worker reading pushed sessions; otherwise the session feed's Run
// drives it while that connector runs.
func addPushIngest(sup *worker.Supervisor, cfg *config.Config, models *inferenceModels, pool *pgxpool.Pool, sink *events.DBSink,
	prompts *promptcatalog.Store, control sessioningest.SummaryControl, thresholds func() sessioningest.SummarizePolicy,
	push *sessioningest.PushStore, pushErr error, logger *slog.Logger,
) *sessionIngest {
	ingest := &sessionIngest{
		cfg: cfg, models: models, pool: pool, prompts: prompts, push: push, pushErr: pushErr,
		control: control, thresholds: thresholds, logger: logger,
	}
	if cfg.SessionPush.Enabled {
		if err := ingest.register(sup, sink, nil, ""); err != nil {
			logger.Error("sessioningest: not registering worker (fail closed)", "err", err)
		}
	}
	return ingest
}

// sourceRunners runs each source with its own observed sink and base
// metrics, and its attachment workers' slots.
func sourceRunners(sources source.Registry, connectorMetrics *settings.ConnectorMetrics, cfg *config.Config, models *inferenceModels,
	pool *pgxpool.Pool, sink *events.DBSink, mirror files.S3Mirror, images map[string]*lentImages, logger *slog.Logger,
) []settings.Connector {
	var runners []settings.Connector
	for _, src := range sources.Sources() {
		eventSource, sourceLogger, metrics := src.Info().EventSource, logger.With("connector", src.Name()), connectorMetrics.For(src.Name())
		deps := source.Deps{Pool: pool, Sink: sink.Observed(metrics), Logger: sourceLogger, Metrics: metrics}.For(src.Name())
		deps.Files = &sourceFiles{
			eventSource: eventSource, images: images[eventSource],
			cfg: cfg, models: models, pool: pool, mirror: mirror, logger: sourceLogger,
		}
		runners = append(runners, sourceRunner{Source: src, deps: deps})
	}
	return runners
}

// automationRunners runs each automation with its own base metrics, the
// term's Ask, and the running Slack source's delivery handle.
func automationRunners(automations automation.Registry, connectorMetrics *settings.ConnectorMetrics, pool *pgxpool.Pool,
	asked *termAsk, slack *settings.Live[slackcx.Delivery], store *settings.Store, logger *slog.Logger,
) []settings.Connector {
	// A nil *termAsk in the interface would read as Ask being on.
	var asker automation.Ask
	observability := func() bool { return false }
	if asked != nil {
		asker, observability = asked, asked.Observability
	}
	var runners []settings.Connector
	for _, auto := range automations.Automations() {
		runners = append(runners, automationRunner{Automation: auto, deps: automation.Deps{
			Pool: pool, Logger: logger.With("connector", auto.Name()), Metrics: connectorMetrics.For(auto.Name()),
			Ask: asker, Observability: observability, Slack: slack,
			AskAllowed: func() bool { return store.AskAllowed(false) },
		}})
	}
	return runners
}

// addScopeSync adds the scope workers. The reconcile runs hourly, the
// accepted window for a privacy change to reach the corpus, refreshing
// channel privacy through the running Slack source first; a reconcile that
// changes nothing makes no Qdrant call. The payload patches it queues, and
// the deletes chunk deletion queues, drain apart from it in bounded
// one-minute passes.
func addScopeSync(sup *worker.Supervisor, pool *pgxpool.Pool, qdrant *qdrantstore.Store, slack *settings.Live[slackcx.Delivery], logger *slog.Logger) {
	scopeService := &scopesync.Service{
		Pool:            pool,
		Store:           qdrant,
		Logger:          logger.With("component", "scopesync"),
		Metrics:         scopesync.NewMetrics(prometheus.DefaultRegisterer),
		RefreshChannels: slackChannelRefresh(slack, pool),
	}
	sup.AddRunner(worker.Runner{
		Name:     "scope-reconcile",
		Interval: time.Hour,
		BootTick: true,
		Logger:   logger.With("component", "scopesync"),
		Run: func(ctx context.Context) error {
			result, err := scopeService.Reconcile(ctx)
			if err != nil {
				return err
			}
			logger.Info("scope reconcile complete",
				"reconciled_events", result.Events,
				"queued_chunks", result.Chunks,
				"pending_chunks", result.Pending)
			return nil
		},
	})
	sup.AddRunner(worker.Runner{
		Name:     "scope-delta",
		Interval: time.Minute,
		BootTick: true,
		Logger:   logger.With("component", "scopesync"),
		Run: func(ctx context.Context) error {
			applied, err := scopeService.DrainScopeCatchUp(ctx)
			if applied > 0 {
				logger.Info("scope delta applied", "chunks", applied)
			}
			return err
		},
	})
	sup.AddRunner(worker.Runner{
		Name:     "scope-metrics",
		Interval: 5 * time.Minute,
		BootTick: true,
		Logger:   logger.With("component", "scopesync"),
		Run:      scopeService.RefreshGauges,
	})
	sup.AddRunner(worker.Runner{
		Name:     "qdrant-delete",
		Interval: time.Minute,
		BootTick: true,
		Logger:   logger.With("component", "scopesync"),
		Run: func(ctx context.Context) error {
			applied, err := scopeService.DrainDeletes(ctx)
			if err == nil && applied > 0 {
				logger.Info("qdrant deletes applied", "points", applied)
			}
			return err
		},
	})
}
