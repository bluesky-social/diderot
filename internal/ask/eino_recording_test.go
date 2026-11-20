package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAgenticRecordingMiddlewarePersistsRealParallelRun(t *testing.T) {
	t.Parallel()

	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		switch modelCalls.Add(1) {
		case 1:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_tools", "object": "response", "created_at": 1,
				"status": "completed", "model": "test", "parallel_tool_calls": true,
				"output": []any{
					map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "lookup_a", "arguments": `{"value":"a"}`, "status": "completed"},
					map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "lookup_b", "arguments": `{"value":"b"}`, "status": "completed"},
				},
				"usage": responsesUsage(10, 3, 1),
			})
		case 2:
			writeResponsesFixture(t, w, map[string]any{
				"id": "resp_final", "object": "response", "created_at": 2,
				"status": "completed", "model": "test", "parallel_tool_calls": true,
				"output": []any{map[string]any{
					"type": "message", "id": "msg_final", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}},
				}},
				"usage": responsesUsage(20, 4, 5),
			})
		default:
			http.Error(w, "unexpected model call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	type input struct {
		Value string `json:"value"`
	}
	type output struct {
		Value string `json:"value"`
	}
	allStarted := make(chan struct{})
	var started atomic.Int32
	var once sync.Once
	newLookup := func(name string) tool.BaseTool {
		base, err := utils.InferTool(name, "parallel lookup", func(ctx context.Context, in input) (output, error) {
			if started.Add(1) == 2 {
				once.Do(func() { close(allStarted) })
			}
			select {
			case <-allStarted:
			case <-ctx.Done():
				return output{}, ctx.Err()
			}
			return output{Value: in.Value + "-done"}, nil
		})
		require.NoError(t, err)
		return base
	}

	agents, err := NewAskAgents(context.Background(), newAskAgentsTestModel(t, server.URL+"/v1"),
		[]tool.BaseTool{newLookup("lookup_a"), newLookup("lookup_b")}, "use both tools", 4)
	require.NoError(t, err)

	appender := &fakeAgenticAppender{}
	recorder, err := newAgenticRunRecorder(uuid.New(), 0, NewAgenticUserText("look up both"), appender.append)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(withAgenticRunRecorder(context.Background(), recorder), 5*time.Second)
	defer cancel()
	iter := agents.HTTP.Query(ctx, "look up both", withAgenticRecorderAfterToolCalls())
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, event.Err)
	}
	require.Equal(t, int32(2), started.Load())
	require.Equal(t, int32(2), modelCalls.Load())
	assistantTurn, err := recorder.CommitFinalAnswer(ctx, "")
	require.NoError(t, err)
	require.Equal(t, 4, assistantTurn)

	_, success := appender.snapshot()
	require.Len(t, success, 2)
	require.Equal(t, []int{0, 1, 2, 3}, storedTurns(success[0]))
	require.Equal(t, "lookup_a", AgenticMessageToolResults(success[0][2].Message)[0].Name)
	require.Equal(t, "lookup_b", AgenticMessageToolResults(success[0][3].Message)[0].Name)
	require.Equal(t, []int{4}, storedTurns(success[1]))
	require.Equal(t, "done", AgenticMessageText(success[1][0].Message))
}

func TestRecordingUnknownToolResultCapturesRepeatedNames(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(
		call("unknown_1", "hallucinated"), call("unknown_2", "hallucinated"),
	)))
	ctx := withAgenticRunRecorder(context.Background(), recorder)

	type unknownResult struct {
		text string
		err  error
	}
	results := make(chan unknownResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			result, err := recordingUnknownToolResult(ctx, "hallucinated", `{"secret":"not echoed"}`)
			results <- unknownResult{text: result, err: err}
		}()
	}
	for i := 0; i < 2; i++ {
		result := <-results
		require.NoError(t, result.err)
		require.NotContains(t, result.text, "not echoed")
	}
	require.True(t, recorder.BatchComplete())

	opt := withAgenticRecorderAfterToolCalls()
	// Exercise the actual hook through Eino in the integration test above;
	// here the recorder commit proves repeated-name assignment was atomic.
	require.NotNil(t, opt)
	require.NoError(t, recorder.CommitToolBatch(ctx))
	_, success := appender.snapshot()
	require.Len(t, success, 1)
	require.Equal(t, "unknown_1", AgenticMessageToolResults(success[0][2].Message)[0].CallID)
	require.Equal(t, "unknown_2", AgenticMessageToolResults(success[0][3].Message)[0].CallID)
}

func TestAgenticRecordingMiddlewareDoesNotCaptureFatalToolError(t *testing.T) {
	recorder, _, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c1", "lookup"))))
	ctx := withAgenticRunRecorder(context.Background(), recorder)
	handler := newAgenticRecordingMiddleware()
	wrapper, err := handler.WrapInvokableToolCall(ctx,
		func(context.Context, string, ...tool.Option) (string, error) {
			return "partial", errors.New("fatal")
		},
		&adk.ToolContext{Name: "lookup", CallID: "c1"},
	)
	require.NoError(t, err)
	result, err := wrapper(ctx, `{}`)
	require.Equal(t, "partial", result)
	require.Error(t, err)
	require.False(t, recorder.BatchComplete())
}
