package migrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ariga.io/atlas-go-sdk/atlasexec"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusal is the error a database partway through the replaced history gets.
const refusal = "This database was migrated by a Diderot release older than v0.9.0. " +
	"Deploy v0.9.0 first and let it start once, then deploy this release."

// The replaced history is gone, so these tests stand in for it with a
// synthetic one under the same versions: its first file builds the whole
// schema (the baseline's own statements), and stand-ins for the later
// migrations change nothing. Atlas compares only versions, so a database that
// applied this history looks to the baseline exactly like one that applied
// the real history.
const (
	firstOldVersion = "20260512000001"
	// lastV080Version is the newest migration v0.8.0 applies; v0.9.0 adds
	// sourceStateVersion, dropDefaultVersion, and then baselineVersion.
	lastV080Version    = "20261117000001"
	sourceStateVersion = "20261118000001"
	dropDefaultVersion = "20261119000001"
)

func TestEmptyDatabaseGetsTheBaseline(t *testing.T) {
	dbURL := emptyDatabase(t)
	applied, err := Apply(t.Context(), dbURL, nil)
	require.NoError(t, err)
	assert.Equal(t, len(migrationFiles(t)), applied)
	assert.Equal(t, baselineVersion, revisions(t, dbURL)[0])

	before := describeDatabase(t, dbURL)
	applied, err = Apply(t.Context(), dbURL, nil)
	require.NoError(t, err)
	assert.Zero(t, applied, "a database already on the baseline has nothing to apply")
	assert.Equal(t, before, describeDatabase(t, dbURL))
}

// TestFullyMigratedDatabaseSkipsTheBaseline upgrades a database that applied
// the whole replaced history, with no step beforehand. Only the migrations
// after the baseline run, its rows survive, and it ends up exactly like a new
// install.
func TestFullyMigratedDatabaseSkipsTheBaseline(t *testing.T) {
	upgraded := emptyDatabase(t)
	applyDir(t, upgraded, oldHistory(t, true))
	insertEvent(t, upgraded)
	history := revisions(t, upgraded)

	applied, err := Apply(t.Context(), upgraded, nil)
	require.NoError(t, err)
	assert.Equal(t, len(migrationFiles(t))-1, applied, "everything but the baseline runs")
	assert.Equal(t, append(history, laterVersions(t)...), revisions(t, upgraded))

	fresh := emptyDatabase(t)
	_, err = Apply(t.Context(), fresh, nil)
	require.NoError(t, err)
	insertEvent(t, fresh)
	assert.Equal(t, describeDatabase(t, fresh), describeDatabase(t, upgraded))
}

func TestPartlyMigratedDatabaseIsRefusedUntouched(t *testing.T) {
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, oldHistory(t, false))
	insertEvent(t, dbURL)
	before, history := describeDatabase(t, dbURL), revisions(t, dbURL)

	_, err := Apply(t.Context(), dbURL, nil)
	require.ErrorContains(t, err, refusal)
	assert.Equal(t, before, describeDatabase(t, dbURL), "nothing was created or changed")
	assert.Equal(t, history, revisions(t, dbURL), "no revision was recorded")

	// The way out the refusal names: the last release of the old history
	// applies its remaining migrations, and then the baseline is skipped.
	applyDir(t, dbURL, oldHistory(t, true))
	applied, err := Apply(t.Context(), dbURL, nil)
	require.NoError(t, err)
	assert.Equal(t, len(migrationFiles(t))-1, applied, "everything but the baseline runs")
}

func TestLaterMigrationsApplyToBothKindsOfDatabase(t *testing.T) {
	files := migrationFiles(t)
	files["20991231000001_later.sql"] = "CREATE TABLE later_test (id integer PRIMARY KEY);\n"
	withLater := migrationDir(t, files)

	fresh := emptyDatabase(t)
	assert.Equal(t, len(files), applyDir(t, fresh, withLater))

	upgraded := emptyDatabase(t)
	applyDir(t, upgraded, oldHistory(t, true))
	assert.Equal(t, len(files)-1, applyDir(t, upgraded, withLater), "everything but the baseline runs")

	assert.Equal(t, describeDatabase(t, fresh), describeDatabase(t, upgraded))
	assert.Contains(t, describeDatabase(t, fresh), "\nlater_test kind=r ")
}

// TestOldReleaseAcceptsABaselineDatabase covers rolling back to the last
// release of the old history after a new install was built from the baseline:
// that release finds its own last version applied and changes nothing.
func TestOldReleaseAcceptsABaselineDatabase(t *testing.T) {
	dbURL := emptyDatabase(t)
	_, err := Apply(t.Context(), dbURL, nil)
	require.NoError(t, err)
	assert.Zero(t, applyDir(t, dbURL, oldHistory(t, true)))
}

// TestRecordedVersionsWithoutFilesAreIgnored upgrades a database whose revision
// table also remembers versions that no migration directory has, as a
// long-lived install's does: files deleted from the history before it was
// replaced, older than all of it or between its versions. Atlas checks that
// the directory's files are recorded, never that recorded versions have files,
// so the upgrade goes as for any database that applied the whole history.
func TestRecordedVersionsWithoutFilesAreIgnored(t *testing.T) {
	upgraded := emptyDatabase(t)
	applyDir(t, upgraded, oldHistory(t, true))
	insertEvent(t, upgraded)
	recordDeletedVersions(t, upgraded,
		"20260508000001", "20260510180000", // older than the whole history
		"20260601000001", "20261118500000", // between versions it has
	)
	history := revisionRows(t, upgraded)

	applied, err := Apply(t.Context(), upgraded, nil)
	require.NoError(t, err)
	assert.Equal(t, len(migrationFiles(t))-1, applied, "everything but the baseline runs")
	assert.Equal(t, history, revisionRows(t, upgraded)[:len(history)], "the recorded revisions are untouched")

	fresh := emptyDatabase(t)
	_, err = Apply(t.Context(), fresh, nil)
	require.NoError(t, err)
	insertEvent(t, fresh)
	assert.Equal(t, describeDatabase(t, fresh), describeDatabase(t, upgraded))

	files := migrationFiles(t)
	files["20991231000001_later.sql"] = "CREATE TABLE later_test (id integer PRIMARY KEY);\n"
	assert.Equal(t, 1, applyDir(t, upgraded, migrationDir(t, files)), "only the later migration runs")
}

// oldHistory returns a synthetic stand-in for the replaced history: as far as
// v0.8.0 took it, or, when complete, as far as v0.9.0 does, up to the version
// the baseline carries.
func oldHistory(t *testing.T, complete bool) fs.FS {
	t.Helper()
	files := map[string]string{
		firstOldVersion + "_initial_schema.sql":      string(baselineSQL(t)),
		lastV080Version + "_slack_notify_emails.sql": "SELECT 1;\n",
	}
	if complete {
		files[sourceStateVersion+"_source_state.sql"] = "SELECT 2;\n"
		files[dropDefaultVersion+"_drop_recovery_source_default.sql"] = "SELECT 3;\n"
		files[baselineVersion+"_drop_unused_tables.sql"] = "SELECT 4;\n"
	}
	return migrationDir(t, files)
}

// migrationFiles returns this build's migration files by name.
func migrationFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := migrationsFS.ReadDir("sql")
	require.NoError(t, err)
	files := map[string]string{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		data, err := migrationsFS.ReadFile("sql/" + entry.Name())
		require.NoError(t, err)
		files[entry.Name()] = string(data)
	}
	return files
}

// laterVersions returns the versions of this build's migrations after the
// baseline, oldest first.
func laterVersions(t *testing.T) []string {
	t.Helper()
	var versions []string
	for name := range migrationFiles(t) {
		version, _, _ := strings.Cut(name, "_")
		if version != baselineVersion {
			versions = append(versions, version)
		}
	}
	slices.Sort(versions)
	return versions
}

// migrationDir writes files to a directory with an atlas.sum of its own.
func migrationDir(t *testing.T, files map[string]string) fs.FS {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	bin, err := atlasBinary()
	require.NoError(t, err)
	client, err := atlasexec.NewClient(dir, bin)
	require.NoError(t, err)
	require.NoError(t, client.MigrateHash(t.Context(), &atlasexec.MigrateHashParams{DirURL: "file://" + dir}))
	return os.DirFS(dir)
}

func applyDir(t *testing.T, dbURL string, dir fs.FS) int {
	t.Helper()
	applied, err := apply(t.Context(), dbURL, dir, nil)
	require.NoError(t, err)
	return applied
}

// revisions returns the versions Atlas has recorded, oldest first.
func revisions(t *testing.T, dbURL string) []string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	rows, err := conn.Query(t.Context(),
		`SELECT version FROM atlas_schema_revisions.atlas_schema_revisions ORDER BY version`)
	require.NoError(t, err)
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return versions
}

// insertEvent gives a database a row of data, as any install in use has.
func insertEvent(t *testing.T, dbURL string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	_, err = conn.Exec(t.Context(), `
		INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('markdown', 'notes/a.md', now(), 'body', '{}')`)
	require.NoError(t, err)
}

// recordDeletedVersions records revisions for migrations no directory has,
// like the ones an install keeps after their files were deleted: applied in
// full and without error.
func recordDeletedVersions(t *testing.T, dbURL string, versions ...string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	for _, version := range versions {
		_, err := conn.Exec(t.Context(), `
			INSERT INTO atlas_schema_revisions.atlas_schema_revisions
			       (version, description, type, applied, total, executed_at, execution_time,
			        error, error_stmt, hash, partial_hashes, operator_version)
			VALUES ($1, 'deleted', 2, 1, 1, now(), 0, '', '', 'h1:deleted', 'null', 'Atlas CLI v1.2.0')`,
			version)
		require.NoError(t, err)
	}
}

// revisionRows returns every recorded revision with all its columns, oldest
// first.
func revisionRows(t *testing.T, dbURL string) []string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	rows, err := conn.Query(t.Context(),
		`SELECT to_jsonb(r)::text FROM atlas_schema_revisions.atlas_schema_revisions r ORDER BY version`)
	require.NoError(t, err)
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return values
}
