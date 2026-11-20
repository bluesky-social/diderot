package modelregistry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

func TestRefSplitsAtTheFirstSlash(t *testing.T) {
	m := Model{Provider: "gateway", ID: "accounts/fireworks/models/glm-5p3:main"}
	provider, id, ok := ParseRef(m.Ref())
	require.True(t, ok)
	assert.Equal(t, "gateway", provider)
	assert.Equal(t, "accounts/fireworks/models/glm-5p3:main", id)
	_, _, ok = ParseRef("no-slash")
	assert.False(t, ok)
	_, _, ok = ParseRef("/id")
	assert.False(t, ok)
}

func TestServesRole(t *testing.T) {
	chat := Model{ID: "m", Protocols: []Protocol{ProtocolChat}}
	assert.True(t, chat.ServesRole(RoleThreadSummaries))
	assert.False(t, chat.ServesRole(RoleImageCaptions), "captions need a model tested with an image")
	chat.Images = true
	assert.True(t, chat.ServesRole(RoleImageCaptions))
	assert.True(t, chat.ServesRole(RoleDocumentExtraction))
	assert.False(t, chat.ServesRole(RoleAsk))

	messages := Model{ID: "c", Protocols: []Protocol{ProtocolChat, ProtocolMessages}}
	protocol, ok := messages.AgenticProtocol()
	require.True(t, ok)
	assert.Equal(t, ProtocolMessages, protocol)
	assert.True(t, messages.ServesRole(RoleSessionSummaryMap))
	assert.False(t, messages.ServesRole(RoleTitles))
	assert.False(t, messages.ServesRole(Role("embeddings")))
}

// Session summaries call a model on the first of SummaryProtocols it lists,
// whatever order the entry lists them in: a model that lists Messages is
// always called on Messages.
func TestSessionSummariesUseTheFirstSummaryProtocolListed(t *testing.T) {
	assert.Equal(t, []Protocol{ProtocolMessages, ProtocolResponses, ProtocolChat}, SummaryProtocols)
	for _, tc := range []struct {
		listed []Protocol
		want   Protocol
	}{
		{[]Protocol{ProtocolMessages}, ProtocolMessages},
		{[]Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages}, ProtocolMessages},
		{[]Protocol{ProtocolResponses, ProtocolMessages}, ProtocolMessages},
		{[]Protocol{ProtocolChat, ProtocolResponses}, ProtocolResponses},
		{[]Protocol{ProtocolResponses}, ProtocolResponses},
		{[]Protocol{ProtocolChat}, ProtocolChat},
	} {
		m := Model{ID: "m", Protocols: tc.listed}
		for _, role := range []Role{RoleSessionSummaryMap, RoleSessionSummaryReduce} {
			got, ok := m.RoleProtocol(role)
			assert.True(t, ok, "%v", tc.listed)
			assert.Equal(t, tc.want, got, "%v", tc.listed)
			assert.True(t, m.ServesRole(role), "%v", tc.listed)
		}
	}
	none := Model{ID: "m"}
	_, ok := none.RoleProtocol(RoleSessionSummaryMap)
	assert.False(t, ok)
	assert.False(t, none.ServesRole(RoleSessionSummaryMap))
	assert.False(t, none.ServesRole(RoleSessionSummaryReduce))
}

// Every role is served exactly when the entry lists a protocol the role
// speaks, and RoleProtocol names the one its requests use.
func TestRoleProtocolOfTheOtherRoles(t *testing.T) {
	m := Model{ID: "m", Protocols: []Protocol{ProtocolMessages, ProtocolResponses, ProtocolChat}, Images: true}
	for role, want := range map[Role]Protocol{
		RoleAsk: ProtocolMessages, RoleTitles: ProtocolResponses, RoleImageCaptions: ProtocolChat,
		RoleDocumentExtraction: ProtocolChat, RoleThreadSummaries: ProtocolChat,
	} {
		got, ok := m.RoleProtocol(role)
		assert.True(t, ok, role)
		assert.Equal(t, want, got, role)
	}
	chat := Model{ID: "m", Protocols: []Protocol{ProtocolChat}}
	for _, role := range []Role{RoleAsk, RoleTitles} {
		_, ok := chat.RoleProtocol(role)
		assert.False(t, ok, role)
	}
}

func TestValidate(t *testing.T) {
	ok := Model{ID: "gpt", Protocols: []Protocol{ProtocolResponses, ProtocolChat}, ReasoningEfforts: []string{"low"},
		ServiceTier: true, PromptCacheRetention: "24h", ChatTokenParam: llmclient.ChatMaxCompletionTokens,
		ChatExtraBody: map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}}}
	require.NoError(t, ok.Validate(KindOpenAI))
	for name, tc := range map[string]struct {
		model Model
		kind  Kind
		want  string
	}{
		"blank id":            {Model{ID: " x"}, KindOpenAI, "enter the model's ID"},
		"anthropic responses": {Model{ID: "c", Protocols: []Protocol{ProtocolResponses}}, KindAnthropic, "does not serve the responses protocol"},
		"embeddings":          {Model{ID: "e", Protocols: []Protocol{ProtocolEmbeddings}}, KindOpenAI, "does not serve the embeddings protocol"},
		"repeated protocol":   {Model{ID: "c", Protocols: []Protocol{ProtocolChat, ProtocolChat}}, KindOpenAI, "listed twice"},
		"unknown effort":      {Model{ID: "c", ReasoningEfforts: []string{"turbo"}}, KindOpenAI, "not a reasoning effort"},
		"tier without resp":   {Model{ID: "c", Protocols: []Protocol{ProtocolChat}, ServiceTier: true}, KindOpenAI, "service tier"},
		"bad retention":       {Model{ID: "c", Protocols: []Protocol{ProtocolResponses}, PromptCacheRetention: "1y"}, KindOpenAI, "prompt cache retention"},
		"reserved extra body": {Model{ID: "c", ChatExtraBody: map[string]any{"model": "x"}}, KindOpenAI, `must not set "model"`},
		"token param":         {Model{ID: "c", ChatTokenParam: "max"}, KindOpenAI, "max_tokens or max_completion_tokens"},
		"negative allowance":  {Model{ID: "c", ChatReasoningTokenAllowance: -1}, KindOpenAI, "negative"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.model.Validate(tc.kind)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRegistryEndpointFollowsTheModelsProvider(t *testing.T) {
	gateway := Provider{Name: "gateway", Kind: KindOpenAI, Endpoint: llmclient.Endpoint{BaseURL: "https://gw.example/v1", APIKey: "k", Auth: llmclient.AuthBearer}}
	keyless := Provider{Name: "local", Kind: KindOpenAI, Endpoint: llmclient.Endpoint{BaseURL: "http://localhost:11434/v1"}}
	registry := New([]Provider{gateway, keyless}, []Model{
		{Provider: "gateway", ID: "a"}, {Provider: "gateway", ID: "a", Protocols: []Protocol{ProtocolChat}},
		{Provider: "off", ID: "b"}, {Provider: "local", ID: "c"},
	})
	models := registry.Models()
	require.Len(t, models, 3, "a repeated ref keeps its first entry")
	assert.Empty(t, models[0].Protocols)

	entry, ok := registry.Lookup("gateway/a")
	require.True(t, ok)
	endpoint, err := registry.Endpoint(entry)
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example/v1", endpoint.BaseURL)

	_, err = registry.Endpoint(Model{Provider: "off", ID: "b"})
	assert.ErrorContains(t, err, "off or removed")
	_, err = registry.Endpoint(Model{Provider: "local", ID: "c"})
	assert.ErrorContains(t, err, "has no key")
	assert.Equal(t, []string{"gateway", "local"}, []string{registry.Providers()[0].Name, registry.Providers()[1].Name})
}

func TestProviderFormatHidesTheKey(t *testing.T) {
	p := Provider{Name: "gateway", Kind: KindOpenAI, Endpoint: llmclient.Endpoint{APIKey: "sk-secret"}, Version: 3}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, sprintf(verb, p), "sk-secret")
	}
}
