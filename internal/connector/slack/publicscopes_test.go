package slack

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicScopesCacheRefreshAndIsolation(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at) VALUES
		('CPUBLIC','public',false,false,now()), ('CPRIVATE','private',true,false,now()),
		('CUNKNOWN','',false,false,now()), ('DOWNER','dm',false,false,now()),
		('CARCHIVED','archived',false,true,now()), ('C_INVALID','invalid',false,false,now())`)
	require.NoError(t, err)
	resolver := NewPublicScopeResolver(pool, nil)
	clock := time.Now()
	resolver.now = func() time.Time { return clock }
	want := []string{events.ScopeChannel("CARCHIVED"), events.ScopeChannel("CPUBLIC")}
	before := pool.Stat().AcquireCount()
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { assert.Equal(t, want, resolver.Scopes(ctx)) })
	}
	workers.Wait()
	require.EqualValues(t, 1, pool.Stat().AcquireCount()-before)
	returned := resolver.Scopes(ctx)
	returned[0] = events.ScopeChannel("CPRIVATE")
	require.Equal(t, want, resolver.Scopes(ctx))
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CPUBLIC';
		INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CNEW','new',false,false,now())`)
	require.NoError(t, err)
	require.Equal(t, want, resolver.Scopes(ctx))
	clock = clock.Add(resolver.ttl)
	require.Equal(t, []string{events.ScopeChannel("CARCHIVED"), events.ScopeChannel("CNEW")}, resolver.Scopes(ctx))
	pool.Close()
	clock = clock.Add(resolver.ttl)
	require.Empty(t, resolver.Scopes(ctx))
	require.Empty(t, resolver.Scopes(ctx))
}

func TestUserScopesRefreshPublicSetIndependentlyOfMembershipCache(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	seedSlackUser(t, pool, "UALICE", "alice@example.com", false)
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CPUBLIC','public',false,false,now())`)
	require.NoError(t, err)
	public := NewPublicScopeResolver(pool, nil)
	clock := time.Now()
	public.now = func() time.Time { return clock }
	api := &fakeUserConvAPI{pages: map[string]fakeConvPage{"": {channels: []slackapi.Channel{chanWith("CPRIVATE", "private", true, false)}}}}
	resolver := newTestResolver(t, pool, api).WithPublicScopes(public)
	require.ElementsMatch(t, []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPUBLIC"), events.ScopeChannel("CPRIVATE")}, resolver.ScopesForEmail(ctx, "alice@example.com"))
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CPUBLIC'`)
	require.NoError(t, err)
	clock = clock.Add(public.ttl)
	require.ElementsMatch(t, []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPRIVATE")}, resolver.ScopesForEmail(ctx, "alice@example.com"))
	require.Equal(t, 1, api.callCount())
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=false WHERE channel_id='CPUBLIC'`)
	require.NoError(t, err)
	clock = clock.Add(public.ttl)
	withoutBot := NewUserScopeResolver(nil, nil, pool, nil).WithPublicScopes(public)
	require.ElementsMatch(t, []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPUBLIC")}, withoutBot.ScopesForEmail(ctx, ""))
}

func TestPublicChannelExpansionRespectsCeilingsAndExplicitPrivateGrants(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at) VALUES
		('CPUBLIC','public',false,false,now()), ('CPRIVATE','private',true,false,now()),
		('COTHER','other',true,false,now()), ('CSUPPORT','support',true,false,now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_ext_shared=false,is_pending_ext_shared=false`)
	require.NoError(t, err)
	policy, err := authorization.New(authorization.Config{
		Grants: []authorization.Grant{{Principals: []authorization.Principal{authorization.ChannelPrincipal("CSUPPORT")}, Permissions: []authorization.Permission{
			{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus},
			{Action: authorization.Retrieve, Resource: authorization.Resource(events.ScopeChannel("CPRIVATE"))},
		}}},
		ChannelRestrictions: map[string]authorization.ChannelRestriction{"CSUPPORT": {AllowedScopes: []string{
			events.ScopeZendesk, events.ScopeChannel("CSUPPORT"), events.ScopeChannel("CPUBLIC"), events.ScopeChannel("CPRIVATE"), events.ScopeChannel("COTHER"),
		}}},
	})
	require.NoError(t, err)
	resolver := &ChannelAuthorizer{Policy: policy, Directory: newDirectoryWithAPI(pool, &fakeAPI{}), PublicScopes: NewPublicScopeResolver(pool, nil)}
	access := resolver.ResolveChannel(ctx, "CSUPPORT")
	require.True(t, access.CorpusOnly)
	require.ElementsMatch(t, []string{events.ScopePublic, events.ScopeZendesk, events.ScopeChannel("CSUPPORT"), events.ScopeChannel("CPUBLIC"), events.ScopeChannel("CPRIVATE")}, access.AllowedScopes)
	ordinary := resolver.ResolveChannel(ctx, "COTHER")
	require.False(t, ordinary.CorpusOnly)
	require.ElementsMatch(t, []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("COTHER"), events.ScopeChannel("CPUBLIC")}, ordinary.AllowedScopes)
	for _, change := range []string{"is_ext_shared=true", "is_ext_shared=false,is_pending_ext_shared=true", "is_pending_ext_shared=false,is_private=false", "is_private=true,name=''"} {
		_, err = pool.Exec(ctx, "UPDATE slack_channel_info SET "+change+" WHERE channel_id='CSUPPORT'")
		require.NoError(t, err)
		require.Empty(t, resolver.ResolveChannel(ctx, "CSUPPORT").AllowedScopes, change)
	}
}
