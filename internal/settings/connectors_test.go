package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

const (
	fromEnv = "tok-from-env-123"
	pasted  = "extra-SECRET-456"
)

// syncBuffer is a log sink the store's goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type connectorHarness struct {
	store  *Store
	pool   *pgxpool.Pool
	fake   *fakeConnector
	logs   *syncBuffer
	tested []Resolved
	keys   *Keyring
	// env is the deployment's variables: DIDEROT_EXAMPLE_TOKEN sets token.
	env map[string]string
}

func newConnectorHarness(t *testing.T) *connectorHarness {
	t.Helper()
	_, pool := newTestStore(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM connector_settings WHERE connector <> 'org'`)
	require.NoError(t, err)
	h := &connectorHarness{pool: pool, logs: &syncBuffer{}, env: map[string]string{"DIDEROT_EXAMPLE_TOKEN": fromEnv}}
	h.keys, err = NewKeyring([][]byte{testKey(7)})
	require.NoError(t, err)
	h.fake = &fakeConnector{name: "example", fields: exampleFields(), test: func(r Resolved) TestReport {
		h.tested = append(h.tested, r)
		return TestReport{
			Checks: []Check{
				{Name: "Token", Passed: r.Secret("token") != "", Detail: "used " + r.Secret("token")},
				{Name: "IDs", Passed: !slices.Contains(r.List("ids"), "fail")},
			},
			Facts: []Fact{{Key: "site", Label: "Site", Value: "found.example"}},
		}
	}}
	h.store = h.newStore(h.keys, h.env)
	require.NoError(t, h.store.Load(ctx))
	return h
}

func (h *connectorHarness) newStore(keys *Keyring, env map[string]string) *Store {
	return NewStore(h.pool, slog.New(slog.NewJSONHandler(h.logs, nil))).
		WithConnectors(NewRegistry(h.fake), Secrets{Keys: keys, Env: env})
}

func TestConnectorSaveTestsAndEnables(t *testing.T) {
	h := newConnectorHarness(t)
	ctx := context.Background()
	actor := Actor{Email: "Op@Example.com", RequestID: "req-7"}
	store := h.store

	view, err := store.View(ctx, "example")
	require.NoError(t, err)
	assert.Equal(t, int64(1), view.Version)
	assert.False(t, view.Enabled)
	assert.Nil(t, view.LastTest)
	assert.Equal(t, "member", view.Settings["mode"])
	assert.True(t, view.CanStoreSecrets)
	assert.Equal(t, map[string]SecretView{"token": {Env: "DIDEROT_EXAMPLE_TOKEN"}}, view.Secrets)

	// A save tests what it saved, and a passing test fills discovered
	// fields.
	view, err = store.Save(ctx, actor, "example", 1, map[string]any{"mode": "all", "ids": "U1"}, map[string]SecretInput{
		"extra": {Value: pasted},
	}, false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), view.Version)
	assert.False(t, view.Enabled, "a save alone doesn't enable")
	assert.Equal(t, map[string]SecretView{"token": {Env: "DIDEROT_EXAMPLE_TOKEN"}, "extra": {Saved: true}}, view.Secrets)
	require.NotNil(t, view.LastTest)
	assert.True(t, view.LastTest.Passed)
	assert.Equal(t, "op@example.com", view.LastTest.By)
	assert.Equal(t, "used [secret]", view.LastTest.Checks[0].Detail, "a secret in a report is scrubbed")
	assert.Equal(t, "found.example", view.Settings["site"])
	require.Len(t, h.tested, 1)
	assert.Equal(t, fromEnv, h.tested[0].Secret("token"), "the test reads the deployment's variable")
	assert.Equal(t, pasted, h.tested[0].Secret("extra"), "the test sees the pasted value")
	assert.Equal(t, "Set DIDEROT_EXAMPLE_TOKEN in the deployment to a token", h.tested[0].SupplySecret("token", "a token"),
		"a remedy changes the variable that sets the value")
	assert.Equal(t, "Paste a key", h.tested[0].SupplySecret("extra", "a key"), "and pastes one pasted")
	assert.Equal(t, "all", h.tested[0].String("mode"))

	_, err = store.Save(ctx, actor, "example", 1, map[string]any{"mode": "member"}, nil, true)
	assert.ErrorIs(t, err, ErrConflict)
	_, err = store.Save(ctx, actor, "example", 2, map[string]any{"mode": "nope"}, nil, true)
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, h.tested, 1, "a stale or invalid save tests nothing")

	// Test checks a draft, or the saved settings, and writes nothing.
	record, err := store.Test(ctx, actor, "example", &Draft{Settings: map[string]any{"ids": "fail"}})
	require.NoError(t, err)
	assert.False(t, record.Passed)
	assert.Equal(t, []string{"fail"}, h.tested[1].List("ids"))
	record, err = store.Test(ctx, actor, "example", nil)
	require.NoError(t, err)
	assert.True(t, record.Passed)
	assert.Equal(t, []string{"U1"}, h.tested[2].List("ids"))
	after, err := store.View(ctx, "example")
	require.NoError(t, err)
	assert.Equal(t, int64(2), after.Version)
	assert.Equal(t, view.LastTest.At, after.LastTest.At, "Test keeps no record")

	// Turning it on with a failing test saves the form but stays off.
	view, err = store.Save(ctx, actor, "example", 2, map[string]any{"ids": "fail"}, map[string]SecretInput{
		"extra": {Value: "extra-NEW-789"},
	}, true)
	require.NoError(t, err)
	assert.Equal(t, int64(3), view.Version)
	assert.False(t, view.Enabled)
	assert.False(t, view.LastTest.Passed)
	assert.Equal(t, []string{"fail"}, view.Settings["ids"])
	_, ok := store.Connector("example")
	assert.False(t, ok)

	view, err = store.Save(ctx, actor, "example", 3, map[string]any{"ids": "U1"}, nil, true)
	require.NoError(t, err)
	assert.True(t, view.Enabled, "a passing test enables")
	resolved, ok := store.Connector("example")
	require.True(t, ok, "the writer's cache updates at once")
	assert.Equal(t, fromEnv, resolved.Secret("token"))
	assert.Equal(t, "extra-NEW-789", resolved.Secret("extra"), "the value pasted with the failed attempt was kept")
	assert.Equal(t, []string{"U1"}, resolved.List("ids"))
	assert.Equal(t, "found.example", store.PromptValues()[promptcatalog.VarSlackWorkspaceName])

	// Another replica's cache catches up on its poll.
	other := h.newStore(h.keys, h.env)
	require.NoError(t, other.Load(ctx))
	resolved, ok = other.Connector("example")
	require.True(t, ok)
	assert.Equal(t, int64(4), resolved.Version)

	// A failing test never turns a connector off: it runs the saved
	// settings, and the view says they failed.
	view, err = store.Save(ctx, actor, "example", 4, map[string]any{"ids": "fail"}, nil, false)
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	assert.False(t, view.LastTest.Passed)
	resolved, ok = store.Connector("example")
	require.True(t, ok)
	assert.Equal(t, []string{"fail"}, resolved.List("ids"))

	_, err = store.Disable(ctx, actor, "example", 5)
	require.NoError(t, err)
	_, ok = store.Connector("example")
	assert.False(t, ok)
	assert.Equal(t, "", store.PromptValues()[promptcatalog.VarSlackWorkspaceName], "a disabled connector's variable is its default")

	// Enabling without changes tests the saved settings first.
	view, err = store.Save(ctx, actor, "example", 6, nil, nil, true)
	require.NoError(t, err)
	assert.False(t, view.Enabled, "the saved settings still fail")
	view, err = store.Save(ctx, actor, "example", 7, map[string]any{"ids": ""}, map[string]SecretInput{"extra": {Clear: true}}, true)
	require.NoError(t, err)
	assert.True(t, view.Enabled)

	var actions []string
	var details []map[string]any
	rows, err := h.pool.Query(ctx, `SELECT action, details FROM admin_audit_log WHERE target = 'connector_settings:example' ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var action string
		var raw []byte
		require.NoError(t, rows.Scan(&action, &raw))
		var detail map[string]any
		require.NoError(t, json.Unmarshal(raw, &detail))
		actions, details = append(actions, action), append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"connectors.example.save", "connectors.example.enable", "connectors.example.enable", "connectors.example.save",
		"connectors.example.disable", "connectors.example.enable", "connectors.example.enable",
	}, actions)
	assert.Equal(t, map[string]any{"extra": "changed"}, details[0]["secrets"], "a variable's value is never stored")
	assert.Equal(t, map[string]any{"old": "member", "new": "all"}, details[0]["changes"].(map[string]any)["mode"])
	assert.Equal(t, true, details[0]["test_passed"])
	assert.Equal(t, false, details[1]["test_passed"])
	assert.NotContains(t, details[1], "enabled", "a failed attempt to enable changes no state")
	assert.Equal(t, true, details[2]["enabled"])
	assert.NotContains(t, details[4], "test_passed", "disabling runs no test")
	assert.Equal(t, map[string]any{"extra": "changed"}, details[6]["secrets"], "a removed secret audits as changed")

	assertNoSecret(t, h.pool, h.logs.String())
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	assertNoSecretIn(t, string(encoded), "the view")
}

// TestPastedSecretsNeedConfigKeys: without DIDEROT_CONFIG_KEYS a value
// can't be pasted, and the error says what to set instead.
func TestPastedSecretsNeedConfigKeys(t *testing.T) {
	h := newConnectorHarness(t)
	ctx := context.Background()
	actor := Actor{Email: "op@example.com"}
	noKeys := h.newStore(nil, nil)
	require.NoError(t, noKeys.Load(ctx))

	view, err := noKeys.View(ctx, "example")
	require.NoError(t, err)
	assert.False(t, view.CanStoreSecrets)
	assert.Empty(t, view.Secrets)
	_, err = noKeys.Save(ctx, actor, "example", 1, nil, map[string]SecretInput{"token": {Value: pasted}}, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.ErrorContains(t, err, "token: can't be saved without DIDEROT_CONFIG_KEYS; set it, or set DIDEROT_EXAMPLE_TOKEN")
	assert.NotContains(t, err.Error(), pasted)
	_, err = noKeys.Test(ctx, actor, "example", &Draft{Secrets: map[string]SecretInput{"token": {Value: pasted}}})
	assert.ErrorContains(t, err, "token: can't be saved without DIDEROT_CONFIG_KEYS")

	paste := h.newStore(h.keys, nil)
	require.NoError(t, paste.Load(ctx))
	_, err = paste.Save(ctx, actor, "example", 1, map[string]any{"mode": "all"}, nil, true)
	assert.ErrorContains(t, err, "token: is required", "without its variable, a required secret must be pasted")
	_, err = paste.Save(ctx, actor, "example", 1, nil, map[string]SecretInput{"token": {Value: "  "}}, true)
	assert.ErrorContains(t, err, "token: enter a value")
	_, err = paste.Save(ctx, actor, "example", 1, nil, map[string]SecretInput{"token": {Value: pasted, Clear: true}}, true)
	assert.ErrorContains(t, err, "token: give a value or clear it, not both")
	_, err = paste.View(ctx, "nope")
	assert.ErrorIs(t, err, ErrUnknownConnector)
	_, err = paste.Save(ctx, actor, "nope", 1, nil, nil, true)
	assert.ErrorIs(t, err, ErrUnknownConnector)

	// A pasted secret saved with a key can't be read once the key is gone:
	// the cache treats the connector as off and says why, without the value.
	view, err = paste.Save(ctx, actor, "example", 1, nil, map[string]SecretInput{"token": {Value: pasted}}, true)
	require.NoError(t, err)
	require.True(t, view.Enabled)
	require.NoError(t, noKeys.refresh(ctx, false))
	_, ok := noKeys.Connector("example")
	assert.False(t, ok)
	assert.Contains(t, h.logs.String(), "the saved value can't be read without DIDEROT_CONFIG_KEYS")
	assertNoSecret(t, h.pool, h.logs.String())
}

// TestDeploymentVariableWinsOverPastedValue: once the deployment sets a
// secret's variable, its value is the one used and tested, and the pasted
// one is ignored.
func TestDeploymentVariableWinsOverPastedValue(t *testing.T) {
	h := newConnectorHarness(t)
	ctx := context.Background()
	actor := Actor{Email: "op@example.com"}
	paste := h.newStore(h.keys, nil)
	require.NoError(t, paste.Load(ctx))
	view, err := paste.Save(ctx, actor, "example", 1, nil, map[string]SecretInput{"token": {Value: pasted}}, true)
	require.NoError(t, err)
	require.True(t, view.Enabled)
	assert.Equal(t, pasted, h.tested[len(h.tested)-1].Secret("token"))

	require.NoError(t, h.store.refresh(ctx, false))
	resolved, ok := h.store.Connector("example")
	require.True(t, ok)
	assert.Equal(t, fromEnv, resolved.Secret("token"))
	view, err = h.store.View(ctx, "example")
	require.NoError(t, err)
	assert.Equal(t, SecretView{Env: "DIDEROT_EXAMPLE_TOKEN", Saved: true}, view.Secrets["token"])

	_, err = h.store.Save(ctx, actor, "example", 2, nil, map[string]SecretInput{"token": {Value: "tok-other"}}, false)
	assert.ErrorContains(t, err, "token: is set by DIDEROT_EXAMPLE_TOKEN; change it there")
	view, err = h.store.Save(ctx, actor, "example", 2, nil, map[string]SecretInput{"token": {Clear: true}}, false)
	require.NoError(t, err, "the ignored pasted value can be removed")
	assert.Equal(t, SecretView{Env: "DIDEROT_EXAMPLE_TOKEN"}, view.Secrets["token"])
	assert.Equal(t, fromEnv, h.tested[len(h.tested)-1].Secret("token"))

	// Each replica tests with the value its own variable holds.
	rotated := h.newStore(h.keys, map[string]string{"DIDEROT_EXAMPLE_TOKEN": "tok-rotated-789"})
	require.NoError(t, rotated.Load(ctx))
	_, err = rotated.Disable(ctx, actor, "example", 3)
	require.NoError(t, err)
	view, err = rotated.Save(ctx, actor, "example", 4, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	assert.Equal(t, "tok-rotated-789", h.tested[len(h.tested)-1].Secret("token"))
	assertNoSecret(t, h.pool, h.logs.String())
}

func assertNoSecret(t *testing.T, pool *pgxpool.Pool, logs string) {
	t.Helper()
	var audit, stored string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(details::text, ' '), '') FROM admin_audit_log`).Scan(&audit))
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(settings::text || secrets::text || COALESCE(last_test::text, ''), ' '), '') FROM connector_settings`).Scan(&stored))
	assertNoSecretIn(t, audit, "audit rows")
	assertNoSecretIn(t, stored, "stored rows")
	assertNoSecretIn(t, logs, "logs")
}

func assertNoSecretIn(t *testing.T, text, where string) {
	t.Helper()
	assert.NotContains(t, text, fromEnv, where)
	assert.NotContains(t, text, pasted, where)
}
