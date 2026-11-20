package ask

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	componenttool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

const telemetryErrorLimit = 4096

type TelemetryWorkload string

const (
	TelemetryWorkloadRoot               TelemetryWorkload = "root"
	TelemetryWorkloadObservabilityChild TelemetryWorkload = "observability_child"
)

// ModelOverrides are request-scoped Ask model controls. Nil fields preserve
// the process configuration; non-nil fields replace it for this turn and all
// child model calls spawned by the turn.
type ModelOverrides struct {
	ReasoningEffort         *string
	ServiceTier             *string
	ReasoningEffortSchedule *ReasoningEffortSchedule
}

// ReasoningEffortSchedule changes the effort used by root model calls after
// the configured number of completed tool rounds. It is intentionally scoped
// to one Ask turn and does not affect observability-child model calls.
type ReasoningEffortSchedule struct {
	AfterToolRounds int    `json:"after_tool_rounds"`
	Effort          string `json:"effort"`
}

type EffectiveModelSettings struct {
	ReasoningEffort string
	ServiceTier     string
}

type RunTelemetry struct {
	ID              uuid.UUID       `json:"id"`
	SessionID       uuid.UUID       `json:"session_id"`
	UserTurn        *int            `json:"user_turn,omitempty"`
	Surface         AskRunSurface   `json:"surface"`
	ModelID         string          `json:"model_id"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	StartedAt       time.Time       `json:"started_at"`
	CompletedAt     time.Time       `json:"completed_at"`
	DurationMS      int64           `json:"duration_ms"`
	Outcome         string          `json:"outcome"`
	Error           string          `json:"error,omitempty"`
	InferenceCalls  []InferenceCall `json:"inference_calls"`
	ToolCalls       []ToolCallTrace `json:"tool_calls"`
}

type InferenceCall struct {
	Sequence             int               `json:"sequence"`
	Workload             TelemetryWorkload `json:"workload"`
	ModelID              string            `json:"model_id"`
	RequestedEffort      string            `json:"requested_effort,omitempty"`
	RequestedServiceTier string            `json:"requested_service_tier,omitempty"`
	ActualServiceTier    string            `json:"actual_service_tier,omitempty"`
	ResponseID           string            `json:"response_id,omitempty"`
	StartedAt            time.Time         `json:"started_at"`
	CompletedAt          time.Time         `json:"completed_at"`
	DurationMS           int64             `json:"duration_ms"`
	InputTokens          *int64            `json:"input_tokens,omitempty"`
	OutputTokens         *int64            `json:"output_tokens,omitempty"`
	CacheReadTokens      *int64            `json:"cache_read_tokens,omitempty"`
	ReasoningTokens      *int64            `json:"reasoning_tokens,omitempty"`
	Outcome              string            `json:"outcome"`
	Error                string            `json:"error,omitempty"`
}

type ToolCallTrace struct {
	Sequence    int               `json:"sequence"`
	Workload    TelemetryWorkload `json:"workload"`
	CallID      string            `json:"call_id,omitempty"`
	ToolName    string            `json:"tool_name"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt time.Time         `json:"completed_at"`
	DurationMS  int64             `json:"duration_ms"`
	Outcome     string            `json:"outcome"`
	Error       string            `json:"error,omitempty"`
}

type telemetryWorkloadContextKey struct{}
type telemetrySpanContextKey struct{}

type telemetrySpan struct {
	sequence        int
	started         time.Time
	requestedEffort string
	requestedTier   string
}

type RunTelemetryRecorder struct {
	mu             sync.Mutex
	run            RunTelemetry
	next           int
	modelResponses map[string]int
	finished       bool
}

func newRunTelemetryRecorder(sessionID uuid.UUID, surface AskRunSurface, modelID string, settings EffectiveModelSettings) *RunTelemetryRecorder {
	started := time.Now()
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		modelID = "unknown"
	}
	return &RunTelemetryRecorder{run: RunTelemetry{
		ID: uuid.New(), SessionID: sessionID, Surface: surface, ModelID: modelID,
		ReasoningEffort: settings.ReasoningEffort, ServiceTier: settings.ServiceTier,
		StartedAt: started, InferenceCalls: []InferenceCall{}, ToolCalls: []ToolCallTrace{},
	}, modelResponses: make(map[string]int)}
}

func withTelemetryWorkload(ctx context.Context, workload TelemetryWorkload) context.Context {
	return context.WithValue(ctx, telemetryWorkloadContextKey{}, workload)
}

func telemetryWorkloadFromContext(ctx context.Context) TelemetryWorkload {
	workload, _ := ctx.Value(telemetryWorkloadContextKey{}).(TelemetryWorkload)
	if workload == "" {
		return TelemetryWorkloadRoot
	}
	return workload
}

func (r *RunTelemetryRecorder) SetUserTurn(turn int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := turn
	r.run.UserTurn = &value
}

func (r *RunTelemetryRecorder) start(ctx context.Context) telemetrySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	span := telemetrySpan{
		sequence:        r.next,
		started:         time.Now(),
		requestedEffort: reasoningEffortForCall(ctx, r.run.ReasoningEffort),
		requestedTier:   r.run.ServiceTier,
	}
	r.next++
	return span
}

func (r *RunTelemetryRecorder) finishModel(ctx context.Context, span telemetrySpan, output callbacks.CallbackOutput, callErr error) {
	completed := time.Now()
	call := InferenceCall{
		Sequence: span.sequence, Workload: telemetryWorkloadFromContext(ctx),
		ModelID: r.run.ModelID, RequestedEffort: span.requestedEffort,
		RequestedServiceTier: span.requestedTier, StartedAt: span.started,
		CompletedAt: completed, DurationMS: completed.Sub(span.started).Milliseconds(), Outcome: "ok",
	}
	if callErr != nil {
		call.Outcome = "error"
		call.Error = boundedTelemetryError(callErr)
	} else if out := model.ConvAgenticCallbackOutput(output); out != nil && out.Message != nil {
		usage := out.TokenUsage
		if usage == nil && out.Message.ResponseMeta != nil {
			usage = agenticModelUsage(out.Message.ResponseMeta.TokenUsage)
		}
		if usage != nil {
			call.InputTokens = int64ptr(usage.PromptTokens)
			call.OutputTokens = int64ptr(usage.CompletionTokens)
			call.CacheReadTokens = int64ptr(usage.PromptTokenDetails.CachedTokens)
			call.ReasoningTokens = int64ptr(usage.CompletionTokensDetails.ReasoningTokens)
		}
		if meta := out.Message.ResponseMeta; meta != nil && meta.OpenAIExtension != nil {
			call.ResponseID = meta.OpenAIExtension.ID
			call.ActualServiceTier = string(meta.OpenAIExtension.ServiceTier)
		}
	}
	r.mu.Lock()
	if call.ResponseID != "" {
		if index, exists := r.modelResponses[call.ResponseID]; exists {
			// Eino may emit the same successful model result at both the
			// component and enclosing graph-node callback layers. The gateway
			// response ID identifies the single billable request. Keep the
			// tighter span while retaining its original sequence slot.
			existing := r.run.InferenceCalls[index]
			kept, other := existing, call
			if call.DurationMS < existing.DurationMS {
				kept, other = call, existing
				kept.Sequence = existing.Sequence
			}
			mergeInferenceMetadata(&kept, other)
			r.run.InferenceCalls[index] = kept
			r.mu.Unlock()
			return
		}
		r.modelResponses[call.ResponseID] = len(r.run.InferenceCalls)
	}
	r.run.InferenceCalls = append(r.run.InferenceCalls, call)
	r.mu.Unlock()
}

func mergeInferenceMetadata(dst *InferenceCall, src InferenceCall) {
	if dst.InputTokens == nil {
		dst.InputTokens = src.InputTokens
	}
	if dst.OutputTokens == nil {
		dst.OutputTokens = src.OutputTokens
	}
	if dst.CacheReadTokens == nil {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if dst.ReasoningTokens == nil {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if dst.ActualServiceTier == "" {
		dst.ActualServiceTier = src.ActualServiceTier
	}
}

func agenticModelUsage(usage *schema.TokenUsage) *model.TokenUsage {
	if usage == nil {
		return nil
	}
	return &model.TokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		PromptTokenDetails: model.PromptTokenDetails{
			CachedTokens: usage.PromptTokenDetails.CachedTokens,
		},
		CompletionTokensDetails: model.CompletionTokensDetails{
			ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens,
		},
	}
}

func (r *RunTelemetryRecorder) finishTool(ctx context.Context, span telemetrySpan, name string, output callbacks.CallbackOutput, callErr error) {
	completed := time.Now()
	call := ToolCallTrace{
		Sequence: span.sequence, Workload: telemetryWorkloadFromContext(ctx),
		CallID: compose.GetToolCallID(ctx), ToolName: name, StartedAt: span.started,
		CompletedAt: completed, DurationMS: completed.Sub(span.started).Milliseconds(), Outcome: "ok",
	}
	if callErr != nil {
		call.Outcome = "error"
		call.Error = boundedTelemetryError(callErr)
	} else if out := componenttool.ConvCallbackOutput(output); out != nil && IsRecoverableToolErrorPayload(out.Response) {
		call.Outcome = "error"
		call.Error = "recoverable tool error"
	}
	r.mu.Lock()
	r.run.ToolCalls = append(r.run.ToolCalls, call)
	r.mu.Unlock()
}

func (r *RunTelemetryRecorder) Complete(outcome string, runErr error) RunTelemetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.run.CompletedAt = time.Now()
		r.run.DurationMS = r.run.CompletedAt.Sub(r.run.StartedAt).Milliseconds()
		r.run.Outcome = outcome
		r.run.Error = boundedTelemetryError(runErr)
		r.finished = true
	}
	snapshot := r.run
	snapshot.InferenceCalls = append([]InferenceCall(nil), r.run.InferenceCalls...)
	snapshot.ToolCalls = append([]ToolCallTrace(nil), r.run.ToolCalls...)
	sort.Slice(snapshot.InferenceCalls, func(i, j int) bool {
		return snapshot.InferenceCalls[i].Sequence < snapshot.InferenceCalls[j].Sequence
	})
	sort.Slice(snapshot.ToolCalls, func(i, j int) bool {
		return snapshot.ToolCalls[i].Sequence < snapshot.ToolCalls[j].Sequence
	})
	return snapshot
}

func (r *RunTelemetryRecorder) CallbackHandler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			if info == nil || (info.Component != components.ComponentOfAgenticModel && info.Component != components.ComponentOfTool) {
				return ctx
			}
			return context.WithValue(ctx, telemetrySpanContextKey{}, r.start(ctx))
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			span, ok := ctx.Value(telemetrySpanContextKey{}).(telemetrySpan)
			if !ok || info == nil {
				return ctx
			}
			switch info.Component {
			case components.ComponentOfAgenticModel:
				r.finishModel(ctx, span, output, nil)
			case components.ComponentOfTool:
				r.finishTool(ctx, span, info.Name, output, nil)
			}
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			span, ok := ctx.Value(telemetrySpanContextKey{}).(telemetrySpan)
			if !ok || info == nil {
				return ctx
			}
			switch info.Component {
			case components.ComponentOfAgenticModel:
				r.finishModel(ctx, span, nil, err)
			case components.ComponentOfTool:
				r.finishTool(ctx, span, info.Name, nil, err)
			}
			return ctx
		}).Build()
}

func boundedTelemetryError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) > telemetryErrorLimit {
		value = value[:telemetryErrorLimit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func int64ptr(value int) *int64 {
	v := int64(value)
	return &v
}
