package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestResponsesConfigValidation(t *testing.T) {
	valid := func() ResponsesConfig {
		return ResponsesConfig{
			BaseURL:    "https://api.openai.com/v1",
			APIKey:     "sk-test",
			ModelID:    "gpt-5",
			MaxTokens:  4096,
			MaxRetries: 3,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*ResponsesConfig)
		wantErr string
	}{
		{name: "valid", mutate: func(*ResponsesConfig) {}},
		{name: "missing base url", mutate: func(c *ResponsesConfig) { c.BaseURL = "" }, wantErr: "BaseURL is required"},
		{name: "missing api key", mutate: func(c *ResponsesConfig) { c.APIKey = "" }, wantErr: "APIKey is required"},
		{name: "missing model id", mutate: func(c *ResponsesConfig) { c.ModelID = "" }, wantErr: "ModelID is required"},
		{name: "unparsable base url", mutate: func(c *ResponsesConfig) { c.BaseURL = "://bad" }, wantErr: "invalid BaseURL"},
		{name: "unsupported scheme", mutate: func(c *ResponsesConfig) { c.BaseURL = "ftp://example.com" }, wantErr: "must use http or https"},
		{name: "missing host", mutate: func(c *ResponsesConfig) { c.BaseURL = "https:///v1" }, wantErr: "must use http or https"},
		{name: "base url with userinfo", mutate: func(c *ResponsesConfig) { c.BaseURL = "https://user:pass@api.openai.com/v1" }, wantErr: "must not contain userinfo"},
		{name: "base url query", mutate: func(c *ResponsesConfig) { c.BaseURL = "https://api.openai.com/v1?x=1" }, wantErr: "query or fragment"},
		{name: "negative timeout", mutate: func(c *ResponsesConfig) { c.Timeout = -time.Second }, wantErr: "Timeout must be positive"},
		{name: "negative max tokens", mutate: func(c *ResponsesConfig) { c.MaxTokens = -1 }, wantErr: "MaxTokens must not be negative"},
		{name: "negative retries", mutate: func(c *ResponsesConfig) { c.MaxRetries = -2 }, wantErr: "MaxRetries must be between"},
		{name: "excess retries", mutate: func(c *ResponsesConfig) { c.MaxRetries = 6 }, wantErr: "MaxRetries must be between"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)

			mdl, err := NewResponsesModel(context.Background(), cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewResponsesModel() error = %v, want nil", err)
				}
				if mdl == nil {
					t.Fatal("NewResponsesModel() returned nil model")
				}
				return
			}
			if err == nil {
				t.Fatalf("NewResponsesModel() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewResponsesModel() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestResponsesConfigValidationDoesNotEchoURLCredentials(t *testing.T) {
	t.Parallel()

	const secret = "url-password-must-not-leak"
	_, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: "https://user:" + secret + "@api.openai.com/v1",
		APIKey:  "sk-test",
		ModelID: "gpt-5",
	})
	if err == nil {
		t.Fatal("NewResponsesModel() error = nil, want userinfo rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("NewResponsesModel() error leaked URL credentials: %v", err)
	}
}

// TestResponsesModelConstructionURLProvesNoRequest verifies the constructor
// wires the configured base URL into the model without issuing any network
// request: the httptest handler would fail the test if contacted.
func TestResponsesModelConstructionURLProvesNoRequest(t *testing.T) {
	t.Parallel()

	var hit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		t.Errorf("construction must not send a request to %s", r.URL)
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL:    server.URL,
		APIKey:     "sk-test",
		ModelID:    "gpt-5",
		Timeout:    time.Minute,
		MaxTokens:  4096,
		MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("NewResponsesModel() error = %v", err)
	}
	if mdl == nil {
		t.Fatal("NewResponsesModel() returned nil model")
	}
	if hit.Load() {
		t.Error("httptest server was contacted during construction")
	}

	// Prove the configured URL is what the model would call by driving a
	// generation and asserting the request lands on the httptest server.
	type recordedRequest struct {
		header http.Header
		body   map[string]any
	}
	requests := make(chan recordedRequest, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body in handler: %v", err)
		}
		requests <- recordedRequest{header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)); err != nil {
			t.Errorf("write response fixture: %v", err)
		}
	})
	callServer := httptest.NewServer(handler)
	defer callServer.Close()

	callModel, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL:              callServer.URL,
		APIKey:               "sk-test",
		ModelID:              "gpt-5",
		MaxTokens:            4096,
		MaxRetries:           2,
		PromptCacheRetention: "in_memory",
	})
	if err != nil {
		t.Fatalf("NewResponsesModel() (call server) error = %v", err)
	}

	msg, err := callModel.Generate(context.Background(), []*schema.AgenticMessage{})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if msg == nil {
		t.Fatal("Generate() returned nil message")
	}
	select {
	case req := <-requests:
		if req.header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q", req.header.Get("Authorization"))
		}
		if req.body["model"] != "gpt-5" {
			t.Errorf("model = %v, want gpt-5", req.body["model"])
		}
		if req.body["max_output_tokens"] != float64(4096) {
			t.Errorf("max_output_tokens = %v, want 4096", req.body["max_output_tokens"])
		}
		if req.body["parallel_tool_calls"] != true {
			t.Errorf("parallel_tool_calls = %v, want true", req.body["parallel_tool_calls"])
		}
		if req.body["store"] != false {
			t.Errorf("store = %v, want false", req.body["store"])
		}
		if req.body["prompt_cache_retention"] != "in_memory" {
			t.Errorf("prompt_cache_retention = %v, want in_memory (five-minute client retention)", req.body["prompt_cache_retention"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request on call server")
	}
}

func TestResponsesModelDefaults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request to %s must not be sent during construction", r.URL)
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL:    server.URL,
		APIKey:     "sk-test",
		ModelID:    "gpt-5",
		MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("NewResponsesModel() error = %v", err)
	}
	if mdl == nil {
		t.Fatal("NewResponsesModel() returned nil model")
	}
}

func TestResponsesModelDoesNotForwardBearerAcrossRedirect(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	model, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: redirect.URL, APIKey: "secret", ModelID: "gpt-5", MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Generate(context.Background(), []*schema.AgenticMessage{}); err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if leaked != "" {
		t.Fatalf("gateway bearer leaked across redirect: %q", leaked)
	}
}

// The effort is the setting that decides whether the model gateway emits
// Anthropic's thinking field at all, so an unknown value must fail loudly
// rather than fall back to the thinking-disabled behavior it exists to avoid.
func TestResponsesReasoningEffortValidation(t *testing.T) {
	t.Parallel()

	base := ResponsesConfig{BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", ModelID: "m"}
	for _, effort := range ReasoningEfforts() {
		cfg := base
		cfg.ReasoningEffort = effort
		if _, err := NewResponsesModel(context.Background(), cfg); err != nil {
			t.Errorf("effort %q must be accepted: %v", effort, err)
		}
	}
	// Empty stays legal at this layer; serve supplies the default.
	if _, err := NewResponsesModel(context.Background(), base); err != nil {
		t.Errorf("empty effort must be accepted: %v", err)
	}
	// Whitespace survives an env default, so treating it as "omitted" would
	// silently disable thinking — the failure this validation prevents.
	for _, bad := range []string{"lo", "LOW", "extreme", "1024", "thinking", " ", "   ", "\t", " low", "low "} {
		cfg := base
		cfg.ReasoningEffort = bad
		_, err := NewResponsesModel(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "reasoning effort") {
			t.Errorf("effort %q must be rejected, got %v", bad, err)
		}
	}
	if err := ValidateReasoningEffort(""); err != nil {
		t.Errorf("empty effort must validate: %v", err)
	}
	if err := ValidateReasoningEffort("nope"); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("unknown effort must name the accepted set, got %v", err)
	}
}

func TestResponsesServiceTierValidation(t *testing.T) {
	t.Parallel()

	base := ResponsesConfig{BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", ModelID: "m"}
	for _, tier := range []string{"", "default", "fast", "priority"} {
		cfg := base
		cfg.ServiceTier = tier
		_, err := NewResponsesModel(context.Background(), cfg)
		if err != nil {
			t.Errorf("service tier %q must be accepted: %v", tier, err)
		}
	}
	for _, tier := range []string{"FAST", " fast", "turbo"} {
		cfg := base
		cfg.ServiceTier = tier
		_, err := NewResponsesModel(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "service tier") {
			t.Errorf("service tier %q must be rejected, got %v", tier, err)
		}
	}
}

// The effort has to reach the wire as reasoning.effort, since that is the field
// the model gateway translates into an Anthropic thinking budget.
func TestResponsesModelSendsReasoningEffort(t *testing.T) {
	t.Parallel()

	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)); err != nil {
			t.Errorf("write fixture: %v", err)
		}
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: server.URL, APIKey: "k", ModelID: "m",
		MaxRetries: NoResponsesRetries, ReasoningEffort: "low", ServiceTier: "fast",
	})
	if err != nil {
		t.Fatalf("NewResponsesModel() error = %v", err)
	}
	if _, err := mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("q")}); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	select {
	case body := <-bodies:
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("request has no reasoning object: %v", body["reasoning"])
		}
		if reasoning["effort"] != "low" {
			t.Errorf("reasoning.effort = %v, want low", reasoning["effort"])
		}
		if body["service_tier"] != "fast" {
			t.Errorf("service_tier = %v, want fast", body["service_tier"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// Omitting the effort must leave the field off entirely rather than sending an
// empty string, which is how the thinking-disabled default arose.
func TestResponsesModelOmitsReasoningWhenUnset(t *testing.T) {
	t.Parallel()

	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)); err != nil {
			t.Errorf("write fixture: %v", err)
		}
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: server.URL, APIKey: "k", ModelID: "m", MaxRetries: NoResponsesRetries,
	})
	if err != nil {
		t.Fatalf("NewResponsesModel() error = %v", err)
	}
	if _, err := mdl.Generate(context.Background(), []*schema.AgenticMessage{NewAgenticUserText("q")}); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	select {
	case body := <-bodies:
		if raw, present := body["reasoning"]; present && raw != nil {
			t.Errorf("reasoning must be absent when unset, got %v", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}
