package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayCaptionerRequestAndResponse(t *testing.T) {
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			http.Error(w, "wrong authorization", http.StatusUnauthorized)
			return
		}
		raw, _ = io.ReadAll(r.Body)
		var req gatewayCaptionRequest
		if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Model != "gemini-3.6-flash" || req.MaxTokens != 4096 || req.Temperature == nil || *req.Temperature != 0 {
			http.Error(w, "wrong inference settings", http.StatusBadRequest)
			return
		}
		if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
			http.Error(w, "wrong content", http.StatusBadRequest)
			return
		}
		image := req.Messages[0].Content[0]
		if image.ImageURL == nil || image.ImageURL.URL != "data:image/png;base64,aW1hZ2U=" {
			http.Error(w, "wrong image", http.StatusBadRequest)
			return
		}
		if req.Messages[0].Content[1].Text != CaptionPrompt() {
			http.Error(w, "wrong prompt settings", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "CAPTION: A chart.\nTEXT: revenue\nUNREAD: no\nTAGS: chart, dashboard"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	// The registry's Gemini entry supplies the zero thinking budget, which
	// must land last, exactly where the request struct used to put it.
	captioner, err := NewGatewayCaptioner(GatewayCaptionerConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", ModelID: "gemini-3.6-flash",
		ExtraBody: map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := captioner.Caption(context.Background(), "image/png", []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Caption != "A chart." || got.Text != "revenue" || got.Unread ||
		strings.Join(got.Tags, ",") != "chart,dashboard" || got.ModelID != "gemini-3.6-flash" {
		t.Fatalf("unexpected caption: %#v", got)
	}
	if !strings.HasSuffix(string(raw), `"max_tokens":4096,"temperature":0,"thinking_config":{"thinkingBudget":0}}`) {
		t.Fatalf("request tail = %s", raw[max(0, len(raw)-120):])
	}

	// Without registry extra fields the request carries none: a plain
	// OpenAI-compatible server rejects unknown fields such as thinking_config.
	plain, err := NewGatewayCaptioner(GatewayCaptionerConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", ModelID: "gemini-3.6-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Caption(context.Background(), "image/png", []byte("image")); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("thinking_config")) {
		t.Fatalf("request without extra body carried thinking_config: %s", raw[max(0, len(raw)-120):])
	}
	if _, err := NewGatewayCaptioner(GatewayCaptionerConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", ExtraBody: map[string]any{"temperature": 1},
	}); err == nil {
		t.Fatal("expected a colliding extra field to be rejected at construction")
	}
}

// A model whose registry entry sets chat_token_param: max_completion_tokens,
// a reasoning token allowance, and chat_omit_temperature (OpenAI's reasoning
// models) gets the caption limit plus the allowance under
// max_completion_tokens, and no temperature.
func TestGatewayCaptionerReasoningModelRequest(t *testing.T) {
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"CAPTION: A chart."},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	captioner, err := NewGatewayCaptioner(GatewayCaptionerConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", ModelID: "gpt-6-luna",
		TokenParam: "max_completion_tokens", ReasoningTokenAllowance: 25000, OmitTemperature: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captioner.Caption(context.Background(), "image/png", []byte("image")); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(`{"model":"gpt-6-luna","messages":[`)) || !bytes.HasSuffix(raw, []byte(`],"max_completion_tokens":29096}`)) {
		t.Fatalf("request = %s", raw)
	}
	for _, field := range []string{`"max_tokens"`, `"temperature"`} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatalf("request carries %s: %s", field, raw)
		}
	}

	// Collisions are checked against the fields this model's request sends.
	for _, cfg := range []GatewayCaptionerConfig{
		{TokenParam: "max_completion_tokens", ExtraBody: map[string]any{"max_completion_tokens": 1}},
		{ExtraBody: map[string]any{"max_tokens": 1}},
		{TokenParam: "max_output_tokens"},
		{ReasoningTokenAllowance: -1},
	} {
		cfg.BaseURL, cfg.APIKey = server.URL+"/v1", "secret"
		if _, err := NewGatewayCaptioner(cfg); err == nil {
			t.Fatalf("expected config rejection for %#v", cfg)
		}
	}
}

// The model sometimes writes the UNREAD marker at the end of the last line
// of transcribed text instead of on a line of its own. The marker must leave
// the text and set the flag exactly as a well-formed one does.
func TestParseCaptionResponseUnreadMarker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		text     string
		unread   bool
	}{
		{
			name:     "own line",
			response: "CAPTION: A login form.\nTEXT: Sign in\nForgot password?\nUNREAD: yes\nTAGS: screenshot, ui",
			text:     "Sign in\nForgot password?",
			unread:   true,
		},
		{
			name:     "glued after punctuation",
			response: "CAPTION: A language picker.\nTEXT: FR DE\n#\n.UNREAD: no\n\nTAGS: screenshot, ui",
			text:     "FR DE\n#\n.",
		},
		{
			name:     "glued to a word",
			response: "CAPTION: A login form.\nTEXT: Sign in\nStuck? Ask the front deskUNREAD: yes\n\nTAGS: screenshot, ui",
			text:     "Sign in\nStuck? Ask the front desk",
			unread:   true,
		},
		{
			name:     "glued after a space, mixed case",
			response: "CAPTION: A login form.\nTEXT: Sign in Unread: Yes\nTAGS: screenshot, ui",
			text:     "Sign in",
			unread:   true,
		},
		{
			name:     "glued to the TEXT label",
			response: "CAPTION: A blurred login form.\nTEXT: UNREAD: yes\nTAGS: screenshot, ui",
			unread:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCaptionResponse(tc.response)
			if got.Text != tc.text || got.Unread != tc.unread || strings.Join(got.Tags, ",") != "screenshot,ui" {
				t.Fatalf("parse = %#v, want text %q unread %v", got, tc.text, tc.unread)
			}
			if flat := FlattenCaption("", got); strings.Contains(strings.ToUpper(flat), "UNREAD") {
				t.Fatalf("flattened caption carries the marker: %q", flat)
			}
		})
	}
}

func TestGatewayCaptionerRejectsInvalidConfig(t *testing.T) {
	for _, cfg := range []GatewayCaptionerConfig{
		{},
		{BaseURL: "ftp://gateway", APIKey: "key"},
		{BaseURL: "https://user:pass@gateway/v1", APIKey: "key"},
		{BaseURL: "https://gateway/v1?token=bad", APIKey: "key"},
	} {
		if _, err := NewGatewayCaptioner(cfg); err == nil {
			t.Fatalf("expected config rejection for %#v", cfg)
		}
	}
}

func TestGatewayCaptionerDoesNotForwardBearerAcrossRedirect(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	captioner, err := NewGatewayCaptioner(GatewayCaptionerConfig{BaseURL: redirect.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captioner.Caption(context.Background(), "image/png", []byte("image")); err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if leaked != "" {
		t.Fatalf("gateway bearer leaked to redirect target: %q", leaked)
	}
}

func TestGatewayCaptionerClassifiesTransientResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	captioner, err := NewGatewayCaptioner(GatewayCaptionerConfig{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = captioner.Caption(context.Background(), "image/png", []byte("image"))
	if !IsTransient(err) {
		t.Fatalf("503 should be transient, got %v", err)
	}
}

func TestGatewayCaptionerRejectsNonImage(t *testing.T) {
	captioner, err := NewGatewayCaptioner(GatewayCaptionerConfig{BaseURL: "https://gateway/v1", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captioner.Caption(context.Background(), "application/pdf", []byte("pdf")); err == nil {
		t.Fatal("expected non-image rejection")
	}
}
