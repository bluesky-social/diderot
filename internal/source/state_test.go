package source

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestStateUpdateIsAtomic: concurrent Updates of one key each see the last
// one's value, a failed change saves nothing, and a key's first Update finds
// none.
func TestStateUpdateIsAtomic(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "source_state_update"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE source_state`)
	require.NoError(t, err)
	state := NewState(pool, "notes")

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			var n int
			if err := state.Update(ctx, "count", &n, func(bool) error { n++; return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var n int
	found, err := state.Get(ctx, "count", &n)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 20, n)

	failed := errors.New("no")
	require.ErrorIs(t, state.Update(ctx, "count", &n, func(bool) error { n = 99; return failed }), failed)
	_, err = state.Get(ctx, "count", &n)
	require.NoError(t, err)
	require.Equal(t, 20, n)

	var fresh string
	require.ErrorIs(t, state.Update(ctx, "new", &fresh, func(found bool) error {
		require.False(t, found)
		return failed
	}), failed)
	found, err = state.Get(ctx, "new", &fresh)
	require.NoError(t, err)
	require.False(t, found, "a failed first Update leaves no value")
}

// TestStateWithoutForIsAnError: a Deps made without For, or For on a Deps
// with no database, has a State whose use is an ordinary error.
func TestStateWithoutForIsAnError(t *testing.T) {
	ctx := t.Context()
	for name, state := range map[string]State{
		"no For":      Deps{}.State,
		"no database": Deps{}.For("notes").State,
	} {
		var value string
		found, err := state.Get(ctx, "cursor", &value)
		require.ErrorIs(t, err, ErrNoState, name)
		require.False(t, found, name)
		require.ErrorIs(t, state.Set(ctx, "cursor", "page-2"), ErrNoState, name)
	}
}

// TestStateKeepsEachSourcesValues: a value set under a key reads back, a
// later Set replaces it, and one source never sees another's.
func TestStateKeepsEachSourcesValues(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "source_state"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE source_state`)
	require.NoError(t, err)

	type cursor struct {
		After string    `json:"after"`
		At    time.Time `json:"at"`
	}
	notes, other := NewState(pool, "notes"), NewState(pool, "other")
	var got cursor
	found, err := notes.Get(ctx, "cursor", &got)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, cursor{}, got)

	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	require.NoError(t, notes.Set(ctx, "cursor", cursor{After: "page-2", At: at}))
	found, err = notes.Get(ctx, "cursor", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, cursor{After: "page-2", At: at}, got)

	require.NoError(t, notes.Set(ctx, "cursor", cursor{After: "page-3", At: at}))
	found, err = notes.Get(ctx, "cursor", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "page-3", got.After)

	found, err = other.Get(ctx, "cursor", &got)
	require.NoError(t, err)
	require.False(t, found, "another source's key is not this one's")

	deps := Deps{Pool: pool}.For("notes")
	found, err = deps.State.Get(ctx, "cursor", &got)
	require.NoError(t, err)
	require.True(t, found, "For hands a source its own state")
}
