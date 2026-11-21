package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/embedder"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelfacts"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
	"github.com/bluesky-social/diderot/internal/sessiontitle"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

// boundModel is an enabled role's model entry and the endpoint of the
// provider the entry names. It is zero when the role is off.
type boundModel struct {
	modelregistry.Model
	endpoint llmclient.Endpoint
}

// fingerprint is what the role's clients are built from: two equal
// fingerprints build the same clients. It is the provider's address and
// key and what the entry makes the role send, and nothing else: a save
// that changes none of them (the models a provider offers, which tests an
// entry has passed) restarts no work. It is empty for a role that is off.
func (b boundModel) fingerprint() string {
	if b.ID == "" {
		return ""
	}
	// The key and the extra headers are hashed together: neither is in the
	// fingerprint itself.
	credential := b.endpoint.APIKey
	names := slices.Sorted(maps.Keys(b.endpoint.Headers))
	for _, name := range names {
		credential += "\n" + name + ": " + strings.Join(b.endpoint.Headers[name], ",")
	}
	key := sha256.Sum256([]byte(credential))
	return b.Ref() + "@" + b.endpoint.BaseURL + "|" + string(b.endpoint.Auth) + "|" + hex.EncodeToString(key[:8]) + "#" + b.RequestKey()
}

// inferenceModels is the model every inference role uses under one set of
// saved choices and providers (modelCatalog.resolve). A role that is off has
// a zero model, and so does embed until an embedding model is chosen.
type inferenceModels struct {
	ask boundModel
	// askLimits is what Ask is built with beside its model.
	askLimits settings.AskLimits
	// title serves both Ask session titles and agent-session titles.
	title         boundModel
	summaryMap    boundModel
	summaryReduce boundModel
	caption       boundModel
	docExtract    boundModel
	summarizer    boundModel
	// embed is the embedding model search uses, and embedding its
	// collection.
	embed     boundModel
	embedding settings.Embedding
	// fill is the embedding model being embedded into its own collection,
	// filling, to replace embed; zero when none is.
	fill    boundModel
	filling settings.Embedding
}

// embedFingerprint is what search is built from.
func (m *inferenceModels) embedFingerprint() string {
	return vectorFingerprint(m.embed, m.embedding)
}

// vectorTarget is the embedding model and collection the leader's embedder
// writes to: the one being filled, or else the one search uses.
func (m *inferenceModels) vectorTarget() (boundModel, settings.Embedding) {
	if m.fill.ID != "" {
		return m.fill, m.filling
	}
	return m.embed, m.embedding
}

func vectorFingerprint(model boundModel, embedding settings.Embedding) string {
	if model.ID == "" {
		return ""
	}
	return model.fingerprint() + "|" + embedding.Collection + "|" + strconv.Itoa(embedding.Dimensions)
}

// role returns the model of a role the Models page chooses.
func (m *inferenceModels) role(role modelregistry.Role) *boundModel {
	switch role {
	case modelregistry.RoleAsk:
		return &m.ask
	case modelregistry.RoleTitles:
		return &m.title
	case modelregistry.RoleSessionSummaryMap:
		return &m.summaryMap
	case modelregistry.RoleSessionSummaryReduce:
		return &m.summaryReduce
	case modelregistry.RoleImageCaptions:
		return &m.caption
	case modelregistry.RoleDocumentExtraction:
		return &m.docExtract
	case modelregistry.RoleThreadSummaries:
		return &m.summarizer
	}
	panic("inference: unknown role " + string(role))
}

// fingerprints is each role's fingerprint; two equal maps mean the same
// clients.
func (m *inferenceModels) fingerprints() map[modelregistry.Role]string {
	prints := map[modelregistry.Role]string{}
	for _, role := range modelregistry.Roles {
		prints[role] = m.role(role).fingerprint()
	}
	return prints
}

func sessionSummariesEnabled(models *inferenceModels) bool { return models.summaryMap.ID != "" }
func sessionTitlesEnabled(models *inferenceModels) bool    { return models.title.ID != "" }
func captionsEnabled(models *inferenceModels) bool         { return models.caption.ID != "" }
func documentExtractionEnabled(models *inferenceModels) bool {
	return models.docExtract.ID != ""
}
func threadSummariesEnabled(models *inferenceModels) bool { return models.summarizer.ID != "" }

// roleInfo is how the Models page describes a role.
var roleInfo = map[modelregistry.Role]struct{ title, description string }{
	modelregistry.RoleAsk:                  {"Ask", "Answers questions in Slack, over the API, and over MCP."},
	modelregistry.RoleTitles:               {"Titles", "Writes short titles for Ask conversations and agent sessions."},
	modelregistry.RoleSessionSummaryMap:    {"Session summaries, part by part", "Summarizes each part of an agent session. A long session is read in parts."},
	modelregistry.RoleSessionSummaryReduce: {"Session summaries, whole session", "Combines the part summaries into the one summary that search finds."},
	modelregistry.RoleImageCaptions:        {"Image captions", "Describes images so screenshots are searchable."},
	modelregistry.RoleDocumentExtraction:   {"Document extraction", "Reads PDF pages that have no text layer. A weak model garbles scanned documents."},
	modelregistry.RoleThreadSummaries:      {"Thread summaries", "Writes the replies to Slack's Summarize thread shortcuts."},
}

// modelCatalog is what the server knows about models: which roles the
// deployment runs, fixed at boot, and the providers, model entries and
// embedding model, which administrators choose on the Models page and every
// replica's settings cache follows. resolve turns saved choices into models.
type modelCatalog struct {
	cfg *config.Config
	// unavailable says why the deployment does not run a role.
	unavailable map[modelregistry.Role]string
	// vectors opens the embedding model's collection, for choosing one.
	vectors *vectorStores

	// providers are the enabled providers, saved the saved models row, and
	// menus every provider's menu, as attach wires them to the settings
	// once they are loaded; nil until then. rows are every provider's
	// settings row, on or off.
	providers func() []modelregistry.Provider
	saved     func() settings.Models
	menus     func(context.Context) ([]modelprovider.Menu, error)
	rows      func() []settings.FamilyRow

	// hadVectors is set once chunks are known to have vectors: at startup
	// when the database has some, and when an embedding model is saved.
	hadVectors atomic.Bool
	// searchFault is why this replica can't open the embedding model's
	// collection, nil while it can.
	searchFault atomic.Pointer[string]
	// loadFacts reads the published model facts the leader keeps; nil
	// before attach, and then nothing is published.
	loadFacts func(context.Context) (*modelfacts.Catalog, modelfacts.Status, error)
	// now is the clock; nil is time.Now.
	now func() time.Time
	// trace, when set, is filled with what each job's test did.
	trace *modelregistry.TestTrace
}

// newModelCatalog works out which roles the deployment runs. Providers,
// models and the embedding model are read from the settings, which attach
// provides once they are loaded.
func newModelCatalog(cfg *config.Config, vectors *vectorStores) *modelCatalog {
	catalog := &modelCatalog{cfg: cfg, unavailable: map[modelregistry.Role]string{}, vectors: vectors}
	if !cfg.AskEnabled {
		catalog.unavailable[modelregistry.RoleAsk] = "Ask is off. Set DIDEROT_ASK_ENABLED=true in the deployment to turn it on."
	}
	if cfg.ImagesBucket == "" {
		off := "This needs file storage. Set DIDEROT_IMAGES_BUCKET in the deployment."
		catalog.unavailable[modelregistry.RoleImageCaptions], catalog.unavailable[modelregistry.RoleDocumentExtraction] = off, off
	}
	return catalog
}

// attach gives the catalog the settings it reads providers and model
// entries from, and the database the providers' listings are kept in.
func (c *modelCatalog) attach(store *settings.Store, pool *pgxpool.Pool) {
	c.providers = func() []modelregistry.Provider {
		var providers []modelregistry.Provider
		for _, row := range store.FamilyRows(modelprovider.Prefix) {
			if row.Row.Enabled && row.Err == nil {
				providers = append(providers, modelprovider.FromResolved(row.Resolved).Provider)
			}
		}
		return providers
	}
	c.saved = func() settings.Models { return store.Models().Models }
	c.rows = func() []settings.FamilyRow { return store.FamilyRows(modelprovider.Prefix) }
	c.loadFacts = modelfacts.NewReader(pool).Load
	c.menus = func(ctx context.Context) ([]modelprovider.Menu, error) {
		return modelprovider.ReadMenus(ctx, store, pool)
	}
}

// registry is the enabled providers, as this replica's settings cache last
// saw them, and saved's model entries.
func (c *modelCatalog) registry(saved settings.Models) *modelregistry.Registry {
	var providers []modelregistry.Provider
	if c.providers != nil {
		providers = c.providers()
	}
	return modelregistry.New(providers, saved.Available)
}

// roleModel is the model role uses under saved, or zero with the reason the
// saved model is not in use; zero with no reason when nothing is saved or
// the deployment does not run the role.
func (c *modelCatalog) roleModel(registry *modelregistry.Registry, saved settings.Models, role modelregistry.Role) (boundModel, string) {
	ref := saved.Roles[string(role)]
	if c.unavailable[role] != "" || ref == "" {
		return boundModel{}, ""
	}
	entry, ok := registry.Lookup(ref)
	if !ok || !entry.ServesRole(role) {
		return boundModel{}, "It has not passed this job's test. Save it for this job again to test it."
	}
	endpoint, err := registry.Endpoint(entry)
	if err != nil {
		return boundModel{}, "It can't be used: " + err.Error() + "."
	}
	bound := boundModel{Model: entry, endpoint: endpoint}
	if err := c.checkRoleClient(saved, role, bound); err != nil {
		return boundModel{}, "This job can't use it: " + err.Error() + "."
	}
	return bound, ""
}

// resolve is the models the roles use under saved.
func (c *modelCatalog) resolve(saved settings.Models) *inferenceModels {
	registry := c.registry(saved)
	models := &inferenceModels{askLimits: saved.AskLimits()}
	for _, role := range modelregistry.Roles {
		*models.role(role), _ = c.roleModel(registry, saved, role)
	}
	models.embed, _ = c.embeddingModel(registry, saved.Embedding)
	if models.embed.ID != "" {
		models.embedding = *saved.Embedding
	}
	models.fill, _ = c.embeddingModel(registry, saved.Filling)
	if models.fill.ID != "" {
		models.filling = *saved.Filling
	}
	if saved.Embedding != nil || saved.Filling != nil {
		c.hadVectors.Store(true)
	}
	return models
}

// embeddingModel is the embedding model e, or zero with the reason it is
// not in use; zero with no reason for nil.
func (c *modelCatalog) embeddingModel(registry *modelregistry.Registry, e *settings.Embedding) (boundModel, string) {
	if e == nil {
		return boundModel{}, ""
	}
	provider, ok := registry.Provider(e.Provider)
	if !ok {
		return boundModel{}, "Its provider, " + e.Provider + ", is off or removed, so search and the embedder are off."
	}
	if reason := provider.Unusable(); reason != "" {
		return boundModel{}, "Its provider, " + e.Provider + ", " + reason + ", so search and the embedder are off."
	}
	return boundModel{Model: modelregistry.Model{Provider: e.Provider, ID: e.Model}, endpoint: provider.Endpoint}, ""
}

// faults says why this deployment can't run the models as they are saved,
// one sentence each, for /readyz: a job's or the embedding's saved model
// whose provider is on but has no usable key, whose provider does not
// exist, or that has no usable entry; and jobs saved with no embedding
// model where chunks already have vectors.
//
// Each is a fault of the deployment or of the saved settings, such as a key
// variable left out or a models row an older version overwrote. None can be
// reached from the admin pages, which refuse the changes that would cause
// one (removing a provider or a key that is in use, an entry change that
// stops a job, a job's model before the embedding model): a replica that is
// not ready gets no requests, the admin pages' among them, so a state an
// administrator can choose must never be one. A provider turned off, a job
// given no model, and a server nobody has set up yet are not faults.
func (c *modelCatalog) faults(saved settings.Models) []string {
	rows := map[string]settings.FamilyRow{}
	if c.rows != nil {
		for _, row := range c.rows() {
			rows[modelprovider.Name(row.Row.Connector)] = row
		}
	}
	registry := c.registry(saved)
	var faults []string
	// provider says what is wrong with the provider serving ref, or "".
	provider := func(what, ref string) (string, bool) {
		name, _, ok := modelregistry.ParseRef(ref)
		if !ok {
			return what + " is saved as " + strconv.Quote(ref) + ", which names no provider's model: the models settings were written by an older version or by hand.", false
		}
		row, exists := rows[name]
		switch {
		case !exists:
			return what + " uses the provider " + name + ", which does not exist.", false
		case !row.Row.Enabled:
			return "", false
		case row.Err != nil:
			return what + " uses the provider " + name + ", whose saved key can't be read: the deployment's config keys may have changed.", false
		case modelprovider.FromResolved(row.Resolved).Provider.Fault != "":
			return what + " uses the provider " + name + ", which is on but " + modelprovider.FromResolved(row.Resolved).Provider.Fault + ": correct " +
				modelprovider.EnvName(name, modelprovider.FieldSecretHeaders) + " in the deployment (one Name: value per line).", false
		case modelprovider.FromResolved(row.Resolved).Provider.Unusable() != "":
			return what + " uses the provider " + name + ", which is on but has no key: set " +
				modelprovider.EnvName(name, modelprovider.FieldKey) + " in the deployment, or save a key on the provider's page.", false
		}
		return "", true
	}
	jobs := false
	for _, role := range modelregistry.Roles {
		ref := saved.Roles[string(role)]
		if ref == "" || c.unavailable[role] != "" {
			continue
		}
		jobs = true
		title := roleInfo[role].title
		fault, usable := provider(title, ref)
		if fault != "" {
			faults = append(faults, fault)
		}
		if !usable {
			continue
		}
		entry, ok := registry.Lookup(ref)
		switch {
		case !ok:
			faults = append(faults, title+" uses "+ref+", which the models settings have no entry for: they were overwritten by an older version or written by hand.")
		case !entry.ServesRole(role):
			faults = append(faults, title+" uses "+ref+", whose entry does not answer for that job. Save the model for the job again on the Models page to test it.")
		default:
			if _, warning := c.roleModel(registry, saved, role); warning != "" {
				faults = append(faults, title+" uses "+ref+". "+warning)
			}
		}
	}
	for what, embedding := range map[string]*settings.Embedding{"Search": saved.Embedding, "The embedding model being filled": saved.Filling} {
		if embedding == nil {
			continue
		}
		if fault, _ := provider(what, embedding.Ref()); fault != "" {
			faults = append(faults, fault)
		}
	}
	if jobs && saved.Embedding == nil && saved.Filling == nil && c.hadVectors.Load() {
		faults = append(faults, noEmbeddingFault)
	}
	slices.Sort(faults)
	return faults
}

// noteVectors records whether any chunk has a vector, once, at startup.
func (c *modelCatalog) noteVectors(ctx context.Context, pool *pgxpool.Pool) error {
	var embedded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_chunks WHERE embedder_id IS NOT NULL)`).Scan(&embedded); err != nil {
		return fmt.Errorf("check for embedded chunks: %w", err)
	}
	if embedded {
		c.hadVectors.Store(true)
	}
	return nil
}

// noEmbeddingFault is a corpus whose chunks have vectors while no embedding
// model is saved: the models settings lost it.
const noEmbeddingFault = "Passages already have vectors, but no embedding model is saved, so search is off: the models settings were overwritten. " +
	"Choose the embedding model on the Models page and name the collection its vectors are in."

// live is the models the roles use now, as this replica's settings cache
// last saw the saved choices and providers.
func (c *modelCatalog) live() *inferenceModels {
	if c.saved == nil {
		return &inferenceModels{}
	}
	return c.resolve(c.saved())
}

// EmbeddingDimensions makes one embedding call to ref on its enabled
// provider and returns the vector size.
func (c *modelCatalog) EmbeddingDimensions(ctx context.Context, saved settings.Models, ref string) (int, error) {
	providerName, id, ok := modelregistry.ParseRef(ref)
	if !ok {
		return 0, fmt.Errorf("%w: %q is not a provider's model", settings.ErrInvalid, ref)
	}
	provider, ok := c.registry(saved).Provider(providerName)
	if !ok {
		return 0, fmt.Errorf("%w: its provider, %s, is off or removed", settings.ErrInvalid, providerName)
	}
	return modelregistry.EmbeddingDimensions(ctx, provider, id, nil)
}

// PrepareCollection creates the collection embedding names, or, when adopt
// is set, requires it to exist already, and checks it holds vectors of the
// embedding's dimension.
func (c *modelCatalog) PrepareCollection(ctx context.Context, embedding settings.Embedding, adopt bool) error {
	if c.vectors == nil {
		return errors.New("this server has no vector store")
	}
	if adopt {
		exists, err := c.vectors.exists(ctx, embedding.Collection, embedding.Dimensions)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: Qdrant has no collection named %s", settings.ErrInvalid, embedding.Collection)
		}
	}
	if _, err := c.vectors.open(ctx, embedding); err != nil {
		return fmt.Errorf("%w: the collection %s can't be used: %s", settings.ErrInvalid, embedding.Collection, err.Error())
	}
	return nil
}

// noSummaryModel is empty while a model serves both session summary jobs,
// and otherwise says where to choose one.
func (c *modelCatalog) noSummaryModel() string {
	models := c.live()
	if models.summaryMap.ID != "" && models.summaryReduce.ID != "" {
		return ""
	}
	return "No model writes session summaries: choose one for both session summary jobs on the Models page."
}

// Roles is every role under saved, as the providers are now, for the
// Models page.
func (c *modelCatalog) Roles(ctx context.Context, saved settings.Models) ([]setup.RoleView, error) {
	var menus []modelprovider.Menu
	if c.menus != nil {
		var err error
		if menus, err = c.menus(ctx); err != nil {
			return nil, err
		}
	}
	registry := c.registry(saved)
	facts, err := c.facts(ctx, saved, menus)
	if err != nil {
		return nil, err
	}
	views := make([]setup.RoleView, 0, len(modelregistry.Roles))
	for _, role := range modelregistry.Roles {
		info := roleInfo[role]
		view := setup.RoleView{
			ModelRole: setup.ModelRole{Key: string(role), Title: info.title, Description: info.description, Unavailable: c.unavailable[role]},
			Saved:     saved.Roles[string(role)],
		}
		bound, warning := c.roleModel(registry, saved, role)
		view.Model, view.Warning = bound.refOrEmpty(), warning
		if view.Saved != "" && view.Unavailable == "" && view.Model == "" && view.Warning == "" {
			view.Warning = "It can't be used."
		}
		for _, menu := range menus {
			if !menu.Enabled {
				continue
			}
			for _, listed := range menu.Models {
				if listed.Embedding {
					continue
				}
				view.Options = append(view.Options, roleOption(registry, facts, role, menu.Name, listed))
			}
		}
		if _, offered := view.Option(view.Saved); view.Saved != "" && !offered {
			provider, id, _ := modelregistry.ParseRef(view.Saved)
			view.Options = append(view.Options, roleOption(registry, facts, role, provider, modelregistry.Listed{ID: id}))
		}
		if view.Model != "" {
			published, match := facts.of(bound.Provider, bound.ID)
			view.Attention, view.Note = facts.ending(bound.ID, published, match)
			if view.Attention == "" {
				view.Attention = unlisted(menus, bound.Model)
			}
		}
		views = append(views, view)
	}
	return views, nil
}

// Embedding is the embedding model's options, every model of the enabled
// OpenAI-compatible providers' menus (those a listing types as embedding
// models first), and why the saved one is not in use.
func (c *modelCatalog) Embedding(ctx context.Context, saved settings.Models) ([]setup.ModelOption, string, string, error) {
	var menus []modelprovider.Menu
	if c.menus != nil {
		var err error
		if menus, err = c.menus(ctx); err != nil {
			return nil, "", "", err
		}
	}
	facts, err := c.facts(ctx, saved, menus)
	if err != nil {
		return nil, "", "", err
	}
	var typed, others []setup.ModelOption
	for _, menu := range menus {
		if !menu.Enabled || menu.Kind == modelregistry.KindAnthropic {
			continue
		}
		for _, listed := range menu.Models {
			option := setup.ModelOption{Ref: modelregistry.Ref(menu.Name, listed.ID), ID: listed.ID, Provider: menu.Name, Name: listed.Name}
			facts.option(&option)
			if listed.Embedding || option.Dimensions > 0 {
				typed = append(typed, option)
			} else {
				others = append(others, option)
			}
		}
	}
	options := append(typed, others...)
	_, warning := c.embeddingModel(c.registry(saved), saved.Embedding)
	switch fault := c.searchFault.Load(); {
	case warning != "":
	case saved.Embedding == nil && saved.Filling == nil && c.hadVectors.Load():
		warning = noEmbeddingFault
	case fault != nil:
		warning = *fault
	}
	if e := saved.Embedding; e != nil && !slices.ContainsFunc(options, func(o setup.ModelOption) bool { return o.Ref == modelregistry.Ref(e.Provider, e.Model) }) {
		options = append(options, setup.ModelOption{Ref: modelregistry.Ref(e.Provider, e.Model), ID: e.Model, Provider: e.Provider})
	}
	// The model search uses, and the one being filled to replace it, which
	// the embedder is already calling.
	attention := ""
	if e := saved.Embedding; e != nil && warning == "" {
		published, match := facts.of(e.Provider, e.Model)
		attention, _ = facts.ending(e.Model, published, match)
		if attention != "" {
			attention += " Changing the embedding model embeds every passage again, which takes time: start before it ends."
		}
	}
	if e := saved.Filling; e != nil && attention == "" {
		published, match := facts.of(e.Provider, e.Model)
		if attention, _ = facts.ending(e.Model, published, match); attention != "" {
			attention = "The embedding model being filled is ending too. " + attention
		}
	}
	return options, warning, attention, nil
}

func (b boundModel) refOrEmpty() string {
	if b.ID == "" {
		return ""
	}
	return b.Ref()
}

// roleOption is one model a role may be given: what is published about it,
// by its provider's list and the catalog, and whether the model passed the
// role's test.
func roleOption(registry *modelregistry.Registry, facts *factsView, role modelregistry.Role, provider string, listed modelregistry.Listed) setup.ModelOption {
	option := setup.ModelOption{Ref: modelregistry.Ref(provider, listed.ID), ID: listed.ID, Provider: provider, Name: listed.Name}
	facts.option(&option)
	entry, ok := registry.Lookup(option.Ref)
	option.Tested = ok && entry.ServesRole(role) && slices.Contains(entry.TestedFor, role)
	if role == modelregistry.RoleAsk {
		option.DefaultEffort = listed.DefaultEffort
		published, _ := facts.of(provider, listed.ID)
		// A model with an entry takes the efforts its entry says, none
		// included: what is published now is not what its requests are
		// built from.
		_, option.ReasoningEfforts, _ = published.Reasoning()
		if ok {
			option.ReasoningEfforts = entry.ReasoningEfforts
		}
	}
	return option
}

// unlisted says that the provider of a model in use, when it offers its
// whole listing, no longer lists the model. A model an administrator typed
// into Models offered is theirs to keep current.
func unlisted(menus []modelprovider.Menu, model modelregistry.Model) string {
	index := slices.IndexFunc(menus, func(m modelprovider.Menu) bool { return m.Name == model.Provider })
	if index < 0 {
		return ""
	}
	menu := menus[index]
	if menu.InListing(model.ID) {
		return ""
	}
	if len(menu.Offered) == 0 && !menu.NoListing && menu.Error == "" && len(menu.Listing) > 0 {
		return "The provider no longer lists " + model.ID + "."
	}
	return ""
}

// TestRole sends ref the requests role makes and returns its entry with what
// the test learned, after checking that the role can build its client with
// it.
func (c *modelCatalog) TestRole(ctx context.Context, saved settings.Models, roleKey, ref, effort string) (modelregistry.Model, error) {
	role := modelregistry.Role(roleKey)
	providerName, id, ok := modelregistry.ParseRef(ref)
	if !ok {
		return modelregistry.Model{}, fmt.Errorf("%w: %q is not a provider's model", settings.ErrInvalid, ref)
	}
	provider, ok := c.registry(saved).Provider(providerName)
	if !ok {
		return modelregistry.Model{}, fmt.Errorf("%w: its provider, %s, is off or removed", settings.ErrInvalid, providerName)
	}
	published, err := c.Published(ctx, saved, ref, nil)
	if err != nil {
		return modelregistry.Model{}, err
	}
	entry, ok := c.registry(saved).Lookup(ref)
	if !ok {
		// A model's first job: what is published about it goes on its
		// entry, and the test finds the rest. Later, only an administrator
		// changes what the entry says.
		entry = modelregistry.NewEntry(providerName, id).WithPublished(published, false)
	}
	opts := modelregistry.TestOptions{PreferMessages: published.Anthropic, Trace: c.trace}
	if role == modelregistry.RoleAsk {
		opts.Effort, opts.ServiceTier = effort, saved.AskServiceTier
	}
	learned, err := modelregistry.TestForRole(ctx, provider, entry, role, opts)
	if err != nil {
		return modelregistry.Model{}, err
	}
	if err := learned.Validate(provider.Kind); err != nil {
		return modelregistry.Model{}, fmt.Errorf("%w: %s", settings.ErrInvalid, err.Error())
	}
	if err := c.checkRoleClient(saved, role, boundModel{Model: learned, endpoint: provider.Endpoint}); err != nil {
		return modelregistry.Model{}, fmt.Errorf("%w: the job can't use it: %s", settings.ErrInvalid, err.Error())
	}
	return learned, nil
}

// CanServe says why role could not use entry under saved, or nil.
func (c *modelCatalog) CanServe(saved settings.Models, roleKey string, entry modelregistry.Model) error {
	role := modelregistry.Role(roleKey)
	if !entry.ServesRole(role) {
		if modelregistry.NeedsImages(role) && !entry.Images {
			return errors.New("the job sends images, which the model would no longer accept")
		}
		return errors.New("the model would no longer answer for the job")
	}
	bound := boundModel{Model: entry}
	if endpoint, err := c.registry(saved).Endpoint(entry); err == nil {
		bound.endpoint = endpoint
	} else {
		// A provider that is off stops the job already; the entry is
		// checked against a stand-in address.
		bound.endpoint = llmclient.Endpoint{BaseURL: "https://provider.invalid/v1", APIKey: "unused"}
	}
	return c.checkRoleClient(saved, role, bound)
}

// checkRoleClient builds role's client for model without calling it, so an
// entry the role's requests can't carry (an extra chat field that collides
// with the role's own, the Ask service tier saved, when the model refuses
// one) is never used.
func (c *modelCatalog) checkRoleClient(saved settings.Models, role modelregistry.Role, model boundModel) error {
	var err error
	switch role {
	case modelregistry.RoleAsk:
		err = ask.ValidateModelSelection(model.Model, "", saved.AskServiceTier)
	case modelregistry.RoleTitles:
		_, err = newTitleGenerator(model, "")
	case modelregistry.RoleImageCaptions:
		_, err = newCaptioner(model)
	case modelregistry.RoleDocumentExtraction:
		_, err = newDocumentExtractor(model, nil)
	case modelregistry.RoleThreadSummaries:
		_, err = newThreadSummarizer(model)
	case modelregistry.RoleSessionSummaryMap, modelregistry.RoleSessionSummaryReduce:
		_, _, err = newSummaryStageClient(role, model)
	}
	return err
}

// sameRoleModels reports whether a and b build every role, the embedder
// among them, the same clients.
func sameRoleModels(a, b *inferenceModels) bool {
	return maps.Equal(a.fingerprints(), b.fingerprints()) && a.embedFingerprint() == b.embedFingerprint() &&
		vectorFingerprint(a.fill, a.filling) == vectorFingerprint(b.fill, b.filling)
}

// sameAskModel reports whether a and b build Ask the same client with the
// same limits, and agree on whether search, which Ask needs, is set up.
func sameAskModel(a, b *inferenceModels) bool {
	return a.ask.fingerprint() == b.ask.fingerprint() && a.askLimits == b.askLimits && (a.embed.ID == "") == (b.embed.ID == "")
}

// sameEmbedding reports whether a and b search with the same model and
// collection.
func sameEmbedding(a, b *inferenceModels) bool { return a.embedFingerprint() == b.embedFingerprint() }

// runPerModels runs work with the models the roles use now, and when the
// saved choices change them (same reports no change) cancels work, waits for
// it to return, and starts it again with the new models. It returns once ctx
// ends and work has returned.
func runPerModels(ctx context.Context, logger *slog.Logger, name string, current func() *inferenceModels,
	same func(a, b *inferenceModels) bool, work func(context.Context, *inferenceModels)) {
	ticker := time.NewTicker(modelsWatchInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		models := current()
		termCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			work(termCtx, models)
		}()
		for termCtx.Err() == nil {
			select {
			case <-termCtx.Done():
			case <-ticker.C:
				if !same(models, current()) {
					logger.Info("the saved models changed; restarting with them", "work", name)
					cancel()
				}
			}
		}
		<-done
		cancel()
	}
}

// modelsWatchInterval is how often runPerModels compares the models in use
// with the settings cache, which the store's poll keeps current.
const modelsWatchInterval = time.Second

// newEmbedder builds the embeddings client of search and the embedder
// worker, which checks every vector has dimensions. Callers check that
// model is set first.
func newEmbedder(model boundModel, dimensions int) (*embedder.GatewayEmbedder, error) {
	return embedder.NewGatewayEmbedder(embedder.GatewayEmbedderConfig{
		BaseURL: model.endpoint.BaseURL, APIKey: model.endpoint.APIKey, Auth: model.endpoint.Auth, Headers: model.endpoint.Headers,
		Model: model.ID, Dim: dimensions,
	})
}

// newThreadSummarizer builds the Slack thread summarizer for model.
func newThreadSummarizer(model boundModel) (*slackcx.Summarizer, error) {
	return slackcx.NewSummarizer(summarizerConfig(model, ""))
}

// summarizerConfig is a chat surface's thread summarizer on model, with the
// surface's instructions.
func summarizerConfig(model boundModel, systemPrompt string) askchat.SummarizerConfig {
	return askchat.SummarizerConfig{
		SystemPrompt: systemPrompt,
		BaseURL:      model.endpoint.BaseURL, APIKey: model.endpoint.APIKey, Auth: model.endpoint.Auth, Headers: model.endpoint.Headers,
		ModelID: model.ID, TokenParam: model.ChatTokenParam,
		ReasoningTokenAllowance: model.ChatReasoningTokenAllowance, ExtraBody: model.ChatFields(),
	}
}

// newSessionSummaryClient builds the client the session-summary map and
// reduce stages share. Each request goes to the provider of its own model,
// on the protocol that model's entry gives the stage, so the two stages may
// use different providers and protocols; two providers' models of the same
// ID would be one model to it, so they are refused. The second result is the
// protocol of each stage's model, by model ID.
func newSessionSummaryClient(models *inferenceModels) (sessionsummary.ToolClient, map[string]modelregistry.Protocol, error) {
	clients := modelRoutedTools{}
	protocols := map[string]modelregistry.Protocol{}
	providers := map[string]string{}
	for _, role := range []modelregistry.Role{modelregistry.RoleSessionSummaryMap, modelregistry.RoleSessionSummaryReduce} {
		stage := *models.role(role)
		if stage.ID == "" {
			continue
		}
		if provider, seen := providers[stage.ID]; seen {
			if provider != stage.Provider {
				return nil, nil, errors.New("session summaries: both stages use a model named " + stage.ID + " from different providers")
			}
			continue
		}
		client, protocol, err := newSummaryStageClient(role, stage)
		if err != nil {
			return nil, nil, err
		}
		clients[stage.ID], protocols[stage.ID], providers[stage.ID] = client, protocol, stage.Provider
	}
	return clients, protocols, nil
}

// newSummaryStageClient builds the client of one session-summary stage for
// model, on the protocol the model's entry gives the stage
// (modelregistry.Model.RoleProtocol), which it returns.
func newSummaryStageClient(role modelregistry.Role, model boundModel) (sessionsummary.ToolClient, modelregistry.Protocol, error) {
	protocol, ok := model.RoleProtocol(role)
	if !ok {
		return nil, "", errors.New("session summaries: " + model.ID + " lists no protocol they speak")
	}
	endpoint := model.endpoint
	var client sessionsummary.ToolClient
	var err error
	switch protocol {
	case modelregistry.ProtocolMessages:
		client, err = sessionsummary.NewGatewayClient(sessionsummary.GatewayClientConfig{
			BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey, Auth: endpoint.Auth, Headers: endpoint.Headers,
		})
	case modelregistry.ProtocolResponses:
		effort := ""
		if model.SupportsReasoningEffort(sessionsummary.ResponsesReasoningEffort) {
			effort = sessionsummary.ResponsesReasoningEffort
		}
		client, err = sessionsummary.NewResponsesClient(sessionsummary.ResponsesClientConfig{
			BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey, Auth: endpoint.Auth, Headers: endpoint.Headers,
			ReasoningEffort: effort,
		})
	default:
		client, err = sessionsummary.NewChatClient(sessionsummary.ChatClientConfig{
			BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey, Auth: endpoint.Auth, Headers: endpoint.Headers,
			TokenParam: model.ChatTokenParam, ReasoningTokenAllowance: model.ChatReasoningTokenAllowance,
			ExtraBody: model.ChatFields(),
		})
	}
	return client, protocol, err
}

// summaryProtocolNames names the protocols session summaries speak, in the
// order they choose among them.
func summaryProtocolNames() string {
	names := make([]string, len(modelregistry.SummaryProtocols))
	for i, protocol := range modelregistry.SummaryProtocols {
		names[i] = string(protocol)
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}

// modelRoutedTools sends each summary request to the client of its model.
type modelRoutedTools map[string]sessionsummary.ToolClient

func (m modelRoutedTools) CallTool(ctx context.Context, request sessionsummary.ToolRequest) (json.RawMessage, sessionsummary.Usage, error) {
	client := m[request.ModelID]
	if client == nil {
		return nil, sessionsummary.Usage{}, fmt.Errorf("session summaries: model %q is not one of this leader term's", request.ModelID)
	}
	return client.CallTool(ctx, request)
}

// newTitleGenerator builds the session-title client for model. An empty
// promptKey selects the agent-session title prompt.
func newTitleGenerator(model boundModel, promptKey string) (*sessiontitle.GatewayGenerator, error) {
	effort := ""
	if model.SupportsReasoningEffort(sessiontitle.ReasoningEffort) {
		effort = sessiontitle.ReasoningEffort
	}
	return sessiontitle.NewGatewayGenerator(sessiontitle.GatewayConfig{
		BaseURL: model.endpoint.BaseURL, APIKey: model.endpoint.APIKey, Headers: model.endpoint.Headers,
		ModelID: model.ID, ReasoningEffort: effort, PromptKey: promptKey,
	})
}
