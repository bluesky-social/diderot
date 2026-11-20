package slack

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/jackc/pgx/v5/pgxpool"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

func supportProfile(t *testing.T, allowPublic bool) *authorization.Authorizer {
	t.Helper()
	policy, err := authorization.New(authorization.Config{ChannelProfiles: map[string]authorization.ChannelProfile{
		"support": {
			Channels: []string{"CSUPPORT", "CQA"}, AllowPublic: allowPublic, IncludeOwnChannel: true,
			AllowedScopes: []string{"corpus:zendesk", "slack-channel:CQA", "slack-channel:CPUBLIC"},
			Permissions:   []authorization.Permission{{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus}},
		},
	}})
	require.NoError(t, err)
	return policy
}

func TestChannelProfilesResolveSharedSourcesAndLocalHistory(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_ext_shared,is_pending_ext_shared,fetched_at) VALUES
		('CSUPPORT','support-leads',true,false,false,now()), ('CQA','support-triage',false,false,false,now()),
		('CPUBLIC','support-public',false,false,false,now()), ('COTHER','other-public',false,false,false,now())`)
	require.NoError(t, err)
	resolver := &ChannelAuthorizer{Policy: supportProfile(t, true), Directory: newDirectoryWithAPI(pool, &fakeAPI{}), PublicScopes: NewPublicScopeResolver(pool, nil)}
	for _, channel := range []string{"CSUPPORT", "CQA"} {
		access := resolver.ResolveChannel(ctx, channel)
		require.True(t, access.CorpusOnly)
		want := []string{"public", "corpus:zendesk", "slack-channel:CQA", "slack-channel:CPUBLIC"}
		if channel == "CSUPPORT" {
			want = append(want, "slack-channel:CSUPPORT")
		}
		slices.Sort(access.AllowedScopes)
		require.ElementsMatch(t, want, slices.Compact(access.AllowedScopes))
		require.True(t, resolver.AuthorizeChannel(ctx, channel).Allows(authorization.Retrieve, authorization.ZendeskCorpus))
	}
	ordinary := resolver.ResolveChannel(ctx, "COTHER")
	require.False(t, ordinary.CorpusOnly)
	require.Contains(t, ordinary.AllowedScopes, events.ScopeInternal)
	require.NotContains(t, ordinary.AllowedScopes, events.ScopeZendesk)
	require.NotContains(t, ordinary.AllowedScopes, "slack-channel:CSUPPORT")
	_, err = pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true WHERE channel_id='CPUBLIC'`)
	require.NoError(t, err)
	resolver.PublicScopes.Invalidate()
	for _, channel := range []string{"CSUPPORT", "CQA"} {
		require.NotContains(t, resolver.ResolveChannel(ctx, channel).AllowedScopes, "slack-channel:CPUBLIC")
	}
}

func TestPublicChannelProfileEligibility(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	for _, test := range []struct {
		name, channelName string
		allowPublic       bool
		private           bool
		external, pending any
		allowed           bool
	}{
		{name: "public opt in", channelName: "qa", allowPublic: true, external: false, pending: false, allowed: true},
		{name: "public default deny", channelName: "qa", external: false, pending: false},
		{name: "private default allow", channelName: "qa", private: true, external: false, pending: false, allowed: true},
		{name: "public Slack Connect", channelName: "qa", allowPublic: true, external: true, pending: false},
		{name: "public pending Connect", channelName: "qa", allowPublic: true, external: false, pending: true},
		{name: "private Slack Connect", channelName: "qa", private: true, allowPublic: true, external: true, pending: false},
		{name: "unknown sharing", channelName: "qa", allowPublic: true},
		{name: "missing channel metadata", allowPublic: true, external: false, pending: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_ext_shared,is_pending_ext_shared,fetched_at)
			VALUES ('CQA',$1,$2,$3,$4,now()) ON CONFLICT (channel_id) DO UPDATE SET name=$1,is_private=$2,is_ext_shared=$3,is_pending_ext_shared=$4,fetched_at=now()`, test.channelName, test.private, test.external, test.pending)
			require.NoError(t, err)
			resolver := &ChannelAuthorizer{Policy: supportProfile(t, test.allowPublic), Directory: newDirectoryWithAPI(pool, &fakeAPI{})}
			access := resolver.ResolveChannel(ctx, "CQA")
			require.True(t, access.CorpusOnly)
			require.Equal(t, test.allowed, len(access.AllowedScopes) > 0)
			require.Equal(t, test.allowed, resolver.AuthorizeChannel(ctx, "CQA").Allows(authorization.Retrieve, authorization.ZendeskCorpus))
		})
	}
}

func TestPublicChannelProfileGuestGates(t *testing.T) {
	pool := getChannelConfigPool(t)
	ctx := context.Background()
	channel := chanWith("CQA", "support-triage", false, false)
	consumer := newTestConsumer(t, pool)
	consumer.dir = newDirectoryWithAPI(pool, &fakeAPI{
		channels: map[string]*slackapi.Channel{channel.ID: &channel},
		users: map[string]*slackapi.User{
			"U_GUEST": {ID: "U_GUEST", Name: "guest", IsRestricted: true},
		},
	})
	consumer.channelConfig = NewChannelConfigStore(pool, consumer.dir, &fakeMembersAPI{members: map[string][]string{channel.ID: {"U_GUEST"}}}, nil)
	consumer.channelAuthorizer = &ChannelAuthorizer{Policy: supportProfile(t, true), Directory: consumer.dir}
	require.NoError(t, consumer.channelConfig.SetAskEnabled(ctx, channel.ID, true, "test"))
	outcome, _, _ := consumer.evaluateAskGates(ctx, channel.ID, "U_GUEST")
	require.NotEqual(t, gateAllowed, outcome)
	require.NoError(t, consumer.channelConfig.SetAllowExternalAsk(ctx, channel.ID, true, "test"))
	outcome, _, external := consumer.evaluateAskGates(ctx, channel.ID, "U_GUEST")
	require.Equal(t, gateAllowed, outcome)
	require.True(t, external)
	access := consumer.channelAuthorizer.ResolveChannel(ctx, channel.ID)
	require.True(t, access.CorpusOnly)
	require.ElementsMatch(t, []string{"public", "corpus:zendesk", "slack-channel:CQA"}, access.AllowedScopes)
	consumer.selfUserID.Store("U_DIDEROT")
	consumer.threadContextFetcher = func(context.Context, *pgxpool.Pool, string, string, time.Time, time.Time) (string, error) {
		t.Error("restricted public turn attempted to load ambient Slack history")
		return "private Slack history", nil
	}
	router := &fakeAskRouter{known: map[string]bool{}, succeedCreate: true}
	consumer.ask = router
	consumer.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type":"message", "channel":"CQA", "user":"U_GUEST",
		"text":"<@U_DIDEROT> find the ticket", "thread_ts":"1700019300.000100", "ts":"1700019301.000100"
	}`))
	waitForLen(t, func() int {
		router.mu.Lock()
		defer router.mu.Unlock()
		return len(router.requests)
	}, 1, "restricted public guest turn should reach Ask")
	router.mu.Lock()
	request := router.requests[0]
	router.mu.Unlock()
	require.True(t, request.CorpusOnly)
	require.Empty(t, request.ThreadContext)
	require.ElementsMatch(t, access.AllowedScopes, request.AllowedScopes)
	require.NoError(t, consumer.channelConfig.SetAskEnabled(ctx, channel.ID, false, "test"))
	outcome, _, _ = consumer.evaluateAskGates(ctx, channel.ID, "U_GUEST")
	require.NotEqual(t, gateAllowed, outcome)
}
