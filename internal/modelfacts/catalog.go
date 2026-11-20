// Package modelfacts reads what is published about inference models, so
// Diderot learns a model's facts by looking them up instead of by sending it
// requests: whether it takes images, calls tools, accepts a temperature, how
// its reasoning is turned down, its limits and price, and when it is
// retired.
//
// Two published lists are read. models.dev (https://models.dev, MIT) is the
// catalog: every fact but two. LiteLLM's model list
// (https://github.com/BerriAI/litellm, MIT) adds the two models.dev has no
// field for: the date a provider retires a model, and the size of an
// embedding model's vectors. Each list is fetched on its own and kept as it
// was last read, so one failing never empties the other.
//
// A Catalog is the two merged. Match finds the entry for a model a provider
// offers; it never guesses: a model whose ID is not exactly a listed one is a
// suggestion an administrator confirms.
package modelfacts

import (
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The published lists.
const (
	SourceModelsDev = "models.dev"
	SourceLiteLLM   = "LiteLLM"
)

// Entry is one model as one provider serves it. Namespace is the catalog's
// name for the provider (openai, anthropic, google-vertex, fireworks-ai).
type Entry struct {
	Namespace string `json:"ns"`
	ID        string `json:"id"`
	// Canonical names the model whoever serves it (openai/gpt-4o), when the
	// catalog says.
	Canonical string `json:"canonical,omitempty"`
	Name      string `json:"name,omitempty"`
	// Embedding marks an embedding model.
	Embedding bool `json:"embedding,omitempty"`
	// Described is set for an entry models.dev describes; an entry without
	// it comes from LiteLLM alone and carries only what the fields below
	// hold.
	Described bool `json:"described,omitempty"`
	Images    bool `json:"images,omitempty"`
	Tools     bool `json:"tools,omitempty"`
	// Temperature is whether requests may carry one; nil when the catalog
	// does not say.
	Temperature *bool `json:"temperature,omitempty"`
	// Reasons marks a model that reasons before it answers; Efforts are the
	// effort levels it takes, and Toggle that its reasoning can be turned
	// off.
	Reasons bool     `json:"reasons,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
	Toggle  bool     `json:"toggle,omitempty"`
	// ContextTokens and OutputTokens are its limits.
	ContextTokens int `json:"context,omitempty"`
	OutputTokens  int `json:"output,omitempty"`
	// Priced is set when the prices, USD per million tokens, are known; a
	// free model is priced at zero.
	Priced          bool    `json:"priced,omitempty"`
	InputPrice      float64 `json:"input_price,omitempty"`
	OutputPrice     float64 `json:"output_price,omitempty"`
	CacheReadPrice  float64 `json:"cache_read_price,omitempty"`
	CacheReadPriced bool    `json:"cache_read_priced,omitempty"`
	// Deprecated is the catalog's status; Retires the date (YYYY-MM-DD) the
	// provider stops serving it and Successor what it names in its place,
	// both from LiteLLM.
	Deprecated bool   `json:"deprecated,omitempty"`
	Retires    string `json:"retires,omitempty"`
	Successor  string `json:"successor,omitempty"`
	// Dimensions is the size of an embedding model's vectors, from LiteLLM.
	Dimensions int `json:"dimensions,omitempty"`
}

// Ref is the entry's name on pages and in a saved match: namespace/id.
func (e Entry) Ref() string { return e.Namespace + "/" + e.ID }

// ParseRef splits a Ref at its first slash; an ID may contain more.
func ParseRef(ref string) (namespace, id string, ok bool) {
	namespace, id, ok = strings.Cut(ref, "/")
	return namespace, id, ok && namespace != "" && id != ""
}

// Namespace is one provider as the catalog knows it.
type Namespace struct {
	// Name is what the provider is called (Google Vertex AI).
	Name string `json:"name,omitempty"`
	// Host is the host of its API, when the catalog gives one.
	Host string `json:"host,omitempty"`
}

// List is one published list as it was last read, reduced to what Diderot
// uses.
type List struct {
	Source     string               `json:"source"`
	FetchedAt  time.Time            `json:"fetched_at"`
	Namespaces map[string]Namespace `json:"namespaces,omitempty"`
	Entries    []Entry              `json:"entries"`
}

// responseLimit bounds one list: models.dev is 7 MB and LiteLLM's 5 MB.
const responseLimit = 48 << 20

// maxText bounds a name or an ID taken from a published list: the lists are
// other people's text, shown on pages. An entry whose ID is longer is not a
// model anyone serves, and is dropped.
const maxText = 200

// clip shortens text from a published list to maxText.
func clip(text string) string {
	if len(text) > maxText {
		return strings.ToValidUTF8(text[:maxText], "")
	}
	return text
}

// ParseModelsDev reads models.dev's api.json: an object of providers, each
// with its models.
func ParseModelsDev(raw []byte) (List, error) {
	var doc map[string]struct {
		Name   string `json:"name"`
		API    string `json:"api"`
		Models map[string]struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Type      string `json:"type"`
			Canonical string `json:"canonical_model_id"`
			Status    string `json:"status"`
			ToolCall  bool   `json:"tool_call"`
			Reasoning bool   `json:"reasoning"`
			Temp      *bool  `json:"temperature"`
			Options   []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost *struct {
				Input     float64  `json:"input"`
				Output    float64  `json:"output"`
				CacheRead *float64 `json:"cache_read"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return List{}, errors.New("models.dev's list is not JSON in the expected shape")
	}
	list := List{Source: SourceModelsDev, Namespaces: map[string]Namespace{}}
	for namespace, provider := range doc {
		if namespace == "" || strings.Contains(namespace, "/") {
			continue
		}
		list.Namespaces[namespace] = Namespace{Name: clip(provider.Name), Host: publicHost(provider.API)}
		for key, model := range provider.Models {
			id := model.ID
			if id == "" {
				id = key
			}
			embedding := model.Type == "embedding"
			// Only models that answer in text, and embedding models, are
			// ones a job can use.
			if !embedding && (model.Type != "" && model.Type != "chat" || !slices.Contains(model.Modalities.Output, "text")) {
				continue
			}
			if len(id) > maxText || len(model.Canonical) > maxText {
				continue
			}
			entry := Entry{
				Namespace: namespace, ID: id, Canonical: model.Canonical, Name: clip(model.Name), Embedding: embedding,
				Described: true, Images: slices.Contains(model.Modalities.Input, "image"), Tools: model.ToolCall,
				Temperature: model.Temp, Reasons: model.Reasoning,
				ContextTokens: model.Limit.Context, OutputTokens: model.Limit.Output,
				Deprecated: strings.EqualFold(model.Status, "deprecated"),
			}
			if entry.Name == id {
				entry.Name = ""
			}
			if embedding {
				// An embedding model takes no temperature and writes no
				// tokens; the catalog's output "limit" for one is not a
				// token limit.
				entry.Temperature, entry.OutputTokens = nil, 0
			}
			for _, option := range model.Options {
				switch option.Type {
				case "toggle":
					entry.Toggle = true
				case "effort":
					for _, value := range option.Values {
						if value != "" && !slices.Contains(entry.Efforts, value) {
							entry.Efforts = append(entry.Efforts, value)
						}
					}
				}
			}
			if model.Cost != nil {
				entry.Priced, entry.InputPrice, entry.OutputPrice = true, model.Cost.Input, model.Cost.Output
				if model.Cost.CacheRead != nil {
					entry.CacheReadPriced, entry.CacheReadPrice = true, *model.Cost.CacheRead
				}
			}
			list.Entries = append(list.Entries, entry)
		}
	}
	if len(list.Entries) == 0 {
		return List{}, errors.New("models.dev's list has no models")
	}
	sortEntries(list.Entries)
	return list, nil
}

// ParseLiteLLM reads LiteLLM's model_prices_and_context_window.json, keeping
// the entries that say when a model is retired or how large an embedding
// model's vectors are.
func ParseLiteLLM(raw []byte) (List, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return List{}, errors.New("LiteLLM's list is not JSON in the expected shape")
	}
	list := List{Source: SourceLiteLLM}
	for key, rawModel := range doc {
		var model struct {
			Provider   string  `json:"litellm_provider"`
			Mode       string  `json:"mode"`
			Retires    string  `json:"deprecation_date"`
			Successor  string  `json:"successor_model"`
			Dimensions int     `json:"output_vector_size"`
			MaxInput   int     `json:"max_input_tokens"`
			InputCost  float64 `json:"input_cost_per_token"`
		}
		// One entry of another shape (the list's own sample) is skipped.
		if json.Unmarshal(rawModel, &model) != nil || model.Provider == "" {
			continue
		}
		retires := day(model.Retires)
		embedding := model.Mode == "embedding"
		if retires == "" && (!embedding || model.Dimensions <= 0) {
			continue
		}
		entry := Entry{
			Namespace: LiteLLMNamespace(model.Provider), ID: withoutLiteLLMPrefix(key, model.Provider),
			Retires: retires, Embedding: embedding,
		}
		if len(entry.ID) > maxText {
			continue
		}
		if retires != "" {
			entry.Successor = clip(withoutLiteLLMPrefix(model.Successor, model.Provider))
		}
		if embedding {
			entry.Dimensions, entry.ContextTokens = model.Dimensions, model.MaxInput
			entry.Priced, entry.InputPrice = true, model.InputCost*1e6
		}
		list.Entries = append(list.Entries, entry)
	}
	if len(list.Entries) == 0 {
		return List{}, errors.New("LiteLLM's list has no retirement dates or embedding sizes")
	}
	sortEntries(list.Entries)
	return list, nil
}

// withoutLiteLLMPrefix removes the provider prefix LiteLLM puts before most
// model names (vertex_ai/gemini-pro for the provider vertex_ai or
// vertex_ai-language-models), leaving the provider's own ID.
func withoutLiteLLMPrefix(key, provider string) string {
	prefix, rest, ok := strings.Cut(key, "/")
	if !ok || rest == "" {
		return key
	}
	if prefix == provider || strings.HasPrefix(provider, prefix+"-") || strings.HasPrefix(provider, prefix+"_") ||
		LiteLLMNamespace(prefix) == LiteLLMNamespace(provider) {
		return rest
	}
	return key
}

func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// liteLLMNamespaces maps LiteLLM's provider names to the catalog's where
// they differ by more than an underscore.
var liteLLMNamespaces = map[string]string{
	"gemini":                     "google",
	"vertex_ai":                  "google-vertex",
	"vertex_ai-language-models":  "google-vertex",
	"vertex_ai-text-models":      "google-vertex",
	"vertex_ai-embedding-models": "google-vertex",
	"vertex_ai-anthropic_models": "google-vertex-anthropic",
	"bedrock":                    "amazon-bedrock",
	"bedrock_converse":           "amazon-bedrock",
	"bedrock_mantle":             "amazon-bedrock",
	"azure_ai":                   "azure",
	"azure_text":                 "azure",
	"cohere_chat":                "cohere",
	"together_ai":                "togetherai",
	"text-completion-openai":     "openai",
	"moonshot":                   "moonshotai",
	"nvidia_nim":                 "nvidia",
	"vercel_ai_gateway":          "vercel",
	"cloudflare":                 "cloudflare-workers-ai",
	"friendliai":                 "friendli",
	"novita":                     "novita-ai",
}

// LiteLLMNamespace is the catalog's namespace for one of LiteLLM's provider
// names, which model IDs behind a LiteLLM proxy also start with.
func LiteLLMNamespace(provider string) string {
	if namespace, ok := liteLLMNamespaces[provider]; ok {
		return namespace
	}
	return strings.ReplaceAll(provider, "_", "-")
}

// publicHost is the host of a provider's API address, or "" for none or for
// an address on the machine or a private network, which says nothing about
// who is behind it.
func publicHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil && (addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified()) {
		return ""
	}
	if host == "localhost" || !strings.Contains(host, ".") || strings.Contains(host, "$") || strings.Contains(host, "{") {
		return ""
	}
	return host
}

// day reduces an RFC 3339 time or a date to YYYY-MM-DD, or "".
func day(raw string) string {
	raw = strings.TrimSpace(raw)
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		return t.Format(time.DateOnly)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	return ""
}

// Catalog is the published lists merged, ready to match models against. The
// zero Catalog has no entries and matches nothing.
type Catalog struct {
	// Lists says when each list in the catalog was read.
	Lists      map[string]time.Time
	namespaces map[string]Namespace
	entries    []Entry
	byKey      map[string]int
	byID       map[string][]int
	byCanon    map[string][]int
	byLoose    map[string][]int
	hosts      map[string]string
}

// Merge builds the catalog from the lists read: models.dev's entries, each
// with the retirement date and vector size LiteLLM gives the same
// provider's model, and LiteLLM's embedding models that models.dev lacks.
func Merge(lists ...List) *Catalog {
	c := &Catalog{
		Lists: map[string]time.Time{}, namespaces: map[string]Namespace{},
		byKey: map[string]int{}, byID: map[string][]int{}, byCanon: map[string][]int{}, byLoose: map[string][]int{},
		hosts: map[string]string{},
	}
	add := func(entry Entry) {
		c.byKey[entry.Ref()] = len(c.entries)
		c.entries = append(c.entries, entry)
	}
	for _, list := range lists {
		if list.Source != SourceModelsDev {
			continue
		}
		c.Lists[list.Source] = list.FetchedAt
		for name, namespace := range list.Namespaces {
			c.namespaces[name] = namespace
		}
		for _, entry := range list.Entries {
			if _, duplicate := c.byKey[entry.Ref()]; !duplicate {
				add(entry)
			}
		}
	}
	for _, list := range lists {
		if list.Source != SourceLiteLLM {
			continue
		}
		c.Lists[list.Source] = list.FetchedAt
		for _, extra := range list.Entries {
			index, known := c.byKey[extra.Ref()]
			switch {
			case known:
				entry := &c.entries[index]
				if extra.Retires != "" {
					entry.Retires, entry.Successor = extra.Retires, extra.Successor
				}
				if extra.Dimensions > 0 && entry.Embedding {
					entry.Dimensions = extra.Dimensions
				}
			case extra.Embedding && extra.Dimensions > 0:
				add(extra)
			}
		}
	}
	// Several namespaces may share an API host (a provider and its coding
	// plan): the host then names the shortest, the provider itself.
	for name, namespace := range c.namespaces {
		if namespace.Host == "" {
			continue
		}
		if current, taken := c.hosts[namespace.Host]; !taken || len(name) < len(current) || len(name) == len(current) && name < current {
			c.hosts[namespace.Host] = name
		}
	}
	for index, entry := range c.entries {
		c.byID[entry.ID] = append(c.byID[entry.ID], index)
		if entry.Canonical != "" {
			c.byCanon[entry.Canonical] = append(c.byCanon[entry.Canonical], index)
		}
		c.byLoose[loose(entry.ID)] = append(c.byLoose[loose(entry.ID)], index)
	}
	return c
}

// Empty reports a catalog with nothing in it: none was fetched, or the
// fetch is turned off.
func (c *Catalog) Empty() bool { return c == nil || len(c.entries) == 0 }

// Len is how many models the catalog describes.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Lookup returns the entry namespace/id.
func (c *Catalog) Lookup(ref string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	index, ok := c.byKey[ref]
	if !ok {
		return Entry{}, false
	}
	return c.entries[index], true
}

// NamespaceName is what a namespace's provider is called, or the namespace
// itself.
func (c *Catalog) NamespaceName(namespace string) string {
	if c != nil {
		if name := c.namespaces[namespace].Name; name != "" {
			return name
		}
	}
	return namespace
}

func (c *Catalog) hasNamespace(namespace string) bool {
	if c == nil {
		return false
	}
	if _, ok := c.namespaces[namespace]; ok {
		return true
	}
	return false
}
