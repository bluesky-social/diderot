package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newHandler(16, log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %s: %s", url, resp.Status, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func TestEmbedIsDeterministicNormalizedAndRanksByOverlap(t *testing.T) {
	query := embed("What is the quickstart smoke codeword?", 1024)
	if got := embed("What is the quickstart smoke codeword?", 1024); cosine(query, got) < 0.9999 {
		t.Fatal("embedding is not deterministic")
	}
	if norm := math.Sqrt(cosine(query, query)); math.Abs(norm-1) > 1e-5 {
		t.Fatalf("norm = %v, want 1", norm)
	}
	match := embed("The quickstart smoke codeword is periwinkle-kestrel-7781.", 1024)
	other := embed("Deploys run migrations before the new pods take traffic.", 1024)
	if cosine(query, match) <= cosine(query, other) {
		t.Fatalf("overlapping text should rank first: match %v, other %v", cosine(query, match), cosine(query, other))
	}
	if empty := embed("", 8); empty[0] != 1 {
		t.Fatalf("empty text must not embed to the zero vector: %v", empty)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	server := newTestServer(t)
	out := post(t, server.URL+"/v1/embeddings", map[string]any{"model": "m", "input": []string{"a b", "c"}})
	data := out["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("got %d embeddings, want 2", len(data))
	}
	for i, item := range data {
		entry := item.(map[string]any)
		if int(entry["index"].(float64)) != i || len(entry["embedding"].([]any)) != 16 {
			t.Fatalf("embedding %d: %v", i, entry)
		}
	}
}

func onlyOutput(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if response["status"] != "completed" {
		t.Fatalf("status = %v", response["status"])
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("got %d output items, want 1", len(output))
	}
	return output[0].(map[string]any)
}

func TestResponsesDriveOneRetrieveRoundTrip(t *testing.T) {
	server := newTestServer(t)
	retrieveTool := map[string]any{"type": "function", "name": "retrieve", "parameters": map[string]any{"type": "object"}}
	answerFormat := map[string]any{"format": map[string]any{
		"type": "json_schema", "name": "diderot_answer",
		"schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer":    map[string]any{"type": "string"},
				"artifacts": map[string]any{"type": "array"},
			},
			"required": []any{"answer", "artifacts"},
		},
	}}
	user := map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "What is the codeword?"}}}

	call := onlyOutput(t, post(t, server.URL+"/v1/responses", map[string]any{
		"model": "m", "input": []any{user}, "tools": []any{retrieveTool}, "text": answerFormat,
	}))
	if call["type"] != "function_call" || call["name"] != "retrieve" {
		t.Fatalf("first turn should call retrieve: %v", call)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil || args["question"] != "What is the codeword?" {
		t.Fatalf("retrieve arguments = %v (%v)", call["arguments"], err)
	}

	toolOutput := `{"hits":[{"content":"The codeword is periwinkle-kestrel-7781."}]}`
	message := onlyOutput(t, post(t, server.URL+"/v1/responses", map[string]any{
		"model": "m", "tools": []any{retrieveTool}, "text": answerFormat,
		"input": []any{user, call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": toolOutput}},
	}))
	if message["type"] != "message" {
		t.Fatalf("second turn should answer: %v", message)
	}
	text := message["content"].([]any)[0].(map[string]any)["text"].(string)
	var final struct {
		Answer    string `json:"answer"`
		Artifacts []any  `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(text), &final); err != nil {
		t.Fatalf("answer is not the requested JSON: %q: %v", text, err)
	}
	if !strings.Contains(final.Answer, "periwinkle-kestrel-7781") || final.Artifacts == nil {
		t.Fatalf("answer should quote the retrieved content: %+v", final)
	}
}

func TestResponsesHonorForcedFunction(t *testing.T) {
	server := newTestServer(t)
	call := onlyOutput(t, post(t, server.URL+"/v1/responses", map[string]any{
		"model": "m", "input": "a session transcript",
		"tools": []any{map[string]any{"type": "function", "name": "record_session_title", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"},
		}}},
		"tool_choice": map[string]any{"type": "function", "name": "record_session_title"},
	}))
	if call["name"] != "record_session_title" || call["arguments"] != `{"title":"Quickstart smoke"}` {
		t.Fatalf("forced call = %v", call)
	}
}

func TestResponsesWithoutToolsAnswerText(t *testing.T) {
	server := newTestServer(t)
	message := onlyOutput(t, post(t, server.URL+"/v1/responses", map[string]any{"model": "m", "input": "ping"}))
	if text := message["content"].([]any)[0].(map[string]any)["text"]; text != "ok" {
		t.Fatalf("text = %v", text)
	}
}

func TestChatCompletionsAnswer(t *testing.T) {
	server := newTestServer(t)
	out := post(t, server.URL+"/v1/chat/completions", map[string]any{
		"model":    "gpt-6-luna",
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", out["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] == "" {
		t.Fatalf("message = %v", message)
	}
}
