package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/sessiontitle"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// summaryThresholds reads when a session is summarized from the settings
// cache, which every replica's poll keeps current, so a change saved on the
// Jobs and maintenance page reaches the leader without a restart. A nil
// store, in tests, is the default.
func summaryThresholds(store *settings.Store) func() sessioningest.SummarizePolicy {
	return func() sessioningest.SummarizePolicy {
		if store == nil {
			return sessioningest.DefaultSummarizePolicy()
		}
		schedule := store.SessionSummaries().EffectiveSchedule()
		return sessioningest.SummarizePolicy{
			QuietFor:             time.Duration(schedule.QuietFor),
			MinAddedUserTurns:    schedule.MinAddedUserTurns,
			MinAddedRecords:      schedule.MinAddedRecords,
			MinAddedContentBytes: schedule.MinAddedBytes,
			MaxPendingFor:        time.Duration(schedule.MaxPendingFor),
		}
	}
}

// sessionIngest builds the leader's session ingest worker. It reads one
// source: the session feed, while that connector runs, or pushed sessions
// (DIDEROT_SESSION_INGEST_SOURCE=push), on every leader term.
type sessionIngest struct {
	cfg     *config.Config
	models  *inferenceModels
	pool    *pgxpool.Pool
	prompts *promptcatalog.Store
	// push is the push store, nil unless push ingest is on; pushErr is why
	// it could not be built.
	push    *sessioningest.PushStore
	pushErr error
	control sessioningest.SummaryControl
	// thresholds reads when a session is summarized, as saved on the Jobs
	// and maintenance page.
	thresholds func() sessioningest.SummarizePolicy
	logger     *slog.Logger
}

// register adds the worker, writing sessions through sink, to sup. With a
// model for session summaries it summarizes them, and adds the
// agent-session title backfill and the summary repair; with none it only
// records the sessions it sees, and the Models page ends the term when one
// is chosen. feed is nil for
// push ingest; viewerBaseURL is the session viewer citations link to. It
// fails closed: an invalid policy, a missing revision size cap on the feed,
// or a summarization prerequisite that is missing registers nothing and
// returns why. Never fall back to a policy the operator did not author, and
// never index without a bounded decompression cap.
func (si *sessionIngest) register(sup *worker.Supervisor, sink *events.DBSink, feed sessioningest.Feed, viewerBaseURL string) error {
	cfg, models, logger := si.cfg, si.models, si.logger
	policy, err := configuredSessionIngestPolicy(cfg)
	switch {
	case err != nil:
	case feed == nil && si.pushErr != nil:
		err = si.pushErr
	case feed == nil && si.push == nil:
		err = errors.New("session ingest has no source: enable the session feed connector, or set DIDEROT_SESSION_INGEST_SOURCE=push")
	case feed != nil && cfg.SessionIngestMaxRevisionBytes == 0:
		err = errors.New("DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES must be set and non-zero to read the session feed")
	}
	var summarizer sessioningest.Summarizer
	var titles sessiontitle.Generator
	summarize := err == nil && sessionSummariesEnabled(models)
	if summarize {
		summarizer, titles, err = si.summarizers()
	}
	if err != nil {
		return fmt.Errorf("session ingest not started (policy %q): %w", cfg.SessionIngestPolicyPath, err)
	}

	ingest := &sessioningest.Worker{
		Cfg: sessioningest.WorkerConfig{
			TickInterval:     cfg.SessionIngestTick,
			PageSize:         cfg.SessionIngestPageSize,
			BatchSize:        cfg.SessionIngestBatchSize,
			Concurrency:      cfg.SessionIngestConcurrency,
			ChunkBatchSize:   cfg.SessionIngestChunkBatchSize,
			Policy:           policy,
			Thresholds:       si.thresholds,
			Summarize:        summarize,
			MaxRevisionBytes: cfg.SessionIngestMaxRevisionBytes,
			ViewerBaseURL:    viewerBaseURL,
		},
		Store:          sessioningest.NewStore(si.pool),
		Summarize:      summarizer,
		TitleGenerator: titles,
		Logger:         logger.With("component", "sessioningest"),
		Metrics:        sessioningest.NewMetrics(prometheus.DefaultRegisterer),
		Control:        si.control,
	}
	if feed != nil {
		ingest.Feed = feed
	} else {
		ingest.Push = si.push
	}
	if summarize {
		publisher := sessioningest.NewEventPublisher(sink, sessioningest.NewStore(si.pool))
		publisher.ViewerBaseURL = viewerBaseURL
		ingest.Publish = publisher
		if titles != nil {
			titleWorker := &sessiontitle.AgentSessionWorker{
				Pool: si.pool, Generator: titles, BatchSize: 100, Concurrency: 10,
				Logger: logger.With("component", "agent-session-title"),
			}
			sup.AddRunner(worker.Runner{
				Name: "agent-session-title", Interval: 10 * time.Second, BootTick: true,
				MinBackoff: 15 * time.Second, MaxBackoff: 5 * time.Minute,
				Logger: titleWorker.Logger, Run: func(ctx context.Context) error {
					if !sessionSummaryControlEnabled(ctx, si.control, logger) {
						return nil
					}
					return titleWorker.Tick(ctx)
				},
			})
			logger.Info("agent session title backfill enabled", "model", models.title.ID,
				"batch_size", 100, "concurrency", 10)
		}
	}
	if summarize {
		repair := &sessioningest.SummaryRepairWorker{
			Ingest: ingest, Jobs: adminjob.NewStore(si.pool),
			Logger: logger.With("component", "session-summary-repair"),
		}
		sup.AddRunner(worker.Runner{
			Name: "session-summary-repair", Interval: 5 * time.Second, BootTick: true,
			Logger: repair.Logger, Run: func(ctx context.Context) error {
				// A repair may call the summary model, so it waits while
				// summaries are paused.
				if !sessionSummaryControlEnabled(ctx, si.control, logger) {
					return nil
				}
				return repair.Tick(ctx)
			},
		})
	}
	sup.AddRunner(worker.Runner{
		Name: "session-ingest", Interval: cfg.SessionIngestTick, BootTick: true,
		Logger: logger.With("component", "sessioningest"), Run: ingest.Tick,
	})
	mode := "shadow"
	if summarize {
		mode = "summarize"
	}
	source := "feed"
	if feed == nil {
		source = "push"
	}
	logger.Info("sessioningest worker enabled ("+mode+")", "source", source,
		"tick", cfg.SessionIngestTick, "policy_hash", policy.Hash(), "summarize", summarize)
	return nil
}

// summarizers builds the map/reduce summarizer and, when a provider serves
// responses, the agent-session title generator.
func (si *sessionIngest) summarizers() (sessioningest.Summarizer, sessiontitle.Generator, error) {
	cfg, models := si.cfg, si.models
	if !sessionSummariesEnabled(models) {
		return nil, nil, errors.New("session summaries need a configured model registry provider serving " + summaryProtocolNames())
	}
	gatewayClient, protocols, err := newSessionSummaryClient(models)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize session-ingest gateway client: %w", err)
	}
	si.logger.Info("session summary models",
		"map_model", models.summaryMap.ID, "map_protocol", protocols[models.summaryMap.ID],
		"reduce_model", models.summaryReduce.ID, "reduce_protocol", protocols[models.summaryReduce.ID])
	summarizer := sessioningest.NewSessionsummarySummarizer(sessioningest.NewStore(si.pool), si.prompts,
		gatewayClient, models.summaryMap.ID, models.summaryReduce.ID, 0, cfg.SessionIngestChunkConcurrency)
	if !sessionTitlesEnabled(models) {
		si.logger.Info("agent session titles disabled (needs a configured model registry provider serving responses)")
		return summarizer, nil, nil
	}
	titles, err := newTitleGenerator(models.title, "")
	if err != nil {
		si.logger.Error("agent session title generator init failed; disabled", "err", err)
		return summarizer, nil, nil
	}
	return summarizer, titles, nil
}

func configuredSessionIngestPolicy(cfg *config.Config) (sessioningest.Policy, error) {
	if cfg.SessionIngestPolicyPath != "" {
		policy, _, err := sessioningest.LoadPolicyFile(cfg.SessionIngestPolicyPath)
		return policy, err
	}
	policy := sessioningest.DefaultPolicy()
	if cfg.SessionIngestCodexApp != "" {
		policy.Include.Clients = append(policy.Include.Clients, cfg.SessionIngestCodexApp)
	}
	if cfg.SessionIngestExcludeApp != "" {
		policy.Exclude.Apps = append(policy.Exclude.Apps, cfg.SessionIngestExcludeApp)
	}
	if err := policy.Validate(); err != nil {
		return sessioningest.Policy{}, err
	}
	return policy, nil
}

func sessionSummaryControlEnabled(ctx context.Context, control sessioningest.SummaryControl, logger *slog.Logger) bool {
	state, err := control.State(ctx)
	if err != nil {
		logger.Warn("agent session title control unavailable; skipping title batch", "err", err)
		return false
	}
	return state.EffectiveEnabled
}
