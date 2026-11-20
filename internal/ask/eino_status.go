package ask

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type toolStatusRunContextKey struct{}

// toolStatusRun is scoped to one Ask execution. Eino can complete tools in
// parallel, while existing StatusEmitter implementations were designed around
// serial dispatch, so emit serializes callback entry without constraining tool
// execution itself.
type toolStatusRun struct {
	emitter       StatusEmitter
	presentations map[string]ToolPresentation
	emitMu        sync.Mutex
}

func withToolStatus(ctx context.Context, emitter StatusEmitter, presentations map[string]ToolPresentation) context.Context {
	if emitter == nil {
		return ctx
	}
	copied := make(map[string]ToolPresentation, len(presentations))
	for name, presentation := range presentations {
		copied[name] = presentation
	}
	return context.WithValue(ctx, toolStatusRunContextKey{}, &toolStatusRun{
		emitter:       emitter,
		presentations: copied,
	})
}

// withoutToolStatus detaches the user-visible status sink. A child run inherits
// the root's context, so clearing the sink is what makes "the child cannot write
// to the asker's transcript" a property of the context rather than of which
// middlewares a future edit happens to install. emit is nil-safe, so anything
// that does try becomes a no-op instead of a panic.
func withoutToolStatus(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolStatusRunContextKey{}, (*toolStatusRun)(nil))
}

func toolStatusRunFromContext(ctx context.Context) *toolStatusRun {
	run, _ := ctx.Value(toolStatusRunContextKey{}).(*toolStatusRun)
	return run
}

func (r *toolStatusRun) emit(update StatusUpdate) {
	if r == nil || r.emitter == nil {
		return
	}
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	r.emitter(update)
}

// agenticStatusMiddleware is stateless; all per-run presentation and emitter
// state is carried by context so the same agents remain safe for concurrent
// sessions.
type agenticStatusMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newAgenticStatusMiddleware() *agenticStatusMiddleware {
	return &agenticStatusMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

func (m *agenticStatusMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolContext *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	if endpoint == nil {
		return nil, errors.New("ask status: nil tool endpoint")
	}
	if toolContext == nil || toolContext.CallID == "" || toolContext.Name == "" {
		return nil, errors.New("ask status: incomplete tool context")
	}
	return func(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
		run := toolStatusRunFromContext(ctx)
		base := StatusUpdate{
			Phase:    "tool_use",
			ToolName: toolContext.Name,
			TaskID:   toolContext.CallID,
		}
		presentation, registered := ToolPresentation{}, false
		if run != nil {
			presentation, registered = run.presentations[toolContext.Name]
		}
		if registered && presentation.Status != nil {
			line := presentation.Status(json.RawMessage(arguments))
			base.Emoji = line.Emoji
			base.Verb = line.Verb
			base.Summary = line.Detail
		}
		pending := base
		pending.State = StatusPending
		run.emit(pending)
		inProgress := base
		inProgress.State = StatusInProgress
		run.emit(inProgress)

		result, err := endpoint(ctx, arguments, opts...)
		terminal := base
		if err != nil || IsRecoverableToolErrorPayload(result) {
			terminal.State = StatusError
			run.emit(terminal)
			return result, err
		}
		terminal.State = StatusComplete
		if registered && presentation.StatusOutput != nil {
			terminal.Output = presentation.StatusOutput(json.RawMessage(result))
		}
		if registered && presentation.StatusSources != nil {
			terminal.Sources = presentation.StatusSources(json.RawMessage(result))
		}
		run.emit(terminal)
		return result, nil
	}, nil
}

// IsRecoverableToolErrorPayload recognizes only the closed envelope produced
// by recoverableTool and UnknownToolResult: exactly one non-empty string
// "error" field. Successful tool outputs that happen to contain an error
// field alongside application data are not reclassified.
func IsRecoverableToolErrorPayload(result string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &object); err != nil || len(object) != 1 {
		return false
	}
	raw, ok := object["error"]
	if !ok {
		return false
	}
	var message string
	return json.Unmarshal(raw, &message) == nil && message != ""
}
