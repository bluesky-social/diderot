package ask

import (
	"testing"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

var allEfforts = []string{"none", "low", "medium", "high", "xhigh", "max"}

// gatewayModels are model entries as a single model gateway that serves the
// Responses and Messages protocols under aliases would have them.
var gatewayModels = []modelregistry.Model{
	{ID: "gpt-6-sol:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts, ServiceTier: true},
	{ID: "gpt-6.1-sol:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, ServiceTier: true},
	{ID: "gpt-6-astra:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, ServiceTier: true},
	{ID: "gpt-5.6-sol:cloud", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts, PromptCacheRetention: "24h"},
	{ID: "gpt-5.6-sol:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts, PromptCacheRetention: "24h"},
	{ID: "gpt-5.6-sol:direct", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts, ServiceTier: true, PromptCacheRetention: "24h"},
	{ID: "gpt-5.4-mini:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat},
		ReasoningEfforts: []string{"low"}},
	{ID: "gpt-5.4:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}},
	{ID: "claude-opus-5:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts},
	{ID: "claude-sonnet-5:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages, modelregistry.ProtocolChat},
		ReasoningEfforts: allEfforts},
	{ID: "claude-haiku-4-5:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages, modelregistry.ProtocolChat}},
	{ID: "gemini-3.6-flash", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}},
	{ID: "deepseek-v4.1-flash:main", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}},
}

// registryModel returns the gateway test entry for id.
func registryModel(t testing.TB, id string) modelregistry.Model {
	t.Helper()
	for _, model := range gatewayModels {
		if model.ID == id {
			model.Provider = "gateway"
			return model
		}
	}
	t.Fatalf("no gateway test model %q", id)
	return modelregistry.Model{}
}
