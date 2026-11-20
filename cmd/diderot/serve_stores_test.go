package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestQdrantKeyInPlaintext: the server warns when the Qdrant API key would
// cross a network without TLS, and only then.
func TestQdrantKeyInPlaintext(t *testing.T) {
	for _, test := range []struct {
		addr, key string
		tls, want bool
	}{
		{addr: "qdrant.example.svc.cluster.local:6334", key: "k", want: true},
		{addr: "10.0.0.5:6334", key: "k", want: true},
		{addr: "qdrant.example.svc.cluster.local:6334", key: "k", tls: true},
		{addr: "qdrant.example.svc.cluster.local:6334"},
		{addr: "localhost:6334", key: "k"},
		{addr: "127.0.0.1:6334", key: "k"},
		{addr: "[::1]:6334", key: "k"},
	} {
		cfg := &config.Config{QdrantAddr: test.addr, QdrantAPIKey: test.key, QdrantTLS: test.tls}
		assert.Equal(t, test.want, qdrantKeyInPlaintext(cfg), "%+v", test)
	}
}

// TestPreparePromptsAppliesTheDatabasesActiveReleaseBeforeReturning is the
// prompt-startup-window regression test.
//
// Trigger: restarting a replica of an already-configured install -- the
// database's active release is the organization's own, not the compiled-in
// example in prompts/. That is the ordinary case on every restart, not just
// first boot.
//
// Contract (internal/promptcatalog/catalog.go's package doc, matching
// AGENTS.md's "Prompts" section): "The database holds the active release;
// the example catalog in prompts/ seeds a fresh install." Nothing reachable
// while the server is serving should read the compiled example in its
// place once an organization's own release is active.
//
// Before the fix, preparePrompts read the database's active release
// through EnsureSeed and then discarded the result, leaving
// promptcatalog.Active() on the compiled example until an unsynchronized
// background watcher goroutine happened to run its own first refresh --
// racing, with no ordering guarantee, against everything the caller does
// next (mounting routes, starting listeners, starting the background
// workers that call Active()/ActiveText() directly: session titles,
// document extraction, image captions, scheduled-report templates). This
// test calls preparePrompts exactly as serve does and checks Active() on
// the very next line, with no sleep and no Eventually: the fixed
// preparePrompts applies the already-read release synchronously, so there
// is nothing left to wait for.
func TestPreparePromptsAppliesTheDatabasesActiveReleaseBeforeReturning(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_prompts_startup"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// The organization already has its own active release from before this
	// restart. EnsureSeed on an empty database stores and activates
	// whatever bundle it is given, which is a convenient way to make a
	// distinct bundle the database's one active release; nothing about it
	// being "seeded" matters to the bug.
	org, err := promptcatalog.MustEmbedded().WithBody(promptcatalog.AskBase,
		"This organization's own instructions, already active in Postgres before this restart.")
	require.NoError(t, err)
	seeded, err := promptcatalog.NewStore(pool).EnsureSeed(ctx, org, "test:org-already-active")
	require.NoError(t, err)
	orgSHA := seeded.Bundle.Render(promptcatalog.Values()).SHA256
	exampleSHA := promptcatalog.MustEmbedded().Render(promptcatalog.Values()).SHA256
	require.NotEqual(t, exampleSHA, orgSHA, "test setup: need a release that differs from the compiled example")

	// Mirror catalog.go's init(): a freshly started process has only the
	// compiled example loaded, before preparePrompts or any watcher has
	// run.
	require.NoError(t, promptcatalog.SetActive(promptcatalog.MustEmbedded()))
	t.Cleanup(func() { require.NoError(t, promptcatalog.SetActive(promptcatalog.MustEmbedded())) })

	watchCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	_, err = preparePrompts(watchCtx, pool, settings.NewStore(pool, discardLogger()), source.Registry{},
		func() string { return "" }, discardLogger())
	require.NoError(t, err)

	// This is the bug: immediately after preparePrompts returns, Active()
	// must already be the organization's own release, not the compiled
	// example. preparePrompts already read it (through EnsureSeed) in this
	// same call, and must apply it before returning -- not leave that to
	// the background watcher's own unsynchronized first read.
	require.Equal(t, orgSHA, promptcatalog.Active().SHA256,
		"BUG: Active() is still the compiled example right after preparePrompts returns, even though "+
			"the organization's own release was already read from Postgres in the same call")
}
