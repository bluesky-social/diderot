package scopesync

import (
	"context"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/stretchr/testify/require"
)

func TestReconcileUsesStableChannelScopesForAllSlack(t *testing.T) {
	pool := setupDB(t)
	ctx := context.Background()
	publicID, publicChunk := seed(t, pool, "CWORKSPACE", false, events.ScopePublic)
	privateID, _ := seed(t, pool, "CPRIVATE", true, events.ScopeChannel("CPRIVATE"))
	ownerID, _ := seed(t, pool, "COWNER", false, events.ScopeOwner("alice@example.com"))
	dmID, _ := seed(t, pool, "DOWNER", false, events.ScopeOwner("alice@example.com"))
	sink := events.NewDBSink(pool, nil)
	article, err := sink.Upsert(ctx, events.Event{Source: "zendesk", ExternalID: "article:public", Body: "public article", OccurredAt: time.Now(), Scope: events.ScopePublic})
	require.NoError(t, err)
	store := &fakeStore{}
	service := service(pool, store)
	result, err := service.Reconcile(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Events)
	require.EqualValues(t, 1, result.Chunks)
	for eventID, want := range map[int64]string{
		publicID: events.ScopeChannel("CWORKSPACE"), privateID: events.ScopeChannel("CPRIVATE"),
		ownerID: events.ScopeOwner("alice@example.com"), dmID: events.ScopeOwner("alice@example.com"), article.EventID: events.ScopePublic,
	} {
		var scope string
		require.NoError(t, pool.QueryRow(ctx, `SELECT scope FROM events WHERE id=$1`, eventID).Scan(&scope))
		require.Equal(t, want, scope)
	}
	applied, err := service.DrainScope(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, applied)
	require.Equal(t, [][]uint64{{uint64(publicChunk)}}, store.scopeCalls)
	require.Equal(t, []string{events.ScopeChannel("CWORKSPACE")}, store.scopeValues)
	result, err = service.Reconcile(ctx)
	require.NoError(t, err)
	require.Zero(t, result.Events)
	require.Zero(t, result.Chunks)

	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CWORKSPACE'`)
	require.NoError(t, err)
	result, err = service.Reconcile(ctx)
	require.NoError(t, err)
	require.Zero(t, result.Events)
	var scope string
	require.NoError(t, pool.QueryRow(ctx, `SELECT scope FROM events WHERE id=$1`, publicID).Scan(&scope))
	require.Equal(t, events.ScopeChannel("CWORKSPACE"), scope)

	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=false WHERE channel_id='CWORKSPACE'`)
	require.NoError(t, err)
	_, err = service.Reconcile(ctx)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT scope FROM events WHERE id=$1`, publicID).Scan(&scope))
	require.Equal(t, events.ScopeChannel("CWORKSPACE"), scope)
}
