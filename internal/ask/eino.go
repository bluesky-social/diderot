package ask

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	openairesponses "github.com/openai/openai-go/v3/responses"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

const (
	responsesDefaultTimeout    = 5 * time.Minute
	responsesDefaultMaxRetries = 2
	responsesMaxRetries        = 5
)

// NoResponsesRetries disables transport retries when set as
// ResponsesConfig.MaxRetries. The zero value keeps the default, so opting out
// needs an explicit sentinel.
const NoResponsesRetries = -1

// ResponsesConfig configures the OpenAI-compatible Responses API model used
// for ask. Only the model construction surface is defined for now; agent,
// runner, tool, prompt, and metadata wiring lands in later slices.
type ResponsesConfig struct {
	BaseURL   string
	APIKey    string
	ModelID   string
	Timeout   time.Duration // zero means the 5m default
	MaxTokens int
	// MaxRetries bounds transport-level retries: zero means the default of 2,
	// NoResponsesRetries disables them entirely. Disabling exists for tests
	// whose scripted fake gateways answer the Nth request with the Nth step —
	// a transparent retry consumes a step and silently shifts the whole
	// script, which is a load-dependent flake, not a failure of the code under
	// test.
	MaxRetries int
	HTTPClient *http.Client
	// ReasoningEffort sets the Responses reasoning effort on every request.
	// Leaving it empty is not a neutral default: a model gateway may only emit
	// Anthropic's `thinking` field when a caller supplies an effort, so an
	// unset value runs Claude with thinking disabled — a configuration
	// Anthropic documents as occasionally writing tool calls into text output
	// instead of emitting a structured tool_use block, most often on
	// tool-heavy loops like Ask's. Callers should set it explicitly.
	ReasoningEffort string
	// ServiceTier selects OpenAI Fast mode for provider-pinned OpenAI routes.
	// Empty leaves the field off and preserves the provider default.
	ServiceTier string
	// PromptCacheRetention is sent as prompt_cache_retention when non-empty;
	// NewModel takes it from the model registry entry.
	PromptCacheRetention string
}

// ReasoningEfforts are the canonical effort names accepted by the transport.
// Model registry entries further restrict these to each model's supported
// values.
func ReasoningEfforts() []string {
	return slices.Clone(modelregistry.ReasoningEfforts)
}

// ValidateReasoningEffort rejects an unknown effort so a typo fails at startup
// rather than silently degrading to thinking-disabled, which is the failure
// this setting exists to prevent.
// Only the exactly-empty value is legal, which is the library/test caller that
// omitted the field. A value that merely looks empty — whitespace surviving an
// env default, say — is explicit configuration and must fail, because trimming
// it to empty would silently disable thinking, the very outcome this validation
// exists to prevent.
func ValidateReasoningEffort(effort string) error {
	if effort == "" {
		return nil
	}
	for _, known := range ReasoningEfforts() {
		if effort == known {
			return nil
		}
	}
	return fmt.Errorf("reasoning effort %q is not one of %s", effort, strings.Join(ReasoningEfforts(), ", "))
}

// NewResponsesModel validates cfg and constructs the agenticopenai Responses
// model. The model is non-streaming, allows parallel tool calls, caps output
// tokens when MaxTokens is positive, and bounds retries to MaxRetries.
func NewResponsesModel(ctx context.Context, cfg ResponsesConfig) (*agenticopenai.ResponsesModel, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("responses: BaseURL is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("responses: APIKey is required")
	}
	if strings.TrimSpace(cfg.ModelID) == "" {
		return nil, fmt.Errorf("responses: ModelID is required")
	}

	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("responses: invalid BaseURL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("responses: BaseURL must use http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("responses: BaseURL must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("responses: BaseURL must not contain a query or fragment")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = responsesDefaultTimeout
	} else if timeout < 0 {
		return nil, fmt.Errorf("responses: Timeout must be positive")
	}
	maxRetries := cfg.MaxRetries
	switch {
	case maxRetries == 0:
		maxRetries = responsesDefaultMaxRetries
	case maxRetries == NoResponsesRetries:
		maxRetries = 0
	case maxRetries < 0 || maxRetries > responsesMaxRetries:
		return nil, fmt.Errorf("responses: MaxRetries must be between 1 and %d, or NoResponsesRetries", responsesMaxRetries)
	}
	if cfg.MaxTokens < 0 {
		return nil, fmt.Errorf("responses: MaxTokens must not be negative")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	httpCopy := *httpClient
	httpCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// Left unstated, this API makes every tool parameter required.
	httpCopy.Transport = &strictToolsTransport{base: httpCopy.Transport}
	if err := ValidateReasoningEffort(cfg.ReasoningEffort); err != nil {
		return nil, fmt.Errorf("responses: %w", err)
	}
	if cfg.ServiceTier != "" && cfg.ServiceTier != "default" && cfg.ServiceTier != "fast" && cfg.ServiceTier != "priority" {
		return nil, fmt.Errorf("responses: service tier %q is not one of default, fast, priority", cfg.ServiceTier)
	}
	var reasoning *openairesponses.ReasoningParam
	if cfg.ReasoningEffort != "" {
		reasoning = &openairesponses.ReasoningParam{Effort: openairesponses.ReasoningEffort(cfg.ReasoningEffort)}
	}
	var serviceTier *openairesponses.ResponseNewParamsServiceTier
	if cfg.ServiceTier != "" {
		value := openairesponses.ResponseNewParamsServiceTier(cfg.ServiceTier)
		serviceTier = &value
	}
	parallelToolCalls := true
	store := false
	var promptCacheRetention *openairesponses.ResponseNewParamsPromptCacheRetention
	if cfg.PromptCacheRetention != "" {
		value := openairesponses.ResponseNewParamsPromptCacheRetention(cfg.PromptCacheRetention)
		promptCacheRetention = &value
	}
	var maxTokens *int
	if cfg.MaxTokens > 0 {
		maxTokens = &cfg.MaxTokens
	}

	mdl, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
		BaseURL:              cfg.BaseURL,
		APIKey:               cfg.APIKey,
		Model:                cfg.ModelID,
		Timeout:              &timeout,
		MaxRetries:           &maxRetries,
		HTTPClient:           &httpCopy,
		MaxTokens:            maxTokens,
		ParallelToolCalls:    &parallelToolCalls,
		Store:                &store,
		EnableAutoCache:      false,
		Include:              []openairesponses.ResponseIncludable{openairesponses.ResponseIncludableReasoningEncryptedContent},
		PromptCacheRetention: promptCacheRetention,
		Reasoning:            reasoning,
		ServiceTier:          serviceTier,
	})
	if err != nil {
		return nil, fmt.Errorf("responses: build model: %w", err)
	}
	return mdl, nil
}
