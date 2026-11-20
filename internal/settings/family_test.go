package settings

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// familyMember is a family row's definition, whose secret field's variable
// is not EnvName's.
type familyMember struct{ *fakeConnector }

func (f familyMember) SecretEnv(field string) string {
	return "DIDEROT_MEMBER_" + strings.ToUpper(strings.TrimPrefix(f.name, "member:")+"_"+field)
}

func newFamilyStore(t *testing.T, h *connectorHarness, env map[string]string) *Store {
	t.Helper()
	store := NewStore(h.pool, slog.New(slog.NewJSONHandler(h.logs, nil))).
		WithConnectors(NewRegistry(h.fake), Secrets{Keys: h.keys, Env: env}).
		WithFamily(Family{Prefix: "member:", Define: func(row string) Definition {
			return familyMember{&fakeConnector{name: row, fields: []Field{
				{Key: "token", Label: "Token", Kind: KindSecret},
				{Key: "url", Label: "URL", Kind: KindText},
			}}}
		}})
	require.NoError(t, store.Load(context.Background()))
	return store
}

// TestFamilyRowsAreCreatedSavedCachedAndRemoved: a family's rows are added
// and removed by an administrator, each audited, and reach another
// replica's cache through its poll, removal included.
func TestFamilyRowsAreCreatedSavedCachedAndRemoved(t *testing.T) {
	h := newConnectorHarness(t)
	ctx := context.Background()
	actor := Actor{Email: "op@example.com"}
	env := map[string]string{"DIDEROT_MEMBER_TWO_TOKEN": fromEnv}
	store, other := newFamilyStore(t, h, env), newFamilyStore(t, h, env)

	_, err := store.Create(ctx, actor, "unregistered", nil)
	require.ErrorIs(t, err, ErrUnknownConnector)
	_, err = store.Create(ctx, actor, "member:", nil)
	require.ErrorIs(t, err, ErrUnknownConnector, "the prefix alone names no row")

	view, err := store.Create(ctx, actor, "member:one", map[string]any{"url": "https://one.example"})
	require.NoError(t, err)
	assert.False(t, view.Enabled)
	assert.Equal(t, "https://one.example", view.Settings["url"])
	assert.Equal(t, "DIDEROT_MEMBER_ONE_TOKEN", view.Fields[0].Env)
	_, err = store.Create(ctx, actor, "member:one", nil)
	require.ErrorIs(t, err, ErrExists)

	view, err = store.Save(ctx, actor, "member:one", view.Version, nil, map[string]SecretInput{"token": {Value: pasted}}, true)
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	_, err = store.Create(ctx, actor, "member:two", nil)
	require.NoError(t, err)
	two, err := store.View(ctx, "member:two")
	require.NoError(t, err)
	assert.Equal(t, SecretView{Env: "DIDEROT_MEMBER_TWO_TOKEN"}, two.Secrets["token"], "a family row's variable sets its secret")

	listed, err := other.ListFamily(ctx, "member:")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, []string{"member:one", "member:two"}, []string{listed[0].Name, listed[1].Name})

	require.NoError(t, other.refresh(ctx, false))
	rows := other.FamilyRows("member:")
	require.Len(t, rows, 2)
	assert.True(t, rows[0].Row.Enabled)
	assert.Equal(t, pasted, rows[0].Resolved.Secret("token"), "another replica reads the pasted secret")

	require.ErrorIs(t, store.Delete(ctx, actor, "member:one", view.Version-1), ErrConflict)
	require.NoError(t, store.Delete(ctx, actor, "member:one", view.Version))
	_, err = store.View(ctx, "member:one")
	require.ErrorIs(t, err, ErrUnknownConnector)
	require.NoError(t, other.refresh(ctx, false))
	rows = other.FamilyRows("member:")
	require.Len(t, rows, 1, "a removed row leaves the other replica's cache")
	assert.Equal(t, "member:two", rows[0].Row.Connector)

	// Made again under the removed name, a row is newer than every version
	// of the removed one, so no cache keeps the removed one.
	again, err := store.Create(ctx, actor, "member:one", nil)
	require.NoError(t, err)
	assert.Greater(t, again.Version, view.Version)

	var actions []string
	rowsOf, err := h.pool.Query(ctx, `SELECT action FROM admin_audit_log WHERE target LIKE 'connector_settings:member:%' ORDER BY id`)
	require.NoError(t, err)
	for rowsOf.Next() {
		var action string
		require.NoError(t, rowsOf.Scan(&action))
		actions = append(actions, action)
	}
	require.NoError(t, rowsOf.Err())
	assert.Equal(t, []string{
		"connectors.member:one.create", "connectors.member:one.enable", "connectors.member:two.create",
		"connectors.member:one.delete", "connectors.member:one.create",
	}, actions)
}
