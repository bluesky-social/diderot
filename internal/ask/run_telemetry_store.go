package ask

import (
	"context"
	"fmt"
)

// RecordRunTelemetry persists one complete in-memory turn trace atomically.
// It is intentionally separate from message persistence: failed turns still
// produce diagnostic spans even when no new replayable message boundary exists.
func (m *Manager) RecordRunTelemetry(ctx context.Context, run RunTelemetry) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ask telemetry begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userTurn any
	if run.UserTurn != nil {
		userTurn = *run.UserTurn
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ask_runs
		       (id, session_id, user_turn, surface, model_id, reasoning_effort,
		        service_tier, started_at, completed_at, duration_ms, outcome, error)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,NULLIF($12,''))`,
		run.ID, run.SessionID, userTurn, string(run.Surface), run.ModelID,
		run.ReasoningEffort, run.ServiceTier, run.StartedAt, run.CompletedAt,
		run.DurationMS, run.Outcome, run.Error); err != nil {
		return fmt.Errorf("ask telemetry insert run: %w", err)
	}

	for _, call := range run.InferenceCalls {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ask_inference_calls
			       (run_id, sequence, workload, model_id, requested_effort,
			        requested_service_tier, actual_service_tier, response_id,
			        started_at, completed_at, duration_ms, input_tokens, output_tokens,
			        cache_read_tokens, reasoning_tokens, outcome, error)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),
			        $9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17,''))`,
			run.ID, call.Sequence, string(call.Workload), call.ModelID,
			call.RequestedEffort, call.RequestedServiceTier, call.ActualServiceTier,
			call.ResponseID, call.StartedAt, call.CompletedAt, call.DurationMS,
			call.InputTokens, call.OutputTokens, call.CacheReadTokens,
			call.ReasoningTokens, call.Outcome, call.Error); err != nil {
			return fmt.Errorf("ask telemetry insert inference sequence=%d: %w", call.Sequence, err)
		}
	}

	for _, call := range run.ToolCalls {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ask_tool_calls
			       (run_id, sequence, workload, call_id, tool_name, started_at,
			        completed_at, duration_ms, outcome, error)
			VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,NULLIF($10,''))`,
			run.ID, call.Sequence, string(call.Workload), call.CallID, call.ToolName,
			call.StartedAt, call.CompletedAt, call.DurationMS, call.Outcome, call.Error); err != nil {
			return fmt.Errorf("ask telemetry insert tool sequence=%d: %w", call.Sequence, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ask telemetry commit: %w", err)
	}
	return nil
}
