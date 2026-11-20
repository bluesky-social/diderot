package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
)

func TestCheckServeDeployment(t *testing.T) {
	kubernetes := &config.Config{Deployment: config.DeploymentKubernetes}
	standalone := &config.Config{Deployment: config.DeploymentStandalone}

	// The default deployment still refuses a laptop run, as the old
	// DIDEROT_ALLOW_LOCAL_SERVE guard did.
	require.ErrorContains(t, checkServeDeployment(kubernetes, false), "DIDEROT_DEPLOYMENT=standalone")
	require.NoError(t, checkServeDeployment(kubernetes, true))
	require.NoError(t, checkServeDeployment(standalone, false))
	require.NoError(t, checkServeDeployment(standalone, true))
}

// An election that can't start is an error serve stops on, not a log line
// above a replica that serves forever and ingests nothing.
func TestRunLeaderElectionReturnsASetupFailure(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	cfg := &config.Config{
		Deployment: config.DeploymentKubernetes, LeaderElection: true,
		LeaderBackend: config.LeaderBackendKubernetes, Namespace: "diderot", LeaseName: "diderot-leader",
	}
	led := false
	err := runLeaderElection(t.Context(), discardLogger(), cfg, nil, func(context.Context) { led = true })
	require.ErrorContains(t, err, "leader election could not start, so no ingest would ever run: ")
	require.ErrorContains(t, err, "DIDEROT_LEADER_BACKEND=postgres")
	assert.False(t, led)

	// Without an election there is nothing to start: leader work runs inline
	// until the process stops.
	cfg.LeaderElection = false
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, runLeaderElection(ctx, discardLogger(), cfg, nil, func(context.Context) { led = true; cancel() }))
	assert.True(t, led)
}
