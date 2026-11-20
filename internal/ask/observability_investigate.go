package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// InvestigateToolName is the root-facing delegation tool. Its implementation
// runs a fresh, context-isolated child agent over the read-only tools of the
// observability MCP servers and returns one compact report, so raw telemetry
// never enters the root context.
const InvestigateToolName = "investigate_observability"

const (
	defaultInvestigateSoftDeadline = 4 * time.Minute
	// Grace must cover a full report generation plus any tool batch still in
	// flight when the soft deadline passed, since that deadline is only
	// evaluated between model calls. Measured report rounds run about 40s and
	// MCP calls can run to their own 2m timeout, so a 60s grace mostly
	// produced hard-deadline expiries rather than the intended partial
	// reports.
	defaultInvestigateGrace            = 2 * time.Minute
	defaultInvestigateSynthesisReserve = 90 * time.Second
	defaultInvestigateMinChildBudget   = 60 * time.Second
)

const investigateRequestDescription = `The investigation to carry out, written to stand alone: the investigator sees nothing of this conversation. Name the service and the symptom, give every relevant URL, ID, dashboard, namespace, route or error string exactly, give the time window, and say what a useful answer would contain.`

type investigateCapabilities struct {
	sentry        bool
	observability bool
}

func investigateToolDescription(capabilities investigateCapabilities) string {
	backends := make([]string, 0, 2)
	signals := make([]string, 0, 2)
	if capabilities.sentry {
		backends = append(backends, "Sentry")
		signals = append(signals, "Sentry issues and events")
	}
	if capabilities.observability {
		backends = append(backends, "Grafana, Prometheus, Loki, Tempo, and Pyroscope")
		signals = append(signals, "metrics, logs, traces, profiles, alerts, dashboards, and panels")
	}
	if len(backends) == 0 {
		backends = append(backends, "configured observability")
		signals = append(signals, "the telemetry exposed by its configured research tools")
	}
	description := fmt.Sprintf("Hand a question about live telemetry to a separate investigator that has read-only %s tools. Use it for questions about %s, which cannot be answered from memory or from discussion in the corpus. The investigator works for up to a few minutes and returns one report: what it observed, what it infers, and what it could not establish.", strings.Join(backends, " and "), strings.Join(signals, " or "))
	if capabilities.observability {
		description += " The report may list the artifact IDs of Grafana panels it rendered; name those IDs in the final artifacts field when the images belong with your answer."
	}
	return description + " It can run in the same round as corpus tools. If it fails, the error says so: answer from the evidence you have, or delegate a narrower request. A report cut short by the time limit says what it did not get to under Remaining uncertainty."
}

var investigateChildRole = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityRole)
var investigateChildDiscipline = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityDiscipline)
var investigateSentryDiscipline = embeddedAskPrompts.MustText(promptcatalog.AskObservabilitySentryDiscipline)
var investigateGrafanaDiscipline = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityGrafanaDiscipline)
var investigateGrafanaReportDiscipline = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityGrafanaReport)
var investigateChildReportContract = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityReport)

var investigateInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "request": {"type": "string", "minLength": 1, "description": ` + mustJSONString(investigateRequestDescription) + `},
    "time_window_utc": {
      "type": "object",
      "additionalProperties": false,
      "description": "The UTC window to investigate, when the asker gave one. When they gave none, leave it unset and say in request what period matters.",
      "properties": {
        "from": {"type": "string", "description": "Start of the window, inclusive: an RFC 3339 UTC time or a relative expression such as now-6h."},
        "to": {"type": "string", "description": "End of the window, inclusive: an RFC 3339 UTC time or now."}
      },
      "required": ["from", "to"]
    }
  },
  "required": ["request"]
}`)

// mustJSONString quotes a Go constant for embedding in the schema literal
// above, so the model-facing description lives beside the other prompt text
// instead of being escaped by hand.
func mustJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("%s: encode schema description: %v", InvestigateToolName, err))
	}
	return string(raw)
}

type investigateTimeWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type investigateInput struct {
	Request       string                 `json:"request"`
	TimeWindowUTC *investigateTimeWindow `json:"time_window_utc,omitempty"`
}

// InvestigateToolOptions assembles the delegation tool. The child agent is
// built per call from these parts; nothing here is shared mutable state.
type InvestigateToolOptions struct {
	// Model is the same configured model the root uses. Request-level
	// deadlines are retried once against this same model.
	Model model.AgenticModel
	// Servers are the MCP servers configured for the investigator. Their
	// tools are the child's research tools, and their kinds select its
	// backend prompts: a grafana server gives it the Grafana/Tempo discipline
	// and panel-report prompts, a sentry server the Sentry discipline.
	Servers []*MCPToolServer
	// Artifacts records steering provenance. Optional; a nil store skips
	// provenance and changes nothing else.
	Artifacts *ArtifactStore
	Metrics   *Metrics
	Logger    *slog.Logger

	// SoftDeadline bounds research before the child is told to report.
	// Grace is the extra window the report itself gets before the child
	// context is cancelled. SynthesisReserve is the turn time held back for
	// the root's own final call, and MinChildBudget is the smallest research
	// window worth launching a child for. Zero values take the 4m/60s/90s/60s
	// defaults.
	SoftDeadline     time.Duration
	Grace            time.Duration
	SynthesisReserve time.Duration
	MinChildBudget   time.Duration

	// Now injects the clock; zero value means time.Now.
	Now func() time.Time
}

type investigateTool struct {
	model            model.AgenticModel
	tools            []tool.BaseTool
	artifacts        *ArtifactStore
	metrics          *Metrics
	logger           *slog.Logger
	softDeadline     time.Duration
	grace            time.Duration
	synthesisReserve time.Duration
	minChildBudget   time.Duration
	now              func() time.Time
	capabilities     investigateCapabilities
}

// NewEinoInvestigateTool builds the delegation tool. Ordinary failures return
// ordinary errors; the registration-time WrapRecoverableToolErrors policy is
// what makes them model-visible recoverable results, so nothing here wraps
// them into a payload itself.
func NewEinoInvestigateTool(opts InvestigateToolOptions) (RegisteredTool, error) {
	if opts.Model == nil {
		return RegisteredTool{}, fmt.Errorf("%s: model is required", InvestigateToolName)
	}
	durations := []struct {
		name  string
		value *time.Duration
		fallD time.Duration
	}{
		{"SoftDeadline", &opts.SoftDeadline, defaultInvestigateSoftDeadline},
		{"Grace", &opts.Grace, defaultInvestigateGrace},
		{"SynthesisReserve", &opts.SynthesisReserve, defaultInvestigateSynthesisReserve},
		{"MinChildBudget", &opts.MinChildBudget, defaultInvestigateMinChildBudget},
	}
	for _, d := range durations {
		if *d.value == 0 {
			*d.value = d.fallD
			continue
		}
		if *d.value < 0 {
			return RegisteredTool{}, fmt.Errorf("%s: %s must be positive", InvestigateToolName, d.name)
		}
	}

	ctx := context.Background()
	capabilities := investigateCapabilities{}
	var research []RegisteredTool
	for _, server := range opts.Servers {
		switch server.Kind() {
		case MCPServerSentry:
			capabilities.sentry = true
		case MCPServerGrafana:
			capabilities.observability = true
		}
		research = append(research, server.Tools()...)
	}
	if len(research) == 0 {
		return RegisteredTool{}, fmt.Errorf("%s: at least one research tool is required", InvestigateToolName)
	}
	tools := make([]tool.BaseTool, 0, len(research))
	for i, reg := range research {
		if reg.Tool == nil {
			return RegisteredTool{}, fmt.Errorf("%s: research tool %d is nil", InvestigateToolName, i+1)
		}
		info, err := reg.Tool.Info(ctx)
		if err != nil {
			return RegisteredTool{}, fmt.Errorf("%s: research tool %d info: %w", InvestigateToolName, i+1, err)
		}
		if info == nil {
			return RegisteredTool{}, fmt.Errorf("%s: research tool %d has no info", InvestigateToolName, i+1)
		}
		// Structural, not stylistic: the terminal tool would let a child end
		// the user's turn from inside a tool result, and the delegation tool
		// would let it recurse.
		if info.Name == InvestigateToolName {
			return RegisteredTool{}, fmt.Errorf("%s: %q must not be a child research tool", InvestigateToolName, info.Name)
		}
		tools = append(tools, reg.Tool)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	t := &investigateTool{
		model:            opts.Model,
		tools:            tools,
		artifacts:        opts.Artifacts,
		metrics:          opts.Metrics,
		logger:           logger,
		softDeadline:     opts.SoftDeadline,
		grace:            opts.Grace,
		synthesisReserve: opts.SynthesisReserve,
		minChildBudget:   opts.MinChildBudget,
		now:              now,
		capabilities:     capabilities,
	}

	js, err := parseToolJSONSchema(investigateInputSchema)
	if err != nil {
		return RegisteredTool{}, fmt.Errorf("%s: parse model schema: %w", InvestigateToolName, err)
	}
	info := &schema.ToolInfo{
		Name:        InvestigateToolName,
		Desc:        investigateToolDescription(capabilities),
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
	return RegisteredTool{
		Tool:         utils.NewTool(info, t.invoke),
		Presentation: investigatePresentation(),
	}, nil
}

func investigatePresentation() ToolPresentation {
	return ToolPresentation{
		Status: func(input json.RawMessage) StatusLine {
			line := StatusLine{Emoji: "mag", Verb: "investigating live telemetry"}
			var probe investigateInput
			if err := json.Unmarshal(input, &probe); err == nil {
				line.Detail = truncateStatusDetail(probe.Request)
			}
			return line
		},
		StatusOutput: func(json.RawMessage) string { return "investigation report received" },
	}
}

func (t *investigateTool) invoke(ctx context.Context, in investigateInput) (string, error) {
	// A cancelled parent turn is an abort, never a recoverable tool result.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	request := strings.TrimSpace(in.Request)
	if request == "" {
		return "", errors.New("request is required and must describe the investigation on its own")
	}

	started := t.now()
	soft, err := t.admit(ctx, started)
	if err != nil {
		t.metrics.observeInvestigateChild("insufficient_time", 0, 0, 0)
		return "", err
	}
	hard := soft.Add(t.grace)
	childCtx, cancel := context.WithDeadline(ctx, hard)
	defer cancel()

	bundle := promptBundleFromContext(ctx)
	steering := buildDelegatedSteeringWith(bundle, request, t.capabilities)
	t.recordSteering(ctx, steering)

	runner, err := newInvestigateChildRunner(ctx, investigateChildConfig{
		model:       t.model,
		tools:       t.tools,
		instruction: investigateChildInstructionWith(bundle, steering, t.capabilities),
	})
	if err != nil {
		t.metrics.observeInvestigateChild("error", t.now().Sub(started), 0, 0)
		return "", fmt.Errorf("telemetry investigation could not start: %v", err)
	}

	child := newInvestigateChildState(soft, t.now)
	runErr := runInvestigateChild(
		investigateChildContext(childCtx, child),
		runner,
		[]*schema.AgenticMessage{NewAgenticUserText(delegatedInvestigationInput(request, in.TimeWindowUTC))},
	)

	duration := t.now().Sub(started)
	rounds := child.toolRoundCount()
	report := strings.TrimSpace(child.reportText())

	switch {
	case ctx.Err() != nil:
		// The turn itself ended. Aborting is correct: there is nobody left to
		// read a recoverable result.
		t.metrics.observeInvestigateChild("error", duration, rounds, 0)
		return "", ctx.Err()
	case runErr == nil && report != "":
		outcome := "reported"
		if child.forcedReportStarted() {
			outcome = "forced_report"
		}
		t.metrics.observeInvestigateChild(outcome, duration, rounds, len(report))
		return report, nil
	case childCtx.Err() != nil:
		// Deliberately not wrapping context.DeadlineExceeded: the tool error
		// policy treats deadline and cancellation errors as aborts, and this
		// one is recoverable — the root can still answer from what it has.
		t.metrics.observeInvestigateChild("deadline_expired", duration, rounds, 0)
		t.logger.Warn("ask: observability investigation deadline expired",
			"tool_rounds", rounds, "duration", duration, "forced_report", child.forcedReportStarted())
		return "", errors.New("investigation deadline expired before a report was produced; answer from the evidence you already have or delegate a narrower request")
	case runErr != nil:
		t.metrics.observeInvestigateChild("error", duration, rounds, 0)
		t.logger.Warn("ask: observability investigation failed", "tool_rounds", rounds, "err", runErr)
		// %v, not %w: an inner deadline or cancellation sentinel must not
		// escape into the tool error policy and abort the whole turn.
		return "", fmt.Errorf("telemetry investigation failed: %v", runErr)
	default:
		t.metrics.observeInvestigateChild("error", duration, rounds, 0)
		return "", errors.New("telemetry investigation finished without producing a report")
	}
}

// admit applies the turn-budget admission check and returns the child's soft
// deadline. A context with no deadline means the surface applied no turn
// bound, so the configured soft deadline stands on its own.
func (t *investigateTool) admit(ctx context.Context, now time.Time) (time.Time, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return now.Add(t.softDeadline), nil
	}
	remaining := deadline.Sub(now)
	if remaining < t.synthesisReserve+t.minChildBudget+t.grace {
		return time.Time{}, errors.New("insufficient time remaining in this turn for a telemetry investigation; answer from existing evidence")
	}
	research := remaining - t.synthesisReserve - t.grace
	if research > t.softDeadline {
		research = t.softDeadline
	}
	return now.Add(research), nil
}

// recordSteering writes the delegated routing decision to
// ask_observability_steering. A provenance failure is logged and swallowed:
// the investigation is the product, the audit row is not worth failing it for.
func (t *investigateTool) recordSteering(ctx context.Context, steering ObservabilitySteering) {
	sessionID := artifactSessionFromContext(ctx)
	if t.artifacts == nil || sessionID == uuid.Nil {
		return
	}
	userTurn := 0
	if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
		userTurn = recorder.NextTurnToCommit()
	}
	if err := t.artifacts.RecordSteering(ctx, sessionID, userTurn, steering); err != nil {
		t.logger.Warn("ask: record delegated observability steering failed",
			"session_id", sessionID, "user_turn", userTurn, "err", err)
	}
}

// buildDelegatedSteering routes the delegated request through the same
// deterministic router the root used. The one difference: inside this tool the
// request is known to be a telemetry investigation, so an unmatched intent
// takes the incident workflow instead of switching steering off — the router's
// intent tokens exist to detect observability questions, and that question is
// already settled by the delegation.
//
// Text and BundleHash are recomputed over the child's steering block, because
// the persisted provenance must hash the text the child was actually given
// rather than the root bundle the router composes.
func buildDelegatedSteering(request string, capabilities investigateCapabilities) ObservabilitySteering {
	return buildDelegatedSteeringWith(promptcatalog.Active(), request, capabilities)
}

func buildDelegatedSteeringWith(bundle promptcatalog.Bundle, request string, capabilities investigateCapabilities) ObservabilitySteering {
	steering := buildObservabilitySteering(bundle, request)
	sentryRequest := capabilities.sentry && containsAnyToken(steering.normalizedInput, []string{"sentry", "sentry io"})
	if capabilities.sentry && (!capabilities.observability || sentryRequest) {
		// The schema currently records the broad intent enum in Postgres; keep
		// Sentry investigations in the incident bucket while the workflow card
		// captures the exact child-facing route.
		steering.Active = true
		steering.Intent = "incident"
		steering.CardIDs = []string{"workflow:sentry"}
		steering.PanelRequested = capabilities.observability && observabilityPanelRequested(steering.normalizedInput)
	} else if !steering.Active {
		steering.Active = true
		steering.Intent = "incident"
		steering.CardIDs = []string{"workflow:incident", "syntax:promql", "syntax:logql", "syntax:traceql"}
		steering.PanelRequested = observabilityPanelRequested(steering.normalizedInput)
	}
	steering.Text = delegatedSteeringText(bundle, steering)
	steering.RouterHash = hashText(ObservabilityRouterVersion + "\n" + steering.normalizedInput + "\n" + strings.Join(steering.CardIDs, ","))
	steering.BundleHash = hashText(ObservabilityBundleVersion + "\n" + steering.Text)
	return steering
}

// delegatedSteeringText is the selected cards alone. The root bundle's tool
// manifest is deliberately omitted: it enumerates the per-intent tool subset,
// and the child holds the whole read-only toolset.
func delegatedSteeringText(bundle promptcatalog.Bundle, steering ObservabilitySteering) string {
	parts := make([]string, 0, len(steering.CardIDs))
	for _, id := range steering.CardIDs {
		if workflow, ok := strings.CutPrefix(id, "workflow:"); ok {
			if key, exists := observabilityWorkflows[workflow]; exists {
				parts = append(parts, bundle.MustText(key))
			}
			continue
		}
		card, ok := strings.CutPrefix(id, "syntax:")
		if !ok {
			continue
		}
		if key, ok := observabilitySyntax[card]; ok {
			parts = append(parts, bundle.MustText(key))
		}
	}
	return strings.Join(parts, "\n\n")
}

func investigateChildInstruction(steering ObservabilitySteering, capabilities investigateCapabilities) string {
	return investigateChildInstructionWith(embeddedAskPrompts, steering, capabilities)
}

func investigateChildInstructionWith(bundle promptcatalog.Bundle, steering ObservabilitySteering, capabilities investigateCapabilities) string {
	parts := []string{
		bundle.MustText(promptcatalog.AskObservabilityRole),
		bundle.MustText(promptcatalog.AskObservabilityDiscipline),
	}
	if capabilities.sentry {
		parts = append(parts, bundle.MustText(promptcatalog.AskObservabilitySentryDiscipline))
	}
	if capabilities.observability {
		parts = append(parts,
			bundle.MustText(promptcatalog.AskObservabilityGrafanaDiscipline),
			bundle.MustText(promptcatalog.AskObservabilityGrafanaReport),
		)
	}
	if text := strings.TrimSpace(steering.Text); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(append(parts, bundle.MustText(promptcatalog.AskObservabilityReport)), "\n\n")
}

func containsToolName(names []string, candidate string) bool {
	for _, name := range names {
		if name == candidate {
			return true
		}
	}
	return false
}

func delegatedInvestigationInput(request string, window *investigateTimeWindow) string {
	if window == nil {
		return request
	}
	from := strings.TrimSpace(window.From)
	to := strings.TrimSpace(window.To)
	if from == "" && to == "" {
		return request
	}
	return fmt.Sprintf("%s\n\nExplicit UTC time window: from %q to %q.", request, from, to)
}
