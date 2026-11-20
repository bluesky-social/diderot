package ask

import (
	"context"
	"math"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// agenticTestMsg builds a minimal valid AgenticMessage.
func agenticTestMsg(role schema.AgenticRoleType, blocks ...*schema.ContentBlock) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: role, ContentBlocks: blocks}
}

func agenticUserTextBlock(text string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.UserInputText{Text: text})
}

func agenticAssistantTextBlock(text string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.AssistantGenText{Text: text})
}

func agenticCallBlock(id, name string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: "{}"})
}

func agenticResultBlock(id, name, text string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.FunctionToolResult{
		CallID:  id,
		Name:    name,
		Content: []*schema.FunctionToolResultContentBlock{agenticTextToolResult(text)},
	})
}

// newAgenticSession creates a bare session row for storage tests.
func newAgenticSession(t *testing.T, mgr *Manager) uuid.UUID {
	t.Helper()
	s, err := mgr.Create(context.Background())
	require.NoError(t, err)
	return s.ID
}

// agenticTurns returns the turn numbers of the loaded messages.
func agenticTurns(msgs []StoredMessage) []int {
	var out []int
	for _, m := range msgs {
		out = append(out, m.Turn)
	}
	return out
}

func TestLoadAgenticSessionNotFound(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	_, _, err := mgr.LoadAgenticSession(context.Background(), uuid.New())
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// TestAppendAgenticMessagesPersistsTextWithEmbeddedNulBytes is a regression
// test against the real database: an embedded NUL byte (U+0000) anywhere in
// a content block's text used to pass ValidateAgenticMessage untouched, but
// then fail the INSERT itself, because Go's json.Marshal encodes a NUL byte
// as the literal escape sequence "\u0000", which Postgres's jsonb input
// function rejects (SQLSTATE 22P05, "unsupported Unicode escape sequence").
// Because the whole batch commits in one transaction, the failure discarded
// not just the offending text but the turn's already-generated answer (and
// any sibling row in the same batch, such as the user's question here).
//
// Trigger: an assistant message whose text contains a literal NUL byte,
// e.g. from the model copying a "\u0000" escape it saw in a tool result's
// JSON into its own generated text (tool output is JSON-escaped, so it
// cannot itself carry a raw NUL this way).
func TestAppendAgenticMessagesPersistsTextWithEmbeddedNulBytes(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	assistant := agenticTestMsg(schema.AgenticRoleTypeAssistant,
		agenticAssistantTextBlock("here is the extracted value: \x00 (from a binary field)"))

	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("what does the attached file contain?")},
		{Turn: 1, Message: assistant},
	})
	require.NoError(t, err, "a message ValidateAgenticMessage accepts must be durably persistable; "+
		"instead the whole turn's answer was discarded on an unvalidated embedded NUL byte")

	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(msgs))
	require.Equal(t, "here is the extracted value:  (from a binary field)", AgenticMessageText(msgs[1].Message),
		"the NUL byte must be stripped on write, not left in place to break persistence")
}

func TestAppendAgenticMessagesSessionNotFound(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	err := mgr.AppendAgenticMessages(context.Background(), uuid.New(), []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("hi")},
	})
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestAgenticRoundTripUsageAndRoles(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	assistant := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.AssistantGenText{Text: "I wrote it"}),
		},
		ResponseMeta: &schema.AgenticResponseMeta{
			TokenUsage: &schema.TokenUsage{
				PromptTokens:       math.MaxInt32,
				PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 999},
				CompletionTokens:   17,
				TotalTokens:        math.MaxInt32,
			},
		},
	}
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("implement x")},
		{Turn: 1, Message: assistant},
	})
	require.NoError(t, err)

	sess, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Equal(t, []int{0, 1}, agenticTurns(msgs))
	require.Equal(t, NewAgenticUserText("implement x"), msgs[0].Message)
	require.Equal(t, assistant, msgs[1].Message)

	// Denormalized columns agree with the payload.
	var input, cached, output, total *int32
	require.NoError(t, mgr.pool.QueryRow(context.Background(),
		`SELECT input_tokens, cache_read_tokens, output_tokens, NULL::int
		   FROM ask_messages WHERE session_id = $1 AND turn = 1`,
		sessionID).Scan(&input, &cached, &output, &total))
	require.NotNil(t, input)
	require.Equal(t, int32(math.MaxInt32), *input)
	require.NotNil(t, cached)
	require.Equal(t, int32(999), *cached)
	require.NotNil(t, output)
	require.Equal(t, int32(17), *output)
	require.Nil(t, total)
}

func TestAppendAgenticMessagesDuplicateTurnRollback(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	assistant := agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("a"))
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: assistant},
	})
	require.NoError(t, err)

	// Duplicate turn 1 fails atomically: no row survives (the pending
	// turn-2 user text must roll back with it) and last_turn_at is
	// unchanged.
	err = mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 1, Message: NewAgenticUserText("dupe")},
		{Turn: 2, Message: agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("b"))},
	})
	require.Error(t, err)

	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(msgs))

	var count int
	require.NoError(t, mgr.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ask_messages WHERE session_id = $1 AND turn = 2`,
		sessionID).Scan(&count))
	require.Zero(t, count)
}

func TestLoadAgenticRejectsDenormalizedDrift(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: NewAgenticAssistantText("a")},
	})
	require.NoError(t, err)

	// Flip token usage on the user row — it should never be persisted
	// by AppendAgenticMessages but a rogue write must be caught on read.
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET input_tokens = 7, output_tokens = 3 WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.NoError(t, err)
	_, _, err = mgr.LoadAgenticSession(context.Background(), sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input_tokens")

	// Restore the token drift first (a legal UPDATE), then separately
	// attempt a role-column drift: Postgres's CHECK constraint
	// (message->>'role' = role) rejects the illegal role swap before
	// any decoder runs, and the failed UPDATE must not touch the row.
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET input_tokens = NULL, output_tokens = NULL WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.NoError(t, err)
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET role = 'assistant' WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.Error(t, err)
	_, loaded, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(loaded))
	require.Equal(t, schema.AgenticRoleTypeUser, loaded[0].Message.Role)
	require.Nil(t, loaded[0].Message.ResponseMeta)

	// A schema-version drift is likewise rejected by the database CHECK
	// constraint (message_schema_version = 1) before any decoder runs.
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET message_schema_version = 99 WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.Error(t, err)

	// The persisted row remains valid.
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET message_schema_version = 1 WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.NoError(t, err)
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, agenticTurns(msgs))
}

// insertAgenticRow writes a row directly (bypassing the store) so a
// test can plant an incomplete/malformed tail as if a prior process
// had been interrupted mid-turn.
func insertAgenticRow(t *testing.T, mgr *Manager, sessionID uuid.UUID, msg *schema.AgenticMessage) int {
	t.Helper()
	row, err := encodeAgenticMessageRow(msg)
	require.NoError(t, err)
	var turn int
	require.NoError(t, mgr.pool.QueryRow(context.Background(),
		`INSERT INTO ask_messages
		   (session_id, turn, role, message_schema_version, message,
		    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
		 VALUES ($1, COALESCE((SELECT max(turn) + 1 FROM ask_messages WHERE session_id = $1), 0),
		         $2, $3, $4, $5, $6, $7, $8)
		 RETURNING turn`,
		sessionID, row.Role, row.AgenticMessageSchemaVersion, row.JSON,
		row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens).Scan(&turn))
	return turn
}

// plantSessionMessages plants multiple rows in order using
// insertAgenticRow.
func plantSessionMessages(t *testing.T, mgr *Manager, sessionID uuid.UUID, msgs []*schema.AgenticMessage) {
	t.Helper()
	for _, msg := range msgs {
		insertAgenticRow(t, mgr, sessionID, msg)
	}
}

func requireAgenticTurns(t *testing.T, mgr *Manager, sessionID uuid.UUID, wantTurns []int) {
	t.Helper()
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	got := agenticTurns(msgs)
	require.Len(t, got, len(wantTurns))
	for i := range wantTurns {
		require.Equal(t, wantTurns[i], got[i])
	}
}

func requireRepairedRows(t *testing.T, mgr *Manager, sessionID uuid.UUID, want int) {
	t.Helper()
	var n int
	require.NoError(t, mgr.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ask_messages WHERE session_id = $1`, sessionID).Scan(&n))
	require.Equal(t, want, n)
}

func TestAgenticTrailingUserTextRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("a")),
		NewAgenticUserText("pending"),
	})
	_, _, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	requireAgenticTurns(t, mgr, sessionID, []int{0, 1})
	requireRepairedRows(t, mgr, sessionID, 2)
}

func TestAgenticLoneUserTextRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
	})
	// A lone user row is an incomplete tail (persistence commits
	// user+assistant or complete tool batches atomically), so repair
	// must delete it entirely, leaving a fresh/empty session.
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticIncompleteCallTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	// Assistant call without any result batch is dropped, and the
	// trailing user row it exposes is also an incomplete tail, so the
	// whole session is trimmed back to empty.
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticRetainsCompleteParallelResultTail(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha"),
			agenticResultBlock("call_b", "grep", "beta")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2}, agenticTurns(msgs))
	require.Equal(t, 2, len(msgs[2].Message.ContentBlocks))
	requireRepairedRows(t, mgr, sessionID, 3)
}

func TestAgenticRetainsCompleteMultiRowParallelResults(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		// Pinned Eino emits one user AgenticMessage per parallel tool
		// result; the whole trailing run is one complete batch.
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_b", "grep", "beta")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(msgs))
	requireRepairedRows(t, mgr, sessionID, 4)
}

func TestAgenticIncompleteMultiRowResultRunRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
		// Missing the result for call_b: the run is incomplete, so the
		// whole multi-row run and the exposed assistant call row are
		// dropped, then the trailing user text too — to empty.
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_c", "lookup", "extra")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticCompleteCoalescedResultRowMultiRowRunMismatch(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		// A coalesced single row carries the whole batch after a partial
		// first row: the run (both rows) must be validated together and
		// rejected for the duplicate call_b result.
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_b", "grep", "beta"),
			agenticResultBlock("call_b", "grep", "again")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticIncompleteResultTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	// Missing result must be dropped, then the now-exposed assistant
	// call row must also be dropped, and the trailing user text it
	// exposes is itself incomplete — iterative repair to empty.
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticMismatchedResultTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_x", "lookup", "wrong")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticMixedResultTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha"),
			agenticUserTextBlock("please continue")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticDuplicateResultTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "one"),
			agenticResultBlock("call_a", "lookup", "two")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticNameMismatchTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "grep", "alpha")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticCompleteTailRetentionIsStrict(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha"),
			agenticResultBlock("call_b", "grep", "beta"),
			agenticResultBlock("call_c", "extra", "gamma")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticTrailingOrphanResultTailRepair(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("a")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("orphan", "lookup", "nope")),
	})
	_, _, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	requireAgenticTurns(t, mgr, sessionID, []int{0, 1})
	requireRepairedRows(t, mgr, sessionID, 2)
}

func TestAppendAgenticMessagesNoLiveMutation(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	msg := NewAgenticAssistantText("done")
	msg.Extra = map[string]any{einoMessageIDExtraKey: "run-msg-7"}

	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: msg},
	})
	require.NoError(t, err)
	require.Equal(t, map[string]any{einoMessageIDExtraKey: "run-msg-7"}, msg.Extra,
		"append must not mutate the live Eino message")
	require.NotContains(t, string(mustAgenticRowJSON(t, mgr, sessionID, 1)), einoMessageIDExtraKey,
		"persisted row must not contain the run-local message ID")

	_, loaded, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Nil(t, loaded[1].Message.Extra)
}

func mustAgenticRowJSON(t *testing.T, mgr *Manager, sessionID uuid.UUID, turn int) []byte {
	t.Helper()
	var raw []byte
	require.NoError(t, mgr.pool.QueryRow(context.Background(),
		`SELECT message::text FROM ask_messages WHERE session_id = $1 AND turn = $2`,
		sessionID, turn).Scan(&raw))
	return raw
}

func TestAgenticRejectsNestedRollbackOnFailure(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	// A nil message fails encoding inside the append path before any
	// insert; no rows exist and the session is unharmed.
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: nil},
	})
	require.Error(t, err)
	requireRepairedRows(t, mgr, sessionID, 0)

	// An invalid (unencodable) message is rejected the same way.
	bad := &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeSystem,
		ContentBlocks: []*schema.ContentBlock{agenticUserTextBlock("nope")},
	}
	err = mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: bad},
	})
	require.Error(t, err)
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticMalformedRowRejection(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	// Plant a syntactically valid JSONB row whose payload is not a
	// valid AgenticMessage (extra provider payload). Load must fail,
	// not silently drop or rewrite it.
	_, err := mgr.pool.Exec(context.Background(),
		`INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message)
		 VALUES ($1, 0, 'user', 1, '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"q"}}],"extra":{"provider_payload":true}}')`,
		sessionID)
	require.NoError(t, err)

	_, _, err = mgr.LoadAgenticSession(context.Background(), sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "message extra is not persisted")
}

func TestAgenticEmptySessionLoadsEmpty(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	sess, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Empty(t, msgs)
}

func TestAppendAgenticMessagesRejectsNonContiguousTurns(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	cases := []struct {
		name string
		msgs []StoredMessage
	}{
		{name: "negative turn", msgs: []StoredMessage{{Turn: -1, Message: NewAgenticUserText("no")}}},
		{name: "negative after positive", msgs: []StoredMessage{
			{Turn: 0, Message: NewAgenticUserText("a")},
			{Turn: -1, Message: NewAgenticUserText("bad")},
		}},
		{name: "gap", msgs: []StoredMessage{
			{Turn: 0, Message: NewAgenticUserText("a")},
			{Turn: 2, Message: NewAgenticUserText("gap")},
		}},
		{name: "non-contiguous", msgs: []StoredMessage{
			{Turn: 0, Message: NewAgenticUserText("a")},
			{Turn: 1, Message: NewAgenticUserText("b")},
			{Turn: 3, Message: NewAgenticUserText("skipped")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mgr.AppendAgenticMessages(context.Background(), sessionID, tc.msgs)
			require.Error(t, err)
			require.Contains(t, err.Error(), "turn")
			requireRepairedRows(t, mgr, sessionID, 0)
		})
	}
}

func TestAppendAgenticMessagesRejectsStaleBatch(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	// A low batch computed against an older history must not interleave
	// once turns 0..2 are already persisted.
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("a"))},
		{Turn: 2, Message: NewAgenticUserText("again")},
	})
	require.NoError(t, err)

	err = mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 1, Message: NewAgenticUserText("stale-low")},
		{Turn: 2, Message: agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("stale"))},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale batch")

	// No partial write escaped the rejected batch: the raw rows are
	// still exactly the initial turns 0..2 (asserted via SQL so tail
	// repair cannot mask them).
	requireRawAgenticTurns(t, mgr, sessionID, []int{0, 1, 2})

	// The exact next batch continues from persisted max+1 and succeeds.
	err = mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 3, Message: agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("next"))},
	})
	require.NoError(t, err)
	requireRawAgenticTurns(t, mgr, sessionID, []int{0, 1, 2, 3})

	// Loading keeps every persisted row: turn 2's user text is an
	// intermediate row followed by the valid assistant tail at turn 3,
	// so only an incomplete SUFFIX would be repaired.
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, agenticTurns(msgs))
}

// requireRawAgenticTurns asserts the persisted turn numbers without
// any load-time tail repair.
func requireRawAgenticTurns(t *testing.T, mgr *Manager, sessionID uuid.UUID, want []int) {
	t.Helper()
	rows, err := mgr.pool.Query(context.Background(),
		`SELECT turn FROM ask_messages WHERE session_id = $1 ORDER BY turn`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var got []int
	for rows.Next() {
		var turn int
		require.NoError(t, rows.Scan(&turn))
		got = append(got, turn)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, want, got)
}

func TestAppendAgenticMessagesRejectsFreshAboveZero(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	// A fresh session has persisted max NULL, so the first batch must
	// start at turn 0; starting above zero is a stale-batch rejection
	// with no writes.
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 5, Message: NewAgenticUserText("q")},
		{Turn: 6, Message: agenticTestMsg(schema.AgenticRoleTypeAssistant, agenticAssistantTextBlock("a"))},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale batch")
	requireRepairedRows(t, mgr, sessionID, 0)
}

func TestAgenticRetainsAssistantReasoningWithCompleteResults(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			schema.NewContentBlock(&schema.Reasoning{Text: "thinking..."}),
			agenticCallBlock("call_a", "lookup"),
			agenticAssistantTextBlock("calling now")),
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
	})
	_, msgs, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	// The call/result IDs and names match exactly; the assistant's
	// reasoning and inline text are legitimate Responses-adapter
	// companions, so the complete tail is retained.
	require.Equal(t, []int{0, 1, 2}, agenticTurns(msgs))
	requireRepairedRows(t, mgr, sessionID, 3)
}

// TestInspectAgenticRoundTrip proves Manager.Inspect returns the
// canonical v1 rows (same central decoder as LoadAgenticSession) for a
// session that includes assistant usage metadata.
func TestInspectAgenticRoundTrip(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)

	assistant := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep"),
		},
		ResponseMeta: &schema.AgenticResponseMeta{
			TokenUsage: &schema.TokenUsage{
				PromptTokens:       1000,
				PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 700},
				CompletionTokens:   42,
				TotalTokens:        1042,
			},
		},
	}
	err := mgr.AppendAgenticMessages(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("q")},
		{Turn: 1, Message: assistant},
	})
	require.NoError(t, err)

	sess, msgs, err := mgr.Inspect(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Equal(t, []int{0, 1}, agenticTurns(msgs))
	require.Equal(t, NewAgenticUserText("q"), msgs[0].Message)
	require.Equal(t, assistant, msgs[1].Message)

	// Role, schema version, and usage survive the decode centrally.
	require.Equal(t, schema.AgenticRoleTypeUser, msgs[0].Message.Role)
	require.Equal(t, schema.AgenticRoleTypeAssistant, msgs[1].Message.Role)
	usage := AgenticMessageUsage(msgs[1].Message)
	require.NotNil(t, usage)
	require.Equal(t, 1000, usage.PromptTokens)
	require.Equal(t, 700, usage.PromptTokenDetails.CachedTokens)
	require.Equal(t, 42, usage.CompletionTokens)
	require.Nil(t, msgs[0].Message.ResponseMeta)
}

// TestInspectNotFound mirrors the LoadAgenticSession contract: a
// missing session row reports ErrSessionNotFound.
func TestInspectNotFound(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	_, _, err := mgr.Inspect(context.Background(), uuid.New())
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// TestInspectEmptySessionLoadsEmpty mirrors LoadAgenticSession: a
// session with no messages inspects as an empty slice.
func TestInspectEmptySessionLoadsEmpty(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	sess, msgs, err := mgr.Inspect(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Empty(t, msgs)
}

// TestInspectIsReadOnlyPreservesIncompleteTail is the core read-only
// regression: Inspect must observe exactly what is on disk — a
// trailing incomplete tool-call tail — and neither trim it nor repair
// it. LoadAgenticSession on the same rows repairs the tail to
// completion, proving the difference is the inspection path, not the
// decoder.
func TestInspectIsReadOnlyPreservesIncompleteTail(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
		agenticTestMsg(schema.AgenticRoleTypeAssistant,
			agenticCallBlock("call_a", "lookup"),
			agenticCallBlock("call_b", "grep")),
		// Missing the result for call_b: the multi-row result run and
		// the exposed assistant call row are incomplete together, so
		// the load path trims the whole session to empty.
		agenticTestMsg(schema.AgenticRoleTypeUser,
			agenticResultBlock("call_a", "lookup", "alpha")),
	})
	// Inspect preserves the incomplete tail on disk.
	sess, msgs, err := mgr.Inspect(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, sess.ID)
	require.Equal(t, []int{0, 1, 2}, agenticTurns(msgs))
	require.Len(t, msgs[1].Message.ContentBlocks, 2)
	require.Len(t, msgs[2].Message.ContentBlocks, 1)
	requireRepairedRows(t, mgr, sessionID, 3)

	// The load path repairs the same rows, proving the preserved tail
	// was genuinely incomplete and Inspect did not repair it.
	_, _, err = mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	requireRepairedRows(t, mgr, sessionID, 0)
}

// TestInspectRejectsMalformedRow proves Inspect fails loudly (same
// central decode validation as LoadAgenticSession) on a malformed
// persisted row instead of best-effort-skipping it in the diagnostics
// surface.
func TestInspectRejectsMalformedRow(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	plantSessionMessages(t, mgr, sessionID, []*schema.AgenticMessage{
		NewAgenticUserText("q"),
	})

	// Token-drift: a rogue UPDATE stamps non-NULL input_tokens on a
	// user row (which must never carry usage). The central row decoder
	// rejects the mismatch on read.
	_, err := mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET input_tokens = 7 WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.NoError(t, err)
	_, _, err = mgr.Inspect(context.Background(), sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input_tokens")

	// Restore the legal row state; Inspect succeeds again.
	_, err = mgr.pool.Exec(context.Background(),
		`UPDATE ask_messages SET input_tokens = NULL WHERE session_id = $1 AND turn = 0`,
		sessionID)
	require.NoError(t, err)
	_, msgs, err := mgr.Inspect(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []int{0}, agenticTurns(msgs))

	// Malformed payload: syntactically valid JSONB that does not
	// validate as a v1 AgenticMessage (an unpersisted extra payload).
	// The role/version CHECKs pass; the central decoder fails loudly.
	_, err = mgr.pool.Exec(context.Background(),
		`INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message)
		 VALUES ($1, 1, 'user', 1, '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"q"}}],"extra":{"provider_payload":true}}')`,
		sessionID)
	require.NoError(t, err)
	_, _, err = mgr.Inspect(context.Background(), sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "message extra is not persisted")
}
