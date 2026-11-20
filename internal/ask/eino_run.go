package ask

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

type agenticAppendFunc func(context.Context, uuid.UUID, []StoredMessage) error
type agenticFinalAppendFunc func(context.Context, uuid.UUID, []StoredMessage, int, []ArtifactSelection) error

// AgenticRunRecorder owns the durable boundary for one Eino Ask run. Model
// output and parallel tool results are captured in memory; only a complete
// tool-call/result batch or a final assistant answer is appended. A recorder
// is safe for concurrent tool-result capture.
//
// The mutex intentionally remains held during AppendAgenticMessages. That
// serializes commit attempts with capture and prevents two goroutines from
// appending the same pending boundary. Any append error terminally fails this
// recorder: a database commit error can be ambiguous, so retrying in-process
// could duplicate a batch that actually committed. Previously committed
// batches remain durable and a later run repairs/reloads the persisted prefix.
type AgenticRunRecorder struct {
	mu sync.Mutex

	sessionID   uuid.UUID
	nextTurn    int
	append      agenticAppendFunc
	appendFinal agenticFinalAppendFunc

	pendingUser      *schema.AgenticMessage
	pendingAssistant *schema.AgenticMessage
	pendingResults   map[string]*schema.AgenticMessage

	finished bool
	failed   error
}

// NewAgenticRunRecorder constructs a production recorder backed by Manager.
func NewAgenticRunRecorder(manager *Manager, sessionID uuid.UUID, nextTurn int, pendingUser *schema.AgenticMessage) (*AgenticRunRecorder, error) {
	if manager == nil {
		return nil, errors.New("ask run recorder: manager is required")
	}
	recorder, err := newAgenticRunRecorder(sessionID, nextTurn, pendingUser, manager.AppendAgenticMessages)
	if err != nil {
		return nil, err
	}
	recorder.appendFinal = manager.AppendAgenticMessagesWithArtifacts
	return recorder, nil
}

// newAgenticRunRecorder exposes only the append seam needed by offline tests;
// production code always uses the concrete Manager constructor above.
func newAgenticRunRecorder(sessionID uuid.UUID, nextTurn int, pendingUser *schema.AgenticMessage, appendFn agenticAppendFunc) (*AgenticRunRecorder, error) {
	if nextTurn < 0 {
		return nil, errors.New("ask run recorder: next turn must be non-negative")
	}
	if appendFn == nil {
		return nil, errors.New("ask run recorder: append function is required")
	}
	if err := validateRecorderMessage(pendingUser, schema.AgenticRoleTypeUser, "pending user"); err != nil {
		return nil, err
	}
	if len(AgenticMessageToolResults(pendingUser)) != 0 {
		return nil, errors.New("ask run recorder: pending user must be a user-input message")
	}
	return &AgenticRunRecorder{
		sessionID: sessionID,
		nextTurn:  nextTurn,
		append:    appendFn,
		appendFinal: func(ctx context.Context, sessionID uuid.UUID, msgs []StoredMessage, _ int, _ []ArtifactSelection) error {
			return appendFn(ctx, sessionID, msgs)
		},
		pendingUser:    pendingUser,
		pendingResults: make(map[string]*schema.AgenticMessage),
	}, nil
}

func validateRecorderMessage(msg *schema.AgenticMessage, role schema.AgenticRoleType, label string) error {
	if msg == nil {
		return fmt.Errorf("ask run recorder: %s message is required", label)
	}
	if msg.Role != role {
		return fmt.Errorf("ask run recorder: %s message must have %s role", label, role)
	}
	// Eino assigns a run-local message ID before middleware observes model
	// output and tool results. The row codec strips exactly that private key
	// on persistence; validate the same non-mutating projection here.
	if err := ValidateAgenticMessage(agenticMessageForPersistence(msg)); err != nil {
		return fmt.Errorf("ask run recorder: %s message: %w", label, err)
	}
	return nil
}

// CaptureAssistant records one complete model output. It does not write; the
// appropriate completed-boundary method performs the append.
func (r *AgenticRunRecorder) CaptureAssistant(msg *schema.AgenticMessage) error {
	if err := validateRecorderMessage(msg, schema.AgenticRoleTypeAssistant, "assistant"); err != nil {
		return err
	}
	calls := AgenticMessageToolCalls(msg)
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if _, exists := seen[call.CallID]; exists {
			return fmt.Errorf("ask run recorder: duplicate assistant call ID %q", call.CallID)
		}
		seen[call.CallID] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	if r.pendingAssistant != nil {
		return errors.New("ask run recorder: assistant message already pending")
	}
	r.pendingAssistant = msg
	r.pendingResults = make(map[string]*schema.AgenticMessage, len(calls))
	return nil
}

// ReplacePendingAssistantForFallback installs a local, tool-free terminal
// answer even when a failed model/middleware path already left an uncommitted
// assistant tail. It never alters a committed batch. A recorder with an
// ambiguous append failure remains terminally failed through mutableLocked.
func (r *AgenticRunRecorder) ReplacePendingAssistantForFallback(msg *schema.AgenticMessage) error {
	if err := validateRecorderMessage(msg, schema.AgenticRoleTypeAssistant, "fallback assistant"); err != nil {
		return err
	}
	if len(AgenticMessageToolCalls(msg)) != 0 {
		return errors.New("ask run recorder: fallback assistant must not contain tool calls")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	r.pendingAssistant = msg
	r.pendingResults = make(map[string]*schema.AgenticMessage)
	return nil
}

// ReplacePendingAssistantText projects a provider-constrained terminal JSON
// message to its user-visible answer without discarding reasoning blocks,
// response IDs, token usage, or other response metadata.
func (r *AgenticRunRecorder) ReplacePendingAssistantText(answer string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	if r.pendingAssistant == nil {
		return errors.New("ask run recorder: no assistant message is pending")
	}
	if len(AgenticMessageToolCalls(r.pendingAssistant)) != 0 {
		return errors.New("ask run recorder: pending assistant still has tool calls")
	}

	projected := *r.pendingAssistant
	projected.ContentBlocks = make([]*schema.ContentBlock, len(r.pendingAssistant.ContentBlocks))
	textBlocks := 0
	for i, block := range r.pendingAssistant.ContentBlocks {
		if block == nil {
			continue
		}
		clonedBlock := *block
		if block.AssistantGenText != nil {
			textBlocks++
			clonedText := *block.AssistantGenText
			clonedText.Text = answer
			if block.AssistantGenText.OpenAIExtension != nil {
				clonedExtension := *block.AssistantGenText.OpenAIExtension
				clonedExtension.Annotations = nil
				clonedText.OpenAIExtension = &clonedExtension
			}
			clonedBlock.AssistantGenText = &clonedText
		}
		projected.ContentBlocks[i] = &clonedBlock
	}
	if textBlocks != 1 {
		return fmt.Errorf("ask run recorder: terminal assistant has %d text blocks; want 1", textBlocks)
	}
	r.pendingAssistant = &projected
	return nil
}

// CaptureToolResult records one Eino result message. Parallel results may
// arrive in any order; duplicate, unknown, mismatched, or mixed-content rows
// are rejected without changing the completed batch.
func (r *AgenticRunRecorder) CaptureToolResult(msg *schema.AgenticMessage) error {
	if err := validateRecorderMessage(msg, schema.AgenticRoleTypeUser, "tool result"); err != nil {
		return err
	}
	results := AgenticMessageToolResults(msg)
	if len(msg.ContentBlocks) != 1 || len(results) != 1 {
		return errors.New("ask run recorder: tool result message must contain exactly one result block")
	}
	result := results[0]

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	if r.pendingAssistant == nil {
		return errors.New("ask run recorder: no assistant tool-call message is pending")
	}
	expected, ok := pendingCallNames(r.pendingAssistant)[result.CallID]
	if !ok {
		return fmt.Errorf("ask run recorder: result call ID %q is not pending", result.CallID)
	}
	if expected != result.Name {
		return fmt.Errorf("ask run recorder: result name %q does not match %q for call %q", result.Name, expected, result.CallID)
	}
	if _, exists := r.pendingResults[result.CallID]; exists {
		return fmt.Errorf("ask run recorder: duplicate result for call %q", result.CallID)
	}
	r.pendingResults[result.CallID] = msg
	return nil
}

func pendingCallNames(msg *schema.AgenticMessage) map[string]string {
	out := make(map[string]string)
	for _, call := range AgenticMessageToolCalls(msg) {
		out[call.CallID] = call.Name
	}
	return out
}

// BatchComplete reports whether every pending assistant call has one exact
// matching result. It never mutates recorder state.
func (r *AgenticRunRecorder) BatchComplete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batchCompleteLocked()
}

func (r *AgenticRunRecorder) batchCompleteLocked() bool {
	if r.pendingAssistant == nil {
		return false
	}
	calls := AgenticMessageToolCalls(r.pendingAssistant)
	if len(calls) == 0 || len(calls) != len(r.pendingResults) {
		return false
	}
	for _, call := range calls {
		msg, ok := r.pendingResults[call.CallID]
		if !ok {
			return false
		}
		results := AgenticMessageToolResults(msg)
		if len(results) != 1 || results[0].Name != call.Name {
			return false
		}
	}
	return true
}

// CommitToolBatch atomically appends the initial user when still pending, the
// assistant call message, and one result message per call in assistant order.
func (r *AgenticRunRecorder) CommitToolBatch(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	if r.pendingAssistant == nil {
		return errors.New("ask run recorder: no assistant tool-call message is pending")
	}
	if !r.batchCompleteLocked() {
		return errors.New("ask run recorder: tool batch is incomplete")
	}
	batch := r.toolBatchLocked()
	if err := r.append(ctx, r.sessionID, batch); err != nil {
		return r.failLocked("commit tool batch", err)
	}
	r.nextTurn += len(batch)
	r.pendingUser = nil
	r.pendingAssistant = nil
	r.pendingResults = make(map[string]*schema.AgenticMessage)
	return nil
}

func (r *AgenticRunRecorder) toolBatchLocked() []StoredMessage {
	calls := AgenticMessageToolCalls(r.pendingAssistant)
	batch := make([]StoredMessage, 0, len(calls)+2)
	turn := r.nextTurn
	if r.pendingUser != nil {
		batch = append(batch, StoredMessage{Turn: turn, Message: r.pendingUser})
		turn++
	}
	batch = append(batch, StoredMessage{Turn: turn, Message: r.pendingAssistant})
	turn++
	for _, call := range calls {
		batch = append(batch, StoredMessage{Turn: turn, Message: r.pendingResults[call.CallID]})
		turn++
	}
	return batch
}

// CommitFinalAnswer appends the initial user when still pending plus the
// captured tool-free assistant answer. It returns the persisted assistant
// turn for citation attribution and response metadata.
func (r *AgenticRunRecorder) CommitFinalAnswer(ctx context.Context, incompleteReason IncompleteReason) (int, error) {
	return r.CommitFinalAnswerWithArtifacts(ctx, incompleteReason, nil)
}

func (r *AgenticRunRecorder) CommitFinalAnswerWithArtifacts(ctx context.Context, incompleteReason IncompleteReason, selections []ArtifactSelection) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return 0, err
	}
	if r.pendingAssistant == nil {
		return 0, errors.New("ask run recorder: no assistant message is pending")
	}
	if len(AgenticMessageToolCalls(r.pendingAssistant)) != 0 {
		return 0, errors.New("ask run recorder: pending assistant still has tool calls")
	}
	turn := r.nextTurn
	batch := make([]StoredMessage, 0, 2)
	if r.pendingUser != nil {
		batch = append(batch, StoredMessage{Turn: turn, Message: r.pendingUser})
		turn++
	}
	assistantTurn := turn
	batch = append(batch, StoredMessage{
		Turn: assistantTurn, Message: r.pendingAssistant, IncompleteReason: incompleteReason,
	})
	if err := r.appendFinal(ctx, r.sessionID, batch, assistantTurn, selections); err != nil {
		return 0, r.failLocked("commit final answer", err)
	}
	r.nextTurn += len(batch)
	r.pendingUser = nil
	r.pendingAssistant = nil
	r.pendingResults = nil
	r.finished = true
	return assistantTurn, nil
}

func (r *AgenticRunRecorder) mutableLocked() error {
	if r.failed != nil {
		return r.failed
	}
	if r.finished {
		return errors.New("ask run recorder: run already finished")
	}
	return nil
}

func (r *AgenticRunRecorder) failLocked(operation string, err error) error {
	r.failed = fmt.Errorf("ask run recorder: %s: %w", operation, err)
	return r.failed
}

func (r *AgenticRunRecorder) Finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

func (r *AgenticRunRecorder) NextTurnToCommit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nextTurn
}

// PendingAssistantSnapshot returns the exact assistant message captured after
// Eino middleware rewrites, before the completed boundary is committed. The
// service uses this rather than the outward AgentEvent, which may still carry
// a pre-rewrite model output. The returned frame is detached from recorder
// slice/map storage and is read-only to callers.
func (r *AgenticRunRecorder) PendingAssistantSnapshot() *schema.AgenticMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingAssistant == nil {
		return nil
	}
	cloned := *r.pendingAssistant
	cloned.ContentBlocks = append([]*schema.ContentBlock(nil), r.pendingAssistant.ContentBlocks...)
	if r.pendingAssistant.Extra != nil {
		cloned.Extra = make(map[string]any, len(r.pendingAssistant.Extra))
		for key, value := range r.pendingAssistant.Extra {
			cloned.Extra[key] = value
		}
	}
	return &cloned
}
