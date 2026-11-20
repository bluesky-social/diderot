// Package embedder defines the Embedder interface and a vLLM-backed
// OpenAI-compatible client. The interface is defined here (not in
// internal/events) so a future retrieval path can depend on it
// without importing the event store.
package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// TransientError wraps an underlying embedder failure that's likely
// to succeed on retry after a short wait (5xx, 429, 404, connection
// refused, dial timeout, i/o timeout). Callers in the worker retry
// the whole batch without touching any row's failure_count — these
// are service-level problems, not per-row poison.
//
// 404 is transient because Ray Serve answers {"detail":"Not Found"}
// while the embed model is scaled to zero or reloading; the route
// comes back once the replica is up. It is never a per-row defect.
type TransientError struct {
	Underlying error
}

func (e *TransientError) Error() string { return "transient: " + e.Underlying.Error() }
func (e *TransientError) Unwrap() error { return e.Underlying }

// IsTransient reports whether err (or anything it wraps) is a
// TransientError. Convenience for the worker's retry decision
// point.
func IsTransient(err error) bool {
	var te *TransientError
	return errors.As(err, &te)
}

// classifyNetError wraps a net/http-layer error in TransientError
// when the cause is "service is unreachable or didn't answer in
// time". Typical shapes: dial tcp: connect: connection refused, i/o
// timeout, context deadline exceeded under a net.Error, url.Error
// wrapping the above.
func classifyNetError(err error) error {
	if err == nil {
		return nil
	}
	// url.Error wraps the transport's underlying error.
	// Timeouts are the reliable check; Temporary() was deprecated
	// because it wasn't well-defined. We fall back to string
	// matching below for the non-timeout transient cases.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return &TransientError{Underlying: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &TransientError{Underlying: err}
	}
	// "connection refused" doesn't implement Temporary() but is
	// semantically transient — the server isn't listening yet.
	// Match on the wrapped *net.OpError's error string.
	if strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "connection reset") ||
		strings.Contains(err.Error(), "EOF") {
		return &TransientError{Underlying: err}
	}
	return err
}

// Embedder converts a batch of strings into fixed-dimension vectors.
// Implementations return exactly len(texts) vectors, one per input,
// preserving order. Dimensions() must match the qdrant collection's
// configured vector size.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	// ID uniquely identifies the embedder + version, persisted to
	// event_chunks.embedder_id so a future re-ingest knows which
	// rows were produced by which model. Format:
	// "<scope>:<model>:<version>", e.g. "self:local-embed:v1".
	ID() string
}

// GatewayEmbedder calls an OpenAI-compatible embeddings endpoint.
type GatewayEmbedder struct {
	client         *llmclient.Client
	Model          string
	Dim            int
	sendDimensions bool
	ModelID        string // overrides the default ID format if set
}

// GatewayEmbedderConfig configures NewGatewayEmbedder.
type GatewayEmbedderConfig struct {
	BaseURL string
	APIKey  string
	// Auth selects the credential header; empty means bearer.
	Auth  llmclient.AuthStyle
	Model string
	// Dim is the vector length every response must carry and the Qdrant
	// collection's size.
	Dim int
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
	// SendDimensions adds "dimensions": Dim to each request, for models that
	// truncate to a requested size. Off by default because many embedding
	// servers reject the parameter.
	SendDimensions bool
}

// NewGatewayEmbedder wires up a GatewayEmbedder with a connection pool
// sized for our concurrency. Go's default Transport caps
// MaxIdleConnsPerHost at 2, which causes constant TCP teardown and
// re-handshake under a 16+ goroutine pool pointed at a single host,
// which then dominates the cost of each call.
func NewGatewayEmbedder(cfg GatewayEmbedderConfig) (*GatewayEmbedder, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("gateway embedder requires a model")
	}
	if cfg.Dim <= 0 {
		return nil, errors.New("gateway embedder requires a positive dimension")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 256
	tr.IdleConnTimeout = 90 * time.Second
	client, err := llmclient.New(llmclient.Config{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth,
		HTTPClient: &http.Client{Timeout: 60 * time.Second, Transport: tr},
		Headers:    cfg.Headers,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway embedder: %w", err)
	}
	return &GatewayEmbedder{client: client, Model: model, Dim: cfg.Dim, sendDimensions: cfg.SendDimensions}, nil
}

func (e *GatewayEmbedder) Dimensions() int { return e.Dim }

func (e *GatewayEmbedder) ID() string {
	if e.ModelID != "" {
		return e.ModelID
	}
	return "self:" + e.Model + ":v1"
}

type embeddingsRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

func (e *GatewayEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	request := embeddingsRequest{Model: e.Model, Input: texts}
	if e.sendDimensions {
		request.Dimensions = e.Dim
	}
	body, err := e.client.PostJSON(ctx, "/embeddings", request, 64<<20)
	if err != nil {
		var statusError *llmclient.HTTPError
		if errors.As(err, &statusError) {
			if statusError.StatusCode >= 500 ||
				statusError.StatusCode == http.StatusTooManyRequests ||
				statusError.StatusCode == http.StatusNotFound {
				return nil, &TransientError{Underlying: err}
			}
			return nil, err
		}
		return nil, classifyNetError(err)
	}
	var out embeddingsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(out.Data))
	}

	// Reorder by Index — vLLM emits them in-order but the protocol
	// doesn't promise that. Cheap insurance.
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embedding index %d out of range", d.Index)
		}
		if len(d.Embedding) != e.Dim {
			return nil, fmt.Errorf("embedding at index %d has dim %d, expected %d",
				d.Index, len(d.Embedding), e.Dim)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("missing embedding for index %d", i)
		}
	}
	return vecs, nil
}
