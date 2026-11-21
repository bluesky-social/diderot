package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/settings"
)

// ModelRole is one inference role: what it does, whether the deployment runs
// it, and the models the enabled providers offer it.
type ModelRole struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Unavailable says, in plain words, why the deployment does not run the
	// role; empty when it does.
	Unavailable string `json:"unavailable,omitempty"`
	// Options are the models the enabled providers offer, by provider and
	// then ID.
	Options []ModelOption `json:"options,omitempty"`
}

// ModelOption is one model a role may be given.
type ModelOption struct {
	// Ref is the modelregistry.Ref a save posts.
	Ref      string `json:"ref"`
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name,omitempty"`
	// Tested reports that the model passed this role's test.
	Tested bool `json:"tested,omitempty"`
	// ReasoningEfforts are the efforts the model is known to take, from its
	// entry or the provider's listing; only Ask's options carry them.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// DefaultEffort is the provider's published choice for an omitted effort.
	DefaultEffort string `json:"default_effort,omitempty"`
	// Images is what the listing says about image input; nil when silent.
	Images *bool `json:"images,omitempty"`
	// InputPrice and OutputPrice are USD per million tokens, when the
	// listing gives them.
	InputPrice  *float64 `json:"input_price,omitempty"`
	OutputPrice *float64 `json:"output_price,omitempty"`
	// Retires and Deprecated are what the listing, or else the published
	// catalog, says about its end.
	Retires    string `json:"retires,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty"`
	// ContextTokens is its context window, and Dimensions the size of an
	// embedding model's vectors, when published.
	ContextTokens int `json:"context,omitempty"`
	Dimensions    int `json:"dimensions,omitempty"`
	// Matched is the published catalog entry that describes it, when one
	// does for certain.
	Matched string `json:"matched,omitempty"`
}

// FactsList is one published list of model facts as last read.
type FactsList struct {
	Name      string    `json:"name"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// Error is why the latest read failed; empty when it worked.
	Error  string `json:"error,omitempty"`
	Models int    `json:"models,omitempty"`
}

// FactsStatus is how the published model facts are doing.
type FactsStatus struct {
	// Off reports that an administrator turned them off.
	Off bool `json:"off,omitempty"`
	// Models is how many models the catalog describes.
	Models int         `json:"models,omitempty"`
	Lists  []FactsList `json:"lists,omitempty"`
	// Attention says the lists have gone stale, and why.
	Attention string `json:"attention,omitempty"`
}

// FactRow is one fact of a model entry on its page: what is sent now, where
// that came from, and what is published now when it differs.
type FactRow struct {
	Key, Label, Value, Source string
	Published, PublishedBy    string
}

// InfoRow is something published about a model that shapes no request: its
// price, its limits.
type InfoRow struct {
	Label, Value, Source string
}

// MatchSuggestion is a catalog entry that may describe a model.
type MatchSuggestion struct {
	Ref, Name string
}

// MatchView is the catalog entry a model is matched to.
type MatchView struct {
	// Ref is the entry (namespace/id) and Name what it is called; empty
	// when none describes the model for certain.
	Ref, Name string
	// How says why that entry is the model.
	How string
	// Pin is the administrator's choice as saved; Pinned, Cleared and
	// Missing say what it did.
	Pin                      string
	Pinned, Cleared, Missing bool
	// Suggestions are entries that may be the model, to confirm.
	Suggestions []MatchSuggestion
}

// ModelDetail is what a model's page shows beyond its entry.
type ModelDetail struct {
	// Off reports that published facts are turned off, and NoCatalog that
	// none has been read yet.
	Off, NoCatalog bool
	Facts          []FactRow
	Info           []InfoRow
	// Differs reports that something published differs from what is sent.
	Differs bool
	// Ending says the model is deprecated or about to be retired.
	Ending string
	Match  MatchView
}

// RoleView is one role and the model it uses now.
type RoleView struct {
	ModelRole
	// Saved is the Ref saved for the role, empty for none.
	Saved string `json:"saved,omitempty"`
	// Model is the Ref the role uses now: Saved, while its provider is on
	// and it passed the role's test; empty while the role is off.
	Model string `json:"model,omitempty"`
	// Warning says why Saved is not in use.
	Warning string `json:"warning,omitempty"`
	// Attention says what is wrong with the model in use: its provider no
	// longer lists it, or it is deprecated or about to be retired.
	Attention string `json:"attention,omitempty"`
	// Note says something about the model in use that needs nobody yet: a
	// retirement further off.
	Note string `json:"note,omitempty"`
}

// Option returns the role's option for ref.
func (r RoleView) Option(ref string) (ModelOption, bool) {
	for _, option := range r.Options {
		if option.Ref == ref {
			return option, true
		}
	}
	return ModelOption{}, false
}

// Catalog is what the server knows about models: each role under saved
// choices, as the providers are now, each role's test, and the embedding
// model's dimension and collection.
type Catalog interface {
	// Roles is every role, in the order pages show them.
	Roles(ctx context.Context, saved settings.Models) ([]RoleView, error)
	// TestRole sends ref the requests role makes, as saving it for the role
	// does, with effort for Ask, and returns its entry, from saved or new,
	// with what the test learned.
	TestRole(ctx context.Context, saved settings.Models, role, ref, effort string) (modelregistry.Model, error)
	// Embedding is the embedding model's options, the enabled providers'
	// models, why the saved one is not in use, and what needs attention
	// about one that is: its retirement.
	Embedding(ctx context.Context, saved settings.Models) (options []ModelOption, warning, attention string, err error)
	// FactsStatus says how the published model facts are doing.
	FactsStatus(ctx context.Context, saved settings.Models) (FactsStatus, error)
	// Published is what is published about the model ref, with pin, when
	// not nil, in place of its entry's choice of catalog entry.
	Published(ctx context.Context, saved settings.Models, ref string, pin *string) (modelregistry.Published, error)
	// Detail is what ref's page shows about its entry.
	Detail(ctx context.Context, saved settings.Models, ref string) (ModelDetail, error)
	// EmbeddingDimensions makes one embedding call to ref and returns the
	// size of its vectors.
	EmbeddingDimensions(ctx context.Context, saved settings.Models, ref string) (int, error)
	// PrepareCollection creates embedding's collection, or with adopt
	// requires it to exist, and checks it holds vectors of its dimension.
	PrepareCollection(ctx context.Context, embedding settings.Embedding, adopt bool) error
	// CanServe says why role could not use entry under saved, or nil: the
	// entry does not answer for the role, or the role's client can't be
	// built with it.
	CanServe(saved settings.Models, role string, entry modelregistry.Model) error
}

// ModelsView is the Models page: every role's model in use, Ask's limits,
// and the model entries.
type ModelsView struct {
	// Version is the models row's version; a save posts it back.
	Version int64      `json:"version"`
	Roles   []RoleView `json:"roles"`
	// AskReasoningEffort is the saved effort; empty is the model's default.
	AskReasoningEffort        string `json:"ask_reasoning_effort"`
	AskReasoningEffortWarning string `json:"ask_reasoning_effort_warning,omitempty"`
	AskMaxToolTurns           int    `json:"ask_max_tool_turns"`
	// AskLimits are Ask's other limits in effect: each saved value, or its
	// default.
	AskLimits settings.AskLimits `json:"ask_limits"`
	// Available is every model entry, in the order saved.
	Available []modelregistry.Model `json:"available,omitempty"`
	// Embedding is the saved embedding model, nil before one is chosen;
	// EmbeddingOptions the models the enabled providers offer for it, and
	// EmbeddingWarning why the saved one is not in use.
	Embedding        *settings.Embedding `json:"embedding,omitempty"`
	EmbeddingOptions []ModelOption       `json:"embedding_options,omitempty"`
	EmbeddingWarning string              `json:"embedding_warning,omitempty"`
	// EmbeddingAttention says the embedding model in use is ending.
	EmbeddingAttention string `json:"embedding_attention,omitempty"`
	// Facts is how the published model facts are doing.
	Facts FactsStatus `json:"facts"`
	// EmbeddedChunks counts the chunks that have vectors, which decides
	// whether a first choice adopts a collection or embeds everything.
	EmbeddedChunks int64 `json:"embedded_chunks"`
	// Filling is the embedding model being embedded into its own
	// collection to replace Embedding, nil when none is; FillingDone of
	// FillingTotal chunks are in it.
	Filling      *settings.Embedding `json:"filling,omitempty"`
	FillingDone  int64               `json:"filling_done,omitempty"`
	FillingTotal int64               `json:"filling_total,omitempty"`
}

// EmbeddingRef is the saved embedding model's Ref, or "".
func (v ModelsView) EmbeddingRef() string {
	if v.Embedding == nil {
		return ""
	}
	return modelregistry.Ref(v.Embedding.Provider, v.Embedding.Model)
}

// Role returns the role with key, or a zero RoleView.
func (v ModelsView) Role(key string) RoleView {
	for _, role := range v.Roles {
		if role.Key == key {
			return role
		}
	}
	return RoleView{}
}

// Attention lists what needs an administrator on the Models page: a saved
// model that is not in use, or one in use that is going away.
func (v ModelsView) Attention() []string {
	var attention []string
	switch {
	case v.EmbeddingWarning != "":
		attention = append(attention, "Embeddings: "+v.EmbeddingWarning)
	case v.EmbeddingAttention != "":
		attention = append(attention, "Embeddings: "+v.EmbeddingAttention)
	}
	for _, role := range v.Roles {
		switch {
		case role.Warning != "":
			attention = append(attention, role.Title+": "+role.Warning)
		case role.Attention != "":
			attention = append(attention, role.Title+": "+role.Attention)
		}
	}
	if v.Facts.Attention != "" {
		attention = append(attention, v.Facts.Attention)
	}
	return attention
}

// Models reads the models row from the database (not the cache, so a page
// shows the version it posts back) and what it means now.
func (s *Service) Models(ctx context.Context) (ModelsView, error) {
	row, err := s.settings.ReadModels(ctx)
	if err != nil {
		return ModelsView{}, err
	}
	view := ModelsView{
		Version: row.Version, AskMaxToolTurns: row.MaxToolTurns(), AskReasoningEffort: row.AskReasoningEffort,
		AskLimits: row.AskLimits(),
		Available: row.Available,
	}
	if s.catalog == nil {
		return view, nil
	}
	if view.Roles, err = s.catalog.Roles(ctx, row.Models); err != nil {
		return ModelsView{}, err
	}
	view.Embedding, view.Filling = row.Embedding, row.Filling
	if row.Filling != nil {
		if view.FillingDone, view.FillingTotal, err = s.fillProgress(ctx, row.Filling.Collection); err != nil {
			return ModelsView{}, err
		}
	}
	if view.EmbeddingOptions, view.EmbeddingWarning, view.EmbeddingAttention, err = s.catalog.Embedding(ctx, row.Models); err != nil {
		return ModelsView{}, err
	}
	if view.Facts, err = s.catalog.FactsStatus(ctx, row.Models); err != nil {
		return ModelsView{}, err
	}
	if view.EmbeddedChunks, err = s.embeddedChunks(ctx); err != nil {
		return ModelsView{}, err
	}
	ask := view.Role(string(modelregistry.RoleAsk))
	if entry, ok := entryOf(row.Available, ask.Model); ok && row.AskReasoningEffort != "" && !entry.SupportsReasoningEffort(row.AskReasoningEffort) {
		view.AskReasoningEffortWarning = fmt.Sprintf("%s doesn't list the saved reasoning effort, %s, so Ask uses the model's default until you save again.",
			entry.ID, row.AskReasoningEffort)
	}
	return view, nil
}

func entryOf(entries []modelregistry.Model, ref string) (modelregistry.Model, bool) {
	index := slices.IndexFunc(entries, func(m modelregistry.Model) bool { return m.Ref() == ref })
	if index < 0 {
		return modelregistry.Model{}, false
	}
	return entries[index], true
}

// ModelsUpdate is a save from the Models page. Roles maps a role to the Ref
// of its model, or "" to turn it off; a role left out keeps its saved
// choice, and a nil limit keeps its saved value.
type ModelsUpdate struct {
	Roles              map[string]string   `json:"roles,omitempty"`
	AskReasoningEffort *string             `json:"ask_reasoning_effort,omitempty"`
	AskMaxToolTurns    *int                `json:"ask_max_tool_turns,omitempty"`
	AskLimits          *settings.AskLimits `json:"ask_limits,omitempty"`
}

// validateAskLimits checks the limits a save carries; nil keeps the saved
// ones.
func validateAskLimits(limits *settings.AskLimits) error {
	if limits == nil {
		return nil
	}
	if limits.ServiceTier != "" && !slices.Contains(modelregistry.ServiceTiers, limits.ServiceTier) {
		return fmt.Errorf("%w: Service tier must be one of %s, or empty for the provider's default",
			settings.ErrInvalid, strings.Join(modelregistry.ServiceTiers, ", "))
	}
	if !settings.AskMaxOutputTokensInRange(limits.MaxOutputTokens) {
		return fmt.Errorf("%w: Longest reply must be a number of tokens from %d to %d",
			settings.ErrInvalid, settings.MinAskMaxOutputTokens, settings.MaxAskMaxOutputTokens)
	}
	for _, limit := range []struct {
		name string
		d    time.Duration
	}{
		{"Time limit for one model request", limits.ModelTimeout},
		{"Time limit for one question", limits.TurnDeadline},
		{"Investigation time", limits.InvestigatorDeadline},
		{"Time to write the investigation report", limits.InvestigatorGrace},
	} {
		if !settings.AskDurationInRange(limit.d) {
			return fmt.Errorf("%w: %s must be between %s and %s", settings.ErrInvalid, limit.name,
				settings.FormatDuration(settings.MinAskDuration), settings.FormatDuration(settings.MaxAskDuration))
		}
	}
	return nil
}

// ModelTestError is a save refused because a model it chose failed its job's
// test. Its message is what the page shows.
type ModelTestError struct {
	Role, Model string
	// Reason is the test's classification (an HTTP status, a timeout, a
	// refused request shape).
	Reason string
}

func (e *ModelTestError) Error() string {
	return fmt.Sprintf("%s didn't pass the test for %s: %s. Nothing was saved.", e.Model, e.Role, e.Reason)
}

// SaveModels gives each role in update its model, and stores Ask's limits,
// at version. A model newly given to a role, and Ask's model when its effort
// changes, first passes the role's test, whose findings go on the model's
// entry; one failure saves nothing. After a save every role is tested again
// and the result recorded, as Test models does.
func (s *Service) SaveModels(ctx context.Context, actor Actor, version int64, update ModelsUpdate) (settings.ModelsRow, error) {
	if s.catalog == nil {
		return settings.ModelsRow{}, fmt.Errorf("%w: this server has no models", settings.ErrInvalid)
	}
	if turns := update.AskMaxToolTurns; turns != nil && (*turns < 1 || *turns > settings.MaxAskToolTurns) {
		return settings.ModelsRow{}, fmt.Errorf("%w: Maximum tool turns must be a number from 1 to %d", settings.ErrInvalid, settings.MaxAskToolTurns)
	}
	if effort := update.AskReasoningEffort; effort != nil && *effort != "" && !slices.Contains(modelregistry.ReasoningEfforts, *effort) {
		return settings.ModelsRow{}, fmt.Errorf("%w: Reasoning effort must be one of %s, or empty for the model's default",
			settings.ErrInvalid, strings.Join(modelregistry.ReasoningEfforts, ", "))
	}
	if err := validateAskLimits(update.AskLimits); err != nil {
		return settings.ModelsRow{}, err
	}
	// Validate and test against the row as read; the write below checks it
	// is still that version, so it stores exactly what was checked. A stale
	// form fails before any model is called.
	current, err := s.settings.ReadModels(ctx)
	if err != nil {
		return settings.ModelsRow{}, err
	}
	if current.Version != version {
		return settings.ModelsRow{}, settings.ErrConflict
	}
	roles, err := s.catalog.Roles(ctx, current.Models)
	if err != nil {
		return settings.ModelsRow{}, err
	}
	// With vectors in the corpus, the embedding model comes first: jobs
	// saved without one is what a models row that lost its embedding looks
	// like, which a server reports as a fault.
	if current.Embedding == nil && current.Filling == nil {
		given := false
		for _, ref := range update.Roles {
			given = given || strings.TrimSpace(ref) != ""
		}
		if embedded, err := s.embeddedChunks(ctx); err != nil {
			return settings.ModelsRow{}, err
		} else if given && embedded > 0 {
			return settings.ModelsRow{}, fmt.Errorf("%w: choose the embedding model first, under Embeddings: passages already have vectors, and search needs to know which model made them", settings.ErrInvalid)
		}
	}
	next := current.Models
	next.Roles = maps.Clone(current.Roles)
	if next.Roles == nil {
		next.Roles = map[string]string{}
	}
	next.Available = slices.Clone(current.Available)
	if update.AskReasoningEffort != nil {
		next.AskReasoningEffort = *update.AskReasoningEffort
	}
	if update.AskMaxToolTurns != nil {
		next.AskMaxToolTurns = *update.AskMaxToolTurns
	}
	if limits := update.AskLimits; limits != nil {
		next.AskServiceTier, next.AskMaxOutputTokens = limits.ServiceTier, limits.MaxOutputTokens
		next.AskModelTimeout, next.AskTurnDeadline = settings.Duration(limits.ModelTimeout), settings.Duration(limits.TurnDeadline)
		next.AskInvestigatorDeadline, next.AskInvestigatorGrace = settings.Duration(limits.InvestigatorDeadline), settings.Duration(limits.InvestigatorGrace)
	}
	// A service tier is sent with every request, so a new one is tested as
	// a new effort is.
	askChanged := next.AskReasoningEffort != current.AskReasoningEffort ||
		(next.AskServiceTier != current.AskServiceTier && next.AskServiceTier != "")
	var tests []string
	for _, role := range roles {
		ref, given := update.Roles[role.Key]
		ref = strings.TrimSpace(ref)
		// Ask is tested again for a new effort or service tier, whatever
		// its model.
		retest := role.Key == string(modelregistry.RoleAsk) && askChanged
		switch {
		case !given && !retest:
			continue
		case !given:
			ref = current.Roles[role.Key]
		case ref == "":
			delete(next.Roles, role.Key)
			continue
		case role.Unavailable != "":
			return settings.ModelsRow{}, fmt.Errorf("%w: %s is off: %s", settings.ErrInvalid, role.Title, role.Unavailable)
		}
		if ref == "" || role.Unavailable != "" {
			continue
		}
		if _, offered := role.Option(ref); !offered && ref != current.Roles[role.Key] {
			return settings.ModelsRow{}, fmt.Errorf("%w: no enabled provider offers %q for %s", settings.ErrInvalid, ref, role.Title)
		}
		next.Roles[role.Key] = ref
		// A model the role already uses keeps its test, unless Ask's effort
		// or service tier changed.
		if ref == role.Model && !retest {
			continue
		}
		tests = append(tests, role.Key)
	}
	if len(tests) > 0 {
		s.testing.Lock()
		err := s.testRoles(ctx, roles, tests, &next)
		s.testing.Unlock()
		if err != nil {
			return settings.ModelsRow{}, err
		}
	}
	row, err := s.settings.UpdateModels(ctx, version, actor.settings(), "setup.models.save", func(models *settings.Models) error {
		*models = next
		return nil
	})
	if err != nil {
		return settings.ModelsRow{}, err
	}
	s.recordModelsTest(ctx, actor)
	return row, nil
}

// testRoles runs each named role's test on the model next gives it, and
// puts what each learned on next's entries.
func (s *Service) testRoles(ctx context.Context, roles []RoleView, keys []string, next *settings.Models) error {
	ctx, cancel := context.WithTimeout(ctx, ModelTestTimeout)
	defer cancel()
	for _, role := range roles {
		if !slices.Contains(keys, role.Key) {
			continue
		}
		ref := next.Roles[role.Key]
		effort := ""
		if role.Key == string(modelregistry.RoleAsk) {
			effort = next.AskReasoningEffort
		}
		entry, err := s.catalog.TestRole(ctx, *next, role.Key, ref, effort)
		if err != nil {
			var testErr *modelregistry.TestError
			if errors.As(err, &testErr) || errors.Is(err, settings.ErrInvalid) {
				_, id, _ := modelregistry.ParseRef(ref)
				return &ModelTestError{Role: role.Title, Model: id, Reason: strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": ")}
			}
			return err
		}
		next.Available = putEntry(next.Available, entry)
	}
	return nil
}

// putEntry replaces the entry with entry's ref, or adds entry.
func putEntry(entries []modelregistry.Model, entry modelregistry.Model) []modelregistry.Model {
	if index := slices.IndexFunc(entries, func(m modelregistry.Model) bool { return m.Ref() == entry.Ref() }); index >= 0 {
		entries[index] = entry
		return entries
	}
	return append(entries, entry)
}

// recordModelsTest tests every role after a save and records the result, so
// setup's Models step follows the save. A conflicting write elsewhere leaves
// the record to the next test.
func (s *Service) recordModelsTest(ctx context.Context, actor Actor) {
	org, err := s.settings.ReadOrg(ctx)
	if err != nil {
		return
	}
	update := func(change func(*settings.Org) error) (settings.OrgRow, error) {
		return s.settings.UpdateOrg(ctx, org.Version, actor.settings(), "setup.test_models", change)
	}
	_, _ = s.testModels(ctx, org.Version, update, strings.ToLower(strings.TrimSpace(actor.Email)))
}

// ModelEntryUpdate is an administrator's correction of a model entry: the
// facts its tests found, and the request fields no test can find.
type ModelEntryUpdate struct {
	ReasoningEfforts            []string
	ServiceTier                 bool
	PromptCacheRetention        string
	Images                      bool
	ChatMaxCompletionTokens     bool
	ChatReasoningTokenAllowance int
	ChatOmitTemperature         bool
	ChatReasoningEffort         string
	ChatExtraBody               map[string]any
}

// EntryChange is one fact an administrator's correction of a model entry
// changes, in words for the page.
type EntryChange struct {
	Fact, Text string
}

// EntryChangeError is a correction of a model entry that was not saved
// because jobs use the model. With Stops, the change would stop those jobs,
// and is refused until they are given another model. Otherwise it changes
// what Jobs send, and the administrator must confirm it.
type EntryChangeError struct {
	Jobs    []string
	Changes []EntryChange
	Stops   []string
}

func (e *EntryChangeError) Error() string {
	if len(e.Stops) > 0 {
		return "Not saved: " + strings.Join(e.Stops, " ")
	}
	return "Not saved yet: this changes what " + strings.Join(e.Jobs, ", ") + " send. Confirm it below."
}

// apply puts update on entry.
func (u ModelEntryUpdate) apply(entry modelregistry.Model) modelregistry.Model {
	entry.ReasoningEfforts = u.ReasoningEfforts
	entry.ServiceTier = u.ServiceTier
	entry.PromptCacheRetention = u.PromptCacheRetention
	entry.Images = u.Images
	entry.ChatTokenParam = ""
	if u.ChatMaxCompletionTokens {
		entry.ChatTokenParam = "max_completion_tokens"
	}
	entry.ChatReasoningTokenAllowance = u.ChatReasoningTokenAllowance
	entry.ChatOmitTemperature = u.ChatOmitTemperature
	entry.ChatReasoningEffort = u.ChatReasoningEffort
	entry.ChatExtraBody = u.ChatExtraBody
	return entry
}

// entryChanges lists the facts that differ between two versions of an
// entry.
func entryChanges(before, after modelregistry.Model) []EntryChange {
	yesNo := func(value bool) string {
		if value {
			return "yes"
		}
		return "no"
	}
	list := func(values []string) string {
		if len(values) == 0 {
			return "none"
		}
		return strings.Join(values, ", ")
	}
	orNone := func(value string) string {
		if value == "" {
			return "not sent"
		}
		return value
	}
	param := func(m modelregistry.Model) string {
		if m.ChatTokenParam == "" {
			return "max_tokens"
		}
		return string(m.ChatTokenParam)
	}
	body := func(m modelregistry.Model) string {
		if len(m.ChatExtraBody) == 0 {
			return "none"
		}
		raw, _ := json.Marshal(m.ChatExtraBody)
		return string(raw)
	}
	var changes []EntryChange
	add := func(fact, label, from, to string) {
		if from != to {
			changes = append(changes, EntryChange{Fact: fact, Text: label + ": " + from + " becomes " + to})
		}
	}
	add(modelregistry.FactEfforts, "Reasoning efforts", list(before.ReasoningEfforts), list(after.ReasoningEfforts))
	add(modelregistry.FactImages, "Accepts images", yesNo(before.Images), yesNo(after.Images))
	add(modelregistry.FactServiceTier, "Takes a service tier", yesNo(before.ServiceTier), yesNo(after.ServiceTier))
	add(modelregistry.FactCacheRetention, "Prompt cache retention", orNone(before.PromptCacheRetention), orNone(after.PromptCacheRetention))
	add(modelregistry.FactTokenParam, "Chat output limit field", param(before), param(after))
	add(modelregistry.FactTemperature, "Sends a temperature", yesNo(!before.ChatOmitTemperature), yesNo(!after.ChatOmitTemperature))
	add(modelregistry.FactChatEffort, "Effort chat jobs ask for", orNone(before.ChatReasoningEffort), orNone(after.ChatReasoningEffort))
	add(modelregistry.FactAllowance, "Reasoning token allowance", strconv.Itoa(before.ChatReasoningTokenAllowance), strconv.Itoa(after.ChatReasoningTokenAllowance))
	add(modelregistry.FactExtraBody, "Extra request fields", body(before), body(after))
	return changes
}

// UpdateModelEntry saves an administrator's correction of the entry ref at
// version, and records each fact it changes as stated by an administrator,
// which no test changes afterwards. While jobs use the model, a correction
// that changes what they send needs confirmed, and one that would stop a
// job is refused: both fail with an *EntryChangeError that names the jobs
// and the changes. Jobs pick a saved change up as a saved model change.
func (s *Service) UpdateModelEntry(ctx context.Context, actor Actor, version int64, ref string, kind modelregistry.Kind, update ModelEntryUpdate, confirmed bool) (settings.ModelsRow, error) {
	return s.changeEntry(ctx, actor, version, ref, kind, "setup.models.entry.save", confirmed, func(before modelregistry.Model) modelregistry.Model {
		entry := update.apply(before)
		for _, change := range entryChanges(before, entry) {
			entry = entry.WithSource(change.Fact, modelregistry.SourceAdmin)
		}
		return entry
	})
}

// UsePublishedFacts puts what is published about the model ref on its
// entry, at an administrator's asking: each fact from the provider's list
// when it says, otherwise from the catalog, leaving what an administrator
// set by hand. It asks and refuses as UpdateModelEntry does.
func (s *Service) UsePublishedFacts(ctx context.Context, actor Actor, version int64, ref string, kind modelregistry.Kind, confirmed bool) (settings.ModelsRow, error) {
	if s.catalog == nil {
		return settings.ModelsRow{}, fmt.Errorf("%w: this server has no models", settings.ErrInvalid)
	}
	current, err := s.settings.ReadModels(ctx)
	if err != nil {
		return settings.ModelsRow{}, err
	}
	published, err := s.catalog.Published(ctx, current.Models, ref, nil)
	if err != nil {
		return settings.ModelsRow{}, err
	}
	return s.changeEntry(ctx, actor, version, ref, kind, "setup.models.entry.published", confirmed, func(before modelregistry.Model) modelregistry.Model {
		return before.WithPublished(published, true)
	})
}

// SetModelMatch saves an administrator's choice of the published catalog
// entry that describes the model ref: an entry's namespace/id, empty to
// match it automatically again, or modelregistry.CatalogNone for none. It
// changes what the pages show about the model, and no request.
func (s *Service) SetModelMatch(ctx context.Context, actor Actor, version int64, ref, pin string) (settings.ModelsRow, error) {
	if s.catalog == nil {
		return settings.ModelsRow{}, fmt.Errorf("%w: this server has no models", settings.ErrInvalid)
	}
	pin = strings.TrimSpace(pin)
	current, err := s.settings.ReadModels(ctx)
	if err != nil {
		return settings.ModelsRow{}, err
	}
	if _, err := s.catalog.Published(ctx, current.Models, ref, &pin); err != nil {
		return settings.ModelsRow{}, err
	}
	return s.settings.UpdateModels(ctx, version, actor.settings(), "setup.models.entry.match", func(models *settings.Models) error {
		index := slices.IndexFunc(models.Available, func(m modelregistry.Model) bool { return m.Ref() == ref })
		if index < 0 {
			return fmt.Errorf("%w: there is no model %s", settings.ErrInvalid, ref)
		}
		models.Available = slices.Clone(models.Available)
		models.Available[index].Catalog = pin
		return nil
	})
}

// SetPublishedFacts turns reading the published model lists on or off.
func (s *Service) SetPublishedFacts(ctx context.Context, actor Actor, version int64, on bool) (settings.ModelsRow, error) {
	return s.settings.UpdateModels(ctx, version, actor.settings(), "setup.models.facts", func(models *settings.Models) error {
		models.PublishedFactsOff = !on
		return nil
	})
}

// ModelDetail is what the page of the model ref shows beyond its entry.
func (s *Service) ModelDetail(ctx context.Context, ref string) (ModelDetail, error) {
	if s.catalog == nil {
		return ModelDetail{}, nil
	}
	row, err := s.settings.ReadModels(ctx)
	if err != nil {
		return ModelDetail{}, err
	}
	return s.catalog.Detail(ctx, row.Models, ref)
}

// changeEntry saves change's version of the entry ref at version. While
// jobs use the model, a change to what they send needs confirmed, and one
// that would stop a job is refused: both fail with an *EntryChangeError.
func (s *Service) changeEntry(ctx context.Context, actor Actor, version int64, ref string, kind modelregistry.Kind, action string, confirmed bool,
	change func(before modelregistry.Model) modelregistry.Model) (settings.ModelsRow, error) {
	var roles []RoleView
	if s.catalog != nil {
		current, err := s.settings.ReadModels(ctx)
		if err != nil {
			return settings.ModelsRow{}, err
		}
		if roles, err = s.catalog.Roles(ctx, current.Models); err != nil {
			return settings.ModelsRow{}, err
		}
	}
	return s.settings.UpdateModels(ctx, version, actor.settings(), action, func(models *settings.Models) error {
		index := slices.IndexFunc(models.Available, func(m modelregistry.Model) bool { return m.Ref() == ref })
		if index < 0 {
			return fmt.Errorf("%w: there is no model %s", settings.ErrInvalid, ref)
		}
		before := models.Available[index]
		entry := change(before)
		if err := entry.Validate(kind); err != nil {
			return fmt.Errorf("%w: %s", settings.ErrInvalid, err.Error())
		}
		changes := entryChanges(before, entry)
		failed := &EntryChangeError{Changes: changes}
		for _, role := range roles {
			if models.Roles[role.Key] != ref || role.Unavailable != "" {
				continue
			}
			failed.Jobs = append(failed.Jobs, role.Title)
			// A job the model does not serve now is not one the change
			// stops.
			if s.catalog.CanServe(*models, role.Key, before) != nil {
				continue
			}
			if err := s.catalog.CanServe(*models, role.Key, entry); err != nil {
				failed.Stops = append(failed.Stops, role.Title+" uses this model and would stop: "+err.Error()+
					". Give "+role.Title+" another model, or set it to Off, on the Models page first.")
			}
		}
		if len(changes) > 0 && len(failed.Jobs) > 0 && (len(failed.Stops) > 0 || !confirmed) {
			return failed
		}
		models.Available = slices.Clone(models.Available)
		models.Available[index] = entry
		return nil
	})
}

// RemoveModelEntry deletes the entry ref at version. A role still given the
// model refuses it.
func (s *Service) RemoveModelEntry(ctx context.Context, actor Actor, version int64, ref string) (settings.ModelsRow, error) {
	return s.settings.UpdateModels(ctx, version, actor.settings(), "setup.models.entry.remove", func(models *settings.Models) error {
		for role, saved := range models.Roles {
			if saved == ref {
				return fmt.Errorf("%w: a job (%s) still uses %s; give it another model first", settings.ErrInvalid, role, ref)
			}
		}
		models.Available = slices.DeleteFunc(slices.Clone(models.Available), func(m modelregistry.Model) bool { return m.Ref() == ref })
		return nil
	})
}

// embeddedChunks counts the chunks that have vectors.
func (s *Service) embeddedChunks(ctx context.Context) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE embedded_at IS NOT NULL`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count embedded chunks: %w", err)
	}
	return count, nil
}

// fillProgress counts the chunks whose vectors are in collection, and all
// the chunks that take one.
func (s *Service) fillProgress(ctx context.Context, collection string) (done, total int64, err error) {
	if s.pool == nil {
		return 0, 0, nil
	}
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE c.embedded_at IS NOT NULL AND c.embedded_collection = $1), count(*)
		  FROM event_chunks c JOIN events e ON e.id = c.event_id
		 WHERE NOT e.deleted`, collection).Scan(&done, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count filled chunks: %w", err)
	}
	return done, total, nil
}

// EmbeddingChoice is a choice of the embedding model from the Models page.
type EmbeddingChoice struct {
	// Ref is the model's modelregistry.Ref.
	Ref string
	// Adopt names an existing collection to keep using: the one the
	// deployment's vectors are already in. Empty names the model's own,
	// which a choice creates.
	Adopt string
	// Reembed confirms that every chunk is embedded again, which choosing
	// a model in place of another, or a new collection while chunks have
	// vectors, needs.
	Reembed bool
}

// ErrReembedUnconfirmed is a choice that would embed every chunk again
// without the confirmation it needs.
var ErrReembedUnconfirmed = errors.New("choosing this embedding model embeds every chunk again: confirm it to go ahead")

// EmbeddingOutcome is what a choice of the embedding model did.
type EmbeddingOutcome int

const (
	// EmbeddingUnchanged is a choice of what is already in use.
	EmbeddingUnchanged EmbeddingOutcome = iota
	// EmbeddingInUse is a model search uses from now on: the first choice
	// on an empty corpus, an adopted collection, or the same model from
	// another provider.
	EmbeddingInUse
	// EmbeddingFilling is a model whose collection the embedder fills
	// first; search switches to it when it holds every chunk.
	EmbeddingFilling
)

// ChooseEmbedding makes choice the embedding model at version. Search never
// moves to a collection that is not full:
//
//   - The model search already uses, with nothing else asked, changes
//     nothing: its collection, adopted or made by Diderot, is kept.
//   - The first model of a corpus with no vectors, and an existing
//     collection named to adopt, are used at once.
//   - Any other choice embeds every chunk again, which choice must confirm.
//     The model is saved as the one being filled: the leader's embedder
//     writes every chunk into its collection while search keeps using the
//     model it had (or stays off, when it had none), and search moves over
//     when the new collection holds every chunk.
//
// One embedding call learns a new model's dimension, and its collection is
// created, or the adopted one checked, before anything is saved.
func (s *Service) ChooseEmbedding(ctx context.Context, actor Actor, version int64, choice EmbeddingChoice) (settings.ModelsRow, EmbeddingOutcome, error) {
	fail := func(err error) (settings.ModelsRow, EmbeddingOutcome, error) {
		return settings.ModelsRow{}, EmbeddingUnchanged, err
	}
	if s.catalog == nil {
		return fail(fmt.Errorf("%w: this server has no models", settings.ErrInvalid))
	}
	current, err := s.settings.ReadModels(ctx)
	if err != nil {
		return fail(err)
	}
	if current.Version != version {
		return fail(settings.ErrConflict)
	}
	provider, model, ok := modelregistry.ParseRef(strings.TrimSpace(choice.Ref))
	if !ok {
		return fail(fmt.Errorf("%w: choose an embedding model", settings.ErrInvalid))
	}
	adopt := strings.TrimSpace(choice.Adopt)
	if adopt != "" && !qdrantstore.ValidCollectionName(adopt) {
		return fail(fmt.Errorf("%w: a collection name is letters, digits, _ or -", settings.ErrInvalid))
	}
	prior, filling := current.Embedding, current.Filling
	same := func(e *settings.Embedding) bool {
		return e != nil && e.Provider == provider && e.Model == model && (adopt == "" || adopt == e.Collection)
	}
	switch {
	case same(filling), same(prior) && filling == nil:
		return current, EmbeddingUnchanged, nil
	case filling != nil && (same(prior) || adopt != ""):
		return fail(fmt.Errorf("%w: %s is being embedded to replace the embedding model, which can't be undone part way. Search moves to it when it is full; choose again then",
			settings.ErrInvalid, filling.Model))
	}
	options, _, _, err := s.catalog.Embedding(ctx, current.Models)
	if err != nil {
		return fail(err)
	}
	if !slices.ContainsFunc(options, func(o ModelOption) bool { return o.Ref == choice.Ref }) {
		return fail(fmt.Errorf("%w: no enabled provider offers %s", settings.ErrInvalid, choice.Ref))
	}
	testCtx, cancel := context.WithTimeout(ctx, ModelTestTimeout)
	dimensions, err := s.catalog.EmbeddingDimensions(testCtx, current.Models, choice.Ref)
	cancel()
	if err != nil {
		var testErr *modelregistry.TestError
		if errors.As(err, &testErr) {
			return fail(&ModelTestError{Role: "embeddings", Model: model, Reason: testErr.Reason})
		}
		return fail(err)
	}
	next := settings.Embedding{Provider: provider, Model: model, Dimensions: dimensions, Collection: adopt}
	if next.Collection == "" {
		next.Collection = qdrantstore.CollectionName(model, dimensions)
	}
	if prior != nil && prior.Model != model && prior.Collection == next.Collection {
		return fail(fmt.Errorf("%w: the collection %s holds %s's vectors, which another model can't share", settings.ErrInvalid, next.Collection, prior.Model))
	}
	embedded, err := s.embeddedChunks(ctx)
	if err != nil {
		return fail(err)
	}
	outcome := EmbeddingFilling
	switch {
	case prior != nil && prior.Model == model && prior.Dimensions == dimensions && adopt == "":
		// The same model from another provider: its vectors, and the
		// collection they are in, stay.
		next.Collection, outcome = prior.Collection, EmbeddingInUse
	case adopt != "", prior == nil && embedded == 0:
		outcome = EmbeddingInUse
	case !choice.Reembed:
		return fail(ErrReembedUnconfirmed)
	}
	if err := s.catalog.PrepareCollection(ctx, next, adopt != ""); err != nil {
		return fail(err)
	}
	row, err := s.settings.UpdateModels(ctx, version, actor.settings(), "setup.models.embedding", func(models *settings.Models) error {
		if outcome == EmbeddingFilling {
			models.Filling = &next
			return nil
		}
		models.Embedding = &next
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return row, outcome, nil
}
