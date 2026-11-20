package sessiontitle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingAgentTitleGenerator struct {
	started chan struct{}
	release chan struct{}
}

func (g *blockingAgentTitleGenerator) Generate(ctx context.Context, _ string) (Result, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return Result{Title: "Generated session title", ModelID: "cheap"}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func seedAgentTitleCandidate(t *testing.T, failures int, digest, narrative string) *pgxpool.Pool {
	t.Helper()
	pool := titleTestPool(t)
	_, err := pool.Exec(t.Context(), `TRUNCATE session_ingest_summaries CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `
		INSERT INTO session_ingest_summaries
		       (owner, client, session_id, revision, input_digest, narrative,
		        tags, title_failure_count)
		VALUES ('owner@example.com', 'codex', 'ses_agent', 'sha256:revision', $1, $2, '{}', $3)
	`, digest, narrative, failures)
	require.NoError(t, err)
	return pool
}

func TestAgentSessionWorkerGeneratesDigestBoundTitle(t *testing.T) {
	pool := seedAgentTitleCandidate(t, 0, "sha256:input", "The session diagnosed and repaired the deploy controller race.")
	gen := &fakeGenerator{result: Result{Title: "Repair deploy controller race", ModelID: "cheap"}}
	worker := &AgentSessionWorker{Pool: pool, Generator: gen, BatchSize: 1}
	require.NoError(t, worker.Tick(t.Context()))
	assert.Contains(t, gen.input, "deploy controller race")
	var title, model, titleDigest string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT title, title_model, title_input_digest
		  FROM session_ingest_summaries
		 WHERE owner='owner@example.com' AND client='codex' AND session_id='ses_agent'
	`).Scan(&title, &model, &titleDigest))
	assert.Equal(t, "Repair deploy controller race", title)
	assert.Equal(t, "cheap", model)
	assert.Equal(t, "sha256:input", titleDigest)
}

func TestAgentSessionWorkerBackfillsPreDigestSummary(t *testing.T) {
	pool := seedAgentTitleCandidate(t, 0, "", "The session repaired historical title generation.")
	gen := &fakeGenerator{result: Result{Title: "Repair historical title generation", ModelID: "cheap"}}
	worker := &AgentSessionWorker{Pool: pool, Generator: gen, BatchSize: 1}
	require.NoError(t, worker.Tick(t.Context()))

	var title, titleDigest string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT title, title_input_digest
		  FROM session_ingest_summaries
		 WHERE owner='owner@example.com' AND client='codex' AND session_id='ses_agent'
	`).Scan(&title, &titleDigest))
	assert.Equal(t, "Repair historical title generation", title)
	assert.Empty(t, titleDigest)
}

func TestAgentSessionWorkerDoesNotAttachTitleToChangedNarrative(t *testing.T) {
	pool := seedAgentTitleCandidate(t, 0, "", "Original summary")
	gen := &blockingAgentTitleGenerator{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := &AgentSessionWorker{Pool: pool, Generator: gen, BatchSize: 1, Concurrency: 1}
	done := make(chan error, 1)
	go func() { done <- worker.Tick(t.Context()) }()

	select {
	case <-gen.started:
	case <-time.After(2 * time.Second):
		t.Fatal("title generation did not start")
	}
	_, err := pool.Exec(t.Context(), `
		UPDATE session_ingest_summaries SET narrative='Replacement summary'
		 WHERE owner='owner@example.com' AND client='codex' AND session_id='ses_agent'
	`)
	require.NoError(t, err)
	close(gen.release)
	require.NoError(t, <-done)

	var title *string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT title FROM session_ingest_summaries
		 WHERE owner='owner@example.com' AND client='codex' AND session_id='ses_agent'
	`).Scan(&title))
	assert.Nil(t, title)
}

func TestAgentSessionWorkerFallsBackAfterFinalFailure(t *testing.T) {
	pool := seedAgentTitleCandidate(t, maxTitleAttempts-1, "sha256:input", "Diagnose the channel filter regression")
	worker := &AgentSessionWorker{
		Pool: pool, Generator: &fakeGenerator{err: errors.New("gateway unavailable")}, BatchSize: 1,
	}
	require.NoError(t, worker.Tick(t.Context()))
	var title, model, titleDigest string
	var failures int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT title, title_model, title_input_digest, title_failure_count
		  FROM session_ingest_summaries
		 WHERE owner='owner@example.com' AND client='codex' AND session_id='ses_agent'
	`).Scan(&title, &model, &titleDigest, &failures))
	assert.Equal(t, "Diagnose the channel filter regression", title)
	assert.Equal(t, "deterministic:fallback", model)
	assert.Equal(t, "sha256:input", titleDigest)
	assert.Equal(t, maxTitleAttempts, failures)
}

func TestAgentSessionWorkerPrioritizesNewestVisibleSession(t *testing.T) {
	pool := titleTestPool(t)
	_, err := pool.Exec(t.Context(), `
		TRUNCATE session_ingest_summaries, session_ingest_candidates CASCADE;
		INSERT INTO session_ingest_candidates
		       (owner, client, session_id, change_seq, owner_canonical,
		        source_started_at, source_updated_at)
		VALUES ('owner@example.com', 'codex', 'older-visible', 10, 'owner@example.com',
		        now() - interval '2 days', now() - interval '2 days'),
		       ('owner@example.com', 'codex', 'newer-visible', 20, 'owner@example.com',
		        now(), now());
		INSERT INTO session_ingest_summaries
		       (owner, client, session_id, revision, input_digest, narrative, tags, summarized_at)
		VALUES ('owner@example.com', 'codex', 'older-visible', 'sha256:old-revision',
		        'sha256:old-input', 'Older visible session', '{}', now()),
		       ('owner@example.com', 'codex', 'newer-visible', 'sha256:new-revision',
		        'sha256:new-input', 'Newer visible session', '{}', now() - interval '1 hour')
	`)
	require.NoError(t, err)
	gen := &fakeGenerator{result: Result{Title: "Newest title", ModelID: "cheap"}}
	worker := &AgentSessionWorker{Pool: pool, Generator: gen, BatchSize: 1, Concurrency: 1}
	require.NoError(t, worker.Tick(t.Context()))
	assert.Equal(t, "Newer visible session", gen.input)
}

func TestAgentSessionWorkerBoundsConcurrentGeneration(t *testing.T) {
	pool := titleTestPool(t)
	_, err := pool.Exec(t.Context(), `TRUNCATE session_ingest_summaries CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `
		INSERT INTO session_ingest_summaries
		       (owner, client, session_id, revision, input_digest, narrative, tags)
		SELECT 'owner@example.com', 'codex', 'ses_parallel_' || n,
		       'sha256:revision-' || n, 'sha256:input-' || n, 'Narrative ' || n, '{}'
		  FROM generate_series(1, 3) AS n
	`)
	require.NoError(t, err)

	gen := &blockingAgentTitleGenerator{started: make(chan struct{}, 3), release: make(chan struct{})}
	worker := &AgentSessionWorker{Pool: pool, Generator: gen, BatchSize: 3, Concurrency: 3}
	done := make(chan error, 1)
	go func() { done <- worker.Tick(t.Context()) }()
	for range 3 {
		select {
		case <-gen.started:
		case <-time.After(2 * time.Second):
			t.Fatal("title generation did not reach configured concurrency")
		}
	}
	close(gen.release)
	require.NoError(t, <-done)

	var titled int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*) FROM session_ingest_summaries
		 WHERE title_input_digest = input_digest
	`).Scan(&titled))
	assert.Equal(t, 3, titled)
}
