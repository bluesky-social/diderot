package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
)

// connectionRetry is how long a build in which a tool connection that is on
// failed to attach is kept before the replica builds Ask again, so a server
// that was down rejoins once it is back.
const connectionRetry = 5 * time.Minute

// replicaAsk is this replica's Ask. Every replica builds it from the Ask
// model and limits the settings name and the tool connections that are on,
// and builds it again when any of them changes, or a while after a
// connection failed to attach: the model's client, the connections, the tools, the prompt
// runtime, and the service. /v1/ask, its artifact route, the MCP ask task,
// and what a leader term lends Slack, scheduled reports and PagerDuty
// (termAsk) run each turn on the build current as the turn starts. A build
// closes its connections once it is no longer current and no turn uses it,
// so a turn finishes on what it started with.
//
// The session lock (ask.Manager) belongs to the replica, not to a build, so
// a session's turns stay serialized across builds as they are across
// replicas.
type replicaAsk struct {
	cfg        *config.Config
	readers    corpusReaders
	registries setup.Registries
	settings   *settings.Store
	prompts    *promptcatalog.Store
	logger     *slog.Logger

	mgr       *ask.Manager
	metrics   *ask.Metrics
	artifacts *ask.ArtifactStore
	// scopeGrants adds the sources' grants to every turn's scopes.
	scopeGrants ask.ScopeGranter

	// turnsCtx ends at stop's deadline, cancelling every turn still running.
	// MCP ask tasks run under it.
	turnsCtx    context.Context
	cancelTurns context.CancelFunc
	// turns are the turns running and the MCP ask tasks held, which stop
	// waits for.
	turns    sync.WaitGroup
	stopOnce sync.Once

	mu sync.Mutex
	// current is the build turns start on: nil before the first.
	current *askBuild
	// changed is closed, and replaced, whenever current changes or the
	// replica closes.
	changed chan struct{}
	// closed is set once the replica starts shutting down: no turn of its
	// own starts, and no MCP ask task is held, after it. A leader term's
	// turns go on until stopping.
	closed bool
	// stopping is set once stop retires the last build: nothing uses a build
	// after it.
	stopping bool
}

// askBuild is one build of a replica's Ask.
type askBuild struct {
	// modelID is the Ask model it was built for, and connections the tool
	// connections (connectionsVersion).
	modelID     string
	connections string
	// service is nil when Ask could not be built; no turn starts on it.
	service *ask.AskService
	// notSetUp says the build has no service because Ask's model or the
	// embedding model is not chosen yet.
	notSetUp bool
	// observability says Ask can delegate to its observability
	// investigator, which has a Grafana connection: PagerDuty
	// investigations need it.
	observability bool
	// mcp are its tool connections.
	mcp askMCPTools
	// builtAt is when it was built.
	builtAt time.Time
	// uses are the turns using it.
	uses sync.WaitGroup
}

// askConfigured reports whether a replica has Ask: the deployment turns it
// on. Until its model and the embedding model are chosen it answers that it
// is not set up.
func askConfigured(cfg *config.Config, readers corpusReaders) bool {
	return cfg.AskEnabled && readers.retriever != nil
}

// startReplicaAsk attaches this replica's Ask to admin and builds it, then
// builds it again whenever the saved Ask model, its limits or the tool
// connections change, until ctx ends. readiness's "ask" turns ready once the first build
// has been tried. It returns nil, and starts nothing, when the replica has
// no Ask.
func startReplicaAsk(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, mirror files.S3Mirror, readers corpusReaders,
	connectors connectorSet, prompts *promptcatalog.Store, admin *adminapi.Server, readiness *httpserver.Readiness,
	liveModels func() *inferenceModels, logger *slog.Logger,
) *replicaAsk {
	if !askConfigured(cfg, readers) {
		if cfg.AskEnabled {
			logger.Warn("ask enabled but retrieval is off (it needs an embeddings model); ask is off")
		}
		return nil
	}
	turnsCtx, cancelTurns := context.WithCancel(context.Background())
	asked := &replicaAsk{
		cfg: cfg, readers: readers, registries: connectors.registries, settings: connectors.store, prompts: prompts,
		logger:    logger,
		mgr:       ask.NewManager(pool, logger.With("component", "ask-mgr")),
		metrics:   ask.NewMetrics(prometheus.DefaultRegisterer),
		artifacts: ask.NewArtifactStore(pool, mirror),
		scopeGrants: sourceScopeGrants{
			sources: connectors.registries.Sources, deps: source.Deps{Pool: pool, Logger: logger},
			logger: logger.With("component", "scope-grants"),
		},
		turnsCtx: turnsCtx, cancelTurns: cancelTurns,
		changed: make(chan struct{}),
	}
	admin.SetAskRunner(turnsCtx, asked)
	go asked.run(ctx, liveModels, askReadiness(readiness))
	return asked
}

// askReadiness sets readiness's "ask" from each build of the replica's Ask:
// ready once Ask is built, or has nothing to build before its models are
// chosen. A replica whose first build fails with both chosen can't answer,
// and is not ready until a build works. A later build that fails, after a
// change on the admin pages, leaves the replica ready: it still serves
// those pages, where the change is put right.
func askReadiness(readiness *httpserver.Readiness) func(*askBuild) {
	ready := false
	return func(built *askBuild) {
		switch {
		case built.service != nil || built.notSetUp:
			ready = true
			readiness.SetReady("ask", true)
		case !ready:
			readiness.SetNotReady("ask", "Ask could not be built with its saved model, "+built.modelID+": this replica's log says why.")
		}
	}
}

// run builds Ask for the Ask model and limits the settings name and the
// tool connections that are on, then builds it again whenever any changes,
// or connectionRetry after a build in which a connection failed to attach,
// until ctx ends. tried is called once the first build has been tried,
// whether or not it succeeded.
func (a *replicaAsk) run(ctx context.Context, liveModels func() *inferenceModels, tried func(*askBuild)) {
	models := liveModels()
	connections := a.connections()
	built := a.build(ctx, models, connections)
	a.publish(built)
	tried(built)
	ticker := time.NewTicker(modelsWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		nextModels, nextConnections := liveModels(), a.connections()
		switch {
		case !sameAskModel(models, nextModels):
			a.logger.Info("the saved Ask model or its limits changed; building Ask with them", "model", nextModels.ask.ID)
		case connectionsVersion(nextConnections) != built.connections:
			a.logger.Info("the tool connections changed; building Ask with them")
		case built.mcp.failed && time.Since(built.builtAt) >= connectionRetry:
			a.logger.Info("a tool connection failed to attach; building Ask again to retry it")
		default:
			continue
		}
		models, connections = nextModels, nextConnections
		built = a.build(ctx, models, connections)
		a.publish(built)
		tried(built)
	}
}

// connections are the tool connections that are on now. Those whose
// credential can't be read are logged at each build and left out.
func (a *replicaAsk) connections() []askconn.Connection {
	connections, _ := liveConnections(a.settings)
	return connections
}

// build builds Ask on models' Ask model with connections. Ask needs
// retrieval. A failure is logged and leaves the build without a service,
// so no turn starts on it.
func (a *replicaAsk) build(ctx context.Context, models *inferenceModels, connections []askconn.Connection) *askBuild {
	logger, limits := a.logger, models.askLimits
	b := &askBuild{modelID: models.ask.ID, connections: connectionsVersion(connections), builtAt: time.Now()}
	if _, unreadable := liveConnections(a.settings); len(unreadable) > 0 {
		logger.Error("tool connections left out: their saved credential can't be read", "connections", unreadable)
	}
	if models.ask.ID == "" || models.embed.ID == "" {
		logger.Info("ask: not set up: Ask's model and the embedding model are chosen on the Models page; ask service not attached",
			"ask_model", models.ask.ID, "embedding_model", models.embed.ID)
		b.notSetUp = true
		return b
	}
	// The reasoning effort is the run's (ask.Limits), not the client's.
	model, err := ask.NewModel(ctx, ask.ModelConfig{
		BaseURL:     models.ask.endpoint.BaseURL,
		APIKey:      models.ask.endpoint.LibraryKey(),
		HTTPClient:  models.ask.endpoint.HTTPClient(nil),
		Auth:        models.ask.endpoint.Auth,
		Model:       models.ask.Model,
		Timeout:     limits.ModelTimeout,
		MaxTokens:   limits.MaxOutputTokens,
		ServiceTier: limits.ServiceTier,
	})
	if err != nil {
		logger.Error("ask: init failed; ask service not attached", "err", err)
		return b
	}
	registrations, mcpTools := askTools(ctx, limits, connections, model, a.artifacts, a.metrics, a.readers, a.registries, a.settings, logger)
	b.mcp = mcpTools
	b.observability = mcpTools.investigate != nil && mcpTools.grafana
	einoTools, presentations, err := ask.NewToolRegistry(ctx, registrations)
	if err != nil {
		logger.Error("ask: tool registration failed; ask service not attached", "err", err)
		return b
	}
	// Eino counts model generations, while the tool-turn limit counts
	// completed tool batches, and the budget middleware forces the final
	// answer at each run's own limit. The cap is the largest limit plus
	// that one tool-free synthesis call.
	promptRuntime, err := ask.NewPromptRuntime(a.prompts, model, einoTools, settings.MaxAskToolTurns+1)
	if err != nil {
		logger.Error("ask: prompt runtime init failed; ask service not attached", "err", err)
		return b
	}
	service, err := ask.NewAskService(ask.AskServiceOptions{
		Manager:       a.mgr,
		PromptRuntime: promptRuntime,
		Model:         models.ask.Model,
		ServiceTier:   limits.ServiceTier,
		Presentations: presentations,
		Logger:        logger.With("component", "ask"),
		Metrics:       a.metrics,
		Limits:        askLimits(a.settings),
		References:    a.readers.references,
		LinearTeams:   a.readers.linearTeams,
		Artifacts:     a.artifacts,
		TurnDeadline:  limits.TurnDeadline,
		OrgName:       a.settings.OrgName,
		Sources:       a.registries.Sources.Infos(),
		ScopeGrants:   a.scopeGrants,
	})
	if err != nil {
		logger.Error("ask: service init failed; ask service not attached", "err", err)
		return b
	}
	b.service = service
	logger.Info("ask service attached", "model", models.ask.ID, "service_tier", limits.ServiceTier)
	return b
}

// publish makes next the build turns start on and retires the one it
// replaces. Once the replica has closed it retires next at once.
func (a *replicaAsk) publish(next *askBuild) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		go next.retire()
		return
	}
	prev := a.current
	a.current = next
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	if prev != nil {
		go prev.retire()
	}
}

// retire closes b's MCP connections once nothing uses b. Only a build that
// is no longer current, or the last one once stop has begun, is retired,
// so no new use can start meanwhile.
func (b *askBuild) retire() {
	b.uses.Wait()
	b.mcp.close()
}

// termAsk is the Ask a leader term lends Slack, scheduled reports and
// PagerDuty. Each call runs on the replica's build current as it starts, so
// a change to the Ask model or the tool connections reaches them without the
// term ending. Unlike the replica's own turns, it goes on once the replica
// has closed, until stop: the leader's work drains first.
type termAsk struct{ a *replicaAsk }

// forTerm returns the Ask a leader term lends, once the replica has tried
// its first build, and false with no Ask on the replica or once ctx ends.
func (a *replicaAsk) forTerm(ctx context.Context) (termAsk, bool) {
	if a == nil {
		return termAsk{}, false
	}
	for {
		a.mu.Lock()
		if a.current != nil || a.closed {
			a.mu.Unlock()
			return termAsk{a: a}, true
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return termAsk{}, false
		case <-changed:
		}
	}
}

// use calls fn with the current build's service, holding the build used
// until fn returns.
func (t termAsk) use(fn func(*ask.AskService) error) error {
	t.a.mu.Lock()
	b := t.a.current
	if t.a.stopping || b == nil || b.service == nil {
		t.a.mu.Unlock()
		if b != nil && b.notSetUp {
			return ask.ErrNotSetUp
		}
		return ask.ErrUnavailable
	}
	b.uses.Add(1)
	t.a.mu.Unlock()
	defer b.uses.Done()
	return fn(b.service)
}

// Observability reports that the current build can delegate to its
// observability investigator with a Grafana connection.
func (t termAsk) Observability() bool {
	t.a.mu.Lock()
	defer t.a.mu.Unlock()
	return t.a.current != nil && t.a.current.observability
}

func (t termAsk) ExecuteTurn(ctx context.Context, req ask.TurnRequest) (result ask.TurnResult, err error) {
	err = t.use(func(s *ask.AskService) error {
		result, err = s.ExecuteTurn(ctx, req)
		return err
	})
	return result, err
}

func (t termAsk) CreateSession(ctx context.Context) (id uuid.UUID, err error) {
	err = t.use(func(s *ask.AskService) error { id, err = s.CreateSession(ctx); return err })
	return id, err
}

func (t termAsk) LookupOrCreateSlackSession(ctx context.Context, channelID, threadTS, askerEmail string) (id uuid.UUID, err error) {
	err = t.use(func(s *ask.AskService) error {
		id, err = s.LookupOrCreateSlackSession(ctx, channelID, threadTS, askerEmail)
		return err
	})
	return id, err
}

func (t termAsk) SlackSessionState(ctx context.Context, channelID, threadTS string) (known bool, through time.Time, err error) {
	err = t.use(func(s *ask.AskService) error {
		known, through, err = s.SlackSessionState(ctx, channelID, threadTS)
		return err
	})
	return known, through, err
}

func (t termAsk) LoadResultArtifacts(ctx context.Context, sessionID uuid.UUID, assistantTurn int) (artifacts []ask.ArtifactDescriptor, err error) {
	err = t.use(func(s *ask.AskService) error {
		artifacts, err = s.LoadResultArtifacts(ctx, sessionID, assistantTurn)
		return err
	})
	return artifacts, err
}

func (t termAsk) ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (record ask.ArtifactRecord, data []byte, err error) {
	err = t.use(func(s *ask.AskService) error {
		record, data, err = s.ReadArtifact(ctx, sessionID, artifactID)
		return err
	})
	return record, data, err
}

func (t termAsk) ArtifactDeliveryExists(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target string) (exists bool, err error) {
	err = t.use(func(s *ask.AskService) error {
		exists, err = s.ArtifactDeliveryExists(ctx, sessionID, assistantTurn, artifactID, surface, target)
		return err
	})
	return exists, err
}

func (t termAsk) RecordArtifactDelivery(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target, externalID, status, failure string) error {
	return t.use(func(s *ask.AskService) error {
		return s.RecordArtifactDelivery(ctx, sessionID, assistantTurn, artifactID, surface, target, externalID, status, failure)
	})
}

// startTurn returns the build a turn starts on, used by the turn until it
// calls done. ok is false while the replica has no build with a service,
// and once it has closed.
func (a *replicaAsk) startTurn() (b *askBuild, done func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.current == nil || a.current.service == nil {
		return nil, nil, false
	}
	b = a.current
	b.uses.Add(1)
	a.turns.Add(1)
	return b, func() {
		b.uses.Done()
		a.turns.Done()
	}, true
}

// Hold admits an MCP ask task, which stop waits for until the task calls
// release, once its result is recorded. ok is false while the replica has no
// build with a service, and once it has closed.
func (a *replicaAsk) Hold() (release func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.current == nil || a.current.service == nil {
		return nil, false
	}
	a.turns.Add(1)
	return sync.OnceFunc(a.turns.Done), true
}

// close starts the replica's shutdown: once it returns, no turn of its own
// starts and no MCP ask task is held. The turns running, and a leader term's
// Ask, go on.
func (a *replicaAsk) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	close(a.changed)
	a.changed = make(chan struct{})
}

// stop closes the replica's Ask and waits until deadline for the turns
// running and the MCP ask tasks held to end. Then it cancels the turns still
// running and waits up to askStopTimeout for them to return, which releases
// their sessions' leases, and for the last build's MCP connections to close.
// Only the first call does anything.
func (a *replicaAsk) stop(deadline time.Time) {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		a.close()
		a.mu.Lock()
		last := a.current
		a.stopping = true
		a.mu.Unlock()
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			a.turns.Wait()
			if last != nil {
				last.retire()
			}
		}()
		drained := time.NewTimer(time.Until(deadline))
		defer drained.Stop()
		select {
		case <-ended:
		case <-drained.C:
			a.logger.Warn("ask: shutdown deadline reached; cancelling the turns still running")
		}
		a.cancelTurns()
		select {
		case <-ended:
		case <-time.After(askStopTimeout):
			a.logger.Warn("ask: still in use as the replica stops", "timeout", askStopTimeout)
		}
	})
}

// ExecuteTurn runs one turn on the current build, cancelled if the replica
// stops first. With no build, or once the replica has closed, it returns
// ask.ErrUnavailable.
func (a *replicaAsk) ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	b, done, ok := a.startTurn()
	if !ok {
		return ask.TurnResult{}, ask.ErrUnavailable
	}
	defer done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancelling := context.AfterFunc(a.turnsCtx, cancel)
	defer stopCancelling()
	return b.service.ExecuteTurn(ctx, req)
}

// ValidateModelOverrides checks overrides against the current build's
// model. With no build, or once the replica has closed, it returns
// ask.ErrUnavailable, so /v1/ask refuses a turn before it creates a session.
func (a *replicaAsk) ValidateModelOverrides(overrides ask.ModelOverrides) error {
	a.mu.Lock()
	b, closed := a.current, a.closed
	a.mu.Unlock()
	if closed || b == nil || b.service == nil {
		if !closed && b != nil && b.notSetUp {
			return ask.ErrNotSetUp
		}
		return ask.ErrUnavailable
	}
	return b.service.ValidateModelOverrides(overrides)
}

// CreateSession creates an empty session.
func (a *replicaAsk) CreateSession(ctx context.Context) (uuid.UUID, error) {
	sess, err := a.mgr.Create(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return sess.ID, nil
}

// CreateOwnedSession creates an empty session owned by ownerEmail.
func (a *replicaAsk) CreateOwnedSession(ctx context.Context, ownerEmail string) (uuid.UUID, error) {
	sess, err := a.mgr.CreateOwned(ctx, ownerEmail)
	if err != nil {
		return uuid.Nil, err
	}
	return sess.ID, nil
}

// The API offers MCP's ask only while its Ask is an ask.CorpusAskRunner.
var _ ask.CorpusAskRunner = (*replicaAsk)(nil)

// OwnsSession reports whether ownerEmail may continue sessionID and read its
// artifacts.
func (a *replicaAsk) OwnsSession(ctx context.Context, sessionID uuid.UUID, ownerEmail string) (bool, error) {
	return a.mgr.OwnsSession(ctx, sessionID, ownerEmail)
}

// ReadArtifact reads one of a session's stored artifacts.
func (a *replicaAsk) ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ask.ArtifactRecord, []byte, error) {
	if a.artifacts == nil {
		return ask.ArtifactRecord{}, nil, errors.New("ask artifact store is unavailable")
	}
	return a.artifacts.ReadArtifact(ctx, sessionID, artifactID)
}

// askTools is the tools Ask registers, in the order the model sees them:
// retrieve (filter and vector search) and list (filter-only enumeration,
// through the retriever's Postgres-only path); investigate_observability
// with a tool connection for the investigator; expand, which hands each
// event to its source's Expand; each tool connector's tools; and the tools
// of the connections Ask itself uses. A run offers a connector's tools only
// while it is enabled, with the settings cached as the run starts. It
// returns the connections, connected, too.
func askTools(ctx context.Context, limits settings.AskLimits, connections []askconn.Connection, model *ask.ConfiguredModel, artifacts *ask.ArtifactStore,
	metrics *ask.Metrics, readers corpusReaders, registries setup.Registries, store *settings.Store, logger *slog.Logger,
) ([]ask.RegisteredTool, askMCPTools) {
	infos := registries.Sources.Infos()
	registrations := []ask.RegisteredTool{
		ask.NewEinoRetrieveTool(readers.retriever, infos),
		ask.NewEinoListTool(readers.retriever, infos),
	}
	mcpTools := buildAskMCPTools(ctx, limits, connections, logger, askMCPToolsOptions{
		Model: model, Artifacts: artifacts, Metrics: metrics,
	})
	if mcpTools.investigate != nil {
		registrations = append(registrations, *mcpTools.investigate)
	}
	// sink is the ScopeLookup: scoped turns check events.scope before any
	// content fetch.
	registrations = append(registrations, ask.NewEinoExpandTool(readers.expander, readers.sink, infos))
	registrations = append(registrations, registries.Tools.Registrations(store)...)
	return append(registrations, mcpTools.root...), mcpTools
}

// askLimits reads Ask's limits from the settings cache, for each run.
func askLimits(store *settings.Store) func() ask.Limits {
	return func() ask.Limits {
		models := store.Models().Models
		return ask.Limits{ReasoningEffort: models.AskReasoningEffort, MaxToolTurns: models.MaxToolTurns()}
	}
}
