package sessiontitle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayGenerator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "gpt-5.4-mini", body["model"])
		assert.Equal(t, false, body["store"])
		assert.Contains(t, body["instructions"], "at most 72 characters")
		assert.Equal(t, float64(maxOutputTokens), body["max_output_tokens"])
		assert.Equal(t, map[string]any{"effort": "low"}, body["reasoning"])
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"function_call","name":"record_session_title","arguments":"{\"title\":\"  Fix Slack session links!  \"}"}]}`))
	}))
	defer server.Close()

	g, err := NewGatewayGenerator(GatewayConfig{
		BaseURL: server.URL + "/v1", APIKey: "test-key", ModelID: "gpt-5.4-mini", ReasoningEffort: ReasoningEffort,
	})
	require.NoError(t, err)
	result, err := g.Generate(t.Context(), strings.Repeat("x", maxInputBytes+100))
	require.NoError(t, err)
	assert.Equal(t, "Fix Slack session links", result.Title)
	assert.Equal(t, "gpt-5.4-mini", result.ModelID)
}

// A model whose registry entry lists no effort gets no reasoning field;
// non-reasoning Responses models reject it.
func TestGatewayGeneratorOmitsReasoningWithoutEffort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.NotContains(t, body, "reasoning")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"function_call","name":"record_session_title","arguments":"{\"title\":\"Plain model\"}"}]}`))
	}))
	defer server.Close()
	g, err := NewGatewayGenerator(GatewayConfig{BaseURL: server.URL + "/v1", APIKey: "test-key", ModelID: "gpt-4.1-mini"})
	require.NoError(t, err)
	result, err := g.Generate(t.Context(), "input")
	require.NoError(t, err)
	assert.Equal(t, "Plain model", result.Title)
}

func TestGatewayGeneratorAskPrompt(t *testing.T) {
	previous := promptcatalog.Active()
	require.NoError(t, promptcatalog.SetActive(promptcatalog.MustEmbedded()))
	t.Cleanup(func() { require.NoError(t, promptcatalog.SetActive(previous)) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body llmclient.ResponsesToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Contains(t, body.Instructions, "title for the question-and-answer exchange")
		assert.NotEqual(t, promptcatalog.ActiveText(promptcatalog.SessionTitle), body.Instructions)
		assert.NotContains(t, body.Instructions, "{{MAX_TITLE_RUNES}}")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"function_call","name":"record_session_title","arguments":"{\"title\":\"Investigating session title ownership and isolation\"}"}]}`))
	}))
	defer server.Close()
	generator, err := NewGatewayGenerator(GatewayConfig{
		BaseURL: server.URL, APIKey: "test-key", ModelID: "cheap", PromptKey: promptcatalog.AskTitle,
	})
	require.NoError(t, err)
	result, err := generator.Generate(t.Context(), "Question:\nWho owns this title?\n\nAnswer:\nDiderot does.")
	require.NoError(t, err)
	assert.Equal(t, "Investigating session title ownership and isolation", result.Title)
}

func TestGatewayGeneratorMissingPromptDoesNotCallGateway(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	generator, err := NewGatewayGenerator(GatewayConfig{
		BaseURL: server.URL, APIKey: "test-key", ModelID: "cheap", PromptKey: "missing.prompt",
	})
	require.NoError(t, err)
	_, err = generator.Generate(t.Context(), "Question and answer")
	require.ErrorContains(t, err, "prompt missing.prompt not found")
	assert.False(t, called)
}

func TestGatewayGeneratorRejectsUnsafeRedirect(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	g, err := NewGatewayGenerator(GatewayConfig{BaseURL: source.URL, APIKey: "secret", ModelID: "cheap"})
	require.NoError(t, err)
	_, err = g.Generate(t.Context(), "hello")
	require.Error(t, err)
	assert.False(t, targetCalled)
}

func TestFallbackExtractsQuestionFromWrappedPrompt(t *testing.T) {
	input := `<context>ASKER: Alice CURRENT TIME: now</context>
<thread_context>hundreds of background words</thread_context>
<question> @diderot find relevant threads related to this. </question>`
	assert.Equal(t, "find relevant threads related to this", Fallback(input))
	assert.Equal(t, "Untitled session", Fallback("<context>metadata only</context>"))
}

func TestCleanBoundsAndFlattensTitles(t *testing.T) {
	assert.Equal(t, "one title", Clean(" \"one\n title.\" "))
	assert.LessOrEqual(t, len([]rune(Clean(strings.Repeat("界", 100)))), maxTitleRunes)
}
