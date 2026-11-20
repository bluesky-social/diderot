package adminjob

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestStoreLifecycleAndExclusiveKind(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminjob_lifecycle"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)

	job, err := store.Start(ctx, "reembed", "operator@example.com", map[string]any{
		"sources": []string{"slack"},
	}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "running", job.Status)
	assert.Equal(t, "operator@example.com", job.RequestedBy)
	assert.NotNil(t, job.StartedAt)
	assert.Nil(t, job.FinishedAt)
	assert.JSONEq(t, `{"sources":["slack"]}`, string(job.Request))

	_, err = store.Start(ctx, "reembed", "second@example.com", nil, time.Minute)
	require.ErrorIs(t, err, ErrActive)

	progress := map[string]any{"processed": 7, "total": 12}
	require.NoError(t, store.Heartbeat(ctx, job.ID, progress))
	require.NoError(t, store.Finish(ctx, job.ID, "succeeded", progress, nil))
	require.ErrorIs(t, store.Finish(ctx, job.ID, "failed", nil, errors.New("late")), ErrNotRunning)

	latest, err := store.Latest(ctx, "reembed")
	require.NoError(t, err)
	assert.Equal(t, job.ID, latest.ID)
	assert.Equal(t, "succeeded", latest.Status)
	assert.NotNil(t, latest.FinishedAt)
	assert.JSONEq(t, `{"processed":7,"total":12}`, string(latest.Progress))
}

func TestStartExpiresStaleJob(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminjob_stale"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)

	stale, err := store.Start(ctx, "reembed", "old@example.com", nil, time.Minute)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET heartbeat_at = now() - interval '2 hours' WHERE id = $1`, stale.ID)
	require.NoError(t, err)

	fresh, err := store.Start(ctx, "reembed", "new@example.com", nil, time.Minute)
	require.NoError(t, err)
	assert.NotEqual(t, stale.ID, fresh.ID)

	var status, errorText string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT status, COALESCE(error, '') FROM admin_jobs WHERE id = $1
	`, stale.ID).Scan(&status, &errorText))
	assert.Equal(t, "failed", status)
	assert.Contains(t, errorText, "heartbeat expired")
}

// TestEnqueuedJobWaitsForItsClaim: a queued job holds its kind, is never
// expired while it waits, and starts its heartbeat when it is claimed. A new
// request still replaces a running job whose executor went away.
func TestEnqueuedJobWaitsForItsClaim(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminjob_queue"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)

	_, claimed, err := store.Claim(ctx, "reembed")
	require.NoError(t, err)
	require.False(t, claimed, "nothing is queued")

	queued, err := store.Enqueue(ctx, "reembed", "operator@example.com", map[string]any{"sources": []string{"slack"}}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "queued", queued.Status)
	assert.Nil(t, queued.StartedAt)
	require.ErrorIs(t, store.Heartbeat(ctx, queued.ID, nil), ErrNotRunning)

	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET heartbeat_at = now() - interval '2 hours' WHERE id = $1`, queued.ID)
	require.NoError(t, err)
	_, err = store.Enqueue(ctx, "reembed", "second@example.com", nil, time.Minute)
	require.ErrorIs(t, err, ErrActive)
	_, err = store.Start(ctx, "reembed", "second@example.com", nil, time.Minute)
	require.ErrorIs(t, err, ErrActive)

	running, claimed, err := store.Claim(ctx, "reembed")
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, queued.ID, running.ID)
	assert.Equal(t, "running", running.Status)
	assert.NotNil(t, running.StartedAt)
	assert.WithinDuration(t, time.Now(), running.HeartbeatAt, time.Minute)
	assert.JSONEq(t, `{"sources":["slack"]}`, string(running.Request))
	_, claimed, err = store.Claim(ctx, "reembed")
	require.NoError(t, err)
	require.False(t, claimed, "a job is claimed once")

	_, err = pool.Exec(ctx, `UPDATE admin_jobs SET heartbeat_at = now() - interval '2 hours' WHERE id = $1`, running.ID)
	require.NoError(t, err)
	next, err := store.Enqueue(ctx, "reembed", "second@example.com", nil, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "queued", next.Status)
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM admin_jobs WHERE id = $1`, running.ID).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestStoreValidation(t *testing.T) {
	store := &Store{}
	_, err := store.Start(context.Background(), "reembed", "operator", nil, time.Minute)
	require.ErrorContains(t, err, "not configured")

	encoded, err := marshalPayload(json.RawMessage(`{"ok":true}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(encoded))
	assert.Equal(t, "abc…", compactError("abcdef", 3))
}
