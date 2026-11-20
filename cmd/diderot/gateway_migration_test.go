package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

// gatewayEntries are the model entries of a deployment that reaches every
// model through one model gateway, under the gateway's aliases: each with
// its protocols, efforts, tier, retention and request fields. Every alias
// the gateway also serves on chat accepts images there.
var gatewayEntries = func() []modelregistry.Model {
	responsesChat := []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat}
	messagesChat := []modelregistry.Protocol{modelregistry.ProtocolMessages, modelregistry.ProtocolChat}
	chat := []modelregistry.Protocol{modelregistry.ProtocolChat}
	all := []string{"none", "low", "medium", "high", "xhigh", "max"}
	fromLow := []string{"low", "medium", "high", "xhigh", "max"}
	entries := []modelregistry.Model{
		{ID: "gpt-6-sol:main", Protocols: responsesChat, ReasoningEfforts: all, ServiceTier: true},
		{ID: "gpt-6.1-sol:main", Protocols: responsesChat, ReasoningEfforts: fromLow, ServiceTier: true},
		{ID: "gpt-6-astra:main", Protocols: responsesChat, ReasoningEfforts: fromLow, ServiceTier: true},
		{ID: "gpt-5.6-sol:main", Protocols: responsesChat, ReasoningEfforts: all, PromptCacheRetention: "24h"},
		{ID: "gpt-5.6-sol:cloud", Protocols: responsesChat, ReasoningEfforts: all, PromptCacheRetention: "24h"},
		{ID: "gpt-5.6-sol:direct", Protocols: responsesChat, ReasoningEfforts: all, ServiceTier: true, PromptCacheRetention: "24h"},
		{ID: "gpt-5.4-mini:main", Protocols: responsesChat, ReasoningEfforts: []string{"low"}},
		{ID: "gpt-5.4:main", Protocols: chat},
		{ID: "claude-opus-5:main", Protocols: messagesChat, ReasoningEfforts: all},
		{ID: "claude-sonnet-5:main", Protocols: messagesChat, ReasoningEfforts: all},
		{ID: "claude-haiku-4-5:main", Protocols: messagesChat},
		{ID: "gemini-3.8-flash", Protocols: chat, ChatExtraBody: map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}}},
		{ID: "deepseek-v4.1-flash:main", Protocols: chat},
	}
	for i := range entries {
		entries[i].Provider = "gateway"
		entries[i].Images = true
	}
	return entries
}()

// gatewayDefaults is the model each role uses.
var gatewayDefaults = map[string]string{
	"ask": "gpt-6.1-sol:main", "titles": "gpt-5.4-mini:main", "document_extraction": "gpt-5.4:main",
	"session_summary_reduce": "claude-sonnet-5:main", "session_summary_map": "claude-haiku-4-5:main",
	"image_captions": "gemini-3.8-flash", "thread_summaries": "deepseek-v4.1-flash:main",
}

// gatewayRoles is what that deployment's roles use.
var gatewayRoles = gatewayDefaults

// gatewayMigration is that deployment's models row for roles, which maps
// each role to a model ID: the gateway's entries, the roles' refs, the
// embedding collection, and Ask's limits, service tier and model timeout.
func gatewayMigration(roles map[string]string) settings.Models {
	models := settings.Models{
		Roles: map[string]string{}, Available: gatewayEntries, AskReasoningEffort: "medium", AskMaxToolTurns: 16,
		AskServiceTier: "fast", AskModelTimeout: settings.Duration(90 * time.Second),
		Embedding: &settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 1024,
			Collection: "diderot_chunks_local_embed_v2"},
	}
	for role, id := range roles {
		models.Roles[role] = modelregistry.Ref("gateway", id)
	}
	return models
}

// TestGatewayMigrationEntriesAreValid: every migrated entry is one the
// Models page could save, and every role's model serves it.
func TestGatewayMigrationEntriesAreValid(t *testing.T) {
	for _, entry := range gatewayEntries {
		require.NoError(t, entry.Validate(modelregistry.KindOpenAI), entry.ID)
	}
	registry := modelregistry.New([]modelregistry.Provider{{
		Name: "gateway", Kind: modelregistry.KindOpenAI,
		Endpoint: llmclient.Endpoint{BaseURL: "http://gateway.invalid/v1", APIKey: "k", Auth: llmclient.AuthBearer},
	}}, gatewayEntries)
	for role, id := range gatewayDefaults {
		entry, ok := registry.Lookup(modelregistry.Ref("gateway", id))
		require.True(t, ok, role)
		assert.True(t, entry.ServesRole(modelregistry.Role(role)), role)
	}
}
