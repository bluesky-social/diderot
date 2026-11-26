// Package codexprovider adapts the ChatGPT Codex stream to Diderot's
// non-streaming Responses provider interface.
package codexprovider

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// A 25 MiB attachment expands by a third when encoded as a data URL.
const maxRequestBytes = 36 << 20

// Leave room for all generation probes in an admin model test to queue together.
const maxQueuedRequests = 8

// Config is the configuration of the adapter process.
type Config struct {
	AuthFile      string
	Model         string
	Token         string
	Timeout       time.Duration
	MaxConcurrent int
}

type request map[string]json.RawMessage

type backend func(context.Context, request) (json.RawMessage, error)
type catalog func(context.Context, string) ([]listedModel, error)

// NewHandler authenticates Diderot with the adapter token. A separate user
// OAuth credential authenticates the adapter to the Codex backend.
func NewHandler(cfg Config) http.Handler {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	store := &credentialStore{path: cfg.AuthFile, client: client, tokenURL: codexTokenURL}
	upstream := &codexBackend{client: client, url: codexResponsesURL, modelsURL: codexModelsURL, credentials: store}
	return newHandler(cfg, upstream.respond, upstream.models)
}

func newHandler(cfg Config, respond backend, list catalog) http.Handler {
	slots := make(chan struct{}, cfg.MaxConcurrent)
	admitted := make(chan struct{}, cfg.MaxConcurrent+maxQueuedRequests)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
		defer cancel()
		models, err := list(ctx, cfg.Model)
		if err != nil {
			writeBackendError(w, ctx, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		select {
		case admitted <- struct{}{}:
			defer func() { <-admitted }()
		default:
			writeError(w, http.StatusServiceUnavailable, "Codex request queue is full")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
		defer cancel()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			writeBackendError(w, ctx, ctx.Err())
			return
		}
		if ctx.Err() != nil {
			writeBackendError(w, ctx, ctx.Err())
			return
		}

		var req request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err := decoder.Decode(&req); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid or oversized JSON request")
			return
		}
		if err := req.prepare(cfg.Model); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		result, err := respond(ctx, req)
		if err != nil || ctx.Err() != nil {
			writeBackendError(w, ctx, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+cfg.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid provider token")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (r request) prepare(model string) error {
	var requested string
	if json.Unmarshal(r["model"], &requested) != nil || strings.TrimSpace(requested) == "" || (model != "" && requested != model) {
		return errors.New("unknown model")
	}
	if string(r["stream"]) == "true" || string(r["store"]) == "true" ||
		present(r["previous_response_id"]) || present(r["conversation"]) {
		return errors.New("only stateless, non-streaming requests are supported")
	}

	if present(r["tools"]) {
		var tools []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(r["tools"], &tools) != nil {
			return errors.New("tools must be a list of functions")
		}
		for _, tool := range tools {
			if tool.Type != "function" {
				return errors.New("only Diderot function tools are supported")
			}
		}
	}

	var instructions string
	if raw := r["instructions"]; present(raw) && json.Unmarshal(raw, &instructions) != nil {
		return errors.New("instructions must be text")
	}
	var input []json.RawMessage
	if raw := r["input"]; len(raw) > 0 && raw[0] == '"' {
		item, err := json.Marshal(map[string]any{"role": "user", "content": raw})
		if err != nil {
			return errors.New("invalid input")
		}
		input = []json.RawMessage{item}
	} else if json.Unmarshal(raw, &input) != nil || len(input) == 0 {
		return errors.New("input must contain text or conversation items")
	}
	// Codex expects initial instructions outside the input history. Preserve
	// all later items, including encrypted reasoning and native function calls.
	instructions, input = liftInstructions(instructions, input)
	if strings.TrimSpace(instructions) == "" {
		instructions = "You are a helpful assistant."
	}
	if len(input) == 0 {
		return errors.New("input must contain at least one conversation item")
	}
	r["instructions"], _ = json.Marshal(instructions)
	r["input"], _ = json.Marshal(input)
	r["stream"] = json.RawMessage("true")
	r["store"] = json.RawMessage("false")
	r["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	if !present(r["tool_choice"]) {
		r["tool_choice"] = json.RawMessage(`"auto"`)
	}
	// This backend does not accept the public API's output cap or cache
	// retention. Its reasoning and native tool settings pass through.
	delete(r, "max_output_tokens")
	delete(r, "prompt_cache_retention")
	delete(r, "stream_options")
	return nil
}

func liftInstructions(instructions string, input []json.RawMessage) (string, []json.RawMessage) {
	lines := []string{instructions}
	for i, raw := range input {
		var item struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || (item.Role != "system" && item.Role != "developer") {
			return strings.TrimSpace(strings.Join(lines, "\n\n")), input[i:]
		}
		var text string
		if json.Unmarshal(item.Content, &text) == nil {
			lines = append(lines, text)
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Content, &blocks) != nil {
			return strings.TrimSpace(strings.Join(lines, "\n\n")), input[i:]
		}
		for _, block := range blocks {
			if block.Type == "input_text" {
				lines = append(lines, block.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n\n")), nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != `""`
}

func writeBackendError(w http.ResponseWriter, ctx context.Context, err error) {
	if ctx.Err() != nil {
		writeError(w, http.StatusGatewayTimeout, "Codex request ended before it completed")
		return
	}
	var upstream *backendError
	if errors.As(err, &upstream) {
		writeError(w, upstream.status, upstream.message)
		return
	}
	writeError(w, http.StatusBadGateway, "Codex backend request failed")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": "codex_provider_error"}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
