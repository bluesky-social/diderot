package ask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// StoredMessage is the application storage record for one
// ask_messages row in the v1 AgenticMessage shape. It carries only
// the persistence-relevant frame — turn, the validated AgenticMessage
// payload, and created_at — not mirrored Eino fields. Role, schema
// version, and token-usage columns are derived from the payload via
// encodeAgenticMessageRow/decodeAgenticMessageRow and validated
// against the denormalized columns on read.
//
// Temporary Agentic-suffixed APIs: this new-path storage lives beside
// the legacy Message store until the final cut removes the old loop,
// then it replaces the legacy load/append surface.
type StoredMessage struct {
	Turn             int
	Message          *schema.AgenticMessage
	CreatedAt        time.Time
	IncompleteReason IncompleteReason
}

// LoadAgenticSession loads a session and its validated AgenticMessage
// history in turn order, repairing an incomplete agentic tail as
// documented on repairAgenticTail. Returns ErrSessionNotFound when the
// session row is missing. A session with no messages loads as an
// empty slice (fresh sessions are appended by AppendAgenticMessages).
//
// Read repair deletes only trimmed tail rows, in the same transaction
// as the reads. Like the legacy Load, callers must hold the session
// lock so a concurrent writer cannot insert the same turn numbers
// between read and delete.
func (m *Manager) LoadAgenticSession(ctx context.Context, sessionID uuid.UUID) (*Session, []StoredMessage, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sess Session
	err = tx.QueryRow(ctx,
		`SELECT id, created_at, last_turn_at, finished_at
		   FROM ask_sessions WHERE id = $1`,
		sessionID,
	).Scan(&sess.ID, &sess.CreatedAt, &sess.LastTurnAt, &sess.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrSessionNotFound
		}
		return nil, nil, fmt.Errorf("session lookup: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT turn, role, message_schema_version, message, created_at,
		        input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		        COALESCE(incomplete_reason, '')
		   FROM ask_messages WHERE session_id = $1 ORDER BY turn`,
		sessionID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("messages query: %w", err)
	}
	scans, err := scanAgenticMessageRows(rows)
	if err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("messages iter: %w", err)
	}

	decoded := make([]StoredMessage, 0, len(scans))
	for _, sc := range scans {
		msg, err := decodeAgenticMessageRow(messageRow{
			Role:                        sc.Role,
			AgenticMessageSchemaVersion: sc.SchemaVersion,
			JSON:                        sc.Message,
			InputTokens:                 sc.InputTokens,
			OutputTokens:                sc.OutputTokens,
			CacheReadTokens:             sc.CacheReadTokens,
			CacheWriteTokens:            sc.CacheWriteTokens,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ask message turn=%d: %w", sc.Turn, err)
		}
		decoded = append(decoded, StoredMessage{
			Turn: sc.Turn, Message: msg, CreatedAt: sc.CreatedAt,
			IncompleteReason: sc.IncompleteReason,
		})
	}

	repaired := repairAgenticTail(decoded)
	if len(repaired) < len(decoded) {
		if err := checkSessionLease(ctx, tx, sessionID); err != nil {
			return nil, nil, err
		}
		droppedTurns := make([]int, 0, len(decoded)-len(repaired))
		for i := len(repaired); i < len(decoded); i++ {
			droppedTurns = append(droppedTurns, decoded[i].Turn)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM ask_messages
			  WHERE session_id = $1 AND turn = ANY($2::int[])`,
			sessionID, droppedTurns,
		); err != nil {
			return nil, nil, fmt.Errorf("repair delete: %w", err)
		}
		m.logger.Warn("ask agentic history repaired on load",
			"session_id", sessionID, "dropped_turns", droppedTurns)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit: %w", err)
	}
	return &sess, repaired, nil
}

// AppendAgenticMessages appends a batch of AgenticMessage rows
// atomically. Callers assign exact, contiguous Turn values continuing
// from the loaded history (like AppendMessagesTx); the whole batch
// goes into one transaction so a crash cannot leave a partial
// role-pair on disk. Every write goes through encodeAgenticMessageRow,
// so the denormalized role and token columns are derived from the
// payload and a caller cannot persist a row whose columns disagree
// with its message.
//
// The intra-batch turn contract (non-negative, previous+1) is enforced
// before any transaction opens so an invalid caller performs no
// writes. Inside the write transaction the session row is locked and
// the persisted high-water mark re-read: the first turn must equal
// max+1 (0 for a fresh session), so a stale batch computed against an
// outdated history can never interleave; a fresh-session append must
// start at turn 0. The transaction requires the session lock UPDATE to
// touch exactly one row; a missing session fails the whole batch with
// no partial insert and no last_turn_at bump. The unique (session_id,
// turn) index additionally fails loudly on a duplicate turn (lock
// bypass). Called within WithSessionLock, the batch commits only while
// the turn still holds the session's lease.
func (m *Manager) AppendAgenticMessages(ctx context.Context, sessionID uuid.UUID, msgs []StoredMessage) error {
	return m.appendAgenticMessages(ctx, sessionID, msgs, -1, nil)
}

// AppendAgenticMessagesWithArtifacts commits a terminal message batch and its
// selected user-facing artifacts in one transaction.
func (m *Manager) AppendAgenticMessagesWithArtifacts(ctx context.Context, sessionID uuid.UUID, msgs []StoredMessage, assistantTurn int, selections []ArtifactSelection) error {
	if err := validateArtifactSelectionShape(selections); err != nil {
		return err
	}
	if len(selections) == 0 {
		return m.appendAgenticMessages(ctx, sessionID, msgs, -1, nil)
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].Turn != assistantTurn || msgs[len(msgs)-1].Message == nil || msgs[len(msgs)-1].Message.Role != schema.AgenticRoleTypeAssistant {
		return errors.New("ask result artifacts require the terminal assistant message")
	}
	return m.appendAgenticMessages(ctx, sessionID, msgs, assistantTurn, selections)
}

func (m *Manager) appendAgenticMessages(ctx context.Context, sessionID uuid.UUID, msgs []StoredMessage, assistantTurn int, selections []ArtifactSelection) error {
	if len(msgs) == 0 {
		return nil
	}
	// Intra-batch caller contract: exact contiguous non-negative turns.
	// Validated before any transaction opens so a negative/non-
	// contiguous caller performs no writes at all.
	for i, msg := range msgs {
		if msg.Turn < 0 {
			return fmt.Errorf("ask agentic messages: negative turn %d", msg.Turn)
		}
		if i > 0 && msg.Turn != msgs[i-1].Turn+1 {
			return fmt.Errorf("ask agentic messages: non-contiguous turn %d after %d", msg.Turn, msgs[i-1].Turn)
		}
	}
	rows := make([]messageRow, 0, len(msgs))
	for i, msg := range msgs {
		if msg.Message == nil {
			return fmt.Errorf("ask agentic message %d: nil message", i)
		}
		if msg.IncompleteReason != "" && msg.Message.Role != schema.AgenticRoleTypeAssistant {
			return fmt.Errorf("ask agentic message %d: incomplete reason requires assistant role", i)
		}
		row, err := encodeAgenticMessageRow(msg.Message)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the session row inside the write transaction first. This
	// serializes appends for the same session and fails loudly when the
	// session is missing (ErrSessionNotFound), leaving the tx empty.
	// The later UPDATE also requires exactly one row as a backstop.
	var sessionIDLocked uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM ask_sessions WHERE id = $1 FOR UPDATE`,
		sessionID,
	).Scan(&sessionIDLocked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSessionNotFound
		}
		return fmt.Errorf("lock session: %w", err)
	}
	// A turn that lost its session lease to another process commits
	// nothing; holding the lease's row keeps it from being taken over
	// until this batch commits.
	if err := checkSessionLease(ctx, tx, sessionID); err != nil {
		return err
	}

	// With the session locked, read the persisted high-water mark. A
	// stale batch computed against an outdated history is rejected:
	// the first turn must equal max+1 (0 for a fresh session).
	var currentTurn *int
	if err := tx.QueryRow(ctx,
		`SELECT max(turn) FROM ask_messages WHERE session_id = $1`,
		sessionID,
	).Scan(&currentTurn); err != nil {
		return fmt.Errorf("next turn: %w", err)
	}
	expected := 0
	if currentTurn != nil {
		expected = *currentTurn + 1
	}
	if msgs[0].Turn != expected {
		return fmt.Errorf("ask agentic messages: stale batch: first turn %d, persisted max+1 is %d", msgs[0].Turn, expected)
	}

	for i, msg := range msgs {
		row := rows[i]
		if _, err := tx.Exec(ctx,
			`INSERT INTO ask_messages
			   (session_id, turn, role, message_schema_version, message,
			    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			    incomplete_reason)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''))`,
			sessionID, msg.Turn, row.Role, row.AgenticMessageSchemaVersion, row.JSON,
			row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens,
			msg.IncompleteReason,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return fmt.Errorf("append agentic messages: %w", ErrSessionNotFound)
			}
			return fmt.Errorf("insert agentic message turn=%d: %w", msg.Turn, err)
		}
	}

	for position, selection := range selections {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ask_result_artifacts
			       (session_id,assistant_turn,position,artifact_id,title,alt_text)
			SELECT $1,$2,$3,id,$5,$6
			  FROM ask_artifacts
			 WHERE id=$4 AND session_id=$1 AND status='ready' AND source_tool=$7`,
			sessionID, assistantTurn, position, selection.ArtifactID,
			strings.TrimSpace(selection.Title), strings.TrimSpace(selection.AltText), PanelImageToolName)
		if err != nil {
			return fmt.Errorf("insert result artifact %s: %w", selection.ArtifactID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("artifact %s is not a ready panel image in this session", selection.ArtifactID)
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE ask_sessions SET last_turn_at = now() WHERE id = $1`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("bump last_turn_at: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrSessionNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// agenticMessageScan is the raw column projection of an ask_messages
// row in the v1 AgenticMessage shape.
type agenticMessageScan struct {
	Turn             int
	Role             string
	SchemaVersion    int16
	Message          []byte
	CreatedAt        time.Time
	InputTokens      *int32
	OutputTokens     *int32
	CacheReadTokens  *int32
	CacheWriteTokens *int32
	IncompleteReason IncompleteReason
}

// scanAgenticMessageRows decodes the v1 column projection into the raw
// scan type without interpreting it; decodeAgenticMessageRow performs
// schema-version, role, and token-drift validation.
func scanAgenticMessageRows(rows pgx.Rows) ([]agenticMessageScan, error) {
	var out []agenticMessageScan
	for rows.Next() {
		var sc agenticMessageScan
		if err := rows.Scan(&sc.Turn, &sc.Role, &sc.SchemaVersion, &sc.Message, &sc.CreatedAt,
			&sc.InputTokens, &sc.OutputTokens, &sc.CacheReadTokens, &sc.CacheWriteTokens,
			&sc.IncompleteReason); err != nil {
			return nil, fmt.Errorf("scan agentic message: %w", err)
		}
		out = append(out, sc)
	}
	return out, nil
}

// repairAgenticTail trims an incomplete agentic tail so the persisted
// history is a valid Eino/Responses resume prefix. It never mutates
// the caller's messages: it returns a new slice referencing the same
// message pointers for the retained prefix; LoadAgenticSession deletes
// the trimmed turns.
//
//   - A trailing user text row is trimmed (no assistant reply yet).
//     Persistence commits user+assistant or complete tool batches
//     atomically, so any trailing user row is an incomplete tail and
//     never a valid durable boundary — including a lone user row,
//     which repair trims entirely (leaving an empty session).
//   - A trailing assistant row with function tool calls is trimmed
//     when any call lacks a complete matching result: the incomplete
//     pair is discarded together rather than exposing half of a
//     function-call/result batch.
//   - A trailing assistant row without calls is the canonical valid
//     tail and is retained.
//
// A complete tool-result batch is retained ONLY when the trailing rows
// are one or more consecutive results-only user messages (the pinned
// Eino adapter emits one user AgenticMessage per parallel tool result;
// a coalesced single message carrying the whole batch is also
// accepted) immediately following an assistant message that has
// function calls, and the collected results match every call by ID and
// name exactly, with no duplicate/missing/extra results. The preceding
// assistant may legitimately carry reasoning and generated text
// alongside its calls — the real Responses adapter emits that shape —
// so only the call set and the result batch must match exactly.
// Eino/Responses can resume from that exact batch. Anything else —
// mixed user text+results, mismatched names, extra/missing/duplicate
// results — is trimmed.
//
// The trim is iterative: dropping an incomplete trailing result run
// exposes its preceding assistant tool-call row, which must then also
// be dropped rather than left as an incomplete call tail. Completion
// is re-checked on every iteration.
func repairAgenticTail(msgs []StoredMessage) []StoredMessage {
	out := msgs
	for len(out) > 0 {
		last := out[len(out)-1]
		switch last.Message.Role {
		case schema.AgenticRoleTypeUser:
			mask := agenticBlockTypes(last.Message)
			switch {
			case mask == agenticOnlyUserText:
				// Pending user text never has a committed assistant reply:
				// persistence commits user+assistant (or complete tool
				// batches) atomically, so this row is an incomplete tail.
				// Iterating also handles the lone-user case, trimming the
				// whole session back to an empty (fresh) history.
				out = out[:len(out)-1]
				continue
			case mask&agenticHasResults != 0:
				runLen, keep := agenticCompleteResultRun(out)
				if keep {
					return out
				}
				if runLen == 0 {
					runLen = 1
				}
				out = out[:len(out)-runLen]
				continue
			default:
				out = out[:len(out)-1]
				continue
			}
		case schema.AgenticRoleTypeAssistant:
			// Assistant text/reasoning without calls is the canonical
			// valid terminal. An assistant WITH calls is only a valid
			// tail when the row after it is a complete result batch —
			// but then that user row would be last, so an assistant
			// with calls at the end is always incomplete: drop it and
			// re-check what it exposes.
			if agenticBlockTypes(last.Message)&agenticHasCalls == 0 {
				return out
			}
			out = out[:len(out)-1]
			continue
		default:
			out = out[:len(out)-1]
			continue
		}
	}
	return out
}

// agenticBlockKind is a bitmask of which high-level block families a
// message contains, computed without a second JSON round-trip.
type agenticBlockKind uint8

const (
	agenticHasUserText agenticBlockKind = 1 << iota
	agenticHasAssistantText
	agenticHasReasoning
	agenticHasCalls
	agenticHasResults
)

// agenticOnlyUserText is the mask for a plain trailing user text row.
const agenticOnlyUserText = agenticHasUserText

func agenticBlockTypes(msg *schema.AgenticMessage) agenticBlockKind {
	var mask agenticBlockKind
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		switch block.Type {
		case schema.ContentBlockTypeUserInputText:
			mask |= agenticHasUserText
		case schema.ContentBlockTypeAssistantGenText:
			mask |= agenticHasAssistantText
		case schema.ContentBlockTypeReasoning:
			mask |= agenticHasReasoning
		case schema.ContentBlockTypeFunctionToolCall:
			mask |= agenticHasCalls
		case schema.ContentBlockTypeFunctionToolResult:
			mask |= agenticHasResults
		}
	}
	return mask
}

// agenticCompleteResultRun inspects the trailing rows of msgs. It
// returns the length of the trailing run of consecutive results-only
// user messages and whether that run, combined with the immediately
// preceding assistant message's function calls, forms a complete
// function-tool-result batch. A batch is complete when every call has
// exactly one result, every result has a matching call, names agree,
// and there are no duplicate or extra results. Each trailing user
// message must contain ONLY function-tool-result blocks; the preceding
// assistant may legitimately carry reasoning and generated text
// alongside its calls.
func agenticCompleteResultRun(msgs []StoredMessage) (runLen int, keep bool) {
	n := len(msgs)
	for i := n - 1; i >= 0; i-- {
		m := msgs[i].Message
		if m.Role != schema.AgenticRoleTypeUser || agenticBlockTypes(m) != agenticHasResults {
			break
		}
		runLen++
	}
	if runLen == 0 {
		return 0, false
	}
	predIdx := n - runLen - 1
	if predIdx < 0 {
		return runLen, false
	}
	pred := msgs[predIdx].Message
	if pred.Role != schema.AgenticRoleTypeAssistant || agenticBlockTypes(pred)&agenticHasCalls == 0 {
		return runLen, false
	}
	calls := AgenticMessageToolCalls(pred)
	if len(calls) == 0 {
		return runLen, false
	}
	var results []schema.FunctionToolResult
	for i := predIdx + 1; i < n; i++ {
		results = append(results, AgenticMessageToolResults(msgs[i].Message)...)
	}
	if len(results) == 0 || !agenticExactBatch(calls, results) {
		return runLen, false
	}
	return runLen, true
}

// agenticExactBatch validates that results is a full batch for calls:
// the same call IDs and names, no duplicate call IDs or results, no
// missing or extra results.
func agenticExactBatch(calls []schema.FunctionToolCall, results []schema.FunctionToolResult) bool {
	seen := make(map[string]string, len(calls))
	for _, call := range calls {
		seen[call.CallID] = call.Name
	}
	if len(seen) != len(calls) {
		return false // duplicate call IDs in the assistant message
	}
	if len(results) != len(calls) {
		return false
	}
	matched := make(map[string]bool, len(calls))
	for _, result := range results {
		name, ok := seen[result.CallID]
		if !ok || name != result.Name {
			return false
		}
		if matched[result.CallID] {
			return false // duplicate result for one call
		}
		matched[result.CallID] = true
	}
	return len(matched) == len(calls)
}
