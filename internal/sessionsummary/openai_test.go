package sessionsummary

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// fakeProvider answers every request with status and body, and keeps the
// last request it was sent.
type fakeProvider struct {
	status int
	body   string
	path   string
	header http.Header
	raw    []byte
}

func (f *fakeProvider) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		f.path, f.header, f.raw = r.URL.Path, r.Header.Clone(), raw
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1"
}

// protocolClients builds the client of each protocol against one fake
// provider, and says how that protocol's answer carries a tool call.
var protocolClients = []struct {
	name string
	// client builds the protocol's client.
	client func(t *testing.T, baseURL string) ToolClient
	// called is an answer that calls tool with arguments.
	called func(tool, arguments string) string
	// truncated is an answer cut off at the output limit, and prose one
	// that answers in text without calling the tool.
	truncated, prose string
}{
	{
		name: "messages",
		client: func(t *testing.T, baseURL string) ToolClient {
			client, err := NewGatewayClient(GatewayClientConfig{BaseURL: baseURL, APIKey: "key"})
			require.NoError(t, err)
			return client
		},
		called: func(tool, arguments string) string {
			return `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"` + tool + `","input":` + arguments + `}],"usage":{"input_tokens":7,"cache_read_input_tokens":3,"output_tokens":5}}`
		},
		truncated: `{"stop_reason":"max_tokens","content":[],"usage":{"input_tokens":10,"output_tokens":5}}`,
		prose:     `{"stop_reason":"end_turn","content":[{"type":"text","text":"{\"evidence\":[\"a\"],\"summary\":\"in prose\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}`,
	},
	{
		name: "responses",
		client: func(t *testing.T, baseURL string) ToolClient {
			client, err := NewResponsesClient(ResponsesClientConfig{BaseURL: baseURL, APIKey: "key"})
			require.NoError(t, err)
			return client
		},
		called: func(tool, arguments string) string {
			quoted, _ := json.Marshal(arguments)
			return `{"status":"completed","output":[{"type":"reasoning"},{"type":"function_call","name":"` + tool + `","arguments":` + string(quoted) + `}],"usage":{"input_tokens":10,"output_tokens":5}}`
		},
		truncated: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":5}}`,
		prose:     `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"evidence\":[\"a\"],\"summary\":\"in prose\"}"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`,
	},
	{
		name: "chat",
		client: func(t *testing.T, baseURL string) ToolClient {
			client, err := NewChatClient(ChatClientConfig{BaseURL: baseURL, APIKey: "key"})
			require.NoError(t, err)
			return client
		},
		called: func(tool, arguments string) string {
			quoted, _ := json.Marshal(arguments)
			return `{"choices":[{"finish_reason":"stop","message":{"tool_calls":[{"type":"function","function":{"name":"` + tool + `","arguments":` + string(quoted) + `}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
		},
		truncated: `{"choices":[{"finish_reason":"length","message":{"content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		prose:     `{"choices":[{"finish_reason":"stop","message":{"content":"{\"evidence\":[\"a\"],\"summary\":\"in prose\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
	},
}

var testCall = CallOptions{ModelID: "model-a", MaxTokens: 1024}

// The map and reduce stages return the same summary from the same tool
// arguments whichever protocol carried them, and count its tokens.
func TestEveryProtocolDecodesTheSameSummary(t *testing.T) {
	for _, protocol := range protocolClients {
		t.Run(protocol.name, func(t *testing.T) {
			provider := &fakeProvider{body: protocol.called("record_chunk_summary", `{"evidence":["a","b","c"],"summary":"mapped\u0000"}`)}
			client := protocol.client(t, provider.serve(t))
			mapped, usage, err := MapCall(t.Context(), client, EmbeddedPromptBundle, testCall, []byte(`{}`), 1, 1)
			require.NoError(t, err)
			assert.Equal(t, ChunkSummary{Evidence: StringList{"a", "b", "c"}, Summary: "mapped"}, mapped, "a NUL byte is removed on every protocol")
			assert.Equal(t, Usage{InputTokens: 10, OutputTokens: 5}, usage)

			provider.body = protocol.called("record_session_summary",
				`{"evidence":"one","narrative":"the story","embedding_text":"keywords","context_hint":"hint","tags":{"languages":["go"]}}`)
			final, _, err := ReduceCall(t.Context(), client, EmbeddedPromptBundle, testCall, []ChunkSummary{mapped})
			require.NoError(t, err)
			assert.Equal(t, FinalSummary{
				Evidence: StringList{"one"}, Narrative: "the story", EmbeddingText: "keywords", ContextHint: "hint",
				Tags: TagSet{Languages: []string{"go"}},
			}, final)
		})
	}
}

// An answer that does not carry the forced tool call with arguments the
// stage accepts fails the summary on every protocol; prose is never read.
func TestEveryProtocolRejectsAnAnswerThatDoesNotValidate(t *testing.T) {
	for _, protocol := range protocolClients {
		t.Run(protocol.name, func(t *testing.T) {
			for name, tc := range map[string]struct{ body, want string }{
				"truncated":            {protocol.truncated, "response truncated at max"},
				"prose":                {protocol.prose, ""},
				"another tool":         {protocol.called("something_else", `{}`), `want "record_session_summary"`},
				"arguments not object": {protocol.called("record_session_summary", `["a"]`), "JSON object"},
				"arguments not JSON":   {strings.Replace(protocol.called("record_session_summary", `{}`), `{}`, `{`, 1), ""},
				"empty narrative":      {protocol.called("record_session_summary", `{"evidence":["a"],"narrative":" "}`), "empty summary"},
				"wrong field type":     {protocol.called("record_session_summary", `{"narrative":["not","text"]}`), "decode final summary"},
				"not JSON at all":      {`<html>bad gateway</html>`, ""},
			} {
				provider := &fakeProvider{body: tc.body}
				client := protocol.client(t, provider.serve(t))
				_, _, err := ReduceCall(t.Context(), client, EmbeddedPromptBundle, testCall, []ChunkSummary{{Summary: "chunk"}})
				require.Error(t, err, name)
				assert.ErrorContains(t, err, tc.want, name)
			}
		})
	}
}

// A provider's refusal reaches the worker as the same error on every
// protocol: the HTTP status, which the worker counts as a failed attempt and
// tries again.
func TestEveryProtocolReportsAProviderErrorAlike(t *testing.T) {
	for _, protocol := range protocolClients {
		t.Run(protocol.name, func(t *testing.T) {
			for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
				provider := &fakeProvider{status: status, body: `{"error":{"message":"no"}}`}
				client := protocol.client(t, provider.serve(t))
				_, _, err := MapCall(t.Context(), client, EmbeddedPromptBundle, testCall, []byte(`{}`), 1, 1)
				var httpErr *llmclient.HTTPError
				require.True(t, errors.As(err, &httpErr), "%d: %v", status, err)
				assert.Equal(t, status, httpErr.StatusCode)
			}
		})
	}
}

func TestResponsesRequestForcesTheTool(t *testing.T) {
	provider := &fakeProvider{body: protocolClients[1].called("record_chunk_summary", `{"evidence":["a"],"summary":"s"}`)}
	client, err := NewResponsesClient(ResponsesClientConfig{
		BaseURL: provider.serve(t), APIKey: "key", Headers: http.Header{"X-Extra": {"one"}}, ReasoningEffort: ResponsesReasoningEffort,
	})
	require.NoError(t, err)
	bundle := PromptBundle{MapSystem: "map instructions"}
	_, _, err = MapCall(t.Context(), client, bundle, testCall, []byte(`{"n":1}`), 2, 3)
	require.NoError(t, err)

	assert.Equal(t, "/v1/responses", provider.path)
	assert.Equal(t, "Bearer key", provider.header.Get("Authorization"))
	assert.Equal(t, "one", provider.header.Get("X-Extra"))
	assert.Empty(t, provider.header.Get("Anthropic-Version"))
	schema, err := json.Marshal(chunkSummaryToolSchema())
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"model":"model-a","instructions":"map instructions",
		"input":"<chunk index=\"2\" of=\"3\">\n{\"n\":1}\n</chunk>",
		"max_output_tokens":1024,
		"tools":[{"type":"function","name":"record_chunk_summary","description":"Record the chunk summary. Call this exactly once, then stop.","parameters":`+string(schema)+`}],
		"tool_choice":{"type":"function","name":"record_chunk_summary"},
		"parallel_tool_calls":false,"store":false,"reasoning":{"effort":"low"}
	}`, string(provider.raw))

	plain, err := NewResponsesClient(ResponsesClientConfig{BaseURL: provider.serve(t), APIKey: "key"})
	require.NoError(t, err)
	_, _, err = MapCall(t.Context(), plain, bundle, testCall, []byte(`{}`), 1, 1)
	require.NoError(t, err)
	assert.NotContains(t, string(provider.raw), "reasoning", "a model whose entry lists no effort is sent none")
}

func TestChatRequestForcesTheTool(t *testing.T) {
	provider := &fakeProvider{body: protocolClients[2].called("record_session_summary", `{"evidence":["a"],"narrative":"n"}`)}
	client, err := NewChatClient(ChatClientConfig{
		BaseURL: provider.serve(t), APIKey: "key", Headers: http.Header{"X-Extra": {"one"}},
		TokenParam: llmclient.ChatMaxCompletionTokens, ReasoningTokenAllowance: 2000,
		ExtraBody: map[string]any{"reasoning_effort": "minimal"},
	})
	require.NoError(t, err)
	bundle := PromptBundle{ReduceSystem: "reduce instructions", LanguageEnum: []string{"go"}, ActivityEnum: []string{"debug"}}
	_, _, err = ReduceCall(t.Context(), client, bundle, CallOptions{ModelID: "model-b", MaxTokens: 4096}, []ChunkSummary{{Summary: "chunk"}})
	require.NoError(t, err)

	assert.Equal(t, "/v1/chat/completions", provider.path)
	assert.Equal(t, "Bearer key", provider.header.Get("Authorization"))
	assert.Equal(t, "one", provider.header.Get("X-Extra"))
	schema, err := json.Marshal(sessionSummaryToolSchema(bundle))
	require.NoError(t, err)
	user, err := renderReduceInput([]ChunkSummary{{Summary: "chunk"}})
	require.NoError(t, err)
	quotedUser, err := json.Marshal(user)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"model":"model-b","max_completion_tokens":6096,
		"messages":[{"role":"system","content":"reduce instructions"},{"role":"user","content":`+string(quotedUser)+`}],
		"tools":[{"type":"function","function":{"name":"record_session_summary","description":"Record the final session summary. Call this exactly once, then stop.","parameters":`+string(schema)+`}}],
		"tool_choice":{"type":"function","function":{"name":"record_session_summary"}},
		"reasoning_effort":"minimal"
	}`, string(provider.raw))

	plain, err := NewChatClient(ChatClientConfig{BaseURL: provider.serve(t)})
	require.NoError(t, err)
	_, _, err = ReduceCall(t.Context(), plain, bundle, CallOptions{ModelID: "model-b", MaxTokens: 4096}, []ChunkSummary{{Summary: "chunk"}})
	require.NoError(t, err)
	assert.Contains(t, string(provider.raw), `"max_tokens":4096`, "the default output-limit field")
	assert.Empty(t, provider.header.Get("Authorization"), "a server that takes no key is sent none")
}

// A chat answer with two calls is not read: which one is the summary would
// be a guess.
func TestChatRejectsMoreThanOneToolCall(t *testing.T) {
	call := `{"type":"function","function":{"name":"record_chunk_summary","arguments":"{\"evidence\":[\"a\"],\"summary\":\"s\"}"}}`
	provider := &fakeProvider{body: `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[` + call + `,` + call + `]}}]}`}
	client, err := NewChatClient(ChatClientConfig{BaseURL: provider.serve(t), APIKey: "key"})
	require.NoError(t, err)
	_, _, err = MapCall(t.Context(), client, EmbeddedPromptBundle, testCall, []byte(`{}`), 1, 1)
	assert.ErrorContains(t, err, "multiple tool calls")
}

// An entry's extra chat field that repeats one the request sets is refused
// when the client is built, so the job is never given the model.
func TestChatClientRefusesACollidingExtraField(t *testing.T) {
	_, err := NewChatClient(ChatClientConfig{BaseURL: "https://provider.invalid/v1", ExtraBody: map[string]any{"tool_choice": "auto"}})
	assert.ErrorContains(t, err, `"tool_choice"`)
}
