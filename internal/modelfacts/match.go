package modelfacts

import (
	"regexp"
	"slices"
	"strings"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

// Query is one model a provider offers, and what hints at who serves it.
type Query struct {
	// ID is the model's ID as the provider lists it.
	ID string
	// Pinned is an administrator's choice: empty to match automatically,
	// modelregistry.CatalogNone for no match, or an entry's Ref.
	Pinned string
	// Host is the host of the provider's base URL. A provider's own API
	// (api.openai.com) names its namespace; a gateway's host names none.
	Host string
	// Anthropic marks a provider of the Anthropic kind.
	Anthropic bool
	// Owners is the provider's listing: each listed ID and who the listing
	// says owns it (empty when it does not say).
	Owners map[string]string
}

// Match is what the catalog has for a model.
type Match struct {
	// Entry is the catalog entry that describes the model; Found is false
	// when none does for certain, and nothing of Entry is then used.
	Entry Entry
	Found bool
	// Pinned reports an administrator's choice, and Cleared their choice of
	// no match. Missing is a pinned entry the catalog no longer has.
	Pinned, Cleared, Missing bool
	// How says, for the model's page, why Entry is the model.
	How string
	// ServedBy is false when Entry is the model's maker's and who serves it
	// behind the provider is not known: price and retirement are then the
	// maker's, and Elsewhere lists the same model's entries at the cloud
	// providers that are deprecated or have a retirement date.
	ServedBy  bool
	Elsewhere []Entry
	// Suggestions are entries that may be the model, for an administrator
	// to confirm, when none is certain.
	Suggestions []Entry
}

// maxSuggestions bounds the entries offered for confirmation.
const maxSuggestions = 6

// hosts are the API hosts of the providers the catalog gives no address
// for.
var hosts = map[string]string{
	"api.openai.com":                    "openai",
	"api.anthropic.com":                 "anthropic",
	"generativelanguage.googleapis.com": "google",
	"api.groq.com":                      "groq",
	"api.mistral.ai":                    "mistral",
	"api.together.xyz":                  "togetherai",
	"api.together.ai":                   "togetherai",
	"api.cerebras.ai":                   "cerebras",
	"api.x.ai":                          "xai",
	"api.cohere.com":                    "cohere",
	"api.cohere.ai":                     "cohere",
	"api.deepinfra.com":                 "deepinfra",
	"api.perplexity.ai":                 "perplexity",
}

// hostSuffixes are the same for providers whose host varies by account or
// region.
var hostSuffixes = map[string]string{
	".openai.azure.com":            "azure",
	"aiplatform.googleapis.com":    "google-vertex",
	".services.ai.azure.com":       "azure",
	".cognitiveservices.azure.com": "azure-cognitive-services",
}

// owners maps the names gateways and model IDs use for a provider to its
// namespace, where they are not the namespace itself.
var owners = map[string]string{
	"gcp.vertex_ai":    "google-vertex",
	"vertex":           "google-vertex",
	"vertexai":         "google-vertex",
	"vertex-ai":        "google-vertex",
	"aws.bedrock":      "amazon-bedrock",
	"bedrock":          "amazon-bedrock",
	"fireworks":        "fireworks-ai",
	"together":         "togetherai",
	"mistralai":        "mistral",
	"google-ai-studio": "google",
	"x-ai":             "xai",
	"azure-openai":     "azure",
}

// clouds are the providers that resell other makers' models on their own
// retirement schedule.
var clouds = []string{"google-vertex", "google-vertex-anthropic", "amazon-bedrock", "azure"}

// namespaceFor is the namespace a provider's name means, or "".
func (c *Catalog) namespaceFor(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	for _, candidate := range []string{owners[name], name, LiteLLMNamespace(name), strings.NewReplacer(".", "-", "_", "-").Replace(name)} {
		if candidate != "" && c.hasNamespace(candidate) {
			return candidate
		}
	}
	return ""
}

// hostNamespace is the namespace of the provider whose own API is at host,
// or "" for a gateway, a proxy, or a server Diderot does not know.
func (c *Catalog) hostNamespace(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if namespace, ok := c.hosts[host]; ok {
		return namespace
	}
	if namespace, ok := hosts[host]; ok && c.hasNamespace(namespace) {
		return namespace
	}
	if strings.HasPrefix(host, "bedrock-") && strings.HasSuffix(host, ".amazonaws.com") && c.hasNamespace("amazon-bedrock") {
		return "amazon-bedrock"
	}
	for suffix, namespace := range hostSuffixes {
		if strings.HasSuffix(host, suffix) && c.hasNamespace(namespace) {
			return namespace
		}
	}
	return ""
}

// form is one reading of a model's ID: the ID the catalog would list, the
// namespace the reading names, and how the reading was made.
type form struct {
	id        string
	namespace string
	// sibling is the listed ID the reading takes its owner from: the ID
	// itself, or the ID without its route suffix.
	sibling string
	// certain is false for a reading that removed a suffix nothing vouches
	// for: a tag may name another size of the model.
	certain bool
	how     string
}

// forms reads q.ID every way a provider, gateway or proxy writes one.
func (c *Catalog) forms(q Query) []form {
	readings := []form{{id: q.ID, sibling: q.ID, certain: true}}
	// accounts/fireworks/models/x is Fireworks' own ID, wherever it is
	// served from.
	if strings.HasPrefix(q.ID, "accounts/fireworks/") && c.hasNamespace("fireworks-ai") {
		readings[0].namespace = "fireworks-ai"
	}
	prefixed := func(base form) (form, bool) {
		prefix, rest, ok := strings.Cut(base.id, "/")
		if !ok || rest == "" {
			return form{}, false
		}
		namespace := c.namespaceFor(prefix)
		if namespace == "" {
			return form{}, false
		}
		how := "its ID starts with " + prefix + "/"
		if base.how != "" {
			how = base.how + ", and " + how
		}
		return form{id: rest, namespace: namespace, sibling: base.sibling, certain: base.certain, how: how}, true
	}
	if reading, ok := prefixed(readings[0]); ok {
		readings = append(readings, reading)
	}
	if index := strings.LastIndex(q.ID, ":"); index > 0 && index < len(q.ID)-1 {
		base, suffix := q.ID[:index], q.ID[index+1:]
		_, listed := q.Owners[base]
		reading := form{id: base, sibling: base, namespace: c.namespaceFor(suffix), how: "without its route suffix :" + suffix}
		// The suffix is a route when it names a provider, or when the
		// provider lists the model without it too.
		reading.certain = reading.namespace != "" || listed
		readings = append(readings, reading)
		if inner, ok := prefixed(reading); ok {
			readings = append(readings, inner)
		}
	}
	return readings
}

// Match finds the catalog entry that describes q's model. A match is
// certain only when the model's ID, read as its provider writes it, is
// exactly an ID the catalog lists for the provider that serves it, or for
// its maker; anything looser is a suggestion.
func (c *Catalog) Match(q Query) Match {
	switch {
	case q.Pinned == modelregistry.CatalogNone:
		return Match{Pinned: true, Cleared: true, How: "An administrator set it to match nothing."}
	case q.Pinned != "":
		entry, ok := c.Lookup(q.Pinned)
		if !ok {
			return Match{Pinned: true, Missing: true, How: "An administrator matched it to " + q.Pinned + ", which the catalog does not list."}
		}
		return Match{Entry: entry, Found: true, Pinned: true, ServedBy: true, How: "Set by an administrator."}
	case c.Empty() || q.ID == "":
		return Match{}
	}
	readings := c.forms(q)

	// The provider's own API: every model is in its namespace.
	if namespace := c.hostNamespace(q.Host); namespace != "" {
		if entry, ok := c.Lookup(namespace + "/" + q.ID); ok {
			return Match{Entry: entry, Found: true, ServedBy: true, How: "The provider is " + c.NamespaceName(namespace) + ", which lists this ID."}
		}
		return Match{Suggestions: c.suggest(readings[:1], []string{namespace})}
	}

	// A gateway, a proxy, or a local server: the ID, or the listing, may
	// name who serves the model.
	var hinted []string
	for _, reading := range readings {
		// A reading that names who serves the model is not second-guessed
		// by who owns the model it routes to.
		owner := ""
		if reading.namespace == "" {
			owner = c.namespaceFor(q.Owners[reading.sibling])
		}
		for _, hint := range []struct{ namespace, why string }{
			{reading.namespace, ""},
			{owner, "the provider's list says " + q.Owners[reading.sibling] + " owns it"},
		} {
			if hint.namespace == "" {
				continue
			}
			if !slices.Contains(hinted, hint.namespace) {
				hinted = append(hinted, hint.namespace)
			}
			entry, ok := c.Lookup(hint.namespace + "/" + reading.id)
			if !ok || !reading.certain {
				continue
			}
			return Match{Entry: entry, Found: true, ServedBy: true, How: how(c.NamespaceName(hint.namespace)+" lists "+reading.id, reading.how, hint.why)}
		}
	}
	if q.Anthropic && !slices.Contains(hinted, "anthropic") && c.hasNamespace("anthropic") {
		if entry, ok := c.Lookup("anthropic/" + q.ID); ok {
			return Match{Entry: entry, Found: true, ServedBy: true, How: "Anthropic lists this ID, and the provider speaks Anthropic's API."}
		}
		hinted = append(hinted, "anthropic")
	}

	// Nothing names who serves it: the model's maker's own entry is still
	// the model, though its price and retirement are the maker's.
	if len(hinted) == 0 {
		for _, reading := range readings {
			if !reading.certain {
				continue
			}
			entry, ok := c.makers(reading.id)
			if !ok {
				continue
			}
			match := Match{Entry: entry, Found: true, How: how(c.NamespaceName(entry.Namespace)+", its maker, lists "+reading.id, reading.how,
				"nothing says who serves it here, so its price and retirement are the maker's")}
			for _, cloud := range clouds {
				for _, index := range c.byCanon[c.canonical(entry)] {
					if other := c.entries[index]; other.Namespace == cloud && (other.Deprecated || other.Retires != "") {
						match.Elsewhere = append(match.Elsewhere, other)
					}
				}
			}
			return match
		}
	}
	return Match{Suggestions: c.suggest(readings, hinted)}
}

// how joins the reasons for a match into a sentence.
func how(parts ...string) string {
	parts = slices.DeleteFunc(parts, func(part string) bool { return part == "" })
	sentence := strings.Join(parts, "; ")
	if sentence == "" {
		return ""
	}
	return strings.ToUpper(sentence[:1]) + sentence[1:] + "."
}

// canonical is the name of entry's model whoever serves it: what the catalog
// gives, or the entry's own name when it is a maker's.
func (c *Catalog) canonical(entry Entry) string {
	if entry.Canonical != "" {
		return entry.Canonical
	}
	return entry.Ref()
}

// canonicalOf is the one model the catalog's entries with exactly this ID
// are, or "" when they are several models or none says. Where a dated
// snapshot shares the ID, the model named exactly by the ID is the one.
func (c *Catalog) canonicalOf(id string) string {
	var names []string
	for _, index := range c.byID[id] {
		if name := c.entries[index].Canonical; name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	for _, name := range names {
		if _, own, _ := ParseRef(name); own == id {
			return name
		}
	}
	return ""
}

// makers returns the entry for id in the namespace of the model's maker.
func (c *Catalog) makers(id string) (Entry, bool) {
	name := c.canonicalOf(id)
	if name == "" {
		return Entry{}, false
	}
	namespace, own, _ := ParseRef(name)
	if own != id {
		return Entry{}, false
	}
	return c.Lookup(namespace + "/" + id)
}

var (
	looseDate    = regexp.MustCompile(`[-@](\d{8}|\d{4}-\d{2}-\d{2}|latest|default)$`)
	loosePoint   = regexp.MustCompile(`(\d)p(\d)`)
	looseVersion = regexp.MustCompile(`-v\d+(:\d+)?$`)
)

// loose reduces an ID to what survives the ways providers respell one: the
// last path segment, lower case, without a date or version suffix, with
// dots, and Fireworks' "p" for a dot, as dashes.
func loose(id string) string {
	id = strings.ToLower(id)
	if index := strings.LastIndex(id, "/"); index >= 0 {
		id = id[index+1:]
	}
	id = looseVersion.ReplaceAllString(id, "")
	id = looseDate.ReplaceAllString(id, "")
	id = loosePoint.ReplaceAllString(id, "$1-$2")
	return strings.NewReplacer(".", "-", "_", "-").Replace(id)
}

// suggest lists the entries that may be the model: in the hinted
// namespaces, those the catalog says are the same model as an entry with
// this ID, and those whose ID is the same loosely; with no hint, the makers'
// entries that are, then any with exactly this ID.
func (c *Catalog) suggest(readings []form, hinted []string) []Entry {
	var found []Entry
	add := func(index int) {
		entry := c.entries[index]
		if entry.Described && !slices.ContainsFunc(found, func(e Entry) bool { return e.Ref() == entry.Ref() }) {
			found = append(found, entry)
		}
	}
	inHint := func(index int) bool { return slices.Contains(hinted, c.entries[index].Namespace) }
	makers := func(index int) bool {
		entry := c.entries[index]
		return entry.Canonical == "" || strings.HasPrefix(entry.Canonical, entry.Namespace+"/")
	}
	for _, reading := range readings {
		if name := c.canonicalOf(reading.id); name != "" {
			for _, index := range c.byCanon[name] {
				if inHint(index) {
					add(index)
				}
			}
		}
		for _, index := range c.byLoose[loose(reading.id)] {
			if inHint(index) || len(hinted) == 0 && makers(index) {
				add(index)
			}
		}
	}
	if len(found) == 0 && len(hinted) == 0 {
		for _, reading := range readings {
			for _, index := range c.byID[reading.id] {
				add(index)
			}
		}
	}
	// The shortest ID first: a provider's plain model before its routers
	// and variants.
	slices.SortStableFunc(found, func(a, b Entry) int { return len(a.ID) - len(b.ID) })
	if len(found) > maxSuggestions {
		found = found[:maxSuggestions]
	}
	return found
}

// Facts is what the entry says about its model.
func (e Entry) Facts() modelregistry.Facts {
	facts := modelregistry.Facts{
		ContextTokens: e.ContextTokens, OutputTokens: e.OutputTokens,
		Deprecated: e.Deprecated, Retires: e.Retires, Successor: e.Successor, Dimensions: e.Dimensions,
	}
	if e.Priced {
		in, out := e.InputPrice, e.OutputPrice
		facts.InputPrice, facts.OutputPrice = &in, &out
		if e.CacheReadPriced {
			cached := e.CacheReadPrice
			facts.CacheReadPrice = &cached
		}
	}
	if !e.Described || e.Embedding {
		return facts
	}
	images, tools, reasons := e.Images, e.Tools, e.Reasons
	facts.Images, facts.Tools, facts.Reasons, facts.Temperature = &images, &tools, &reasons, e.Temperature
	facts.Efforts = []string{}
	if e.Reasons {
		// A model whose reasoning can be switched off takes "none", which
		// Diderot sends as that switch.
		if e.Toggle && !slices.Contains(e.Efforts, "none") {
			facts.Efforts = append(facts.Efforts, "none")
		}
		for _, effort := range e.Efforts {
			if slices.Contains(modelregistry.ReasoningEfforts, effort) {
				facts.Efforts = append(facts.Efforts, effort)
			}
		}
		// A reasoning model with no effort or switch listed (a token
		// budget only) has no control Diderot can send.
		if len(facts.Efforts) == 0 {
			facts.Efforts = nil
		}
	}
	return facts
}

// Anthropic reports a model of Anthropic's, whoever serves it.
func (e Entry) Anthropic() bool {
	return e.Namespace == "anthropic" || strings.HasPrefix(e.Canonical, "anthropic/")
}
