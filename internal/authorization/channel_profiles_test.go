package authorization

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

const supportProfilePolicy = `authorization:
  channel_profiles:
    support:
      channels: [CSUPPORT, CQA]
      allow_public: true
      include_own_channel: true
      allowed_scopes: [corpus:zendesk, slack-channel:CPUBLIC]
      permissions:
        - action: retrieve
          resource: corpus:zendesk
  grants:
    - principals: [group:support-team]
      permissions:
        - action: retrieve
          resource: corpus:zendesk
`

// sharedChannelSupportPolicy is a support profile spanning several channels:
// every channel reads Zendesk and the four shared channels' history, and only
// CTRIAGE, which is not one of the shared channels, adds its own history
// through include_own_channel.
const sharedChannelSupportPolicy = `authorization:
  channel_profiles:
    support:
      channels:
        - CTRIAGE
        - CSHAREDA
        - CSHAREDB
        - CSHAREDC
        - CSHAREDD
      allow_public: true
      include_own_channel: true
      allowed_scopes:
        - corpus:zendesk
        - slack-channel:CSHAREDA
        - slack-channel:CSHAREDB
        - slack-channel:CSHAREDC
        - slack-channel:CSHAREDD
      permissions:
        - action: retrieve
          resource: corpus:zendesk
  grants:
    - principals:
        - group:support-team
      permissions:
        - action: retrieve
          resource: corpus:zendesk
`

func TestSharedChannelSupportProfile(t *testing.T) {
	policy, err := Parse([]byte(sharedChannelSupportPolicy))
	require.NoError(t, err)
	sharedScopes := []string{"corpus:zendesk", "slack-channel:CSHAREDA", "slack-channel:CSHAREDB", "slack-channel:CSHAREDC", "slack-channel:CSHAREDD"}
	for _, channel := range []string{"CTRIAGE", "CSHAREDA", "CSHAREDB", "CSHAREDC", "CSHAREDD"} {
		decision, err := policy.Resolve([]Principal{ChannelPrincipal(channel)})
		require.NoError(t, err)
		require.Equal(t, []string{"corpus:zendesk"}, decision.CorpusScopes())
		require.True(t, policy.AllowsPublicChannel(channel))
		scopes := append(slices.Clone(sharedScopes), "public", "internal", "slack-channel:CTRIAGE", "slack-channel:COTHER")
		want := append(slices.Clone(sharedScopes), "public")
		if channel == "CTRIAGE" {
			want = append(want, "slack-channel:CTRIAGE")
		}
		require.ElementsMatch(t, want, policy.RestrictChannelScopes(channel, scopes).AllowedScopes)
	}
}

func TestChannelProfilesExpandGrantsAndCeilings(t *testing.T) {
	policy, err := Parse([]byte(supportProfilePolicy))
	require.NoError(t, err)
	baseline := []string{"public", "internal", "corpus:other", "slack-channel:CPUBLIC", "slack-channel:CSUPPORT", "slack-channel:CQA"}
	for _, channel := range []string{"CSUPPORT", "CQA"} {
		decision, err := policy.Resolve([]Principal{ChannelPrincipal(channel)})
		require.NoError(t, err)
		require.Equal(t, []Permission{{Action: Retrieve, Resource: ZendeskCorpus}}, decision.Permissions())
		require.True(t, policy.AllowsPublicChannel(channel))
		access := policy.RestrictChannelScopes(channel, append(slices.Clone(baseline), decision.CorpusScopes()...))
		require.True(t, access.CorpusOnly)
		require.ElementsMatch(t, []string{"public", "corpus:zendesk", "slack-channel:CPUBLIC", "slack-channel:" + channel}, access.AllowedScopes)
		require.NotContains(t, policy.RestrictChannelScopes(channel, baseline).AllowedScopes, "corpus:zendesk")
	}
	group, err := policy.Resolve([]Principal{GroupPrincipal("support-team")})
	require.NoError(t, err)
	require.True(t, group.Allows(Retrieve, ZendeskCorpus))
	for _, channel := range []string{"CPUBLIC", "COTHER"} {
		decision, err := policy.Resolve([]Principal{ChannelPrincipal(channel)})
		require.NoError(t, err)
		require.Empty(t, decision.Permissions())
		require.False(t, policy.AllowsPublicChannel(channel))
		require.Equal(t, ChannelAccess{AllowedScopes: baseline}, policy.RestrictChannelScopes(channel, baseline))
	}
}

func TestChannelProfilesDefaultsAndIsolation(t *testing.T) {
	config := Config{ChannelProfiles: map[string]ChannelProfile{
		"support": {
			Channels: []string{"CSUPPORT", "CQA"}, AllowedScopes: []string{"corpus:zendesk"},
			Permissions: []Permission{{Action: Retrieve, Resource: ZendeskCorpus}},
		},
	}}
	policy, err := New(config)
	require.NoError(t, err)
	profile := config.ChannelProfiles["support"]
	profile.Channels[0] = "COTHER"
	profile.AllowedScopes[0] = "corpus:other"
	profile.Permissions[0].Resource = "corpus:other"
	delete(config.ChannelProfiles, "support")
	require.Nil(t, config.ChannelRestrictions)
	require.Empty(t, config.Grants)
	for _, channel := range []string{"CSUPPORT", "CQA"} {
		require.False(t, policy.AllowsPublicChannel(channel))
		decision, err := policy.Resolve([]Principal{ChannelPrincipal(channel)})
		require.NoError(t, err)
		require.True(t, decision.Allows(Retrieve, ZendeskCorpus))
		access := policy.RestrictChannelScopes(channel, []string{"public", "corpus:zendesk", "corpus:other", "slack-channel:" + channel})
		require.Equal(t, []string{"public", "corpus:zendesk"}, access.AllowedScopes)
		access.AllowedScopes[1] = "corpus:other"
	}
	var missing *Authorizer
	require.False(t, missing.AllowsPublicChannel("CSUPPORT"))
}

func TestChannelProfileOwnHistoryDoesNotGrantForeignHistory(t *testing.T) {
	policy, err := New(Config{ChannelProfiles: map[string]ChannelProfile{
		"own-history": {Channels: []string{"CSUPPORT", "CQA"}, IncludeOwnChannel: true},
	}})
	require.NoError(t, err)
	for _, channel := range []string{"CSUPPORT", "CQA"} {
		decision, err := policy.Resolve([]Principal{ChannelPrincipal(channel)})
		require.NoError(t, err)
		require.Empty(t, decision.Permissions())
		access := policy.RestrictChannelScopes(channel, []string{"public", "slack-channel:CSUPPORT", "slack-channel:CQA"})
		require.Equal(t, []string{"public", "slack-channel:" + channel}, access.AllowedScopes)
	}
}

func TestInvalidChannelProfilesFailStartup(t *testing.T) {
	for _, profile := range []string{
		"{'bad name': {channels: [CSUPPORT], include_own_channel: true}}",
		"{support: {include_own_channel: true}}",
		"{support: {channels: [], include_own_channel: true}}",
		"{support: {channels: [CSUPPORT, CSUPPORT], include_own_channel: true}}",
		"{support: {channels: [CSUPPORT]}, other: {channels: [CSUPPORT]}}",
		"{support: {channels: [DSUPPORT], include_own_channel: true}}",
		"{support: {channels: ['*'], include_own_channel: true}}",
		"{support: {channels: [CSUPPORT]}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [internal]}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [public]}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: ['corpus:*']}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [corpus:zendesk], permissions: [{action: retrieve, resource: corpus:other}]}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [corpus:zendesk], permissions: [{action: write, resource: corpus:zendesk}]}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [corpus:zendesk], allow_public: unknown}}",
		"{support: {channels: [CSUPPORT], allowed_scopes: [corpus:zendesk], allow_pubic: true}}",
	} {
		t.Run(profile, func(t *testing.T) {
			_, err := Parse([]byte("authorization:\n  channel_profiles: " + profile))
			require.Error(t, err)
		})
	}
	_, err := Parse([]byte(supportProfilePolicy + "  channel_restrictions:\n    CSUPPORT:\n      allowed_scopes: [corpus:zendesk]\n"))
	require.ErrorContains(t, err, "multiple restrictions or profile assignments")
}
