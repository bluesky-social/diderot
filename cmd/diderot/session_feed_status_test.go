package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/source"
)

// TestSessionFeedStatusCountsSessions: the page shows when the feed was
// last read and how many sessions wait, are indexed, excluded or deleted.
func TestSessionFeedStatusCountsSessions(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_session_status")
	_, err := pool.Exec(ctx, `TRUNCATE session_ingest_candidates, session_ingest_cursor CASCADE`)
	require.NoError(t, err)
	old := time.Now().Add(-48 * time.Hour)
	_, err = pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		       (owner, owner_canonical, client, session_id, revision, complete, first_pending_at)
		VALUES ('operator@example.com', 'operator@example.com', 'codex', 'pending-session', 'rev-1', true, $1),
		       ('operator@example.com', 'operator@example.com', 'codex', 'deleted-session', 'rev-2', true, $1),
		       ('operator@example.com', 'operator@example.com', 'codex', 'parked-session', 'rev-3', true, $1)`, old)
	require.NoError(t, err)
	// Every attempt at this one failed. Nothing tries it again, so it is not
	// waiting, however long ago it arrived.
	_, err = pool.Exec(ctx, `UPDATE session_ingest_candidates
		SET index_failure_revision = revision, index_failure_count = 5, index_last_error = 'status 503',
		    index_last_failed_at = now(), index_quarantined_at = now()
		WHERE session_id = 'parked-session'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE session_ingest_candidates SET deleted = true, excluded_at = now()
		WHERE session_id = 'deleted-session'`)
	require.NoError(t, err)

	status, err := (&sessionFeedSource{}).Status(ctx, source.Deps{Pool: pool})
	require.NoError(t, err)
	values := map[string]any{}
	for _, row := range status.Rows {
		values[row.Label] = row.Value
	}
	require.Nil(t, values["Last read"].(*time.Time), "the feed was never read")
	require.Equal(t, int64(3), values["Sessions seen"])
	require.Equal(t, int64(1), values["Waiting for a summary"])
	require.Equal(t, int64(0), values["Indexed"])
	require.Equal(t, int64(1), values["Excluded"])
	require.Equal(t, int64(1), values["Deleted"])
	require.Equal(t, int64(2), values["Waiting over a day"])
	require.Equal(t, int64(1), values["Could not be summarized"])
	require.Contains(t, status.Note, "DIDEROT_SESSION_INGEST_POLICY_PATH")
	require.Empty(t, status.Waiting, "without summaries to ask, nothing is said about them")

	// While summaries can't run, the page says why.
	_, err = pool.Exec(ctx, `UPDATE session_ingest_control SET summarization_enabled = true`)
	require.NoError(t, err)
	summaries := sessioningest.NewSummaryControl(sessioningest.NewStore(pool), func() string { return "No model can write session summaries: set ANTHROPIC_API_KEY." })
	status, err = (&sessionFeedSource{summaries: &summaries}).Status(ctx, source.Deps{Pool: pool})
	require.NoError(t, err)
	require.Equal(t, "Sessions aren't summarized, so they aren't searchable. No model can write session summaries: set ANTHROPIC_API_KEY.", status.Waiting)
	summaries.NoModel = nil
	status, err = (&sessionFeedSource{summaries: &summaries}).Status(ctx, source.Deps{Pool: pool})
	require.NoError(t, err)
	require.Empty(t, status.Waiting)
}

// TestSessionFeedStatusLastRead: a new database's cursor row is not a read
// of the feed; the first read is.
func TestSessionFeedStatusLastRead(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_session_status_read")
	_, err := pool.Exec(ctx, `TRUNCATE session_ingest_candidates, session_ingest_cursor CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO session_ingest_cursor DEFAULT VALUES`)
	require.NoError(t, err)
	lastRead := func() *time.Time {
		status, err := (&sessionFeedSource{}).Status(ctx, source.Deps{Pool: pool})
		require.NoError(t, err)
		return status.Rows[0].Value.(*time.Time)
	}
	require.Nil(t, lastRead())
	_, err = pool.Exec(ctx, `UPDATE session_ingest_cursor SET policy_hash = 'p', updated_at = now()`)
	require.NoError(t, err)
	require.NotNil(t, lastRead())
}
