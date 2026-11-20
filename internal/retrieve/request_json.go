package retrieve

import (
	"encoding/json"
	"fmt"
	"time"
)

// RequestInput is the stable snake_case/RFC3339 input shape used by the Ask
// retrieve tool and persisted in ask_messages tool traces.
type RequestInput struct {
	Question       string      `json:"question"`
	Sources        []string    `json:"sources,omitempty"`
	Actors         []string    `json:"actors,omitempty"`
	OccurredAfter  string      `json:"occurred_after,omitempty"`
	OccurredBefore string      `json:"occurred_before,omitempty"`
	TopK           json.Number `json:"top_k,omitempty"`
}

// RequestFromInput converts typed tool input into the shared Request type.
// allowedScopes is always supplied by the server-side caller and cannot be
// widened by model input.
func RequestFromInput(in RequestInput, allowedScopes []string) (Request, error) {
	topK := 0
	if in.TopK != "" {
		n, err := in.TopK.Int64()
		if err != nil {
			return Request{}, fmt.Errorf("top_k: %w", err)
		}
		topK = int(n)
	}
	req := Request{
		Question: in.Question,
		Filters: Filters{
			Sources: in.Sources, Actors: in.Actors, AllowedScopes: allowedScopes,
		},
		TopK: topK,
	}
	var err error
	if in.OccurredAfter != "" {
		req.Filters.OccurredAfter, err = time.Parse(time.RFC3339, in.OccurredAfter)
		if err != nil {
			return Request{}, fmt.Errorf("occurred_after: %w", err)
		}
	}
	if in.OccurredBefore != "" {
		req.Filters.OccurredBefore, err = time.Parse(time.RFC3339, in.OccurredBefore)
		if err != nil {
			return Request{}, fmt.Errorf("occurred_before: %w", err)
		}
	}
	return req, nil
}
