package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type postgresCorpusTaskBackend struct {
	pool *pgxpool.Pool
}

func newPostgresCorpusTaskBackend(pool *pgxpool.Pool) *postgresCorpusTaskBackend {
	return &postgresCorpusTaskBackend{pool: pool}
}

func (b *postgresCorpusTaskBackend) Create(ctx context.Context, task *storedCorpusTask) error {
	createdAt, err := time.Parse(time.RFC3339Nano, task.descriptor.CreatedAt)
	if err != nil {
		return fmt.Errorf("parse task creation time: %w", err)
	}
	lastUpdatedAt, err := time.Parse(time.RFC3339Nano, task.descriptor.LastUpdatedAt)
	if err != nil {
		return fmt.Errorf("parse task update time: %w", err)
	}
	_, err = b.pool.Exec(ctx, `
		INSERT INTO mcp_tasks
		       (id, owner_email, status, status_message, created_at, last_updated_at,
		        expires_at, poll_interval_ms)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8)`,
		task.descriptor.TaskID, task.owner, task.descriptor.Status, task.descriptor.StatusMessage,
		createdAt, lastUpdatedAt, task.expiresAt,
		pointerUint64(task.descriptor.PollIntervalMS))
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func (b *postgresCorpusTaskBackend) Get(ctx context.Context, owner, taskID string) (*storedCorpusTask, error) {
	var (
		task                     storedCorpusTask
		statusMessage            *string
		createdAt, lastUpdatedAt time.Time
		pollIntervalMS           *int64
		resultJSON, errorJSON    []byte
	)
	err := b.pool.QueryRow(ctx, `
		SELECT owner_email, status, status_message, created_at, last_updated_at,
		       expires_at, poll_interval_ms, result, error
		  FROM mcp_tasks
		 WHERE id = $1 AND owner_email = $2 AND expires_at > now()`, taskID, owner,
	).Scan(&task.owner, &task.descriptor.Status, &statusMessage, &createdAt, &lastUpdatedAt,
		&task.expiresAt, &pollIntervalMS, &resultJSON, &errorJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errCorpusTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query task: %w", err)
	}
	ttlMS := uint64(task.expiresAt.Sub(createdAt) / time.Millisecond)
	task.descriptor.TaskID = taskID
	task.descriptor.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	task.descriptor.LastUpdatedAt = lastUpdatedAt.UTC().Format(time.RFC3339Nano)
	task.descriptor.TTLMS = &ttlMS
	if pollIntervalMS != nil {
		value := uint64(*pollIntervalMS)
		task.descriptor.PollIntervalMS = &value
	}
	if statusMessage != nil {
		task.descriptor.StatusMessage = *statusMessage
	}
	if len(resultJSON) > 0 {
		if err := json.Unmarshal(resultJSON, &task.result); err != nil {
			return nil, fmt.Errorf("decode task result: %w", err)
		}
	}
	if len(errorJSON) > 0 {
		if err := json.Unmarshal(errorJSON, &task.rpcError); err != nil {
			return nil, fmt.Errorf("decode task error: %w", err)
		}
	}
	return &task, nil
}

func (b *postgresCorpusTaskBackend) Finish(ctx context.Context, owner, taskID, status string, result *mcp.CallToolResult, rpcError *taskRPCError, at time.Time) error {
	resultJSON, err := marshalNullableJSON(result)
	if err != nil {
		return fmt.Errorf("encode task result: %w", err)
	}
	errorJSON, err := marshalNullableJSON(rpcError)
	if err != nil {
		return fmt.Errorf("encode task error: %w", err)
	}
	_, err = b.pool.Exec(ctx, `
		UPDATE mcp_tasks
		   SET status = $4, last_updated_at = $5, poll_interval_ms = NULL,
		       result = $6::jsonb, error = $7::jsonb
		 WHERE id = $1 AND owner_email = $2 AND status = 'working' AND expires_at > $3`,
		taskID, owner, at, status, at, resultJSON, errorJSON)
	if err != nil {
		return fmt.Errorf("finish task: %w", err)
	}
	return nil
}

func (b *postgresCorpusTaskBackend) Cancel(ctx context.Context, owner, taskID string, at time.Time) error {
	tag, err := b.pool.Exec(ctx, `
		UPDATE mcp_tasks
		   SET status = CASE WHEN status = 'working' THEN 'cancelled' ELSE status END,
		       status_message = CASE WHEN status = 'working' THEN 'cancelled by client' ELSE status_message END,
		       last_updated_at = CASE WHEN status = 'working' THEN $3 ELSE last_updated_at END,
		       poll_interval_ms = CASE WHEN status = 'working' THEN NULL ELSE poll_interval_ms END
		 WHERE id = $1 AND owner_email = $2 AND expires_at > $3`, taskID, owner, at)
	if err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errCorpusTaskNotFound
	}
	return nil
}

func (b *postgresCorpusTaskBackend) Delete(ctx context.Context, taskID string) error {
	if _, err := b.pool.Exec(ctx, `DELETE FROM mcp_tasks WHERE id = $1`, taskID); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

func (b *postgresCorpusTaskBackend) CleanupExpired(ctx context.Context) error {
	if _, err := b.pool.Exec(ctx, `DELETE FROM mcp_tasks WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("delete expired tasks: %w", err)
	}
	return nil
}

func pointerUint64(value *uint64) any {
	if value == nil {
		return nil
	}
	return *value
}

// marshalNullableJSON is the one choke point mcp_tasks.result and
// .error (both jsonb columns) are written through. It strips any embedded
// NUL byte (U+0000) before marshaling: Go's json.Marshal encodes a NUL byte
// as the literal escape sequence "\u0000", which Postgres's jsonb input
// function rejects outright (SQLSTATE 22P05). A task's result or error text
// can carry one for the same reason ask_messages text can -- result often
// echoes the same decoded model answer Ask persists there, and an error
// message can quote arbitrary input -- so without this, an otherwise
// successful turn's result was discarded when Finish tried to record it.
func marshalNullableJSON(value any) (any, error) {
	if value == nil || (reflect.ValueOf(value).Kind() == reflect.Pointer && reflect.ValueOf(value).IsNil()) {
		return nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	// Only pay the decode/re-encode cost when a NUL escape is actually
	// present; a false positive on an already-escaped literal "\u0000" in
	// text only costs the safe round trip, never a missed strip.
	if bytes.Contains(b, []byte(`\u0000`)) {
		b, err = stripNULFromJSON(b)
		if err != nil {
			return nil, err
		}
	}
	return string(b), nil
}

// stripNULFromJSON removes any embedded NUL byte from raw's string values
// and re-encodes the result. It walks raw through the generic JSON data
// model rather than the concrete Go types that produced it: several of
// mcp.Content's variants (e.g. *mcp.TextContent) have a custom MarshalJSON,
// which makes mutating their fields directly brittle, while every JSON
// value can be decoded into maps, slices, strings and numbers regardless of
// its original Go type. json.Number preserves numeric literals exactly
// (avoiding float64 precision loss) through the round trip.
func stripNULFromJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode for NUL stripping: %w", err)
	}
	cleaned, err := json.Marshal(stripNULFromJSONValue(v))
	if err != nil {
		return nil, fmt.Errorf("encode after NUL stripping: %w", err)
	}
	return cleaned, nil
}

func stripNULFromJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		return stripNUL(t)
	case []any:
		for i, e := range t {
			t[i] = stripNULFromJSONValue(e)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = stripNULFromJSONValue(e)
		}
		return t
	default:
		return v
	}
}
