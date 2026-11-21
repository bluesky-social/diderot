package codexprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

const maxCatalogBytes = 8 << 20

// listedModel is an OpenAI-shaped listing entry carrying Codex's model
// metadata in the fields Diderot reads.
type listedModel struct {
	ID            string                   `json:"id"`
	Object        string                   `json:"object"`
	Protocols     []modelregistry.Protocol `json:"protocols"`
	Name          string                   `json:"name,omitempty"`
	OwnedBy       string                   `json:"owned_by"`
	ContextWindow int                      `json:"context_window,omitempty"`
	Efforts       []string                 `json:"reasoning_efforts,omitempty"`
	DefaultEffort string                   `json:"default_reasoning_effort,omitempty"`
	Architecture  struct {
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"architecture,omitzero"`
}

func (b *codexBackend) models(ctx context.Context, selected string) ([]listedModel, error) {
	resp, err := b.do(ctx, http.MethodGet, b.modelsURL, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var page struct {
		Models []struct {
			Slug            string   `json:"slug"`
			Name            string   `json:"display_name"`
			Visibility      string   `json:"visibility"`
			ContextWindow   int      `json:"context_window"`
			InputModalities []string `json:"input_modalities"`
			DefaultEffort   string   `json:"default_reasoning_level"`
			Efforts         []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBytes))
	if decoder.Decode(&page) != nil || page.Models == nil {
		return nil, &backendError{http.StatusBadGateway, "invalid model catalog from Codex"}
	}

	models := make([]listedModel, 0, len(page.Models))
	for _, model := range page.Models {
		if model.Slug == "" || model.Visibility != "list" || (selected != "" && model.Slug != selected) {
			continue
		}
		entry := listedModel{
			ID: model.Slug, Object: "model", Name: model.Name, OwnedBy: "openai",
			ContextWindow: model.ContextWindow, Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses},
		}
		entry.Architecture.InputModalities = model.InputModalities
		// The catalog also advertises harness presets such as Ultra. Offer
		// only the effort values the direct Responses transport accepts.
		for _, level := range model.Efforts {
			if slices.Contains(modelregistry.ReasoningEfforts, level.Effort) {
				entry.Efforts = append(entry.Efforts, level.Effort)
			}
		}
		if slices.Contains(modelregistry.ReasoningEfforts, model.DefaultEffort) {
			entry.DefaultEffort = model.DefaultEffort
		}
		models = append(models, entry)
	}
	if selected != "" && len(models) == 0 {
		return nil, &backendError{http.StatusBadRequest, "the configured model is not selectable in this Codex account"}
	}
	return models, nil
}
