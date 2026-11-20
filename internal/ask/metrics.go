package ask

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	componenttool "github.com/cloudwego/eino/components/tool"
	"github.com/prometheus/client_golang/prometheus"
)

const unknownToolMetricLabel = "unknown"

// Metrics is the Prometheus sink for Ask. Eino owns component lifecycle;
// CallbackHandler translates its native model and tool callbacks into this
// bounded metric surface while AskService records application-level outcomes.
type Metrics struct {
	turns                *prometheus.CounterVec
	turnDuration         *prometheus.HistogramVec
	modelRequests        *prometheus.CounterVec
	modelRequestDuration *prometheus.HistogramVec
	modelRounds          *prometheus.CounterVec
	tokens               *prometheus.CounterVec
	toolCalls            *prometheus.CounterVec
	toolCallDuration     *prometheus.HistogramVec
	forcedSynthesis      *prometheus.CounterVec
	persistenceFailures  *prometheus.CounterVec

	// Child investigations get their own series rather than an agent label on
	// the shared model/tool series: the dashboards key on the current label
	// sets, and a new label would silently split every existing panel.
	investigateRuns        *prometheus.CounterVec
	investigateDuration    prometheus.Histogram
	investigateToolRounds  prometheus.Counter
	investigateReportBytes prometheus.Histogram
}

// NewMetrics registers the Ask collectors. A nil registerer returns a
// zero-value, nil-safe sink for callers that intentionally disable metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return &Metrics{}
	}
	m := &Metrics{
		turns: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_turns_total",
			Help: "Count of Ask turns by surface and outcome.",
		}, []string{"surface", "outcome"})),
		turnDuration: regAskHistogramVec(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_ask_turn_duration_seconds",
			Help:    "End-to-end Ask turn latency by surface.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 10),
		}, []string{"surface"})),
		modelRequests: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_model_requests_total",
			Help: "Count of Eino model requests by Ask surface and outcome.",
		}, []string{"surface", "outcome"})),
		modelRequestDuration: regAskHistogramVec(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_ask_model_request_duration_seconds",
			Help:    "Eino model request latency by Ask surface.",
			Buckets: prometheus.ExponentialBuckets(0.25, 2, 12),
		}, []string{"surface"})),
		modelRounds: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_model_rounds_total",
			Help: "Successful Eino model rounds by Ask surface and whether the response requested tools.",
		}, []string{"surface", "tool_calls"})),
		tokens: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_tokens_total",
			Help: "Tokens reported by Eino model responses by Ask surface and token type.",
		}, []string{"surface", "type"})),
		toolCalls: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_tool_calls_total",
			Help: "Count of Eino tool calls by Ask surface, registered tool name, and outcome.",
		}, []string{"surface", "tool", "outcome"})),
		toolCallDuration: regAskHistogramVec(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_ask_tool_call_duration_seconds",
			Help:    "Eino tool call latency by Ask surface and registered tool name.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
		}, []string{"surface", "tool"})),
		forcedSynthesis: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_forced_synthesis_total",
			Help: "Tool-budget forced synthesis turns by Ask surface and outcome.",
		}, []string{"surface", "outcome"})),
		persistenceFailures: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_persistence_failures_total",
			Help: "Ask persistence failures by surface and operation.",
		}, []string{"surface", "operation"})),
		investigateRuns: regAskCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_observability_child_runs_total",
			Help: "Delegated observability investigations by outcome (reported, forced_report, deadline_expired, insufficient_time, error).",
		}, []string{"outcome"})),
		investigateDuration: regAskHistogram(reg, prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "diderot_ask_observability_child_duration_seconds",
			Help:    "Wall-clock duration of one delegated observability investigation.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		})),
		investigateToolRounds: regAskCounter(reg, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_ask_observability_child_tool_rounds_total",
			Help: "Assistant tool-call batches issued inside delegated observability investigations.",
		})),
		investigateReportBytes: regAskHistogram(reg, prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "diderot_ask_observability_child_report_bytes",
			Help:    "Size of the report returned by a delegated observability investigation.",
			Buckets: prometheus.ExponentialBuckets(256, 2, 10),
		})),
	}
	return m
}

type modelMetricStartKey struct{}
type toolMetricStartKey struct{}

// CallbackHandler returns a per-turn Eino callback handler. knownTools is
// copied so model-produced unknown names cannot create unbounded Prometheus
// label cardinality.
func (m *Metrics) CallbackHandler(surface AskRunSurface, knownTools map[string]ToolPresentation) callbacks.Handler {
	known := make(map[string]struct{}, len(knownTools))
	for name := range knownTools {
		known[name] = struct{}{}
	}
	toolLabel := func(name string) string {
		if _, ok := known[name]; ok {
			return name
		}
		return unknownToolMetricLabel
	}

	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			if info == nil {
				return ctx
			}
			switch info.Component {
			case components.ComponentOfAgenticModel:
				return context.WithValue(ctx, modelMetricStartKey{}, time.Now())
			case components.ComponentOfTool:
				return context.WithValue(ctx, toolMetricStartKey{}, time.Now())
			default:
				return ctx
			}
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			if info == nil {
				return ctx
			}
			switch info.Component {
			case components.ComponentOfAgenticModel:
				duration, timed := elapsedSeconds(ctx, modelMetricStartKey{})
				out := model.ConvAgenticCallbackOutput(output)
				if out == nil || out.Message == nil {
					m.observeModelRequest(surface, "unknown", duration, timed)
					return ctx
				}
				m.observeModelRequest(surface, "ok", duration, timed)
				toolCalls := "false"
				if len(AgenticMessageToolCalls(out.Message)) > 0 {
					toolCalls = "true"
				}
				m.observeModelRound(surface, toolCalls)
				usage := out.TokenUsage
				if usage == nil && out.Message != nil && out.Message.ResponseMeta != nil {
					u := out.Message.ResponseMeta.TokenUsage
					if u != nil {
						usage = &model.TokenUsage{
							PromptTokens:     u.PromptTokens,
							CompletionTokens: u.CompletionTokens,
							TotalTokens:      u.TotalTokens,
							PromptTokenDetails: model.PromptTokenDetails{
								CachedTokens: u.PromptTokenDetails.CachedTokens,
							},
						}
					}
				}
				m.observeUsage(surface, usage)
			case components.ComponentOfTool:
				label := toolLabel(info.Name)
				outcome := "unknown"
				if out := componenttool.ConvCallbackOutput(output); out != nil {
					outcome = "ok"
					if IsRecoverableToolErrorPayload(out.Response) {
						outcome = "error"
					}
				}
				duration, timed := elapsedSeconds(ctx, toolMetricStartKey{})
				m.observeToolCall(surface, label, outcome, duration, timed)
			}
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, _ error) context.Context {
			if info == nil {
				return ctx
			}
			// A call cut short because whoever asked ended the turn did
			// not fail.
			outcome := cmp.Or(turnEnding(ctx), "error")
			switch info.Component {
			case components.ComponentOfAgenticModel:
				duration, timed := elapsedSeconds(ctx, modelMetricStartKey{})
				m.observeModelRequest(surface, outcome, duration, timed)
			case components.ComponentOfTool:
				duration, timed := elapsedSeconds(ctx, toolMetricStartKey{})
				m.observeToolCall(surface, toolLabel(info.Name), outcome, duration, timed)
			}
			return ctx
		}).
		Build()
}

func elapsedSeconds(ctx context.Context, key any) (float64, bool) {
	started, _ := ctx.Value(key).(time.Time)
	if started.IsZero() {
		return 0, false
	}
	return time.Since(started).Seconds(), true
}

func (m *Metrics) observeTurn(surface AskRunSurface, outcome string, duration time.Duration) {
	if m == nil || m.turns == nil {
		return
	}
	m.turns.WithLabelValues(string(surface), outcome).Inc()
	if m.turnDuration != nil {
		m.turnDuration.WithLabelValues(string(surface)).Observe(duration.Seconds())
	}
}

func (m *Metrics) observeModelRequest(surface AskRunSurface, outcome string, duration float64, timed bool) {
	if m == nil || m.modelRequests == nil {
		return
	}
	m.modelRequests.WithLabelValues(string(surface), outcome).Inc()
	if timed && m.modelRequestDuration != nil {
		m.modelRequestDuration.WithLabelValues(string(surface)).Observe(duration)
	}
}

func (m *Metrics) observeModelRound(surface AskRunSurface, toolCalls string) {
	if m != nil && m.modelRounds != nil {
		m.modelRounds.WithLabelValues(string(surface), toolCalls).Inc()
	}
}

func (m *Metrics) observeUsage(surface AskRunSurface, usage *model.TokenUsage) {
	if m == nil || m.tokens == nil || usage == nil {
		return
	}
	values := []struct {
		label string
		value int
	}{
		{"input", usage.PromptTokens},
		{"cached_input", usage.PromptTokenDetails.CachedTokens},
		{"output", usage.CompletionTokens},
	}
	for _, value := range values {
		if value.value > 0 {
			m.tokens.WithLabelValues(string(surface), value.label).Add(float64(value.value))
		}
	}
}

func (m *Metrics) observeToolCall(surface AskRunSurface, toolName, outcome string, duration float64, timed bool) {
	if m == nil || m.toolCalls == nil {
		return
	}
	m.toolCalls.WithLabelValues(string(surface), toolName, outcome).Inc()
	if timed && m.toolCallDuration != nil {
		m.toolCallDuration.WithLabelValues(string(surface), toolName).Observe(duration)
	}
}

func (m *Metrics) observeForcedSynthesis(surface AskRunSurface, outcome string) {
	if m != nil && m.forcedSynthesis != nil {
		m.forcedSynthesis.WithLabelValues(string(surface), outcome).Inc()
	}
}

// observeInvestigateChild records one delegated investigation. Duration,
// rounds, and report size are only meaningful for a child that actually ran,
// so a zero value skips its series rather than polluting it.
func (m *Metrics) observeInvestigateChild(outcome string, duration time.Duration, toolRounds, reportBytes int) {
	if m == nil || m.investigateRuns == nil {
		return
	}
	m.investigateRuns.WithLabelValues(outcome).Inc()
	if duration > 0 && m.investigateDuration != nil {
		m.investigateDuration.Observe(duration.Seconds())
	}
	if toolRounds > 0 && m.investigateToolRounds != nil {
		m.investigateToolRounds.Add(float64(toolRounds))
	}
	if reportBytes > 0 && m.investigateReportBytes != nil {
		m.investigateReportBytes.Observe(float64(reportBytes))
	}
}

func (m *Metrics) observePersistenceFailure(surface AskRunSurface, operation string) {
	if m != nil && m.persistenceFailures != nil {
		m.persistenceFailures.WithLabelValues(string(surface), operation).Inc()
	}
}

func regAskCounterVec(reg prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return collector
}

func regAskCounter(reg prometheus.Registerer, collector prometheus.Counter) prometheus.Counter {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Counter)
		}
		panic(err)
	}
	return collector
}

func regAskHistogram(reg prometheus.Registerer, collector prometheus.Histogram) prometheus.Histogram {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Histogram)
		}
		panic(err)
	}
	return collector
}

func regAskHistogramVec(reg prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return collector
}
