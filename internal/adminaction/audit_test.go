package adminaction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestRunRecordsOutcome(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminaction_audit"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	audit := Audit{Pool: pool, Actor: Actor{Email: " Operator@Example.com ", RequestID: "req-1"}}

	ran := false
	require.NoError(t, audit.Run(ctx, "test.ok", "target-1", map[string]any{"n": 1}, func() error {
		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM admin_audit_log WHERE id = 1`).Scan(&status))
		assert.Equal(t, "started", status, "the record exists before the action runs")
		ran = true
		return nil
	}))
	require.True(t, ran)

	// Cut at the limit, the error would end inside "é": its text is cut
	// before it, and stays valid UTF-8 for Postgres.
	failure := errors.New(strings.Repeat("x", maxErrorText-1) + "é and more \xff")
	require.ErrorIs(t, audit.Run(ctx, "test.fail", "target-2", nil, func() error { return failure }), failure)

	type row struct {
		actor, action, target, requestID, status, details, message string
		finished                                                   bool
	}
	var rows []row
	result, err := pool.Query(ctx, `
		SELECT actor_email, action, target, request_id, status, details::text,
		       COALESCE(error, ''), finished_at IS NOT NULL
		  FROM admin_audit_log ORDER BY id`)
	require.NoError(t, err)
	for result.Next() {
		var r row
		require.NoError(t, result.Scan(&r.actor, &r.action, &r.target, &r.requestID, &r.status, &r.details, &r.message, &r.finished))
		rows = append(rows, r)
	}
	require.NoError(t, result.Err())
	require.Len(t, rows, 2)
	assert.Equal(t, row{"operator@example.com", "test.ok", "target-1", "req-1", "succeeded", `{"n": 1}`, "", true}, rows[0])
	assert.Equal(t, "failed", rows[1].status)
	assert.True(t, rows[1].finished)
	assert.Equal(t, strings.Repeat("x", maxErrorText-1), rows[1].message)
	assert.Equal(t, "null", rows[1].details)
}

func TestRunRefusesWithoutRecord(t *testing.T) {
	act := func() error {
		t.Fatal("the action ran without an audit record")
		return nil
	}
	err := Audit{Actor: Actor{Email: "operator@example.com"}}.Run(context.Background(), "test", "t", nil, act)
	require.ErrorIs(t, err, ErrRecord)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminaction_audit"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	err = Audit{Pool: pool, Actor: Actor{Email: "  "}}.Run(ctx, "test", "t", nil, act)
	require.ErrorIs(t, err, ErrRecord)
}
