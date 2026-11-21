package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Listed is one model a provider's listing offers, with what the listing
// says about it. Only the ID is standard; every other field is read from the
// shapes providers are known to use and is zero when the listing is silent.
type Listed struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ContextTokens is the context window.
	ContextTokens int `json:"context,omitempty"`
	// Images is whether the listing says the model takes image input; nil
	// when it says nothing.
	Images *bool `json:"images,omitempty"`
	// Embedding marks a model the listing types as an embedding model.
	Embedding bool `json:"embedding,omitempty"`
	// Efforts are the reasoning efforts the listing says the model accepts.
	Efforts []string `json:"efforts,omitempty"`
	// DefaultEffort describes the provider's choice when effort is omitted.
	DefaultEffort string `json:"default_effort,omitempty"`
	// Tools, Temperature and Reasons are whether the listing says the model
	// calls tools, accepts a temperature, and reasons; nil when it is
	// silent.
	Tools       *bool `json:"tools,omitempty"`
	Temperature *bool `json:"temperature,omitempty"`
	Reasons     *bool `json:"reasons,omitempty"`
	// OutputTokens is the most it writes in one answer.
	OutputTokens int `json:"output,omitempty"`
	// OwnedBy is who the listing says owns the model: on a gateway, the
	// provider it routes to.
	OwnedBy string `json:"owned_by,omitempty"`
	// InputPrice and OutputPrice are USD per million tokens.
	InputPrice  *float64 `json:"input_price,omitempty"`
	OutputPrice *float64 `json:"output_price,omitempty"`
	// Deprecated is set when the listing says the model is deprecated.
	Deprecated bool `json:"deprecated,omitempty"`
	// Retires is the date (YYYY-MM-DD) the listing says the model stops
	// working.
	Retires string `json:"retires,omitempty"`
}

// ErrNoListing means the provider has no model list endpoint (it answered
// 404 or 405): the models it offers are entered by hand.
var ErrNoListing = errors.New("the provider has no model list")

// listResponseLimit bounds one listing page: a large gateway's metadata-rich
// listing is a few hundred kilobytes.
const listResponseLimit = 8 << 20

// listPages bounds how many pages a paginated listing is followed for.
const listPages = 20

// anthropicVersion is the anthropic-version header every Messages-family
// request carries.
const anthropicVersion = "2023-06-01"

// List reads the provider's model listing. Errors never carry the response
// body or the key.
func List(ctx context.Context, client *http.Client, p Provider) ([]Listed, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if p.Kind == KindAnthropic {
		return listAnthropic(ctx, client, p)
	}
	raw, err := getListing(ctx, client, p, strings.TrimRight(p.Endpoint.BaseURL, "/")+"/models")
	if err != nil {
		return nil, err
	}
	var page struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, errors.New("the model list is not JSON in the expected shape")
	}
	listed := make([]Listed, 0, len(page.Data))
	for _, model := range page.Data {
		if model.ID == "" {
			continue
		}
		listed = append(listed, model.listed())
	}
	return sortListed(listed), nil
}

// openAIModel is one entry of an OpenAI-shaped listing, with the fields
// OpenAI (shutdown_date), OpenRouter and gateways that echo its shape
// (architecture, context_length, pricing per token, expiration_date,
// supported_parameters, capabilities), Together and LM Studio (type, pricing
// per million), Groq and vLLM (context_window, max_model_len,
// max_completion_tokens) add.
type openAIModel struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	Type                string `json:"type"`
	OwnedBy             string `json:"owned_by"`
	ContextWindow       int    `json:"context_window"`
	ContextLength       int    `json:"context_length"`
	MaxModelLen         int    `json:"max_model_len"`
	MaxTokens           int    `json:"max_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Architecture        *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	// Capabilities is read only when it is an object of flags; servers use
	// the name for other shapes too.
	Capabilities        json.RawMessage            `json:"capabilities"`
	ReasoningEfforts    []string                   `json:"reasoning_efforts"`
	DefaultEffort       string                     `json:"default_reasoning_effort"`
	SupportedParameters []string                   `json:"supported_parameters"`
	Pricing             map[string]json.RawMessage `json:"pricing"`
	ShutdownDate        string                     `json:"shutdown_date"`
	ExpirationDate      string                     `json:"expiration_date"`
	Status              string                     `json:"status"`
}

func (m openAIModel) listed() Listed {
	listed := Listed{ID: m.ID, Name: firstNonEmpty(m.Name, m.DisplayName)}
	if listed.Name == m.ID {
		listed.Name = ""
	}
	listed.ContextTokens = max(m.ContextWindow, m.ContextLength, m.MaxModelLen)
	if m.Architecture != nil && len(m.Architecture.InputModalities) > 0 {
		images := slices.Contains(m.Architecture.InputModalities, "image")
		listed.Images = &images
	}
	switch strings.ToLower(m.Type) {
	case "embedding", "embeddings":
		listed.Embedding = true
	case "vlm":
		images := true
		listed.Images = &images
	}
	// OpenRouter prices per token as strings (prompt, completion); Together
	// per million as numbers (input, output).
	if in, out, ok := price(m.Pricing, "prompt", "completion", 1e6); ok {
		listed.InputPrice, listed.OutputPrice = &in, &out
	} else if in, out, ok := price(m.Pricing, "input", "output", 1); ok {
		listed.InputPrice, listed.OutputPrice = &in, &out
	}
	listed.Retires = firstNonEmpty(day(m.ShutdownDate), day(m.ExpirationDate))
	listed.Deprecated = strings.EqualFold(m.Status, "deprecated")
	listed.OwnedBy = m.OwnedBy
	listed.OutputTokens = max(m.MaxTokens, m.MaxCompletionTokens)
	for _, effort := range m.ReasoningEfforts {
		if slices.Contains(ReasoningEfforts, effort) && !slices.Contains(listed.Efforts, effort) {
			listed.Efforts = append(listed.Efforts, effort)
		}
	}
	if slices.Contains(ReasoningEfforts, m.DefaultEffort) {
		listed.DefaultEffort = m.DefaultEffort
	}
	// A list of the request parameters a model supports says, by leaving
	// one out, that the model does not take it.
	if len(m.SupportedParameters) > 0 {
		tools := slices.Contains(m.SupportedParameters, "tools")
		temperature := slices.Contains(m.SupportedParameters, "temperature")
		listed.Tools, listed.Temperature = &tools, &temperature
		if slices.Contains(m.SupportedParameters, "reasoning") || slices.Contains(m.SupportedParameters, "reasoning_effort") {
			reasons := true
			listed.Reasons = &reasons
		}
	}
	var flags struct {
		Tools       *bool `json:"tools"`
		Temperature *bool `json:"temperature"`
		Reasoning   *bool `json:"reasoning"`
	}
	if json.Unmarshal(m.Capabilities, &flags) == nil {
		listed.Tools = cmpOr(flags.Tools, listed.Tools)
		listed.Temperature = cmpOr(flags.Temperature, listed.Temperature)
		listed.Reasons = cmpOr(flags.Reasoning, listed.Reasons)
	}
	return listed
}

// cmpOr is the first of the two that is set.
func cmpOr(first, second *bool) *bool {
	if first != nil {
		return first
	}
	return second
}

// price reads two prices from pricing, each a number or a numeric string,
// times scale.
func price(pricing map[string]json.RawMessage, inKey, outKey string, scale float64) (in, out float64, ok bool) {
	read := func(key string) (float64, bool) {
		raw, present := pricing[key]
		if !present {
			return 0, false
		}
		var number float64
		if json.Unmarshal(raw, &number) == nil {
			return number * scale, true
		}
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		number, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			return 0, false
		}
		return number * scale, true
	}
	in, inOK := read(inKey)
	out, outOK := read(outKey)
	return in, out, inOK && outOK
}

// listAnthropic follows the Anthropic listing's pages.
func listAnthropic(ctx context.Context, client *http.Client, p Provider) ([]Listed, error) {
	base := strings.TrimRight(p.Endpoint.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	var listed []Listed
	after := ""
	for range listPages {
		query := url.Values{"limit": {"1000"}}
		if after != "" {
			query.Set("after_id", after)
		}
		raw, err := getListing(ctx, client, p, base+"/models?"+query.Encode())
		if err != nil {
			return nil, err
		}
		var page struct {
			Data    []anthropicModel `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errors.New("the model list is not JSON in the expected shape")
		}
		for _, model := range page.Data {
			if model.ID != "" {
				listed = append(listed, model.listed())
			}
		}
		if !page.HasMore || page.LastID == "" || page.LastID == after {
			return sortListed(listed), nil
		}
		after = page.LastID
	}
	return sortListed(listed), nil
}

// supported is Anthropic's capability flag.
type supported struct {
	Supported bool `json:"supported"`
}

// anthropicModel is one entry of Anthropic's listing.
type anthropicModel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
	Lifecycle      string `json:"lifecycle"`
	RetiresAt      string `json:"retires_at"`
	Capabilities   *struct {
		ImageInput supported            `json:"image_input"`
		Effort     map[string]supported `json:"effort"`
		Thinking   struct {
			Types map[string]supported `json:"types"`
		} `json:"thinking"`
	} `json:"capabilities"`
}

func (m anthropicModel) listed() Listed {
	listed := Listed{ID: m.ID, Name: m.DisplayName, ContextTokens: m.MaxInputTokens, OutputTokens: m.MaxTokens}
	if m.Capabilities != nil {
		images := m.Capabilities.ImageInput.Supported
		listed.Images = &images
		// Ask sends "none" as thinking turned off, and any other effort as
		// adaptive thinking at that effort.
		if m.Capabilities.Thinking.Types["disabled"].Supported {
			listed.Efforts = append(listed.Efforts, "none")
		}
		if m.Capabilities.Effort["supported"].Supported || len(m.Capabilities.Effort) > 0 {
			for _, effort := range ReasoningEfforts {
				if effort != "none" && m.Capabilities.Effort[effort].Supported {
					listed.Efforts = append(listed.Efforts, effort)
				}
			}
		}
	}
	listed.Deprecated = m.Lifecycle == "deprecated" || m.Lifecycle == "retired"
	listed.Retires = day(m.RetiresAt)
	return listed
}

// UnmarshalJSON reads Anthropic's effort object, whose "supported" key is a
// bool beside the per-level objects.
func (s *supported) UnmarshalJSON(raw []byte) error {
	var asBool bool
	if json.Unmarshal(raw, &asBool) == nil {
		s.Supported = asBool
		return nil
	}
	var object struct {
		Supported bool `json:"supported"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	s.Supported = object.Supported
	return nil
}

// getListing GETs one listing page with the provider's key.
func getListing(ctx context.Context, client *http.Client, p Provider, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("the base URL is not a valid address")
	}
	req.Header.Set("Accept", "application/json")
	if p.Endpoint.APIKey != "" {
		if p.Kind.Auth() == "x-api-key" {
			req.Header.Set("X-Api-Key", p.Endpoint.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+p.Endpoint.APIKey)
		}
	}
	if p.Kind == KindAnthropic {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	for name, values := range p.Endpoint.Headers {
		req.Header[name] = slices.Clone(values)
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirects.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("the provider did not answer in time")
		}
		return nil, errors.New("the provider could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, ErrNoListing
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the provider refused the key (HTTP %d)", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("the provider answered HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, listResponseLimit+1))
	if err != nil {
		return nil, errors.New("the model list could not be read")
	}
	if len(raw) > listResponseLimit {
		return nil, errors.New("the model list is too large")
	}
	return raw, nil
}

// day reduces an RFC 3339 time or a date to YYYY-MM-DD, or "".
func day(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		return t.Format(time.DateOnly)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sortListed(listed []Listed) []Listed {
	slices.SortFunc(listed, func(a, b Listed) int { return strings.Compare(a.ID, b.ID) })
	return slices.CompactFunc(listed, func(a, b Listed) bool { return a.ID == b.ID })
}
