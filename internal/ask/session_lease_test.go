package ask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replicaManager is a Manager as another replica has it: its own in-memory
// locks over the same database, with lease times short enough for a test.
func replicaManager(pool *pgxpool.Pool, timing sessionLeaseTiming) *Manager {
	m := NewManager(pool, nil)
	m.lease = timing
	return m
}

var testLeaseTiming = sessionLeaseTiming{ttl: 2 * time.Second, renew: 100 * time.Millisecond, retry: 20 * time.Millisecond}

func leaseHolders(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM ask_session_leases WHERE session_id = $1`, sessionID).Scan(&n))
	return n
}

// A turn of a session on one replica waits for the turn another replica is
// running, and starts as soon as that one ends.
func TestSessionLockExcludesAcrossReplicas(t *testing.T) {
	pool := testPool(t)
	first, second := replicaManager(pool, testLeaseTiming), replicaManager(pool, testLeaseTiming)
	s, err := first.Create(t.Context())
	require.NoError(t, err)

	running, finish := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithSessionLock(t.Context(), s.ID, func(context.Context) error {
			close(running)
			<-finish
			return nil
		})
	}()
	<-running
	require.Equal(t, 1, leaseHolders(t, pool, s.ID))

	entered := make(chan time.Time, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.WithSessionLock(t.Context(), s.ID, func(context.Context) error {
			entered <- time.Now()
			return nil
		})
	}()
	select {
	case <-entered:
		t.Fatal("the second replica's turn ran while the first's did")
	case <-time.After(300 * time.Millisecond):
	}
	finished := time.Now()
	close(finish)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	assert.Less(t, (<-entered).Sub(finished), testLeaseTiming.ttl, "the waiter starts once the lease is released, not when it expires")
	assert.Zero(t, leaseHolders(t, pool, s.ID), "the lease is released when the turn ends")
}

// A lease its holder stopped renewing (a replica that died mid-turn) holds
// the session until it expires, and then the next turn takes it over.
func TestSessionLockTakesOverAnExpiredLease(t *testing.T) {
	pool := testPool(t)
	m := replicaManager(pool, testLeaseTiming)
	s, err := m.Create(t.Context())
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(),
		`INSERT INTO ask_session_leases (session_id, holder, expires_at) VALUES ($1, $2, now() + interval '400 milliseconds')`,
		s.ID, uuid.New())
	require.NoError(t, err)

	started := time.Now()
	require.NoError(t, m.WithSessionLock(t.Context(), s.ID, func(context.Context) error { return nil }))
	assert.GreaterOrEqual(t, time.Since(started), 300*time.Millisecond, "the dead holder's lease is honored until it expires")
	assert.Zero(t, leaseHolders(t, pool, s.ID))
}

// A turn whose lease another process took over (it froze past its lease)
// is cancelled, and says why.
func TestSessionLockCancelsATurnThatLostItsLease(t *testing.T) {
	pool := testPool(t)
	m := replicaManager(pool, testLeaseTiming)
	s, err := m.Create(t.Context())
	require.NoError(t, err)

	err = m.WithSessionLock(t.Context(), s.ID, func(ctx context.Context) error {
		_, err := pool.Exec(t.Context(), `UPDATE ask_session_leases SET holder = $2 WHERE session_id = $1`, s.ID, uuid.New())
		require.NoError(t, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("the turn was not cancelled")
		}
	})
	require.ErrorIs(t, err, errSessionLeaseLost)
	assert.Equal(t, 1, leaseHolders(t, pool, s.ID), "the new holder's lease is not released by the old one")
}

// A turn whose lease was taken over commits no history, even before its
// renewal notices: its write checks the lease in its own transaction.
func TestSessionLockFencesTheWritesOfATakenOverTurn(t *testing.T) {
	pool := testPool(t)
	m := replicaManager(pool, sessionLeaseTiming{ttl: time.Hour, renew: time.Hour, retry: 20 * time.Millisecond})
	s, err := m.Create(t.Context())
	require.NoError(t, err)
	batch := []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("question")},
		{Turn: 1, Message: NewAgenticAssistantText("answer")},
	}

	err = m.WithSessionLock(t.Context(), s.ID, func(ctx context.Context) error {
		_, err := pool.Exec(t.Context(), `UPDATE ask_session_leases SET holder = $2 WHERE session_id = $1`, s.ID, uuid.New())
		require.NoError(t, err)
		return m.AppendAgenticMessages(ctx, s.ID, batch)
	})
	require.ErrorIs(t, err, errSessionLeaseLost)
	var messages int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM ask_messages WHERE session_id = $1`, s.ID).Scan(&messages))
	assert.Zero(t, messages)

	require.NoError(t, m.AppendAgenticMessages(t.Context(), s.ID, batch), "a write outside a turn's lease checks none")
}

// Renewal keeps a turn that outlasts its lease's life holding the session.
func TestSessionLockRenewsALongTurnsLease(t *testing.T) {
	pool := testPool(t)
	// A renewal has its interval to finish in, so the interval is long
	// enough for a loaded database.
	timing := sessionLeaseTiming{ttl: 1500 * time.Millisecond, renew: 250 * time.Millisecond, retry: 20 * time.Millisecond}
	first, second := replicaManager(pool, timing), replicaManager(pool, timing)
	s, err := first.Create(t.Context())
	require.NoError(t, err)

	running, finish := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithSessionLock(t.Context(), s.ID, func(ctx context.Context) error {
			close(running)
			<-finish
			return ctx.Err()
		})
	}()
	<-running
	waitCtx, cancel := context.WithTimeout(t.Context(), 2*timing.ttl)
	defer cancel()
	err = second.WithSessionLock(waitCtx, s.ID, func(context.Context) error { return errors.New("entered") })
	require.ErrorIs(t, err, context.DeadlineExceeded, "the renewed lease kept the session held")
	close(finish)
	require.NoError(t, <-firstDone)
}

// A turn's lease is released even when the turn's own context has ended.
func TestSessionLockReleasesTheLeaseOfACancelledTurn(t *testing.T) {
	pool := testPool(t)
	m := replicaManager(pool, testLeaseTiming)
	s, err := m.Create(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	err = m.WithSessionLock(ctx, s.ID, func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, leaseHolders(t, pool, s.ID))
}

func TestSessionLockOnAMissingSession(t *testing.T) {
	pool := testPool(t)
	m := replicaManager(pool, testLeaseTiming)
	err := m.WithSessionLock(t.Context(), uuid.New(), func(context.Context) error { return errors.New("entered") })
	require.ErrorIs(t, err, ErrSessionNotFound)
}
