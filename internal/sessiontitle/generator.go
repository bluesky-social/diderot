// Package sessiontitle generates small display titles from long session
// prompts. Its Generator interface is transport-agnostic so another service or
// an HTTP handler can reuse the capability without depending on Ask internals.
package sessiontitle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

const (
	maxInputBytes    = 16 << 10
	maxOutputTokens  = 256
	maxResponseBytes = 1 << 20
	maxTitleRunes    = promptcatalog.MaxTitleRunes
)

type Result struct {
	Title   string
	ModelID string
}

type Generator interface {
	Generate(context.Context, string) (Result, error)
}

// GenerateFromSummary is the one title-generation path used by live session
// summarization and the historical backfill. Its input is the final reduce
// narrative, never the raw session transcript.
func GenerateFromSummary(ctx context.Context, generator Generator, summary string) (Result, error) {
	if generator == nil {
		return Result{}, errors.New("sessiontitle: generator is required")
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := generator.Generate(callCtx, summary)
	if err != nil {
		return Result{}, err
	}
	result.Title = Clean(result.Title)
	if result.Title == "" {
		return Result{}, errors.New("sessiontitle: generator returned an empty title")
	}
	if strings.TrimSpace(result.ModelID) == "" {
		result.ModelID = "unknown"
	}
	return result, nil
}

// ReasoningEffort is the effort title requests send to models whose
// registry entry lists it.
const ReasoningEffort = "low"

type GatewayConfig struct {
	// BaseURL is the Responses endpoint, including any /v1 prefix.
	BaseURL string
	APIKey  string
	ModelID string
	// ReasoningEffort is sent as reasoning.effort when non-empty. Callers
	// pass ReasoningEffort when the model's registry entry lists it.
	ReasoningEffort string
	PromptKey       string
	HTTPClient      *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
}

type GatewayGenerator struct {
	client    *llmclient.Client
	modelID   string
	effort    string
	promptKey string
}

func NewGatewayGenerator(cfg GatewayConfig) (*GatewayGenerator, error) {
	if strings.TrimSpace(cfg.ModelID) == "" {
		return nil, errors.New("sessiontitle: model ID is required")
	}
	client, err := llmclient.New(llmclient.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers})
	if err != nil {
		return nil, fmt.Errorf("sessiontitle: %w", err)
	}
	promptKey := cfg.PromptKey
	if promptKey == "" {
		promptKey = promptcatalog.SessionTitle
	}
	return &GatewayGenerator{client: client, modelID: strings.TrimSpace(cfg.ModelID), effort: cfg.ReasoningEffort, promptKey: promptKey}, nil
}

func (g *GatewayGenerator) Generate(ctx context.Context, input string) (Result, error) {
	input = truncateUTF8(strings.TrimSpace(input), maxInputBytes)
	if input == "" {
		return Result{}, errors.New("sessiontitle: input is empty")
	}
	instructions, err := promptcatalog.Active().Text(g.promptKey)
	if err != nil {
		return Result{}, fmt.Errorf("sessiontitle: load prompt: %w", err)
	}
	body := llmclient.NewResponsesToolRequest(g.modelID, instructions, input, maxOutputTokens, g.effort,
		"record_session_title", "Record the concise session title.", map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"title": map[string]any{"type": "string", "maxLength": maxTitleRunes}},
			"required":   []string{"title"},
		})
	raw, err := g.client.PostJSON(ctx, "/responses", body, maxResponseBytes)
	if err != nil {
		return Result{}, fmt.Errorf("sessiontitle: request: %w", err)
	}
	var decoded llmclient.ResponsesToolAnswer
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Result{}, fmt.Errorf("sessiontitle: decode response: %w", err)
	}
	if decoded.Status != "completed" {
		return Result{}, fmt.Errorf("sessiontitle: response status %q", decoded.Status)
	}
	for _, output := range decoded.Output {
		if output.Type != "function_call" || output.Name != "record_session_title" {
			continue
		}
		var args struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(output.Arguments), &args); err != nil {
			return Result{}, fmt.Errorf("sessiontitle: decode tool arguments: %w", err)
		}
		title := Clean(args.Title)
		if title == "" {
			return Result{}, errors.New("sessiontitle: model returned an empty title")
		}
		return Result{Title: title, ModelID: g.modelID}, nil
	}
	return Result{}, errors.New("sessiontitle: response contained no title tool call")
}

func Clean(title string) string {
	title = strings.Join(strings.Fields(strings.Trim(events.SanitizeText(title), " \t\r\n\"'`")), " ")
	title = strings.TrimRight(title, ".!?:;,-")
	return truncateRunes(title, maxTitleRunes)
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return strings.TrimSpace(string(r[:limit-1])) + "…"
}
