package authorization

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelRestrictionsCapRatherThanGrant(t *testing.T) {
	policy, err := Parse([]byte(testPolicy + `  channel_restrictions:
    C0EXAMPLE02:
      allowed_scopes: [corpus:zendesk]
`))
	require.NoError(t, err)
	baseline := []string{"public", "internal", "slack-channel:C0EXAMPLE02"}
	for _, test := range []struct {
		name    string
		channel string
		scopes  []string
		want    ChannelAccess
	}{
		{
			name: "ceiling is not a grant", channel: "C0EXAMPLE02", scopes: baseline,
			want: ChannelAccess{AllowedScopes: []string{"public"}, CorpusOnly: true},
		},
		{
			name: "retain public but remove internal and other grants", channel: "C0EXAMPLE02",
			scopes: append(slices.Clone(baseline), "corpus:zendesk", "corpus:other"),
			want:   ChannelAccess{AllowedScopes: []string{"public", "corpus:zendesk"}, CorpusOnly: true},
		},
		{
			name: "public alone remains accessible", channel: "C0EXAMPLE02", scopes: []string{"public"},
			want: ChannelAccess{AllowedScopes: []string{"public"}, CorpusOnly: true},
		},
		{
			name: "workspace public Slack still needs ceiling entry", channel: "C0EXAMPLE02",
			scopes: []string{"public", "slack-channel:CPUBLIC", "owner:alice@example.com", "unstamped"},
			want:   ChannelAccess{AllowedScopes: []string{"public"}, CorpusOnly: true},
		},
		{
			name: "missing audience remains denied", channel: "C0EXAMPLE02",
			want: ChannelAccess{CorpusOnly: true},
		},
		{
			name: "ordinary channel unchanged", channel: "COTHER", scopes: baseline,
			want: ChannelAccess{AllowedScopes: baseline},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := slices.Clone(test.scopes)
			require.Equal(t, test.want, policy.RestrictChannelScopes(test.channel, test.scopes))
			require.Equal(t, original, test.scopes)
		})
	}
	decision, err := policy.Resolve([]Principal{GroupPrincipal("support-team")})
	require.NoError(t, err)
	require.True(t, decision.Allows(Retrieve, ZendeskCorpus))
}

func TestChannelRestrictionConfigurationIsImmutable(t *testing.T) {
	config := Config{ChannelRestrictions: map[string]ChannelRestriction{
		"CSUPPORT": {AllowedScopes: []string{"corpus:zendesk"}},
	}}
	policy, err := New(config)
	require.NoError(t, err)
	config.ChannelRestrictions["CSUPPORT"].AllowedScopes[0] = "corpus:other"
	delete(config.ChannelRestrictions, "CSUPPORT")
	scopes := []string{"corpus:zendesk", "corpus:other"}
	access := policy.RestrictChannelScopes("CSUPPORT", scopes)
	require.Equal(t, []string{"corpus:zendesk"}, access.AllowedScopes)
	access.AllowedScopes[0] = "public"
	require.Equal(t, []string{"corpus:zendesk"}, policy.RestrictChannelScopes("CSUPPORT", scopes).AllowedScopes)
}

func TestInvalidChannelRestrictionsFailStartup(t *testing.T) {
	for _, restriction := range []string{
		"{support: {allowed_scopes: [corpus:zendesk]}}",
		"{DSUPPORT: {allowed_scopes: [corpus:zendesk]}}",
		"{CSUPPORT: {}}",
		"{CSUPPORT: {allowed_scopes: []}}",
		"{CSUPPORT: {allowed_scopes: [public]}}",
		"{CSUPPORT: {allowed_scopes: [internal]}}",
		"{CSUPPORT: {allowed_scopes: [channel:CSUPPORT]}}",
		"{CSUPPORT: {allowed_scopes: [slack-channel:DSUPPORT]}}",
		"{CSUPPORT: {allowed_scopes: ['slack-channel:*']}}",
		"{CSUPPORT: {allowed_scopes: ['corpus:*']}}",
		"{CSUPPORT: {allowed_scope: [corpus:zendesk]}}",
	} {
		t.Run(restriction, func(t *testing.T) {
			_, err := Parse([]byte("authorization:\n  channel_restrictions: " + restriction))
			require.Error(t, err)
		})
	}
}

func TestMissingChannelRestrictionPolicyDenies(t *testing.T) {
	var policy *Authorizer
	require.Empty(t, policy.RestrictChannelScopes("CSUPPORT", []string{"public", "internal"}).AllowedScopes)
}
