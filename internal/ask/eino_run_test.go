package ask

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	schemaopenai "github.com/cloudwego/eino/schema/openai"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeAgenticAppender struct {
	mu       sync.Mutex
	err      error
	attempts [][]StoredMessage
	success  [][]StoredMessage
}

func (f *fakeAgenticAppender) append(_ context.Context, _ uuid.UUID, msgs []StoredMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]StoredMessage(nil), msgs...)
	f.attempts = append(f.attempts, cp)
	if f.err != nil {
		return f.err
	}
	f.success = append(f.success, cp)
	return nil
}

func (f *fakeAgenticAppender) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeAgenticAppender) snapshot() (attempts, success [][]StoredMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]StoredMessage(nil), f.attempts...), append([][]StoredMessage(nil), f.success...)
}

func newRecorderTest(t *testing.T, nextTurn int) (*AgenticRunRecorder, *fakeAgenticAppender, *schema.AgenticMessage) {
	t.Helper()
	appender := &fakeAgenticAppender{}
	user := NewAgenticUserText("implement x")
	recorder, err := newAgenticRunRecorder(uuid.New(), nextTurn, user, appender.append)
	require.NoError(t, err)
	return recorder, appender, user
}

func assistantCallMsg(calls ...schema.FunctionToolCall) *schema.AgenticMessage {
	blocks := make([]*schema.ContentBlock, 0, len(calls))
	for i := range calls {
		call := calls[i]
		blocks = append(blocks, schema.NewContentBlock(&call))
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: blocks}
}

func resultMsg(callID, name, text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{
			CallID: callID,
			Name:   name,
			Content: []*schema.FunctionToolResultContentBlock{
				agenticTextToolResult(text),
			},
		})},
	}
}

func call(id, name string) schema.FunctionToolCall {
	return schema.FunctionToolCall{CallID: id, Name: name, Arguments: `{}`}
}

func TestAgenticRunRecorderFinalOnly(t *testing.T) {
	recorder, appender, user := newRecorderTest(t, 7)
	final := NewAgenticAssistantText("done")
	require.NoError(t, recorder.CaptureAssistant(final))
	assistantTurn, err := recorder.CommitFinalAnswer(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 8, assistantTurn)
	require.True(t, recorder.Finished())
	require.Equal(t, 9, recorder.NextTurnToCommit())

	_, success := appender.snapshot()
	require.Len(t, success, 1)
	require.Equal(t, []StoredMessage{
		{Turn: 7, Message: user},
		{Turn: 8, Message: final},
	}, success[0])
}

func TestAgenticRunRecorderSequentialToolBatchesThenFinal(t *testing.T) {
	recorder, appender, user := newRecorderTest(t, 7)

	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c1", "retrieve"))))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "hits")))
	require.NoError(t, recorder.CommitToolBatch(context.Background()))
	require.Equal(t, 10, recorder.NextTurnToCommit())

	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c2", "search"))))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c2", "search", "more")))
	require.NoError(t, recorder.CommitToolBatch(context.Background()))
	require.Equal(t, 12, recorder.NextTurnToCommit())

	require.NoError(t, recorder.CaptureAssistant(NewAgenticAssistantText("answer")))
	assistantTurn, err := recorder.CommitFinalAnswer(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 12, assistantTurn)
	require.Equal(t, 13, recorder.NextTurnToCommit())

	_, success := appender.snapshot()
	require.Len(t, success, 3)
	require.Len(t, success[0], 3)
	require.Same(t, user, success[0][0].Message)
	require.Equal(t, []int{7, 8, 9}, storedTurns(success[0]))
	require.Equal(t, []int{10, 11}, storedTurns(success[1]))
	require.Equal(t, []int{12}, storedTurns(success[2]))
}

func TestAgenticRunRecorderReplacesUncommittedAssistantWithFallback(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c1", "lookup"))))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "lookup", "result")))
	require.NoError(t, recorder.CommitToolBatch(context.Background()))

	// Simulate a forced-synthesis output captured before a later middleware or
	// event failure. Replacing this uncommitted tail must not touch the already
	// durable tool batch.
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c2", "lookup"))))
	fallback := NewAgenticAssistantText(capErrorCannedText)
	require.NoError(t, recorder.ReplacePendingAssistantForFallback(fallback))
	turn, err := recorder.CommitFinalAnswer(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 3, turn)

	_, success := appender.snapshot()
	require.Len(t, success, 2)
	require.Equal(t, []int{0, 1, 2}, storedTurns(success[0]))
	require.Equal(t, []StoredMessage{{Turn: 3, Message: fallback}}, success[1])
}

func TestAgenticRunRecorderProjectsTerminalTextAndPreservesMetadata(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	terminal := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.Reasoning{Text: "summary", Signature: "encrypted"}),
			schema.NewContentBlock(&schema.AssistantGenText{
				Text: `{"answer":"done","artifacts":[]}`,
				OpenAIExtension: &schemaopenai.AssistantGenTextExtension{Annotations: []*schemaopenai.TextAnnotation{{
					Type: schemaopenai.TextAnnotationTypeURLCitation,
				}}},
			}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{
			TokenUsage:      &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
			OpenAIExtension: &schemaopenai.ResponseMetaExtension{ID: "resp_terminal"},
		},
	}
	require.NoError(t, recorder.CaptureAssistant(terminal))
	require.NoError(t, recorder.ReplacePendingAssistantText("done"))
	_, err := recorder.CommitFinalAnswer(context.Background(), "")
	require.NoError(t, err)

	_, success := appender.snapshot()
	persisted := success[0][1].Message
	require.Equal(t, "done", AgenticMessageText(persisted))
	require.Equal(t, "encrypted", persisted.ContentBlocks[0].Reasoning.Signature)
	require.Equal(t, "resp_terminal", persisted.ResponseMeta.OpenAIExtension.ID)
	require.Equal(t, 12, persisted.ResponseMeta.TokenUsage.TotalTokens)
	require.Empty(t, persisted.ContentBlocks[1].AssistantGenText.OpenAIExtension.Annotations)
	require.Equal(t, `{"answer":"done","artifacts":[]}`, AgenticMessageText(terminal), "source message must not mutate")
}

func TestAgenticRunRecorderFallbackReplacementValidation(t *testing.T) {
	recorder, _, _ := newRecorderTest(t, 0)
	require.ErrorContains(t, recorder.ReplacePendingAssistantForFallback(nil), "required")
	require.ErrorContains(t,
		recorder.ReplacePendingAssistantForFallback(assistantCallMsg(call("c1", "lookup"))),
		"must not contain tool calls",
	)
}

func storedTurns(msgs []StoredMessage) []int {
	out := make([]int, len(msgs))
	for i := range msgs {
		out[i] = msgs[i].Turn
	}
	return out
}

func TestAgenticRunRecorderOrdersParallelResultsByCallOrder(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(
		call("c1", "retrieve"), call("c2", "search"),
	)))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c2", "search", "second")))
	require.False(t, recorder.BatchComplete())
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "first")))
	require.True(t, recorder.BatchComplete())
	require.NoError(t, recorder.CommitToolBatch(context.Background()))

	_, success := appender.snapshot()
	require.Len(t, success, 1)
	require.Equal(t, "retrieve", AgenticMessageToolResults(success[0][2].Message)[0].Name)
	require.Equal(t, "search", AgenticMessageToolResults(success[0][3].Message)[0].Name)
}

func TestAgenticRunRecorderRejectsInvalidBatchesWithoutAppend(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(
		call("c1", "retrieve"), call("c2", "search"),
	)))

	require.Error(t, recorder.CommitToolBatch(context.Background()))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "first")))
	require.Error(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "duplicate")))
	require.Error(t, recorder.CaptureToolResult(resultMsg("unknown", "retrieve", "extra")))
	require.Error(t, recorder.CaptureToolResult(resultMsg("c2", "wrong", "mismatch")))
	require.Error(t, recorder.CommitToolBatch(context.Background()))
	attempts, success := appender.snapshot()
	require.Empty(t, attempts)
	require.Empty(t, success)
}

func TestAgenticRunRecorderRejectsDuplicateAssistantCallIDs(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	err := recorder.CaptureAssistant(assistantCallMsg(
		call("same", "retrieve"), call("same", "search"),
	))
	require.ErrorContains(t, err, "duplicate assistant call ID")
	attempts, _ := appender.snapshot()
	require.Empty(t, attempts)
}

func TestAgenticRunRecorderAppendFailureIsTerminal(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 4)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c1", "retrieve"))))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "hits")))
	appender.setError(errors.New("commit outcome unknown"))

	err := recorder.CommitToolBatch(context.Background())
	require.ErrorContains(t, err, "commit tool batch")
	require.Equal(t, 4, recorder.NextTurnToCommit())
	require.False(t, recorder.Finished())
	require.ErrorIs(t, recorder.CaptureAssistant(NewAgenticAssistantText("no retry")), err)
	require.ErrorIs(t, recorder.CommitToolBatch(context.Background()), err)
	attempts, success := appender.snapshot()
	require.Len(t, attempts, 1, "ambiguous append must never be retried")
	require.Empty(t, success)
}

func TestAgenticRunRecorderPreservesEarlierBatchAfterFinalFailure(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("c1", "retrieve"))))
	require.NoError(t, recorder.CaptureToolResult(resultMsg("c1", "retrieve", "hits")))
	require.NoError(t, recorder.CommitToolBatch(context.Background()))

	require.NoError(t, recorder.CaptureAssistant(NewAgenticAssistantText("final")))
	appender.setError(errors.New("db unavailable"))
	_, err := recorder.CommitFinalAnswer(context.Background(), "")
	require.Error(t, err)
	require.Equal(t, 3, recorder.NextTurnToCommit())
	attempts, success := appender.snapshot()
	require.Len(t, attempts, 2)
	require.Len(t, success, 1, "the completed tool batch remains committed")
}

func TestAgenticRunRecorderConcurrentCaptureAndCommit(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(
		call("c1", "retrieve"), call("c2", "search"),
	)))

	var captureErrors atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := recorder.CaptureToolResult(resultMsg("c1", "retrieve", "first")); err != nil {
				captureErrors.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			if err := recorder.CaptureToolResult(resultMsg("c2", "search", "second")); err != nil {
				captureErrors.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(98), captureErrors.Load())

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- recorder.CommitToolBatch(context.Background()) }()
	}
	err1, err2 := <-results, <-results
	require.True(t, (err1 == nil) != (err2 == nil), "exactly one concurrent commit succeeds")
	loser := err1
	if loser == nil {
		loser = err2
	}
	require.ErrorContains(t, loser, "no assistant tool-call message is pending")
	attempts, success := appender.snapshot()
	require.Len(t, attempts, 1)
	require.Len(t, success, 1)
}

func TestAgenticRunRecorderValidation(t *testing.T) {
	appender := &fakeAgenticAppender{}
	_, err := newAgenticRunRecorder(uuid.New(), -1, NewAgenticUserText("x"), appender.append)
	require.Error(t, err)
	_, err = newAgenticRunRecorder(uuid.New(), 0, nil, appender.append)
	require.Error(t, err)
	_, err = newAgenticRunRecorder(uuid.New(), 0, NewAgenticAssistantText("x"), appender.append)
	require.Error(t, err)
	_, err = newAgenticRunRecorder(uuid.New(), 0, resultMsg("c1", "retrieve", "x"), appender.append)
	require.Error(t, err)

	recorder, _, _ := newRecorderTest(t, 0)
	live := NewAgenticAssistantText("valid live output")
	live.Extra = map[string]any{einoMessageIDExtraKey: "run-local-id"}
	require.NoError(t, recorder.CaptureAssistant(live))
	_, err = recorder.CommitFinalAnswer(context.Background(), "")
	require.NoError(t, err)

	recorder, _, _ = newRecorderTest(t, 0)
	unsupported := NewAgenticAssistantText("bad extra")
	unsupported.Extra = map[string]any{"provider-private": "not durable"}
	require.Error(t, recorder.CaptureAssistant(unsupported))
	require.Error(t, recorder.CaptureAssistant(NewAgenticUserText("wrong role")))
	require.Error(t, recorder.CaptureToolResult(NewAgenticUserText("not a result")))
	_, err = recorder.CommitFinalAnswer(context.Background(), "")
	require.Error(t, err)
}
