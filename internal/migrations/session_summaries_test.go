package migrations

import (
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewInstallSummarizesSessions: a new install starts with session
// summaries unpaused, whether it is built from the baseline or from the
// history the baseline replaced.
func TestNewInstallSummarizesSessions(t *testing.T) {
	fresh := emptyDatabase(t)
	_, err := Apply(t.Context(), fresh, nil)
	require.NoError(t, err)
	assert.True(t, summariesEnabled(t, fresh))

	upgraded := emptyDatabase(t)
	applyDir(t, upgraded, oldHistory(t, true))
	_, err = Apply(t.Context(), upgraded, nil)
	require.NoError(t, err)
	assert.True(t, summariesEnabled(t, upgraded))
}

// TestStartedInstallKeepsItsSummaryPause: on a database a server has started
// on, built from the baseline or from the history it replaced, the migration
// leaves the switch as it is: paused stays paused, since that may be an
// operator's choice, and on stays on.
func TestStartedInstallKeepsItsSummaryPause(t *testing.T) {
	type install struct {
		name    string
		history fs.FS
		enabled bool
	}
	baseline, old := migrationsThrough(t, sessionFeedRenameVersion), oldHistory(t, true)
	for _, tc := range []install{
		{"baseline, paused", baseline, false}, {"baseline, on", baseline, true},
		{"old history, paused", old, false}, {"old history, on", old, true},
	} {
		enabled := tc.enabled
		dbURL := emptyDatabase(t)
		applyDir(t, dbURL, tc.history)
		conn, err := pgx.Connect(t.Context(), dbURL)
		require.NoError(t, err)
		// What every server start writes: a row for each connector.
		_, err = conn.Exec(t.Context(), `
			INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
			VALUES ('models', '{}', '{}', 1, 'diderot:registry', now())`)
		require.NoError(t, err)
		_, err = conn.Exec(t.Context(), `UPDATE session_ingest_control SET summarization_enabled = $1`, enabled)
		require.NoError(t, err)
		require.NoError(t, conn.Close(t.Context()))

		_, err = Apply(t.Context(), dbURL, nil)
		require.NoError(t, err)
		assert.Equal(t, enabled, summariesEnabled(t, dbURL), tc.name)
	}
}

func summariesEnabled(t *testing.T, dbURL string) bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(t.Context()) }()
	var enabled bool
	require.NoError(t, conn.QueryRow(t.Context(),
		`SELECT summarization_enabled FROM session_ingest_control WHERE id`).Scan(&enabled))
	return enabled
}
