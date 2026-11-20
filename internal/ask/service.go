package ask

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// ErrUnavailable is a turn asked of a replica that has no Ask service right
// now: it is still building one, the build failed, or the replica is
// stopping. Another replica, or this one a little later, may answer.
var ErrUnavailable = errors.New("ask is unavailable on this replica; retry")

// ErrNotSetUp is a turn asked before an administrator has chosen Ask's
// model and the embedding model search needs. It is an ErrUnavailable,
// with a message that says what is missing.
var ErrNotSetUp error = notSetUp{}

type notSetUp struct{}

func (notSetUp) Error() string {
	return "Ask is not set up yet: an administrator chooses its model and the embedding model on the Models page"
}

func (notSetUp) Is(target error) bool { return target == ErrUnavailable }

// ErrTurnStopped is the cause a surface cancels a turn's context with when
// the person who asked stopped it (Slack's stop button). The turn records
// the outcome "stopped", not an error.
var ErrTurnStopped = errors.New("ask: the person who asked stopped this turn")

// TurnStopped reports whether ctx ended because the person who asked
// stopped the turn.
func TurnStopped(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrTurnStopped)
}

// ErrTurnCancelled is the cause a surface cancels a turn's context with when
// the caller that asked went away: an API client that closed its connection,
// or an MCP client that cancelled its task. The turn records the outcome
// "cancelled", not an error.
var ErrTurnCancelled = errors.New("ask: the caller cancelled this turn")

// TurnCancelled reports whether ctx ended because the turn's caller went
// away.
func TurnCancelled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrTurnCancelled)
}

// WithCallerCancel returns the context of a turn asked through request, a
// caller's request context: it carries request's values, and is cancelled
// with ErrTurnCancelled when the caller cancels request. Anything else that
// cancels the turn (its deadline, the replica stopping) is not the caller's
// doing and keeps its own cause. The returned function releases the context.
func WithCallerCancel(request context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(request))
	stop := context.AfterFunc(request, func() {
		cause := context.Cause(request)
		if cause == context.Canceled {
			cause = ErrTurnCancelled
		}
		cancel(cause)
	})
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// turnEnding is the outcome of a turn whose context was ended by whoever
// asked: "stopped" (ErrTurnStopped) or "cancelled" (ErrTurnCancelled). It is
// empty for a context that has not ended, or that ended any other way.
// Neither ending is a failure, of the turn or of the model and tool calls it
// cut short.
func turnEnding(ctx context.Context) string {
	switch {
	case TurnStopped(ctx):
		return "stopped"
	case TurnCancelled(ctx):
		return "cancelled"
	}
	return ""
}

// defaultTurnDeadline is the fallback uniform turn deadline when
// AskServiceOptions.TurnDeadline is unset (zero). Kept in sync with
// settings.DefaultAskTurnDeadline.
const defaultTurnDeadline = 10 * time.Minute

// TurnResult is the durable, user-visible answer projection of one Ask turn.
// HTTP and Slack deliberately deliver only the answer projection.
type TurnResult struct {
	Reply            string
	IncompleteReason IncompleteReason
	// AssistantTurn is the monotonic turn index of the persisted assistant
	// message. Slack uses it to associate answer chunks with the session.
	AssistantTurn int
	// Citations is the deduplicated corpus evidence accumulated by the
	// shared tool loop. Delivery surfaces may ignore it.
	Citations []Citation
	// Artifacts are the selected, durable panel images associated atomically
	// with the terminal assistant turn.
	Artifacts []ArtifactDescriptor
}

// TurnRequest is the transport-neutral boundary into the Ask state machine.
// HTTP and Slack ingress resolve their own authentication, session mapping,
// audience scopes, and optional thread context, then submit the same request
// here. Everything after this point is shared: locking, exact references,
// tool execution, recording, terminal validation, and persistence.
type TurnRequest struct {
	SessionID      uuid.UUID
	UserInput      string
	Surface        AskRunSurface
	Asker          *AskerInfo
	ThreadContext  string
	ContextThrough time.Time
	AllowedScopes  []string
	CorpusOnly     bool
	StatusEmitter  StatusEmitter
	ModelOverrides ModelOverrides
	modelSettings  EffectiveModelSettings
	// limits are the service's limits as the run started.
	limits Limits
}

// Limits are the Ask settings read as each run starts, so a change applies
// to the next question.
type Limits struct {
	// ReasoningEffort is the effort of every model call in the run; empty
	// is the model's default, as is an effort the model does not accept.
	ReasoningEffort string
	// MaxToolTurns is the tool rounds before a final answer is forced; zero
	// is the default.
	MaxToolTurns int
}

// AskService is the Eino-backed Ask turn orchestrator for one Ask model.
// Goroutine-safe: per-session exclusivity, across services sharing a
// Manager and across processes, is Manager.WithSessionLock's.
//
// ExecuteTurn contract:
//   - success: a provider-constrained final response is durable. Its JSON text
//     is projected to the user-visible answer before persistence, while response
//     metadata and selected artifacts remain attached to the assistant turn.
//   - error: no incomplete model/tool boundary is committed. Completed tool
//     batches from this run may already be durable (each batch commits
//     atomically through the after-tool-calls hook); the next interaction
//     reloads and replays that valid prefix.
type AskService struct {
	mgr             *Manager
	agents          *AskAgents
	promptRuntime   *PromptRuntime
	logger          *slog.Logger
	metrics         *Metrics
	limits          func() Limits
	references      ReferencePrerequisiteResolver
	linearTeams     LinearTeamProvider
	model           modelregistry.Model
	promptReleaseID int64
	serviceTier     string
	artifacts       *ArtifactStore
	// turnDeadline is the uniform wall-clock bound ExecuteTurn applies to
	// every surface. See its doc comment for why this exists.
	turnDeadline time.Duration

	// presentations is immutable after construction. Per-run status context
	// copies it again before parallel tool execution so callers cannot mutate
	// the registry observed by an in-flight turn.
	presentations map[string]ToolPresentation

	// orgName reads the organization's current name for each turn.
	orgName func() string

	// sources project the actors of a turn's citations.
	sources source.Infos
	// scopeGrants adds the scopes sources grant to a turn's scopes.
	scopeGrants ScopeGranter

	// now is the wall clock for formatContextEnvelope's per-turn
	// anchor; injectable for deterministic tests.
	now func() time.Time
}

// AskServiceOptions are the assembly knobs for AskService.
type AskServiceOptions struct {
	Manager *Manager
	Agents  *AskAgents
	// PromptRuntime is the production release resolver. When set, Agents is
	// ignored and each session is pinned to the active immutable release.
	PromptRuntime *PromptRuntime
	// Presentations is the Diderot-only status metadata returned beside the
	// native Eino tools by NewToolRegistry. It may be nil when no surface asks
	// for progress updates.
	Presentations map[string]ToolPresentation
	Logger        *slog.Logger
	Metrics       *Metrics
	// Limits returns Ask's limits; it is read as each run starts. Nil is
	// the model's default effort and the default tool turns.
	Limits func() Limits
	// References is the exact-corpus prerequisite seam. Production wires it
	// for HTTP and Slack.
	References ReferencePrerequisiteResolver
	// LinearTeams gates which bare issue keys in the user's turn are explicit
	// references. Required; the server shares one cache with MCP retrieve.
	LinearTeams LinearTeamProvider
	// Model is the Ask model's registry entry; request-level overrides are
	// validated against it and its ID is recorded on every run.
	// PromptReleaseID identifies fixed test/embedded configurations.
	// The server supplies PromptRuntime and records the resolved release ID.
	Model           modelregistry.Model
	PromptReleaseID int64
	ServiceTier     string
	Artifacts       *ArtifactStore
	// TurnDeadline bounds every ExecuteTurn call regardless of surface; zero
	// means defaultTurnDeadline. See ExecuteTurn's doc comment for why a
	// single cross-surface bound exists.
	TurnDeadline time.Duration
	// OrgName returns the organization's current name, read once per turn
	// to fill OrgNamePlaceholder in tool descriptions. Nil names none.
	OrgName func() string
	// Sources are the registered sources, which project the actors of a
	// turn's citations.
	Sources source.Infos
	// ScopeGrants adds the scopes sources grant to each turn's allowed
	// scopes; nil adds none.
	ScopeGrants ScopeGranter
	// Now injects the clock; zero value means time.Now.
	Now func() time.Time
}

// NewAskService validates its parts and constructs the service. It performs
// no I/O and no model requests; AskAgents was already constructed against
// its own model/tool set.
func NewAskService(opts AskServiceOptions) (*AskService, error) {
	if opts.Manager == nil {
		return nil, errors.New("ask service: manager is required")
	}
	if opts.PromptRuntime == nil {
		if opts.Agents == nil {
			return nil, errors.New("ask service: agents are required unless prompt runtime is configured")
		}
		if opts.Agents.HTTP == nil {
			return nil, errors.New("ask service: HTTP agent runner is required")
		}
		if opts.Agents.Slack == nil {
			return nil, errors.New("ask service: Slack agent runner is required")
		}
	}
	if opts.LinearTeams == nil {
		return nil, errors.New("ask service: linear team provider is required")
	}
	presentations := make(map[string]ToolPresentation, len(opts.Presentations))
	for name, presentation := range opts.Presentations {
		presentations[name] = presentation
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	limits := opts.Limits
	if limits == nil {
		limits = func() Limits { return Limits{} }
	}
	turnDeadline := opts.TurnDeadline
	if turnDeadline <= 0 {
		turnDeadline = defaultTurnDeadline
	}
	svc := &AskService{
		mgr:             opts.Manager,
		agents:          opts.Agents,
		promptRuntime:   opts.PromptRuntime,
		logger:          logger,
		metrics:         opts.Metrics,
		limits:          limits,
		references:      opts.References,
		linearTeams:     opts.LinearTeams,
		model:           opts.Model,
		promptReleaseID: opts.PromptReleaseID,
		serviceTier:     opts.ServiceTier,
		artifacts:       opts.Artifacts,
		turnDeadline:    turnDeadline,
		presentations:   presentations,
		orgName:         opts.OrgName,
		sources:         opts.Sources,
		scopeGrants:     opts.ScopeGrants,
		now:             opts.Now,
	}
	if svc.now == nil {
		svc.now = time.Now
	}
	return svc, nil
}

// CreateSession provisions a fresh ask_sessions row and returns its UUID.
// Convenience pass-through to Manager.Create so callers (e.g. the /v1/ask
// HTTP handler) never depend on the Manager directly.
func (s *AskService) CreateSession(ctx context.Context) (uuid.UUID, error) {
	sess, err := s.mgr.Create(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return sess.ID, nil
}

// ValidateModelOverrides lets transports reject a model-specific override
// before creating a session or starting any side effects.
func (s *AskService) ValidateModelOverrides(overrides ModelOverrides) error {
	return validateModelOverrides(s.model, overrides)
}

// CreateOwnedSession provisions a session with its verified owner in the same
// database statement. MCP uses this path because task execution is detached
// from the initiating HTTP request and must not rely on a later ownership
// update.
func (s *AskService) CreateOwnedSession(ctx context.Context, ownerEmail string) (uuid.UUID, error) {
	sess, err := s.mgr.CreateOwned(ctx, ownerEmail)
	if err != nil {
		return uuid.Nil, err
	}
	return sess.ID, nil
}

func (s *AskService) LookupOrCreateSlackSession(ctx context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error) {
	id, err := s.mgr.LoadOrCreateForSlackThread(ctx, channelID, threadTS, askerEmail)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *AskService) SlackSessionState(ctx context.Context, channelID, threadTS string) (bool, time.Time, error) {
	return s.mgr.SlackSessionState(ctx, channelID, threadTS)
}

func (s *AskService) LoadResultArtifacts(ctx context.Context, sessionID uuid.UUID, assistantTurn int) ([]ArtifactDescriptor, error) {
	if s.artifacts == nil {
		return nil, errors.New("ask artifact store is unavailable")
	}
	return s.artifacts.LoadResultArtifacts(ctx, sessionID, assistantTurn)
}

func (s *AskService) ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, []byte, error) {
	if s.artifacts == nil {
		return ArtifactRecord{}, nil, errors.New("ask artifact store is unavailable")
	}
	return s.artifacts.ReadArtifact(ctx, sessionID, artifactID)
}

func (s *AskService) ArtifactDeliveryExists(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target string) (bool, error) {
	if s.artifacts == nil {
		return false, errors.New("ask artifact store is unavailable")
	}
	return s.artifacts.DeliveryExists(ctx, sessionID, assistantTurn, artifactID, surface, target)
}

func (s *AskService) RecordArtifactDelivery(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target, externalID, status, failure string) error {
	if s.artifacts == nil {
		return errors.New("ask artifact store is unavailable")
	}
	return s.artifacts.RecordDelivery(ctx, sessionID, assistantTurn, artifactID, surface, target, externalID, status, failure)
}

// ExecuteTurn is the only production entrypoint into the Ask state machine.
//
// It applies a single uniform turn deadline here, before any surface-specific
// work, so all three surfaces are bounded identically: Slack already wraps
// its turn in a 15-minute context, but HTTP previously handed r.Context()
// (unbounded by request lifetime alone) straight through, and scheduled
// reports passed their worker context. context.WithTimeout keeps whichever
// deadline is sooner, so an outer caller deadline (e.g. Slack's 15m) still
// wins when it is shorter than the configured turn deadline.
func (s *AskService) ExecuteTurn(ctx context.Context, req TurnRequest) (result TurnResult, retErr error) {
	// The outer boundary: runTurnLocked's own recover (below) only covers
	// its own body, not this function's preamble (scopeGrants.GrantScopes,
	// the limits/orgName callbacks) or acc.finalize() after it returns.
	// Every surface -- HTTP, Slack, Discord, the MCP ask task -- shares
	// this one entry point, so a panic anywhere in it must still come
	// back as an ordinary error rather than crash whichever process is
	// serving the turn.
	defer worker.Recover(s.logger, "ask-turn-executor", func() {
		result = TurnResult{}
		retErr = fmt.Errorf("ask service: turn panicked; see server logs for the stack")
	})
	ctx, cancel := context.WithTimeout(ctx, s.turnDeadline)
	defer cancel()
	if req.SessionID == uuid.Nil {
		return TurnResult{}, errors.New("ask service: session ID is required")
	}
	switch req.Surface {
	case AskRunSurfaceHTTP, AskRunSurfacePagerDuty:
	case AskRunSurfaceSlack, AskRunSurfaceDiscord, AskRunSurfaceScheduled:
	default:
		return TurnResult{}, fmt.Errorf("ask service: unsupported surface %q", req.Surface)
	}
	if len(req.AllowedScopes) == 0 {
		return TurnResult{}, fmt.Errorf("ask: %s turn requires a non-empty allowedScopes set", req.Surface)
	}
	if s.scopeGrants != nil {
		req.AllowedScopes = s.scopeGrants.GrantScopes(ctx, req.AllowedScopes)
	}
	if req.ModelOverrides.ReasoningEffort != nil || req.ModelOverrides.ServiceTier != nil || req.ModelOverrides.ReasoningEffortSchedule != nil {
		if err := validateModelOverrides(s.model, req.ModelOverrides); err != nil {
			return TurnResult{}, fmt.Errorf("ask service: model overrides: %w", err)
		}
	}
	req.limits = s.runLimits()
	req.modelSettings = resolveModelSettings(EffectiveModelSettings{
		ReasoningEffort: req.limits.ReasoningEffort, ServiceTier: s.serviceTier,
	}, req.ModelOverrides)
	req.AllowedScopes = append([]string(nil), req.AllowedScopes...)
	if s.orgName != nil {
		ctx = withOrgName(ctx, s.orgName())
	}
	acc := &citationAccumulator{sources: s.sources}
	result, err := s.executeTurn(ctx, req, acc)
	if err != nil {
		return TurnResult{}, err
	}
	result.Citations = acc.finalize()
	return result, nil
}

// runLimits reads the limits for one run. The service's model may not be the
// one an effort was saved for (a change not yet applied, or a registry
// change), so an effort it does not accept falls back to its default.
func (s *AskService) runLimits() Limits {
	limits := s.limits()
	if s.model.ValidateReasoningEffort(limits.ReasoningEffort) != nil {
		limits.ReasoningEffort = ""
	}
	if limits.MaxToolTurns <= 0 {
		limits.MaxToolTurns = defaultMaxToolTurns
	}
	return limits
}

func (s *AskService) executeTurn(ctx context.Context, req TurnRequest, acc *citationAccumulator) (TurnResult, error) {
	started := time.Now()
	outcome := "error"
	defer func() {
		s.metrics.observeTurn(req.Surface, outcome, time.Since(started))
	}()
	if req.UserInput == "" {
		return TurnResult{}, errors.New("ask service: user input is required")
	}
	var result TurnResult
	outerErr := s.mgr.WithSessionLock(ctx, req.SessionID, func(lockCtx context.Context) error {
		var err error
		result, err = s.runTurnLocked(lockCtx, req, acc)
		return err
	})
	if outerErr != nil {
		if ending := turnEnding(ctx); ending != "" {
			outcome = ending
		}
		return TurnResult{}, outerErr
	}
	if result.IncompleteReason != "" {
		outcome = "incomplete"
	} else {
		outcome = "answered"
	}
	return result, nil
}

func (s *AskService) runTurnLocked(ctx context.Context, req TurnRequest, acc *citationAccumulator) (result TurnResult, retErr error) {
	start := s.now()
	telemetry := newRunTelemetryRecorder(req.SessionID, req.Surface, s.model.ID, req.modelSettings)
	ctx = withTelemetryWorkload(ctx, TelemetryWorkloadRoot)
	ctx = withModelOverrides(ctx, req.ModelOverrides)
	ctx = WithReasoningEffort(ctx, req.limits.ReasoningEffort)
	defer func() {
		outcome := "answered"
		runErr := retErr
		ending := turnEnding(ctx)
		switch {
		case retErr != nil && ending != "":
			// Whoever asked ended the turn: not a failure.
			outcome, runErr = ending, nil
		case retErr != nil:
			outcome = "error"
		case result.IncompleteReason != "":
			outcome = "incomplete"
		}
		snapshot := telemetry.Complete(outcome, runErr)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.mgr.RecordRunTelemetry(persistCtx, snapshot); err != nil {
			s.metrics.observePersistenceFailure(req.Surface, "run_telemetry")
			s.logger.Warn("ask: persist run telemetry failed", "session_id", req.SessionID, "run_id", snapshot.ID, "err", err)
		}
	}()
	// A panic anywhere in the rest of this turn (a bad type assertion in a
	// connector, malformed provider output, a nil tool result) must end
	// this one turn the way an ordinary failure does, not crash the
	// serving process -- every surface (HTTP, Slack, Discord, the MCP ask
	// task) already turns a returned error here into its own failure
	// path (a reply, a failed task, an HTTP 500). Deferred after the
	// telemetry recorder above so it runs FIRST on unwind, setting retErr
	// before telemetry reads it.
	defer worker.Recover(s.logger, "ask-turn-executor", func() {
		result = TurnResult{}
		retErr = fmt.Errorf("ask service: turn panicked; see server logs for the stack")
	})
	agents := s.agents
	releaseID := s.promptReleaseID
	bundle := embeddedAskPrompts
	if s.promptRuntime != nil {
		pinnedID, pinned, err := s.mgr.SessionPromptReleaseID(ctx, req.SessionID)
		if err != nil {
			return TurnResult{}, fmt.Errorf("ask service: resolve session prompt release: %w", err)
		}
		var release *promptcatalog.Release
		if pinned {
			release, err = s.promptRuntime.Get(ctx, pinnedID)
		} else {
			release, err = s.promptRuntime.Current(ctx)
			if err == nil {
				winningID, pinErr := s.mgr.PinOrGetSessionPromptRelease(ctx, req.SessionID, release.ID)
				if pinErr != nil {
					return TurnResult{}, fmt.Errorf("ask service: pin session prompt release: %w", pinErr)
				}
				if winningID != release.ID {
					release, err = s.promptRuntime.Get(ctx, winningID)
				}
			}
		}
		if err != nil {
			return TurnResult{}, fmt.Errorf("ask service: resolve prompt release: %w", err)
		}
		releaseID = release.ID
		bundle = release.Bundle.Render(promptcatalog.Values())
		agents, err = s.promptRuntime.Agents(ctx, bundle)
		if err != nil {
			return TurnResult{}, err
		}
	}
	if err := s.mgr.RecordSessionProvenance(ctx, req.SessionID, req.Surface, s.model.ID, releaseID, req.AllowedScopes); err != nil {
		return TurnResult{}, fmt.Errorf("ask service: record session provenance: %w", err)
	}
	ctx = withPromptBundle(ctx, bundle)
	ctx = withCitations(ctx, acc)
	ctx = withSourceCoverage(ctx)
	ctx = WithAllowedScopes(ctx, append([]string(nil), req.AllowedScopes...))
	answering, err := s.mgr.SessionConversations(ctx, req.SessionID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("ask service: resolve the conversation being answered: %w", err)
	}
	ctx = withConversationsBeingAnswered(ctx, answering)
	ctx = withCorpusOnly(ctx, req.CorpusOnly)
	ctx = withToolStatus(ctx, req.StatusEmitter, s.presentations)
	budget, err := newAgenticToolBudget(req.limits.MaxToolTurns)
	if err != nil {
		return TurnResult{}, err
	}
	ctx = withAgenticToolBudget(ctx, budget)
	forcedSynthesisObserved := false
	defer func() {
		if budget.forcedSynthesisStarted() && !forcedSynthesisObserved {
			s.metrics.observeForcedSynthesis(req.Surface, "error")
		}
	}()

	_, history, err := s.mgr.LoadAgenticSession(ctx, req.SessionID)
	if err != nil {
		s.metrics.observePersistenceFailure(req.Surface, "load_history")
		return TurnResult{}, fmt.Errorf("ask service: load history: %w", err)
	}

	// Exact next turn index: the persisted agentic history is the
	// authoritative high-water mark under the session lock. Recorder
	// and metadata both derive from this single value.
	nextTurn := 0
	if len(history) > 0 {
		nextTurn = history[len(history)-1].Turn + 1
	}
	telemetry.SetUserTurn(nextTurn)
	if s.artifacts != nil {
		history = s.artifacts.HydrateHistory(ctx, req.SessionID, history)
	}

	refs := parseExplicitReferences(ctx, s.linearTeams, req.UserInput)
	referenceState := ReferencePrerequisiteState{References: refs}
	if len(refs) > 0 && s.references == nil {
		// An explicit corpus handle must never silently become a semantic
		// question when the exact-reader prerequisite is absent.
		referenceState.PrimaryBlocked = true
	} else if len(refs) > 0 {
		resolved, resolveErr := s.references.PrepareReferences(ctx, refs, req.AllowedScopes)
		if resolveErr != nil {
			// Backend failures are fail-closed at the same prerequisite
			// boundary as missing/denied rows. The user-facing response is
			// deliberately identical; details stay in server logs.
			s.logger.Warn("ask: exact reference preparation failed; blocking generation", "err", resolveErr)
			referenceState = ReferencePrerequisiteState{
				References:     refs,
				PrimaryBlocked: true,
			}
		} else {
			referenceState = resolved
			if len(referenceState.References) == 0 {
				referenceState.References = refs
			}
		}
	}

	wrappedInput := formatContextEnvelope(req.Asker, start, req.ThreadContext, req.UserInput)
	if len(referenceState.Resolved) > 0 {
		wrappedInput += formatReferenceContext(referenceState.Resolved)
	}

	pendingUser := NewAgenticUserText(wrappedInput)
	recorder, err := NewAgenticRunRecorder(s.mgr, req.SessionID, nextTurn, pendingUser)
	if err != nil {
		return TurnResult{}, fmt.Errorf("ask service: build run recorder: %w", err)
	}
	if referenceState.HasUnavailablePrimary() {
		return s.commitReferenceUnavailable(ctx, recorder, req)
	}

	// Replay the persisted agentic history plus the current user input
	// through the shared state machine's output-format runner. Eino appends each model
	// output and each tool result into the run's working history and
	// feeds the accumulated messages into the next ChatModel call.
	runInput := make([]*schema.AgenticMessage, 0, len(history)+1)
	for _, stored := range history {
		runInput = append(runInput, stored.Message)
	}
	runInput = append(runInput, pendingUser)

	modelOptions := []model.Option{finalResponseModelOption()}
	modelOptions = append(modelOptions, modelOptionsForOverrides(req.ModelOverrides, nil)...)
	runMetadata := AskRunMetadata{SessionID: req.SessionID}
	meta, err := withAskRunMetadata(runMetadata, modelOptions...)
	if err != nil {
		return TurnResult{}, fmt.Errorf("ask service: run metadata: %w", err)
	}

	runCtx := withAgenticRunRecorder(ctx, recorder)
	runCtx = withArtifactSession(runCtx, req.SessionID)
	runCtx = withAskRunMetadataContext(runCtx, runMetadata)
	runner := agents.HTTP
	// A Discord thread is a chat thread like Slack's: the chat agent answers it.
	if req.Surface == AskRunSurfaceSlack || req.Surface == AskRunSurfaceDiscord || req.Surface == AskRunSurfaceScheduled || req.Surface == AskRunSurfacePagerDuty {
		runner = agents.Slack
	}
	iter := runner.Run(runCtx, runInput,
		meta,
		withAgenticRecorderAfterToolCalls(budget),
		adk.WithCallbacks(s.metrics.CallbackHandler(req.Surface, s.presentations), telemetry.CallbackHandler()),
	)

	// Drain the event stream, propagating Eino/model/hook errors. The
	// after-tool-calls hook commits completed tool batches while the
	// runner is still alive; committed batches stay durable even when a
	// later model call fails.
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
		_, getErr := event.Output.MessageOutput.GetMessage()
		if getErr != nil {
			if runErr == nil {
				runErr = getErr
			}
			continue
		}
	}
	if runErr != nil {
		if budget.forcedSynthesisStarted() && ctx.Err() == nil {
			result, fallbackErr := s.commitForcedSynthesisErrorFallback(ctx, recorder, req)
			if fallbackErr != nil {
				s.metrics.observeForcedSynthesis(req.Surface, "error")
			} else {
				s.metrics.observeForcedSynthesis(req.Surface, "fallback")
			}
			forcedSynthesisObserved = true
			return result, fallbackErr
		}
		return TurnResult{}, fmt.Errorf("ask service: Eino run: %w", runErr)
	}
	// Eino's outward event can contain the raw model output from before an
	// AfterModelRewriteState middleware rewrite. The recorder observes the
	// final rewritten state and is also the persistence source of truth, so use
	// its snapshot for both the returned reply and terminal validation.
	finalAssistant := recorder.PendingAssistantSnapshot()
	if finalAssistant == nil {
		return TurnResult{}, errors.New("ask service: run completed without an assistant message")
	}

	if len(AgenticMessageToolCalls(finalAssistant)) != 0 {
		return TurnResult{}, errors.New("ask service: final assistant message still carries tool calls")
	}
	incompleteReason := incompleteReasonForResponse(finalAssistant)
	if incompleteReason != "" {
		if _, err := recorder.CommitFinalAnswer(ctx, incompleteReason); err != nil {
			s.metrics.observePersistenceFailure(req.Surface, "final_answer")
			return TurnResult{}, fmt.Errorf("ask service: commit incomplete answer: %w", err)
		}
		return TurnResult{}, fmt.Errorf("ask service: model response incomplete: %s", incompleteReason)
	}
	payload, err := decodeFinalResponseMessage(finalAssistant)
	if err != nil {
		if budget.forcedSynthesisStarted() && ctx.Err() == nil {
			result, fallbackErr := s.commitForcedSynthesisErrorFallback(ctx, recorder, req)
			s.metrics.observeForcedSynthesis(req.Surface, "error")
			forcedSynthesisObserved = true
			return result, fallbackErr
		}
		return TurnResult{}, fmt.Errorf("ask service: invalid terminal response: %w", err)
	}
	if len(payload.Artifacts) > 0 {
		if s.artifacts == nil {
			return TurnResult{}, errors.New("ask service: artifact delivery is unavailable")
		}
		if err := s.artifacts.ValidateSelections(ctx, req.SessionID, payload.Artifacts); err != nil {
			return TurnResult{}, fmt.Errorf("ask service: validate terminal artifacts: %w", err)
		}
	}
	if err := recorder.ReplacePendingAssistantText(payload.Answer); err != nil {
		return TurnResult{}, fmt.Errorf("ask service: project terminal answer: %w", err)
	}
	if budget.forcedSynthesisStarted() {
		incompleteReason = IncompleteToolBudget
	}
	assistantTurn, err := recorder.CommitFinalAnswerWithArtifacts(ctx, incompleteReason, payload.Artifacts)
	if err != nil {
		s.metrics.observePersistenceFailure(req.Surface, "final_answer")
		return TurnResult{}, fmt.Errorf("ask service: commit final answer: %w", err)
	}
	if budget.forcedSynthesisStarted() {
		s.metrics.observeForcedSynthesis(req.Surface, "answered")
		forcedSynthesisObserved = true
	}
	result = TurnResult{Reply: payload.Answer, IncompleteReason: incompleteReason, AssistantTurn: assistantTurn}
	if s.artifacts != nil {
		result.Artifacts, err = s.artifacts.LoadResultArtifacts(ctx, req.SessionID, assistantTurn)
		if err != nil {
			return TurnResult{}, fmt.Errorf("ask service: load result artifacts: %w", err)
		}
	}
	s.persistCitations(ctx, req.Surface, req.SessionID, assistantTurn, payload.Answer, acc)
	s.advanceContextThrough(ctx, req.Surface, req.SessionID, req.ContextThrough)
	return result, nil
}

// commitReferenceUnavailable records a terminal, concise answer without
// starting Eino. Keeping this on the same recorder path preserves session
// history semantics while guaranteeing there is no semantic retrieval
// fallback after an exact primary reference is missing or denied.
func (s *AskService) commitReferenceUnavailable(
	ctx context.Context,
	recorder *AgenticRunRecorder,
	req TurnRequest,
) (TurnResult, error) {
	if err := recorder.ReplacePendingAssistantForFallback(NewAgenticAssistantText(ReferenceUnavailableText)); err != nil {
		return TurnResult{}, fmt.Errorf("ask service: install unavailable-reference answer: %w", err)
	}
	assistantTurn, err := recorder.CommitFinalAnswer(ctx, IncompleteReferenceUnavailable)
	if err != nil {
		s.metrics.observePersistenceFailure(req.Surface, "reference_unavailable")
		return TurnResult{}, fmt.Errorf("ask service: persist unavailable-reference answer: %w", err)
	}
	result := TurnResult{
		Reply:            ReferenceUnavailableText,
		IncompleteReason: IncompleteReferenceUnavailable,
		AssistantTurn:    assistantTurn,
	}
	s.advanceContextThrough(ctx, req.Surface, req.SessionID, req.ContextThrough)
	return result, nil
}

func formatReferenceContext(refs []ResolvedReference) string {
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<exact_references>\n")
	b.WriteString("The following corpus items were resolved by exact identifiers. They are authoritative context, not search results. Use each item only according to its role; do not infer a different item from semantic similarity.\n")
	for _, ref := range refs {
		b.WriteString("\n<reference role=\"")
		b.WriteString(string(ref.Reference.Role))
		b.WriteString("\" kind=\"")
		b.WriteString(string(ref.Reference.Kind))
		b.WriteString("\" source=\"")
		b.WriteString(ref.Source)
		if ref.Truncated {
			b.WriteString("\" truncated=\"true")
		}
		b.WriteString("\">\n")
		if ref.URL != "" {
			b.WriteString("URL: ")
			b.WriteString(ref.URL)
			b.WriteString("\n")
		}
		b.WriteString(ref.Content)
		b.WriteString("\n</reference>\n")
	}
	b.WriteString("</exact_references>\n")
	return b.String()
}

func (s *AskService) commitForcedSynthesisErrorFallback(
	ctx context.Context,
	recorder *AgenticRunRecorder,
	req TurnRequest,
) (TurnResult, error) {
	fallback := NewAgenticAssistantText(capErrorCannedText)
	if err := recorder.ReplacePendingAssistantForFallback(fallback); err != nil {
		return TurnResult{}, fmt.Errorf("ask service: install forced-synthesis error fallback: %w", err)
	}
	assistantTurn, err := recorder.CommitFinalAnswer(ctx, IncompleteSynthesisError)
	if err != nil {
		s.metrics.observePersistenceFailure(req.Surface, "forced_fallback")
		return TurnResult{}, fmt.Errorf("ask service: persist forced-synthesis error fallback: %w", err)
	}
	// The canned text remains a durable diagnostic, but it is not a terminal
	// response payload and therefore must not cross an HTTP or Slack delivery
	// boundary.
	_ = assistantTurn
	return TurnResult{}, errors.New("ask service: synthesis failed without a valid terminal response")
}

func (s *AskService) persistCitations(ctx context.Context, surface AskRunSurface, sessionID uuid.UUID, turn int, answer string, acc *citationAccumulator) {
	rows := resolveCitations(answer, acc)
	if len(rows) == 0 {
		return
	}
	if err := s.mgr.InsertCitations(ctx, sessionID, turn, rows); err != nil {
		s.metrics.observePersistenceFailure(surface, "citations")
		s.logger.Warn("ask: persist citations failed", "session_id", sessionID, "turn", turn, "err", err)
	}
}

func (s *AskService) advanceContextThrough(ctx context.Context, surface AskRunSurface, sessionID uuid.UUID, through time.Time) {
	if through.IsZero() {
		return
	}
	if err := s.mgr.AdvanceSlackContextThrough(ctx, sessionID, through); err != nil {
		s.metrics.observePersistenceFailure(surface, "context_through")
		s.logger.Warn("ask: failed to advance context_through; next turn may re-inject context",
			"session_id", sessionID, "through", through, "err", err)
	}
}
