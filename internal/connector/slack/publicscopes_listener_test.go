package slack

import (
	"context"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func startPublicScopeListener(t *testing.T, resolver *PublicScopeResolver) {
	t.Helper()
	resolver.Scopes(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		resolver.RunInvalidationListener(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("invalidation listener failed to stop")
		}
	})
	require.Eventually(t, func() bool {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return resolver.expiresAt.IsZero()
	}, 5*time.Second, 5*time.Millisecond)
}

func TestPublicScopeNotificationsInvalidateEveryReplicaAfterCommit(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CTARGET','target',false,false,now())`)
	require.NoError(t, err)
	resolvers := []*PublicScopeResolver{NewPublicScopeResolver(pool, nil), NewPublicScopeResolver(pool, nil)}
	for _, resolver := range resolvers {
		resolver.ttl = 24 * time.Hour
		startPublicScopeListener(t, resolver)
		require.Contains(t, resolver.Scopes(ctx), events.ScopeChannel("CTARGET"))
	}
	transaction, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(ctx) })
	_, err = transaction.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CTARGET'`)
	require.NoError(t, err)
	for _, resolver := range resolvers {
		require.Contains(t, resolver.Scopes(ctx), events.ScopeChannel("CTARGET"))
	}
	require.NoError(t, transaction.Commit(ctx))
	for _, resolver := range resolvers {
		require.Eventually(t, func() bool { return len(resolver.Scopes(ctx)) == 0 }, 5*time.Second, 5*time.Millisecond)
	}
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=false WHERE channel_id='CTARGET'`)
	require.NoError(t, err)
	for _, resolver := range resolvers {
		require.Eventually(t, func() bool { return len(resolver.Scopes(ctx)) == 1 }, 5*time.Second, 5*time.Millisecond)
	}
	_, err = pool.Exec(ctx, `DELETE FROM slack_channel_info WHERE channel_id='CTARGET'`)
	require.NoError(t, err)
	for _, resolver := range resolvers {
		require.Eventually(t, func() bool { return len(resolver.Scopes(ctx)) == 0 }, 5*time.Second, 5*time.Millisecond)
	}
}

func TestPublicScopeListenerClearsMissedChangesOnConnect(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CTARGET','target',false,false,now())`)
	require.NoError(t, err)
	resolver := NewPublicScopeResolver(pool, nil)
	resolver.ttl = 24 * time.Hour
	require.Contains(t, resolver.Scopes(ctx), events.ScopeChannel("CTARGET"))
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CTARGET'`)
	require.NoError(t, err)
	require.Contains(t, resolver.Scopes(ctx), events.ScopeChannel("CTARGET"))
	startPublicScopeListener(t, resolver)
	require.Empty(t, resolver.Scopes(ctx))
}

// The listener's own connection must go through the pool's BeforeConnect,
// which is where RDS IAM auth sets the login token (internal/dbconn).
func TestPublicScopeListenerUsesPoolBeforeConnect(t *testing.T) {
	ctx := context.Background()
	config := getWarmPool(t).Config()
	config.BeforeConnect = func(_ context.Context, connConfig *pgx.ConnConfig) error {
		connConfig.RuntimeParams["application_name"] = "scopes-before-connect"
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	resolver := NewPublicScopeResolver(pool, nil)
	startPublicScopeListener(t, resolver)
	require.Eventually(t, func() bool {
		var listeners int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = 'scopes-before-connect' AND query LIKE 'LISTEN%'`).Scan(&listeners)
		return err == nil && listeners == 1
	}, 5*time.Second, 20*time.Millisecond)
}
