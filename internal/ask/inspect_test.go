package ask

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Inspect reads a session and its whole persisted history, as stored.
func (m *Manager) Inspect(ctx context.Context, sessionID uuid.UUID) (*Session, []StoredMessage, error) {
	s, messages, _, err := m.InspectWindow(ctx, sessionID, nil, nil)
	return s, messages, err
}

// InspectRunTelemetry returns all traces for a session, ordered by execution
// time and with each run's model/tool calls ordered by their start sequence.
func (m *Manager) InspectRunTelemetry(ctx context.Context, sessionID uuid.UUID) ([]RunTelemetry, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, user_turn, surface, model_id, COALESCE(reasoning_effort,''),
		       COALESCE(service_tier,''), started_at, completed_at, duration_ms,
		       outcome, COALESCE(error,'')
		  FROM ask_runs WHERE session_id=$1 ORDER BY started_at,id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ask telemetry runs query: %w", err)
	}
	defer rows.Close()
	var runs []RunTelemetry
	for rows.Next() {
		run := RunTelemetry{SessionID: sessionID, InferenceCalls: []InferenceCall{}, ToolCalls: []ToolCallTrace{}}
		if err := rows.Scan(&run.ID, &run.UserTurn, &run.Surface, &run.ModelID,
			&run.ReasoningEffort, &run.ServiceTier, &run.StartedAt, &run.CompletedAt,
			&run.DurationMS, &run.Outcome, &run.Error); err != nil {
			return nil, fmt.Errorf("ask telemetry scan run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ask telemetry runs iter: %w", err)
	}

	byID := make(map[uuid.UUID]*RunTelemetry, len(runs))
	for i := range runs {
		byID[runs[i].ID] = &runs[i]
	}
	if len(runs) == 0 {
		return runs, nil
	}

	inferenceRows, err := m.pool.Query(ctx, `
		SELECT c.run_id,c.sequence,c.workload,c.model_id,COALESCE(c.requested_effort,''),
		       COALESCE(c.requested_service_tier,''),COALESCE(c.actual_service_tier,''),
		       COALESCE(c.response_id,''),c.started_at,c.completed_at,c.duration_ms,
		       c.input_tokens,c.output_tokens,c.cache_read_tokens,c.reasoning_tokens,
		       c.outcome,COALESCE(c.error,'')
		  FROM ask_inference_calls c JOIN ask_runs r ON r.id=c.run_id
		 WHERE r.session_id=$1 ORDER BY r.started_at,c.sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ask telemetry inference query: %w", err)
	}
	for inferenceRows.Next() {
		var runID uuid.UUID
		var call InferenceCall
		if err := inferenceRows.Scan(&runID, &call.Sequence, &call.Workload, &call.ModelID,
			&call.RequestedEffort, &call.RequestedServiceTier, &call.ActualServiceTier,
			&call.ResponseID, &call.StartedAt, &call.CompletedAt, &call.DurationMS,
			&call.InputTokens, &call.OutputTokens, &call.CacheReadTokens,
			&call.ReasoningTokens, &call.Outcome, &call.Error); err != nil {
			inferenceRows.Close()
			return nil, fmt.Errorf("ask telemetry scan inference: %w", err)
		}
		if run := byID[runID]; run != nil {
			run.InferenceCalls = append(run.InferenceCalls, call)
		}
	}
	if err := inferenceRows.Err(); err != nil {
		inferenceRows.Close()
		return nil, fmt.Errorf("ask telemetry inference iter: %w", err)
	}
	inferenceRows.Close()

	toolRows, err := m.pool.Query(ctx, `
		SELECT c.run_id,c.sequence,c.workload,COALESCE(c.call_id,''),c.tool_name,
		       c.started_at,c.completed_at,c.duration_ms,c.outcome,COALESCE(c.error,'')
		  FROM ask_tool_calls c JOIN ask_runs r ON r.id=c.run_id
		 WHERE r.session_id=$1 ORDER BY r.started_at,c.sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ask telemetry tool query: %w", err)
	}
	defer toolRows.Close()
	for toolRows.Next() {
		var runID uuid.UUID
		var call ToolCallTrace
		if err := toolRows.Scan(&runID, &call.Sequence, &call.Workload, &call.CallID,
			&call.ToolName, &call.StartedAt, &call.CompletedAt, &call.DurationMS,
			&call.Outcome, &call.Error); err != nil {
			return nil, fmt.Errorf("ask telemetry scan tool: %w", err)
		}
		if run := byID[runID]; run != nil {
			run.ToolCalls = append(run.ToolCalls, call)
		}
	}
	if err := toolRows.Err(); err != nil {
		return nil, fmt.Errorf("ask telemetry tool iter: %w", err)
	}
	return runs, nil
}
