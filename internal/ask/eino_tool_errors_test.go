package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

type toolErrorInput struct {
	Query string `json:"query"`
}

type toolErrorOutput struct {
	OK bool `json:"ok"`
}

func newToolErrorFixture(t *testing.T, run func(context.Context, toolErrorInput) (toolErrorOutput, error)) tool.BaseTool {
	t.Helper()
	base, err := utils.InferTool("lookup", "Look something up", run)
	require.NoError(t, err)
	return base
}

func wrapOneTool(t *testing.T, base tool.BaseTool) tool.InvokableTool {
	t.Helper()
	wrapped, err := WrapRecoverableToolErrors([]tool.BaseTool{base}, RootToolResultMaxBytes)
	require.NoError(t, err)
	require.Len(t, wrapped, 1)
	invokable, ok := wrapped[0].(tool.InvokableTool)
	require.True(t, ok)
	return invokable
}

func TestRecoverableToolErrorBecomesModelVisibleJSON(t *testing.T) {
	t.Parallel()
	wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		return toolErrorOutput{}, errors.New("query is too broad")
	}))

	result, err := wrapped.InvokableRun(context.Background(), `{"query":"everything"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"error":"query is too broad"}`, result)
}

func TestRecoverableToolErrorPreservesApplicationContext(t *testing.T) {
	t.Parallel()
	wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		return toolErrorOutput{}, fmt.Errorf("corpus lookup: %w", errors.New("permission denied"))
	}))

	result, err := wrapped.InvokableRun(context.Background(), `{"query":"private"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"error":"corpus lookup: permission denied"}`, result)
}

func TestRecoverableToolMalformedArgumentsBecomeModelVisible(t *testing.T) {
	t.Parallel()
	wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		return toolErrorOutput{OK: true}, nil
	}))

	result, err := wrapped.InvokableRun(context.Background(), `{"query":`)
	require.NoError(t, err)
	var payload map[string]string
	require.NoError(t, json.Unmarshal([]byte(result), &payload))
	require.NotEmpty(t, payload["error"])
}

func TestRecoverableToolFatalErrorsStillAbort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: fmt.Errorf("lookup: %w", context.Canceled)},
		{name: "deadline", err: fmt.Errorf("lookup: %w", context.DeadlineExceeded)},
		{name: "invariant", err: fmt.Errorf("lookup state: %w", ErrToolInvariant)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
				return toolErrorOutput{}, tt.err
			}))
			_, err := wrapped.InvokableRun(context.Background(), `{"query":"q"}`)
			require.ErrorIs(t, err, tt.err)
		})
	}
}

func TestRecoverableToolCanceledContextWinsOverOrdinaryError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		return toolErrorOutput{}, errors.New("dependency stopped")
	}))

	_, err := wrapped.InvokableRun(ctx, `{"query":"q"}`)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRecoverableToolCanceledContextRejectsIgnoredCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	wrapped := wrapOneTool(t, newToolErrorFixture(t, func(context.Context, toolErrorInput) (toolErrorOutput, error) {
		called = true
		return toolErrorOutput{OK: true}, nil
	}))

	_, err := wrapped.InvokableRun(ctx, `{"query":"q"}`)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called, "a pre-canceled context must not invoke the tool")
}

func TestWrapRecoverableToolErrorsRejectsInvalidShapes(t *testing.T) {
	t.Parallel()
	_, err := WrapRecoverableToolErrors([]tool.BaseTool{nil}, RootToolResultMaxBytes)
	require.ErrorContains(t, err, "nil tool")

	_, err = WrapRecoverableToolErrors([]tool.BaseTool{infoOnlyTool{}}, RootToolResultMaxBytes)
	require.ErrorContains(t, err, "not invokable")
}

type infoOnlyTool struct{}

func (infoOnlyTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "info_only", Desc: "not runnable"}, nil
}

func TestUnknownToolResultIsBoundedAndDoesNotEchoArguments(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("x", maxUnknownToolNameRunes+20)
	result, err := UnknownToolResult(context.Background(), name, `{"secret":"do not echo"}`)
	require.NoError(t, err)
	require.NotContains(t, result, "do not echo")
	require.Contains(t, result, "unknown tool")
	require.LessOrEqual(t, len([]rune(result)), maxUnknownToolNameRunes+100)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = UnknownToolResult(ctx, "missing", `{}`)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestUnknownToolHandlerRunsThroughAgenticToolsNode(t *testing.T) {
	t.Parallel()
	node, err := compose.NewAgenticToolsNode(context.Background(), &compose.ToolsNodeConfig{
		UnknownToolsHandler: UnknownToolResult,
	})
	require.NoError(t, err)

	results, err := node.Invoke(context.Background(), &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{
			CallID: "call_missing", Name: "hallucinated", Arguments: `{"private":"value"}`,
		})},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, schema.AgenticRoleTypeUser, results[0].Role)
	require.Len(t, results[0].ContentBlocks, 1)
	toolResult := results[0].ContentBlocks[0].FunctionToolResult
	require.NotNil(t, toolResult)
	require.Equal(t, "call_missing", toolResult.CallID)
	require.Equal(t, "hallucinated", toolResult.Name)
	require.Len(t, toolResult.Content, 1)
	require.Contains(t, toolResult.Content[0].Text.Text, "unknown tool")
	require.NotContains(t, toolResult.Content[0].Text.Text, "private")
}
