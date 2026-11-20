package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
)

// TestValidateModelSelection pins the built-in registry to the Ask
// allowlist it replaced: the same models, efforts, and service tiers.
func TestValidateModelSelection(t *testing.T) {
	t.Parallel()

	selection := func(modelID, effort, tier string) error {
		return ValidateModelSelection(registryModel(t, modelID), effort, tier)
	}
	for _, modelID := range []string{"gpt-6-sol:main", "gpt-5.6-sol:main", "gpt-5.6-sol:cloud", "gpt-5.6-sol:direct", "claude-opus-5:main", "claude-sonnet-5:main"} {
		for _, effort := range []string{"", "none", "low", "medium", "high", "xhigh", "max"} {
			require.NoError(t, selection(modelID, effort, ""), "%s/%s", modelID, effort)
		}
		require.ErrorContains(t, selection(modelID, "minimal", ""), "does not support")
	}
	for _, modelID := range []string{"gpt-6.1-sol:main", "gpt-6-astra:main"} {
		for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
			require.NoError(t, selection(modelID, effort, ""), "%s/%s", modelID, effort)
		}
		for _, effort := range []string{"none", "minimal"} {
			require.ErrorContains(t, selection(modelID, effort, ""), "does not support")
		}
	}
	for _, modelID := range []string{"gpt-6-sol:main", "gpt-6.1-sol:main", "gpt-6-astra:main"} {
		for _, tier := range []string{"", "default", "fast", "priority"} {
			require.NoError(t, selection(modelID, "", tier), "%s/%s", modelID, tier)
		}
		require.ErrorContains(t, selection(modelID, "", "turbo"), "not supported")
	}
	require.NoError(t, selection("gpt-5.6-sol:direct", "", "fast"))
	require.NoError(t, selection("gpt-5.6-sol:direct", "", "priority"))
	require.ErrorContains(t, selection("gpt-5.6-sol:direct", "", "turbo"), "not supported")
	require.ErrorContains(t, selection("gpt-5.6-sol:cloud", "", "fast"), "does not support")
	require.ErrorContains(t, selection("gpt-5.6-sol:main", "", "fast"), "does not support")
	for _, modelID := range []string{"claude-opus-5:main", "claude-sonnet-5:main"} {
		require.ErrorContains(t, selection(modelID, "", "fast"), "does not support")
	}
	// Chat-only entries exist for other roles and cannot drive Ask.
	require.ErrorContains(t, selection("gemini-3.6-flash", "", ""), "no agentic protocol")
}

func TestModelFactoryRoutesResponsesProtocol(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		modelID   string
		tier      string
		retention string
	}{
		{modelID: "gpt-6-sol:main", tier: "fast"},
		{modelID: "gpt-6.1-sol:main", tier: "fast"},
		{modelID: "gpt-6-astra:main", tier: "fast"},
		{modelID: "gpt-5.6-sol:main", retention: "24h"},
		{modelID: "gpt-5.6-sol:cloud", retention: "24h"},
		{modelID: "gpt-5.6-sol:direct", tier: "fast", retention: "24h"},
	} {
		modelID := testCase.modelID
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			requests := make(chan recordedModelRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- decodeModelRequest(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
				require.NoError(t, err)
			}))
			defer server.Close()

			serviceTier := testCase.tier
			mdl, err := NewModel(context.Background(), ModelConfig{
				BaseURL: server.URL + "/v1", APIKey: "gateway-token",
				Model: registryModel(t, modelID), MaxTokens: 256, ReasoningEffort: "high", ServiceTier: serviceTier,
			})
			require.NoError(t, err)
			require.Equal(t, modelregistry.ProtocolResponses, mdl.Protocol())

			_, err = mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("hello")})
			require.NoError(t, err)
			req := <-requests
			require.Equal(t, "/v1/responses", req.path)
			require.Equal(t, "Bearer gateway-token", req.header.Get("Authorization"))
			require.Equal(t, modelID, req.body["model"])
			require.Equal(t, "high", req.body["reasoning"].(map[string]any)["effort"])
			require.Equal(t, []any{"reasoning.encrypted_content"}, req.body["include"])
			if testCase.retention == "" {
				require.NotContains(t, req.body, "prompt_cache_retention")
			} else {
				require.Equal(t, testCase.retention, req.body["prompt_cache_retention"])
			}
			if serviceTier == "" {
				require.NotContains(t, req.body, "service_tier")
			} else {
				require.Equal(t, serviceTier, req.body["service_tier"])
			}
		})
	}
}

func TestModelFactoryRoutesNativeClaudeReasoning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		effort         string
		wantThinking   string
		wantEffort     string
		wantThinkingOn bool
	}{
		{name: "provider default"},
		{name: "explicit disabled", effort: "none", wantThinking: "disabled", wantThinkingOn: true},
		{name: "adaptive high", effort: "high", wantThinking: "adaptive", wantEffort: "high", wantThinkingOn: true},
		{name: "adaptive max", effort: "max", wantThinking: "adaptive", wantEffort: "max", wantThinkingOn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan recordedModelRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- decodeModelRequest(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5:main","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`))
				require.NoError(t, err)
			}))
			defer server.Close()

			mdl, err := NewModel(context.Background(), ModelConfig{
				BaseURL: server.URL + "/v1", APIKey: "gateway-token",
				Model: registryModel(t, "claude-opus-5:main"), MaxTokens: 256, ReasoningEffort: tt.effort,
			})
			require.NoError(t, err)
			require.Equal(t, modelregistry.ProtocolMessages, mdl.Protocol())

			_, err = mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("hello")})
			require.NoError(t, err)
			req := <-requests
			require.Equal(t, "/v1/messages", req.path)
			require.Equal(t, "Bearer gateway-token", req.header.Get("Authorization"))
			require.Empty(t, req.header.Get("X-Api-Key"), "gateway bearer must not be sent as an Anthropic API key")
			require.Equal(t, "claude-opus-5:main", req.body["model"])
			thinking, hasThinking := req.body["thinking"].(map[string]any)
			require.Equal(t, tt.wantThinkingOn, hasThinking)
			if hasThinking {
				require.Equal(t, tt.wantThinking, thinking["type"])
			}
			outputConfig, hasOutputConfig := req.body["output_config"].(map[string]any)
			if tt.wantEffort == "" {
				require.False(t, hasOutputConfig, "output_config must be omitted")
			} else {
				require.Equal(t, tt.wantEffort, outputConfig["effort"])
			}
		})
	}
}

// TestRunReasoningEffortReachesEachCall: a model built with no effort sends
// the effort its call's context carries, in the protocol's own encoding, and
// an option the caller passes (a request override) still wins.
func TestRunReasoningEffortReachesEachCall(t *testing.T) {
	t.Parallel()
	requests := make(chan recordedModelRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeModelRequest(t, r)
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	build := func(id string) *ConfiguredModel {
		mdl, err := NewModel(context.Background(), ModelConfig{
			BaseURL: server.URL + "/v1", APIKey: "token", Model: registryModel(t, id), MaxTokens: 256,
		})
		require.NoError(t, err)
		return mdl
	}
	generate := func(mdl *ConfiguredModel, ctx context.Context, opts ...model.Option) map[string]any {
		_, err := mdl.Generate(ctx, []*schema.AgenticMessage{NewAgenticUserText("hello")}, opts...)
		require.NoError(t, err)
		return (<-requests).body
	}

	responses := build("gpt-5.6-sol:direct")
	assert.NotContains(t, generate(responses, context.Background()), "reasoning", "no effort: the model's default")
	assert.NotContains(t, generate(responses, WithReasoningEffort(context.Background(), "")), "reasoning")
	assert.Equal(t, map[string]any{"effort": "medium"}, generate(responses, WithReasoningEffort(context.Background(), "medium"))["reasoning"])
	high := "high"
	overridden := generate(responses, WithReasoningEffort(context.Background(), "medium"),
		modelOptionsForOverrides(ModelOverrides{ReasoningEffort: &high}, nil)...)
	assert.Equal(t, map[string]any{"effort": "high"}, overridden["reasoning"])

	claude := build("claude-opus-5:main")
	body := generate(claude, context.Background())
	assert.NotContains(t, body, "thinking")
	assert.NotContains(t, body, "output_config")
	body = generate(claude, WithReasoningEffort(context.Background(), "high"))
	assert.Equal(t, map[string]any{"type": "adaptive"}, body["thinking"])
	assert.Equal(t, map[string]any{"effort": "high"}, body["output_config"])
	body = generate(claude, WithReasoningEffort(context.Background(), "none"))
	assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
	assert.NotContains(t, body, "output_config")
}

// TestModelFactoryMessagesAPIKeyAuth: the x-api-key style is what the public
// Anthropic API accepts; the bearer must not be sent alongside it.
func TestModelFactoryMessagesAPIKeyAuth(t *testing.T) {
	t.Parallel()

	requests := make(chan recordedModelRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- decodeModelRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	mdl, err := NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL, APIKey: "sk-ant-test", Auth: llmclient.AuthXAPIKey,
		Model:     modelregistry.Model{ID: "claude-sonnet-4-5", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages}},
		MaxTokens: 256,
	})
	require.NoError(t, err)
	_, err = mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("hello")})
	require.NoError(t, err)
	req := <-requests
	require.Equal(t, "/v1/messages", req.path)
	require.Equal(t, "sk-ant-test", req.header.Get("X-Api-Key"))
	require.Empty(t, req.header.Get("Authorization"))
	require.NotEmpty(t, req.header.Get("Anthropic-Version"))
	require.NotContains(t, req.body, "thinking")
	require.NotContains(t, req.body, "output_config")

	_, err = NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL, APIKey: "k", Auth: llmclient.AuthXAPIKey,
		Model: registryModel(t, "gpt-5.6-sol:main"), MaxTokens: 256,
	})
	require.ErrorContains(t, err, "only supports bearer auth")
}

func TestModelFactoryRejectsURLCredentialsWithoutLeakingThem(t *testing.T) {
	t.Parallel()

	const secret = "do-not-log-this"
	_, err := NewModel(context.Background(), ModelConfig{
		BaseURL: "https://user:" + secret + "@gateway.example/v1",
		APIKey:  "token", Model: registryModel(t, "claude-opus-5:main"), MaxTokens: 1,
	})
	require.ErrorContains(t, err, "must not contain userinfo")
	require.NotContains(t, err.Error(), secret)
}

func TestNativeClaudeSupportsForcedFunctionTool(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeModelRequest(t, r)
		require.Equal(t, "/v1/messages", req.path)
		tools := req.body["tools"].([]any)
		require.Len(t, tools, 1)
		require.Equal(t, "lookup", tools[0].(map[string]any)["name"])
		choice := req.body["tool_choice"].(map[string]any)
		require.Equal(t, "tool", choice["type"])
		require.Equal(t, "lookup", choice["name"])

		w.Header().Set("Content-Type", "application/json")
		writeClaudeMessage(t, w, map[string]any{
			"id": "msg_tool", "stop_reason": "tool_use",
			"content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_forced", "name": "lookup", "input": map[string]any{},
			}},
		})
	}))
	defer server.Close()

	mdl, err := NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL + "/v1", APIKey: "gateway-token",
		Model: registryModel(t, "claude-opus-5:main"), MaxTokens: 256,
	})
	require.NoError(t, err)
	out, err := mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("use lookup")},
		model.WithTools([]*schema.ToolInfo{{
			Name: "lookup", Desc: "Look up a value.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
		}}),
		model.WithAgenticToolChoice(&schema.AgenticToolChoice{
			Type: schema.ToolChoiceForced,
			Forced: &schema.AgenticForcedToolChoice{Tools: []*schema.AllowedTool{
				{FunctionName: "lookup"},
			}},
		}),
	)
	require.NoError(t, err)
	require.Equal(t, "toolu_forced", AgenticMessageToolCalls(out)[0].CallID)
}

type recordedModelRequest struct {
	path   string
	header http.Header
	body   map[string]any
}

func decodeModelRequest(t *testing.T, r *http.Request) recordedModelRequest {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	return recordedModelRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}
}

func TestClaudeBaseURL(t *testing.T) {
	t.Parallel()

	got, err := claudeBaseURL("https://gateway.example/prefix/v1/")
	require.NoError(t, err)
	require.Equal(t, "https://gateway.example/prefix/", got)

	_, err = claudeBaseURL("https://gateway.example/v1?x=1")
	require.ErrorContains(t, err, "query or fragment")
}
