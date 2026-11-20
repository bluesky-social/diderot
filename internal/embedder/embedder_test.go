package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestGatewayEmbedder(t *testing.T, baseURL, apiKey, model string, dim int) *GatewayEmbedder {
	t.Helper()
	embedder, err := NewGatewayEmbedder(GatewayEmbedderConfig{BaseURL: baseURL, APIKey: apiKey, Model: model, Dim: dim})
	if err != nil {
		t.Fatal(err)
	}
	return embedder
}

func TestGatewayEmbedderBasic(t *testing.T) {
	// Mock vLLM: echoes the input count as fixed-dim vectors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gateway-key" {
			http.Error(w, "missing gateway authorization", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var req embeddingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := make([]item, len(req.Input))
		for i := range req.Input {
			v := make([]float32, 1024)
			for j := range v {
				v[j] = float32(i + 1)
			}
			out[i] = item{Index: i, Embedding: v}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "model": req.Model})
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "gateway-key", "local-embed", 1024)
	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 || len(vecs[0]) != 1024 {
		t.Fatalf("Embed shape: got %dx%d, want 3x1024", len(vecs), len(vecs[0]))
	}
	for i, v := range vecs {
		if v[0] != float32(i+1) {
			t.Errorf("vec %d not identifiable: got %v", i, v[:3])
		}
	}
}

func TestGatewayEmbedderEmpty(t *testing.T) {
	e := newTestGatewayEmbedder(t, "http://never-called", "key", "x", 1024)
	vecs, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if vecs != nil {
		t.Fatalf("empty input should return nil, got %v", vecs)
	}
}

func TestGatewayEmbedderRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []GatewayEmbedderConfig{
		{BaseURL: "ftp://gateway", APIKey: "key", Model: "local-embed", Dim: 1024},
		{BaseURL: "https://user:password@gateway/v1", APIKey: "key", Model: "local-embed", Dim: 1024},
		{BaseURL: "https://gateway/v1?token=secret", APIKey: "key", Model: "local-embed", Dim: 1024},
		{BaseURL: "https://gateway/v1", APIKey: "key", Dim: 1024},
		{BaseURL: "https://gateway/v1", APIKey: "key", Model: "local-embed"},
		{BaseURL: "", APIKey: "key", Model: "local-embed", Dim: 1024},
	} {
		if _, err := NewGatewayEmbedder(tc); err == nil {
			t.Fatalf("expected invalid config to be rejected: %#v", tc)
		}
	}
}

// TestGatewayEmbedderDimensionsField: the request carries dimensions only
// when the deployment opts in, and every returned vector is checked against
// the configured size either way.
func TestGatewayEmbedderDimensionsField(t *testing.T) {
	for _, send := range []bool{false, true} {
		var body map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"index": 0, "embedding": make([]float32, 256)}},
			})
		}))
		e, err := NewGatewayEmbedder(GatewayEmbedderConfig{
			BaseURL: srv.URL + "/v1", APIKey: "key", Model: "text-embedding-3-small", Dim: 256, SendDimensions: send,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
			t.Fatalf("send=%t: Embed: %v", send, err)
		}
		if got, ok := body["dimensions"]; ok != send || (send && got != float64(256)) {
			t.Errorf("send=%t: dimensions = %v (present=%t)", send, got, ok)
		}
		srv.Close()
	}
}

func TestGatewayEmbedderBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestGatewayEmbedderWrongDim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{0, 1, 2}}, // only 3 dims, we want 1024
			},
		})
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error on dim mismatch, got nil")
	}
}

func TestGatewayEmbedderWrongCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Input is 2 strings; return only 1 embedding.
		v := make([]float32, 1024)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": v}},
		})
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error on count mismatch, got nil")
	}
}

func TestGatewayEmbedder5xxIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Model 'local-embed' is temporarily unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsTransient(err) {
		t.Errorf("503 should be classified TransientError; got %T: %v", err, err)
	}
}

func TestGatewayEmbedder429IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if !IsTransient(err) {
		t.Errorf("429 should be transient; got %v", err)
	}
}

func TestGatewayEmbedder404IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ray Serve's answer while the embed model has no live replica.
		http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if !IsTransient(err) {
		t.Errorf("404 should be transient; got %v", err)
	}
}

func TestGatewayEmbedder400IsNotTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad input", http.StatusBadRequest)
	}))
	defer srv.Close()

	e := newTestGatewayEmbedder(t, srv.URL+"/v1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if IsTransient(err) {
		t.Errorf("400 should be terminal; got transient: %v", err)
	}
}

func TestGatewayEmbedderDoesNotForwardBearerAcrossRedirect(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	e := newTestGatewayEmbedder(t, redirect.URL+"/v1", "gateway-secret", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("expected redirect response to be rejected, got %v", err)
	}
	if leaked != "" {
		t.Fatalf("gateway bearer leaked across redirect: %q", leaked)
	}
}

func TestGatewayEmbedderConnectionRefusedIsTransient(t *testing.T) {
	// Point at a port nothing's listening on.
	e := newTestGatewayEmbedder(t, "http://127.0.0.1:1", "key", "x", 1024)
	_, err := e.Embed(context.Background(), []string{"hello"})
	if !IsTransient(err) {
		t.Errorf("connection refused should be transient; got %T: %v", err, err)
	}
}

func TestChunkRowEmbedInput(t *testing.T) {
	// With prefix: "<prefix>\n\n<content>".
	r := chunkRow{content: "hello", contextPrefix: "Slack message in #ops:"}
	if got := r.embedInput(); got != "Slack message in #ops:\n\nhello" {
		t.Errorf("got %q", got)
	}
	// Without prefix: content only (no leading newlines so old data
	// produces the same embedding as before the column existed).
	r = chunkRow{content: "hello"}
	if got := r.embedInput(); got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestGatewayEmbedderID(t *testing.T) {
	e := newTestGatewayEmbedder(t, "http://x", "key", "local-embed", 1024)
	if id := e.ID(); id != "self:local-embed:v1" {
		t.Errorf("default ID: got %q", id)
	}
	e.ModelID = "scope:custom:v9"
	if id := e.ID(); id != "scope:custom:v9" {
		t.Errorf("custom ID: got %q", id)
	}
}
