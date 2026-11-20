package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/corpus"
	"github.com/bluesky-social/diderot/internal/events"
)

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the diderot server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), os.Stdout)
		},
	}
}

// runServe runs one replica, in stages that each build what the next ones
// take:
//
//  1. Load and check the configuration and the authorization policy, and
//     build the model catalog, which reads providers, models and the
//     embedding model from the settings once they load.
//  2. Apply migrations and open the Postgres pool.
//  3. Build the connectors and load their saved settings, then the
//     authenticators, which read whether the bootstrap admin is on from
//     the settings.
//  4. Seed the prompt catalog and build what reads the corpus. Search
//     follows the saved embedding model, opening its Qdrant collection
//     once one is chosen.
//  5. Mount the probes, the admin API, the browser UI, and the corpus MCP
//     server, then start the listeners. Only then does object storage
//     attach to the file proxy, and the replica's Ask start building.
//  6. Contend for leadership: while this replica leads, a leader term runs
//     the connectors and every other leader-only worker.
//  7. On SIGINT or SIGTERM, start no more Ask turns, then wait, until one
//     drain deadline, for the leader's work to drain and then for Ask's
//     turns, and cancel the turns left; then shut the listeners down. A
//     listener that fails, or an election that cannot start, ends serve at
//     once.
//
// Background work that every replica runs (the settings poll, the prompt
// watcher, the public-channel invalidation listener, the Ask readiness
// gate and rebuilds, the overview warm-up, the corpus gauges) stops with
// the signal. The log, JSON at info level, goes to stdout.
func runServe(parent context.Context, stdout io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, authorizer, err := loadServeConfig(logger)
	if err != nil {
		return err
	}
	vectors := newVectorStores(cfg, logger)
	defer vectors.close()
	catalog := newModelCatalog(cfg, vectors)
	listenAddrs := serveListenAddrs(cfg)
	logger.Info("diderot starting", "version", Version)

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := openDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	connectors, err := loadConnectors(ctx, cfg, pool, authorizer, catalog.noSummaryModel, logger)
	if err != nil {
		return err
	}
	catalog.attach(connectors.store, pool)
	// liveModels is the models the roles use now, as this replica's settings
	// cache last saw the providers and the saved choices.
	liveModels := catalog.live
	serverAuth, err := configureServerAuth(cfg, listenAddrs, connectors.store, logger)
	if err != nil {
		return err
	}
	stopScopeInvalidations := startScopeInvalidations(ctx, connectors.publicScopes)
	defer stopScopeInvalidations()

	prompts, err := preparePrompts(ctx, pool, connectors.store, connectors.registries.Sources, grafanaDatasourcesPrompt(connectors.store), logger)
	if err != nil {
		return err
	}
	// The event sink is Postgres-only. Chunk deletion queues durable Qdrant
	// cleanup, which the leader drains.
	sink := events.NewDBSink(pool, prometheus.DefaultRegisterer)
	readers, err := newCorpusReaders(pool, sink, connectors.registries.Sources, logger)
	if err != nil {
		return err
	}
	// A corpus whose chunks have vectors needs an embedding model saved:
	// without one the models settings were lost, which /readyz reports.
	if err := catalog.noteVectors(ctx, pool); err != nil {
		return err
	}
	readiness := startReadiness(ctx, cfg, catalog, askConfigured(cfg, readers), logger)
	go followEmbedding(ctx, logger, catalog, liveModels, vectors, readers.retriever, readiness)
	ops := newOperatorServices(cfg, pool, sink, connectors, catalog, serverAuth, logger)
	mux := probeMux(readiness)
	admin, err := mountAdminAPI(mux, cfg, serverAuth, authorizer, connectors, ops, readers, pool, logger)
	if err != nil {
		return err
	}
	// The Jobs page offers to describe images again only while the caption
	// worker can: it needs a model and file storage.
	captionModel := func() string {
		if cfg.ImagesBucket == "" {
			return ""
		}
		return liveModels().caption.ID
	}
	if err := mountBrowserUI(ctx, mux, cfg, serverAuth, connectors, ops, admin, pool, captionModel, logger); err != nil {
		return err
	}
	mirror, err := attachFileStorage(ctx, cfg, admin, logger)
	if err != nil {
		return err
	}
	if err := mountCorpusMCP(ctx, mux, cfg, serverAuth, connectors.registries.Sources, readers, admin, pool, mirror, logger); err != nil {
		return err
	}
	logger.Info("API mounted")
	servers := newListeners(listenAddrs, mux, pagerDutyWebhook(connectors.store, pool, logger))
	listenErr := startListeners(servers, logger)
	// The corpus-state gauges are database reads every replica refreshes.
	go corpus.Run(ctx, pool, corpus.NewMetrics(prometheus.DefaultRegisterer), logger.With("component", "corpus-gauges"))
	asked := startReplicaAsk(ctx, cfg, pool, mirror, readers, connectors, prompts, admin, readiness, liveModels, logger)
	// A clean shutdown stops Ask in order (shutDown); a listener or an
	// election that fails returns at once, and this cancels Ask's turns
	// then, before the pool closes.
	defer func() { asked.stop(time.Now()) }()

	leaderDone, electionErr := startLeaderElection(ctx, logger, cfg, pool, leaderTerms(leaderWork{
		cfg: cfg, logger: logger, pool: pool, vectors: vectors, readers: readers, mirror: mirror,
		prompts: prompts, reports: ops.reports, settings: connectors.store, registries: connectors.registries,
		ask: asked, summaries: ops.sessionSummary, term: connectors.term, slack: connectors.slack, catalog: catalog,
	}, liveModels))

	if drain, err := waitForStop(ctx, listenErr, electionErr, logger); !drain {
		return err
	}
	shutDown(leaderDone, servers, asked, logger)
	return nil
}

// waitForStop blocks until serve must stop: the shutdown signal, after which
// it drains in order, or a listener that fails or an election that cannot
// start, after which it returns err at once. A replica that can never lead
// would serve forever and ingest nothing; exiting puts the cause where the
// operator looks.
func waitForStop(ctx context.Context, listenErr, electionErr <-chan error, logger *slog.Logger) (drain bool, err error) {
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		return true, nil
	case err := <-listenErr:
		if err != nil {
			logger.Error("http server error", "err", err)
		}
		return false, err
	case err := <-electionErr:
		logger.Error("leader election could not start", "err", err)
		return false, err
	}
}

// drainTimeout bounds shutdown's wait, from the signal, for the work in
// progress: the leader's, Slack's answers among it, and the replica's Ask
// turns. It is the Slack consumer's own drain bound; with askStopTimeout and
// listenerGrace after it, it leaves 15 seconds of the pod's 15-minute
// termination grace. Tests shorten it.
var drainTimeout = 14*time.Minute + 30*time.Second

// askStopTimeout bounds the wait, once the drain deadline has passed, for
// the Ask turns still running to end once cancelled.
const askStopTimeout = 5 * time.Second

// listenerGrace bounds the wait, once Ask has stopped, for the requests in
// progress: a /v1/ask whose turn has just ended needs only to write its
// answer, and no other request holds shutdown longer. Tests shorten it.
var listenerGrace = 10 * time.Second

// shutDown stops serve once the signal has ended ctx, and with it the
// leader's work. From then on no Ask turn starts on the replica, and each
// connection closes once it has answered the request it is serving, so a
// client's next request reaches another replica. Until one drain deadline,
// it waits for the leader's work to drain, and then for the replica's Ask
// turns, MCP ask tasks among them, cancelling those left at the deadline.
// The listeners serve requests all the while, so a /v1/ask can write its
// answer and tasks/get keeps answering. Then it shuts the listeners down,
// giving the requests in progress listenerGrace to end.
func shutDown(leaderDone <-chan struct{}, servers []*http.Server, asked *replicaAsk, logger *slog.Logger) {
	deadline := time.Now().Add(drainTimeout)
	asked.close()
	for _, server := range servers {
		server.SetKeepAlivesEnabled(false)
	}
	logger.Info("shutdown: draining the work in progress", "deadline", deadline)
	drain := time.NewTimer(time.Until(deadline))
	select {
	case <-leaderDone:
		drain.Stop()
	case <-drain.C:
		logger.Warn("shutdown: leader workers did not drain before deadline")
	}
	asked.stop(deadline)
	shutDownListeners(servers, listenerGrace)
	logger.Info("shutdown complete")
}

// loadServeConfig reads the configuration, refuses a deployment serve must
// not run in, and loads the authorization policy. No model is configured
// here: providers, models and the embedding model are settings.
func loadServeConfig(logger *slog.Logger) (*config.Config, *authorization.Authorizer, error) {
	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		return nil, nil, err
	}
	if err := checkServeDeployment(cfg, os.Getenv("KUBERNETES_SERVICE_HOST") != ""); err != nil {
		return nil, nil, err
	}
	authorizer, err := authorization.Load(cfg.AuthorizationPolicyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load authorization policy: %w", err)
	}
	return cfg, authorizer, nil
}
