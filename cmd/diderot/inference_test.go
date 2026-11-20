package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
	"github.com/bluesky-social/diderot/internal/settings"
)

// recordedRequest is one inference call as an endpoint saw it.
type recordedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Raw     []byte
	Body    map[string]any
	// Keys are the body's top-level keys in wire order.
	Keys []string
}

type inferenceRecorder struct {
	t      *testing.T
	server *httptest.Server
	dim    int
	mu     sync.Mutex
	calls  []recordedRequest
}

// newInferenceRecorder answers every protocol Diderot speaks with a minimal
// valid response and records what it received.
func newInferenceRecorder(t *testing.T, dim int) *inferenceRecorder {
	t.Helper()
	recorder := &inferenceRecorder{t: t, dim: dim}
	recorder.server = httptest.NewServer(http.HandlerFunc(recorder.serve))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (r *inferenceRecorder) serve(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(req.Body)
	require.NoError(r.t, err)
	var body map[string]any
	require.NoError(r.t, json.Unmarshal(raw, &body))
	r.mu.Lock()
	r.calls = append(r.calls, recordedRequest{
		Method: req.Method, Path: req.URL.Path, Headers: req.Header.Clone(), Raw: raw, Body: body, Keys: topLevelKeys(r.t, raw),
	})
	r.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	var response string
	switch {
	case strings.HasSuffix(req.URL.Path, "/responses") && bytes.Contains(raw, []byte("record_session_title")):
		response = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"record_session_title","arguments":"{\"title\":\"Inference seam\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	case strings.HasSuffix(req.URL.Path, "/responses") && bytes.Contains(raw, []byte("record_probe")):
		response = `{"status":"completed","output":[{"type":"function_call","name":"record_probe","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.HasSuffix(req.URL.Path, "/chat/completions") && bytes.Contains(raw, []byte("record_probe")):
		response = `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"record_probe","arguments":"{}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	case strings.HasSuffix(req.URL.Path, "/responses"):
		response = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	case strings.HasSuffix(req.URL.Path, "/messages") && bytes.Contains(raw, []byte("record_probe")):
		response = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"tool_1","name":"record_probe","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.HasSuffix(req.URL.Path, "/messages"):
		response = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.HasSuffix(req.URL.Path, "/chat/completions"):
		response = `{"choices":[{"message":{"content":"CAPTION: A chart.\n--- PAGE 1 ---\nhello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	case strings.HasSuffix(req.URL.Path, "/embeddings"):
		vector, _ := json.Marshal(make([]float32, r.dim))
		response = `{"data":[{"index":0,"embedding":` + string(vector) + `}]}`
	default:
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	_, _ = io.WriteString(w, response)
}

// last returns the single request recorded since the previous call.
func (r *inferenceRecorder) last() recordedRequest {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(r.t, r.calls, 1)
	call := r.calls[0]
	r.calls = nil
	return call
}

func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	_, err := decoder.Token()
	require.NoError(t, err)
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		require.NoError(t, err)
		keys = append(keys, key.(string))
		var skip json.RawMessage
		require.NoError(t, decoder.Decode(&skip))
	}
	return keys
}

// gatewayInferenceEnv is the environment inference reads in a deployment
// that runs every job; every model is a setting.
func gatewayInferenceEnv(t *testing.T) {
	t.Helper()
	serveEnv(t)
	for key, value := range map[string]string{
		"DIDEROT_ASK_ENABLED":           "true",
		"DIDEROT_ASK_READINESS_TIMEOUT": "20s",
		"DIDEROT_IMAGES_BUCKET":         "diderot-images-test",
	} {
		t.Setenv(key, value)
	}
}

// loadInference loads the configuration and a catalog whose enabled
// providers are providers.
func loadInference(t *testing.T, providers ...modelregistry.Provider) (*config.Config, *modelCatalog) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	catalog := newModelCatalog(cfg, nil)
	catalog.providers = func() []modelregistry.Provider { return providers }
	return cfg, catalog
}

// openAIProvider and anthropicProvider are providers of each kind at a
// recorder.
func openAIProvider(name string, recorder *inferenceRecorder, key string) modelregistry.Provider {
	return modelregistry.Provider{Name: name, Kind: modelregistry.KindOpenAI, Version: 1, Endpoint: llmclient.Endpoint{
		BaseURL: recorder.server.URL + "/v1", APIKey: key, Auth: llmclient.AuthBearer,
	}}
}

func anthropicProvider(name string, recorder *inferenceRecorder, key string) modelregistry.Provider {
	return modelregistry.Provider{Name: name, Kind: modelregistry.KindAnthropic, Version: 1, Endpoint: llmclient.Endpoint{
		BaseURL: recorder.server.URL + "/v1", APIKey: key, Auth: llmclient.AuthXAPIKey,
	}}
}

// exerciseInference sends one request down every inference path serve wires,
// through the same constructors, with the models saved chooses, and returns
// what each request's provider received. recorders maps each request name to
// the provider expected to receive it.
func exerciseInference(t *testing.T, cfg *config.Config, catalog *modelCatalog, saved settings.Models, recorders map[string]*inferenceRecorder) map[string]recordedRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got := map[string]recordedRequest{}
	models := catalog.resolve(saved)

	// The reasoning effort is each run's, as AskService sets it.
	mdl, err := ask.NewModel(ctx, ask.ModelConfig{
		BaseURL: models.ask.endpoint.BaseURL, APIKey: models.ask.endpoint.APIKey, Auth: models.ask.endpoint.Auth,
		Model: models.ask.Model, Timeout: models.askLimits.ModelTimeout, MaxTokens: models.askLimits.MaxOutputTokens, ServiceTier: models.askLimits.ServiceTier,
	})
	require.NoError(t, err)
	_, err = mdl.Generate(ask.WithReasoningEffort(ctx, saved.AskReasoningEffort), []*schema.AgenticMessage{ask.NewAgenticUserText("hello")})
	require.NoError(t, err)
	got["ask"] = recorders["ask"].last()

	check, err := gatewayReadinessCheck(ctx, cfg, models, testLogger())
	require.NoError(t, err)
	require.NoError(t, check.CheckOnce(ctx))
	got["readiness"] = recorders["ask"].last()

	title, err := newTitleGenerator(models.title, "")
	require.NoError(t, err)
	_, err = title.Generate(ctx, "Move inference behind a provider seam.")
	require.NoError(t, err)
	got["title"] = recorders["title"].last()

	summaries, _, err := newSessionSummaryClient(models)
	require.NoError(t, err)
	for name, model := range map[string]string{"session summary": models.summaryMap.ID, "session summary reduce": models.summaryReduce.ID} {
		_, _, err = summaries.CallTool(ctx, sessionsummary.ToolRequest{
			ModelID: model, MaxTokens: 16, System: "system", User: "hello",
			ToolName: "record_probe", ToolDescription: "Record.", ToolSchema: map[string]any{"type": "object"},
		})
		require.NoError(t, err)
		got[name] = recorders[name].last()
	}

	captioner, err := newCaptioner(models.caption)
	require.NoError(t, err)
	_, err = captioner.Caption(ctx, "image/png", []byte("png"))
	require.NoError(t, err)
	got["caption"] = recorders["caption"].last()

	extractor, err := newDocumentExtractor(models.docExtract, testLogger())
	require.NoError(t, err)
	_, err = extractor.Extract(ctx, "pdf", "a.pdf", []byte("%PDF-1.4"))
	require.NoError(t, err)
	got["document extraction"] = recorders["document extraction"].last()

	summarizer, err := newThreadSummarizer(models.summarizer)
	require.NoError(t, err)
	_, _, err = summarizer.Summarize(ctx, "alice: hi")
	require.NoError(t, err)
	got["thread summary"] = recorders["thread summary"].last()

	emb, err := newEmbedder(models.embed, models.embedding.Dimensions)
	require.NoError(t, err)
	_, err = emb.Embed(ctx, []string{"hello"})
	require.NoError(t, err)
	got["embeddings"] = recorders["embeddings"].last()
	return got
}

var inferenceRequests = []string{
	"ask", "title", "session summary", "session summary reduce", "caption",
	"document extraction", "thread summary", "embeddings",
}

// everyRequestTo routes every request name to one provider.
func everyRequestTo(recorder *inferenceRecorder) map[string]*inferenceRecorder {
	recorders := map[string]*inferenceRecorder{}
	for _, name := range inferenceRequests {
		recorders[name] = recorder
	}
	return recorders
}

// TestGatewayInferenceRequestShapes pins every job's request: with
// gatewayInferenceEnv and the gatewayMigration models row, the gateway
// provider receives each request with its method, path, credential header,
// model ID, and body fields.
func TestGatewayInferenceRequestShapes(t *testing.T) {
	gateway := newInferenceRecorder(t, 1024)
	gatewayInferenceEnv(t)
	cfg, catalog := loadInference(t, openAIProvider("gateway", gateway, "agw-token"))
	saved := gatewayMigration(gatewayRoles)
	views, err := catalog.Roles(t.Context(), saved)
	require.NoError(t, err)
	for _, role := range views {
		assert.Empty(t, role.Unavailable, role.Key)
		assert.Empty(t, role.Warning, role.Key)
		assert.Equal(t, "gateway/"+gatewayRoles[role.Key], role.Model, role.Key)
	}
	models := catalog.resolve(saved)
	assert.Equal(t, "local-embed", models.embed.ID)
	assert.Equal(t, settings.Embedding{Provider: "gateway", Model: "local-embed", Dimensions: 1024,
		Collection: "diderot_chunks_local_embed_v2"}, models.embedding)

	got := exerciseInference(t, cfg, catalog, saved, everyRequestTo(gateway))
	require.Len(t, got, len(inferenceRequests)+1)
	for name, request := range got {
		assert.Equal(t, http.MethodPost, request.Method, name)
		assert.Equal(t, "Bearer agw-token", request.Headers.Get("Authorization"), name)
		assert.Empty(t, request.Headers.Get("X-Api-Key"), name)
		assert.Equal(t, "application/json", request.Headers.Get("Content-Type"), name)
	}
	// The bounded JSON transport sends exactly these headers; the agentic
	// SDK clients add their own fixed set on top.
	for _, name := range []string{"title", "caption", "document extraction", "thread summary", "embeddings"} {
		assert.Equal(t, []string{"Accept-Encoding", "Authorization", "Content-Length", "Content-Type", "User-Agent"}, headerNames(got[name].Headers), name)
	}
	for _, name := range []string{"session summary", "session summary reduce"} {
		assert.Equal(t, []string{"Accept-Encoding", "Anthropic-Version", "Authorization", "Content-Length", "Content-Type", "User-Agent"}, headerNames(got[name].Headers), name)
	}

	for _, name := range []string{"ask", "readiness"} {
		request := got[name]
		assert.Equal(t, "/v1/responses", request.Path, name)
		assert.Equal(t, "gpt-6.1-sol:main", request.Body["model"], name)
		assert.Equal(t, "fast", request.Body["service_tier"], name)
		assert.NotContains(t, request.Body, "prompt_cache_retention", name)
		assert.Equal(t, []any{"reasoning.encrypted_content"}, request.Body["include"], name)
		assert.Equal(t, false, request.Body["store"], name)
	}
	assert.Equal(t, "medium", got["ask"].Body["reasoning"].(map[string]any)["effort"])
	assert.Equal(t, float64(32768), got["ask"].Body["max_output_tokens"])
	assert.Equal(t, "low", got["readiness"].Body["reasoning"].(map[string]any)["effort"])

	title := got["title"]
	assert.Equal(t, "/v1/responses", title.Path)
	assert.Equal(t, []string{"model", "instructions", "input", "max_output_tokens", "tools", "tool_choice", "parallel_tool_calls", "store", "reasoning"}, title.Keys)
	assert.Equal(t, "gpt-5.4-mini:main", title.Body["model"])
	assert.Equal(t, map[string]any{"effort": "low"}, title.Body["reasoning"])

	for name, model := range map[string]string{"session summary": "claude-haiku-4-5:main", "session summary reduce": "claude-sonnet-5:main"} {
		summary := got[name]
		assert.Equal(t, "/v1/messages", summary.Path, name)
		assert.Equal(t, "2023-06-01", summary.Headers.Get("Anthropic-Version"), name)
		assert.Equal(t, `{"max_tokens":16,"messages":[{"content":[{"text":"hello","type":"text"}],"role":"user"}],"model":"`+model+
			`","system":[{"text":"system","cache_control":{"type":"ephemeral"},"type":"text"}],"tool_choice":{"name":"record_probe","disable_parallel_tool_use":true,"type":"tool"},`+
			`"tools":[{"input_schema":{"type":"object"},"name":"record_probe","description":"Record."}]}`, string(summary.Raw), name)
	}

	caption := got["caption"]
	assert.Equal(t, "/v1/chat/completions", caption.Path)
	assert.Equal(t, []string{"model", "messages", "max_tokens", "temperature", "thinking_config"}, caption.Keys)
	assert.Equal(t, "gemini-3.8-flash", caption.Body["model"])
	assert.Equal(t, map[string]any{"thinkingBudget": float64(0)}, caption.Body["thinking_config"])

	document := got["document extraction"]
	assert.Equal(t, "/v1/chat/completions", document.Path)
	assert.Equal(t, []string{"model", "max_tokens", "messages"}, document.Keys)
	assert.Equal(t, "gpt-5.4:main", document.Body["model"])

	thread := got["thread summary"]
	assert.Equal(t, "/v1/chat/completions", thread.Path)
	assert.Equal(t, []string{"max_tokens", "messages", "model"}, thread.Keys)
	assert.Equal(t, "deepseek-v4.1-flash:main", thread.Body["model"])

	embeddings := got["embeddings"]
	assert.Equal(t, "/v1/embeddings", embeddings.Path)
	assert.Equal(t, `{"model":"local-embed","input":["hello"]}`, string(embeddings.Raw))
}

func headerNames(headers http.Header) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestMixedProviderInferenceRequestShapes covers a deployment with no
// gateway: OpenAI serves Responses, chat, and embeddings; the public
// Anthropic API serves Messages with x-api-key; a self-hosted proxy serves
// the reduce stage and thread summaries. Each request reaches only its
// model's provider, and no provider ever sees another's key.
func TestMixedProviderInferenceRequestShapes(t *testing.T) {
	openai := newInferenceRecorder(t, 512)
	anthropicAPI := newInferenceRecorder(t, 0)
	proxy := newInferenceRecorder(t, 0)
	keys := map[*inferenceRecorder]string{openai: "sk-openai", anthropicAPI: "sk-ant", proxy: "proxy-key"}
	serveEnv(t)
	for key, value := range map[string]string{
		"DIDEROT_ASK_ENABLED": "true", "DIDEROT_IMAGES_BUCKET": "images",
	} {
		t.Setenv(key, value)
	}
	cfg, catalog := loadInference(t,
		openAIProvider("openai", openai, keys[openai]),
		anthropicProvider("anthropic", anthropicAPI, keys[anthropicAPI]),
		openAIProvider("proxy", proxy, keys[proxy]))

	chat := []modelregistry.Protocol{modelregistry.ProtocolChat}
	messages := []modelregistry.Protocol{modelregistry.ProtocolMessages}
	saved := settings.Models{
		Roles: map[string]string{
			"ask": "anthropic/claude-sonnet-4-5", "titles": "openai/gpt-5-mini",
			"session_summary_map": "anthropic/claude-haiku-4-5", "session_summary_reduce": "proxy/proxy/claude-opus-4-1",
			"thread_summaries": "proxy/proxy/llama-3.3-70b", "image_captions": "openai/gpt-4.1-mini",
			"document_extraction": "openai/gpt-4.1-mini",
		},
		Available: []modelregistry.Model{
			{Provider: "openai", ID: "gpt-5-mini", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses}, ReasoningEfforts: []string{"minimal", "low", "medium", "high"}},
			{Provider: "openai", ID: "gpt-4.1-mini", Protocols: chat, Images: true},
			{Provider: "anthropic", ID: "claude-sonnet-4-5", Protocols: messages},
			{Provider: "anthropic", ID: "claude-haiku-4-5", Protocols: messages},
			{Provider: "proxy", ID: "proxy/claude-opus-4-1", Protocols: messages},
			{Provider: "proxy", ID: "proxy/llama-3.3-70b", Protocols: chat},
		},
		Embedding: &settings.Embedding{Provider: "openai", Model: "text-embedding-3-small", Dimensions: 512, Collection: "c"},
	}

	route := map[string]*inferenceRecorder{
		"ask": anthropicAPI, "title": openai, "session summary": anthropicAPI, "session summary reduce": proxy,
		"caption": openai, "document extraction": openai, "thread summary": proxy,
		"embeddings": openai,
	}
	got := exerciseInference(t, cfg, catalog, saved, route)
	route["readiness"] = anthropicAPI

	for name, request := range got {
		provider := route[name]
		for header, values := range request.Headers {
			for recorder, key := range keys {
				if recorder != provider {
					assert.NotContains(t, strings.Join(values, ","), key, "%s: %s carries another provider's key", name, header)
				}
			}
		}
		if provider == anthropicAPI {
			assert.Equal(t, keys[anthropicAPI], request.Headers.Get("X-Api-Key"), name)
			assert.Empty(t, request.Headers.Get("Authorization"), name)
		} else {
			assert.Equal(t, "Bearer "+keys[provider], request.Headers.Get("Authorization"), name)
			assert.Empty(t, request.Headers.Get("X-Api-Key"), name)
		}
	}

	for _, name := range []string{"ask", "readiness", "session summary", "session summary reduce"} {
		assert.Equal(t, "/v1/messages", got[name].Path, name)
		assert.Equal(t, "2023-06-01", got[name].Headers.Get("Anthropic-Version"), name)
	}
	// No efforts listed: the provider default, with no thinking fields.
	assert.Equal(t, "claude-sonnet-4-5", got["ask"].Body["model"])
	assert.NotContains(t, got["ask"].Body, "thinking")
	assert.NotContains(t, got["readiness"].Body, "thinking")
	assert.NotContains(t, got["ask"].Body, "output_config")
	assert.Equal(t, "claude-haiku-4-5", got["session summary"].Body["model"])
	assert.Equal(t, "proxy/claude-opus-4-1", got["session summary reduce"].Body["model"])

	assert.Equal(t, "/v1/responses", got["title"].Path)
	assert.Equal(t, "gpt-5-mini", got["title"].Body["model"])
	assert.Equal(t, map[string]any{"effort": "low"}, got["title"].Body["reasoning"])
	for name, model := range map[string]string{
		"caption": "gpt-4.1-mini", "document extraction": "gpt-4.1-mini",
		"thread summary": "proxy/llama-3.3-70b",
	} {
		assert.Equal(t, "/v1/chat/completions", got[name].Path, name)
		assert.Equal(t, model, got[name].Body["model"], name)
		assert.NotContains(t, got[name].Body, "thinking_config", name)
		assert.NotContains(t, got[name].Body, "reasoning_effort", name)
		assert.Empty(t, got[name].Headers.Get("Anthropic-Version"), name)
	}
	assert.Equal(t, "/v1/embeddings", got["embeddings"].Path)
	assert.Equal(t, []string{"model", "input"}, got["embeddings"].Keys)
}

// TestReasoningModelChatRequestShapes covers OpenAI's reasoning models on
// every chat role: their entries, as a job's test leaves them, set
// chat_token_param: max_completion_tokens, a reasoning token allowance, and
// chat_omit_temperature, so no chat request carries max_tokens or
// temperature, and each role's fixed limit grows by the allowance.
func TestReasoningModelChatRequestShapes(t *testing.T) {
	provider := newInferenceRecorder(t, 256)
	serveEnv(t)
	for key, value := range map[string]string{
		"DIDEROT_ASK_ENABLED": "true", "DIDEROT_IMAGES_BUCKET": "images",
	} {
		t.Setenv(key, value)
	}
	cfg, catalog := loadInference(t, openAIProvider("openai", provider, "sk-openai"))
	responsesAndChat := []modelregistry.Protocol{modelregistry.ProtocolResponses, modelregistry.ProtocolChat}
	got := exerciseInference(t, cfg, catalog, settings.Models{
		Roles: map[string]string{
			"ask": "openai/gpt-6.1-sol", "titles": "openai/gpt-6-luna", "session_summary_map": "openai/claude-haiku-4-5",
			"session_summary_reduce": "openai/claude-haiku-4-5", "thread_summaries": "openai/gpt-6-luna", "image_captions": "openai/gpt-6-luna",
			"document_extraction": "openai/gpt-6.1-sol",
		},
		Available: []modelregistry.Model{
			{Provider: "openai", ID: "gpt-6.1-sol", Protocols: responsesAndChat, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
				ChatTokenParam: llmclient.ChatMaxCompletionTokens, ChatReasoningTokenAllowance: 25000, ChatOmitTemperature: true, Images: true},
			{Provider: "openai", ID: "gpt-6-luna", Protocols: responsesAndChat, ReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"},
				ChatTokenParam: llmclient.ChatMaxCompletionTokens, ChatReasoningTokenAllowance: 10000, ChatOmitTemperature: true, Images: true},
			{Provider: "openai", ID: "claude-haiku-4-5", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages}},
		},
		Embedding: &settings.Embedding{Provider: "openai", Model: "text-embedding-3-small", Dimensions: 256, Collection: "c"},
	}, everyRequestTo(provider))

	for name, want := range map[string]struct {
		model string
		keys  []string
		limit float64
	}{
		"caption":             {"gpt-6-luna", []string{"model", "messages", "max_completion_tokens"}, 4096 + 10000},
		"document extraction": {"gpt-6.1-sol", []string{"model", "max_completion_tokens", "messages"}, 16000 + 25000},
		"thread summary":      {"gpt-6-luna", []string{"max_completion_tokens", "messages", "model"}, 2000 + 10000},
	} {
		request := got[name]
		assert.Equal(t, "/v1/chat/completions", request.Path, name)
		assert.Equal(t, want.model, request.Body["model"], name)
		assert.Equal(t, want.keys, request.Keys, name)
		assert.Equal(t, want.limit, request.Body["max_completion_tokens"], name)
	}
}

// TestSessionSummaryStagesFollowEachModelsProtocol: each summary stage calls
// its model on the protocol the model's entry gives the job, with that
// protocol's forced tool call, and a model that lists Messages is called on
// Messages whatever else it lists.
func TestSessionSummaryStagesFollowEachModelsProtocol(t *testing.T) {
	provider := newInferenceRecorder(t, 1)
	endpoint := llmclient.Endpoint{BaseURL: provider.server.URL + "/v1", APIKey: "k", Auth: llmclient.AuthBearer}
	bound := func(id string, entry modelregistry.Model) boundModel {
		entry.Provider, entry.ID = "p", id
		return boundModel{Model: entry, endpoint: endpoint}
	}
	all := []modelregistry.Protocol{modelregistry.ProtocolChat, modelregistry.ProtocolResponses, modelregistry.ProtocolMessages}
	for _, tc := range []struct {
		name     string
		model    boundModel
		protocol modelregistry.Protocol
		path     string
		keys     []string
	}{
		{"messages", bound("claude", modelregistry.Model{Protocols: all}), modelregistry.ProtocolMessages, "/v1/messages",
			[]string{"max_tokens", "messages", "model", "system", "tool_choice", "tools"}},
		{"responses", bound("gpt", modelregistry.Model{Protocols: all[:2], ReasoningEfforts: []string{"low", "high"}}), modelregistry.ProtocolResponses, "/v1/responses",
			[]string{"model", "instructions", "input", "max_output_tokens", "tools", "tool_choice", "parallel_tool_calls", "store", "reasoning"}},
		{"chat", bound("local", modelregistry.Model{
			Protocols: all[:1], ChatTokenParam: llmclient.ChatMaxCompletionTokens, ChatReasoningTokenAllowance: 100, ChatReasoningEffort: "minimal",
		}), modelregistry.ProtocolChat, "/v1/chat/completions",
			[]string{"model", "max_completion_tokens", "messages", "tools", "tool_choice", "reasoning_effort"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, models := range []*inferenceModels{{summaryMap: tc.model, summaryReduce: tc.model}, {summaryMap: tc.model}} {
				client, protocols, err := newSessionSummaryClient(models)
				require.NoError(t, err)
				assert.Equal(t, map[string]modelregistry.Protocol{tc.model.ID: tc.protocol}, protocols)
				_, _, err = client.CallTool(t.Context(), sessionsummary.ToolRequest{
					ModelID: tc.model.ID, MaxTokens: 16, System: "system", User: "hello",
					ToolName: "record_probe", ToolDescription: "Record.", ToolSchema: map[string]any{"type": "object"},
				})
				require.NoError(t, err)
				request := provider.last()
				assert.Equal(t, tc.path, request.Path)
				assert.Equal(t, tc.keys, request.Keys)
				assert.Equal(t, tc.model.ID, request.Body["model"])
				assert.Equal(t, "Bearer k", request.Headers.Get("Authorization"))
			}
		})
	}

	// The limit is the stage's, plus the entry's allowance on chat.
	chat := bound("local", modelregistry.Model{Protocols: all[:1], ChatReasoningTokenAllowance: 100})
	client, _, err := newSessionSummaryClient(&inferenceModels{summaryMap: chat})
	require.NoError(t, err)
	_, _, err = client.CallTool(t.Context(), sessionsummary.ToolRequest{ModelID: "local", MaxTokens: 16, ToolName: "record_probe"})
	require.NoError(t, err)
	assert.Equal(t, float64(116), provider.last().Body["max_tokens"])

	// A model that lists none of the protocols is not given a client.
	_, _, err = newSessionSummaryClient(&inferenceModels{summaryMap: bound("embed", modelregistry.Model{})})
	assert.ErrorContains(t, err, "lists no protocol")
	_, _, err = client.CallTool(t.Context(), sessionsummary.ToolRequest{ModelID: "another"})
	assert.ErrorContains(t, err, "not one of this leader term's")
}
