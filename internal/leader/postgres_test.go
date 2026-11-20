package leader

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// fastOptions keeps the production ratios (TakeoverDelay > renew deadline,
// SessionTimeout > renew deadline) at test speed: R = 250ms, W = 500ms.
func fastOptions(name, identity string) PostgresOptions {
	return PostgresOptions{
		Name:           name,
		Identity:       identity,
		RetryPeriod:    25 * time.Millisecond,
		CheckInterval:  50 * time.Millisecond,
		CheckTimeout:   200 * time.Millisecond,
		TakeoverDelay:  500 * time.Millisecond,
		SessionTimeout: 2 * time.Second,
	}
}

// testURL provisions the package database. testdb.URL drops and recreates
// it on every call, so each top-level test calls it exactly once and every
// pool in that test shares the result.
func testURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "leader")
}

// newPool opens one pool per simulated process.
func newPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testLogger shows election logs only when a test fails.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}

// observer records every leaderFn start and stop across contenders and flags
// any moment at which two leaderFns ran at once.
type observer struct {
	mu      sync.Mutex
	current string
	overlap []string
	starts  map[string][]time.Time
	stops   map[string][]time.Time
}

func newObserver() *observer {
	return &observer{starts: map[string][]time.Time{}, stops: map[string][]time.Time{}}
}

func (o *observer) leaderFn(id string) func(context.Context) {
	return func(ctx context.Context) {
		o.mu.Lock()
		if o.current != "" {
			o.overlap = append(o.overlap, o.current+" still leading when "+id+" started")
		}
		o.current = id
		o.starts[id] = append(o.starts[id], time.Now())
		o.mu.Unlock()

		<-ctx.Done()

		o.mu.Lock()
		if o.current == id {
			o.current = ""
		}
		o.stops[id] = append(o.stops[id], time.Now())
		o.mu.Unlock()
	}
}

func (o *observer) leader() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.current
}

func (o *observer) startCount(id string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.starts[id])
}

func (o *observer) lastStart(id string) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.starts[id][len(o.starts[id])-1]
}

func (o *observer) lastStop(id string) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stops[id][len(o.stops[id])-1]
}

func (o *observer) requireNoOverlap(t *testing.T) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.overlap) > 0 {
		t.Fatalf("leaders overlapped: %s", strings.Join(o.overlap, "; "))
	}
}

type contender struct {
	cancel   context.CancelFunc
	finished chan struct{}
	err      error
}

func startContender(t *testing.T, url string, opts PostgresOptions, leaderFn func(context.Context)) *contender {
	t.Helper()
	pool := newPool(t, url)
	logger := testLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	c := &contender{cancel: cancel, finished: make(chan struct{})}
	go func() {
		c.err = RunPostgres(ctx, logger, pool, opts, leaderFn)
		close(c.finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.finished:
		case <-time.After(10 * time.Second):
			t.Errorf("contender %s did not stop", opts.Identity)
		}
	})
	return c
}

func (c *contender) stop(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case <-c.finished:
		if c.err != nil {
			t.Fatalf("RunPostgres returned %v", c.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPostgres did not return after cancellation")
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// lockHolder reports the backend currently granted the election's lock and
// its application_name.
func lockHolder(t *testing.T, admin *pgxpool.Pool, name string) (int32, string, bool) {
	t.Helper()
	key := advisoryKey(name)
	var pid int32
	var app string
	err := admin.QueryRow(context.Background(), `
		SELECT l.pid, a.application_name
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid::bigint = $1 AND l.objid::bigint = $2`,
		int64(uint32(uint64(key)>>32)), int64(uint32(uint64(key)))).Scan(&pid, &app)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false
	}
	if err != nil {
		t.Fatalf("query lock holder: %v", err)
	}
	return pid, app, true
}

func terminate(t *testing.T, admin *pgxpool.Pool, pid int32) {
	t.Helper()
	var ok bool
	if err := admin.QueryRow(context.Background(), "SELECT pg_terminate_backend($1)", pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
	}
}

func other(id string) string {
	if id == "a" {
		return "b"
	}
	return "a"
}

func TestRunPostgresOneLeaderAndHandoffOnShutdown(t *testing.T) {
	url := testURL(t)
	admin := newPool(t, url)
	obs := newObserver()
	contenders := map[string]*contender{}
	for _, id := range []string{"a", "b"} {
		opts := fastOptions(t.Name(), id)
		contenders[id] = startContender(t, url, opts, obs.leaderFn(id))
	}

	waitFor(t, 10*time.Second, "a leader", func() bool { return obs.leader() != "" })
	first := obs.leader()
	// Many check intervals pass with exactly one leader and no churn.
	time.Sleep(time.Second)
	if obs.leader() != first || obs.startCount(first) != 1 || obs.startCount(other(first)) != 0 {
		t.Fatalf("leadership churned: leader %q, starts a=%d b=%d", obs.leader(), obs.startCount("a"), obs.startCount("b"))
	}
	if _, app, ok := lockHolder(t, admin, t.Name()); !ok || app != "diderot-leader "+first {
		t.Fatalf("lock holder application_name = %q (held %v), want the leader's identity", app, ok)
	}

	stoppedAt := time.Now()
	contenders[first].stop(t)
	second := other(first)
	waitFor(t, 10*time.Second, "the follower to take over", func() bool { return obs.leader() == second })
	obs.requireNoOverlap(t)
	if gap := obs.lastStart(second).Sub(stoppedAt); gap < fastOptions("", "").TakeoverDelay {
		t.Fatalf("successor started %s after shutdown, inside the %s takeover delay", gap, fastOptions("", "").TakeoverDelay)
	}
}

// Killing the leader's lock session from the server side is the abrupt-loss
// case: the lock is free at once, so the old leader must cancel before the
// successor's takeover delay ends. Both contenders keep running, so this also
// proves a deposed leader re-contends.
func TestRunPostgresFailoverWhenLeaderSessionIsTerminated(t *testing.T) {
	url := testURL(t)
	admin := newPool(t, url)
	obs := newObserver()
	opts := fastOptions(t.Name(), "")
	for _, id := range []string{"a", "b"} {
		o := opts
		o.Identity = id
		startContender(t, url, o, obs.leaderFn(id))
	}
	waitFor(t, 10*time.Second, "a leader", func() bool { return obs.leader() != "" })

	for round := 0; round < 2; round++ {
		old := obs.leader()
		pid, _, ok := lockHolder(t, admin, t.Name())
		if !ok {
			t.Fatalf("round %d: %s leads but no session holds the lock", round, old)
		}
		killedAt := time.Now()
		terminate(t, admin, pid)

		successor := other(old)
		waitFor(t, 10*time.Second, successor+" to take over", func() bool { return obs.leader() == successor })
		obs.requireNoOverlap(t)
		stopped := obs.lastStop(old)
		started := obs.lastStart(successor)
		if detect := stopped.Sub(killedAt); detect > opts.renewDeadline()+time.Second {
			t.Errorf("round %d: old leader took %s to notice its lost session", round, detect)
		}
		if !started.After(stopped) {
			t.Fatalf("round %d: successor started at %s, before the old leader stopped at %s", round, started, stopped)
		}
		if gap := started.Sub(killedAt); gap < opts.TakeoverDelay {
			t.Fatalf("round %d: successor started %s after the kill, inside the %s takeover delay", round, gap, opts.TakeoverDelay)
		}
	}
}

func TestRunPostgresContextCancellation(t *testing.T) {
	url := testURL(t)
	t.Run("leader drains and releases", func(t *testing.T) {
		admin := newPool(t, url)
		obs := newObserver()
		c := startContender(t, url, fastOptions(t.Name(), "solo"), obs.leaderFn("solo"))
		waitFor(t, 10*time.Second, "leadership", func() bool { return obs.leader() == "solo" })

		c.stop(t)
		if obs.leader() != "" {
			t.Fatal("RunPostgres returned before leaderFn did")
		}
		waitFor(t, 5*time.Second, "the lock to be released", func() bool {
			_, _, held := lockHolder(t, admin, t.Name())
			return !held
		})
	})

	t.Run("waiting contender never leads", func(t *testing.T) {
		admin := newPool(t, url)
		holder, err := admin.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		if _, err := holder.Exec(context.Background(), "SELECT pg_advisory_lock($1)", advisoryKey(t.Name())); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryKey(t.Name()))
		}()

		obs := newObserver()
		c := startContender(t, url, fastOptions(t.Name(), "waiter"), obs.leaderFn("waiter"))
		time.Sleep(300 * time.Millisecond)
		c.stop(t)
		if obs.startCount("waiter") != 0 {
			t.Fatal("leaderFn ran without the lock")
		}
	})

	t.Run("canceled during takeover delay", func(t *testing.T) {
		admin := newPool(t, url)
		opts := fastOptions(t.Name(), "slow")
		opts.TakeoverDelay = time.Minute
		obs := newObserver()
		c := startContender(t, url, opts, obs.leaderFn("slow"))
		waitFor(t, 10*time.Second, "the lock to be taken", func() bool {
			_, _, held := lockHolder(t, admin, t.Name())
			return held
		})
		c.stop(t)
		if obs.startCount("slow") != 0 {
			t.Fatal("leaderFn ran before the takeover delay elapsed")
		}
		waitFor(t, 5*time.Second, "the lock to be released", func() bool {
			_, _, held := lockHolder(t, admin, t.Name())
			return !held
		})
	})
}

// A leaderFn that returns on its own releases the lock and contends again
// rather than holding leadership with nothing running.
func TestRunPostgresLeaderFnReturningReleasesAndRecontends(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	second := make(chan struct{})
	leaderFn := func(ctx context.Context) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return
		}
		if n == 2 {
			close(second)
		}
		<-ctx.Done()
	}
	startContender(t, testURL(t), fastOptions(t.Name(), "restless"), leaderFn)
	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("leaderFn was not called again after returning early")
	}
}

// The server-side lease: a holder that stops talking to the database loses
// the lock after SessionTimeout without any help from the client.
func TestPostgresLockSessionExpiresWhenHolderGoesSilent(t *testing.T) {
	url := testURL(t)
	admin := newPool(t, url)
	opts, err := fastOptions(t.Name(), "silent").withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	opts.SessionTimeout = time.Second
	ctx := context.Background()
	conn, err := openLockConn(ctx, newPool(t, url), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockConn(conn)
	if _, _, err := acquireLock(ctx, testLogger(t), conn, opts, advisoryKey(t.Name())); err != nil {
		t.Fatal(err)
	}
	acquiredAt := time.Now()
	if _, _, held := lockHolder(t, admin, t.Name()); !held {
		t.Fatal("lock not held after acquireLock")
	}
	waitFor(t, 10*time.Second, "the server to end the silent session", func() bool {
		_, _, held := lockHolder(t, admin, t.Name())
		return !held
	})
	if elapsed := time.Since(acquiredAt); elapsed < opts.SessionTimeout-100*time.Millisecond {
		t.Fatalf("lock released after %s, before the %s session timeout", elapsed, opts.SessionTimeout)
	}
	if err := conn.Ping(ctx); err == nil {
		t.Fatal("silent session still answers after the server ended it")
	}
}

func TestPostgresOptionsValidation(t *testing.T) {
	opts, err := PostgresOptions{Name: "n", Identity: "i"}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if opts.renewDeadline() != 4*time.Second || opts.TakeoverDelay != 9*time.Second || opts.SessionTimeout != 15*time.Second {
		t.Fatalf("defaults: renew %s, takeover %s, session %s", opts.renewDeadline(), opts.TakeoverDelay, opts.SessionTimeout)
	}
	for _, tc := range []struct {
		name string
		opts PostgresOptions
		want string
	}{
		{"missing name", PostgresOptions{}, "name is required"},
		{"negative", PostgresOptions{Name: "n", CheckInterval: -time.Second}, "positive"},
		{"short takeover", PostgresOptions{Name: "n", TakeoverDelay: 3 * time.Second}, "takeover delay"},
		{"short session", PostgresOptions{Name: "n", SessionTimeout: 4 * time.Second}, "session timeout"},
		{"sub-millisecond session", PostgresOptions{Name: "n", CheckInterval: 100 * time.Microsecond, CheckTimeout: 100 * time.Microsecond, SessionTimeout: 500 * time.Microsecond}, "at least 1ms"},
		{"slow retry", PostgresOptions{Name: "n", RetryPeriod: 20 * time.Second}, "retry period"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.opts.withDefaults(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("withDefaults() error = %v, want %q", err, tc.want)
			}
		})
	}
}
