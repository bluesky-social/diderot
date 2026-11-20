package ask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// investigateChildMaxIterations is a pure circuit breaker, not a research
// budget: depth is bounded by the soft/hard wall-clock deadlines below. It
// exists so a model that loops on a failing tool cannot spin until the hard
// deadline with no progress.
const investigateChildMaxIterations = 32

const childForcedReportInstr = `Your time for this investigation is up, so you cannot call tools again. Write the report now, in the required format, from the evidence above. Put what you did not get to under Remaining uncertainty.`

const childPartialReportText = "(investigation hit its time limit before completing a report; partial tool evidence was gathered but no summary is available)"

type investigateChildStateContextKey struct{}

// investigateChildState is one child run's forced-report state. It holds the
// soft deadline, the one-shot forced flag, the completed tool-round count, and
// the final rewritten assistant text.
//
// The report is captured here rather than read off the outward AgentEvent for
// the same reason AskService uses AgenticRunRecorder.PendingAssistantSnapshot:
// the outward event can still carry the pre-rewrite model output, so a forced
// run's fallback text would not be what the tool returned.
type investigateChildState struct {
	softDeadline time.Time
	now          func() time.Time

	mu         sync.Mutex
	forced     bool
	toolRounds int
	report     string
}

func newInvestigateChildState(softDeadline time.Time, now func() time.Time) *investigateChildState {
	if now == nil {
		now = time.Now
	}
	return &investigateChildState{softDeadline: softDeadline, now: now}
}

func withInvestigateChildState(ctx context.Context, state *investigateChildState) context.Context {
	return context.WithValue(ctx, investigateChildStateContextKey{}, state)
}

func investigateChildStateFromContext(ctx context.Context) *investigateChildState {
	state, _ := ctx.Value(investigateChildStateContextKey{}).(*investigateChildState)
	return state
}

func (s *investigateChildState) softDeadlinePassed() bool {
	if s == nil || s.softDeadline.IsZero() {
		return false
	}
	return !s.now().Before(s.softDeadline)
}

// beginForcedReport flips the one-shot forced flag, reporting whether this
// call is the one that flipped it.
func (s *investigateChildState) beginForcedReport() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forced {
		return false
	}
	s.forced = true
	return true
}

func (s *investigateChildState) forcedReportStarted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forced
}

func (s *investigateChildState) captureReport(text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report = text
}

func (s *investigateChildState) reportText() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

// recordToolRound counts one completed assistant tool-call batch. It is
// counted from the model boundary rather than from the drained event stream so
// the count is exactly one per model output regardless of how the runner
// surfaces events.
func (s *investigateChildState) recordToolRound() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolRounds++
}

func (s *investigateChildState) toolRoundCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolRounds
}

// investigateReportMiddleware bounds a child investigation by wall clock. It
// reuses the state-rewrite shape of agenticBudgetMiddleware, but it is
// triggered by time rather than by a round count, and its terminal mechanism
// is ordinary assistant text rather than the root's constrained JSON response.
type investigateReportMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newInvestigateReportMiddleware() *investigateReportMiddleware {
	return &investigateReportMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

func (m *investigateReportMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	child := investigateChildStateFromContext(ctx)
	if child == nil || child.forcedReportStarted() || !child.softDeadlinePassed() {
		return m.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	if state == nil || len(state.Messages) == 0 {
		return m.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	last := state.Messages[len(state.Messages)-1]
	if last == nil || last.Role != schema.AgenticRoleTypeUser || len(AgenticMessageToolResults(last)) == 0 {
		// The only call whose trailing message is not a tool-result batch is
		// the first one, before any evidence exists. Forcing a report there
		// would demand a summary of nothing, so leave the run alone; the hard
		// deadline still bounds it. The admission check makes this
		// unreachable outside tests, where a child always gets a minimum
		// research window.
		return m.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}
	if !child.beginForcedReport() {
		return m.TypedBaseChatModelAgentMiddleware.BeforeModelRewriteState(ctx, state, modelContext)
	}

	next := *state
	next.Messages = append([]*schema.AgenticMessage(nil), state.Messages...)
	// Keep the complete tool declarations. The model gateway's Anthropic converter
	// validates every historical function call against the tools declared on
	// the current Responses request, so dropping research schemas here would
	// fail an otherwise valid conversation. WrapModel separately forbids new
	// calls.
	next.ToolInfos = append([]*schema.ToolInfo(nil), state.ToolInfos...)
	next.DeferredToolInfos = append([]*schema.ToolInfo(nil), state.DeferredToolInfos...)
	// Anthropic rejects a Responses history item that mixes tool results with
	// ordinary user text, so the instruction is its own user message after the
	// complete result batch.
	next.Messages = append(next.Messages, NewAgenticUserText(childForcedReportInstr))
	return ctx, &next, nil
}

func (m *investigateReportMiddleware) AfterModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	child := investigateChildStateFromContext(ctx)
	if child == nil {
		return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
	}
	if state == nil || len(state.Messages) == 0 {
		if child.forcedReportStarted() {
			return ctx, state, errors.New("ask investigate: forced report produced no message")
		}
		return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
	}
	latest := state.Messages[len(state.Messages)-1]
	if latest != nil && len(AgenticMessageToolCalls(latest)) > 0 {
		child.recordToolRound()
	}
	if latest != nil && len(AgenticMessageToolCalls(latest)) == 0 && strings.TrimSpace(AgenticMessageText(latest)) != "" {
		// Terminal on both the ordinary and the forced path: the child's final
		// tool-free assistant text is the report.
		child.captureReport(AgenticMessageText(latest))
		return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
	}
	if !child.forcedReportStarted() {
		return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
	}

	// Correctness must not depend on the gateway honoring tool_choice: a
	// forced round that still asks for tools, or returns nothing usable, is
	// replaced by a bounded partial-report result.
	fallback := NewAgenticAssistantText(childPartialReportText)
	if latest != nil {
		if usage := AgenticMessageUsage(latest); usage != nil {
			fallback.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: usage}
		}
	}
	child.captureReport(childPartialReportText)
	next := *state
	next.Messages = append([]*schema.AgenticMessage(nil), state.Messages...)
	next.Messages[len(next.Messages)-1] = fallback
	return ctx, &next, nil
}

func (m *investigateReportMiddleware) WrapModel(
	_ context.Context,
	mdl model.BaseModel[*schema.AgenticMessage],
	_ *adk.TypedModelContext[*schema.AgenticMessage],
) (model.BaseModel[*schema.AgenticMessage], error) {
	return forcedReportModel{inner: mdl}, nil
}

type forcedReportModel struct {
	inner model.BaseModel[*schema.AgenticMessage]
}

func (m forcedReportModel) Generate(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.AgenticMessage, error) {
	return m.inner.Generate(ctx, input, appendForcedReportToolChoice(ctx, opts)...)
}

func (m forcedReportModel) Stream(
	ctx context.Context,
	input []*schema.AgenticMessage,
	opts ...model.Option,
) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return m.inner.Stream(ctx, input, appendForcedReportToolChoice(ctx, opts)...)
}

func appendForcedReportToolChoice(ctx context.Context, opts []model.Option) []model.Option {
	if !investigateChildStateFromContext(ctx).forcedReportStarted() {
		return opts
	}
	// Use the provider-specific extra-fields channel. Eino's
	// AgenticToolChoice has no "forbid every tool" form, and the model gateway's
	// Responses-to-Anthropic converter takes the plain OpenAI Responses string
	// for this case rather than an object.
	restrictions := modelOptionsForOverrides(modelOverridesFromContext(ctx), map[string]any{"tool_choice": "none"})
	return append(append([]model.Option(nil), opts...), restrictions...)
}

// investigateChildConfig is the per-call child agent recipe. Everything in it
// is derived inside the investigate tool function; nothing is shared between
// concurrent investigations.
type investigateChildConfig struct {
	model       model.AgenticModel
	tools       []tool.BaseTool
	instruction string
}

// newInvestigateChildRunner builds one throwaway child agent and its runner.
// It mirrors NewAskAgents' construction shape with three deliberate
// differences: ordinary-text terminal output, a child-specific instruction, and only the
// status and forced-report middlewares.
//
// The root's recording and budget middlewares are explicitly NOT installed.
// Both act on state carried in context by the root turn, so a child that omits
// them cannot touch the root's recorder or spend the root's tool budget. That
// omission is the isolation property; do not add middlewares here without
// checking what root state they reach for.
func newInvestigateChildRunner(ctx context.Context, cfg investigateChildConfig) (*adk.TypedRunner[*schema.AgenticMessage], error) {
	if cfg.model == nil {
		return nil, errors.New("ask investigate child: model is required")
	}
	if strings.TrimSpace(cfg.instruction) == "" {
		return nil, errors.New("ask investigate child: instruction is required")
	}
	if len(cfg.tools) == 0 {
		return nil, errors.New("ask investigate child: at least one research tool is required")
	}
	// An MCP failure must be visible to the child model as a result it can
	// route around, not an aborted investigation.
	wrapped, err := WrapRecoverableToolErrors(cfg.tools, ChildToolResultMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("ask investigate child: apply tool error policy: %w", err)
	}
	agentCfg := &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Model:         cfg.model,
		Instruction:   cfg.instruction,
		MaxIterations: investigateChildMaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: wrapped,
			// UnknownToolResult rather than recordingUnknownToolResult: the
			// recording variant writes into the root recorder it finds in
			// context, which is exactly the root state a child must not touch.
			UnknownToolsHandler: UnknownToolResult,
			ExecuteSequentially: false,
		}},
		// The status middleware is deliberately absent. A child routinely makes
		// dozens of tool calls, and its internal progress must not leak into the
		// root run's user-visible status sink or recorder.
		Handlers: []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{
			newInvestigateReportMiddleware(),
		},
	}
	agent, err := adk.NewTypedChatModelAgent(ctx, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("ask investigate child: build agent: %w", err)
	}
	return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{
		Agent:           agent,
		EnableStreaming: false,
		CheckPointStore: nil,
	}), nil
}

// investigateChildContext installs the child's forced-report state and removes
// the root-only run mechanisms from the inherited context: the recorder, the
// tool budget, and the user-visible status sink. Everything else the child needs
// — artifact session, allowed scopes, citations, logger, cancellation — is
// context-carried on purpose and rides through unchanged.
func investigateChildContext(ctx context.Context, child *investigateChildState) context.Context {
	ctx = withInvestigateChildState(ctx, child)
	ctx = withTelemetryWorkload(ctx, TelemetryWorkloadObservabilityChild)
	ctx = withAgenticRunRecorder(ctx, nil)
	ctx = withAgenticToolBudget(ctx, nil)
	ctx = withoutToolStatus(ctx)
	return ctx
}

// runInvestigateChild drains the child event stream exactly as
// AskService.runTurnLocked drains the root's, propagating the first event
// error. The child runner is deliberately invoked with no adk.WithCallbacks
// and no after-tool-calls hook: Eino propagates the root run's callback
// handlers to nested runs through context, so re-installing the metrics
// handler here would double-count every model and tool callback.
func runInvestigateChild(
	ctx context.Context,
	runner *adk.TypedRunner[*schema.AgenticMessage],
	input []*schema.AgenticMessage,
) error {
	options := modelOptionsForOverrides(modelOverridesFromContext(ctx), nil)
	if metadata, ok := askRunMetadataFromContext(ctx); ok {
		var err error
		options, err = askRunModelOptions(metadata, options...)
		if err != nil {
			return fmt.Errorf("ask investigate child: run metadata: %w", err)
		}
	}
	runOptions := make([]adk.AgentRunOption, 0, 1)
	runOptions = append(runOptions, adk.WithChatModelOptions(options))
	iter := runner.Run(ctx, input, runOptions...)
	var runErr error
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			if runErr == nil {
				runErr = event.Err
			}
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		if _, getErr := event.Output.MessageOutput.GetMessage(); getErr != nil {
			if runErr == nil {
				runErr = getErr
			}
		}
	}
	return runErr
}
