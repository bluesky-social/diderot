package codexprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/cloudwego/eino/schema"
)

func testConfig() Config {
	return Config{Model: "test-model", Token: "test-token", Timeout: time.Second, MaxConcurrent: 1}
}

func TestProviderSupportsDiderotModelProbes(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	server := httptest.NewServer(newHandler(cfg, func(_ context.Context, prompt []byte, effort string) (turnResult, error) {
		require.Contains(t, string(prompt), "Reply with OK.")
		_, raw, _ := strings.Cut(string(prompt), "\nUse only")
		_, raw, _ = strings.Cut(raw, "\n")
		var req request
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			return turnResult{}, err
		}
		var forced llmclient.ResponsesToolChoice
		if json.Unmarshal(req.ToolChoice, &forced) == nil && forced.Type == "function" {
			return turnResult{ToolCalls: []toolCall{{Name: forced.Name, Arguments: `{"query":"ok"}`}}}, nil
		}
		return turnResult{Answer: "ok"}, nil
	}))
	defer server.Close()
	provider := modelregistry.Provider{
		Name: "codex", Kind: modelregistry.KindOpenAI,
		Endpoint: llmclient.Endpoint{BaseURL: server.URL + "/v1", APIKey: cfg.Token},
	}

	ctx := context.Background()
	models, err := modelregistry.List(ctx, server.Client(), provider)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, cfg.Model, models[0].ID)

	entry := modelregistry.NewEntry(provider.Name, cfg.Model)
	for _, role := range []modelregistry.Role{modelregistry.RoleAsk, modelregistry.RoleSessionSummaryMap, modelregistry.RoleSessionSummaryReduce, modelregistry.RoleTitles} {
		entry, err = modelregistry.TestForRole(ctx, provider, entry, role, modelregistry.TestOptions{})
		require.NoError(t, err, string(role))
		require.True(t, entry.ServesRole(role))
	}
}

func TestTextResponseAndToolResultReplayThroughEino(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	server := httptest.NewServer(newHandler(cfg, func(_ context.Context, prompt []byte, effort string) (turnResult, error) {
		require.Equal(t, "low", effort)
		require.Contains(t, string(prompt), "What did we decide?")
		require.Contains(t, string(prompt), `"type":"function_call_output"`)
		require.Contains(t, string(prompt), "synthetic-source-sentinel")
		return turnResult{Answer: "We kept the original plan.", Usage: usage{InputTokens: 20, OutputTokens: 9}}, nil
	}))
	defer server.Close()

	model, err := ask.NewResponsesModel(context.Background(), ask.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: cfg.Token, ModelID: cfg.Model,
		ReasoningEffort: "low", MaxRetries: ask.NoResponsesRetries,
	})
	require.NoError(t, err)
	result, err := model.Generate(context.Background(), []*schema.AgenticMessage{
		schema.UserAgenticMessage("What did we decide?"),
		{
			Role: schema.AgenticRoleTypeUser,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{
				CallID: "call_example",
				Content: []*schema.FunctionToolResultContentBlock{
					{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "synthetic-source-sentinel"}},
				},
			})},
		},
	})
	require.NoError(t, err)
	require.Contains(t, result.ContentBlocks[0].AssistantGenText.Text, "original plan")
}

func TestProviderRejectsUnsupportedRequestsAndToolOutput(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	handler := newHandler(cfg, func(_ context.Context, _ []byte, _ string) (turnResult, error) {
		return turnResult{ToolCalls: []toolCall{{Name: "unknown", Arguments: "{}"}}}, nil
	})
	for _, tc := range []struct {
		name, token, body string
		code              int
	}{
		{"unauthenticated", "", `{}`, http.StatusUnauthorized},
		{"wrong model", cfg.Token, `{"model":"other","input":"text"}`, http.StatusBadRequest},
		{"streaming", cfg.Token, `{"model":"test-model","input":"text","stream":true}`, http.StatusBadRequest},
		{"image", cfg.Token, `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_image"}]}]}`, http.StatusBadRequest},
		{"trailing data", cfg.Token, `{"model":"test-model","input":"text"}{}`, http.StatusBadRequest},
		{"unknown tool output", cfg.Token, `{"model":"test-model","input":"text","tools":[{"type":"function","name":"known"}]}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			require.Equal(t, tc.code, res.Code, res.Body.String())
			require.True(t, json.Valid(res.Body.Bytes()))
		})
	}
}
