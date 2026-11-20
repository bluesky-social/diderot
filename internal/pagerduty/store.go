package pagerduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrLeaseLost = errors.New("pagerduty investigation lease lost")

const maxAttempts = 5
const maxDeliveryAttempts = 12

type Job struct {
	Event            Event
	Token            uuid.UUID
	SessionID        *uuid.UUID
	Output           *string
	AssistantTurn    int
	Attempts         int
	DeliveryAttempts int
	NoteID           string
	SlackChannelID   string
	SlackThreadTS    string
	SlackMessageTS   string
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (store *Store) Enqueue(ctx context.Context, event Event) error {
	event = stripNULFromEvent(event)
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO pagerduty_investigations
		(event_id, incident_id, occurred_at, event) VALUES ($1,$2,$3,$4)
		ON CONFLICT DO NOTHING`, event.ID, event.Data.ID, event.OccurredAt, body)
	return err
}

// stripNULFromEvent returns a copy of event with any NUL byte in its string
// fields replaced by U+FFFD. Go's encoding/json freely decodes a `\u0000`
// escape into a Go string holding a literal NUL and re-encodes it just as
// freely, but Postgres's text and jsonb types reject that byte outright
// (SQLSTATES 22021 and 22P05). Left unstripped, a NUL anywhere in the event
// makes Enqueue's INSERT fail deterministically, so every one of PagerDuty's
// identical redeliveries fails the same way and the alert is never recorded
// at all: an alert must never be dropped because of a byte Postgres cannot
// store.
func stripNULFromEvent(event Event) Event {
	event.ID = stripNUL(event.ID)
	event.Data.ID = stripNUL(event.Data.ID)
	event.Data.Title = stripNUL(event.Data.Title)
	event.Data.HTMLURL = stripNUL(event.Data.HTMLURL)
	event.Data.Status = stripNUL(event.Data.Status)
	event.Data.Urgency = stripNUL(event.Data.Urgency)
	event.Data.Service.ID = stripNUL(event.Data.Service.ID)
	event.Data.Service.Summary = stripNUL(event.Data.Service.Summary)
	event.Data.Service.Name = stripNUL(event.Data.Service.Name)
	event.Data.Service.HTMLURL = stripNUL(event.Data.Service.HTMLURL)
	return event
}

// stripNUL replaces every NUL byte in s with U+FFFD, leaving s untouched
// (and avoiding an allocation) when it holds none.
func stripNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "�")
}

func (store *Store) Claim(ctx context.Context) (*Job, error) {
	_, err := store.pool.Exec(ctx, `UPDATE pagerduty_investigations
		SET state='failed', worker_token=NULL, lease_until=NULL,
		last_error='investigation exhausted attempts after worker loss'
		WHERE state='running' AND lease_until < now() AND
		((output IS NULL AND attempts >= $1) OR (output IS NOT NULL AND delivery_attempts >= $2))`, maxAttempts, maxDeliveryAttempts)
	if err != nil {
		return nil, err
	}
	job := &Job{Token: uuid.New()}
	var raw []byte
	err = store.pool.QueryRow(ctx, `UPDATE pagerduty_investigations SET
		state='running', worker_token=$1, lease_until=now()+interval '20 minutes',
		attempts=attempts+CASE WHEN output IS NULL THEN 1 ELSE 0 END,
		delivery_attempts=delivery_attempts+CASE WHEN output IS NOT NULL THEN 1 ELSE 0 END
		WHERE event_id=(SELECT event_id FROM pagerduty_investigations
			WHERE ((output IS NULL AND attempts < $2) OR (output IS NOT NULL AND delivery_attempts < $3))
			AND ((state='pending' AND next_attempt_at <= now())
				OR (state='running' AND lease_until < now()))
			ORDER BY next_attempt_at, created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING event, session_id, output, assistant_turn, attempts, delivery_attempts, COALESCE(note_id,''),
		COALESCE(slack_channel_id,''), COALESCE(slack_thread_ts,''), COALESCE(slack_message_ts,'')`, job.Token, maxAttempts, maxDeliveryAttempts).
		Scan(&raw, &job.SessionID, &job.Output, &job.AssistantTurn, &job.Attempts, &job.DeliveryAttempts,
			&job.NoteID, &job.SlackChannelID, &job.SlackThreadTS, &job.SlackMessageTS)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &job.Event); err != nil {
		return nil, fmt.Errorf("decode stored event: %w", err)
	}
	return job, nil
}

func (store *Store) SaveSession(ctx context.Context, job *Job, id uuid.UUID) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET session_id=$3
		WHERE event_id=$1 AND worker_token=$2 AND state='running'`, id)
}

func (store *Store) SaveOutput(ctx context.Context, job *Job, output string, assistantTurn int) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET output=$3, assistant_turn=$4, delivery_attempts=1
		WHERE event_id=$1 AND worker_token=$2 AND state='running'`, output, assistantTurn)
}

func (store *Store) SaveNote(ctx context.Context, job *Job, noteID string) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET note_id=$3
		WHERE event_id=$1 AND worker_token=$2 AND state='running'`, noteID)
}

func (store *Store) SaveSlackTarget(ctx context.Context, job *Job, channelID, threadTS string) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET slack_channel_id=$3, slack_thread_ts=$4
		WHERE event_id=$1 AND worker_token=$2 AND state='running'
		AND (slack_thread_ts IS NULL OR (slack_channel_id=$3 AND slack_thread_ts=$4))`, channelID, threadTS)
}

func (store *Store) SaveSlackMessage(ctx context.Context, job *Job, messageTS string) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET slack_message_ts=$3
		WHERE event_id=$1 AND worker_token=$2 AND state='running'`, messageTS)
}

func (store *Store) Complete(ctx context.Context, job *Job) error {
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET state='done',
		completed_at=now(), worker_token=NULL, lease_until=NULL, last_error=NULL
		WHERE event_id=$1 AND worker_token=$2 AND state='running'
		AND note_id IS NOT NULL AND slack_message_ts IS NOT NULL`)
}

func (store *Store) Retry(ctx context.Context, job *Job, failure error) error {
	state := "pending"
	attempts, limit := job.Attempts, maxAttempts
	if job.Output != nil {
		attempts, limit = job.DeliveryAttempts, maxDeliveryAttempts
	}
	if attempts >= limit {
		state = "failed"
	}
	delay := min(time.Duration(1<<min(attempts, 8))*time.Minute, 15*time.Minute)
	message := failure.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	// A NUL byte is valid UTF-8, so ToValidUTF8 alone would leave one in
	// place; Postgres's text column (last_error) rejects it outright
	// (SQLSTATE 22021), which would fail this UPDATE instead of recording
	// the backoff.
	message = strings.ToValidUTF8(stripNUL(message), "")
	return store.update(ctx, job, `UPDATE pagerduty_investigations SET state=$3,
		next_attempt_at=now()+($4 * interval '1 second'), last_error=$5,
		worker_token=NULL, lease_until=NULL
		WHERE event_id=$1 AND worker_token=$2 AND state='running'`, state, delay.Seconds(), message)
}

func (store *Store) update(ctx context.Context, job *Job, query string, values ...any) error {
	args := append([]any{job.Event.ID, job.Token}, values...)
	result, err := store.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}
