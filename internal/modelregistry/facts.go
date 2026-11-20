package modelregistry

import (
	"encoding/json"
	"slices"
)

// Source is where a fact on a model entry came from. A fact with no source
// was stated by whoever wrote the entry into the settings. Where sources say
// different things, the earlier one here wins: an administrator's
// correction, then the provider's own listing, then the published catalog,
// then what a job's test found. Only a fact a test found is a test's to
// change.
type Source string

const (
	// SourceAdmin marks a fact an administrator set on the model's page.
	SourceAdmin Source = "admin"
	// SourceProvider marks a fact the provider's own model listing gave.
	SourceProvider Source = "provider"
	// SourceCatalog marks a fact the published catalog gave.
	SourceCatalog Source = "catalog"
	// SourceTest marks a fact a job's test found.
	SourceTest Source = "test"
)

// Sources lists every source, the one that wins first.
var Sources = []Source{SourceAdmin, SourceProvider, SourceCatalog, SourceTest}

// Title is what pages call the source.
func (s Source) Title() string {
	switch s {
	case SourceAdmin:
		return "set by an administrator"
	case SourceProvider:
		return "from the provider's list"
	case SourceCatalog:
		return "from the published catalog"
	case SourceTest:
		return "found by a job's test"
	}
	return "set when the model was added"
}

// The facts of a model entry that shape requests. Each is a key of
// Model.Sources.
const (
	FactProtocols      = "protocols"
	FactImages         = "images"
	FactEfforts        = "efforts"
	FactServiceTier    = "service_tier"
	FactCacheRetention = "prompt_cache_retention"
	FactTokenParam     = "token_param"
	FactTemperature    = "temperature"
	FactChatEffort     = "chat_effort"
	FactAllowance      = "allowance"
	FactExtraBody      = "extra_body"
)

// RequestFacts lists every fact of an entry, in the order pages show them.
var RequestFacts = []string{
	FactProtocols, FactImages, FactEfforts, FactServiceTier, FactCacheRetention,
	FactTokenParam, FactTemperature, FactChatEffort, FactAllowance, FactExtraBody,
}

// SourceOf is where the entry's fact came from; "" for one that was stated
// without saying by whom.
func (m Model) SourceOf(fact string) Source { return m.Sources[fact] }

// Learned reports whether a test found the fact.
func (m Model) Learned(fact string) bool { return m.Sources[fact] == SourceTest }

// fresh reports an entry nothing has been said or found about: a model that
// is being tested for its first job.
func (m Model) fresh() bool { return len(m.Protocols) == 0 && len(m.Sources) == 0 }

// learnable reports whether a test may set the fact: on a fresh entry, or
// when a test found the fact's present value. A fact that was stated or
// published is never a test's to change.
func (m Model) learnable(fact string) bool { return m.fresh() || m.Learned(fact) }

// WithSource returns the entry with fact recorded as source's. The map is
// copied: entries are values.
func (m Model) WithSource(fact string, source Source) Model {
	next := make(map[string]Source, len(m.Sources)+1)
	for key, value := range m.Sources {
		next[key] = value
	}
	next[fact] = source
	m.Sources = next
	return m
}

// learn records that a test found fact.
func (m *Model) learn(fact string) { *m = m.WithSource(fact, SourceTest) }

// NewEntry is the entry of a model nothing is known about yet: every fact is
// its tests' to find, until something published or an administrator says
// otherwise.
func NewEntry(provider, id string) Model {
	m := Model{Provider: provider, ID: id}
	for _, fact := range RequestFacts {
		m.learn(fact)
	}
	return m
}

// SameRequests reports whether two entries make every job send the same
// requests: they differ at most in which tests they passed, where their
// facts came from, and which catalog entry describes them.
func (m Model) SameRequests(other Model) bool {
	return m.requestKey() == other.requestKey()
}

// LearnedFacts lists the facts a test found, in page order.
func (m Model) LearnedFacts() []string {
	var facts []string
	for _, fact := range RequestFacts {
		if m.Learned(fact) {
			facts = append(facts, fact)
		}
	}
	return facts
}

// requestKey is the entry as its requests see it.
func (m Model) requestKey() string {
	m.TestedFor, m.Sources, m.Catalog = nil, nil, ""
	m.Protocols = slices.Clone(m.Protocols)
	raw, _ := json.Marshal(m)
	return string(raw)
}

// RequestKey identifies what the entry makes jobs send: two entries with the
// same key build the same clients.
func (m Model) RequestKey() string { return m.requestKey() }

// Facts is what one published source says about a model: the provider's own
// listing, or the catalog. A nil pointer, a zero number, and a nil slice are
// silence.
type Facts struct {
	// Images is whether it takes image input, Tools whether it calls tools,
	// and Temperature whether a request may carry one.
	Images, Tools, Temperature *bool
	// Reasons is whether it reasons before it answers. Efforts are the
	// efforts it then accepts, with "none" when its reasoning can be turned
	// off; nil when the source does not list them.
	Reasons *bool
	Efforts []string
	// ContextTokens and OutputTokens are its limits.
	ContextTokens, OutputTokens int
	// The prices are USD per million tokens.
	InputPrice, OutputPrice, CacheReadPrice *float64
	// Deprecated is the source marking it deprecated; Retires the date
	// (YYYY-MM-DD) it stops working, and Successor the model the source
	// names in its place.
	Deprecated         bool
	Retires, Successor string
	// Dimensions is the size of an embedding model's vectors.
	Dimensions int
}

// Facts is what the listing says about the model.
func (l Listed) Facts() Facts {
	return Facts{
		Images: l.Images, Tools: l.Tools, Temperature: l.Temperature, Reasons: l.Reasons, Efforts: l.Efforts,
		ContextTokens: l.ContextTokens, OutputTokens: l.OutputTokens,
		InputPrice: l.InputPrice, OutputPrice: l.OutputPrice,
		Deprecated: l.Deprecated, Retires: l.Retires,
	}
}

// Published is what is published about one model: by its provider's own
// listing, and by the catalog entry the model is matched to. The listing
// wins where both speak.
type Published struct {
	Provider Facts
	Catalog  Facts
	// Anthropic marks a model of Anthropic's, which a gateway serves on the
	// Messages protocol when it serves it natively: Ask's test tries that
	// protocol first.
	Anthropic bool
}

// pick returns the first of the provider's and the catalog's values that
// says something, and which it was.
func pick[T any](provider, catalog *T) (*T, Source) {
	switch {
	case provider != nil:
		return provider, SourceProvider
	case catalog != nil:
		return catalog, SourceCatalog
	}
	return nil, ""
}

// Images is whether the model takes images, and who says so.
func (p Published) Images() (*bool, Source) { return pick(p.Provider.Images, p.Catalog.Images) }

// Tools is whether the model calls tools, and who says so.
func (p Published) Tools() (*bool, Source) { return pick(p.Provider.Tools, p.Catalog.Tools) }

// Temperature is whether requests may carry a temperature, and who says so.
func (p Published) Temperature() (*bool, Source) {
	return pick(p.Provider.Temperature, p.Catalog.Temperature)
}

// Reasoning is whether the model reasons and the efforts it accepts, each
// from the first source that says, and the source of the efforts, or of
// whether it reasons when neither lists efforts. efforts is nil when no
// source lists them, and empty for a model that does not reason; source is
// "" when nothing is published about its reasoning.
func (p Published) Reasoning() (reasons bool, efforts []string, source Source) {
	switch {
	case p.Provider.Efforts != nil:
		efforts, source = p.Provider.Efforts, SourceProvider
	case p.Catalog.Efforts != nil:
		efforts, source = p.Catalog.Efforts, SourceCatalog
	}
	said, saidBy := pick(p.Provider.Reasons, p.Catalog.Reasons)
	switch {
	case said != nil:
		reasons = *said
	case efforts != nil:
		reasons = len(efforts) > 0
	default:
		return false, nil, ""
	}
	if source == "" {
		source = saidBy
	}
	if !reasons {
		return false, []string{}, source
	}
	return true, efforts, source
}

// Limits is the model's context and output limits, each from the first
// source that gives it.
func (p Published) Limits() (context, output int) {
	return firstNonZero(p.Provider.ContextTokens, p.Catalog.ContextTokens),
		firstNonZero(p.Provider.OutputTokens, p.Catalog.OutputTokens)
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// Price is the model's price per million input and output tokens, and who
// gives it; ok is false when neither does.
func (p Published) Price() (in, out float64, source Source, ok bool) {
	switch {
	case p.Provider.InputPrice != nil && p.Provider.OutputPrice != nil:
		return *p.Provider.InputPrice, *p.Provider.OutputPrice, SourceProvider, true
	case p.Catalog.InputPrice != nil && p.Catalog.OutputPrice != nil:
		return *p.Catalog.InputPrice, *p.Catalog.OutputPrice, SourceCatalog, true
	}
	return 0, 0, "", false
}

// End is when the model stops working and whether it is deprecated, from
// the provider's listing when it says either, otherwise the catalog.
func (p Published) End() (retires string, deprecated bool, successor string, source Source) {
	if p.Provider.Retires != "" || p.Provider.Deprecated {
		return p.Provider.Retires, p.Provider.Deprecated, p.Provider.Successor, SourceProvider
	}
	if p.Catalog.Retires != "" || p.Catalog.Deprecated {
		return p.Catalog.Retires, p.Catalog.Deprecated, p.Catalog.Successor, SourceCatalog
	}
	return "", false, "", ""
}

// QuietEffort is how a job that wants an answer and no reasoning (captions,
// document extraction, thread summaries) asks a reasoning model for the
// least of it: the effort to send, and whether the model still reasons, so
// that its output limit needs room for that.
func QuietEffort(efforts []string) (effort string, stillReasons bool) {
	for _, candidate := range ReasoningEfforts {
		if slices.Contains(efforts, candidate) {
			return candidate, candidate != "none"
		}
	}
	return "", true
}

// allowanceFor is the room a chat job's output limit gets for a model that
// reasons whatever it is asked: the default, or half the model's output
// limit when that is less.
func allowanceFor(outputTokens int) int {
	if outputTokens > 0 && outputTokens/2 < DefaultReasoningTokenAllowance {
		return outputTokens / 2
	}
	return DefaultReasoningTokenAllowance
}

// WithPublished returns the entry with what is published about the model
// put on it: each fact from the provider's listing when it says, otherwise
// from the catalog. A fact that was stated, by an administrator or by
// whoever wrote the entry, is left alone, and so is one nothing published
// speaks of, which a job's test finds. With all, the facts whoever wrote
// the entry stated are replaced too: an administrator asking for the
// published facts.
func (m Model) WithPublished(p Published, all bool) Model {
	open := func(fact string) bool {
		source := m.Sources[fact]
		return source != SourceAdmin && (source != "" || all || m.fresh())
	}
	set := func(fact string, source Source) { m = m.WithSource(fact, source) }
	if images, source := p.Images(); images != nil && open(FactImages) {
		m.Images = *images
		set(FactImages, source)
	}
	if temperature, source := p.Temperature(); temperature != nil && open(FactTemperature) {
		m.ChatOmitTemperature = !*temperature
		set(FactTemperature, source)
	}
	reasons, efforts, source := p.Reasoning()
	if source == "" {
		return m
	}
	if efforts != nil {
		efforts = slices.DeleteFunc(slices.Clone(efforts), func(effort string) bool { return !slices.Contains(ReasoningEfforts, effort) })
		if open(FactEfforts) {
			m.ReasoningEfforts = efforts
			set(FactEfforts, source)
		}
	}
	// What chat jobs send follows from how the model reasons. A model said
	// to reason whose efforts are not listed gets room for its reasoning
	// and no effort.
	quiet, stillReasons := "", reasons
	if reasons && efforts != nil {
		quiet, stillReasons = QuietEffort(efforts)
	}
	if _, clash := m.ChatExtraBody[chatEffortField]; open(FactChatEffort) && !clash {
		m.ChatReasoningEffort = quiet
		set(FactChatEffort, source)
	}
	if open(FactAllowance) {
		m.ChatReasoningTokenAllowance = 0
		if stillReasons {
			_, output := p.Limits()
			m.ChatReasoningTokenAllowance = allowanceFor(output)
		}
		set(FactAllowance, source)
	}
	return m
}
