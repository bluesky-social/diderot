package leader

// Postgres leader election for deployments without a Kubernetes API.
//
// RunPostgres holds a session-level advisory lock on one dedicated
// connection. The lock lives exactly as long as that session, so leadership
// ends when the connection does, whoever ends it.
//
// # Timing guarantees
//
// Let R = CheckInterval + CheckTimeout, the renew deadline.
//
//   - The holder checks every CheckInterval that its own backend still holds
//     the lock. Each check must finish within R of the start of the last check
//     that succeeded. A failed or late check cancels the leader context and
//     closes the connection, which releases the lock.
//   - The server ends the lock session after SessionTimeout without a query
//     (idle_session_timeout). A holder that crashes, hangs, or is partitioned
//     from the database therefore frees the lock within SessionTimeout, and
//     because SessionTimeout > R a holder that is still running has canceled
//     itself before the lock can change hands.
//   - A new holder waits TakeoverDelay (>= R) after acquiring before calling
//     leaderFn. When the old session ends abruptly (pg_terminate_backend, a
//     database restart or failover), the lock is free immediately, but the old
//     holder's next check fails at most R after its last good one, so its
//     leader context is canceled before the new holder's leaderFn starts.
//     TakeoverDelay - R (5s by default) is the budget for the old leader's
//     workers to observe that cancellation, mirroring the 5s between the
//     Kubernetes Lease's renew deadline and its expiry.
//
// Only the local monotonic clock is involved; contenders need no clock sync.
//
// # Limits
//
//   - The ordering is between the old leader's cancellation and the new
//     leader's start. Nothing waits for the old leader's work to finish, so
//     leader-only work must stop promptly once its context is canceled.
//   - There are no fencing tokens. A process frozen for longer than
//     SessionTimeout (SIGSTOP, VM suspend, swap storms) wakes with every
//     goroutine at once and can act before its monitor sees the lost session.
//     The Kubernetes Lease has the same limit.
//   - On shutdown (ctx canceled) the lock is released at once while leaderFn
//     drains, as the Kubernetes path's ReleaseOnCancel does, so a successor
//     can start TakeoverDelay later. Work that deliberately outlives the
//     leader context (the Slack consumer's in-flight Asks run on the process
//     context) must tolerate overlapping the successor.
//   - The connection must reach the primary directly or through a
//     session-pooling proxy. Transaction pooling moves queries between server
//     sessions: the backend-pid check notices and gives up leadership, but the
//     lock stays with the pooled session until the pooler resets it.
//   - Advisory locks are scoped to one database: every contender must use the
//     same database. idle_session_timeout requires Postgres 14 or later.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresOptions configures RunPostgres. Zero durations take the defaults.
type PostgresOptions struct {
	// Name selects the election; the advisory lock key is derived from it.
	Name string
	// Identity labels this contender in logs and in the lock session's
	// application_name. Empty uses POD_NAME, else hostname-pid.
	Identity string
	// RetryPeriod spaces acquisition attempts and re-contention after a lost
	// or failed term (default 2s).
	RetryPeriod time.Duration
	// CheckInterval spaces liveness checks while holding the lock
	// (default 2s).
	CheckInterval time.Duration
	// CheckTimeout bounds one check (default 2s).
	CheckTimeout time.Duration
	// TakeoverDelay is how long a new holder waits before running leaderFn
	// (default CheckInterval + CheckTimeout + 5s). It must be at least
	// CheckInterval + CheckTimeout.
	TakeoverDelay time.Duration
	// SessionTimeout is the server-side idle_session_timeout on the lock
	// session (default 15s). It must exceed CheckInterval + CheckTimeout.
	SessionTimeout time.Duration

	// serverVersion replaces reading the server's server_version_num, for
	// tests.
	serverVersion func(context.Context) (int, error)
}

// minPostgresVersion is the first Postgres with idle_session_timeout, which
// frees the lock of a holder that goes silent, as server_version_num.
const minPostgresVersion = 140000

// postgresServerVersion reads the server's server_version_num.
func postgresServerVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresConnectTimeout)
	defer cancel()
	var version int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return 0, fmt.Errorf("leader: read the Postgres version: %w", err)
	}
	return version, nil
}

// requirePostgresVersion fails when the server is older than
// minPostgresVersion. A version it cannot read is retried every retry until
// it can, or ctx ends, which returns nil.
func requirePostgresVersion(ctx context.Context, logger *slog.Logger, version func(context.Context) (int, error), retry time.Duration) error {
	for {
		num, err := version(ctx)
		if err == nil {
			if num < minPostgresVersion {
				return fmt.Errorf("the Postgres leader election needs Postgres %d or later, and this database is Postgres %s; "+
					"upgrade it, or set DIDEROT_LEADER_ELECTION=false if this is the only process that runs serve against it",
					minPostgresVersion/10000, postgresMajor(num))
			}
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("leader election: couldn't read the Postgres version; retrying", "err", err, "retry", retry)
		if !sleepContext(ctx, retry) {
			return nil
		}
	}
}

// postgresMajor renders a server_version_num as its major version: 13 for
// 130012, 9.6 for 90624.
func postgresMajor(num int) string {
	if num >= 100000 {
		return fmt.Sprint(num / 10000)
	}
	return fmt.Sprintf("%d.%d", num/10000, num/100%100)
}

const (
	defaultRetryPeriod    = 2 * time.Second
	defaultCheckInterval  = 2 * time.Second
	defaultCheckTimeout   = 2 * time.Second
	defaultTakeoverMargin = 5 * time.Second
	defaultSessionTimeout = 15 * time.Second
	// postgresConnectTimeout bounds taking a connection from the pool, which
	// may dial (and sign an IAM token) when no idle connection is free.
	postgresConnectTimeout = 30 * time.Second
	closeTimeout           = time.Second
)

func (o PostgresOptions) withDefaults() (PostgresOptions, error) {
	if o.Name == "" {
		return o, errors.New("leader: postgres election name is required")
	}
	if o.Identity == "" {
		o.Identity = defaultIdentity()
	}
	if o.RetryPeriod == 0 {
		o.RetryPeriod = defaultRetryPeriod
	}
	if o.CheckInterval == 0 {
		o.CheckInterval = defaultCheckInterval
	}
	if o.CheckTimeout == 0 {
		o.CheckTimeout = defaultCheckTimeout
	}
	if o.TakeoverDelay == 0 {
		o.TakeoverDelay = o.renewDeadline() + defaultTakeoverMargin
	}
	if o.SessionTimeout == 0 {
		o.SessionTimeout = defaultSessionTimeout
	}
	switch {
	case o.RetryPeriod < 0 || o.CheckInterval < 0 || o.CheckTimeout < 0 || o.TakeoverDelay < 0 || o.SessionTimeout < 0:
		return o, errors.New("leader: postgres election durations must be positive")
	case o.TakeoverDelay < o.renewDeadline():
		return o, fmt.Errorf("leader: takeover delay %s must be at least check interval + check timeout (%s)", o.TakeoverDelay, o.renewDeadline())
	case o.SessionTimeout <= o.renewDeadline():
		return o, fmt.Errorf("leader: session timeout %s must exceed check interval + check timeout (%s)", o.SessionTimeout, o.renewDeadline())
	case o.SessionTimeout.Milliseconds() < 1:
		// idle_session_timeout has millisecond resolution, and 0 disables it.
		return o, errors.New("leader: session timeout must be at least 1ms")
	case o.RetryPeriod >= o.SessionTimeout:
		return o, fmt.Errorf("leader: retry period %s must be shorter than the session timeout %s", o.RetryPeriod, o.SessionTimeout)
	}
	return o, nil
}

func (o PostgresOptions) renewDeadline() time.Duration {
	return o.CheckInterval + o.CheckTimeout
}

func defaultIdentity() string {
	if id := os.Getenv("POD_NAME"); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "diderot"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// advisoryKey maps an election name onto the bigint advisory-lock keyspace.
// A 64-bit hash of a namespaced name makes a collision with the database's
// other advisory-lock users (Atlas's migration lock) improbable.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("diderot/leader/" + name))
	return int64(h.Sum64())
}

// RunPostgres blocks until ctx is canceled, contending for leadership through
// a Postgres advisory lock and re-contending after every lost term. While
// this process leads, leaderFn runs with a context that is canceled the
// moment leadership is lost or ctx is canceled; RunPostgres does not contend
// again until leaderFn has returned. Like Run, it calls leaderFn once per
// term, so leaderFn must be safe to call again in the same process (metrics
// it registers, for one). If leaderFn returns while leadership is still
// held, the lock is released and contention starts over. The comment
// at the top of this file states the split-brain guarantees and their limits.
//
// pool supplies the dedicated lock connection, which is hijacked out of it
// for each term. It returns an error only when the election cannot start:
// invalid options, or a server older than Postgres 14, which no retry fixes.
// Every other failure, reading the server's version included, is retried.
func RunPostgres(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, opts PostgresOptions, leaderFn func(context.Context)) error {
	opts, err := opts.withDefaults()
	if err != nil {
		return err
	}
	key := advisoryKey(opts.Name)
	logger = logger.With("leader_backend", "postgres", "id", opts.Identity)
	version := opts.serverVersion
	if version == nil {
		version = func(ctx context.Context) (int, error) { return postgresServerVersion(ctx, pool) }
	}
	if err := requirePostgresVersion(ctx, logger, version, opts.RetryPeriod); err != nil || ctx.Err() != nil {
		return err
	}
	for {
		err := runPostgresTerm(ctx, logger, pool, opts, key, leaderFn)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Warn("leader election: term ended; re-contending", "err", err, "retry", opts.RetryPeriod)
		}
		if !sleepContext(ctx, opts.RetryPeriod) {
			return nil
		}
	}
}

// runPostgresTerm opens one lock session, waits for the lock, and leads until
// the session is lost, ctx is canceled, or leaderFn returns.
func runPostgresTerm(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, opts PostgresOptions, key int64, leaderFn func(context.Context)) error {
	conn, err := openLockConn(ctx, pool, opts)
	if err != nil {
		return err
	}
	pid, heldSince, err := acquireLock(ctx, logger, conn, opts, key)
	if err != nil {
		closeLockConn(conn)
		return err
	}

	leaderCtx, cancelLeader := context.WithCancelCause(ctx)
	defer cancelLeader(nil)
	release := make(chan struct{})
	monitorDone := make(chan error, 1)
	go func() {
		monitorDone <- monitorLock(ctx, conn, opts, key, pid, heldSince, cancelLeader, release)
	}()
	// stop releases the lock (if the monitor still holds it) and reports why
	// the monitor stopped: nil for shutdown or release, else the loss.
	stop := func() error {
		close(release)
		return <-monitorDone
	}

	logger.Info("acquired leader lock; waiting out takeover delay", "delay", opts.TakeoverDelay)
	takeover := time.NewTimer(opts.TakeoverDelay)
	select {
	case <-leaderCtx.Done():
		takeover.Stop()
		return stop()
	case <-takeover.C:
	}

	logger.Info("became leader")
	leaderFn(leaderCtx)
	if leaderCtx.Err() == nil {
		cancelLeader(errors.New("leader work returned"))
		_ = stop()
		logger.Warn("leader work returned while holding leadership; released the lock")
		return nil
	}
	err = stop()
	logger.Info("lost leadership", "cause", context.Cause(leaderCtx))
	return err
}

// openLockConn takes a connection out of the pool for the lock session and
// arms the server-side lease.
func openLockConn(ctx context.Context, pool *pgxpool.Pool, opts PostgresOptions) (*pgx.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, postgresConnectTimeout)
	defer cancel()
	pooled, err := pool.Acquire(connectCtx)
	if err != nil {
		return nil, fmt.Errorf("leader: acquire lock connection: %w", err)
	}
	conn := pooled.Hijack()
	_, err = conn.Exec(connectCtx,
		"SELECT set_config('idle_session_timeout', $1, false), set_config('application_name', $2, false)",
		fmt.Sprintf("%dms", opts.SessionTimeout.Milliseconds()), "diderot-leader "+opts.Identity)
	if err != nil {
		closeLockConn(conn)
		return nil, fmt.Errorf("leader: configure lock session (idle_session_timeout needs Postgres 14+): %w", err)
	}
	return conn, nil
}

// acquireLock polls pg_try_advisory_lock until it succeeds. It returns the
// lock session's backend pid and the start of the winning attempt, the
// earliest moment the lock is known held.
func acquireLock(ctx context.Context, logger *slog.Logger, conn *pgx.Conn, opts PostgresOptions, key int64) (int32, time.Time, error) {
	var pid int32
	pidCtx, cancel := context.WithTimeout(ctx, opts.CheckTimeout)
	err := conn.QueryRow(pidCtx, "SELECT pg_backend_pid()").Scan(&pid)
	cancel()
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("leader: read lock session pid: %w", err)
	}
	waiting := false
	for {
		start := time.Now()
		var acquired bool
		tryCtx, cancel := context.WithTimeout(ctx, opts.CheckTimeout)
		err := conn.QueryRow(tryCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired)
		cancel()
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("leader: try advisory lock: %w", err)
		}
		if acquired {
			return pid, start, nil
		}
		if !waiting {
			logger.Info("leader lock held by another contender; waiting")
			waiting = true
		}
		if !sleepContext(ctx, opts.RetryPeriod) {
			return 0, time.Time{}, ctx.Err()
		}
	}
}

// heldQuery confirms that the lock session is still the same server backend
// and that this backend still holds the lock. A bigint advisory key appears
// in pg_locks split into its high (classid) and low (objid) halves.
const heldQuery = `SELECT pg_backend_pid() = $1 AND EXISTS (
	SELECT 1 FROM pg_locks
	WHERE locktype = 'advisory' AND pid = $1 AND granted
	  AND classid::bigint = $2 AND objid::bigint = $3 AND objsubid = 1)`

// monitorLock owns conn once the lock is held. It cancels the leader context
// on the first failed or late check and closes conn (releasing the lock) on
// return: after a loss, when ctx is canceled, or when release is closed.
func monitorLock(ctx context.Context, conn *pgx.Conn, opts PostgresOptions, key int64, pid int32, heldSince time.Time,
	cancelLeader context.CancelCauseFunc, release <-chan struct{}) error {
	defer closeLockConn(conn)
	high := int64(uint32(uint64(key) >> 32))
	low := int64(uint32(uint64(key)))
	ticker := time.NewTicker(opts.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-release:
			return nil
		case <-ticker.C:
		}
		start := time.Now()
		checkCtx, cancel := context.WithDeadline(ctx, heldSince.Add(opts.renewDeadline()))
		var held bool
		err := conn.QueryRow(checkCtx, heldQuery, pid, high, low).Scan(&held)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && !held {
			err = errors.New("lock no longer held by this session")
		}
		if err != nil {
			err = fmt.Errorf("leader: lock check: %w", err)
			cancelLeader(err)
			return err
		}
		heldSince = start
	}
}

func closeLockConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
