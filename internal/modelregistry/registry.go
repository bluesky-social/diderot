// Package modelregistry describes the inference providers and models Diderot
// may call, as administrators set them up on the Models page: where each
// provider is and how its key is sent, and for each model chosen for a job,
// the wire protocols it answers on, the reasoning efforts and service tier it
// accepts, and the per-model request fields its protocol needs.
//
// Providers are settings rows (internal/modelprovider); models are entries
// of the models row (settings.Models.Available), written when a job is given
// a model and the model passes that job's test (TestForRole). A Registry is
// one consistent view of both, which every role's client is built from.
//
// The endpoint follows the model: each model names one provider, and
// requests for it go to that provider's base URL with that provider's key. A
// key is never sent anywhere else.
package modelregistry

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// Protocol is a wire protocol an inference endpoint serves a model on.
type Protocol string

const (
	// ProtocolResponses is the OpenAI Responses API (POST {base}/responses).
	ProtocolResponses Protocol = "responses"
	// ProtocolMessages is the Anthropic Messages API (POST {base}/v1/messages).
	ProtocolMessages Protocol = "anthropic-messages"
	// ProtocolChat is the OpenAI Chat Completions API
	// (POST {base}/chat/completions).
	ProtocolChat Protocol = "chat"
	// ProtocolEmbeddings is the OpenAI Embeddings API (POST {base}/embeddings).
	// Only the deployment's embedding endpoint speaks it: no model entry
	// lists it.
	ProtocolEmbeddings Protocol = "embeddings"
)

// ReasoningEfforts are the effort names any model entry may list.
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ServiceTiers are the Responses service tiers Diderot accepts. Fast and
// priority are equivalent on supported OpenAI routes.
var ServiceTiers = []string{"default", "fast", "priority"}

// PromptCacheRetentions are the Responses prompt_cache_retention values.
var PromptCacheRetentions = []string{"in_memory", "24h"}

// Kind is a provider's protocol family: how its models are listed and how
// its key is sent.
type Kind string

const (
	// KindOpenAI is an OpenAI-compatible server: OpenAI itself, a gateway, a
	// proxy, or a local server. It lists models at GET {base}/models and
	// takes a bearer key. A gateway may serve Messages too.
	KindOpenAI Kind = "openai"
	// KindAnthropic is the Anthropic API: models at GET {base}/v1/models,
	// the key as x-api-key, Messages only.
	KindAnthropic Kind = "anthropic"
)

// Kinds lists every kind, in the order pages offer them.
var Kinds = []Kind{KindOpenAI, KindAnthropic}

// Title is what pages call the kind.
func (k Kind) Title() string {
	if k == KindAnthropic {
		return "Anthropic"
	}
	return "OpenAI-compatible"
}

// Protocols are the protocols a model of the kind may answer on.
func (k Kind) Protocols() []Protocol {
	if k == KindAnthropic {
		return []Protocol{ProtocolMessages}
	}
	return []Protocol{ProtocolResponses, ProtocolChat, ProtocolMessages}
}

// Auth is how the kind sends its key.
func (k Kind) Auth() llmclient.AuthStyle {
	if k == KindAnthropic {
		return llmclient.AuthXAPIKey
	}
	return llmclient.AuthBearer
}

// Role is a job an administrator chooses a model for on the Models page.
// Embeddings are not a role: their model is tied to the vector collection
// and set in the deployment.
type Role string

const (
	RoleAsk                  Role = "ask"
	RoleTitles               Role = "titles"
	RoleSessionSummaryMap    Role = "session_summary_map"
	RoleSessionSummaryReduce Role = "session_summary_reduce"
	RoleImageCaptions        Role = "image_captions"
	RoleDocumentExtraction   Role = "document_extraction"
	RoleThreadSummaries      Role = "thread_summaries"
)

// Roles lists every role in the order pages show them.
var Roles = []Role{
	RoleAsk, RoleTitles, RoleSessionSummaryMap, RoleSessionSummaryReduce,
	RoleImageCaptions, RoleDocumentExtraction, RoleThreadSummaries,
}

// Provider is one inference endpoint and the key presented to it.
type Provider struct {
	// Name is what model entries reference.
	Name string
	Kind Kind
	// Endpoint carries the base URL, the key, the kind's auth style, and
	// the provider's extra request headers.
	Endpoint llmclient.Endpoint
	// Keyless marks a server that takes no key, which an administrator
	// says on the provider's page: requests carry no credential.
	Keyless bool
	// Fault says why the provider's settings can't be used as saved ("has
	// request headers that can't be read"); empty when they can. A provider
	// with one is sent nothing, rather than requests that lack what its
	// administrator set.
	Fault string
	// Version is the provider's settings version: a change rebuilds every
	// client of its models.
	Version int64
}

// AllowsNoKey reports whether a provider of the kind may be one that takes
// no key: an OpenAI-compatible server may be a local model server, or an
// embedder beside Diderot. Anthropic's API always takes one.
func (k Kind) AllowsNoKey() bool { return k == KindOpenAI }

// Unusable says why requests can't be sent to the provider ("has no key"),
// or is empty when they can. A provider needs its key unless it is marked
// as taking none, so that a key left out of the deployment is a fault and
// not a silent change to requests without one.
func (p Provider) Unusable() string {
	switch {
	case p.Endpoint.BaseURL == "":
		return "has no address"
	case p.Fault != "":
		return p.Fault
	case !p.Keyless && p.Endpoint.APIKey == "":
		return "has no key"
	}
	return ""
}

// Format keeps the key out of every fmt verb.
func (p Provider) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "modelregistry.Provider{%s %s v%d}", p.Name, p.Kind, p.Version)
}

// Model is one model entry: a provider's model and what Diderot knows about
// calling it, detected by its job tests or set by an administrator.
type Model struct {
	// Provider names the provider that serves this ID.
	Provider string `json:"provider"`
	// ID is the exact model name sent in the request body.
	ID string `json:"id"`
	// Protocols lists the wire protocols the model answers on. RoleProtocol
	// says which of them each role uses.
	Protocols []Protocol `json:"protocols,omitempty"`
	// ReasoningEfforts lists the accepted efforts. Empty means the model
	// takes no effort and every role leaves the field off.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// ServiceTier reports whether Responses requests may carry service_tier.
	ServiceTier bool `json:"service_tier,omitempty"`
	// PromptCacheRetention is sent as Responses prompt_cache_retention when
	// non-empty.
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`
	// Images reports that the model accepts images on Chat Completions,
	// which captions and PDF pages send.
	Images bool `json:"images,omitempty"`
	// ChatTokenParam is the chat-completions field that carries the output
	// limit of captions, document extraction, thread summaries, and session
	// summaries on chat: empty or
	// max_tokens, or max_completion_tokens, which OpenAI's reasoning models
	// require.
	ChatTokenParam llmclient.ChatTokenParam `json:"chat_token_param,omitempty"`
	// ChatReasoningTokenAllowance is added to the fixed output limit of
	// captions, document extraction, thread summaries, and session summaries
	// on chat, for a model whose limit also counts reasoning tokens.
	ChatReasoningTokenAllowance int `json:"chat_reasoning_token_allowance,omitempty"`
	// ChatOmitTemperature leaves temperature off chat-completions requests,
	// for models that reject the zero temperature captions send.
	ChatOmitTemperature bool `json:"chat_omit_temperature,omitempty"`
	// ChatReasoningEffort is sent as reasoning_effort on chat-completions
	// requests: the least reasoning the model offers, for jobs that want an
	// answer and none of it. Empty sends no effort.
	ChatReasoningEffort string `json:"chat_reasoning_effort,omitempty"`
	// ChatExtraBody is merged into every chat-completions request for this
	// model, after the role's own fields. A key may not repeat one the role
	// sets.
	ChatExtraBody map[string]any `json:"chat_extra_body,omitempty"`
	// TestedFor lists the roles whose test the model has passed.
	TestedFor []Role `json:"tested_for,omitempty"`
	// Sources says which facts above a job's test found (SourceTest) and
	// which an administrator set on the model's page (SourceAdmin), by Fact
	// constant. A fact with no source was stated by whoever wrote the
	// entry. A test changes only what a test found.
	Sources map[string]Source `json:"sources,omitempty"`
	// Catalog is an administrator's choice of the published catalog entry
	// that describes the model: empty matches it automatically, CatalogNone
	// matches nothing, and anything else is the entry's namespace/id. It
	// changes what the pages show about the model, never a request.
	Catalog string `json:"catalog,omitempty"`
}

// CatalogNone is the Model.Catalog of a model an administrator says no
// published catalog entry describes.
const CatalogNone = "none"

// chatEffortField is the chat-completions field ChatReasoningEffort is sent
// as.
const chatEffortField = "reasoning_effort"

// ChatFields is what every chat-completions request for the model carries
// beyond the job's own fields: the entry's extra fields, and the effort for
// jobs that want no reasoning.
func (m Model) ChatFields() map[string]any {
	if m.ChatReasoningEffort == "" {
		return m.ChatExtraBody
	}
	fields := make(map[string]any, len(m.ChatExtraBody)+1)
	for key, value := range m.ChatExtraBody {
		fields[key] = value
	}
	fields[chatEffortField] = m.ChatReasoningEffort
	return fields
}

// Ref names a provider's model: the provider's name, a slash, and the ID.
// Provider names have no slash, so the first one splits it.
func Ref(provider, id string) string { return provider + "/" + id }

// ParseRef splits a Ref.
func ParseRef(ref string) (provider, id string, ok bool) {
	provider, id, ok = strings.Cut(ref, "/")
	return provider, id, ok && provider != "" && id != ""
}

// Ref is the model's Ref.
func (m Model) Ref() string { return Ref(m.Provider, m.ID) }

// Supports reports whether p is listed for the model.
func (m Model) Supports(p Protocol) bool { return slices.Contains(m.Protocols, p) }

// SummaryProtocols are the protocols session summaries can call a model on,
// in the order they are chosen: the first one the model lists. Messages is
// first, so a model that lists it is called exactly as it was when Messages
// was the only one.
var SummaryProtocols = []Protocol{ProtocolMessages, ProtocolResponses, ProtocolChat}

// RoleProtocol returns the protocol role calls the model on, and false when
// the model lists none the role speaks. Ask drives the first agentic
// protocol listed, session summaries the first of SummaryProtocols listed,
// and every other role speaks one protocol.
func (m Model) RoleProtocol(role Role) (Protocol, bool) {
	switch role {
	case RoleAsk:
		return m.AgenticProtocol()
	case RoleSessionSummaryMap, RoleSessionSummaryReduce:
		for _, p := range SummaryProtocols {
			if m.Supports(p) {
				return p, true
			}
		}
		return "", false
	case RoleTitles:
		return ProtocolResponses, m.Supports(ProtocolResponses)
	}
	return ProtocolChat, m.Supports(ProtocolChat)
}

// NeedsImages reports whether a role sends images.
func NeedsImages(role Role) bool {
	return role == RoleImageCaptions || role == RoleDocumentExtraction
}

// ServesRole reports whether the model can serve role: it lists a protocol
// the role speaks (RoleProtocol), and accepts images for captions and PDF
// pages, which send them.
func (m Model) ServesRole(role Role) bool {
	if !slices.Contains(Roles, role) || NeedsImages(role) && !m.Images {
		return false
	}
	_, ok := m.RoleProtocol(role)
	return ok
}

// AgenticProtocol returns the first listed protocol Ask can drive.
func (m Model) AgenticProtocol() (Protocol, bool) {
	for _, p := range m.Protocols {
		if p == ProtocolResponses || p == ProtocolMessages {
			return p, true
		}
	}
	return "", false
}

// SupportsReasoningEffort reports whether effort is listed for the model.
func (m Model) SupportsReasoningEffort(effort string) bool {
	return slices.Contains(m.ReasoningEfforts, effort)
}

// ValidateReasoningEffort accepts the empty effort (provider default) and any
// listed effort.
func (m Model) ValidateReasoningEffort(effort string) error {
	if effort == "" || m.SupportsReasoningEffort(effort) {
		return nil
	}
	if len(m.ReasoningEfforts) == 0 {
		return fmt.Errorf("model %q does not accept a reasoning effort; leave it empty for the provider default", m.ID)
	}
	return fmt.Errorf("model %q does not support reasoning effort %q; supported efforts: %s (or empty for provider default)",
		m.ID, effort, strings.Join(m.ReasoningEfforts, ", "))
}

// ValidateServiceTier accepts the empty tier (provider default) and, for
// models that allow one, any known tier.
func (m Model) ValidateServiceTier(tier string) error {
	if tier == "" {
		return nil
	}
	if !m.ServiceTier {
		return fmt.Errorf("model %q does not support a service tier", m.ID)
	}
	if !slices.Contains(ServiceTiers, tier) {
		return fmt.Errorf("service tier %q is not supported; supported tiers: %s (or empty for provider default)",
			tier, strings.Join(ServiceTiers, ", "))
	}
	return nil
}

// Validate checks an entry against the kind of its provider. Its messages
// are written for the Models page.
func (m Model) Validate(kind Kind) error {
	if m.ID == "" || strings.TrimSpace(m.ID) != m.ID || strings.ContainsAny(m.ID, "\r\n") {
		return errors.New("enter the model's ID, without spaces around it")
	}
	for index, protocol := range m.Protocols {
		if !slices.Contains(kind.Protocols(), protocol) {
			return fmt.Errorf("a %s provider does not serve the %s protocol", kind.Title(), protocol)
		}
		if slices.Contains(m.Protocols[:index], protocol) {
			return fmt.Errorf("the %s protocol is listed twice", protocol)
		}
	}
	for index, effort := range m.ReasoningEfforts {
		if !slices.Contains(ReasoningEfforts, effort) {
			return fmt.Errorf("%q is not a reasoning effort (use %s)", effort, strings.Join(ReasoningEfforts, ", "))
		}
		if slices.Contains(m.ReasoningEfforts[:index], effort) {
			return fmt.Errorf("reasoning effort %q is listed twice", effort)
		}
	}
	if m.ServiceTier && !m.Supports(ProtocolResponses) {
		return errors.New("only a model that answers on Responses takes a service tier")
	}
	if m.PromptCacheRetention != "" {
		if !slices.Contains(PromptCacheRetentions, m.PromptCacheRetention) {
			return fmt.Errorf("prompt cache retention must be one of %s", strings.Join(PromptCacheRetentions, ", "))
		}
		if !m.Supports(ProtocolResponses) {
			return errors.New("only a model that answers on Responses takes a prompt cache retention")
		}
	}
	if m.ChatReasoningTokenAllowance < 0 {
		return errors.New("the reasoning token allowance must not be negative")
	}
	if _, err := llmclient.ParseChatTokenParam(string(m.ChatTokenParam)); err != nil {
		return fmt.Errorf("the chat output limit field must be max_tokens or max_completion_tokens")
	}
	for _, reserved := range []string{"model", "messages", "stream"} {
		if _, ok := m.ChatExtraBody[reserved]; ok {
			return fmt.Errorf("extra chat fields must not set %q", reserved)
		}
	}
	if m.ChatReasoningEffort != "" {
		if !slices.Contains(ReasoningEfforts, m.ChatReasoningEffort) {
			return fmt.Errorf("%q is not a reasoning effort (use %s)", m.ChatReasoningEffort, strings.Join(ReasoningEfforts, ", "))
		}
		if _, ok := m.ChatExtraBody[chatEffortField]; ok {
			return fmt.Errorf("extra chat fields set %q, which the effort for chat jobs sets too: keep one", chatEffortField)
		}
	}
	for _, role := range m.TestedFor {
		if !slices.Contains(Roles, role) {
			return fmt.Errorf("unknown role %q", role)
		}
	}
	for fact, source := range m.Sources {
		if !slices.Contains(RequestFacts, fact) || !slices.Contains(Sources, source) {
			return fmt.Errorf("unknown fact source %q: %q", fact, source)
		}
	}
	return nil
}

// Registry is an immutable view of the enabled providers and the model
// entries.
type Registry struct {
	providers map[string]Provider
	models    map[string]Model
	// order is the entries' refs as they were given.
	order []string
}

// New builds a registry. providers are the enabled ones; an entry whose
// provider is not among them stays in the registry, and Endpoint reports
// it.
func New(providers []Provider, models []Model) *Registry {
	registry := &Registry{providers: map[string]Provider{}, models: map[string]Model{}}
	for _, provider := range providers {
		registry.providers[provider.Name] = provider
	}
	for _, model := range models {
		ref := model.Ref()
		if _, duplicate := registry.models[ref]; duplicate {
			continue
		}
		registry.models[ref] = model
		registry.order = append(registry.order, ref)
	}
	return registry
}

// Providers returns the enabled providers in name order.
func (r *Registry) Providers() []Provider {
	providers := make([]Provider, 0, len(r.providers))
	for _, provider := range r.providers {
		providers = append(providers, provider)
	}
	slices.SortFunc(providers, func(a, b Provider) int { return strings.Compare(a.Name, b.Name) })
	return providers
}

// Provider returns the named enabled provider.
func (r *Registry) Provider(name string) (Provider, bool) {
	provider, ok := r.providers[name]
	return provider, ok
}

// Models returns every entry, in the order they were given.
func (r *Registry) Models() []Model {
	models := make([]Model, 0, len(r.order))
	for _, ref := range r.order {
		models = append(models, r.models[ref])
	}
	return models
}

// Lookup returns the entry for ref.
func (r *Registry) Lookup(ref string) (Model, bool) {
	model, ok := r.models[ref]
	return model, ok
}

// Endpoint returns the endpoint of m's provider. It fails when the provider
// is off or removed, or has no key, so a role never builds a client that
// would send no credential.
func (r *Registry) Endpoint(m Model) (llmclient.Endpoint, error) {
	provider, ok := r.providers[m.Provider]
	if !ok {
		return llmclient.Endpoint{}, fmt.Errorf("its provider, %s, is off or removed", m.Provider)
	}
	if reason := provider.Unusable(); reason != "" {
		return llmclient.Endpoint{}, fmt.Errorf("its provider, %s, %s", m.Provider, reason)
	}
	return provider.Endpoint, nil
}
