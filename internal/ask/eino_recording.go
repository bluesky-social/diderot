package ask

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type agenticRunRecorderContextKey struct{}

func withAgenticRunRecorder(ctx context.Context, recorder *AgenticRunRecorder) context.Context {
	return context.WithValue(ctx, agenticRunRecorderContextKey{}, recorder)
}

func agenticRunRecorderFromContext(ctx context.Context) *AgenticRunRecorder {
	recorder, _ := ctx.Value(agenticRunRecorderContextKey{}).(*AgenticRunRecorder)
	return recorder
}

// agenticRecordingMiddleware is stateless. One instance may be shared by all
// surface agents and concurrent runs; each run's recorder lives in context.
type agenticRecordingMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
}

func newAgenticRecordingMiddleware() *agenticRecordingMiddleware {
	return &agenticRecordingMiddleware{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
	}
}

// AfterModelRewriteState observes Eino's completed, non-streaming model output
// after it has been appended to agent state. The response is captured in
// memory; completed-boundary hooks decide when it is durable.
func (m *agenticRecordingMiddleware) AfterModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.AgenticMessage],
	modelContext *adk.TypedModelContext[*schema.AgenticMessage],
) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
		if state == nil || len(state.Messages) == 0 {
			return ctx, state, errors.New("ask recording: model completed without a state message")
		}
		latest := state.Messages[len(state.Messages)-1]
		if err := recorder.CaptureAssistant(latest); err != nil {
			return ctx, state, fmt.Errorf("ask recording: capture assistant: %w", err)
		}
	}
	return m.TypedBaseChatModelAgentMiddleware.AfterModelRewriteState(ctx, state, modelContext)
}

// WrapInvokableToolCall captures the exact string result Eino will turn into
// a function_tool_result. Fatal tool errors are not captured because Eino
// aborts the run and no completed tool batch exists.
func (m *agenticRecordingMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolContext *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	if endpoint == nil {
		return nil, errors.New("ask recording: nil tool endpoint")
	}
	if toolContext == nil || toolContext.CallID == "" || toolContext.Name == "" {
		return nil, errors.New("ask recording: incomplete tool context")
	}
	return func(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
		result, err := endpoint(ctx, arguments, opts...)
		if err != nil {
			return result, err
		}
		if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
			if err := recorder.CaptureToolResult(newAgenticToolResult(toolContext.CallID, toolContext.Name, result)); err != nil {
				return result, fmt.Errorf("ask recording: capture tool result: %w", err)
			}
		}
		return result, nil
	}, nil
}

// WrapEnhancedInvokableToolCall lets the model consume the full multimodal
// result while persisting only its text reference. Panel PNG bytes are already
// durable in ArtifactStore; storing base64 in ask_messages would multiply DB
// size and make replay depend on an incidental encoding.
func (m *agenticRecordingMiddleware) WrapEnhancedInvokableToolCall(
	_ context.Context,
	endpoint adk.EnhancedInvokableToolCallEndpoint,
	toolContext *adk.ToolContext,
) (adk.EnhancedInvokableToolCallEndpoint, error) {
	if endpoint == nil {
		return nil, errors.New("ask recording: nil enhanced tool endpoint")
	}
	if toolContext == nil || toolContext.CallID == "" || toolContext.Name == "" {
		return nil, errors.New("ask recording: incomplete enhanced tool context")
	}
	return func(ctx context.Context, argument *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		result, err := endpoint(ctx, argument, opts...)
		if err != nil {
			return result, err
		}
		persisted, err := newAgenticEnhancedToolResult(toolContext.CallID, toolContext.Name, result)
		if err != nil {
			return result, fmt.Errorf("ask recording: project enhanced tool result: %w", err)
		}
		if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
			if err := recorder.CaptureToolResult(persisted); err != nil {
				return result, fmt.Errorf("ask recording: capture enhanced tool result: %w", err)
			}
		}
		return result, nil
	}, nil
}

func newAgenticEnhancedToolResult(callID, name string, result *schema.ToolResult) (*schema.AgenticMessage, error) {
	if result == nil {
		return nil, errors.New("nil enhanced tool result")
	}
	content := make([]*schema.FunctionToolResultContentBlock, 0, len(result.Parts))
	for _, part := range result.Parts {
		if part.Type != schema.ToolPartTypeText {
			continue
		}
		content = append(content, &schema.FunctionToolResultContentBlock{
			Type: schema.FunctionToolResultContentBlockTypeText,
			Text: &schema.UserInputText{Text: part.Text},
		})
	}
	if len(content) == 0 {
		return nil, errors.New("enhanced tool result has no durable text part")
	}
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{
			CallID: callID, Name: name, Content: content,
		})},
	}, nil
}

func newAgenticToolResult(callID, name, text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{
			CallID: callID,
			Name:   name,
			Content: []*schema.FunctionToolResultContentBlock{{
				Type: schema.FunctionToolResultContentBlockTypeText,
				Text: &schema.UserInputText{Text: text},
			}},
		})},
	}
}

// recordingUnknownToolResult preserves Eino's model-visible unknown-tool
// policy while feeding the same result into the completed-batch recorder.
func recordingUnknownToolResult(ctx context.Context, name, arguments string) (string, error) {
	result, err := UnknownToolResult(ctx, name, arguments)
	if err != nil {
		return result, err
	}
	if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
		if err := captureUnknownToolResult(recorder, name, result); err != nil {
			return result, fmt.Errorf("ask recording: capture unknown-tool result: %w", err)
		}
	}
	return result, nil
}

// captureUnknownToolResult assigns the handler output to the first unmatched
// pending call with that name. The unknown-tool callback omits call ID; doing
// lookup and capture under the recorder mutex keeps parallel repeated names
// exact. Their model-visible output is identical and does not echo arguments.
func captureUnknownToolResult(recorder *AgenticRunRecorder, name, result string) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.mutableLocked(); err != nil {
		return err
	}
	if recorder.pendingAssistant == nil {
		return errors.New("no assistant tool-call message is pending")
	}
	for _, call := range AgenticMessageToolCalls(recorder.pendingAssistant) {
		if call.Name != name {
			continue
		}
		if _, exists := recorder.pendingResults[call.CallID]; exists {
			continue
		}
		recorder.pendingResults[call.CallID] = newAgenticToolResult(call.CallID, name, result)
		return nil
	}
	return fmt.Errorf("no unmatched pending call for unknown tool %q", name)
}

func withAgenticRecorderAfterToolCalls(budgets ...*agenticToolBudget) adk.AgentRunOption {
	return adk.WithAfterToolCallsHook(func(ctx context.Context) error {
		if len(budgets) > 1 {
			return errors.New("ask recording: multiple tool budgets")
		}
		if recorder := agenticRunRecorderFromContext(ctx); recorder != nil {
			if err := recorder.CommitToolBatch(ctx); err != nil {
				return fmt.Errorf("ask recording: persist completed tool batch: %w", err)
			}
		}
		if len(budgets) == 1 && budgets[0] != nil {
			if err := budgets[0].recordCompletedRound(); err != nil {
				return fmt.Errorf("ask recording: record completed tool round: %w", err)
			}
		}
		toolStatusRunFromContext(ctx).emit(StatusUpdate{Phase: "synthesizing"})
		return nil
	})
}
