package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/modelfacts"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/safehttp"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/worker"
)

// retirementLead is how long before a model's retirement the jobs that use
// it need attention: long enough to choose and test another, short enough
// that a date years away is not a standing alarm.
const retirementLead = 90 * 24 * time.Hour

// modelFactsInterval is how often the leader looks at whether a published
// list is due to be read again, the first time one interval after it starts
// leading. Reading happens once a day (and hourly after a failure); the look
// itself is one small query.
const modelFactsInterval = time.Minute

// modelFactsRunner keeps the published model facts current: the leader reads
// each list when it is due, unless an administrator turned that off. A list
// that can't be read keeps its last copy.
func modelFactsRunner(store *settings.Store, pool *pgxpool.Pool, logger *slog.Logger) worker.Runner {
	logger = logger.With("component", "model-facts")
	fetcher := modelfacts.Fetcher{HTTPClient: safehttp.NewClient(safehttp.Options{Timeout: 2 * time.Minute, CheckRedirect: safehttp.MaxRedirects(3)})}
	failing := map[string]string{}
	return worker.Runner{
		Name: "model-facts", Interval: modelFactsInterval, Logger: logger,
		Run: func(ctx context.Context) error {
			if store.Models().PublishedFactsOff {
				return nil
			}
			status, err := fetcher.Refresh(ctx, pool, false)
			if err != nil {
				return err
			}
			for source, list := range status {
				if list.Error != failing[source] {
					if list.Error != "" {
						logger.Warn("a published model list could not be read; the last copy is kept", "list", source, "why", list.Error, "last_read", list.FetchedAt)
					} else {
						logger.Info("a published model list is read again", "list", source, "models", list.Entries)
					}
					failing[source] = list.Error
				}
			}
			return nil
		},
	}
}

// modelAttentionInterval is how often the leader looks at whether a model in
// use needs attention.
const modelAttentionInterval = 10 * time.Minute

// modelAttentionRunner makes a model in use that needs attention (it is
// deprecated, about to be retired, or gone from its provider's list) hard
// to miss by whoever does not open the admin pages: the leader logs it when
// it starts or changes, and sets diderot_model_attention{job} to 1 for the
// job, for an alert to watch.
func modelAttentionRunner(catalog *modelCatalog, store *settings.Store, logger *slog.Logger) worker.Runner {
	logger = logger.With("component", "model-attention")
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "diderot_model_attention",
		Help: "1 while the model a job uses needs attention: it is deprecated, about to be retired, or no longer listed by its provider.",
	}, []string{"job"})
	if err := prometheus.DefaultRegisterer.Register(gauge); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			panic(err)
		}
		gauge = already.ExistingCollector.(*prometheus.GaugeVec)
	}
	said := map[string]string{}
	return worker.Runner{
		Name: "model-attention", Interval: modelAttentionInterval, BootTick: true, Logger: logger,
		Run: func(ctx context.Context) error {
			saved := store.Models().Models
			views, err := catalog.Roles(ctx, saved)
			if err != nil {
				return err
			}
			attention := map[string]string{}
			for _, view := range views {
				attention[view.Key] = view.Attention
			}
			if _, _, embedding, err := catalog.Embedding(ctx, saved); err == nil {
				attention["embeddings"] = embedding
			}
			for job, text := range attention {
				value := 0.0
				if text != "" {
					value = 1
				}
				gauge.WithLabelValues(job).Set(value)
				if text != said[job] {
					if text != "" {
						logger.Warn("a model in use needs attention", "job", job, "why", text)
					} else {
						logger.Info("a model in use no longer needs attention", "job", job)
					}
					said[job] = text
				}
			}
			return nil
		},
	}
}

// factsView is what is published about the providers' models at one moment:
// each provider's menu, and the catalog.
type factsView struct {
	catalog *modelfacts.Catalog
	status  modelfacts.Status
	off     bool
	menus   map[string]modelprovider.Menu
	// hosts is each provider's API host.
	hosts map[string]string
	// pins is each model entry's administrator-chosen catalog entry, by
	// ref.
	pins map[string]string
	now  time.Time
	// owners is each provider's listing as the IDs it lists and who it says
	// owns each, and found what of has already worked out, by ref.
	owners map[string]map[string]string
	found  map[string]publishedMatch
}

// publishedMatch is what is published about one model.
type publishedMatch struct {
	published modelregistry.Published
	match     modelfacts.Match
}

// facts reads what is published now, for saved's entries.
func (c *modelCatalog) facts(ctx context.Context, saved settings.Models, menus []modelprovider.Menu) (*factsView, error) {
	view := &factsView{
		catalog: modelfacts.Merge(), off: saved.PublishedFactsOff, menus: map[string]modelprovider.Menu{},
		hosts: map[string]string{}, pins: map[string]string{}, now: time.Now(),
	}
	if c.now != nil {
		view.now = c.now()
	}
	for _, menu := range menus {
		view.menus[menu.Name] = menu
	}
	if c.rows != nil {
		for _, row := range c.rows() {
			if parsed, err := url.Parse(row.Resolved.String(modelprovider.FieldBaseURL)); err == nil {
				view.hosts[modelprovider.Name(row.Row.Connector)] = parsed.Hostname()
			}
		}
	}
	for _, entry := range saved.Available {
		if entry.Catalog != "" {
			view.pins[entry.Ref()] = entry.Catalog
		}
	}
	if view.off || c.loadFacts == nil {
		return view, nil
	}
	var err error
	if view.catalog, view.status, err = c.loadFacts(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

// of returns what is published about provider's model id, and the catalog
// entry it is matched to.
func (f *factsView) of(provider, id string) (modelregistry.Published, modelfacts.Match) {
	ref := modelregistry.Ref(provider, id)
	if known, ok := f.found[ref]; ok {
		return known.published, known.match
	}
	menu := f.menus[provider]
	owners, ok := f.owners[provider]
	if !ok {
		owners = make(map[string]string, len(menu.Listing))
		for _, listed := range menu.Listing {
			owners[listed.ID] = listed.OwnedBy
		}
		if f.owners == nil {
			f.owners = map[string]map[string]string{}
		}
		f.owners[provider] = owners
	}
	listed, _ := menu.Listed(id)
	published := modelregistry.Published{Provider: listed.Facts()}
	match := f.catalog.Match(modelfacts.Query{
		ID: id, Pinned: f.pins[modelregistry.Ref(provider, id)], Host: f.hosts[provider],
		Anthropic: menu.Kind == modelregistry.KindAnthropic, Owners: owners,
	})
	if match.Found {
		published.Catalog = match.Entry.Facts()
		published.Anthropic = match.Entry.Anthropic()
	}
	if f.found == nil {
		f.found = map[string]publishedMatch{}
	}
	f.found[ref] = publishedMatch{published: published, match: match}
	return published, match
}

// when says how far day (YYYY-MM-DD) is from now, in words, and whether it
// is within lead.
func when(now time.Time, day string, lead time.Duration) (words string, soon bool) {
	date, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return "", false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := int(date.Sub(today).Hours() / 24)
	switch {
	case days < -1:
		words = strconv.Itoa(-days) + " days ago"
	case days == -1:
		words = "yesterday"
	case days == 0:
		words = "today"
	case days == 1:
		words = "tomorrow"
	default:
		words = "in " + strconv.Itoa(days) + " days"
	}
	return words, date.Sub(today) <= lead
}

// ending says what a job's administrator needs to know about the end of the
// model id: attention, when it is deprecated or retires within
// retirementLead (or already has), and otherwise note, a retirement further
// off. Both are empty for a model nothing says is ending.
func (f *factsView) ending(id string, published modelregistry.Published, match modelfacts.Match) (attention, note string) {
	retires, deprecated, successor, source := published.End()
	who := "its provider's list"
	if source == modelregistry.SourceCatalog {
		who = "the published list for " + f.catalog.NamespaceName(match.Entry.Namespace)
	}
	replacement := ""
	if successor != "" {
		replacement = " Its replacement there is " + successor + "."
	}
	words, soon := when(f.now, retires, retirementLead)
	switch {
	case retires != "" && soon:
		verb := "retires"
		if strings.HasSuffix(words, "ago") || words == "yesterday" {
			verb = "was retired"
		}
		attention = fmt.Sprintf("%s %s on %s (%s), %s says.%s Choose another model.", id, verb, retires, words, who, replacement)
	case deprecated && retires != "":
		// Deprecated is the provider saying to move, however far off the
		// date is.
		attention = fmt.Sprintf("%s is deprecated, %s says, and retires on %s (%s).%s Choose another model.", id, who, retires, words, replacement)
	case deprecated:
		attention = fmt.Sprintf("%s is deprecated, %s says, with no date given for its retirement.%s Choose another model.", id, who, replacement)
	case retires != "":
		note = "Retires on " + retires + ", " + who + " says."
	}
	if attention != "" {
		return attention, ""
	}
	// Nothing says who serves the model here, and a cloud that resells it
	// is ending it: if that cloud is behind this provider, the job stops
	// with it.
	for _, other := range match.Elsewhere {
		name := f.catalog.NamespaceName(other.Namespace)
		words, soon := when(f.now, other.Retires, retirementLead)
		switch {
		case other.Retires != "" && soon:
			return fmt.Sprintf("%s retires %s on %s (%s). If this provider serves it from there, choose another model before then; if not, say who serves it on the model's page.",
				name, id, other.Retires, words), ""
		case other.Deprecated && other.Retires != "":
			return fmt.Sprintf("%s lists %s as deprecated, and retires it on %s (%s). If this provider serves it from there, choose another model; if not, say who serves it on the model's page.",
				name, id, other.Retires, words), ""
		case other.Deprecated:
			return fmt.Sprintf("%s lists %s as deprecated. If this provider serves it from there, choose another model; if not, say who serves it on the model's page.", name, id), ""
		}
	}
	return "", note
}

// option fills in what is published about a model a job may be given.
func (f *factsView) option(option *setup.ModelOption) {
	published, match := f.of(option.Provider, option.ID)
	option.Images, _ = published.Images()
	if in, out, _, ok := published.Price(); ok {
		option.InputPrice, option.OutputPrice = &in, &out
	}
	option.Retires, option.Deprecated, _, _ = published.End()
	option.ContextTokens, _ = published.Limits()
	option.Dimensions = published.Catalog.Dimensions
	if match.Found {
		option.Matched = match.Entry.Ref()
	}
}

// FactsStatus says how the published model facts are doing, for the Models
// page.
func (c *modelCatalog) FactsStatus(ctx context.Context, saved settings.Models) (setup.FactsStatus, error) {
	view, err := c.facts(ctx, saved, nil)
	if err != nil {
		return setup.FactsStatus{}, err
	}
	status := setup.FactsStatus{Off: view.off, Models: view.catalog.Len()}
	for _, source := range []string{modelfacts.SourceModelsDev, modelfacts.SourceLiteLLM} {
		list, tried := view.status[source]
		if !tried {
			continue
		}
		status.Lists = append(status.Lists, setup.FactsList{Name: source, FetchedAt: list.FetchedAt, Error: list.Error, Models: list.Entries})
	}
	if since, source, reason, stale := view.status.StaleSince(view.now); stale && !view.off {
		status.Attention = fmt.Sprintf("The published model facts are out of date: %s has not been read since %s (%s). Prices, limits and retirement dates may be wrong. "+
			"If this server has no way out to the internet, turn the facts off on the Models page.", source, since.Format(time.DateOnly), reason)
	}
	return status, nil
}

// Published returns what is published about the model ref, with pin (when
// not nil) in place of the entry's own choice of catalog entry.
func (c *modelCatalog) Published(ctx context.Context, saved settings.Models, ref string, pin *string) (modelregistry.Published, error) {
	published, _, err := c.publishedOf(ctx, saved, ref, pin)
	return published, err
}

func (c *modelCatalog) publishedOf(ctx context.Context, saved settings.Models, ref string, pin *string) (modelregistry.Published, modelfacts.Match, error) {
	provider, id, ok := modelregistry.ParseRef(ref)
	if !ok {
		return modelregistry.Published{}, modelfacts.Match{}, fmt.Errorf("%w: %q is not a provider's model", settings.ErrInvalid, ref)
	}
	var menus []modelprovider.Menu
	if c.menus != nil {
		var err error
		if menus, err = c.menus(ctx); err != nil {
			return modelregistry.Published{}, modelfacts.Match{}, err
		}
	}
	view, err := c.facts(ctx, saved, menus)
	if err != nil {
		return modelregistry.Published{}, modelfacts.Match{}, err
	}
	if pin != nil {
		view.pins[ref] = *pin
	}
	published, match := view.of(provider, id)
	if pin != nil && *pin != "" && *pin != modelregistry.CatalogNone && !match.Found {
		return published, match, fmt.Errorf("%w: the published catalog has no entry %s", settings.ErrInvalid, *pin)
	}
	return published, match, nil
}

// Detail is what the model's page shows about ref's entry: each fact in
// use with where it came from, what is published about the model, and the
// catalog entry it is matched to.
func (c *modelCatalog) Detail(ctx context.Context, saved settings.Models, ref string) (setup.ModelDetail, error) {
	entry, ok := c.registry(saved).Lookup(ref)
	if !ok {
		return setup.ModelDetail{}, fmt.Errorf("%w: there is no model %s", settings.ErrInvalid, ref)
	}
	published, match, err := c.publishedOf(ctx, saved, ref, nil)
	if err != nil {
		return setup.ModelDetail{}, err
	}
	view, err := c.facts(ctx, saved, nil)
	if err != nil {
		return setup.ModelDetail{}, err
	}
	detail := setup.ModelDetail{Off: view.off, NoCatalog: view.catalog.Empty()}
	// What the entry would be with the published facts on it, to show
	// where they differ from what is sent now.
	fromPublished := entry.WithPublished(published, true)
	for _, fact := range modelregistry.RequestFacts {
		row := setup.FactRow{Key: fact, Label: factLabel(fact), Value: factValue(entry, fact), Source: entry.SourceOf(fact).Title()}
		if now := factValue(fromPublished, fact); now != row.Value && entry.SourceOf(fact) != modelregistry.SourceAdmin {
			row.Published, row.PublishedBy = now, fromPublished.SourceOf(fact).Title()
			detail.Differs = true
		}
		detail.Facts = append(detail.Facts, row)
	}
	said := func(source modelregistry.Source) string {
		if source == modelregistry.SourceCatalog {
			return "the published catalog"
		}
		return "the provider's list"
	}
	if tools, source := published.Tools(); tools != nil {
		detail.Info = append(detail.Info, setup.InfoRow{Label: "Calls tools", Value: yesNo(*tools), Source: said(source)})
	}
	if contextTokens, output := published.Limits(); contextTokens > 0 || output > 0 {
		var parts []string
		if contextTokens > 0 {
			parts = append(parts, strconv.Itoa(contextTokens)+" tokens of context")
		}
		if output > 0 {
			parts = append(parts, strconv.Itoa(output)+" tokens of output")
		}
		detail.Info = append(detail.Info, setup.InfoRow{Label: "Limits", Value: strings.Join(parts, ", ")})
	}
	if in, out, source, ok := published.Price(); ok {
		value := fmt.Sprintf("$%s in, $%s out per million tokens", trimFloat(in), trimFloat(out))
		if cached := published.Catalog.CacheReadPrice; cached != nil && source == modelregistry.SourceCatalog {
			value += fmt.Sprintf(" ($%s for input read from the cache)", trimFloat(*cached))
		}
		by := said(source)
		if source == modelregistry.SourceCatalog && !match.ServedBy {
			by += ", at its maker's own prices"
		}
		detail.Info = append(detail.Info, setup.InfoRow{Label: "Price", Value: value, Source: by})
	}
	if published.Catalog.Dimensions > 0 {
		detail.Info = append(detail.Info, setup.InfoRow{Label: "Vector size", Value: strconv.Itoa(published.Catalog.Dimensions), Source: "the published catalog"})
	}
	attention, note := view.ending(entry.ID, published, match)
	detail.Ending = attention
	if note != "" {
		detail.Info = append(detail.Info, setup.InfoRow{Label: "End", Value: note})
	}
	detail.Match = setup.MatchView{
		Pin: entry.Catalog, Pinned: match.Pinned, Cleared: match.Cleared, Missing: match.Missing, How: match.How,
	}
	if match.Found {
		detail.Match.Ref = match.Entry.Ref()
		detail.Match.Name = strings.TrimSpace(match.Entry.Name + " at " + view.catalog.NamespaceName(match.Entry.Namespace))
	}
	for _, suggestion := range match.Suggestions {
		detail.Match.Suggestions = append(detail.Match.Suggestions, setup.MatchSuggestion{
			Ref: suggestion.Ref(), Name: strings.TrimSpace(suggestion.Name + " at " + view.catalog.NamespaceName(suggestion.Namespace)),
		})
	}
	return detail, nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func trimFloat(value float64) string {
	text := strconv.FormatFloat(value, 'f', 4, 64)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "" {
		return "0"
	}
	return text
}

// factLabel is what the model's page calls a fact.
func factLabel(fact string) string {
	switch fact {
	case modelregistry.FactProtocols:
		return "Answers on"
	case modelregistry.FactImages:
		return "Accepts images"
	case modelregistry.FactEfforts:
		return "Reasoning efforts"
	case modelregistry.FactServiceTier:
		return "Takes a service tier"
	case modelregistry.FactCacheRetention:
		return "Prompt cache retention"
	case modelregistry.FactTokenParam:
		return "Chat output limit field"
	case modelregistry.FactTemperature:
		return "Chat requests carry a temperature"
	case modelregistry.FactChatEffort:
		return "Effort chat jobs ask for"
	case modelregistry.FactAllowance:
		return "Room for reasoning in chat output"
	case modelregistry.FactExtraBody:
		return "Extra request fields"
	}
	return fact
}

// factValue is an entry's fact in words. A fact that is absent never reads
// as a value the fact can have: "none" is a reasoning effort, so no effort
// is "not sent".
func factValue(m modelregistry.Model, fact string) string {
	none := func(text string) string {
		if text == "" {
			return "none"
		}
		return text
	}
	unsent := func(text string) string {
		if text == "" {
			return "not sent"
		}
		return text
	}
	switch fact {
	case modelregistry.FactProtocols:
		names := make([]string, len(m.Protocols))
		for i, protocol := range m.Protocols {
			names[i] = string(protocol)
		}
		return none(strings.Join(names, ", "))
	case modelregistry.FactImages:
		return yesNo(m.Images)
	case modelregistry.FactEfforts:
		if len(m.ReasoningEfforts) == 0 {
			return "takes none"
		}
		return strings.Join(m.ReasoningEfforts, ", ")
	case modelregistry.FactServiceTier:
		return yesNo(m.ServiceTier)
	case modelregistry.FactCacheRetention:
		return unsent(m.PromptCacheRetention)
	case modelregistry.FactTokenParam:
		if m.ChatTokenParam == "" {
			return "max_tokens"
		}
		return string(m.ChatTokenParam)
	case modelregistry.FactTemperature:
		return yesNo(!m.ChatOmitTemperature)
	case modelregistry.FactChatEffort:
		return unsent(m.ChatReasoningEffort)
	case modelregistry.FactAllowance:
		if m.ChatReasoningTokenAllowance == 0 {
			return "none"
		}
		return strconv.Itoa(m.ChatReasoningTokenAllowance) + " tokens"
	case modelregistry.FactExtraBody:
		if len(m.ChatExtraBody) == 0 {
			return "none"
		}
		return strconv.Itoa(len(m.ChatExtraBody)) + " set"
	}
	return ""
}
