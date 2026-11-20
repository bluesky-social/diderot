package pagerduty

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "pagerduty"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `TRUNCATE pagerduty_investigations`)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM ask_slack_sessions WHERE channel_id='CBOTALERTS'`)
	require.NoError(t, err)
	return NewStore(pool), pool
}

func TestStoreDeduplicationAndFencedRecovery(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	event := testEvent()
	require.NoError(t, store.Enqueue(ctx, event))
	require.NoError(t, store.Enqueue(ctx, event))
	duplicate := event
	duplicate.ID = "different-subscription-event"
	require.NoError(t, store.Enqueue(ctx, duplicate))
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pagerduty_investigations`).Scan(&count))
	require.Equal(t, 1, count)
	job, err := store.Claim(ctx)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, event, job.Event)
	empty, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Nil(t, empty)
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id))
	require.NoError(t, store.SaveSession(ctx, job, id))
	require.NoError(t, store.SaveOutput(ctx, job, "saved report", 7))
	_, err = pool.Exec(ctx, `UPDATE pagerduty_investigations SET lease_until=now()-interval '1 second'`)
	require.NoError(t, err)
	recovered, err := store.Claim(ctx)
	require.NoError(t, err)
	require.NotNil(t, recovered)
	require.NotEqual(t, job.Token, recovered.Token)
	require.Equal(t, &id, recovered.SessionID)
	require.Equal(t, "saved report", *recovered.Output)
	require.Equal(t, 7, recovered.AssistantTurn)
	require.Equal(t, 1, recovered.Attempts)
	require.Equal(t, 2, recovered.DeliveryAttempts)
	require.ErrorIs(t, store.SaveOutput(ctx, job, "stale overwrite", 99), ErrLeaseLost)
	require.ErrorIs(t, store.Complete(ctx, recovered), ErrLeaseLost)
	require.NoError(t, store.SaveNote(ctx, recovered, "NOTE1"))
	require.NoError(t, store.SaveSlackMessage(ctx, recovered, "1234.000001"))
	require.NoError(t, store.Complete(ctx, recovered))
	empty, err = store.Claim(ctx)
	require.NoError(t, err)
	require.Nil(t, empty)
	event.ID = "new-trigger"
	event.OccurredAt = event.OccurredAt.Add(time.Hour)
	require.NoError(t, store.Enqueue(ctx, event))
	newJob, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "new-trigger", newJob.Event.ID)
}

func TestStoreClaimsAreExclusiveAcrossWorkers(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	require.NoError(t, store.Enqueue(ctx, testEvent()))
	var group sync.WaitGroup
	jobs := make(chan *Job, 2)
	failures := make(chan error, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			job, err := store.Claim(ctx)
			failures <- err
			jobs <- job
		}()
	}
	group.Wait()
	close(jobs)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	count := 0
	for job := range jobs {
		if job != nil {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestStoreExhaustedCrashedJobBecomesFailed(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	require.NoError(t, store.Enqueue(ctx, testEvent()))
	_, err := pool.Exec(ctx, `UPDATE pagerduty_investigations SET state='running', attempts=$1,
		worker_token=$2, lease_until=now()-interval '1 second'`, maxAttempts, uuid.New())
	require.NoError(t, err)
	job, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Nil(t, job)
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM pagerduty_investigations`).Scan(&state))
	require.Equal(t, "failed", state)
}

func TestStoreRetriesBackOffAndExhaustSafely(t *testing.T) {
	store, pool := testStore(t)
	ctx := t.Context()
	require.NoError(t, store.Enqueue(ctx, testEvent()))
	job, err := store.Claim(ctx)
	require.NoError(t, err)
	failure := errors.New("prefix" + strings.Repeat("💥", 700))
	require.NoError(t, store.Retry(ctx, job, failure))
	var state, message string
	var backedOff bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT state, next_attempt_at > now(), last_error FROM pagerduty_investigations`).Scan(&state, &backedOff, &message))
	require.Equal(t, "pending", state)
	require.True(t, backedOff)
	require.LessOrEqual(t, len(message), 2000)
	empty, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Nil(t, empty)
	_, err = pool.Exec(ctx, `UPDATE pagerduty_investigations SET attempts=$1, next_attempt_at=now()-interval '1 second'`, maxAttempts-1)
	require.NoError(t, err)
	finalJob, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, maxAttempts, finalJob.Attempts)
	require.NoError(t, store.Retry(ctx, finalJob, errors.New("still unavailable")))
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM pagerduty_investigations`).Scan(&state))
	require.Equal(t, "failed", state)
}

func TestStoreDeliveryRetriesHaveIndependentBudgetAndCheckpoints(t *testing.T) {
	store, pool := testStore(t)
	ctx := t.Context()
	require.NoError(t, store.Enqueue(ctx, testEvent()))
	_, err := pool.Exec(ctx, `UPDATE pagerduty_investigations SET attempts=$1`, maxAttempts-1)
	require.NoError(t, err)
	job, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, maxAttempts, job.Attempts)
	require.NoError(t, store.SaveOutput(ctx, job, "saved analysis", 7))
	require.NoError(t, store.SaveNote(ctx, job, "NOTE1"))
	require.NoError(t, store.SaveSlackTarget(ctx, job, "CBOTALERTS", "1234.001"))
	output := "saved analysis"
	job.Output, job.DeliveryAttempts = &output, 1
	require.NoError(t, store.Retry(ctx, job, ErrSlackThreadNotFound))
	for attempt := 2; attempt <= maxDeliveryAttempts; attempt++ {
		_, err := pool.Exec(ctx, `UPDATE pagerduty_investigations SET next_attempt_at=now()-interval '1 second'`)
		require.NoError(t, err)
		job, err = store.Claim(ctx)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, maxAttempts, job.Attempts)
		require.Equal(t, attempt, job.DeliveryAttempts)
		require.Equal(t, output, *job.Output)
		require.Equal(t, "NOTE1", job.NoteID)
		require.Equal(t, "1234.001", job.SlackThreadTS)
		require.ErrorIs(t, store.SaveSlackTarget(ctx, job, "CBOTALERTS", "wrong-thread"), ErrLeaseLost)
		require.NoError(t, store.Retry(ctx, job, ErrSlackThreadNotFound))
	}
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM pagerduty_investigations`).Scan(&state))
	require.Equal(t, "failed", state)
}

// TestEnqueueStoresEventWithNulByteInAModeledStringField: the contract (a
// redelivered webhook starts one investigation) implies every
// validly-signed, well-formed incident.triggered event starts exactly one,
// not zero. Trigger: PagerDuty's v3 webhook JSON contains a `\u0000` escape
// in a string field of the event (here, the incident title); Go's
// encoding/json decodes that into a Go string holding a literal NUL byte.
// Before the fix, Enqueue's json.Marshal re-encodes the NUL as `\u0000`,
// and Postgres's jsonb column rejects that escape outright (SQLSTATE
// 22P05), so the INSERT never happens. Because the failure is
// deterministic, every one of PagerDuty's redeliveries over its retry
// window fails the exact same way, and the alert is silently never
// investigated: not succeeded, not failed, not skipped, just absent.
func TestEnqueueStoresEventWithNulByteInAModeledStringField(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	event := testEvent()
	event.Data.Title = "disk usage alert: path=/var/log/app\x00.log is full"

	require.NoError(t, store.Enqueue(ctx, event))

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pagerduty_investigations`).Scan(&count))
	require.Equal(t, 1, count)

	var stored []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT event FROM pagerduty_investigations`).Scan(&stored))
	var decoded Event
	require.NoError(t, json.Unmarshal(stored, &decoded))
	require.Equal(t, "disk usage alert: path=/var/log/app�.log is full", decoded.Data.Title)
}

// TestEnqueueStoresEventWithNulByteInEventID: same mechanism as
// TestEnqueueStoresEventWithNulByteInAModeledStringField, but in the
// webhook event's own id, which is also the primary-key `event_id text`
// column rather than a field inside the jsonb `event` column. Postgres's
// text type rejects an embedded NUL byte outright (SQLSTATE 22021), so
// without stripping it there the INSERT fails the same deterministic way
// on every redelivery.
func TestEnqueueStoresEventWithNulByteInEventID(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	event := testEvent()
	event.ID = "sub-123\x00evil"

	require.NoError(t, store.Enqueue(ctx, event))

	var storedID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT event_id FROM pagerduty_investigations`).Scan(&storedID))
	require.Equal(t, "sub-123�evil", storedID)
}

// TestRetryStoresFailureMessageWithNulByte: Store.Retry already calls
// strings.ToValidUTF8 on the failure message for a related but distinct
// hazard (invalid UTF-8), but a literal NUL byte is valid UTF-8, so it
// passed through unstripped. Trigger: a failure whose error text contains
// a NUL byte (for example one echoing back external data). Postgres's
// text column (last_error) rejects an embedded NUL outright (SQLSTATE
// 22021), so without stripping it the Retry UPDATE itself fails instead of
// recording the backoff, leaving the job stuck running until its lease
// expires.
func TestRetryStoresFailureMessageWithNulByte(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	require.NoError(t, store.Enqueue(ctx, testEvent()))
	job, err := store.Claim(ctx)
	require.NoError(t, err)

	failure := errors.New("upstream said: request id=\x00abc123 failed")
	require.NoError(t, store.Retry(ctx, job, failure))

	var message string
	require.NoError(t, pool.QueryRow(ctx, `SELECT last_error FROM pagerduty_investigations`).Scan(&message))
	require.Equal(t, "upstream said: request id=�abc123 failed", message)
}
