package modelregistry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// fakeProvider answers each request with what answer returns for its path
// and decoded body, and records every request.
type fakeProvider struct {
	mu       sync.Mutex
	requests []fakeRequest
	answer   func(path string, body map[string]any) int
	// reply is the body of a 200 answer; nil answers {"ok":true}.
	reply func(path string) string
}

// toolCallReply answers each protocol's forced tool call with the call.
func toolCallReply(path string) string {
	switch {
	case strings.HasSuffix(path, "/messages"):
		return `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"lookup","input":{"query":"OK"}}]}`
	case strings.HasSuffix(path, "/responses"):
		return `{"status":"completed","output":[{"type":"function_call","name":"lookup","arguments":"{\"query\":\"OK\"}"}]}`
	}
	return `{"choices":[{"finish_reason":"stop","message":{"tool_calls":[{"type":"function","function":{"name":"lookup","arguments":"{\"query\":\"OK\"}"}}]}}]}`
}

// proseReply answers each protocol in text, without the tool call.
func proseReply(path string) string {
	switch {
	case strings.HasSuffix(path, "/messages"):
		return `{"stop_reason":"end_turn","content":[{"type":"text","text":"OK"}]}`
	case strings.HasSuffix(path, "/responses"):
		return `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`
	}
	return `{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}]}`
}

type fakeRequest struct {
	Path string
	Body map[string]any
}

func (f *fakeProvider) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{Path: r.URL.Path, Body: body})
		f.mu.Unlock()
		status := f.answer(r.URL.Path, body)
		if status != http.StatusOK {
			http.Error(w, `{"error":{"message":"upstream says no"}}`, status)
			return
		}
		if f.reply != nil {
			_, _ = w.Write([]byte(f.reply(r.URL.Path)))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *fakeProvider) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var paths []string
	for _, request := range f.requests {
		paths = append(paths, request.Path)
	}
	return paths
}

func (f *fakeProvider) last() fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func testProvider(kind Kind, server *httptest.Server, base string) Provider {
	return Provider{Name: "p", Kind: kind, Endpoint: llmclient.Endpoint{BaseURL: server.URL + base, APIKey: "k", Auth: kind.Auth()}}
}

func TestCaptionsTestFindsTheChatShapeAReasoningModelTakes(t *testing.T) {
	// An OpenAI reasoning model: max_tokens and a zero temperature are both
	// refused with 400.
	fake := &fakeProvider{answer: func(_ string, body map[string]any) int {
		if _, ok := body["max_tokens"]; ok {
			return http.StatusBadRequest
		}
		if _, ok := body["temperature"]; ok {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	// Titles' test made the entry; captions is its second job.
	model := testMade(Model{Provider: "p", ID: "gpt-reasoner", Protocols: []Protocol{ProtocolResponses}, ReasoningEfforts: []string{"low"}})
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), model, RoleImageCaptions, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/chat/completions", "/v1/chat/completions", "/v1/chat/completions", "/v1/chat/completions"}, fake.paths())
	assert.Equal(t, llmclient.ChatMaxCompletionTokens, learned.ChatTokenParam)
	assert.True(t, learned.ChatOmitTemperature)
	assert.Equal(t, DefaultReasoningTokenAllowance, learned.ChatReasoningTokenAllowance)
	assert.True(t, learned.Images)
	assert.Equal(t, []Protocol{ProtocolResponses, ProtocolChat}, learned.Protocols, "nothing another job found is removed")
	assert.Equal(t, []string{"low"}, learned.ReasoningEfforts)
	assert.Equal(t, []Role{RoleImageCaptions}, learned.TestedFor)
	assert.True(t, learned.ServesRole(RoleImageCaptions))

	// The image rode the request.
	content := fake.last().Body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	assert.Equal(t, "image_url", content[1].(map[string]any)["type"])
	assert.True(t, strings.HasPrefix(content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,"))
}

func TestChatTestKeepsTheDefaultShapeAndExtraBody(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	model := Model{Provider: "p", ID: "gemini", ChatExtraBody: map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}}}
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), model, RoleThreadSummaries, TestOptions{})
	require.NoError(t, err)
	assert.Len(t, fake.paths(), 1)
	body := fake.last().Body
	assert.EqualValues(t, 16, body["max_tokens"])
	assert.EqualValues(t, 0, body["temperature"])
	assert.Equal(t, map[string]any{"thinkingBudget": float64(0)}, body["thinking_config"])
	assert.Empty(t, learned.ChatTokenParam)
	assert.False(t, learned.ChatOmitTemperature)
	assert.Zero(t, learned.ChatReasoningTokenAllowance)
	assert.False(t, learned.Images, "a text-only job learns nothing about images")
}

func TestATestStopsAtAFailureThatIsNotARefusedShape(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		fake := &fakeProvider{answer: func(string, map[string]any) int { return status }}
		server := fake.serve(t)
		model := Model{Provider: "p", ID: "m"}
		learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), model, RoleImageCaptions, TestOptions{})
		require.Error(t, err)
		assert.Len(t, fake.paths(), 1, "HTTP %d is not a refused shape", status)
		assert.Equal(t, model, learned, "a failed test learns nothing")
		assert.Equal(t, statusReason(status), err.Error())
		assert.NotContains(t, err.Error(), "upstream says no")
	}
}

func TestCaptionsOnAModelThatRefusesImages(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusBadRequest }}
	server := fake.serve(t)
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "m"}, RoleImageCaptions, TestOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not accept images")
	assert.Len(t, fake.paths(), 4)
}

func TestAskTestFallsBackFromResponsesToMessagesOnAGateway(t *testing.T) {
	fake := &fakeProvider{answer: func(path string, _ map[string]any) int {
		if path == "/v1/responses" {
			return http.StatusNotFound
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "claude:main"}, RoleAsk,
		TestOptions{Effort: "high"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/responses", "/v1/messages"}, fake.paths())
	body := fake.last().Body
	assert.Equal(t, map[string]any{"type": "adaptive"}, body["thinking"])
	assert.Equal(t, map[string]any{"effort": "high"}, body["output_config"])
	assert.NotNil(t, body["tools"])
	assert.Nil(t, body["tool_choice"], "Ask offers tools without forcing one")
	assert.Equal(t, []Protocol{ProtocolMessages}, learned.Protocols)
	assert.Equal(t, []string{"high"}, learned.ReasoningEfforts)
	assert.True(t, learned.ServesRole(RoleAsk))
}

func TestAskTestOnResponsesRecordsTheEffortAndTier(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "gpt"}, RoleAsk,
		TestOptions{Effort: "medium", ServiceTier: "priority"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/responses"}, fake.paths())
	body := fake.last().Body
	assert.Equal(t, map[string]any{"effort": "medium"}, body["reasoning"])
	assert.Equal(t, "priority", body["service_tier"])
	assert.Equal(t, []Protocol{ProtocolResponses}, learned.Protocols)
	assert.True(t, learned.ServiceTier)
	assert.Equal(t, []string{"medium"}, learned.ReasoningEfforts)
}

func TestAskTestStopsOnARefusedKey(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusUnauthorized }}
	server := fake.serve(t)
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "gpt"}, RoleAsk, TestOptions{})
	require.Error(t, err)
	assert.Equal(t, []string{"/v1/responses"}, fake.paths(), "a refused key is not a missing protocol")
}

func TestAnthropicProviderAnswersOnMessagesAlone(t *testing.T) {
	var key string
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("X-Api-Key")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, fakeRequest{Path: r.URL.Path, Body: body})
		fake.mu.Unlock()
		_, _ = w.Write([]byte(toolCallReply(r.URL.Path)))
	}))
	defer server.Close()
	p := testProvider(KindAnthropic, server, "")
	learned, err := TestForRole(context.Background(), p, Model{Provider: "p", ID: "claude-haiku"}, RoleSessionSummaryMap, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, "k", key)
	assert.Equal(t, []string{"/v1/messages"}, fake.paths())
	assert.Equal(t, map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true}, fake.last().Body["tool_choice"],
		"summaries force their tool call, which some models refuse")
	assert.Equal(t, []Protocol{ProtocolMessages}, learned.Protocols)

	learned, err = TestForRole(context.Background(), p, learned, RoleAsk, TestOptions{Effort: "none"})
	require.NoError(t, err)
	assert.Equal(t, "/v1/messages", fake.last().Path)
	assert.Equal(t, map[string]any{"type": "disabled"}, fake.last().Body["thinking"])
	assert.Equal(t, []Role{RoleSessionSummaryMap, RoleAsk}, learned.TestedFor)
}

func TestTitlesTestDropsTheEffortTheModelRefuses(t *testing.T) {
	fake := &fakeProvider{answer: func(_ string, body map[string]any) int {
		if body["reasoning"] != nil {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "mini"}, RoleTitles, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/responses", "/v1/responses"}, fake.paths())
	assert.Equal(t, map[string]any{"type": "function", "name": "lookup"}, fake.last().Body["tool_choice"])
	assert.Empty(t, learned.ReasoningEfforts)
	assert.True(t, learned.ServesRole(RoleTitles))
}

func TestATestNeedsAKey(t *testing.T) {
	_, err := TestForRole(context.Background(), Provider{Name: "p", Kind: KindOpenAI, Endpoint: llmclient.Endpoint{BaseURL: "https://x.example/v1"}},
		Model{ID: "m"}, RoleAsk, TestOptions{})
	assert.EqualError(t, err, "the provider has no key")
}

// TestAServerThatTakesNoKeyIsSentNone: a provider marked as taking no key
// is tested, and listed, with no credential header, and with the provider's
// extra headers.
func TestAServerThatTakesNoKeyIsSentNone(t *testing.T) {
	var seen []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		_, _ = w.Write([]byte(`{"data":[{"id":"local"}]}`))
	}))
	defer server.Close()
	p := Provider{Name: "p", Kind: KindOpenAI, Keyless: true, Endpoint: llmclient.Endpoint{
		BaseURL: server.URL + "/v1", Auth: KindOpenAI.Auth(), Headers: http.Header{"X-Team": {"search"}},
	}}
	require.Empty(t, p.Unusable())
	_, err := TestForRole(context.Background(), p, Model{Provider: "p", ID: "local"}, RoleThreadSummaries, TestOptions{})
	require.NoError(t, err)
	_, err = List(context.Background(), server.Client(), p)
	require.NoError(t, err)
	_, err = EmbeddingDimensions(context.Background(), p, "local", nil)
	require.Error(t, err, "the fake answers with no embedding")
	require.Len(t, seen, 3)
	for _, header := range seen {
		assert.Empty(t, header.Get("Authorization"))
		assert.Empty(t, header.Get("X-Api-Key"))
		assert.Equal(t, "search", header.Get("X-Team"))
	}
	// Without the mark, a missing key is a fault, not a request without
	// one; and Anthropic's API always takes a key.
	p.Keyless = false
	assert.Equal(t, "has no key", p.Unusable())
	assert.False(t, KindAnthropic.AllowsNoKey())
}

// servesOnly answers 200 on the paths ending in served, and 404 elsewhere,
// as a provider that does not serve a protocol does.
func servesOnly(served ...string) func(string, map[string]any) int {
	return func(path string, _ map[string]any) int {
		for _, suffix := range served {
			if strings.HasSuffix(path, suffix) {
				return http.StatusOK
			}
		}
		return http.StatusNotFound
	}
}

var summaryRoles = []Role{RoleSessionSummaryMap, RoleSessionSummaryReduce}

// A model that lists Messages is sent the Messages request and nothing
// else: a deployment that summarizes on Messages keeps doing so.
func TestSummaryTestOfAMessagesModelSendsMessagesAlone(t *testing.T) {
	for _, role := range summaryRoles {
		fake := &fakeProvider{answer: servesOnly("/messages", "/responses", "/chat/completions"), reply: toolCallReply}
		server := fake.serve(t)
		m := Model{Provider: "p", ID: "claude", Protocols: []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages}}
		learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), m, role, TestOptions{})
		require.NoError(t, err)
		assert.Equal(t, []string{"/v1/messages"}, fake.paths())
		assert.Equal(t, m.Protocols, learned.Protocols)
		assert.Contains(t, learned.TestedFor, role)
	}
}

// A model on a provider that does not serve Messages is summarized on
// Responses: the test sends the forced tool call there, and lists it.
func TestSummaryTestFindsResponsesWhereMessagesIsNotServed(t *testing.T) {
	fake := &fakeProvider{answer: servesOnly("/responses", "/chat/completions"), reply: toolCallReply}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"),
		NewEntry("p", "gpt"), RoleSessionSummaryMap, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/messages", "/v1/responses"}, fake.paths())
	last := fake.last().Body
	assert.Equal(t, map[string]any{"type": "function", "name": "lookup"}, last["tool_choice"])
	assert.Len(t, last["tools"], 1)
	assert.Equal(t, map[string]any{"effort": "low"}, last["reasoning"], "the effort summaries send when the entry lists it")
	assert.Equal(t, []Protocol{ProtocolResponses}, learned.Protocols)
	assert.Equal(t, []string{"low"}, learned.ReasoningEfforts)
	protocol, ok := learned.RoleProtocol(RoleSessionSummaryMap)
	assert.True(t, ok)
	assert.Equal(t, ProtocolResponses, protocol)
}

// A model on a server that speaks Chat Completions alone is summarized on
// chat: the test sends the forced tool call in the chat shape the model
// takes.
func TestSummaryTestFindsChatWhereNothingElseIsServed(t *testing.T) {
	fake := &fakeProvider{reply: toolCallReply, answer: func(path string, body map[string]any) int {
		if !strings.HasSuffix(path, "/chat/completions") {
			return http.StatusNotFound
		}
		if _, old := body["max_tokens"]; old {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"),
		NewEntry("p", "local"), RoleSessionSummaryReduce, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/messages", "/v1/responses", "/v1/chat/completions", "/v1/chat/completions"}, fake.paths())
	last := fake.last().Body
	assert.Equal(t, map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, last["tool_choice"])
	tools := last["tools"].([]any)
	require.Len(t, tools, 1)
	assert.Equal(t, "lookup", tools[0].(map[string]any)["function"].(map[string]any)["name"])
	assert.Equal(t, []Protocol{ProtocolChat}, learned.Protocols)
	assert.Equal(t, llmclient.ChatMaxCompletionTokens, learned.ChatTokenParam)
	assert.True(t, learned.ServesRole(RoleSessionSummaryMap))
	assert.True(t, learned.ServesRole(RoleThreadSummaries), "what the test found about chat serves the other chat jobs")
}

// The entry decides the protocol, and the test sends what the job will: a
// model another job listed on chat is tested, and summarized, on chat, even
// where its provider would serve it on Messages too.
func TestSummaryTestSendsTheProtocolTheEntrySelects(t *testing.T) {
	fake := &fakeProvider{answer: servesOnly("/messages", "/responses", "/chat/completions"), reply: toolCallReply}
	server := fake.serve(t)
	m := Model{Provider: "p", ID: "local", Protocols: []Protocol{ProtocolChat}, ChatTokenParam: llmclient.ChatMaxCompletionTokens}
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), m, RoleSessionSummaryMap, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/chat/completions"}, fake.paths())
	assert.Equal(t, []Protocol{ProtocolChat}, learned.Protocols)
	body := fake.last().Body
	assert.Contains(t, body, "max_completion_tokens")
	assert.NotContains(t, body, "temperature", "summaries send no temperature, so their test sends none")
}

// A refusal on the protocol the entry selects fails the test, and no other
// protocol is tried in its place.
func TestSummaryTestDoesNotFallBackFromTheSelectedProtocol(t *testing.T) {
	fake := &fakeProvider{answer: servesOnly("/chat/completions"), reply: toolCallReply}
	server := fake.serve(t)
	m := Model{Provider: "p", ID: "gpt", Protocols: []Protocol{ProtocolResponses, ProtocolChat}}
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), m, RoleSessionSummaryMap, TestOptions{})
	require.Error(t, err)
	assert.Equal(t, "HTTP 404 Not Found", err.Error())
	assert.Equal(t, []string{"/v1/responses"}, fake.paths())
}

// While no protocol is known, any failure that is not a refusal or a
// missing tool call ends the test: a rejected key is not a reason to try
// the next protocol.
func TestSummaryTestStopsAtAFailureThatIsNotARefusal(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusUnauthorized }}
	server := fake.serve(t)
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), NewEntry("p", "m"), RoleSessionSummaryMap, TestOptions{})
	require.Error(t, err)
	assert.Equal(t, "HTTP 401 Unauthorized", err.Error())
	assert.Equal(t, []string{"/v1/messages"}, fake.paths())
}

// A model found on chat by a summary test is asked for no temperature, and
// the test says nothing about one.
func TestSummaryTestOnChatFindsOnlyTheOutputLimitField(t *testing.T) {
	fake := &fakeProvider{reply: toolCallReply, answer: func(path string, body map[string]any) int {
		_, old := body["max_tokens"]
		if !strings.HasSuffix(path, "/chat/completions") || old {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	var trace TestTrace
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"),
		NewEntry("p", "local"), RoleSessionSummaryMap, TestOptions{Trace: &trace})
	require.NoError(t, err)
	fake.mu.Lock()
	for _, request := range fake.requests {
		assert.NotContains(t, request.Body, "temperature", request.Path)
	}
	fake.mu.Unlock()
	assert.Equal(t, []string{"/v1/messages", "/v1/responses", "/v1/responses", "/v1/chat/completions", "/v1/chat/completions"}, fake.paths())
	assert.False(t, learned.ChatOmitTemperature)
	assert.Equal(t, llmclient.ChatMaxCompletionTokens, learned.ChatTokenParam)
	assert.NotContains(t, trace.Learned, FactTemperature)
	assert.Contains(t, trace.Learned, FactTokenParam)
}

// A model that takes the request and answers without the tool call would
// fail every summary, so it fails the test, on each protocol.
func TestSummaryTestNeedsTheToolCallInTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		protocol Protocol
		path     string
	}{{ProtocolMessages, "/messages"}, {ProtocolResponses, "/responses"}, {ProtocolChat, "/chat/completions"}} {
		fake := &fakeProvider{answer: servesOnly(tc.path), reply: proseReply}
		server := fake.serve(t)
		m := Model{Provider: "p", ID: "m", Protocols: []Protocol{tc.protocol}}
		_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), m, RoleSessionSummaryMap, TestOptions{})
		require.Error(t, err, tc.protocol)
		assert.Contains(t, err.Error(), "without calling the tool", tc.protocol)

	}
}

// While no protocol is known, a protocol on which the model answers without
// the call is passed over for the next, and a model that makes the call on
// none fails the test.
func TestSummaryTestMovesOnFromAProtocolThatDoesNotCallTheTool(t *testing.T) {
	fake := &fakeProvider{answer: servesOnly("/messages", "/responses", "/chat/completions"), reply: func(path string) string {
		if strings.HasSuffix(path, "/chat/completions") {
			return toolCallReply(path)
		}
		return proseReply(path)
	}}
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, fake.serve(t), "/v1"), NewEntry("p", "m"), RoleSessionSummaryMap, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []Protocol{ProtocolChat}, learned.Protocols)

	prose := &fakeProvider{answer: servesOnly("/messages", "/responses", "/chat/completions"), reply: proseReply}
	_, err = TestForRole(context.Background(), testProvider(KindOpenAI, prose.serve(t), "/v1"), NewEntry("p", "m"), RoleSessionSummaryMap, TestOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without calling the tool")
}

// Titles are tested as before: the forced call is sent, and the answer is
// not read.
func TestTitlesTestDoesNotReadTheAnswer(t *testing.T) {
	fake := &fakeProvider{answer: servesOnly("/responses"), reply: proseReply}
	server := fake.serve(t)
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), NewEntry("p", "mini"), RoleTitles, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, float64(testTokens), fake.last().Body["max_output_tokens"])
}
