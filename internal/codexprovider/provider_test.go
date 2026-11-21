package codexprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
)

func testConfig() Config {
	return Config{Model: "test-model", Token: "test-token", Timeout: time.Second, MaxConcurrent: 1}
}

func TestResponsesQueueWaitsForActiveRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig()
		release := make(chan struct{})
		calls := 0
		handler := newHandler(cfg, func(context.Context, request) (json.RawMessage, error) {
			calls++
			<-release
			return json.RawMessage(`{"status":"completed"}`), nil
		}, nil)

		send := func(ctx context.Context) *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"test-model","input":"Hello"}`))
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}

		ctx := context.Background()
		completed := make(chan int, 1+maxQueuedRequests)
		go func() { completed <- send(ctx).Code }()
		synctest.Wait()
		waiting, cancel := context.WithCancel(ctx)
		defer cancel()
		canceled := make(chan int, 1)
		go func() { canceled <- send(waiting).Code }()
		for range maxQueuedRequests - 1 {
			go func() { completed <- send(ctx).Code }()
		}
		synctest.Wait()

		assert.Equal(t, 1, calls)
		assert.Empty(t, completed)
		assert.Empty(t, canceled)
		overflow := send(ctx)
		assert.Equal(t, http.StatusServiceUnavailable, overflow.Code)
		assert.Contains(t, overflow.Body.String(), "Codex request queue is full")

		cancel()
		synctest.Wait()
		assert.Equal(t, 1, calls)
		assert.Equal(t, http.StatusGatewayTimeout, <-canceled)

		close(release)
		for range maxQueuedRequests {
			assert.Equal(t, http.StatusOK, <-completed)
		}
		assert.Equal(t, maxQueuedRequests, calls)
	})
}

func TestCodexTransportSupportsDiderotModelProbesAndToolReplay(t *testing.T) {
	t.Parallel()
	fresh := testJWT(t, time.Now().Add(time.Hour))
	authFile := testAuthFile(t, fresh, "test-refresh")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+fresh, r.Header.Get("Authorization"))
		assert.Equal(t, "test-account", r.Header.Get("ChatGPT-Account-Id"))
		if r.Method == http.MethodGet {
			assert.Equal(t, "/codex/models", r.URL.Path)
			assert.Equal(t, "0.170.0", r.URL.Query().Get("client_version"))
			assert.Equal(t, "application/json", r.Header.Get("Accept"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"models":[
				{"slug":"test-model","display_name":"Test model","visibility":"list","context_window":500000,"input_modalities":["text","image"],"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"ultra"}]},
				{"slug":"other-visible","visibility":"list"},
				{"slug":"hidden-model","visibility":"hide"}
			]}`)
			return
		}
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		var body request
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		assert.Equal(t, "true", string(body["stream"]))
		assert.Equal(t, "false", string(body["store"]))
		assert.NotEmpty(t, jsonString(body["instructions"]))
		assert.NotContains(t, body, "max_output_tokens")
		assert.NotContains(t, body, "prompt_cache_retention")

		item := `{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"We kept the original plan.","annotations":[]}]}`
		if strings.HasPrefix(string(body["tool_choice"]), "{") {
			item = `{"type":"function_call","id":"fc_test","call_id":"call_test","name":"lookup","arguments":"{\"query\":\"ok\"}","status":"completed"}`
		}
		if strings.Contains(string(body["input"]), "synthetic-source-sentinel") {
			assert.Equal(t, "Follow the evidence.", jsonString(body["instructions"]))
			assert.NotContains(t, string(body["input"]), "Follow the evidence.")
			assert.Contains(t, string(body["input"]), "function_call_output")
			assert.JSONEq(t, `{"effort":"high"}`, string(body["reasoning"]))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"test-model\",\"created_at\":1,\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	cfg := testConfig()
	bridge := &codexBackend{
		client: upstream.Client(), url: upstream.URL + "/codex/responses",
		modelsURL:   upstream.URL + "/codex/models?client_version=0.170.0",
		credentials: &credentialStore{path: authFile},
	}
	cfg.Model = ""
	server := httptest.NewServer(newHandler(cfg, bridge.respond, bridge.models))
	defer server.Close()

	ctx := context.Background()
	provider := modelregistry.Provider{
		Name: "codex", Kind: modelregistry.KindOpenAI,
		Endpoint: llmclient.Endpoint{BaseURL: server.URL + "/v1", APIKey: cfg.Token},
	}
	models, err := modelregistry.List(ctx, server.Client(), provider)
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "other-visible", models[0].ID)
	require.Equal(t, "test-model", models[1].ID)
	require.Equal(t, 500000, models[1].ContextTokens)
	require.True(t, *models[1].Images)
	require.Equal(t, []string{"low", "high"}, models[1].Efforts)
	require.Equal(t, "low", models[1].DefaultEffort)

	pinned, err := bridge.models(ctx, "test-model")
	require.NoError(t, err)
	require.Len(t, pinned, 1)
	require.Equal(t, "test-model", pinned[0].ID)
	_, err = bridge.models(ctx, "hidden-model")
	require.ErrorContains(t, err, "not selectable")

	entry := modelregistry.NewEntry(provider.Name, "test-model").
		WithPublished(modelregistry.Published{Provider: models[1].Facts()}, false)
	require.Equal(t, modelregistry.SourceProvider, entry.SourceOf(modelregistry.FactEfforts))
	for _, role := range []modelregistry.Role{modelregistry.RoleAsk, modelregistry.RoleSessionSummaryMap, modelregistry.RoleSessionSummaryReduce, modelregistry.RoleTitles} {
		entry, err = modelregistry.TestForRole(ctx, provider, entry, role, modelregistry.TestOptions{})
		require.NoError(t, err, string(role))
		require.True(t, entry.ServesRole(role))
	}

	model, err := ask.NewResponsesModel(ctx, ask.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: cfg.Token, ModelID: "test-model",
		ReasoningEffort: "high",
		MaxRetries:      ask.NoResponsesRetries, MaxTokens: 1024, PromptCacheRetention: "in_memory",
	})
	require.NoError(t, err)
	result, err := model.Generate(ctx, []*schema.AgenticMessage{
		schema.SystemAgenticMessage("Follow the evidence."),
		schema.UserAgenticMessage("What did we decide?"),
		{
			Role: schema.AgenticRoleTypeUser,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{
				CallID: "call_test",
				Content: []*schema.FunctionToolResultContentBlock{
					{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "synthetic-source-sentinel"}},
				},
			})},
		},
	})
	require.NoError(t, err)
	require.Contains(t, result.ContentBlocks[0].AssistantGenText.Text, "original plan")
}

func TestRefreshRotatesAndPersistsCredentials(t *testing.T) {
	t.Parallel()
	expired := testJWT(t, time.Now().Add(-time.Hour))
	fresh := testJWT(t, time.Now().Add(time.Hour))
	authFile := testAuthFile(t, expired, "test-refresh")
	var calls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		assert.Equal(t, codexClientID, r.Form.Get("client_id"))
		assert.Equal(t, "test-refresh", r.Form.Get("refresh_token"))
		writeJSON(w, http.StatusOK, map[string]string{"access_token": fresh, "refresh_token": "new-refresh"})
	}))
	defer oauth.Close()
	s := &credentialStore{path: authFile, client: oauth.Client(), tokenURL: oauth.URL}
	c, err := s.get(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, fresh, c.access)
	c, err = s.get(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, fresh, c.access)
	require.Equal(t, int32(1), calls.Load())
	saved, err := os.ReadFile(authFile)
	require.NoError(t, err)
	require.Contains(t, string(saved), "new-refresh")
	require.Contains(t, string(saved), "preserved")
}

func TestProviderRejectsUnauthorizedAndStatefulRequests(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	handler := newHandler(cfg, func(_ context.Context, _ request) (json.RawMessage, error) {
		t.Error("request reached the backend")
		return nil, nil
	}, nil)
	for _, tc := range []struct {
		name, token, body string
		code              int
	}{
		{"unauthenticated", "", `{}`, http.StatusUnauthorized},
		{"wrong model", cfg.Token, `{"model":"other","input":"text"}`, http.StatusBadRequest},
		{"streaming", cfg.Token, `{"model":"test-model","input":"text","stream":true}`, http.StatusBadRequest},
		{"cached conversation", cfg.Token, `{"model":"test-model","input":"text","previous_response_id":"resp_1"}`, http.StatusBadRequest},
		{"hosted tool", cfg.Token, `{"model":"test-model","input":"text","tools":[{"type":"web_search"}]}`, http.StatusBadRequest},
		{"trailing data", cfg.Token, `{"model":"test-model","input":"text"}{}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			require.Equal(t, tc.code, res.Code, res.Body.String())
		})
	}
}

func testJWT(t *testing.T, expires time.Time) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"exp": expires.Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "test-account"},
	})
	require.NoError(t, err)
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func testAuthFile(t *testing.T, access, refresh string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	content, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "unrelated": "preserved",
		"tokens": map[string]string{"access_token": access, "refresh_token": refresh},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0600))
	return path
}
