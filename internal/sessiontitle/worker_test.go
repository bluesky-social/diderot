package sessiontitle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/testdb"
)

type fakeGenerator struct {
	result Result
	err    error
	input  string
	calls  int
}

func (generator *fakeGenerator) Generate(_ context.Context, input string) (Result, error) {
	generator.input = input
	generator.calls++
	return generator.result, generator.err
}

func titleTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.URL(t, "sessiontitle"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `TRUNCATE ask_sessions, ask_messages, ask_slack_sessions RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return pool
}

func seedTitleCandidate(t *testing.T, pool *pgxpool.Pool, question, answer string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id))
	appendTitleMessage(t, pool, id, 0, "user", question)
	if answer != "" {
		appendTitleMessage(t, pool, id, 1, "assistant", answer)
	}
	return id
}

func appendTitleMessage(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, turn int, role, text string) {
	t.Helper()
	blockType := "user_input_text"
	if role == "assistant" {
		blockType = "assistant_gen_text"
	}
	_, err := pool.Exec(t.Context(), `INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message)
		VALUES ($1, $2, $3, 1, jsonb_build_object('role',$3::text,'content_blocks',jsonb_build_array(
			jsonb_build_object('type',$4::text,$4::text,jsonb_build_object('text',$5::text)))))`, id, turn, role, blockType, text)
	require.NoError(t, err)
}

func TestWorkerGeneratesStableTitleAfterSuccessfulAnswer(t *testing.T) {
	pool := titleTestPool(t)
	id := seedTitleCandidate(t, pool, "<context>metadata</context><question>How do we deploy checkout?</question>", "Use the deployment pipeline.")
	generator := &fakeGenerator{result: Result{Title: "Deploying Checkout through the release pipeline", ModelID: "cheap"}}
	worker := &Worker{Pool: pool, Generator: generator, BatchSize: 1}
	require.NoError(t, worker.Tick(t.Context()))
	assert.Equal(t, "Question:\nHow do we deploy checkout?\n\nAnswer:\nUse the deployment pipeline.", generator.input)
	var title, model string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT title, title_model FROM ask_sessions WHERE id=$1`, id).Scan(&title, &model))
	assert.Equal(t, generator.result.Title, title)
	assert.Equal(t, "cheap", model)
	appendTitleMessage(t, pool, id, 2, "user", "What about an unrelated topic?")
	appendTitleMessage(t, pool, id, 3, "assistant", "A different answer.")
	require.NoError(t, worker.Tick(t.Context()))
	assert.Equal(t, 1, generator.calls)
}

func TestWorkerWaitsForSuccessfulAnswerAndSkipsToolRounds(t *testing.T) {
	pool := titleTestPool(t)
	id := seedTitleCandidate(t, pool, "How do we deploy checkout?", "")
	generator := &fakeGenerator{result: Result{Title: "Deploying Checkout", ModelID: "cheap"}}
	worker := &Worker{Pool: pool, Generator: generator, BatchSize: 1}
	require.NoError(t, worker.Tick(t.Context()))
	assert.Zero(t, generator.calls)
	appendTitleMessage(t, pool, id, 1, "assistant", "Incomplete response.")
	_, err := pool.Exec(t.Context(), `UPDATE ask_messages SET incomplete_reason='max_tokens' WHERE session_id=$1 AND turn=1`, id)
	require.NoError(t, err)
	appendTitleMessage(t, pool, id, 2, "assistant", "Internal tool commentary.")
	_, err = pool.Exec(t.Context(), `UPDATE ask_messages SET message=jsonb_set(message, '{content_blocks}',
		(message->'content_blocks') || '[{"type":"function_tool_call","function_tool_call":{"call_id":"tool-1","name":"retrieve","arguments":"{}"}}]'::jsonb)
		WHERE session_id=$1 AND turn=2`, id)
	require.NoError(t, err)
	require.NoError(t, worker.Tick(t.Context()))
	assert.Zero(t, generator.calls)
	var attemptedAt *time.Time
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT title_attempted_at FROM ask_sessions WHERE id=$1`, id).Scan(&attemptedAt))
	assert.Nil(t, attemptedAt)
	appendTitleMessage(t, pool, id, 3, "assistant", "The successful answer.")
	appendTitleMessage(t, pool, id, 4, "assistant", "A later answer.")
	require.NoError(t, worker.Tick(t.Context()))
	assert.Contains(t, generator.input, "The successful answer.")
	assert.NotContains(t, generator.input, "Incomplete response")
	assert.NotContains(t, generator.input, "Internal tool commentary")
	assert.NotContains(t, generator.input, "A later answer")
}

func TestWorkerDoesNotRetryOrCreateFallbackTitle(t *testing.T) {
	for _, generationError := range []error{errors.New("gateway unavailable"), nil} {
		pool := titleTestPool(t)
		id := seedTitleCandidate(t, pool, "Fix the channel filter", "Here is the fix.")
		generator := &fakeGenerator{err: generationError}
		worker := &Worker{Pool: pool, Generator: generator, BatchSize: 1}
		require.NoError(t, worker.Tick(t.Context()))
		generator.result = Result{Title: "Would now succeed", ModelID: "cheap"}
		generator.err = nil
		require.NoError(t, worker.Tick(t.Context()))
		assert.Equal(t, 1, generator.calls)
		var title, model *string
		var attemptedAt *time.Time
		var lastError string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT title,title_model,title_attempted_at,title_last_error FROM ask_sessions WHERE id=$1`, id).
			Scan(&title, &model, &attemptedAt, &lastError))
		assert.Nil(t, title)
		assert.Nil(t, model)
		assert.NotNil(t, attemptedAt)
		assert.NotEmpty(t, lastError)
	}
}

func TestWorkerBoundsQuestionAndAnswerExcerpts(t *testing.T) {
	pool := titleTestPool(t)
	seedTitleCandidate(t, pool, strings.Repeat("界", maxInputBytes), strings.Repeat("答", maxInputBytes))
	generator := &fakeGenerator{result: Result{Title: "A concise title", ModelID: "cheap"}}
	require.NoError(t, (&Worker{Pool: pool, Generator: generator}).Tick(t.Context()))
	assert.LessOrEqual(t, len(generator.input), maxInputBytes)
	assert.True(t, utf8.ValidString(generator.input))
	assert.Contains(t, generator.input, "Question:\n界")
	assert.Contains(t, generator.input, "Answer:\n答")
}

func TestWorkerPreservesExistingTitlesAndClaims(t *testing.T) {
	pool := titleTestPool(t)
	titledID := seedTitleCandidate(t, pool, "First question", "First answer")
	claimedID := seedTitleCandidate(t, pool, "Other question", "Other answer")
	_, err := pool.Exec(t.Context(), `UPDATE ask_sessions SET title='Existing title' WHERE id=$1`, titledID)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `UPDATE ask_sessions SET title_attempted_at=now()-interval '1 day' WHERE id=$1`, claimedID)
	require.NoError(t, err)
	generator := &fakeGenerator{}
	require.NoError(t, (&Worker{Pool: pool, Generator: generator}).Tick(t.Context()))
	assert.Zero(t, generator.calls)
}

func TestWorkerWaitsForAskTitlePromptBeforeClaiming(t *testing.T) {
	pool := titleTestPool(t)
	id := seedTitleCandidate(t, pool, "What owns this title?", "This Ask conversation.")
	previous := promptcatalog.Active()
	t.Cleanup(func() { require.NoError(t, promptcatalog.SetActive(previous)) })
	files := fstest.MapFS{}
	manifest := "schema_version: 1\nprompts:\n"
	for _, prompt := range promptcatalog.MustEmbedded().Prompts {
		if prompt.Key == promptcatalog.AskTitle {
			continue
		}
		manifest += fmt.Sprintf("  - key: %s\n    path: %s\n", prompt.Key, prompt.Path)
		files[prompt.Path] = &fstest.MapFile{Data: []byte(prompt.Body)}
	}
	files["manifest.yaml"] = &fstest.MapFile{Data: []byte(manifest)}
	historical, err := promptcatalog.Load(files)
	require.NoError(t, err)
	require.NoError(t, promptcatalog.SetActive(historical))
	generator := &fakeGenerator{result: Result{Title: "Independently owned Ask conversation titles", ModelID: "cheap"}}
	worker := &Worker{Pool: pool, Generator: generator}
	require.ErrorContains(t, worker.Tick(t.Context()), "Ask title prompt unavailable")
	assert.Zero(t, generator.calls)
	var attemptedAt *time.Time
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT title_attempted_at FROM ask_sessions WHERE id=$1`, id).Scan(&attemptedAt))
	assert.Nil(t, attemptedAt)
	require.NoError(t, promptcatalog.SetActive(promptcatalog.MustEmbedded()))
	require.NoError(t, worker.Tick(t.Context()))
	assert.Equal(t, 1, generator.calls)
}
