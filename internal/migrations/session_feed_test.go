package migrations

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionFeedConnectorKeepsItsSettings upgrades a database that ran the
// session feed connector under its old name, skipper. Its settings row, with
// its secrets, last test, and version, and its source state move to
// session_feed unchanged; the audit log keeps the old name.
func TestSessionFeedConnectorKeepsItsSettings(t *testing.T) {
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, baselineOnly(t))
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	_, err = conn.Exec(t.Context(), `
		INSERT INTO connector_settings (connector, enabled, settings, secrets, version, last_test, updated_by, updated_at)
		VALUES ('skipper', true,
		        '{"url": "https://feed.example.com", "viewer_base_url": "https://sessions.example.com"}',
		        '{"token": {"ciphertext": "AQIDBA==", "fingerprint": "0a1b2c3d", "set_by": "admin@example.com", "set_at": "2026-11-01T00:00:00Z"}}',
		        7,
		        '{"checks": [{"name": "Feed", "passed": true, "detail": "The feed answered with the token."}], "passed": true, "by": "admin@example.com", "at": "2026-11-01T00:00:00Z"}',
		        'admin@example.com', '2026-11-01T00:00:00Z');
		INSERT INTO source_state (source, key, value) VALUES ('skipper', 'last_read', '{"at": "2026-11-01T00:00:00Z"}');
		INSERT INTO admin_audit_log (actor_email, action, target, status, details)
		VALUES ('admin@example.com', 'connectors.skipper.enable', 'connector_settings:skipper', 'succeeded', '{"version": 7}')`)
	require.NoError(t, err)
	settingsRow := `SELECT (to_jsonb(c) - 'connector')::text FROM connector_settings c WHERE connector = $1`
	var before string
	require.NoError(t, conn.QueryRow(t.Context(), settingsRow, "skipper").Scan(&before))
	audit := `SELECT to_jsonb(a)::text FROM admin_audit_log a ORDER BY id`
	auditBefore := queryStrings(t, conn, audit)

	_, err = Apply(t.Context(), dbURL, nil)
	require.NoError(t, err)

	var after string
	require.NoError(t, conn.QueryRow(t.Context(), settingsRow, "session_feed").Scan(&after))
	assert.JSONEq(t, before, after, "the row moves with every column unchanged")
	assert.Empty(t, queryStrings(t, conn, `SELECT connector FROM connector_settings WHERE connector = 'skipper'`))
	assert.Equal(t, []string{`session_feed last_read {"at": "2026-11-01T00:00:00Z"}`},
		queryStrings(t, conn, `SELECT format('%s %s %s', source, key, value) FROM source_state`))
	assert.Equal(t, auditBefore, queryStrings(t, conn, audit), "the audit log keeps the old name")
}

// TestSessionFeedRenameWithoutTheConnector: on a database that never had the
// connector's row, as on a new install before the server first starts, the
// rename changes nothing.
func TestSessionFeedRenameWithoutTheConnector(t *testing.T) {
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, baselineOnly(t))
	before := describeDatabase(t, dbURL)
	applyDir(t, dbURL, migrationsThrough(t, sessionFeedRenameVersion))
	assert.Equal(t, before, describeDatabase(t, dbURL))
}

const sessionFeedRenameVersion = "20261121000001"

// baselineOnly returns a migration directory holding only the baseline, the
// schema a database has before any later migration.
func baselineOnly(t *testing.T) fs.FS {
	t.Helper()
	return migrationsThrough(t, baselineVersion)
}

// migrationsThrough returns a migration directory holding this build's
// migrations up to and including version.
func migrationsThrough(t *testing.T, version string) fs.FS {
	t.Helper()
	files := map[string]string{}
	found := false
	for name, body := range migrationFiles(t) {
		fileVersion, _, _ := strings.Cut(name, "_")
		if fileVersion <= version {
			files[name] = body
		}
		found = found || fileVersion == version
	}
	require.True(t, found, "no migration has version %s", version)
	return migrationDir(t, files)
}

func queryStrings(t *testing.T, conn *pgx.Conn, query string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), query)
	require.NoError(t, err)
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return values
}
