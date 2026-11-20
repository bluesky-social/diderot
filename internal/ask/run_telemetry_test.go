package ask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openaischema "github.com/cloudwego/eino/schema/openai"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRunTelemetryRecorderCapturesModelUsageAndTier(t *testing.T) {
	recorder := newRunTelemetryRecorder(uuid.New(), AskRunSurfaceHTTP, "gpt-5.6-sol:direct", EffectiveModelSettings{
		ReasoningEffort: "medium",
		ServiceTier:     "fast",
	})
	ctx := withTelemetryWorkload(context.Background(), TelemetryWorkloadRoot)
	span := recorder.start(ctx)
	recorder.finishModel(ctx, span, &model.AgenticCallbackOutput{
		Message: &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{
			OpenAIExtension: &openaischema.ResponseMetaExtension{
				ID: "resp_123", ServiceTier: openaischema.ServiceTierPriority,
			},
		}},
		TokenUsage: &model.TokenUsage{
			PromptTokens: 100, CompletionTokens: 20,
			PromptTokenDetails:      model.PromptTokenDetails{CachedTokens: 60},
			CompletionTokensDetails: model.CompletionTokensDetails{ReasoningTokens: 12},
		},
	}, nil)

	run := recorder.Complete("answered", nil)
	require.Len(t, run.InferenceCalls, 1)
	call := run.InferenceCalls[0]
	require.Equal(t, "resp_123", call.ResponseID)
	require.Equal(t, "priority", call.ActualServiceTier)
	require.EqualValues(t, 100, *call.InputTokens)
	require.EqualValues(t, 20, *call.OutputTokens)
	require.EqualValues(t, 60, *call.CacheReadTokens)
	require.EqualValues(t, 12, *call.ReasoningTokens)
}

func TestRunTelemetryRecorderDeduplicatesNestedModelCallbacks(t *testing.T) {
	recorder := newRunTelemetryRecorder(uuid.New(), AskRunSurfaceHTTP, "gpt-5.6-sol:direct", EffectiveModelSettings{})
	ctx := withTelemetryWorkload(context.Background(), TelemetryWorkloadRoot)
	outerOutput := &model.AgenticCallbackOutput{
		Message: &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{
			OpenAIExtension: &openaischema.ResponseMetaExtension{ID: "resp_same", ServiceTier: openaischema.ServiceTierPriority},
		}},
		TokenUsage: &model.TokenUsage{PromptTokens: 100, CompletionTokens: 20},
	}
	innerOutput := &model.AgenticCallbackOutput{
		Message: &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{
			OpenAIExtension: &openaischema.ResponseMetaExtension{ID: "resp_same"},
		}},
	}
	outer := telemetrySpan{sequence: 0, started: time.Now().Add(-time.Second)}
	inner := telemetrySpan{sequence: 1, started: time.Now().Add(-500 * time.Millisecond)}
	recorder.finishModel(ctx, outer, outerOutput, nil)
	recorder.finishModel(ctx, inner, innerOutput, nil)

	run := recorder.Complete("answered", nil)
	require.Len(t, run.InferenceCalls, 1)
	require.Equal(t, "resp_same", run.InferenceCalls[0].ResponseID)
	require.Equal(t, 0, run.InferenceCalls[0].Sequence)
	require.Less(t, run.InferenceCalls[0].DurationMS, int64(750))
	require.Equal(t, "priority", run.InferenceCalls[0].ActualServiceTier)
	require.EqualValues(t, 100, *run.InferenceCalls[0].InputTokens)
	require.EqualValues(t, 20, *run.InferenceCalls[0].OutputTokens)
}

func TestRunTelemetryStoreRoundTrip(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	session, err := mgr.Create(context.Background())
	require.NoError(t, err)

	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	turn := 2
	in, out, cached, reasoning := int64(100), int64(20), int64(60), int64(12)
	run := RunTelemetry{
		ID: uuid.New(), SessionID: session.ID, UserTurn: &turn,
		Surface: AskRunSurfaceHTTP, ModelID: "gpt-5.6-sol:direct",
		ReasoningEffort: "medium", ServiceTier: "fast", StartedAt: started,
		CompletedAt: started.Add(3 * time.Second), DurationMS: 3000, Outcome: "answered",
		InferenceCalls: []InferenceCall{{
			Sequence: 1, Workload: TelemetryWorkloadObservabilityChild,
			ModelID: "gpt-5.6-sol:direct", RequestedEffort: "medium",
			RequestedServiceTier: "fast", ActualServiceTier: "priority", ResponseID: "resp_child",
			StartedAt: started.Add(time.Second), CompletedAt: started.Add(2 * time.Second),
			DurationMS: 1000, InputTokens: &in, OutputTokens: &out,
			CacheReadTokens: &cached, ReasoningTokens: &reasoning, Outcome: "ok",
		}},
		ToolCalls: []ToolCallTrace{{
			Sequence: 0, Workload: TelemetryWorkloadRoot, CallID: "call_1", ToolName: "retrieve",
			StartedAt: started, CompletedAt: started.Add(time.Second), DurationMS: 1000,
			Outcome: "error", Error: "recoverable tool error",
		}},
	}
	require.NoError(t, mgr.RecordRunTelemetry(context.Background(), run))

	got, err := mgr.InspectRunTelemetry(context.Background(), session.ID)
	require.NoError(t, err)
	for i := range got {
		got[i].StartedAt = got[i].StartedAt.UTC()
		got[i].CompletedAt = got[i].CompletedAt.UTC()
		for j := range got[i].InferenceCalls {
			got[i].InferenceCalls[j].StartedAt = got[i].InferenceCalls[j].StartedAt.UTC()
			got[i].InferenceCalls[j].CompletedAt = got[i].InferenceCalls[j].CompletedAt.UTC()
		}
		for j := range got[i].ToolCalls {
			got[i].ToolCalls[j].StartedAt = got[i].ToolCalls[j].StartedAt.UTC()
			got[i].ToolCalls[j].CompletedAt = got[i].ToolCalls[j].CompletedAt.UTC()
		}
	}
	require.Equal(t, []RunTelemetry{run}, got)
}

func TestBoundedTelemetryError(t *testing.T) {
	got := boundedTelemetryError(errors.New(string(make([]byte, telemetryErrorLimit+10))))
	require.Len(t, got, telemetryErrorLimit)
}
