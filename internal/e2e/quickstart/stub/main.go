// Command stub stands in for the OpenAI-compatible model provider and the
// embedder in the quickstart smoke (scripts/quickstart-smoke.sh), so the smoke
// needs no secrets and no network. It is deterministic, not a model: it serves
// just enough of the Responses and Embeddings APIs to drive the readiness
// probe, session titles, and one Ask turn through the retrieve tool.
//
// Embeddings hash each word into a fixed-dimension vector, so texts that share
// words score high under cosine similarity and retrieval ranks meaningfully.
//
// Responses:
//   - a forced function tool_choice (session titles) gets that call, with
//     arguments filled from the tool's schema;
//   - a request offering the retrieve tool with no tool output yet gets a
//     retrieve call for the last user message;
//   - anything else gets a message. After a tool output, the message quotes
//     the retrieved chunks' content, so an answer can only contain fixture
//     text if Ask really ran retrieval against the corpus.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	dim := flag.Int("dim", 1024, "embedding dimension")
	healthcheck := flag.String("healthcheck", "", "GET this URL, exit 0 on 200, and exit (for container health checks)")
	flag.Parse()

	if *healthcheck != "" {
		os.Exit(probe(*healthcheck))
	}
	if *dim <= 0 {
		log.Fatalf("stub: -dim must be positive")
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           newHandler(*dim, log.New(os.Stdout, "", log.LstdFlags)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("stub: listening on %s (embedding dim %d)", *listen, *dim)
	log.Fatal(srv.ListenAndServe())
}

func probe(url string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}

type stub struct {
	dim    int
	logger *log.Logger
	seq    atomic.Int64
}

func newHandler(dim int, logger *log.Logger) http.Handler {
	s := &stub{dim: dim, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	return mux
}

const maxRequestBytes = 8 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(into); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// --- models ---

// Models are what the stub's listing offers: a model for every chat job and
// an embedding model. It answers any model name, so the names only need to
// match what the smoke chooses.
const (
	ChatModel  = "stub-chat"
	EmbedModel = "stub-embed"
)

// handleModels lists the stub's models, as an OpenAI-compatible provider's
// listing does, with the embedding model typed as one.
func (s *stub) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
		{"id": ChatModel, "object": "model", "owned_by": "stub"},
		{"id": EmbedModel, "object": "model", "owned_by": "stub", "type": "embedding"},
	}})
}

// --- chat completions ---

// handleChatCompletions answers every Chat Completions request with one short
// assistant message. Chat roles (thread summaries) only need a valid reply
// here: the smoke drives Ask through the Responses API, and the setup page's
// model test needs every enabled role to answer.
func (s *stub) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	s.logger.Printf("chat model=%q", req.Model)
	writeJSON(w, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-stub-%d", s.seq.Add(1)),
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

// --- embeddings ---

type embeddingsRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

func (s *stub) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req embeddingsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var inputs []string
	if err := json.Unmarshal(req.Input, &inputs); err != nil {
		var single string
		if err := json.Unmarshal(req.Input, &single); err != nil {
			http.Error(w, "input must be a string or an array of strings", http.StatusBadRequest)
			return
		}
		inputs = []string{single}
	}
	data := make([]map[string]any, len(inputs))
	tokens := 0
	for i, text := range inputs {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": embed(text, s.dim)}
		tokens += len(words(text))
	}
	s.logger.Printf("embeddings model=%q inputs=%d", req.Model, len(inputs))
	writeJSON(w, map[string]any{
		"object": "list", "model": req.Model, "data": data,
		"usage": map[string]any{"prompt_tokens": tokens, "total_tokens": tokens},
	})
}

// embed hashes each word into one signed dimension and normalizes the result.
func embed(text string, dim int) []float32 {
	vector := make([]float64, dim)
	for _, word := range words(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(word))
		sum := h.Sum64()
		sign := 1.0
		if sum&(1<<63) != 0 {
			sign = -1
		}
		vector[sum%uint64(dim)] += sign
	}
	var norm float64
	for _, x := range vector {
		norm += x * x
	}
	out := make([]float32, dim)
	if norm == 0 {
		// Qdrant cannot normalize a zero vector for cosine distance.
		out[0] = 1
		return out
	}
	norm = math.Sqrt(norm)
	for i, x := range vector {
		out[i] = float32(x / norm)
	}
	return out
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// --- responses ---

type responsesTool struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters"`
}

type responsesRequest struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Tools      []responsesTool `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Text       *struct {
		Format struct {
			Type   string         `json:"type"`
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	} `json:"text"`
}

// transcript is what the stub reads from a Responses input: the last user
// message and the tool outputs that follow it.
type transcript struct {
	lastUser    string
	toolOutputs []string
}

func readTranscript(raw json.RawMessage) (transcript, error) {
	var t transcript
	if len(raw) == 0 {
		return t, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		t.lastUser = text
		return t, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return t, errors.New("input must be a string or an array of items")
	}
	for _, item := range items {
		switch {
		case item["type"] == "function_call_output":
			t.toolOutputs = append(t.toolOutputs, contentText(item["output"]))
		case item["role"] == "user":
			t.lastUser = contentText(item["content"])
			t.toolOutputs = nil
		}
	}
	return t, nil
}

// contentText flattens a Responses content value: a string, or an array of
// parts that carry text.
func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func forcedFunction(raw json.RawMessage) string {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) == nil && choice.Type == "function" {
		return choice.Name
	}
	return ""
}

func (s *stub) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if !decodeBody(w, r, &req) {
		return
	}
	t, err := readTranscript(req.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n := s.seq.Add(1)

	var output map[string]any
	var kind string
	if name := forcedFunction(req.ToolChoice); name != "" {
		var schema map[string]any
		for _, tool := range req.Tools {
			if tool.Name == name {
				schema = tool.Parameters
			}
		}
		kind = "forced call " + name
		output = functionCall(n, name, fillSchema(schema, "Quickstart smoke"))
	} else if offersTool(req.Tools, "retrieve") && len(t.toolOutputs) == 0 {
		kind = "retrieve call"
		output = functionCall(n, "retrieve", map[string]any{"question": truncateRunes(t.lastUser, 500)})
	} else {
		answer := "ok"
		if len(t.toolOutputs) > 0 {
			answer = "The corpus says:\n" + retrievedContent(t.toolOutputs)
		}
		text := answer
		if req.Text != nil && req.Text.Format.Type == "json_schema" {
			encoded, err := json.Marshal(fillSchema(req.Text.Format.Schema, answer))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			text = string(encoded)
		}
		kind = fmt.Sprintf("message after %d tool outputs", len(t.toolOutputs))
		output = map[string]any{
			"type": "message", "id": fmt.Sprintf("msg_%d", n), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
	}
	s.logger.Printf("responses model=%q tools=%d -> %s", req.Model, len(req.Tools), kind)
	writeJSON(w, map[string]any{
		"id": fmt.Sprintf("resp_%d", n), "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": req.Model, "parallel_tool_calls": false,
		"output": []any{output},
		"usage": map[string]any{
			"input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens": 2,
		},
	})
}

func offersTool(tools []responsesTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func functionCall(n int64, name string, args any) map[string]any {
	encoded, _ := json.Marshal(args)
	return map[string]any{
		"type": "function_call", "id": fmt.Sprintf("fc_%d", n), "call_id": fmt.Sprintf("call_%d", n),
		"name": name, "arguments": string(encoded), "status": "completed",
	}
}

// fillSchema builds the smallest value a JSON schema accepts, using text for
// every string.
func fillSchema(schema map[string]any, text string) any {
	switch schema["type"] {
	case "object":
		out := map[string]any{}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, name := range required {
			key, _ := name.(string)
			child, _ := properties[key].(map[string]any)
			out[key] = fillSchema(child, text)
		}
		return out
	case "array":
		return []any{}
	case "integer", "number":
		return 0
	case "boolean":
		return false
	default:
		return text
	}
}

// retrievedContent quotes the content of every retrieved chunk in the tool
// outputs, falling back to the raw output when it carries none.
func retrievedContent(outputs []string) string {
	var parts []string
	for _, output := range outputs {
		var decoded any
		found := 0
		if json.Unmarshal([]byte(output), &decoded) == nil {
			walk(decoded, func(key string, value any) {
				if text, ok := value.(string); ok && key == "content" && text != "" {
					parts = append(parts, text)
					found++
				}
			})
		}
		if found == 0 {
			parts = append(parts, output)
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), 4000)
}

func walk(value any, visit func(key string, value any)) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			visit(key, child)
			walk(child, visit)
		}
	case []any:
		for _, child := range v {
			walk(child, visit)
		}
	}
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
