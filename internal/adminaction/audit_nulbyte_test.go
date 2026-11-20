package adminaction

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// A failed action's error text can carry bytes from whatever the action
// called. Postgres rejects a NUL byte in it, and the row would then stay
// "started" with no outcome.
func TestRunRecordsAFailureWhoseErrorHasANulByte(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminaction_audit"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	audit := Audit{Pool: pool, Actor: Actor{Email: "operator@example.com"}}

	failure := errors.New("upstream said: bad\x00request")
	require.ErrorIs(t, audit.Run(ctx, "test.fail", "target", nil, func() error { return failure }), failure)

	var status, message string
	var finished bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT status, COALESCE(error, ''), finished_at IS NOT NULL FROM admin_audit_log WHERE id = 1
	`).Scan(&status, &message, &finished))
	assert.Equal(t, "failed", status)
	assert.Equal(t, "upstream said: badrequest", message)
	assert.True(t, finished)
}
