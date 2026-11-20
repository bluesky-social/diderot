package leader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Lease election that can't load the pod's credentials can never lead, so
// Run returns the setup error, saying what to fix, instead of contending.
func TestRunFailsWithoutInClusterCredentials(t *testing.T) {
	// rest.InClusterConfig reads these first; unset, it fails as off a
	// cluster or in a pod without its service account mounted.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	led := false
	err := Run(t.Context(), testLogger(t), "diderot", "diderot-leader", func(context.Context) { led = true })
	require.ErrorContains(t, err, "can't load this pod's service account")
	require.ErrorContains(t, err, "DIDEROT_LEADER_BACKEND=postgres")
	assert.False(t, led)
}

// A Postgres too old for idle_session_timeout can't hold a lock whose holder
// may go silent, and no retry makes it newer: RunPostgres fails at once,
// naming the minimum, and never contends.
func TestRunPostgresRefusesPostgresBefore14(t *testing.T) {
	opts := fastOptions("too-old", "a")
	calls := 0
	opts.serverVersion = func(context.Context) (int, error) { calls++; return 130012, nil }
	led := false
	err := RunPostgres(t.Context(), testLogger(t), nil, opts, func(context.Context) { led = true })
	require.EqualError(t, err, "the Postgres leader election needs Postgres 14 or later, and this database is Postgres 13; "+
		"upgrade it, or set DIDEROT_LEADER_ELECTION=false if this is the only process that runs serve against it")
	assert.Equal(t, 1, calls)
	assert.False(t, led)
}

// A version that can't be read is retried, as a failed term is, until it
// reads or the process stops; only a version that reads too old fails.
func TestRunPostgresRetriesAnUnreadableVersion(t *testing.T) {
	opts := fastOptions("transient", "a")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	opts.serverVersion = func(context.Context) (int, error) {
		calls++
		if calls < 3 {
			return 0, errors.New("connection reset")
		}
		cancel() // the process stops before contending
		return 180000, nil
	}
	done := make(chan error, 1)
	go func() { done <- RunPostgres(ctx, testLogger(t), nil, opts, func(context.Context) { t.Error("led") }) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RunPostgres did not return")
	}
	assert.Equal(t, 3, calls)
}

func TestPostgresMajor(t *testing.T) {
	for num, want := range map[int]string{180001: "18", 140000: "14", 130012: "13", 100023: "10", 90624: "9.6"} {
		assert.Equal(t, want, postgresMajor(num), num)
	}
}

// The default reads server_version_num from the database itself.
func TestPostgresServerVersionReadsTheServer(t *testing.T) {
	pool := newPool(t, testURL(t))
	version, err := postgresServerVersion(t.Context(), pool)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, version, minPostgresVersion, "the development database is Postgres 14 or later")
}
