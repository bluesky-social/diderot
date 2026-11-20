package slack

import (
	"context"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func restrictedChannelPolicy(t *testing.T, grant bool) *authorization.Authorizer {
	t.Helper()
	config := authorization.Config{ChannelRestrictions: map[string]authorization.ChannelRestriction{
		"CSUPPORT": {AllowedScopes: []string{"corpus:zendesk"}},
	}}
	if grant {
		config.Grants = []authorization.Grant{{
			Principals: []authorization.Principal{authorization.ChannelPrincipal("CSUPPORT")},
			Permissions: []authorization.Permission{
				{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus},
				{Action: authorization.Retrieve, Resource: "corpus:other"},
			},
		}}
	}
	policy, err := authorization.New(config)
	require.NoError(t, err)
	return policy
}

func TestRestrictedChannelResolution(t *testing.T) {
	pool := getChannelConfigPool(t)
	ctx := context.Background()
	directory := newDirectoryWithAPI(pool, &fakeAPI{})
	for _, test := range []struct {
		name              string
		private, grant    bool
		external, pending any
		missingDirectory  bool
		want              []string
	}{
		{name: "private approved", private: true, grant: true, external: false, pending: false, want: []string{"public", "corpus:zendesk"}},
		{name: "grant removed retains public", private: true, external: false, pending: false, want: []string{"public"}},
		{name: "public", grant: true, external: false, pending: false},
		{name: "Slack Connect", private: true, grant: true, external: true, pending: false},
		{name: "pending Connect", private: true, grant: true, external: false, pending: true},
		{name: "unknown sharing", private: true, grant: true},
		{name: "missing directory", private: true, grant: true, missingDirectory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_ext_shared,is_pending_ext_shared,fetched_at)
			VALUES ('CSUPPORT','support',$1,$2,$3,now()) ON CONFLICT (channel_id) DO UPDATE SET name=EXCLUDED.name,is_private=$1,is_ext_shared=$2,is_pending_ext_shared=$3,fetched_at=now()`, test.private, test.external, test.pending)
			require.NoError(t, err)
			resolver := &ChannelAuthorizer{Policy: restrictedChannelPolicy(t, test.grant), Directory: directory}
			if test.missingDirectory {
				resolver.Directory = nil
			}
			access := resolver.ResolveChannel(ctx, "CSUPPORT")
			require.True(t, access.CorpusOnly)
			require.Equal(t, test.want, access.AllowedScopes)
			ordinary := resolver.ResolveChannel(ctx, "COTHER")
			require.False(t, ordinary.CorpusOnly)
			require.Equal(t, scopesForChannel("COTHER"), ordinary.AllowedScopes)
		})
	}
}

func TestRestrictedChannelRoutingOmitsSlackContext(t *testing.T) {
	pool := getConsumerPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_ext_shared,is_pending_ext_shared,fetched_at)
	VALUES ('CSUPPORT','support',true,false,false,now()) ON CONFLICT (channel_id) DO UPDATE SET name='support',is_private=true,is_ext_shared=false,is_pending_ext_shared=false,fetched_at=now()`)
	require.NoError(t, err)
	consumer := newTestConsumer(t, pool)
	consumer.selfUserID.Store("U_DIDEROT")
	consumer.channelAuthorizer = &ChannelAuthorizer{Policy: restrictedChannelPolicy(t, true), Directory: newDirectoryWithAPI(pool, &fakeAPI{})}
	consumer.threadContextFetcher = func(context.Context, *pgxpool.Pool, string, string, time.Time, time.Time) (string, error) {
		t.Error("restricted turn attempted to load Slack history")
		return "private Slack history", nil
	}
	router := &fakeAskRouter{known: map[string]bool{}, succeedCreate: true}
	consumer.ask = router
	consumer.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type":"message", "channel":"CSUPPORT", "user":"U_SUPPORT",
		"text":"<@U_DIDEROT> find the ticket", "thread_ts":"1700019200.000100", "ts":"1700019201.000100"
	}`))
	waitForLen(t, func() int {
		router.mu.Lock()
		defer router.mu.Unlock()
		return len(router.requests)
	}, 1, "restricted turn should reach Ask")
	router.mu.Lock()
	defer router.mu.Unlock()
	require.Equal(t, []string{"public", "corpus:zendesk"}, router.requests[0].AllowedScopes)
	require.True(t, router.requests[0].CorpusOnly)
	require.Empty(t, router.requests[0].ThreadContext)
	require.Equal(t, "<@U_DIDEROT> find the ticket", router.requests[0].UserInput)
}

func TestMissingChannelAuthorizerDenies(t *testing.T) {
	for _, resolver := range []*ChannelAuthorizer{nil, {}} {
		require.Empty(t, resolver.ResolveChannel(context.Background(), "CSUPPORT").AllowedScopes)
	}
	pool := getConsumerPool(t)
	consumer := newTestConsumer(t, pool)
	consumer.selfUserID.Store("U_DIDEROT")
	consumer.channelAuthorizer = nil
	router := &fakeAskRouter{known: map[string]bool{}, succeedCreate: true}
	consumer.ask = router
	consumer.handleEventsAPI(context.Background(), wrapMessageEvent(t, `{
		"type":"message", "channel":"CSUPPORT", "user":"U_SUPPORT",
		"text":"<@U_DIDEROT> find the ticket", "ts":"1700019211.000100"
	}`))
	router.mu.Lock()
	defer router.mu.Unlock()
	require.Empty(t, router.knownCalls)
	require.Empty(t, router.createCalls)
	require.Empty(t, router.requests)
}
