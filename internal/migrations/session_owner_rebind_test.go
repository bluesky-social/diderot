package migrations

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// priorToOwnerRebindRetireVersion is the last migration before the cleanup
// this file tests.
const priorToOwnerRebindRetireVersion = "20261202000001"

// TestOwnerRebindRetireMigrationRemovesCandidatesDisagreeingWithBinding
// upgrades a database holding the stale state an earlier release could
// leave behind: a session_ingest_candidates row for a session
// whose session_ingest_bindings row (the sole durable authority for a
// session's current owner) now names a different owner_canonical. The
// migration removes only that row -- a candidate agreeing with its binding,
// and one with no binding at all, are both left alone.
func TestOwnerRebindRetireMigrationRemovesCandidatesDisagreeingWithBinding(t *testing.T) {
	ctx := t.Context()
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, migrationsThrough(t, priorToOwnerRebindRetireVersion))
	conn, err := pgx.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })

	_, err = conn.Exec(ctx, `
		-- Agrees with its binding: must survive.
		INSERT INTO session_ingest_bindings (session_id, owner_canonical, writer, format)
		VALUES ('ses-agrees', 'owner-current@example.com', 'feed', 'bsky.session.v1');
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, published_revision)
		VALUES ('owner-current@example.com', 'codex', 'ses-agrees', 'owner-current@example.com', 'sha256:v1');

		-- Disagrees with its binding (the former owner's row, orphaned by
		-- a rebind): must be removed.
		INSERT INTO session_ingest_bindings (session_id, owner_canonical, writer, format)
		VALUES ('ses-disagrees', 'owner-current@example.com', 'feed', 'bsky.session.v1');
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, published_revision)
		VALUES ('owner-former@example.com', 'codex', 'ses-disagrees', 'owner-former@example.com', 'sha256:v1');

		-- No binding row at all (predates the bindings cutover): nothing to
		-- disagree with, so it must be left alone.
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, published_revision)
		VALUES ('owner-legacy@example.com', 'codex', 'ses-no-binding', 'owner-legacy@example.com', 'sha256:v1');
	`)
	require.NoError(t, err)

	_, err = Apply(ctx, dbURL, nil)
	require.NoError(t, err)

	var remaining []string
	rows, err := conn.Query(ctx, `SELECT session_id FROM session_ingest_candidates ORDER BY session_id`)
	require.NoError(t, err)
	remaining, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"ses-agrees", "ses-no-binding"}, remaining,
		"the candidate disagreeing with its binding is removed; the agreeing one and the bindingless one survive")
}

// TestOwnerRebindRetireMigrationCleansUpLegacyBindinglessDuplicates covers
// the case the first migration deliberately leaves alone: a session the
// feed moved between owners before session_ingest_bindings existed, so two
// candidate rows for the same session_id disagree with each other with no
// binding row to catch it. The migration must keep only the feed's latest
// report (the higher change_seq) and remove the earlier, disagreeing-owner
// row along with its per-identity derived state -- the same cleanup
// retireRebindStaleCandidatesTx performs for a live rebind -- while never
// touching the surviving row's own derived state. A tie at the highest
// change_seq is left alone entirely.
func TestOwnerRebindRetireMigrationCleansUpLegacyBindinglessDuplicates(t *testing.T) {
	ctx := t.Context()
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, migrationsThrough(t, priorToOwnerRebindRetireVersion))
	conn, err := pgx.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })

	_, err = conn.Exec(ctx, `
		-- Two bindingless rows for one session_id: the feed moved
		-- "ses-legacy-dup" from owner-former to owner-current before
		-- the bindings table existed. owner-former's row is stale and
		-- carries derived state that must go with it.
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, change_seq, published_revision)
		VALUES ('owner-former@example.com', 'codex', 'ses-legacy-dup', 'owner-former@example.com', 1, 'sha256:v1');
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, change_seq, published_revision)
		VALUES ('owner-current@example.com', 'codex', 'ses-legacy-dup', 'owner-current@example.com', 2, 'sha256:v1');

		INSERT INTO session_ingest_summaries (owner, client, session_id, revision, narrative)
		VALUES ('owner-former@example.com', 'codex', 'ses-legacy-dup', 'sha256:v1', 'stale narrative'),
		       ('owner-current@example.com', 'codex', 'ses-legacy-dup', 'sha256:v1', 'current narrative');
		INSERT INTO session_ingest_chunk_summaries (owner, client, session_id, record_start, record_end, summary)
		VALUES ('owner-former@example.com', 'codex', 'ses-legacy-dup', 0, 10, 'stale chunk'),
		       ('owner-current@example.com', 'codex', 'ses-legacy-dup', 0, 10, 'current chunk');
		INSERT INTO session_ingest_map_checkpoints (session_id, record_start, owner, client, record_end, content_hash, map_input_digest, summary, model_id)
		VALUES ('ses-legacy-dup', 0, 'owner-former@example.com', 'codex', 10, 'h', 'd', 'stale map', 'm'),
		       ('ses-legacy-dup', 10, 'owner-current@example.com', 'codex', 20, 'h', 'd', 'current map', 'm');
		INSERT INTO session_ingest_published (owner, client, session_id, revision)
		VALUES ('owner-former@example.com', 'codex', 'ses-legacy-dup', 'sha256:v1'),
		       ('owner-current@example.com', 'codex', 'ses-legacy-dup', 'sha256:v1');

		-- A tie at the highest change_seq: neither row is preferred,
		-- so both must survive untouched.
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, change_seq, published_revision)
		VALUES ('owner-a@example.com', 'codex', 'ses-legacy-tie', 'owner-a@example.com', 5, 'sha256:v1');
		INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, change_seq, published_revision)
		VALUES ('owner-b@example.com', 'codex', 'ses-legacy-tie', 'owner-b@example.com', 5, 'sha256:v1');
	`)
	require.NoError(t, err)

	_, err = Apply(ctx, dbURL, nil)
	require.NoError(t, err)

	var remainingOwners []string
	rows, err := conn.Query(ctx, `SELECT owner FROM session_ingest_candidates WHERE session_id = 'ses-legacy-dup' ORDER BY owner`)
	require.NoError(t, err)
	remainingOwners, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"owner-current@example.com"}, remainingOwners,
		"only the feed's latest (higher change_seq) report survives")

	var tieOwners []string
	rows, err = conn.Query(ctx, `SELECT owner FROM session_ingest_candidates WHERE session_id = 'ses-legacy-tie' ORDER BY owner`)
	require.NoError(t, err)
	tieOwners, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"owner-a@example.com", "owner-b@example.com"}, tieOwners,
		"a tie at the highest change_seq has nothing to prefer, so both rows are left alone")

	for _, table := range []string{"session_ingest_summaries", "session_ingest_chunk_summaries"} {
		var count int
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-former@example.com'`,
		).Scan(&count))
		assert.Equal(t, 0, count, "%s: the retired row's derived state must go with it", table)

		require.NoError(t, conn.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-current@example.com'`,
		).Scan(&count))
		assert.Equal(t, 1, count, "%s: the surviving owner's derived state must be untouched", table)
	}
	var mapCheckpointCount int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM session_ingest_map_checkpoints WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-former@example.com'`,
	).Scan(&mapCheckpointCount))
	assert.Equal(t, 0, mapCheckpointCount, "the retired row's map checkpoint must go with it")
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM session_ingest_map_checkpoints WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-current@example.com'`,
	).Scan(&mapCheckpointCount))
	assert.Equal(t, 1, mapCheckpointCount, "the surviving owner's map checkpoint must be untouched")

	var formerSuperseded, currentSuperseded bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT superseded_at IS NOT NULL FROM session_ingest_published WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-former@example.com'`,
	).Scan(&formerSuperseded))
	assert.True(t, formerSuperseded, "the retired row's published ledger entry is marked superseded")
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT superseded_at IS NOT NULL FROM session_ingest_published WHERE session_id = 'ses-legacy-dup' AND owner = 'owner-current@example.com'`,
	).Scan(&currentSuperseded))
	assert.False(t, currentSuperseded, "the surviving owner's published ledger entry is untouched")
}
