package askchat

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AnswerStore is where delivered answer chunks are registered, so that a
// reaction on one resolves to its session and turn, and where those
// reactions are kept as feedback (ask_answer_messages, ask_feedback). Rows
// are keyed by a surface's channel and message IDs.
type AnswerStore struct{ Pool *pgxpool.Pool }

// Insert registers a posted answer chunk; chunkIndex is 0-based.
func (s *AnswerStore) Insert(ctx context.Context, channelID, messageID, sessionID string, turn, chunkIndex int) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO ask_answer_messages (channel_id, message_ts, session_id, turn, chunk_index)
		VALUES ($1, $2, $3::uuid, $4, $5)
		ON CONFLICT (channel_id, message_ts) DO NOTHING
	`, channelID, messageID, sessionID, turn, chunkIndex)
	return err
}

// Lookup resolves a message to the session and turn whose answer it is;
// found is false for any other message, the common case.
func (s *AnswerStore) Lookup(ctx context.Context, channelID, messageID string) (sessionID string, turn int, found bool, err error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT session_id::text, turn
		  FROM ask_answer_messages
		 WHERE channel_id = $1 AND message_ts = $2
	`, channelID, messageID)
	if scanErr := row.Scan(&sessionID, &turn); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, scanErr
	}
	return sessionID, turn, true, nil
}

// UpsertFeedback records a rating of an answer chunk: inserted, or brought
// back and corrected. added_at is when the rating was given, so it moves
// only for a new one: a re-add after a retract, or a changed verdict. A
// sweep that sees a live rating again leaves its time alone, also when it
// corrects a verdict stored as "other". The chunk's message is part of the
// key, so ratings of an answer's chunks are kept apart.
func (s *AnswerStore) UpsertFeedback(ctx context.Context, sessionID string, turn int, channelID, messageID, reactor, emoji, verdict string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO ask_feedback (session_id, turn, channel_id, message_ts, reactor, emoji, verdict, added_at, removed_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, now(), NULL)
		ON CONFLICT (session_id, turn, message_ts, reactor, emoji)
		DO UPDATE SET added_at = CASE WHEN ask_feedback.removed_at IS NULL
		                                   AND ask_feedback.verdict IN (EXCLUDED.verdict, 'other')
		                              THEN ask_feedback.added_at ELSE now() END,
		              removed_at = NULL, verdict = EXCLUDED.verdict
	`, sessionID, turn, channelID, messageID, reactor, emoji, verdict)
	return err
}

// StampFeedbackRemoved marks a rating retracted; no-op when there is none.
func (s *AnswerStore) StampFeedbackRemoved(ctx context.Context, sessionID string, turn int, messageID, reactor, emoji string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE ask_feedback
		   SET removed_at = now()
		 WHERE session_id = $1::uuid
		   AND turn = $2
		   AND message_ts = $3
		   AND reactor = $4
		   AND emoji = $5
		   AND removed_at IS NULL
	`, sessionID, turn, messageID, reactor, emoji)
	return err
}

// Claim takes a question for one turn: a redelivered question finds its key
// taken (ask_turn_claims). It reports "claimed", "duplicate", or "error";
// on an error the caller answers anyway, since failing closed would drop
// real questions on every database blip.
func Claim(ctx context.Context, pool *pgxpool.Pool, channelID, key string) (string, error) {
	tag, err := pool.Exec(ctx,
		`INSERT INTO ask_turn_claims (channel_id, message_ts) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		channelID, key)
	switch {
	case err != nil:
		return "error", err
	case tag.RowsAffected() == 0:
		return "duplicate", nil
	}
	return "claimed", nil
}
