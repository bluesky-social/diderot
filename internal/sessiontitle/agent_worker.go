package sessiontitle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

const maxTitleAttempts = 5

// AgentSessionWorker generates display titles for
// Diderot's persisted agent-session summaries. Titles are bound to the
// summary input digest. Pre-digest summaries use the stored empty digest and
// also compare the exact narrative on write, so an in-flight generation
// cannot attach an old title to changed summary text.
type AgentSessionWorker struct {
	Pool        *pgxpool.Pool
	Generator   Generator
	BatchSize   int
	Concurrency int
	Logger      *slog.Logger
}

type agentSessionCandidate struct {
	Owner       string
	Client      string
	SessionID   string
	InputDigest string
	Input       string
	Failures    int
}

func (w *AgentSessionWorker) Tick(ctx context.Context) error {
	if w.Pool == nil || w.Generator == nil {
		return fmt.Errorf("sessiontitle: pool and generator are required")
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	batch := w.BatchSize
	if batch <= 0 || batch > 100 {
		batch = 10
	}
	concurrency := w.Concurrency
	if concurrency <= 0 || concurrency > 10 {
		concurrency = 4
	}
	rows, err := w.Pool.Query(ctx, `
		WITH candidates AS (
			SELECT summary.owner, summary.client, summary.session_id
			  FROM session_ingest_summaries summary
			  LEFT JOIN session_ingest_candidates candidate
			    ON candidate.owner = summary.owner
			   AND candidate.client = summary.client
			   AND candidate.session_id = summary.session_id
			 WHERE (summary.title IS NULL OR summary.title_input_digest IS DISTINCT FROM summary.input_digest)
			   AND summary.title_failure_count < $2
			   AND (summary.title_attempted_at IS NULL OR summary.title_attempted_at < now() - interval '5 minutes')
			 ORDER BY summary.title_attempted_at NULLS FIRST,
			          candidate.source_updated_at DESC NULLS LAST, summary.summarized_at DESC
			 FOR UPDATE OF summary SKIP LOCKED
			 LIMIT $1
		), claimed AS (
			UPDATE session_ingest_summaries summary SET title_attempted_at = now()
			  FROM candidates candidate
			 WHERE summary.owner = candidate.owner
			   AND summary.client = candidate.client
			   AND summary.session_id = candidate.session_id
			 RETURNING summary.owner, summary.client, summary.session_id,
			           summary.input_digest, summary.narrative, summary.title_failure_count
		)
		SELECT owner, client, session_id, input_digest, narrative, title_failure_count
		  FROM claimed`, batch, maxTitleAttempts)
	if err != nil {
		return fmt.Errorf("sessiontitle: claim agent sessions: %w", err)
	}
	var candidates []agentSessionCandidate
	for rows.Next() {
		var candidate agentSessionCandidate
		if err := rows.Scan(&candidate.Owner, &candidate.Client, &candidate.SessionID,
			&candidate.InputDigest, &candidate.Input, &candidate.Failures); err != nil {
			rows.Close()
			return fmt.Errorf("sessiontitle: scan agent session claim: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sessiontitle: agent session claims: %w", err)
	}

	var group errgroup.Group
	group.SetLimit(concurrency)
	for _, candidate := range candidates {
		candidate := candidate
		group.Go(func() error {
			return w.processCandidate(ctx, candidate)
		})
	}
	return group.Wait()
}

func (w *AgentSessionWorker) processCandidate(ctx context.Context, candidate agentSessionCandidate) error {
	result, generateErr := GenerateFromSummary(ctx, w.Generator, candidate.Input)
	if generateErr == nil {
		if _, err := w.Pool.Exec(ctx, `
				UPDATE session_ingest_summaries
				   SET title=$6, title_model=$7, title_input_digest=$4,
				       title_generated_at=now(), title_failure_count=0, title_last_error=NULL
				 WHERE owner=$1 AND client=$2 AND session_id=$3
				   AND input_digest=$4 AND narrative=$5
			`, candidate.Owner, candidate.Client, candidate.SessionID,
			candidate.InputDigest, candidate.Input, result.Title, result.ModelID); err != nil {
			return fmt.Errorf("sessiontitle: save agent session %s/%s: %w", candidate.Client, candidate.SessionID, err)
		}
		return nil
	}

	failures := candidate.Failures + 1
	if failures >= maxTitleAttempts {
		if _, err := w.Pool.Exec(ctx, `
				UPDATE session_ingest_summaries
				   SET title=$6, title_model='deterministic:fallback', title_input_digest=$4,
				       title_generated_at=now(), title_failure_count=$7, title_last_error=$8
				 WHERE owner=$1 AND client=$2 AND session_id=$3
				   AND input_digest=$4 AND narrative=$5
			`, candidate.Owner, candidate.Client, candidate.SessionID, candidate.InputDigest, candidate.Input,
			Fallback(candidate.Input), failures, boundedError(generateErr)); err != nil {
			return fmt.Errorf("sessiontitle: save agent fallback %s/%s: %w", candidate.Client, candidate.SessionID, err)
		}
	} else if _, err := w.Pool.Exec(ctx, `
			UPDATE session_ingest_summaries
			   SET title_failure_count=$6, title_last_error=$7
			 WHERE owner=$1 AND client=$2 AND session_id=$3
			   AND input_digest=$4 AND narrative=$5
		`, candidate.Owner, candidate.Client, candidate.SessionID, candidate.InputDigest,
		candidate.Input, failures, boundedError(generateErr)); err != nil {
		return fmt.Errorf("sessiontitle: record agent failure %s/%s: %w", candidate.Client, candidate.SessionID, err)
	}
	w.Logger.Warn("agent session title generation failed", "owner", candidate.Owner,
		"client", candidate.Client, "session_id", candidate.SessionID,
		"attempt", failures, "err", generateErr)
	return nil
}
