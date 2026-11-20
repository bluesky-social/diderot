package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bluesky-social/diderot/internal/worker"
)

const (
	taskTTL                    = time.Hour
	taskPollInterval           = time.Second
	taskCancellationPollPeriod = time.Second
	taskCleanupPeriod          = 15 * time.Minute
	taskFinishInitialBackoff   = 100 * time.Millisecond
	taskFinishMaxBackoff       = 5 * time.Second
)

var errCorpusTaskNotFound = errors.New("corpus task not found")

type taskDescriptor struct {
	TaskID         string  `json:"taskId"`
	Status         string  `json:"status"`
	StatusMessage  string  `json:"statusMessage,omitempty"`
	CreatedAt      string  `json:"createdAt"`
	LastUpdatedAt  string  `json:"lastUpdatedAt"`
	TTLMS          *uint64 `json:"ttlMs"`
	PollIntervalMS *uint64 `json:"pollIntervalMs,omitempty"`
}

type createTaskResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskDescriptor
}

type getTaskResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskDescriptor
	Result *mcp.CallToolResult `json:"result,omitempty"`
	Error  *taskRPCError       `json:"error,omitempty"`
}

type taskAckResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

type taskRPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type taskParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}

type updateTaskParams struct {
	mcp.ParamsBase
	TaskID         string         `json:"taskId"`
	InputResponses map[string]any `json:"inputResponses"`
}

type storedCorpusTask struct {
	owner      string
	descriptor taskDescriptor
	expiresAt  time.Time
	result     *mcp.CallToolResult
	rpcError   *taskRPCError
}

// corpusTaskBackend makes SEP-2663 task state independent of the stateless
// HTTP request and serving replica. Production uses Postgres; tests use the
// in-memory implementation below.
type corpusTaskBackend interface {
	Create(ctx context.Context, task *storedCorpusTask) error
	Get(ctx context.Context, owner, taskID string) (*storedCorpusTask, error)
	Finish(ctx context.Context, owner, taskID, status string, result *mcp.CallToolResult, rpcError *taskRPCError, at time.Time) error
	Cancel(ctx context.Context, owner, taskID string, at time.Time) error
	Delete(ctx context.Context, taskID string) error
	CleanupExpired(ctx context.Context) error
}

type corpusTaskStore struct {
	mu      sync.Mutex
	cancels map[string]context.CancelCauseFunc
	backend corpusTaskBackend
	runner  CorpusAskRunnerProvider
	now     func() time.Time
}

func newCorpusTaskStore(backend corpusTaskBackend, runner CorpusAskRunnerProvider, lifecycle context.Context) *corpusTaskStore {
	store := &corpusTaskStore{
		cancels: make(map[string]context.CancelCauseFunc), backend: backend, runner: runner, now: time.Now,
	}
	go store.cleanupLoop(lifecycle)
	return store
}

func (s *corpusTaskStore) attach(server *mcp.Server) error {
	server.AddReceivingMiddleware(s.middleware)
	if err := mcp.AddReceivingCustomMethod(server, "tasks/get", s.get); err != nil {
		return err
	}
	if err := mcp.AddReceivingCustomMethod(server, "tasks/update", s.update); err != nil {
		return err
	}
	return mcp.AddReceivingCustomMethod(server, "tasks/cancel", s.cancel)
}

func (s *corpusTaskStore) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}
		req, ok := request.(*mcp.CallToolRequest)
		if !ok || req.Params.Name != "ask" {
			return next(ctx, method, request)
		}
		owner, err := mcpCaller(ctx)
		if err != nil {
			return toolError(err), nil
		}
		if notice := MCPAskBlocked(ctx); notice != "" {
			return toolError(errors.New(notice)), nil
		}
		if !supportsTasks(req.ClientCapabilities()) {
			return nil, invalidParams("ask requires the io.modelcontextprotocol/tasks client extension")
		}
		var input askMCPInput
		if err := decodeMCPArguments(req, &input); err != nil {
			return toolError(fmt.Errorf("invalid ask arguments: %w", err)), nil
		}
		if strings.TrimSpace(input.Question) == "" {
			return toolError(errors.New("question is required")), nil
		}
		if s.runner == nil {
			return toolError(ErrUnavailable), nil
		}
		lease := s.runner()
		if lease.Runner == nil || lease.Context == nil || lease.Context.Err() != nil {
			return toolError(ErrUnavailable), nil
		}
		// Ownership is settled before the task exists, so a session the
		// caller may not continue is refused at once and never runs.
		sessionID, err := askSessionForMCP(ctx, lease.Runner, owner, input.SessionID)
		if err != nil {
			return toolError(err), nil
		}
		release, ok := lease.Runner.Hold()
		if !ok {
			return toolError(ErrUnavailable), nil
		}
		// create hands the hold off to the task's own background goroutine
		// (which releases it once the task's result is recorded) only once
		// it has launched that goroutine. A panic anywhere in create
		// before then -- recovered by recoverMCPMiddleware, outside this
		// function -- must not leak the hold, so release it here unless
		// hand-off is confirmed.
		handedOff := false
		defer func() {
			if !handedOff {
				release()
			}
		}()
		created, err := s.create(ctx, lease, release, owner, scopesFromContext(ctx), sessionID, input)
		if err != nil {
			return nil, err
		}
		handedOff = true
		return created, nil
	}
}

// create records a task and runs it in the background, holding the runner
// until the task's result is recorded.
func (s *corpusTaskStore) create(requestCtx context.Context, lease CorpusAskLease, release func(), owner string, scopes []string, sessionID uuid.UUID, input askMCPInput) (*createTaskResult, error) {
	now := s.now().UTC()
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	task := &storedCorpusTask{
		owner: owner, expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: uuid.NewString(), Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}
	if err := s.backend.Create(requestCtx, task); err != nil {
		return nil, fmt.Errorf("create ask task: %w", err)
	}

	// The turn's context: tasks/cancel ends it with ErrTurnCancelled.
	ctx, cancel := context.WithCancelCause(detachedMCPContext(lease.Context, owner, scopes))
	s.mu.Lock()
	s.cancels[task.descriptor.TaskID] = cancel
	s.mu.Unlock()
	time.AfterFunc(taskTTL, func() { s.expireRecovered(task.descriptor.TaskID) })
	go func() {
		// watchCancellation polls the task's own row (s.backend.Get) on a
		// separate goroutine from the turn it watches; a panic here is
		// outside the turn goroutine's recovery below. cancel(nil) ends
		// the turn the same way watchCancellation's own "not found" path
		// does, so run still reaches its ordinary completion/finish path
		// instead of leaking the turn's context forever.
		defer worker.Recover(slog.Default(), "mcp-task-cancellation-watch", func() { cancel(nil) })
		s.watchCancellation(ctx, owner, task.descriptor.TaskID, cancel)
	}()
	go func() {
		defer release()
		// A panic anywhere in the turn (run's own result shaping, or
		// anything runMCPAsk/ExecuteTurn didn't already turn into a
		// returned error) must end this one task as failed, not crash the
		// replica -- every other request, Slack turn, and task it is
		// serving goes down with it otherwise. s.run's own
		// `defer s.removeCancel(...)` never gets registered when
		// runMCPAsk itself panics (the defer statement is textually
		// after that call), so removeCancel is repeated here to be sure
		// the cancel func is never left in s.cancels.
		defer worker.Recover(slog.Default(), "mcp-ask-task", func() {
			s.removeCancel(task.descriptor.TaskID)
			now := s.now().UTC()
			rpcErr := &taskRPCError{Code: int64(jsonrpc.CodeInternalError), Message: "internal error running this task; retry ask"}
			s.finish(task.owner, task.descriptor.TaskID, "failed", nil, rpcErr, now, task.expiresAt)
		})
		s.run(ctx, lease.Runner, lease.Notifier, task, sessionID, input)
	}()
	return &createTaskResult{ResultType: "task", taskDescriptor: task.descriptor}, nil
}

func (s *corpusTaskStore) run(ctx context.Context, runner CorpusAskRunner, notifier AskInvocationNotifier, task *storedCorpusTask, sessionID uuid.UUID, input askMCPInput) {
	result, err := runMCPAsk(ctx, runner, notifier, task.owner, scopesFromContext(ctx), sessionID, input)
	now := s.now().UTC()
	defer s.removeCancel(task.descriptor.TaskID)
	if TurnCancelled(ctx) {
		// tasks/cancel has already recorded the task as cancelled.
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		interrupted := &taskRPCError{Code: int64(jsonrpc.CodeInternalError), Message: "task execution interrupted; retry ask"}
		s.finish(task.owner, task.descriptor.TaskID, "failed", nil, interrupted, now, task.expiresAt)
		return
	}
	if err != nil {
		rpcError := &taskRPCError{Code: int64(jsonrpc.CodeInternalError), Message: err.Error()}
		s.finish(task.owner, task.descriptor.TaskID, "failed", nil, rpcError, now, task.expiresAt)
		return
	}
	s.finish(task.owner, task.descriptor.TaskID, "completed", result, nil, now, task.expiresAt)
}

func (s *corpusTaskStore) finish(owner, taskID, status string, result *mcp.CallToolResult, rpcError *taskRPCError, at, expiresAt time.Time) {
	// Keep the terminal result in memory and retry for the task's full
	// remaining lifetime. The conditional update is idempotent, and after
	// expiresAt the result is no longer retrievable by the Tasks contract.
	backoff := taskFinishInitialBackoff
	for time.Now().Before(expiresAt) {
		attemptDeadline := time.Now().Add(5 * time.Second)
		if expiresAt.Before(attemptDeadline) {
			attemptDeadline = expiresAt
		}
		ctx, cancel := context.WithDeadline(context.Background(), attemptDeadline)
		err := s.backend.Finish(ctx, owner, taskID, status, result, rpcError, at)
		cancel()
		if err == nil {
			return
		}
		delay := backoff
		if remaining := time.Until(expiresAt); delay > remaining {
			delay = remaining
		}
		if delay <= 0 {
			return
		}
		timer := time.NewTimer(delay)
		<-timer.C
		if backoff < taskFinishMaxBackoff {
			backoff *= 2
			if backoff > taskFinishMaxBackoff {
				backoff = taskFinishMaxBackoff
			}
		}
	}
}

// watchCancellation ends a task's turn once its row says the task is over:
// cancelled through another replica, or expired.
func (s *corpusTaskStore) watchCancellation(ctx context.Context, owner, taskID string, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(taskCancellationPollPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, checkCancel := context.WithTimeout(context.Background(), taskCancellationPollPeriod)
			task, err := s.backend.Get(checkCtx, owner, taskID)
			checkCancel()
			switch {
			case err == nil && task.descriptor.Status == "cancelled":
				cancel(ErrTurnCancelled)
				return
			case errors.Is(err, errCorpusTaskNotFound) || (err == nil && task.descriptor.Status != "working"):
				cancel(nil)
				return
			}
		}
	}
}

func (s *corpusTaskStore) get(ctx context.Context, session *mcp.ServerSession, params *taskParams) (*getTaskResult, error) {
	owner, err := mcpCaller(ctx)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	if !supportsTaskParams(session, &params.ParamsBase) {
		return nil, invalidParams("tasks/get requires the io.modelcontextprotocol/tasks client extension")
	}
	task, err := s.backend.Get(ctx, owner, params.TaskID)
	if errors.Is(err, errCorpusTaskNotFound) {
		return nil, invalidParams("unknown task")
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return &getTaskResult{ResultType: "complete", taskDescriptor: task.descriptor, Result: task.result, Error: task.rpcError}, nil
}

func (s *corpusTaskStore) update(ctx context.Context, session *mcp.ServerSession, params *updateTaskParams) (*taskAckResult, error) {
	owner, err := mcpCaller(ctx)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	if !supportsTaskParams(session, &params.ParamsBase) {
		return nil, invalidParams("tasks/update requires the io.modelcontextprotocol/tasks client extension")
	}
	if _, err := s.backend.Get(ctx, owner, params.TaskID); errors.Is(err, errCorpusTaskNotFound) {
		return nil, invalidParams("unknown task")
	} else if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return nil, invalidParams("task has no outstanding input requests")
}

func (s *corpusTaskStore) cancel(ctx context.Context, session *mcp.ServerSession, params *taskParams) (*taskAckResult, error) {
	owner, err := mcpCaller(ctx)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	if !supportsTaskParams(session, &params.ParamsBase) {
		return nil, invalidParams("tasks/cancel requires the io.modelcontextprotocol/tasks client extension")
	}
	if err := s.backend.Cancel(ctx, owner, params.TaskID, s.now().UTC()); errors.Is(err, errCorpusTaskNotFound) {
		return nil, invalidParams("unknown task")
	} else if err != nil {
		return nil, fmt.Errorf("cancel task: %w", err)
	}
	s.mu.Lock()
	cancel := s.cancels[params.TaskID]
	s.mu.Unlock()
	if cancel != nil {
		cancel(ErrTurnCancelled)
	}
	return &taskAckResult{ResultType: "complete"}, nil
}

func (s *corpusTaskStore) expire(taskID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.backend.Delete(ctx, taskID)
	s.removeCancel(taskID)
}

// expireRecovered is time.AfterFunc's taskTTL callback (create, above):
// it runs once per MCP ask task, on every replica, on its own goroutine
// with nothing else on its stack to recover it.
func (s *corpusTaskStore) expireRecovered(taskID string) {
	defer worker.Recover(slog.Default(), "mcp-task-expire")
	s.expire(taskID)
}

func (s *corpusTaskStore) cleanupLoop(ctx context.Context) {
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = s.backend.CleanupExpired(cleanupCtx)
	}
	cleanup()
	ticker := time.NewTicker(taskCleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func (s *corpusTaskStore) removeCancel(taskID string) {
	s.mu.Lock()
	cancel := s.cancels[taskID]
	delete(s.cancels, taskID)
	s.mu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
}

func supportsTasks(caps *mcp.ClientCapabilities) bool {
	if caps == nil || caps.Extensions == nil {
		return false
	}
	_, ok := caps.Extensions[tasksExtensionID]
	return ok
}

func supportsTaskParams(session *mcp.ServerSession, params *mcp.ParamsBase) bool {
	if raw := params.GetMeta()[mcp.MetaKeyClientCapabilities]; raw != nil {
		var caps mcp.ClientCapabilities
		b, err := json.Marshal(raw)
		if err == nil && json.Unmarshal(b, &caps) == nil {
			return supportsTasks(&caps)
		}
	}
	if session != nil && session.InitializeParams() != nil {
		return supportsTasks(session.InitializeParams().Capabilities)
	}
	return false
}

func invalidParams(message string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: message}
}

// memoryCorpusTaskBackend keeps protocol tests hermetic. It mirrors the
// conditional terminal transitions used by the Postgres implementation.
type memoryCorpusTaskBackend struct {
	mu    sync.Mutex
	tasks map[string]*storedCorpusTask
}

func newMemoryCorpusTaskBackend() *memoryCorpusTaskBackend {
	return &memoryCorpusTaskBackend{tasks: make(map[string]*storedCorpusTask)}
}

func (b *memoryCorpusTaskBackend) Create(_ context.Context, task *storedCorpusTask) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tasks[task.descriptor.TaskID] = cloneStoredCorpusTask(task)
	return nil
}

func (b *memoryCorpusTaskBackend) Get(_ context.Context, owner, taskID string) (*storedCorpusTask, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.tasks[taskID]
	if !ok || task.owner != owner || !time.Now().Before(task.expiresAt) {
		return nil, errCorpusTaskNotFound
	}
	return cloneStoredCorpusTask(task), nil
}

func (b *memoryCorpusTaskBackend) Finish(_ context.Context, owner, taskID, status string, result *mcp.CallToolResult, rpcError *taskRPCError, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if task, ok := b.tasks[taskID]; ok && task.owner == owner && task.descriptor.Status == "working" && at.Before(task.expiresAt) {
		task.descriptor.Status = status
		task.descriptor.LastUpdatedAt = at.Format(time.RFC3339Nano)
		task.descriptor.PollIntervalMS = nil
		task.result = result
		task.rpcError = rpcError
	}
	return nil
}

func (b *memoryCorpusTaskBackend) Cancel(_ context.Context, owner, taskID string, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.tasks[taskID]
	if !ok || task.owner != owner || !at.Before(task.expiresAt) {
		return errCorpusTaskNotFound
	}
	if task.descriptor.Status == "working" {
		task.descriptor.Status = "cancelled"
		task.descriptor.StatusMessage = "cancelled by client"
		task.descriptor.LastUpdatedAt = at.Format(time.RFC3339Nano)
		task.descriptor.PollIntervalMS = nil
	}
	return nil
}

func (b *memoryCorpusTaskBackend) Delete(_ context.Context, taskID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tasks, taskID)
	return nil
}

func (b *memoryCorpusTaskBackend) CleanupExpired(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for id, task := range b.tasks {
		if !now.Before(task.expiresAt) {
			delete(b.tasks, id)
		}
	}
	return nil
}

func cloneStoredCorpusTask(task *storedCorpusTask) *storedCorpusTask {
	copy := *task
	return &copy
}
