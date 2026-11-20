package ask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestPostgresCorpusTaskBackendSharesStateAndEnforcesOwnership(t *testing.T) {
	pool := testPool(t)
	creator := newPostgresCorpusTaskBackend(pool)
	poller := newPostgresCorpusTaskBackend(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	task := &storedCorpusTask{
		owner:     "alex@example.com",
		expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}
	require.NoError(t, creator.Create(t.Context(), task))

	got, err := poller.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "working", got.descriptor.Status)
	require.Equal(t, pollMS, *got.descriptor.PollIntervalMS)
	_, err = poller.Get(t.Context(), "mallory@example.com", taskID)
	require.ErrorIs(t, err, errCorpusTaskNotFound)

	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}
	require.NoError(t, creator.Finish(context.Background(), "alex@example.com", taskID, "completed", result, nil, now.Add(time.Second)))
	got, err = poller.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "completed", got.descriptor.Status)
	require.Nil(t, got.descriptor.PollIntervalMS)
	require.Equal(t, "done", got.result.Content[0].(*mcp.TextContent).Text)
}

// TestPostgresCorpusTaskBackendFinishPersistsResultWithEmbeddedNulBytes is a
// regression test: Finish marshals result/error with marshalNullableJSON
// and writes the JSON text into mcp_tasks.result/.error, jsonb columns.
// Go's json.Marshal encodes an embedded NUL byte (U+0000) as the literal
// escape sequence "\u0000", which Postgres's jsonb input function rejects
// (SQLSTATE 22P05, "unsupported Unicode escape sequence") -- the same
// failure mode as ask_messages, since mcp_tasks.result can carry the same
// decoded structured answer text.
//
// Trigger: a CallToolResult whose text content contains a literal NUL
// byte, e.g. echoing the final answer text of an ask turn.
func TestPostgresCorpusTaskBackendFinishPersistsResultWithEmbeddedNulBytes(t *testing.T) {
	pool := testPool(t)
	backend := newPostgresCorpusTaskBackend(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	require.NoError(t, backend.Create(t.Context(), &storedCorpusTask{
		owner: "alex@example.com", expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}))

	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "the value is \x00 end"}}}
	err := backend.Finish(context.Background(), "alex@example.com", taskID, "completed", result, nil, now.Add(time.Second))
	require.NoError(t, err, "a result the task store accepts must be durably persistable; "+
		"instead the whole task result was discarded on an unvalidated embedded NUL byte")

	got, err := backend.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "completed", got.descriptor.Status)
	require.Equal(t, "the value is  end", got.result.Content[0].(*mcp.TextContent).Text,
		"the NUL byte must be stripped, not left in place to break the jsonb write")
}

// TestPostgresCorpusTaskBackendFinishPersistsErrorWithEmbeddedNulBytes
// mirrors the result-side regression above for taskRPCError.Message, which
// goes through the same marshalNullableJSON call and mcp_tasks.error jsonb
// column.
func TestPostgresCorpusTaskBackendFinishPersistsErrorWithEmbeddedNulBytes(t *testing.T) {
	pool := testPool(t)
	backend := newPostgresCorpusTaskBackend(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	require.NoError(t, backend.Create(t.Context(), &storedCorpusTask{
		owner: "alex@example.com", expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}))

	rpcError := &taskRPCError{Code: -32603, Message: "model unavailable: \x00 end"}
	err := backend.Finish(context.Background(), "alex@example.com", taskID, "failed", nil, rpcError, now.Add(time.Second))
	require.NoError(t, err, "an error the task store accepts must be durably persistable; "+
		"instead the whole task error was discarded on an unvalidated embedded NUL byte")

	got, err := backend.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "failed", got.descriptor.Status)
	require.Equal(t, "model unavailable:  end", got.rpcError.Message,
		"the NUL byte must be stripped, not left in place to break the jsonb write")
}

func TestPostgresCorpusTaskBackendCancellationIsShared(t *testing.T) {
	pool := testPool(t)
	backend := newPostgresCorpusTaskBackend(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	require.NoError(t, backend.Create(t.Context(), &storedCorpusTask{
		owner: "alex@example.com", expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}))
	require.NoError(t, backend.Cancel(t.Context(), "alex@example.com", taskID, now.Add(time.Second)))
	got, err := backend.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", got.descriptor.Status)
	require.Equal(t, "cancelled by client", got.descriptor.StatusMessage)

	err = backend.Cancel(t.Context(), "mallory@example.com", taskID, now.Add(2*time.Second))
	require.True(t, errors.Is(err, errCorpusTaskNotFound))
}

func TestPostgresCorpusTaskBackendStoresFailedPayload(t *testing.T) {
	pool := testPool(t)
	backend := newPostgresCorpusTaskBackend(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	require.NoError(t, backend.Create(t.Context(), &storedCorpusTask{
		owner: "alex@example.com", expiresAt: now.Add(taskTTL),
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: now.Format(time.RFC3339Nano),
			LastUpdatedAt: now.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}))
	want := &taskRPCError{Code: -32603, Message: "model unavailable"}
	require.NoError(t, backend.Finish(t.Context(), "alex@example.com", taskID, "failed", nil, want, now.Add(time.Second)))
	got, err := backend.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "failed", got.descriptor.Status)
	require.Equal(t, want, got.rpcError)
	require.Nil(t, got.result)
}

func TestPostgresCorpusTaskBackendCleansExpiredRows(t *testing.T) {
	pool := testPool(t)
	backend := newPostgresCorpusTaskBackend(pool)
	createdAt := time.Now().UTC().Add(-2 * taskTTL).Truncate(time.Microsecond)
	expiresAt := createdAt.Add(taskTTL)
	ttlMS := uint64(taskTTL / time.Millisecond)
	pollMS := uint64(taskPollInterval / time.Millisecond)
	taskID := uuid.NewString()
	require.NoError(t, backend.Create(t.Context(), &storedCorpusTask{
		owner: "alex@example.com", expiresAt: expiresAt,
		descriptor: taskDescriptor{
			TaskID: taskID, Status: "working", CreatedAt: createdAt.Format(time.RFC3339Nano),
			LastUpdatedAt: createdAt.Format(time.RFC3339Nano), TTLMS: &ttlMS, PollIntervalMS: &pollMS,
		},
	}))
	require.NoError(t, backend.CleanupExpired(t.Context()))
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_tasks WHERE id = $1`, taskID).Scan(&count))
	require.Zero(t, count)
}
