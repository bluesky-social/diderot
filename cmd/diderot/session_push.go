package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/worker"
)

// newSessionPushStore builds the push store when
// DIDEROT_SESSION_INGEST_SOURCE=push and returns nil otherwise. A push
// deployment never polls the session feed: the two session sources are
// mutually exclusive.
func newSessionPushStore(cfg *config.Config, pool *pgxpool.Pool) (*sessioningest.PushStore, error) {
	if !cfg.SessionPush.Enabled {
		return nil, nil
	}
	policy, err := configuredSessionIngestPolicy(cfg)
	if err != nil {
		return nil, err
	}
	p := cfg.SessionPush
	return sessioningest.NewPushStore(pool, sessioningest.PushConfig{
		RevisionKey:         p.RevisionKey,
		MaxDocumentBytes:    p.MaxDocumentBytes,
		OwnerQuotaBytes:     p.OwnerQuotaBytes,
		OwnerQuotaRevisions: p.OwnerQuotaRevisions,
		Policy:              policy,
		Formats:             sessioningest.PushFormats(),
	})
}

// sessionPushRetentionRunner prunes superseded revisions older than the
// operator's retention window (leader only).
func sessionPushRetentionRunner(store *sessioningest.PushStore, retention time.Duration, logger *slog.Logger) worker.Runner {
	log := logger.With("component", "session-push-retention")
	return worker.Runner{
		Name: "session-push-retention", Interval: 15 * time.Minute, BootTick: true,
		MinBackoff: time.Minute, MaxBackoff: time.Hour, Logger: log,
		Run: func(ctx context.Context) error {
			for {
				n, err := store.PruneSuperseded(ctx, retention, 200)
				if n > 0 {
					log.Info("pruned superseded session revisions", "revisions", n, "retention", retention)
				}
				if err != nil || n < 200 {
					return err
				}
			}
		},
	}
}

// newSessionPushHandler builds the HTTP side of push ingest, or nil when push
// is off. Startup fails on any invalid push setting.
func newSessionPushHandler(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (*sessioningest.PushHandler, error) {
	store, err := newSessionPushStore(cfg, pool)
	if err != nil || store == nil {
		return nil, err
	}
	p := cfg.SessionPush
	handler, err := sessioningest.NewPushHandler(store, sessioningest.PushLimits{
		MaxConcurrent:         p.MaxConcurrent,
		MaxConcurrentPerOwner: p.MaxConcurrentPerOwner,
		MaxInflightBytes:      p.MaxInflightBytes,
	}, logger.With("component", "session-push"))
	if err != nil {
		return nil, err
	}
	logger.Info("session push ingest enabled", "formats", store.Formats(),
		"max_document_bytes", p.MaxDocumentBytes, "superseded_retention", p.SupersededRetention)
	return handler, nil
}
