package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// State is one source's own durable state, such as a sync cursor or a
// pending request: a JSON value per key, kept across runs and shared by
// every replica. The zero State, which a Deps not made by Deps.For has,
// returns ErrNoState.
type State struct {
	pool   *pgxpool.Pool
	source string
}

// ErrNoState is the error of a State that names no source or database.
var ErrNoState = errors.New("source state: no source or database; make the Deps with Deps.For")

// NewState returns the state of the source named name.
func NewState(pool *pgxpool.Pool, name string) State {
	return State{pool: pool, source: name}
}

// Get reads key's value into value. It reports false, leaving value as it
// was, when key has none.
func (s State) Get(ctx context.Context, key string, value any) (bool, error) {
	if s.pool == nil || s.source == "" {
		return false, ErrNoState
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM source_state WHERE source = $1 AND key = $2`, s.source, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return false, fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	return true, nil
}

// Update changes key's value in one transaction that holds it against every
// other Update and Set: it reads the value into value (leaving it as it was
// when key has none, which found says), calls change, and saves value
// unless change returns an error, which Update returns.
func (s State) Update(ctx context.Context, key string, value any, change func(found bool) error) error {
	if s.pool == nil || s.source == "" {
		return ErrNoState
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The row may not exist yet; insert it empty first so FOR UPDATE has a
	// row to hold.
	if _, err := tx.Exec(ctx, `
		INSERT INTO source_state (source, key, value) VALUES ($1, $2, 'null')
		ON CONFLICT (source, key) DO NOTHING
	`, s.source, key); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM source_state WHERE source = $1 AND key = $2 FOR UPDATE`, s.source, key).Scan(&raw); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	found := string(raw) != "null"
	if found {
		if err := json.Unmarshal(raw, value); err != nil {
			return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
		}
	}
	if err := change(found); err != nil {
		return err
	}
	if raw, err = json.Marshal(value); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	// value holds whatever upstream text a source's cursor or directory
	// carries (a Discord thread or category name, say): make it storable
	// the same way the event sink does, so a NUL byte here fails the same
	// way it would in events.raw instead of permanently failing this
	// source's every future Set/Update (SQLSTATE 22P05, source_state.value
	// is jsonb).
	raw = events.SanitizeJSON(raw)
	if _, err := tx.Exec(ctx, `UPDATE source_state SET value = $3, updated_at = now() WHERE source = $1 AND key = $2`,
		s.source, key, raw); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	return nil
}

// Set saves value, as JSON, as key's value.
func (s State) Set(ctx context.Context, key string, value any) error {
	if s.pool == nil || s.source == "" {
		return ErrNoState
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	// See the matching comment in Update: make value storable before the
	// jsonb write reaches Postgres.
	raw = events.SanitizeJSON(raw)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO source_state (source, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (source, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, s.source, key, raw); err != nil {
		return fmt.Errorf("source state %s/%s: %w", s.source, key, err)
	}
	return nil
}
