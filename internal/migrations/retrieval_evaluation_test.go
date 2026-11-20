package migrations

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/prompts"
)

const askSessionLeasesVersion = "20261123000001"

// TestRetrievalEvaluationIsDropped upgrades a database that used retrieval
// evaluation: rows in every one of its tables, its jobs, its model saved and
// tested, and an active prompt catalog that still has its prompts. The
// migration drops what was the feature's alone, and the rest still loads.
func TestRetrievalEvaluationIsDropped(t *testing.T) {
	ctx := t.Context()
	dbURL := emptyDatabase(t)
	applyDir(t, dbURL, migrationsThrough(t, askSessionLeasesVersion))
	pool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	insertEvent(t, dbURL)
	_, err = pool.Exec(ctx, `
		INSERT INTO eval_questions (golden_set, question, source_kind) VALUES ('v1', 'Is it done?', 'mined');
		INSERT INTO eval_golden_set_splits (golden_set, seed, confirmation_percent, assigned_by)
		VALUES ('v1', 'seed', 10, 'operator@example.com');
		INSERT INTO eval_question_triage_runs (golden_set, model, rubric_version, rubric_prompt, audit_percent)
		VALUES ('v1', 'grader', 'v5', 'rubric', 20);
		INSERT INTO eval_question_triage (run_id, question_id, recommendation, confidence, reason, raw_response)
		VALUES (1, 1, 'approve', 'high', 'Replayable.', 'raw');
		UPDATE eval_questions
		   SET curated_at = now(), curated_by = 'operator@example.com', curation_method = 'model', applied_triage_id = 1;
		INSERT INTO eval_label_runs (golden_set, label_source) VALUES ('v1', 'human');
		INSERT INTO eval_judgments (question_id, event_id, grade, label_run_id, judged_content_hash)
		SELECT 1, id, 2, 1, content_hash FROM events;
		INSERT INTO eval_pool_candidates (question_id, event_id, origin) SELECT 1, id, 'cited' FROM events;
		INSERT INTO eval_runs (golden_set, retriever_tag, params, results, label_run_ids, effective_set_hash,
		                       pool_coverage, decision_grade)
		VALUES ('v1', 'tag', '{}', '{}', '[1]', 'hash', 1, false);
		INSERT INTO admin_jobs (kind, status, requested_by, stale_after_seconds, finished_at)
		VALUES ('eval_retrieval', 'succeeded', 'operator@example.com', 120, now()),
		       ('eval_judge', 'failed', 'operator@example.com', 120, now()),
		       ('eval_triage', 'running', 'operator@example.com', 120, NULL),
		       ('reembed', 'succeeded', 'operator@example.com', 120, now());
		INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
		VALUES ('models', '{"roles": {"ask": "asker", "eval_label_judge": "grader"}, "ask_max_tool_turns": 12}',
		        '{}', 4, 'operator@example.com', now());
		UPDATE connector_settings
		   SET settings = settings || '{"models_test": {"passed": false, "by": "operator@example.com",
		           "at": "2026-08-28T17:40:00Z", "roles": [
		               {"role": "Ask", "model": "asker", "passed": true},
		               {"role": "Evaluation grading", "model": "grader", "passed": false, "error": "HTTP 401"},
		               {"role": "Embeddings", "model": "embedder", "passed": true}]}}'
		 WHERE connector = 'org'`)
	require.NoError(t, err)

	store := promptcatalog.NewStore(pool)
	seeded, err := store.EnsureSeed(ctx, catalogWithEvaluationPrompts(t), "seed")
	require.NoError(t, err)

	_, err = Apply(ctx, dbURL, nil)
	require.NoError(t, err)

	var left []string
	rows, err := pool.Query(ctx, `
		SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname LIKE 'eval\_%'
		UNION ALL
		SELECT p.proname::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'public' AND p.proname LIKE 'eval\_%'
		UNION ALL
		SELECT attname::text FROM pg_attribute
		 WHERE attrelid = 'events'::regclass AND attname = 'eval_content_hash' AND NOT attisdropped`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		left = append(left, name)
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, left, "tables, indexes, sequences, functions, and the events column")

	var events int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&events))
	assert.Equal(t, 1, events)

	var kinds []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT array_agg(kind ORDER BY kind) FROM admin_jobs`).Scan(&kinds))
	assert.Equal(t, []string{"reembed"}, kinds)

	var models, modelsTest string
	var version int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT settings::text, version FROM connector_settings WHERE connector = 'models'`).Scan(&models, &version))
	assert.JSONEq(t, `{"roles": {"ask": "asker"}, "ask_max_tool_turns": 12}`, models)
	assert.Equal(t, int64(5), version, "a replica's cache reloads the row")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT settings ->> 'models_test' FROM connector_settings WHERE connector = 'org'`).Scan(&modelsTest))
	assert.JSONEq(t, `{"passed": true, "by": "operator@example.com", "at": "2026-08-28T17:40:00Z", "roles": [
		{"role": "Ask", "model": "asker", "passed": true},
		{"role": "Embeddings", "model": "embedder", "passed": true}]}`, modelsTest)

	// The stored catalog is immutable, so it keeps the prompts nothing reads
	// any more. It still loads, and still takes an edit.
	current, err := store.Current(ctx)
	require.NoError(t, err)
	assert.Equal(t, seeded.ID, current.ID)
	_, err = current.Bundle.Text("eval.relevance_judge")
	require.NoError(t, err)
	title, err := current.Bundle.Text(promptcatalog.AskTitle)
	require.NoError(t, err)
	require.NoError(t, store.WriteDraft(ctx, []promptcatalog.DraftChange{{
		Key: promptcatalog.AskTitle, Body: "Write a short title.", LiveSHA256: promptcatalog.BodySHA256(title),
	}}, "operator@example.com"))
	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	reviewed, err := draft.Bundle()
	require.NoError(t, err)
	edited, err := store.PublishDraft(ctx, current.ID, reviewed.SHA256, "operator@example.com", "")
	require.NoError(t, err)
	assert.True(t, edited.Changed)

	// A catalog without them replaces it.
	pushed, err := store.Create(ctx, promptcatalog.MustEmbedded(), "push:test", "operator@example.com", "")
	require.NoError(t, err)
	activated, err := store.Activate(ctx, pushed.ID, edited.Release.ID, "operator@example.com")
	require.NoError(t, err)
	assert.True(t, activated.Changed)
	_, err = activated.Release.Bundle.Text("eval.relevance_judge")
	assert.Error(t, err)
}

// TestRetrievalEvaluationDropLeavesOtherSettings: the models row and the
// last models test of an install that never chose or tested the removed role
// are untouched, and a test of that role alone is forgotten.
func TestRetrievalEvaluationDropLeavesOtherSettings(t *testing.T) {
	ctx := t.Context()
	const modelsTest = `{"passed": false, "by": "operator@example.com", "at": "2026-08-28T17:40:00Z",
		"roles": [{"role": "Ask", "model": "asker", "passed": false, "error": "HTTP 401"}]}`
	for name, tc := range map[string]struct{ before, after string }{
		"never tested the role": {modelsTest, modelsTest},
		"tested only the role": {`{"passed": false, "by": "operator@example.com", "at": "2026-08-28T17:40:00Z",
			"roles": [{"role": "Evaluation grading", "model": "grader", "passed": false, "error": "HTTP 401"}]}`, ""},
		"a test with no roles": {`{"passed": false, "by": "operator@example.com", "at": "2026-08-28T17:40:00Z", "roles": null}`,
			`{"passed": false, "by": "operator@example.com", "at": "2026-08-28T17:40:00Z", "roles": null}`},
		"never tested": {"", ""},
	} {
		dbURL := emptyDatabase(t)
		applyDir(t, dbURL, migrationsThrough(t, askSessionLeasesVersion))
		pool, err := pgxpool.New(ctx, dbURL)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		_, err = pool.Exec(ctx, `
			INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
			VALUES ('models', '{"roles": {"ask": "asker"}}', '{}', 4, 'operator@example.com', now())`)
		require.NoError(t, err)
		if tc.before != "" {
			_, err = pool.Exec(ctx, `
				UPDATE connector_settings SET settings = jsonb_set(settings, '{models_test}', $1::jsonb)
				 WHERE connector = 'org'`, tc.before)
			require.NoError(t, err)
		}

		_, err = Apply(ctx, dbURL, nil)
		require.NoError(t, err)

		var models string
		var modelsVersion, orgVersion int64
		var after *string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT settings::text, version FROM connector_settings WHERE connector = 'models'`).Scan(&models, &modelsVersion))
		assert.JSONEq(t, `{"roles": {"ask": "asker"}}`, models, name)
		assert.Equal(t, int64(4), modelsVersion, name)
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT settings ->> 'models_test', version FROM connector_settings WHERE connector = 'org'`).Scan(&after, &orgVersion))
		if tc.after == "" {
			assert.Nil(t, after, name)
		} else if assert.NotNil(t, after, name) {
			assert.JSONEq(t, tc.after, *after, name)
		}
		assert.Equal(t, tc.before != tc.after, orgVersion != 1, name)
	}
}

// catalogWithEvaluationPrompts is the example catalog as it was while it
// still listed the evaluation prompts.
func catalogWithEvaluationPrompts(t *testing.T) promptcatalog.Bundle {
	t.Helper()
	files := fstest.MapFS{}
	require.NoError(t, fs.WalkDir(prompts.FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(prompts.FS, path)
		files[path] = &fstest.MapFile{Data: data}
		return err
	}))
	files["manifest.yaml"] = &fstest.MapFile{Data: append(files["manifest.yaml"].Data, []byte(`  - key: eval.relevance_judge
    path: eval/relevance-judge.md
    description: Retrieval relevance grading rubric.
  - key: eval.question_triage
    path: eval/question-triage.md
    description: Golden-set question acceptance rubric.
`)...)}
	files["eval/relevance-judge.md"] = &fstest.MapFile{Data: []byte("Grade the document from 0 to 3.\n")}
	files["eval/question-triage.md"] = &fstest.MapFile{Data: []byte("Approve or exclude the question.\n")}
	bundle, err := promptcatalog.Load(files)
	require.NoError(t, err)
	return bundle
}
