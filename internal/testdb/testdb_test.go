package testdb

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCheckoutKeyIsStableAndBounded(t *testing.T) {
	t.Setenv("DIDEROT_TEST_DB_PREFIX", "")
	first, second := checkoutKey(), checkoutKey()
	if first == "" || first != second {
		t.Fatalf("checkout key not stable: %q then %q", first, second)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(first) {
		t.Fatalf("checkout key %q is not 8 hex characters", first)
	}
	if got := dbNamePrefix + first + "_" + sanitizeKey("a-very-long-package-key-that-needs-hashing-to-fit"); len(got) > 63 {
		t.Fatalf("clone name %q exceeds Postgres's 63-byte identifier limit", got)
	}
}

func TestCheckoutKeyOverride(t *testing.T) {
	t.Setenv("DIDEROT_TEST_DB_PREFIX", "")
	derived := checkoutKey()
	t.Setenv("DIDEROT_TEST_DB_PREFIX", "my-worktree")
	overridden := checkoutKey()
	if overridden == derived {
		t.Fatalf("override did not change the checkout key (%q)", overridden)
	}
	t.Setenv("DIDEROT_TEST_DB_PREFIX", "other-worktree")
	if other := checkoutKey(); other == overridden {
		t.Fatalf("different overrides produced the same key %q", other)
	}
}

// TestSweepDropsOnlyStaleUnheldClones: a sweep drops a clone older than its
// threshold that nobody is connected to and no other session holds, and
// leaves younger, connected, and held clones.
func TestSweepDropsOnlyStaleUnheldClones(t *testing.T) {
	base, conn := maintenanceConn(t)
	prefix := sweepPrefix()
	stale, connected, held := prefix+"stale", prefix+"connected", prefix+"held"
	createDatabases(t, conn, stale, connected, held)
	var logs logLines

	sweep(t.Context(), base, prefix, staleAfter, logs.logf)
	requireExist(t, conn, map[string]bool{stale: true, connected: true, held: true})

	user := connect(t, withDatabase(base, connected))
	// Another process holding a clone it provisioned is another session
	// holding its in-use lock.
	other := connect(t, withDatabase(base, maintenanceDB))
	if _, err := other.Exec(t.Context(), "SELECT pg_advisory_lock_shared($1)", inUseKey(held)); err != nil {
		t.Fatalf("hold %q: %v", held, err)
	}

	sweep(t.Context(), base, prefix, 0, logs.logf)
	requireExist(t, conn, map[string]bool{stale: false, connected: true, held: true})

	_ = user.Close(t.Context())
	waitForNoConnections(t, conn, connected)
	if _, err := other.Exec(t.Context(), "SELECT pg_advisory_unlock_shared($1)", inUseKey(held)); err != nil {
		t.Fatalf("release %q: %v", held, err)
	}
	sweep(t.Context(), base, prefix, 0, logs.logf)
	requireExist(t, conn, map[string]bool{connected: false, held: false})
	if len(logs.lines) != 0 {
		t.Fatalf("sweeps logged %q", logs.lines)
	}
}

// TestSweepLeavesBases: bases are gcStaleBases' to drop, never a sweep's.
func TestSweepLeavesBases(t *testing.T) {
	base, conn := maintenanceConn(t)
	name := fmt.Sprintf("%ssweep%08x", baseDBPrefix, rand.Uint32())
	createDatabases(t, conn, name)
	var logs logLines
	sweep(t.Context(), base, name, 0, logs.logf)
	requireExist(t, conn, map[string]bool{name: true})
	if len(logs.lines) != 0 {
		t.Fatalf("sweep logged %q", logs.lines)
	}
}

// TestConcurrentSweeps: two sweeps over the same stale clones, as two
// processes' sweeps are (each its own session), drop them all without an
// error.
func TestConcurrentSweeps(t *testing.T) {
	base, conn := maintenanceConn(t)
	prefix := sweepPrefix()
	want := map[string]bool{}
	for i := range 8 {
		name := fmt.Sprintf("%s%d", prefix, i)
		createDatabases(t, conn, name)
		want[name] = false
	}
	var logs logLines
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { sweep(t.Context(), base, prefix, 0, logs.logf) })
	}
	wg.Wait()
	requireExist(t, conn, want)
	if len(logs.lines) != 0 {
		t.Fatalf("concurrent sweeps logged %q", logs.lines)
	}
}

// TestSweepWithoutPrivilegeLogsOnce: a role that can't date clones, or can't
// drop them, drops nothing and logs one line however many clones are stale.
func TestSweepWithoutPrivilegeLogsOnce(t *testing.T) {
	base, conn := maintenanceConn(t)
	role := fmt.Sprintf("diderot_test_sweeper_%08x", rand.Uint32())
	if _, err := conn.Exec(t.Context(), "CREATE ROLE "+quoteIdent(role)+" LOGIN PASSWORD 'sweeper'"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			t.Skipf("the test role can't create roles: %v", err)
		}
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP ROLE "+quoteIdent(role)) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DIDEROT_TEST_DB_URL: %v", err)
	}
	u.User = url.UserPassword(role, "sweeper")
	asRole := u.String()
	prefix := sweepPrefix()
	createDatabases(t, conn, prefix+"a", prefix+"b")
	both := map[string]bool{prefix + "a": true, prefix + "b": true}

	// Without pg_stat_file it can't tell how old a clone is.
	var logs logLines
	sweep(t.Context(), asRole, prefix, 0, logs.logf)
	requireExist(t, conn, both)
	if len(logs.lines) != 1 || !strings.Contains(logs.lines[0], "pg_stat_file") {
		t.Fatalf("sweep without pg_stat_file logged %q, want one line about it", logs.lines)
	}

	// With it, but owning neither clone, its first drop is refused.
	if _, err := conn.Exec(t.Context(), "GRANT EXECUTE ON FUNCTION pg_stat_file(text, boolean) TO "+quoteIdent(role)); err != nil {
		t.Fatalf("grant pg_stat_file: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "REVOKE EXECUTE ON FUNCTION pg_stat_file(text, boolean) FROM "+quoteIdent(role))
	})
	var refused logLines
	sweep(t.Context(), asRole, prefix, 0, refused.logf)
	requireExist(t, conn, both)
	if len(refused.lines) != 1 || !strings.Contains(refused.lines[0], "must be owner") {
		t.Fatalf("sweep that can't drop logged %q, want one line about it", refused.lines)
	}
}

// TestURLHoldsItsClone: the clone URL provisions survives a sweep from
// another session that treats every clone as stale, because this process
// holds it.
func TestURLHoldsItsClone(t *testing.T) {
	base, conn := maintenanceConn(t)
	dbURL, err := url.Parse(URL(t, "testdb_held"))
	if err != nil {
		t.Fatalf("parse clone URL: %v", err)
	}
	name := strings.TrimPrefix(dbURL.Path, "/")
	var logs logLines
	sweep(t.Context(), base, name, 0, logs.logf)
	requireExist(t, conn, map[string]bool{name: true})
	if len(logs.lines) != 0 {
		t.Fatalf("sweep logged %q", logs.lines)
	}
}

// maintenanceConn returns DIDEROT_TEST_DB_URL and a connection to its
// cluster's maintenance database, skipping without it.
func maintenanceConn(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	base := os.Getenv("DIDEROT_TEST_DB_URL")
	if base == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set; skipping DB integration test")
	}
	return base, connect(t, withDatabase(base, maintenanceDB))
}

func connect(t *testing.T, connURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), connURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// sweepPrefix is a clone-name prefix no other test or checkout uses, so a
// sweep that treats every clone as stale touches only this test's.
func sweepPrefix() string {
	return fmt.Sprintf("%ssweep%08x_", dbNamePrefix, rand.Uint32())
}

// createDatabases creates empty databases and drops whichever a test leaves.
func createDatabases(t *testing.T, conn *pgx.Conn, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := createDatabase(t.Context(), conn, name, "template0"); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		t.Cleanup(func() { _ = dropDatabase(context.Background(), conn, name) })
	}
}

// requireExist fails unless each named database's existence is as given.
func requireExist(t *testing.T, conn *pgx.Conn, want map[string]bool) {
	t.Helper()
	for name, exists := range want {
		got, err := databaseExists(t.Context(), conn, name)
		if err != nil {
			t.Fatalf("check %q: %v", name, err)
		}
		if got != exists {
			t.Errorf("%q exists = %v, want %v", name, got, exists)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// waitForNoConnections waits for a closed connection's backend to leave
// pg_stat_activity, which happens after the client's Close returns.
func waitForNoConnections(t *testing.T, conn *pgx.Conn, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE datname = $1", name).Scan(&n); err != nil {
			t.Fatalf("count connections to %q: %v", name, err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections to %q remain", n, name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logLines collects what a sweep logs.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
