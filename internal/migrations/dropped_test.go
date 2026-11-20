package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestDroppedTablesStayDropped checks that a migrated database has none of the
// tables nothing reads or writes any more, nor their sequences.
func TestDroppedTablesStayDropped(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "migrations_dropped"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	for _, name := range []string{
		"bedrock_chunk_summaries",
		"bedrock_session_summaries",
		"bedrock_prompts",
		"bedrock_prompts_id_seq",
		"legacy_session_discovery_stub_chunk_archives",
		"legacy_session_discovery_stub_archives",
		"session_rekey_map",
		"session_revision_rekey_map",
		"notion_root_pages",
		"eval_questions",
		"eval_questions_id_seq",
		"eval_golden_set_splits",
		"eval_label_runs",
		"eval_judgments",
		"eval_pool_candidates",
		"eval_runs",
		"eval_question_triage_runs",
		"eval_question_triage",
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists))
		require.False(t, exists, name)
	}
}
