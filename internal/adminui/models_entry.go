package adminui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

var modelEntryPage = mustPage("model.html")

// modelPath is a model entry's page; ?ref= names the model.
const modelPath = modelsPath + "/model"

// modelHref is the page of the model entry ref.
func modelHref(ref string) string { return modelPath + "?ref=" + url.QueryEscape(ref) }

// modelEntryData is a model entry's page: what its tests and its provider's
// listing found, and the request fields an administrator sets.
type modelEntryData struct {
	pageData
	CSRFToken     string
	Notice, Error string
	// ModelsVersion is the models row's version, which a save posts back.
	ModelsVersion int64
	Entry         modelregistry.Model
	// Jobs are the jobs that use it.
	Jobs []string
	// Efforts are every effort, to tick those it accepts.
	Efforts     []string
	Retentions  []string
	ExtraBody   string
	FieldErrors map[string]string
	// Changes are what a save that was not confirmed would change for the
	// jobs that use the model, and Stops the jobs it would stop, which
	// refuses it. Confirmed is the confirmation the form carried.
	Changes   []setup.EntryChange
	Stops     []string
	Confirmed bool
	// PublishedChanges are what using the published facts would change,
	// when that was asked and not confirmed.
	PublishedChanges []setup.EntryChange
	// Detail is where each fact came from, what is published about the
	// model, and the catalog entry it is matched to.
	Detail setup.ModelDetail
}

// OtherPin is the administrator's choice of catalog entry when it is none
// of the choices the page lists.
func (d modelEntryData) OtherPin() string {
	pin := d.Detail.Match.Pin
	if pin == "" || pin == modelregistry.CatalogNone {
		return ""
	}
	for _, suggestion := range d.Detail.Match.Suggestions {
		if suggestion.Ref == pin {
			return ""
		}
	}
	return pin
}

// Found lists, in words, the facts a job's test found; the others were set
// by an administrator or by the installation.
func (d modelEntryData) Found() []string {
	var found []string
	for _, fact := range d.Entry.LearnedFacts() {
		found = append(found, factTitle(fact))
	}
	return found
}

// factTitle is what the page calls a fact of an entry.
func factTitle(fact string) string {
	switch fact {
	case modelregistry.FactProtocols:
		return "the protocols it answers on"
	case modelregistry.FactImages:
		return "whether it accepts images"
	case modelregistry.FactEfforts:
		return "its reasoning efforts"
	case modelregistry.FactServiceTier:
		return "whether it takes a service tier"
	case modelregistry.FactCacheRetention:
		return "its prompt cache retention"
	case modelregistry.FactTokenParam:
		return "the chat output limit field"
	case modelregistry.FactTemperature:
		return "whether chat requests carry a temperature"
	case modelregistry.FactChatEffort:
		return "the effort chat jobs ask for"
	case modelregistry.FactAllowance:
		return "the reasoning token allowance"
	case modelregistry.FactExtraBody:
		return "the extra request fields"
	}
	return fact
}

// submittedEntry is entry as the form would save it, to show the form again
// as it was submitted.
func submittedEntry(entry modelregistry.Model, update setup.ModelEntryUpdate) modelregistry.Model {
	entry.ReasoningEfforts, entry.ServiceTier, entry.PromptCacheRetention = update.ReasoningEfforts, update.ServiceTier, update.PromptCacheRetention
	entry.Images, entry.ChatOmitTemperature = update.Images, update.ChatOmitTemperature
	entry.ChatReasoningEffort = update.ChatReasoningEffort
	entry.ChatTokenParam = ""
	if update.ChatMaxCompletionTokens {
		entry.ChatTokenParam = "max_completion_tokens"
	}
	entry.ChatReasoningTokenAllowance, entry.ChatExtraBody = update.ChatReasoningTokenAllowance, update.ChatExtraBody
	return entry
}

// Ref is the entry's ref.
func (d modelEntryData) Ref() string { return d.Entry.Ref() }

// Href is the entry's page.
func (d modelEntryData) Href() string { return modelHref(d.Entry.Ref()) }

// AcceptsEffort reports whether the entry lists effort.
func (d modelEntryData) AcceptsEffort(effort string) bool {
	return d.Entry.SupportsReasoningEffort(effort)
}

// MaxCompletionTokens reports that chat requests carry max_completion_tokens.
func (d modelEntryData) MaxCompletionTokens() bool {
	return d.Entry.ChatTokenParam == "max_completion_tokens"
}

// TestedFor names the jobs whose test it passed.
func (d modelEntryData) TestedFor() []string {
	var titles []string
	for _, role := range d.Entry.TestedFor {
		titles = append(titles, roleTitle(role))
	}
	return titles
}

// TestedFor names the jobs whose test m passed.
func (d modelsData) TestedFor(m modelregistry.Model) string {
	return strings.Join(modelEntryData{Entry: m}.TestedFor(), ", ")
}

// roleTitle is what pages call a role.
func roleTitle(role modelregistry.Role) string {
	switch role {
	case modelregistry.RoleAsk:
		return "Ask"
	case modelregistry.RoleTitles:
		return "Titles"
	case modelregistry.RoleSessionSummaryMap:
		return "Session summaries, part by part"
	case modelregistry.RoleSessionSummaryReduce:
		return "Session summaries, whole session"
	case modelregistry.RoleImageCaptions:
		return "Image captions"
	case modelregistry.RoleDocumentExtraction:
		return "Document extraction"
	case modelregistry.RoleThreadSummaries:
		return "Thread summaries"
	}
	return string(role)
}

// handleEmbeddingSave chooses the embedding model. A choice that embeds
// every chunk again shows the page again asking to confirm it.
func (s *Server) handleEmbeddingSave(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	choice := setup.EmbeddingChoice{
		Ref: r.Form.Get("embedding"), Adopt: strings.TrimSpace(r.Form.Get("adopt")),
		Reembed: r.Form.Get("confirm") == "reembed",
	}
	_, outcome, err := s.setup.ChooseEmbedding(r.Context(), s.setupActor(r), version, choice)
	var failed *setup.ModelTestError
	switch {
	case errors.Is(err, setup.ErrReembedUnconfirmed):
		s.renderModelsWith(w, r, http.StatusUnprocessableEntity, nil, "", func(data *modelsData) {
			data.Reembed, data.EmbeddingRef, data.Adopt = true, choice.Ref, choice.Adopt
		})
		return
	case errors.As(err, &failed):
		s.renderModelsWith(w, r, http.StatusUnprocessableEntity, nil, failed.Error(), func(data *modelsData) {
			data.EmbeddingRef, data.Adopt = choice.Ref, choice.Adopt
		})
		return
	case errors.Is(err, settings.ErrInvalid):
		message, _ := setupNotice(err)
		s.renderModelsWith(w, r, http.StatusUnprocessableEntity, nil, message, func(data *modelsData) {
			data.EmbeddingRef, data.Adopt = choice.Ref, choice.Adopt
		})
		return
	}
	notice, ok := setupNotice(err)
	if !ok {
		s.logger.Error("embedding save failed", "err", err)
		http.Error(w, "embedding save failed", http.StatusInternalServerError)
		return
	}
	if err == nil {
		switch outcome {
		case setup.EmbeddingUnchanged:
			notice = "That is already the embedding model. Nothing was changed."
		case setup.EmbeddingFilling:
			notice = "Embedding every passage with the new model has started. Search keeps using what it has until the new collection is full, then moves to it."
		default:
			notice = "Embedding model saved. Search uses it within seconds, and the embedder works through what is waiting."
		}
	}
	redirectWithResult(w, r, modelsPath, notice, err != nil)
}

// modelEntry reads the entry ?ref= names and the models row's version.
func (s *Server) modelEntry(w http.ResponseWriter, r *http.Request) (modelEntryData, bool) {
	ref := r.URL.Query().Get("ref")
	if r.Method == http.MethodPost {
		ref = r.Form.Get("ref")
	}
	models, err := s.setup.Models(r.Context())
	if err != nil {
		s.logger.Error("models read failed", "err", err)
		http.Error(w, "models read failed", http.StatusInternalServerError)
		return modelEntryData{}, false
	}
	index := slices.IndexFunc(models.Available, func(m modelregistry.Model) bool { return m.Ref() == ref })
	if index < 0 {
		s.renderNotFound(w, r, missingModel)
		return modelEntryData{}, false
	}
	data := modelEntryData{
		ModelsVersion: models.Version, Entry: models.Available[index],
		Efforts: modelregistry.ReasoningEfforts, Retentions: modelregistry.PromptCacheRetentions,
	}
	if len(data.Entry.ChatExtraBody) > 0 {
		raw, _ := json.MarshalIndent(data.Entry.ChatExtraBody, "", "  ")
		data.ExtraBody = string(raw)
	}
	for _, role := range models.Roles {
		if role.Saved == ref {
			data.Jobs = append(data.Jobs, role.Title)
		}
	}
	if data.Detail, err = s.setup.ModelDetail(r.Context(), ref); err != nil {
		s.logger.Error("model detail read failed", "err", err)
		http.Error(w, "models read failed", http.StatusInternalServerError)
		return modelEntryData{}, false
	}
	return data, true
}

// entryKind is the kind of the provider that serves entry.
func (s *Server) entryKind(r *http.Request, entry modelregistry.Model) modelregistry.Kind {
	if s.providers != nil {
		if view, err := s.providers.View(r.Context(), modelprovider.Row(entry.Provider)); err == nil {
			if k, _ := view.Settings[modelprovider.FieldKind].(string); k != "" {
				return modelregistry.Kind(k)
			}
		}
	}
	return modelregistry.KindOpenAI
}

// handleModelMatch saves which published catalog entry describes the model.
func (s *Server) handleModelMatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	data, ok := s.modelEntry(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	pin := r.Form.Get("match")
	if pin == "other" {
		pin = strings.TrimSpace(r.Form.Get("match_other"))
		if pin == "" {
			redirectWithResult(w, r, data.Href()+"#match", "Enter the entry as provider/id, or choose another option.", true)
			return
		}
	}
	_, err = s.setup.SetModelMatch(r.Context(), s.setupActor(r), version, data.Ref(), pin)
	switch {
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrConflict):
		message, _ := setupNotice(err)
		redirectWithResult(w, r, data.Href()+"#match", message, true)
	case err != nil:
		s.logger.Error("model match save failed", "err", err)
		http.Error(w, "model save failed", http.StatusInternalServerError)
	default:
		redirectWithResult(w, r, data.Href(), "Saved. No request changes: the page now shows what that entry says.", false)
	}
}

// handleModelPublished puts what is published about the model on its entry,
// asking first when jobs use it.
func (s *Server) handleModelPublished(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	data, ok := s.modelEntry(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	_, err = s.setup.UsePublishedFacts(r.Context(), s.setupActor(r), version, data.Ref(), s.entryKind(r, data.Entry), r.Form.Get("confirm") == "change")
	var changeErr *setup.EntryChangeError
	switch {
	case errors.As(err, &changeErr):
		data.Error = changeErr.Error()
		data.PublishedChanges, data.Stops = changeErr.Changes, changeErr.Stops
		s.renderModelEntry(w, r, data, http.StatusUnprocessableEntity)
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrConflict):
		message, _ := setupNotice(err)
		redirectWithResult(w, r, data.Href(), message, true)
	case err != nil:
		s.logger.Error("model published facts save failed", "err", err)
		http.Error(w, "model save failed", http.StatusInternalServerError)
	default:
		redirectWithResult(w, r, data.Href(), "Saved. "+data.Entry.ID+" is now called as its published facts say.", false)
	}
}

func (s *Server) handleModelEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	data, ok := s.modelEntry(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	data.Notice, data.Error = cleanNotice(query.Get("notice")), cleanNotice(query.Get("error"))
	s.renderModelEntry(w, r, data, http.StatusOK)
}

func (s *Server) renderModelEntry(w http.ResponseWriter, r *http.Request, data modelEntryData, code int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.settingsPage(r, data.Entry.ID, tabSettingsModels)
	data.CSRFToken = token
	s.renderStatus(w, modelEntryPage, data, code)
}

// handleModelEntrySave saves an administrator's correction of the entry.
func (s *Server) handleModelEntrySave(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	data, ok := s.modelEntry(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	data.Confirmed = r.Form.Get("confirm") == "change"
	update := setup.ModelEntryUpdate{
		ReasoningEfforts:        r.Form["effort"],
		ServiceTier:             r.Form.Get("service_tier") == "true",
		PromptCacheRetention:    r.Form.Get("prompt_cache_retention"),
		Images:                  r.Form.Get("images") == "true",
		ChatMaxCompletionTokens: r.Form.Get("max_completion_tokens") == "true",
		ChatOmitTemperature:     r.Form.Get("omit_temperature") == "true",
		ChatReasoningEffort:     r.Form.Get("chat_effort"),
	}
	data.FieldErrors = map[string]string{}
	data.ExtraBody = r.Form.Get("extra_body")
	if raw := strings.TrimSpace(r.Form.Get("allowance")); raw != "" {
		if update.ChatReasoningTokenAllowance, err = strconv.Atoi(raw); err != nil || update.ChatReasoningTokenAllowance < 0 {
			data.FieldErrors["allowance"] = "Enter a whole number of tokens, 0 or more."
		}
	}
	if raw := strings.TrimSpace(data.ExtraBody); raw != "" {
		if err := json.Unmarshal([]byte(raw), &update.ChatExtraBody); err != nil {
			data.FieldErrors["extra_body"] = "Enter a JSON object, such as {\"thinking_config\": {\"thinkingBudget\": 0}}."
		}
	}
	if len(data.FieldErrors) > 0 {
		data.Error = "Not saved: fix the marked settings."
		s.renderModelEntry(w, r, data, http.StatusBadRequest)
		return
	}
	kind := s.entryKind(r, data.Entry)
	_, err = s.setup.UpdateModelEntry(r.Context(), s.setupActor(r), version, data.Ref(), kind, update, data.Confirmed)
	var changeErr *setup.EntryChangeError
	switch {
	case errors.As(err, &changeErr):
		// Show the form as submitted, with what it changes and for whom.
		data.Entry = submittedEntry(data.Entry, update)
		data.Error = changeErr.Error()
		data.Changes, data.Stops = changeErr.Changes, changeErr.Stops
		s.renderModelEntry(w, r, data, http.StatusUnprocessableEntity)
		return
	case errors.Is(err, settings.ErrInvalid):
		data.Error = "Not saved: " + strings.TrimPrefix(err.Error(), settings.ErrInvalid.Error()+": ") + "."
		s.renderModelEntry(w, r, data, http.StatusBadRequest)
		return
	case errors.Is(err, settings.ErrConflict):
		redirectWithResult(w, r, data.Href(), "The models changed since this page loaded. Check them and try again.", true)
		return
	case err != nil:
		s.logger.Error("model entry save failed", "err", err)
		http.Error(w, "model save failed", http.StatusInternalServerError)
		return
	}
	redirectWithResult(w, r, data.Href(), "Saved. The jobs that use "+data.Entry.ID+" use the change within seconds.", false)
}

// handleModelEntryRemove removes an entry no job uses.
func (s *Server) handleModelEntryRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	data, ok := s.modelEntry(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	_, err = s.setup.RemoveModelEntry(r.Context(), s.setupActor(r), version, data.Ref())
	switch {
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrConflict):
		message, _ := setupNotice(err)
		redirectWithResult(w, r, data.Href(), message, true)
	case err != nil:
		s.logger.Error("model entry remove failed", "err", err)
		http.Error(w, "model remove failed", http.StatusInternalServerError)
	default:
		redirectWithResult(w, r, modelsPath, data.Entry.ID+" is removed. Choosing it for a job tests it again.", false)
	}
}
