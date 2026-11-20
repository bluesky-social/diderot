//go:build gateway

package ask

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
)

// These tests send Ask's real requests to a real model provider. They are
// built only with the gateway tag, and each skips unless the provider and
// the models are named in the environment:
//
//	DIDEROT_TEST_PROVIDER_URL          base URL of an OpenAI-compatible provider or gateway, with any /v1
//	DIDEROT_TEST_PROVIDER_KEY          its API key
//	DIDEROT_TEST_ASK_MODELS            comma-separated model IDs to run Ask on
//	DIDEROT_TEST_ASK_REASONING_EFFORT  optional: the effort sent with each
//	DIDEROT_TEST_ASK_SERVICE_TIER      optional: the service tier sent with each
//
//	go test -tags=gateway -v -count=1 -timeout 20m ./internal/ask -run TestProviderAsk
//
// The key is never printed.

// providerAskModels returns the provider's address and key and, for each
// model DIDEROT_TEST_ASK_MODELS names, the entry Ask's own test makes of it
// (the protocol it answers on), as choosing it on the Models page would.
func providerAskModels(t *testing.T) (baseURL, apiKey string, models []modelregistry.Model) {
	t.Helper()
	baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_URL")), "/")
	apiKey = strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_KEY"))
	ids := strings.FieldsFunc(os.Getenv("DIDEROT_TEST_ASK_MODELS"), func(r rune) bool { return r == ',' || r == ' ' })
	if baseURL == "" || apiKey == "" || len(ids) == 0 {
		t.Skip("set DIDEROT_TEST_PROVIDER_URL, DIDEROT_TEST_PROVIDER_KEY and DIDEROT_TEST_ASK_MODELS to run")
	}
	provider := modelregistry.Provider{Name: "provider", Kind: modelregistry.KindOpenAI, Endpoint: llmclient.Endpoint{
		BaseURL: baseURL, APIKey: apiKey, Auth: llmclient.AuthBearer,
	}}
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		entry, err := modelregistry.TestForRole(ctx, provider, modelregistry.NewEntry("provider", id), modelregistry.RoleAsk,
			modelregistry.TestOptions{Effort: providerAskEffort(), ServiceTier: providerAskServiceTier()})
		cancel()
		gatewayInferenceRequireNoError(t, err, apiKey)
		models = append(models, entry)
	}
	return baseURL, apiKey, models
}

func providerAskEffort() string {
	return strings.TrimSpace(os.Getenv("DIDEROT_TEST_ASK_REASONING_EFFORT"))
}

func providerAskServiceTier() string {
	return strings.TrimSpace(os.Getenv("DIDEROT_TEST_ASK_SERVICE_TIER"))
}

// TestProviderAskToolRoundTrip: the first model calls an ordinary tool and
// then returns Ask's structured final response.
func TestProviderAskToolRoundTrip(t *testing.T) {
	baseURL, apiKey, models := providerAskModels(t)

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	mdl, err := NewModel(ctx, ModelConfig{
		BaseURL: baseURL, APIKey: apiKey, Model: models[0],
		Timeout: 2 * time.Minute, MaxTokens: 4096, ReasoningEffort: providerAskEffort(), ServiceTier: providerAskServiceTier(),
	})
	gatewayInferenceRequireNoError(t, err, apiKey)

	type probeInput struct {
		Nonce string `json:"nonce"`
	}
	nonce := uuid.NewString()
	var called atomic.Bool
	probe, err := utils.InferTool("diderot_probe", "Call exactly once with the nonce supplied by the user before answering.",
		func(_ context.Context, input probeInput) (string, error) {
			require.Equal(t, nonce, input.Nonce)
			called.Store(true)
			return `{"verified":true}`, nil
		})
	require.NoError(t, err)

	agents, err := NewAskAgents(ctx, mdl, []tool.BaseTool{probe},
		"Call diderot_probe exactly once with the requested nonce, then confirm it succeeded.", 4)
	require.NoError(t, err)
	iter := agents.HTTP.Run(ctx, []*schema.AgenticMessage{NewAgenticUserText("Use nonce " + nonce + " and then answer.")},
		adk.WithChatModelOptions([]model.Option{finalResponseModelOption()}))
	var final *schema.AgenticMessage
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		gatewayInferenceRequireNoError(t, event.Err, apiKey)
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, getErr := event.Output.MessageOutput.GetMessage()
		gatewayInferenceRequireNoError(t, getErr, apiKey)
		if msg != nil {
			final = msg
		}
	}
	require.True(t, called.Load(), "the model must complete the ordinary tool round")
	require.NotNil(t, final)
	require.Empty(t, AgenticMessageToolCalls(final))
	response, err := decodeFinalResponseMessage(final)
	gatewayInferenceRequireNoError(t, err, apiKey)
	require.NotEmpty(t, strings.TrimSpace(response.Answer))
	require.NotNil(t, response.Artifacts)
}

// TestProviderAskReadinessProbe proves the deliberately small readiness
// request is one each model accepts.
func TestProviderAskReadinessProbe(t *testing.T) {
	baseURL, apiKey, models := providerAskModels(t)

	for _, entry := range models {
		t.Run(entry.ID, func(t *testing.T) {
			check, err := NewGatewayCheck(t.Context(), GatewayCheckOptions{
				BaseURL:     baseURL,
				APIKey:      apiKey,
				Model:       entry,
				ServiceTier: providerAskServiceTier(),
				Timeout:     20 * time.Second,
			})
			gatewayInferenceRequireNoError(t, err, apiKey)
			gatewayInferenceRequireNoError(t, check.CheckOnce(t.Context()), apiKey)
			require.True(t, check.Ready())
		})
	}
}

// TestProviderAskPersistenceReplay proves each model can consume its own
// previous tool turn after Diderot's durable JSON projection of it is
// written and reloaded, as a follow-up question replays it. Reasoning the
// provider signs or encrypts must come back from the projection unchanged;
// a provider that returns none passes too. On the Messages protocol the
// stored row must not carry the SDK's own extension of the message.
func TestProviderAskPersistenceReplay(t *testing.T) {
	baseURL, apiKey, models := providerAskModels(t)

	for _, entry := range models {
		t.Run(entry.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			mdl, err := NewModel(ctx, ModelConfig{
				BaseURL: baseURL, APIKey: apiKey, Model: entry,
				Timeout: 2 * time.Minute, MaxTokens: 4096, ReasoningEffort: providerAskEffort(), ServiceTier: providerAskServiceTier(),
			})
			gatewayInferenceRequireNoError(t, err, apiKey)
			messages := mdl.Protocol() == modelregistry.ProtocolMessages

			const toolName = "diderot_persistence_probe"
			nonce := uuid.NewString()
			toolInfo := &schema.ToolInfo{
				Name: toolName,
				Desc: "Persist the supplied nonce. Call this tool exactly once before answering.",
				ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
					"nonce": {Type: schema.String, Desc: "opaque nonce to persist", Required: true},
				}),
			}
			headers := mdl.WithCustomHeaders(map[string]string{
				"x-agent-parent-session-id": uuid.NewString(),
			})

			user := NewAgenticUserText("Think carefully, then call " + toolName + " with nonce " + nonce + ".")
			toolResponse, err := mdl.Generate(ctx, []*schema.AgenticMessage{user},
				model.WithTools([]*schema.ToolInfo{toolInfo}),
				model.WithAgenticToolChoice(&schema.AgenticToolChoice{
					Type: schema.ToolChoiceForced,
					Forced: &schema.AgenticForcedToolChoice{Tools: []*schema.AllowedTool{
						{FunctionName: toolName},
					}},
				}),
				headers,
			)
			gatewayInferenceRequireNoError(t, err, apiKey)
			if messages {
				require.NotNil(t, toolResponse.ResponseMeta)
				require.NotNil(t, toolResponse.ResponseMeta.ClaudeExtension)
			}
			calls := AgenticMessageToolCalls(toolResponse)
			require.Len(t, calls, 1)
			require.Equal(t, toolName, calls[0].Name)
			var arguments struct {
				Nonce string `json:"nonce"`
			}
			gatewayInferenceRequireNoError(t, json.Unmarshal([]byte(calls[0].Arguments), &arguments), apiKey)
			require.Equal(t, nonce, arguments.Nonce)

			toolRow, err := encodeAgenticMessageRow(toolResponse)
			gatewayInferenceRequireNoError(t, err, apiKey)
			if messages {
				require.NotContains(t, string(toolRow.JSON), "claude_extension")
			}
			replayedToolResponse, err := decodeAgenticMessageRow(toolRow)
			gatewayInferenceRequireNoError(t, err, apiKey)
			require.Equal(t, gatewayInferenceThinkingSignature(toolResponse), gatewayInferenceThinkingSignature(replayedToolResponse))
			require.Equal(t, calls[0].CallID, AgenticMessageToolCalls(replayedToolResponse)[0].CallID)

			toolResult := newAgenticToolResult(calls[0].CallID, toolName, `{"persisted":true}`)
			final, err := mdl.Generate(ctx, []*schema.AgenticMessage{user, replayedToolResponse, toolResult},
				model.WithTools([]*schema.ToolInfo{toolInfo}),
				model.WithAgenticToolChoice(&schema.AgenticToolChoice{Type: schema.ToolChoiceForbidden}),
				headers,
			)
			gatewayInferenceRequireNoError(t, err, apiKey)
			require.Empty(t, AgenticMessageToolCalls(final))
			require.NotEmpty(t, strings.TrimSpace(AgenticMessageText(final)))

			finalRow, err := encodeAgenticMessageRow(final)
			gatewayInferenceRequireNoError(t, err, apiKey)
			if messages {
				require.NotContains(t, string(finalRow.JSON), "claude_extension")
			}
			replayedFinal, err := decodeAgenticMessageRow(finalRow)
			gatewayInferenceRequireNoError(t, err, apiKey)
			require.Equal(t, AgenticMessageText(final), AgenticMessageText(replayedFinal))
		})
	}
}

func gatewayInferenceThinkingSignature(msg *schema.AgenticMessage) string {
	if msg == nil {
		return ""
	}
	for _, block := range msg.ContentBlocks {
		if block != nil && block.Reasoning != nil && block.Reasoning.Signature != "" {
			return block.Reasoning.Signature
		}
	}
	return ""
}

func gatewayInferenceRequireNoError(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	require.FailNow(t, "provider request failed", "%s", message)
}
