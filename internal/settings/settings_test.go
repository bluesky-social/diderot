package settings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "settings"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, prompt_releases RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE connector_settings
		   SET settings = '{"ORG_NAME":"","ORG_DESCRIPTION":"","state":"setup","bootstrap_admin_enabled":true}',
		       version = 1
		 WHERE connector = 'org'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE connector = 'models'`)
	require.NoError(t, err)
	store := NewStore(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, store.Load(ctx))
	return store, pool
}

// TestModelsRowStartsEmptyAndWritesAreVersionedAndAudited: an install without
// the models row gets an empty one (every role on its default), and a write
// is checked against the version, bumps it, reaches the writer's cache and
// another replica's on its poll, and is audited.
func TestModelsRowStartsEmptyAndWritesAreVersionedAndAudited(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	row, err := store.ReadModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), row.Version)
	assert.Empty(t, row.Roles)
	assert.Equal(t, DefaultAskMaxToolTurns, row.MaxToolTurns())
	assert.Equal(t, row, store.Models())

	other := NewStore(pool, nil)
	require.NoError(t, other.Load(ctx), "a second replica's Load keeps the row")
	actor := Actor{Email: "op@example.com", RequestID: "req-2"}
	updated, err := other.UpdateModels(ctx, 1, actor, "setup.models.save", func(models *Models) error {
		models.Roles = map[string]string{"ask": "big"}
		models.AskMaxToolTurns = 16
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Version)
	assert.Equal(t, updated, other.Models(), "the writer's own cache updates at once")
	_, err = other.UpdateModels(ctx, 1, actor, "setup.models.save", func(*Models) error { return nil })
	assert.ErrorIs(t, err, ErrConflict)

	_, err = other.UpdateModels(ctx, 2, actor, "setup.models.save", func(models *Models) error {
		models.Roles["titles"] = "small"
		return nil
	})
	require.NoError(t, err)
	var details []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT details FROM admin_audit_log WHERE target = 'connector_settings:models' ORDER BY id DESC LIMIT 1`).Scan(&details))
	var decoded struct {
		Version int64                     `json:"version"`
		Changes map[string]map[string]any `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(details, &decoded))
	assert.Equal(t, int64(3), decoded.Version)
	assert.Equal(t, map[string]map[string]any{"roles": {
		"old": map[string]any{"ask": "big"},
		"new": map[string]any{"ask": "big", "titles": "small"},
	}}, decoded.Changes, "a changed map is audited against the row as read")

	assert.Equal(t, int64(1), store.Models().Version, "the cache serves the old row until the poll")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(runCtx, 10*time.Millisecond)
	}()
	require.Eventually(t, func() bool { return store.Models().Version == 3 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, 16, store.Models().MaxToolTurns())
	assert.Equal(t, map[string]string{"ask": "big", "titles": "small"}, store.Models().Roles)
}

func TestModelsMaxToolTurnsFallsBackOutOfRange(t *testing.T) {
	for turns, want := range map[int]int{0: DefaultAskMaxToolTurns, -1: DefaultAskMaxToolTurns, 1: 1, MaxAskToolTurns: MaxAskToolTurns, MaxAskToolTurns + 1: DefaultAskMaxToolTurns} {
		assert.Equal(t, want, Models{AskMaxToolTurns: turns}.MaxToolTurns(), turns)
	}
}

// TestNewDatabaseStartsInSetup reads the org row a freshly migrated database
// starts with: a new install opens in setup with the bootstrap admin on.
func TestNewDatabaseStartsInSetup(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "settings"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	row, err := readOrg(ctx, pool, "")
	require.NoError(t, err)
	assert.Equal(t, StateSetup, row.State)
	assert.True(t, row.BootstrapAdminEnabled)
	assert.Equal(t, int64(1), row.Version)
	assert.Empty(t, row.Name)
}

// TestChangedByLeavesOutDiderot: pages attribute a row to the
// person who changed it, and to nobody while only Diderot has written it.
func TestChangedByLeavesOutDiderot(t *testing.T) {
	assert.Empty(t, ChangedBy("diderot:migration"))
	assert.Empty(t, ChangedBy(registryActor))
	assert.Empty(t, ChangedBy(""))
	assert.Equal(t, "op@example.com", ChangedBy("op@example.com"))
}

func TestUpdateOrgChecksVersionBumpsAndAudits(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	actor := Actor{Email: "Op@Example.com", RequestID: "req-1"}

	updated, err := store.UpdateOrg(ctx, 1, actor, "setup.org.save", func(org *Org) error {
		org.Name = "Example"
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Version)
	assert.Equal(t, "op@example.com", updated.UpdatedBy)
	assert.Equal(t, "Example", store.OrgName(), "the writer's own cache updates at once")

	_, err = store.UpdateOrg(ctx, 1, actor, "setup.org.save", func(org *Org) error {
		org.Name = "Stale"
		return nil
	})
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, "Example", store.OrgName())

	refused := errors.New("refused")
	_, err = store.UpdateOrg(ctx, 2, actor, "setup.org.save", func(*Org) error { return refused })
	assert.ErrorIs(t, err, refused)

	var action, target, status, requestID string
	var details []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT action, target, status, request_id, details FROM admin_audit_log`).
		Scan(&action, &target, &status, &requestID, &details))
	assert.Equal(t, "setup.org.save", action)
	assert.Equal(t, "connector_settings:org", target)
	assert.Equal(t, "succeeded", status)
	assert.Equal(t, "req-1", requestID)
	var decoded struct {
		Version int64                     `json:"version"`
		Changes map[string]map[string]any `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(details, &decoded))
	assert.Equal(t, int64(2), decoded.Version)
	assert.Equal(t, map[string]map[string]any{"ORG_NAME": {"old": "", "new": "Example"}}, decoded.Changes)

	var audits int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_log`).Scan(&audits))
	assert.Equal(t, 1, audits, "refused writes leave no audit record")
}

func TestForceUpdateOrgIgnoresVersion(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	_, err := store.ForceUpdateOrg(ctx, Actor{Email: "root@host"}, "setup.bootstrap_admin.reset", func(org *Org) error {
		org.BootstrapAdminEnabled = false
		return nil
	})
	require.NoError(t, err)
	enabled, err := store.BootstrapAdminEnabled(ctx)
	require.NoError(t, err)
	assert.False(t, enabled)
}

// TestRefreshPicksUpAnotherReplicasWrite covers the poll: a write through a
// second store reaches the first one's cache on its next refresh.
func TestRefreshPicksUpAnotherReplicasWrite(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	other := NewStore(pool, nil)
	require.NoError(t, other.Load(ctx))
	_, err := other.UpdateOrg(ctx, 1, Actor{Email: "op@example.com"}, "setup.go_live", func(org *Org) error {
		org.State = StateLive
		return nil
	})
	require.NoError(t, err)

	assert.False(t, store.AskAllowed(false), "the cache serves the old state until the poll")
	assert.True(t, store.AskAllowed(true), "administrators may always use Ask")

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(runCtx, 10*time.Millisecond)
	}()
	require.Eventually(t, func() bool { return store.AskAllowed(false) }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, int64(2), store.Org().Version)
}

func TestValidateText(t *testing.T) {
	for _, test := range []struct {
		value, want string
		ok          bool
	}{
		{"  Example Corp ", "Example Corp", true},
		{"", "", true},
		{"two\nlines", "", false},
		{"Hello {{ORG_NAME}}", "", false},
		{"ends }}", "", false},
		{strings.Repeat("x", 201), "", false},
	} {
		got, err := ValidateText("ORG_NAME", test.value, 200)
		if !test.ok {
			assert.ErrorIs(t, err, ErrInvalid, test.value)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, test.want, got)
	}
}
