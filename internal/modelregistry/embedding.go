package modelregistry

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// EmbeddingDimensions makes one embedding call to model on p and returns the
// size of the vector it answers with: what the vector collection for the
// model must hold.
func EmbeddingDimensions(ctx context.Context, p Provider, model string, httpClient *http.Client) (int, error) {
	if reason := p.Unusable(); reason != "" {
		return 0, &TestError{Reason: "the provider " + reason}
	}
	if p.Kind != KindOpenAI {
		return 0, &TestError{Reason: "an " + p.Kind.Title() + " provider serves no embeddings"}
	}
	client, err := llmclient.New(llmclient.Config{
		BaseURL: p.Endpoint.BaseURL, APIKey: p.Endpoint.APIKey, Auth: p.Kind.Auth(), HTTPClient: p.Endpoint.HTTPClient(httpClient),
	})
	if err != nil {
		return 0, &TestError{Reason: "the provider's base URL is not usable"}
	}
	raw, err := client.PostJSON(ctx, "/embeddings", map[string]any{"model": model, "input": []string{testPrompt}}, 4<<20)
	if err := classify(ctx, err); err != nil {
		return 0, err
	}
	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Data) != 1 || len(decoded.Data[0].Embedding) == 0 {
		return 0, &TestError{Reason: "it did not answer with an embedding"}
	}
	return len(decoded.Data[0].Embedding), nil
}
