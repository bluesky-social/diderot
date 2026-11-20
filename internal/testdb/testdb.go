// Package testdb gives every integration-test package its own
// isolated Postgres database so the suite can run packages in
// parallel (no `-p 1`).
//
// # The problem this solves
//
// Diderot's integration tests TRUNCATE shared tables (events,
// slack_*, ask_*, …) at the start of each test to get a clean slate.
// When `go test` ran packages concurrently they wiped each other's
// fixtures mid-run, so the suite was pinned to `-p 1` (one package at
// a time) — ~10 minutes serial. The fix is to stop sharing one
// database: each test package connects to its OWN database, so a
// TRUNCATE in package A can't touch package B's rows, and packages
// run concurrently.
//
// # The template-database trick
//
// Postgres `CREATE DATABASE <x> TEMPLATE <base>` is a near-instant
// file-level copy (single-digit milliseconds for a small schema). So:
//
//  1. ONE base database per migration-set fingerprint
//     (diderot_test_base_<hash of atlas.sum>) is created once and has
//     all migrations applied to it. This is the "golden" schema. The
//     fingerprint in the name means a checkout with different
//     migrations (new migration landed, branch/worktree divergence)
//     bootstraps its own base instead of reusing a stale one; stale
//     bases are GC'd on each fresh bootstrap.
//  2. Each test package gets a database cloned from the base:
//     diderot_test_<pkg> = CREATE DATABASE … TEMPLATE <base>.
//     The clone is dropped-and-recreated on first use within a process
//     so a crashed prior run can't leave stale rows behind — the
//     package starts from the golden schema every time.
//  3. Tests in a package still TRUNCATE between cases (intra-package
//     tests share the package DB and run serially, which is fine and
//     unchanged). Cross-package isolation is the only thing we added.
//
// # Concurrency model
//
//   - The base DB is created + migrated under a Postgres advisory lock
//     (BaseAdvisoryLock) taken on the maintenance ("postgres")
//     database. Many test packages start at once when `go test` fans
//     out; the lock makes exactly one of them do the create+migrate
//     while the others wait, then all proceed. Within a single process
//     a sync.Once also short-circuits repeat work.
//   - Provisioning a per-package clone takes the same advisory lock
//     only for the CREATE/DROP DDL (TEMPLATE copies require no other
//     session be connected to the template, and concurrent CREATE …
//     TEMPLATE against the same template serialize in Postgres
//     anyway), so we serialize them to avoid "source database is being
//     accessed by other users" races between packages cloning at once.
//
// # Cleanup
//
// A clone outlives its test, so a failed test's database can be
// inspected. The first URL call in each process starts a background
// sweep that drops every clone created more than staleAfter ago that has
// no connection and that no live process holds; what one process's sweep
// doesn't reach before it exits, the next one's does. A process holds
// each clone it provisions with a shared advisory lock on a connection it
// keeps until it exits, and the sweep drops only a clone it can lock
// exclusively, so a running test's database is never dropped; the age
// also protects test processes built before that lock existed. The
// creation time is the mtime of the clone's PG_VERSION file, read with
// pg_stat_file, which needs a superuser or an EXECUTE grant: without it
// the sweep logs one line and drops nothing.
//
// # The skip contract (load-bearing)
//
// When DIDEROT_TEST_DB_URL is unset (the no-DB CI lanes and plain
// `go build`/`go test` on a laptop without `just up`), URL calls
// t.Skip and returns "". Callers wire it the same way they used to
// wire os.Getenv — the skip happens inside URL, so the package's
// pool-getter never tries to dial. Nothing here panics or errors when
// the env is unset; that path must stay intact.
//
// # Atlas binary
//
// Migrating the base DB reuses internal/migrations.Apply, which needs
// the atlas binary. It honors $DIDEROT_ATLAS_BINARY and otherwise
// looks at /usr/local/bin/atlas (the container path). Local dev sets
// DIDEROT_ATLAS_BINARY=~/bin/atlas (see justfile); CI resolves the
// atlas installed by setup-atlas and exports DIDEROT_ATLAS_BINARY.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bluesky-social/diderot/internal/migrations"
)

const (
	// baseDBPrefix prefixes the golden database's name. The full name
	// is baseDBPrefix + migrations.SumHash() — fingerprinted on the
	// embedded atlas.sum so a checkout with different migrations
	// (new migration landed, worktree on a branch, git pull) gets its
	// own base instead of silently cloning a stale schema. Before the
	// fingerprint, a base migrated by an older checkout was reused
	// forever: new-table tests failed with "relation does not exist"
	// and nothing ever re-migrated it (bootstrapBase skips migration
	// when the base exists).
	baseDBPrefix = "diderot_test_base_"

	// dbNamePrefix prefixes every per-package clone, followed by the
	// checkout key and the package key, e.g. diderot_test_1a2b3c4d_events.
	dbNamePrefix = "diderot_test_"

	// baseAdvisoryLock is the 64-bit key for the session-level advisory
	// lock that guards base-DB creation + all clone DDL. The value is
	// arbitrary but must be stable across processes (it's the whole
	// point — different `go test` package processes coordinate through
	// it). pg_advisory_lock(bigint) namespace is shared cluster-wide.
	baseAdvisoryLock int64 = 0x0D1DE7000DB7E57 // "diderot db test"-ish nonce

	// staleAfter is how old an unheld, unconnected clone must be before a
	// sweep drops it. It exceeds the suite's 30-minute -timeout, so no
	// test process, even one that takes no in-use lock, is still using it.
	staleAfter = time.Hour
)

// staleClone is the condition on pg_database d under which a sweep drops a
// clone: it is not a base, its PG_VERSION file was written more than $2
// seconds ago, and nobody is connected to it. pg_stat_file gives NULL for a
// missing file, which is never stale. An autovacuum worker visiting the
// clone is nobody: the launcher sends one into every database in turn, and
// DROP DATABASE ends it rather than failing.
const staleClone = `NOT starts_with(d.datname, 'diderot_test_base')
	AND (pg_stat_file('base/' || d.oid || '/PG_VERSION', true)).modification < now() - make_interval(secs => $2)
	AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datid = d.oid AND a.backend_type <> 'autovacuum worker')`

// maintenanceDB is the database we connect to in order to run
// CREATE/DROP DATABASE and hold the advisory lock. CREATE DATABASE
// cannot run inside the database being created/dropped, so we always
// do DDL from a connection to "postgres". The base DB URL's own
// database name is swapped out for this.
const maintenanceDB = "postgres"

// once guards the in-process base-DB bootstrap. The advisory lock
// handles cross-process coordination; once avoids redundant work
// (re-running migrations, re-locking) when many packages in the SAME
// process call URL — though in practice each test package is its own
// process, this keeps repeated URL calls within a package cheap.
var (
	once    sync.Once
	baseErr error

	// provisionMu serializes per-package clone DDL within a process.
	// Cross-process serialization is the advisory lock; this just
	// avoids two getters in one package racing the same clone. It also
	// guards holder and held.
	provisionMu sync.Mutex

	// holder is the maintenance connection on which this process holds a
	// shared in-use lock for each clone name in held (holdInUse).
	holder *pgx.Conn
	held   map[string]bool
)

// baseDBName is the schema-fingerprinted golden database name for
// THIS build's embedded migrations. Function (not const) because it
// depends on the compiled-in atlas.sum.
func baseDBName() string {
	return baseDBPrefix + migrations.SumHash()
}

// URL returns a connection URL to a per-package database cloned from
// the migrated base. pkgKey is a short stable identifier for the
// calling package (e.g. "events", "slack", "ask"); it becomes the
// suffix of the database name (diderot_test_<pkgKey>).
//
// If DIDEROT_TEST_DB_URL is unset, URL calls t.Skip and returns "".
// This preserves the original skip behavior — the caller's pool
// getter should treat a skip as "no DB available" exactly as before.
//
// The first call in a process (across all packages, coordinated by an
// advisory lock) ensures diderot_test_base exists and is migrated, then
// starts a sweep of stale clones. Every call drops + recreates
// diderot_test_<pkgKey> from that base, so the package starts from the
// golden schema with empty tables.
func URL(t *testing.T, pkgKey string) string {
	t.Helper()

	base := os.Getenv("DIDEROT_TEST_DB_URL")
	if base == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set; skipping DB integration test")
		return ""
	}

	pkgKey = sanitizeKey(pkgKey)
	if pkgKey == "" {
		t.Fatalf("testdb.URL: empty pkgKey")
	}
	dbName := dbNamePrefix + pkgKey
	if key := checkoutKey(); key != "" {
		dbName = dbNamePrefix + key + "_" + pkgKey
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ensure the base exists + is migrated (once per process; advisory
	// lock coordinates across processes).
	if err := ensureBase(ctx, base); err != nil {
		t.Fatalf("testdb: ensure base database: %v", err)
	}

	// Clone the base into this package's database (drop-and-recreate).
	if err := provisionPackageDB(ctx, base, dbName); err != nil {
		t.Fatalf("testdb: provision %q: %v", dbName, err)
	}

	return withDatabase(base, dbName)
}

// ensureBase creates diderot_test_base (if absent) and applies all
// migrations to it, exactly once per process and under a cross-process
// advisory lock so concurrent package processes don't double-create or
// race the migration, then starts a sweep of stale clones in the
// background, which runs until it finishes or the process exits.
// Subsequent calls reuse the sync.Once result.
func ensureBase(ctx context.Context, baseURL string) error {
	once.Do(func() {
		baseErr = bootstrapBase(ctx, baseURL)
		if baseErr == nil {
			go sweep(context.Background(), baseURL, dbNamePrefix, staleAfter, log.Printf)
		}
	})
	return baseErr
}

func bootstrapBase(ctx context.Context, baseURL string) error {
	adminURL := withDatabase(baseURL, maintenanceDB)

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to maintenance db: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Session-level advisory lock: held until we explicitly unlock (or
	// the connection closes). Blocks peer processes here until the
	// first finishes creating + migrating the base.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", baseAdvisoryLock); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", baseAdvisoryLock)
	}()

	base := baseDBName()
	exists, err := databaseExists(ctx, conn, base)
	if err != nil {
		return fmt.Errorf("check base existence: %w", err)
	}

	// If a peer process created+migrated the base while we waited on
	// the lock, we're done — its migrations are durable in the base DB.
	// The fingerprinted name makes "exists" trustworthy: a base for a
	// DIFFERENT migration set has a different name entirely, so the
	// stale-schema reuse bug (base migrated by an older checkout,
	// never re-migrated) can't recur.
	if !exists {
		if err := createDatabase(ctx, conn, base, ""); err != nil {
			return fmt.Errorf("create base database: %w", err)
		}
		// Apply migrations to the freshly-created base. migrations.Apply
		// runs atlas against the base DB URL.
		migURL := withDatabase(baseURL, base)
		if _, err := migrations.Apply(ctx, migURL, nil); err != nil {
			// If migration fails, drop the half-baked base so the next
			// run starts clean rather than cloning a broken schema.
			_ = dropDatabase(ctx, conn, base)
			return fmt.Errorf("apply migrations to base: %w", err)
		}
		// GC bases fingerprinted for other migration sets — each is
		// ~the size of the full schema and they accrete one per
		// migration change otherwise. Still under the advisory lock;
		// best-effort (a base busy serving a concurrent OLD-checkout
		// test run refuses the drop, which is fine — that run's next
		// fresh boot will GC it).
		gcStaleBases(ctx, conn, base)
	}

	return nil
}

// gcStaleBases drops every diderot_test_base_* database except keep.
// Errors are ignored per-database: a template in use by a concurrent
// clone refuses DROP, and that's acceptable — GC re-runs on every fresh
// bootstrap.
func gcStaleBases(ctx context.Context, conn *pgx.Conn, keep string) {
	rows, err := conn.Query(ctx, `
		SELECT datname FROM pg_database
		 WHERE datname LIKE $1 AND datname <> $2
	`, baseDBPrefix+"%", keep)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return
		}
		stale = append(stale, name)
	}
	rows.Close()
	for _, name := range stale {
		_ = dropDatabase(ctx, conn, name)
	}
}

// sweep drops the stale clones (staleClone) whose names start with prefix
// and that no live process holds. Cleanup never fails a test: an error ends
// the sweep with one line through logf.
func sweep(ctx context.Context, baseURL, prefix string, olderThan time.Duration, logf func(format string, args ...any)) {
	if err := dropStale(ctx, baseURL, prefix, olderThan.Seconds()); err != nil {
		logf("testdb: not dropping stale test databases: %v", err)
	}
}

func dropStale(ctx context.Context, baseURL, prefix string, olderThan float64) error {
	conn, err := pgx.Connect(ctx, withDatabase(baseURL, maintenanceDB))
	if err != nil {
		return fmt.Errorf("connect to maintenance db: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `SELECT d.datname FROM pg_database d WHERE starts_with(d.datname, $1) AND `+staleClone, prefix, olderThan)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := dropIfStale(ctx, conn, name, olderThan); err != nil {
			return fmt.Errorf("drop %q: %w", name, err)
		}
	}
	return nil
}

// dropIfStale drops the named clone unless a live process holds it (another
// session has its in-use lock), it stopped being stale since the sweep listed
// it (a peer re-provisioned it), or someone connected to it before the drop.
// It checks and drops under the clone-DDL advisory lock, so no provisioning
// runs between the check and the drop. A concurrent sweep holds the in-use
// lock exclusively while it drops, so the two never drop the same clone.
func dropIfStale(ctx context.Context, conn *pgx.Conn, name string, olderThan float64) error {
	var free bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", inUseKey(name)).Scan(&free); err != nil {
		return err
	}
	if !free {
		return nil
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", inUseKey(name)) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", baseAdvisoryLock); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", baseAdvisoryLock) }()

	var stale bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database d WHERE d.datname = $1 AND `+staleClone+`)`, name, olderThan).Scan(&stale); err != nil {
		return err
	}
	if !stale {
		return nil
	}
	err := dropDatabase(ctx, conn, name)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55006" { // object_in_use: someone connected
		return nil
	}
	return err
}

// inUseKey is the advisory lock key that marks the named clone in use.
func inUseKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("diderot testdb in use: " + name))
	return int64(h.Sum64())
}

// holdInUse marks dbName in use until this process exits: a shared advisory
// lock, taken once per name, on a maintenance connection the process keeps
// open. It waits while a sweep holds the lock exclusively to drop the clone.
// The caller holds provisionMu.
func holdInUse(ctx context.Context, baseURL, dbName string) error {
	if holder == nil || holder.IsClosed() {
		conn, err := pgx.Connect(ctx, withDatabase(baseURL, maintenanceDB))
		if err != nil {
			return fmt.Errorf("connect to maintenance db: %w", err)
		}
		// A closed connection's locks were released with it.
		holder, held = conn, map[string]bool{}
	}
	if held[dbName] {
		return nil
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", inUseKey(dbName)); err != nil {
		return err
	}
	held[dbName] = true
	return nil
}

// provisionPackageDB drops (if present) and recreates the package's
// database from the base template, under the same advisory lock used
// for base creation. The drop-and-recreate gives a clean schema with
// empty tables every process start; intra-package TRUNCATEs handle
// between-test cleanup.
//
// CREATE DATABASE … TEMPLATE requires that NO session be connected to
// the template database. The base DB is never connected to after
// bootstrap (we only ever connect to clones), so the template is free.
// We still serialize clone DDL (advisory lock + provisionMu) because
// two CREATE … TEMPLATE against the same template can race.
func provisionPackageDB(ctx context.Context, baseURL, dbName string) error {
	provisionMu.Lock()
	defer provisionMu.Unlock()

	// Before the clone-DDL lock: a sweep takes the in-use lock first, then
	// the clone-DDL lock, and this order keeps the two from deadlocking.
	if err := holdInUse(ctx, baseURL, dbName); err != nil {
		return fmt.Errorf("hold %q in use: %w", dbName, err)
	}

	adminURL := withDatabase(baseURL, maintenanceDB)
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to maintenance db: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", baseAdvisoryLock); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", baseAdvisoryLock)
	}()

	// Terminate any lingering backends connected to this package DB
	// (e.g. a leaked pool from a crashed prior run in the same process)
	// so DROP DATABASE doesn't fail with "database is being accessed by
	// other users". WITH (FORCE) (PG13+) would do this too, but
	// terminate-then-drop works on every supported version.
	if err := terminateBackends(ctx, conn, dbName); err != nil {
		return fmt.Errorf("terminate backends on %q: %w", dbName, err)
	}
	if err := dropDatabase(ctx, conn, dbName); err != nil {
		return fmt.Errorf("drop %q: %w", dbName, err)
	}
	if err := createDatabase(ctx, conn, dbName, baseDBName()); err != nil {
		// Self-heal a vanished template: a concurrent test run from a
		// DIFFERENT checkout GCs bases that aren't its own, and can
		// race us between our bootstrap and this clone. Recreate the
		// base inline on the connection (and advisory lock) we
		// already hold — calling bootstrapBase here would deadlock,
		// since it opens a new session and blocks on the same lock.
		if isMissingDatabase(err) {
			if berr := recreateBase(ctx, conn, baseURL); berr != nil {
				return fmt.Errorf("recreate base after GC race: %w", berr)
			}
			if err := createDatabase(ctx, conn, dbName, baseDBName()); err != nil {
				return fmt.Errorf("create %q from template (after recreate): %w", dbName, err)
			}
			return nil
		}
		return fmt.Errorf("create %q from template: %w", dbName, err)
	}
	return nil
}

// recreateBase re-creates + re-migrates this build's base database
// on an existing maintenance connection whose session already holds
// the advisory lock. Used only on the clone-time GC race path.
func recreateBase(ctx context.Context, conn *pgx.Conn, baseURL string) error {
	base := baseDBName()
	exists, err := databaseExists(ctx, conn, base)
	if err != nil {
		return fmt.Errorf("check base existence: %w", err)
	}
	if exists {
		return nil
	}
	if err := createDatabase(ctx, conn, base, ""); err != nil {
		return fmt.Errorf("create base database: %w", err)
	}
	if _, err := migrations.Apply(ctx, withDatabase(baseURL, base), nil); err != nil {
		_ = dropDatabase(ctx, conn, base)
		return fmt.Errorf("apply migrations to base: %w", err)
	}
	return nil
}

// isMissingDatabase reports whether err is Postgres "database does
// not exist" (SQLSTATE 3D000) — the clone-time signal that our
// template was GC'd by a concurrent run from another checkout.
func isMissingDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "3D000"
}

// databaseExists reports whether a database with the given name is
// present in pg_database.
func databaseExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name,
	).Scan(&exists)
	return exists, err
}

// createDatabase issues CREATE DATABASE. If template is non-empty the
// new DB is cloned from it. Database names are validated identifiers
// (sanitizeKey + a fixed prefix) so the interpolation below is safe —
// CREATE DATABASE does not accept parameter placeholders.
func createDatabase(ctx context.Context, conn *pgx.Conn, name, template string) error {
	stmt := fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))
	if template != "" {
		stmt += fmt.Sprintf(" TEMPLATE %s", quoteIdent(template))
	}
	_, err := conn.Exec(ctx, stmt)
	return err
}

// dropDatabase drops the database if it exists. Safe to call when the
// database is absent (IF EXISTS).
func dropDatabase(ctx context.Context, conn *pgx.Conn, name string) error {
	_, err := conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(name)))
	return err
}

// terminateBackends kills all sessions connected to the named database
// so a subsequent DROP can proceed. Ignores the current backend (it's
// connected to the maintenance DB, not the target, but the filter is
// belt-and-suspenders).
func terminateBackends(ctx context.Context, conn *pgx.Conn, name string) error {
	_, err := conn.Exec(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
	return err
}

// withDatabase returns baseURL with its path (database name) replaced
// by dbName, preserving scheme, credentials, host, and query params
// (sslmode, search_path, …). DIDEROT_TEST_DB_URL is a standard
// postgres:// URL, so net/url round-trips it cleanly.
func withDatabase(baseURL, dbName string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		// Fall back to naive replacement only if the URL is unparseable,
		// which shouldn't happen for a real DIDEROT_TEST_DB_URL. Returning
		// baseURL unchanged would point at the wrong DB, so surface the
		// problem loudly via an obviously-bad URL.
		return baseURL + "#unparseable-url-" + dbName
	}
	u.Path = "/" + dbName
	return u.String()
}

// sanitizeKey lowercases the package key and strips anything that
// isn't a safe identifier char, so the database name is a valid,
// unquoted-friendly identifier. We still quote on use.
func sanitizeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '/', r == '-', r == '.':
			b.WriteRune('_')
		}
	}
	out := b.String()
	// Postgres identifiers cap at 63 bytes; with the diderot_test_
	// prefix (13 chars) we keep the key short. If a caller passes a
	// long path, hash-suffix to stay unique + bounded.
	const maxKeyLen = 40
	if len(out) > maxKeyLen {
		h := fnv.New32a()
		_, _ = h.Write([]byte(out))
		out = fmt.Sprintf("%s_%08x", out[:maxKeyLen-9], h.Sum32())
	}
	return out
}

// checkoutKey scopes per-package clones to one source checkout. Every run
// drops and recreates its package's clone, so two worktrees testing the same
// package at once used to drop each other's database mid-run (SQLSTATE
// 57P01, "terminating connection due to administrator command").
// DIDEROT_TEST_DB_PREFIX overrides the key. Otherwise it is derived from the
// checkout's module root: `go test` runs each package's binary from that
// package's absolute source directory, so walking up to go.mod finds a
// per-worktree path even under -trimpath (which would make compiled source
// paths identical across checkouts). The key is always 8 hex characters,
// keeping clone names within Postgres's 63-byte identifier limit. Clones left
// by deleted worktrees are dropped by a later sweep (see Cleanup).
func checkoutKey() string {
	source := strings.TrimSpace(os.Getenv("DIDEROT_TEST_DB_PREFIX"))
	if source == "" {
		source = moduleRoot()
	}
	if source == "" {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(source))
	return fmt.Sprintf("%08x", h.Sum32())
}

// moduleRoot returns the absolute directory holding the nearest go.mod above
// the working directory, or the working directory itself when none is found.
func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if parent := filepath.Dir(d); parent == d {
			return dir
		}
	}
}

// quoteIdent wraps a SQL identifier in double quotes, doubling any
// embedded double quotes. Our identifiers are already sanitized, but
// quoting is the correct discipline for interpolated identifiers.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
