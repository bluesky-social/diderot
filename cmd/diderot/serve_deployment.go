package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/leader"
)

// checkServeDeployment refuses to serve outside Kubernetes unless the
// operator declared a standalone deployment.
//
// Slack spreads Socket Mode events round-robin across every connection
// holding the app's token, so a stray process with production credentials,
// on a laptop say, takes a share of the events the deployment then never
// sees, while its own socket looks healthy. Leader election cannot prevent
// it: such a process is outside the deployment's election whichever backend
// it uses, so the guard is a deliberate declaration instead.
// KUBERNETES_SERVICE_HOST is set on every pod by the kubelet and never on a
// laptop, so an undeclared run with production credentials in the shell
// stops here.
func checkServeDeployment(cfg *config.Config, inKubernetes bool) error {
	if cfg.Deployment == config.DeploymentKubernetes && !inKubernetes {
		return errors.New(
			"refusing to run `serve` outside Kubernetes — set " +
				"DIDEROT_DEPLOYMENT=standalone for a deliberate deployment " +
				"elsewhere (and make sure no other deployment shares its " +
				"Slack app)")
	}
	return nil
}

// startLeaderElection runs runLeaderElection in the background. done closes
// once it has returned, and with it every leader term; failed receives the
// error of an election that cannot start.
func startLeaderElection(ctx context.Context, logger *slog.Logger, cfg *config.Config, pool *pgxpool.Pool, leaderFn func(context.Context)) (done <-chan struct{}, failed <-chan error) {
	returned := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		defer close(returned)
		if err := runLeaderElection(ctx, logger, cfg, pool, leaderFn); err != nil {
			errs <- err
		}
	}()
	return returned, errs
}

// runLeaderElection runs leaderFn under the configured election and blocks
// until ctx is canceled and leaderFn has returned. It returns an error when
// the election cannot start, which no retry fixes: the replica would serve
// requests but never run Slack, embedding, or any other leader work, so
// serve stops instead. Failures once the election runs are retried inside it.
func runLeaderElection(ctx context.Context, logger *slog.Logger, cfg *config.Config, pool *pgxpool.Pool, leaderFn func(context.Context)) error {
	var err error
	switch {
	case !cfg.LeaderElection:
		logger.Warn("leader election disabled; any second process running the same Slack app " +
			"will split Slack events with this one")
		leader.RunInline(ctx, logger, leaderFn)
	case cfg.LeaderBackend == config.LeaderBackendPostgres:
		err = leader.RunPostgres(ctx, logger, pool, leader.PostgresOptions{Name: cfg.LeaseName}, leaderFn)
	default:
		err = leader.Run(ctx, logger, cfg.Namespace, cfg.LeaseName, leaderFn)
	}
	if err != nil {
		return fmt.Errorf("leader election could not start, so no ingest would ever run: %w", err)
	}
	return nil
}
