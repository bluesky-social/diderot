package slack

import (
	"context"
	"testing"

	"github.com/bluesky-social/diderot/internal/authorization"
	slackapi "github.com/slack-go/slack"
)

func TestProtectedChannelAuthorization(t *testing.T) {
	pool := getChannelConfigPool(t)
	ctx := context.Background()
	policy, err := authorization.New(authorization.Config{Grants: []authorization.Grant{{
		Principals: []authorization.Principal{authorization.ChannelPrincipal("C0EXAMPLE02"), authorization.GroupPrincipal("support-team")},
		Permissions: []authorization.Permission{
			{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	directory := newDirectoryWithAPI(pool, &fakeAPI{})
	resolver := &ChannelAuthorizer{Policy: policy, Directory: directory}
	for _, test := range []struct {
		name                                                   string
		private, external, pending, member, unknown, unchecked bool
		allowed                                                bool
	}{
		{name: "private internal", private: true, allowed: true},
		{name: "public"},
		{name: "Slack Connect", private: true, external: true},
		{name: "pending Connect", private: true, pending: true},
		{name: "guest members", private: true, member: true, allowed: true},
		{name: "unchecked membership", private: true, unchecked: true, allowed: true},
		{name: "public with guests", member: true},
		{name: "Slack Connect with guests", private: true, external: true, member: true},
		{name: "unknown sharing state", private: true, unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var external, pending any = test.external, test.pending
			if test.unknown {
				external, pending = nil, nil
			}
			if _, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_ext_shared,is_pending_ext_shared,fetched_at)
			VALUES ('C0EXAMPLE02','support',$1,$2,$3,now()) ON CONFLICT (channel_id) DO UPDATE SET name=EXCLUDED.name,is_private=$1,is_ext_shared=$2,is_pending_ext_shared=$3,fetched_at=now()`, test.private, external, pending); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM slack_channel_config WHERE channel_id = 'C0EXAMPLE02'`); err != nil {
				t.Fatal(err)
			}
			if !test.unchecked {
				if _, err := pool.Exec(ctx, `INSERT INTO slack_channel_config (channel_id,has_external_members,external_members_checked_at)
				VALUES ('C0EXAMPLE02',$1,now())`, test.member); err != nil {
					t.Fatal(err)
				}
			}
			decision := resolver.AuthorizeChannel(ctx, "C0EXAMPLE02")
			if decision.Allows(authorization.Retrieve, authorization.ZendeskCorpus) != test.allowed {
				t.Fatalf("permission=%v want allowed=%v", decision.Permissions(), test.allowed)
			}
		})
	}
	if len(resolver.AuthorizeChannel(ctx, "COTHER").Permissions()) != 0 {
		t.Fatal("personal group grant leaked into an unlisted channel")
	}
}

func TestProtectedChannelGuestAsk(t *testing.T) {
	pool := getChannelConfigPool(t)
	ctx := context.Background()
	channel := chanWith("C0EXAMPLE02", "support-leads", true, false)
	consumer := newTestConsumer(t, pool)
	consumer.dir = newDirectoryWithAPI(pool, &fakeAPI{
		channels: map[string]*slackapi.Channel{channel.ID: &channel},
		users: map[string]*slackapi.User{
			"U_GUEST":        {ID: "U_GUEST", Name: "guest", IsRestricted: true},
			"U_SINGLE_GUEST": {ID: "U_SINGLE_GUEST", Name: "single-guest", IsRestricted: true, IsUltraRestricted: true},
		},
	})
	consumer.channelConfig = NewChannelConfigStore(pool, consumer.dir, &fakeMembersAPI{
		members: map[string][]string{channel.ID: {"U_GUEST", "U_SINGLE_GUEST"}},
	}, nil)
	if err := consumer.channelConfig.SetAskEnabled(ctx, channel.ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := consumer.channelConfig.SetAllowExternalAsk(ctx, channel.ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	policy, err := authorization.New(authorization.Config{Grants: []authorization.Grant{{
		Principals:  []authorization.Principal{authorization.ChannelPrincipal(channel.ID)},
		Permissions: []authorization.Permission{{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	consumer.channelAuthorizer = &ChannelAuthorizer{Policy: policy, Directory: consumer.dir}
	for _, userID := range []string{"U_GUEST", "U_SINGLE_GUEST"} {
		t.Run(userID, func(t *testing.T) {
			outcome, _, external := consumer.evaluateAskGates(ctx, channel.ID, userID)
			if outcome != gateAllowed || !external {
				t.Fatalf("outcome=%s external=%v; want allowed guest", outcome, external)
			}
			if !consumer.channelAuthorizer.AuthorizeChannel(ctx, channel.ID).Allows(authorization.Retrieve, authorization.ZendeskCorpus) {
				t.Fatal("approved channel's guest asker did not receive Zendesk access")
			}
		})
	}
}

func TestChannelSharingFactsPersistThroughExistingRefresh(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	channel := chanWith("CSHARED", "shared", true, false)
	channel.IsExtShared, channel.IsPendingExtShared = true, true
	directory := newDirectoryWithAPI(pool, &fakeAPI{channels: map[string]*slackapi.Channel{channel.ID: &channel}})
	if _, err := directory.LookupChannel(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := directory.eligibleProtectedChannel(ctx, channel.ID, false); err != nil || allowed {
		t.Fatalf("info refresh: allowed=%v err=%v", allowed, err)
	}
	channel.IsExtShared, channel.IsPendingExtShared = false, false
	if _, err := WarmChannels(ctx, &warmFakeAPI{channelsP1: []slackapi.Channel{channel}}, nil, pool); err != nil {
		t.Fatal(err)
	}
	if allowed, err := directory.eligibleProtectedChannel(ctx, channel.ID, false); err != nil || !allowed {
		t.Fatalf("warm refresh: allowed=%v err=%v", allowed, err)
	}
}
