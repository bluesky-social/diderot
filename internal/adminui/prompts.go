package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// The Prompts section: the prompts of the live version by what they are for,
// one prompt's editor and history, the draft, the shipped text, the history
// of versions, one version, the tools Ask can use, and an Ask prompt from
// before versions existed.
var (
	promptsPage        = mustPage("prompts.html", "prompt_parts.html", "setup_progress.html")
	promptPage         = mustPage("prompt.html", "prompt_parts.html", "setup_progress.html")
	promptDraftPage    = mustPage("prompt_draft.html", "prompt_parts.html")
	promptShippedPage  = mustPage("prompt_shipped.html", "prompt_parts.html")
	promptVersionsPage = mustPage("prompt_versions.html", "prompt_parts.html")
	promptVersionPage  = mustPage("prompt_version.html", "prompt_parts.html")
	promptToolsPage    = mustPage("prompt_tools.html", "prompt_parts.html")
	legacyPromptPage   = mustPage("prompt_legacy.html")
)

const (
	promptsPath           = "/admin/prompts"
	promptDraftPath       = promptsPath + "/draft"
	promptShippedPath     = promptsPath + "/shipped"
	promptHistoryPageSize = 10
	promptVersionPageSize = 25
	// promptDiffsOpen is how many changed prompts a version page shows
	// opened; beyond it each starts closed.
	promptDiffsOpen = 3
	// maxPromptFormBytes leaves room for the largest prompt to grow: ask.base
	// is about 33 KB, and form encoding inflates it.
	maxPromptFormBytes = 1 << 20
)

var errPromptNotFound = errors.New("prompt not found")

func (s *Server) mountPromptRoutes(mux Mux) {
	mux.Handle("GET "+promptsPath, s.authedBrowser(http.HandlerFunc(s.handlePrompts)))
	mux.Handle("GET "+promptDraftPath, s.authedBrowser(http.HandlerFunc(s.handlePromptDraft)))
	mux.Handle("POST "+promptDraftPath+"/publish", s.authedBrowser(http.HandlerFunc(s.handlePromptDraftPublish)))
	mux.Handle("POST "+promptDraftPath+"/discard", s.authedBrowser(http.HandlerFunc(s.handlePromptDraftDiscard)))
	mux.Handle("GET "+promptShippedPath, s.authedBrowser(http.HandlerFunc(s.handlePromptShipped)))
	mux.Handle("POST "+promptShippedPath, s.authedBrowser(http.HandlerFunc(s.handlePromptShippedTake)))
	mux.Handle("GET "+promptsPath+"/versions", s.authedBrowser(http.HandlerFunc(s.handlePromptVersions)))
	mux.Handle("GET "+promptsPath+"/versions/{id}", s.authedBrowser(http.HandlerFunc(s.handlePromptVersion)))
	mux.Handle("POST "+promptsPath+"/versions/{id}/activate", s.authedBrowser(http.HandlerFunc(s.handlePromptVersionActivate)))
	mux.Handle("GET "+promptsPath+"/tools", s.authedBrowser(http.HandlerFunc(s.handlePromptTools)))
	mux.Handle("GET "+promptsPath+"/legacy/{id}", s.authedBrowser(http.HandlerFunc(s.handleLegacyPrompt)))
	mux.Handle("GET "+promptsPath+"/{key}", s.authedBrowser(http.HandlerFunc(s.handlePrompt)))
	mux.Handle("POST "+promptsPath+"/{key}", s.authedBrowser(http.HandlerFunc(s.handlePromptSave)))
	mux.Handle("POST "+promptsPath+"/{key}/discard", s.authedBrowser(http.HandlerFunc(s.handlePromptDiscard)))
	mux.Handle("POST "+promptsPath+"/{key}/restore", s.authedBrowser(http.HandlerFunc(s.handlePromptRestore)))
}

func (s *Server) promptStore() *promptcatalog.Store { return promptcatalog.NewStore(s.pool) }

// promptsPage is pageData for a page of the Prompts section.
func (s *Server) promptsPage(r *http.Request, title string) pageData {
	page := s.page(r, title, navPrompts)
	page.Stylesheet = "prompts.css"
	return page
}

// promptSetup is the prompts step's way on while the install is in setup.
func (s *Server) promptSetup(ctx context.Context) *setupProgress {
	if s.setup == nil {
		return nil
	}
	return s.setupProgress(ctx, setup.StepPrompts)
}

// Shipped says how a prompt's live text compares with the text this version
// of Diderot ships.
const (
	shippedSame    = "same"
	shippedDiffers = "differs"
)

// promptRow is a prompt as lists show it: its name, key and description, and
// the version that last changed its text.
type promptRow struct {
	Key         string
	Name        string
	Description string
	ReleaseID   int64
	ChangedAt   time.Time
	ChangedBy   string
	// Example reports that its text is still the one the example installed.
	Example bool
	// Note marks an organization note, Unset one the live prompts do not
	// have.
	Note  bool
	Unset bool
	// InDraft marks a prompt the draft changes.
	InDraft bool
	// Shipped is shippedSame or shippedDiffers for a prompt this version
	// ships, and empty for any other.
	Shipped string
}

func newPromptRow(state promptState, key string, latest promptcatalog.PromptVersion) promptRow {
	row := promptRow{
		Key: key, Name: promptName(key), Description: promptDescriptionOf(state.Live.Bundle, key),
		ReleaseID: latest.ReleaseID, ChangedAt: latest.ActivatedAt, ChangedBy: latest.ActivatedBy,
		Example: latest.Example, Note: promptcatalog.IsNote(key),
	}
	live, inLive := state.liveText(key)
	row.Unset = !inLive
	if row.Unset {
		row.ReleaseID, row.ChangedAt, row.ChangedBy, row.Example = 0, time.Time{}, "", false
	}
	_, row.InDraft = state.Entry(key)
	if shipped, ok := state.shippedText(key); ok && inLive {
		row.Shipped = shippedDiffers
		if shipped == live {
			row.Shipped = shippedSame
		}
	}
	return row
}

type promptGroupView struct {
	ID      string
	Title   string
	Summary string
	Prompts []promptRow
}

// promptLive is the live version: which, since when, and who made it live.
type promptLive struct {
	ReleaseID int64
	Since     time.Time
	By        string
	// Note is the version's note, empty when Diderot wrote it.
	Note    string
	Example bool
}

type promptsData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	Live      promptLive
	Groups    []promptGroupView
	Setup     *setupProgress
	// KeepExample offers the setup step's other way to finish while the
	// example is live: keeping it. OrgVersion is what its form posts.
	KeepExample bool
	OrgVersion  int64
	// Draft is the draft, nil while it is empty.
	Draft *promptDraftSummary
	// ShippedDiffer counts the prompts whose text, with the draft applied,
	// differs from the text this version of Diderot ships.
	ShippedDiffer int
}

// promptDraftSummary is the draft as the Prompts list shows it.
type promptDraftSummary struct {
	Count int
	Names string
	// OutOfDate counts the changes written against live text that has
	// changed since.
	OutOfDate int
	UpdatedAt time.Time
	UpdatedBy string
}

func (s *Server) handlePrompts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := s.loadPrompts(ctx)
	if err != nil {
		s.logger.Error("admin prompt catalog query failed", "err", err)
		http.Error(w, "prompt catalog query failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.promptsPage(r, "Prompts")
	data.Notice, data.Error = cleanNotice(r.URL.Query().Get("notice")), cleanNotice(r.URL.Query().Get("error"))
	data.Setup = s.promptSetup(ctx)
	if data.Setup != nil && !data.Setup.StepDone && data.Live.Example {
		row, err := s.setup.ReadOrg(ctx)
		if err != nil {
			s.logger.Error("organization read failed", "err", err)
			http.Error(w, "organization read failed", http.StatusInternalServerError)
			return
		}
		data.KeepExample, data.OrgVersion = true, row.Version
		if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
			http.Error(w, "CSRF token failed", http.StatusInternalServerError)
			return
		}
	}
	s.render(w, promptsPage, data)
}

func (s *Server) loadPrompts(ctx context.Context) (promptsData, error) {
	store := s.promptStore()
	state, err := s.loadPromptState(ctx)
	if err != nil {
		return promptsData{}, err
	}
	by, since, err := store.ActiveSince(ctx)
	if err != nil {
		return promptsData{}, err
	}
	latest, err := store.LatestVersions(ctx)
	if err != nil {
		return promptsData{}, err
	}
	data := promptsData{
		Live: newPromptLive(state.Live, by, since), Draft: state.summary(),
		ShippedDiffer: len(state.shippedChanges()),
	}
	listed := map[string]bool{}
	for _, group := range promptGroups {
		view := promptGroupView{ID: group.ID, Title: group.Title, Summary: group.Summary}
		for _, named := range group.Prompts {
			_, inLive := state.liveText(named.Key)
			_, inDraft := state.Entry(named.Key)
			if inLive || inDraft || promptcatalog.IsNote(named.Key) {
				view.Prompts = append(view.Prompts, newPromptRow(state, named.Key, latest[named.Key]))
				listed[named.Key] = true
			}
		}
		if len(view.Prompts) > 0 {
			data.Groups = append(data.Groups, view)
		}
	}
	other := promptGroupView{ID: otherPrompts.ID, Title: otherPrompts.Title, Summary: otherPrompts.Summary}
	for _, prompt := range state.Live.Bundle.Prompts {
		if !listed[prompt.Key] {
			other.Prompts = append(other.Prompts, newPromptRow(state, prompt.Key, latest[prompt.Key]))
		}
	}
	if len(other.Prompts) > 0 {
		data.Groups = append(data.Groups, other)
	}
	return data, nil
}

func newPromptLive(release promptcatalog.Release, by string, since time.Time) promptLive {
	return promptLive{ReleaseID: release.ID, Since: since, By: by, Note: personNote(release.CreatedBy, release.Note), Example: release.Example}
}

// personNote is a version's note unless Diderot wrote it.
func personNote(createdBy, note string) string {
	if strings.HasPrefix(createdBy, systemActorPrefix) {
		return ""
	}
	return note
}

// promptVariable is one variable a prompt may use, what it is replaced with,
// its value now, and whether this prompt uses it. Lines counts the lines of
// a value longer than one.
type promptVariable struct {
	Name      string
	Meaning   string
	Value     string
	Multiline bool
	Lines     int
	Used      bool
}

func promptVariables(body string) []promptVariable {
	values := promptcatalog.Values()
	var variables []promptVariable
	for _, name := range promptcatalog.VariableNames() {
		placeholder := "{{" + name + "}}"
		lines := strings.Count(values[name], "\n") + 1
		variables = append(variables, promptVariable{
			Name: placeholder, Meaning: promptVariableMeanings[name], Value: values[name],
			Multiline: lines > 1, Lines: lines, Used: strings.Contains(body, placeholder),
		})
	}
	return variables
}

// promptVersionView is one version of a prompt's text in its history.
type promptVersionView struct {
	ReleaseID   int64
	ActivatedAt time.Time
	ActivatedBy string
	Note        string
	Example     bool
	// Live marks the version whose text is live now.
	Live bool
}

func newPromptVersionView(version promptcatalog.PromptVersion) promptVersionView {
	view := promptVersionView{
		ReleaseID: version.ReleaseID, ActivatedAt: version.ActivatedAt, ActivatedBy: version.ActivatedBy,
		Note: version.Note, Example: version.Example,
	}
	if version.Example {
		view.Note = ""
	}
	return view
}

// promptChangeView is one version opened from the history: its text and how
// it changed the text before it.
type promptChangeView struct {
	promptVersionView
	Body string
	// First marks the version with no text before it.
	First bool
	Diff  promptDiff
	// Drafted marks a version whose text is the draft's.
	Drafted bool
}

// promptDraftView is the draft's change to one prompt.
type promptDraftView struct {
	UpdatedAt time.Time
	UpdatedBy string
	// Remove marks a removal; Diff is the change from the live text.
	Remove bool
	Diff   promptDiff
	// OutOfDate marks a change written against live text that has changed
	// since.
	OutOfDate bool
}

type promptPageData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	// Conflict is a save whose page showed a draft or live text that has
	// changed since.
	Conflict bool
	Prompt   promptRow
	// GroupID and GroupTitle are the group the prompt lists under.
	GroupID    string
	GroupTitle string
	// Body is the editor's text: the draft's, or else the live text.
	Body string
	// DraftSHA256 and LiveSHA256 identify the draft and live text the page
	// shows; a save applies only while both are unchanged.
	DraftSHA256 string
	LiveSHA256  string
	// Draft is the draft's change to this prompt, nil when it has none.
	Draft     *promptDraftView
	Variables []promptVariable
	History   []promptVersionView
	Pages     pagination
	Viewing   *promptChangeView
	Setup     *setupProgress
}

// EditorRows sizes the editor to its text, allowing for wrapped lines.
func (d promptPageData) EditorRows() int {
	rows := 0
	for line := range strings.SplitSeq(d.Body, "\n") {
		rows += 1 + len(line)/110
	}
	return min(max(rows+1, 8), 32)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.PathValue("key")
	query := r.URL.Query()
	before, _ := strconv.ParseInt(query.Get("before"), 10, 64)
	data, err := s.loadPrompt(ctx, key, max(before, 0))
	if errors.Is(err, errPromptNotFound) {
		s.renderNotFound(w, r, missingPrompt)
		return
	}
	if err != nil {
		s.logger.Error("admin prompt query failed", "key", key, "err", err)
		http.Error(w, "prompt query failed", http.StatusInternalServerError)
		return
	}
	if release, parseErr := strconv.ParseInt(query.Get("version"), 10, 64); parseErr == nil {
		data.Viewing, err = s.loadPromptChange(ctx, key, release)
		switch {
		case errors.Is(err, promptcatalog.ErrNotFound):
			data.Error = fmt.Sprintf("Version %d did not change this prompt.", release)
		case err != nil:
			s.logger.Error("admin prompt version query failed", "key", key, "err", err)
			http.Error(w, "prompt query failed", http.StatusInternalServerError)
			return
		default:
			data.Viewing.Live = promptcatalog.BodySHA256(data.Viewing.Body) == data.LiveSHA256
			data.Viewing.Drafted = data.Draft != nil && data.Viewing.Body == data.Body
		}
	}
	data.Notice = cleanNotice(query.Get("notice"))
	data.Error = firstNonEmpty(data.Error, cleanNotice(query.Get("error")))
	s.renderPrompt(w, r, data, http.StatusOK)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// loadPrompt reads one prompt's live and draft text and a page of its
// history, before an activation when that is not zero.
func (s *Server) loadPrompt(ctx context.Context, key string, before int64) (promptPageData, error) {
	store := s.promptStore()
	state, err := s.loadPromptState(ctx)
	if err != nil {
		return promptPageData{}, err
	}
	if !state.known(key) {
		return promptPageData{}, errPromptNotFound
	}
	history, err := store.PromptHistory(ctx, key, before, promptHistoryPageSize+1)
	if err != nil {
		return promptPageData{}, err
	}
	latest := history
	if before != 0 {
		if latest, err = store.PromptHistory(ctx, key, 0, 1); err != nil {
			return promptPageData{}, err
		}
	}
	var newest promptcatalog.PromptVersion
	if len(latest) > 0 {
		newest = latest[0]
	}
	live, _ := state.liveText(key)
	data := promptPageData{
		Prompt: newPromptRow(state, key, newest), GroupID: promptGroupOf(key).ID, GroupTitle: promptGroupOf(key).Title,
		Body: live,
	}
	if !data.Prompt.Unset {
		data.LiveSHA256 = promptcatalog.BodySHA256(live)
	}
	if entry, ok := state.Entry(key); ok {
		data.Body, data.DraftSHA256 = entry.Body, entry.SHA256()
		data.Draft = &promptDraftView{
			UpdatedAt: entry.UpdatedAt, UpdatedBy: entry.UpdatedBy, Remove: entry.Remove,
			Diff: diffText(live, entry.Body), OutOfDate: state.OutOfDate(entry),
		}
	}
	data.Variables = promptVariables(data.Body)
	path := promptsPath + "/" + url.PathEscape(key)
	if len(history) > promptHistoryPageSize {
		history = history[:promptHistoryPageSize]
		data.Pages.ForwardURL = fmt.Sprintf("%s?before=%d#history", path, history[len(history)-1].ActivationID)
	}
	if before != 0 {
		data.Pages.BackURL = path + "#history"
	}
	for _, version := range history {
		view := newPromptVersionView(version)
		view.Live = !data.Prompt.Unset && version.ActivationID == newest.ActivationID && version.SHA256 == data.LiveSHA256
		data.History = append(data.History, view)
	}
	data.Pages.Summary = "Newest first"
	return data, nil
}

func (s *Server) loadPromptChange(ctx context.Context, key string, release int64) (*promptChangeView, error) {
	change, err := s.promptStore().PromptChange(ctx, key, release)
	if err != nil {
		return nil, err
	}
	view := &promptChangeView{promptVersionView: newPromptVersionView(change.PromptVersion), Body: change.Body, First: change.First}
	if !change.First {
		view.Diff = diffText(change.PreviousBody, change.Body)
	}
	return view, nil
}

func (s *Server) renderPrompt(w http.ResponseWriter, r *http.Request, data promptPageData, status int) {
	token, err := issueCSRFToken(w, r)
	if err != nil {
		s.logger.Error("admin prompt CSRF token failed", "err", err)
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.CSRFToken = token
	data.pageData = s.promptsPage(r, data.Prompt.Name)
	data.Setup = s.promptSetup(r.Context())
	s.renderStatus(w, promptPage, data, status)
}

// promptChange is the draft change a prompt's form posts: its text, and the
// draft and live text its page showed.
func promptChange(r *http.Request, key, body string) promptcatalog.DraftChange {
	return promptcatalog.DraftChange{
		Key: key, Body: body,
		DraftSHA256: r.Form.Get("draft_sha256"), LiveSHA256: r.Form.Get("live_sha256"),
	}
}

// handlePromptSave puts the editor's text in the draft.
func (s *Server) handlePromptSave(w http.ResponseWriter, r *http.Request) {
	if !requireFormMutationLimit(w, r, s.publicOrigin, csrfCookieName, maxPromptFormBytes) {
		return
	}
	key := r.PathValue("key")
	// Browsers submit textarea line breaks as CRLF.
	body := strings.TrimSpace(strings.ReplaceAll(r.Form.Get("body"), "\r\n", "\n"))
	if body == "" && !promptcatalog.IsNote(key) {
		s.refusePrompt(w, r, key, body, "Not saved. The text is empty.")
		return
	}
	err := adminaction.SaveDraftPrompt(r.Context(), s.audit(r), s.promptStore(), promptChange(r, key, body))
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, promptcatalog.ErrDraftChanged):
		s.refusePrompt(w, r, key, body, "")
	case err != nil:
		s.logger.Warn("admin prompt draft save rejected", "key", key, "err", err)
		s.refusePrompt(w, r, key, body, "Not saved. "+promptProblem(key, err))
	default:
		redirectWithResult(w, r, promptsPath+"/"+url.PathEscape(key), "Saved to the draft. Nothing changes until the draft is made live.", false)
	}
}

// refusePrompt answers a save that was not applied with the editor and the
// text the administrator wrote: under problem, or, when problem is empty,
// because the draft or live text changed while the page was open.
func (s *Server) refusePrompt(w http.ResponseWriter, r *http.Request, key, body, problem string) {
	data, err := s.loadPrompt(r.Context(), key, 0)
	if errors.Is(err, errPromptNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.logger.Error("admin prompt query failed", "key", key, "err", err)
		http.Error(w, "prompt query failed", http.StatusInternalServerError)
		return
	}
	data.Body = body
	data.Variables = promptVariables(body)
	if problem == "" {
		data.Conflict = true
		s.renderPrompt(w, r, data, http.StatusConflict)
		return
	}
	data.Error = problem
	// The text was refused, not overtaken: the form keeps what the page
	// showed, so fixing the text and saving again applies.
	data.DraftSHA256, data.LiveSHA256 = r.Form.Get("draft_sha256"), r.Form.Get("live_sha256")
	s.renderPrompt(w, r, data, http.StatusUnprocessableEntity)
}

// handlePromptDiscard drops one prompt's change from the draft.
func (s *Server) handlePromptDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	key := r.PathValue("key")
	state, err := s.loadPromptState(r.Context())
	if err != nil {
		s.logger.Error("admin prompt query failed", "key", key, "err", err)
		http.Error(w, "prompt query failed", http.StatusInternalServerError)
		return
	}
	live, _ := state.liveText(key)
	err = adminaction.DiscardDraftPrompt(r.Context(), s.audit(r), s.promptStore(), promptChange(r, key, live))
	s.finishDraftWrite(w, r, key, err, "Discarded. The draft no longer changes this prompt.")
}

// handlePromptRestore puts one prompt's text from an earlier version in the
// draft.
func (s *Server) handlePromptRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	key := r.PathValue("key")
	restore, ok := formReleaseID(w, r, "restore_release_id")
	if !ok {
		return
	}
	release, err := s.promptStore().Get(r.Context(), restore)
	if err != nil {
		http.Error(w, "version not found", http.StatusBadRequest)
		return
	}
	body, err := release.Bundle.Text(key)
	if err != nil {
		http.Error(w, "prompt not in that version", http.StatusBadRequest)
		return
	}
	err = adminaction.RestorePromptToDraft(r.Context(), s.audit(r), s.promptStore(), restore, promptChange(r, key, body))
	s.finishDraftWrite(w, r, key, err, fmt.Sprintf("Version %d’s text is in the draft. Nothing changes until the draft is made live.", restore))
}

// finishDraftWrite answers a discard or restore on a prompt's page.
func (s *Server) finishDraftWrite(w http.ResponseWriter, r *http.Request, key string, err error, done string) {
	if s.auditFailed(w, err) {
		return
	}
	path := promptsPath + "/" + url.PathEscape(key)
	switch {
	case errors.Is(err, promptcatalog.ErrDraftChanged):
		redirectWithResult(w, r, path, "The draft or the live text changed while this page was open, so nothing changed. Check the text below and try again.", true)
	case err != nil:
		s.logger.Warn("admin prompt draft write rejected", "key", key, "err", err)
		redirectWithResult(w, r, path, "Nothing changed. "+promptProblem(key, err), true)
	default:
		redirectWithResult(w, r, path, done, false)
	}
}

// promptProblem says why the catalog refused a prompt's text.
func promptProblem(key string, err error) string {
	message := strings.TrimPrefix(err.Error(), "prompt catalog: ")
	if rest, ok := strings.CutPrefix(message, key+" "); ok && key != "" {
		return "The text " + rest + "."
	}
	if message == "" {
		return "The catalog refused it."
	}
	return strings.ToUpper(message[:1]) + message[1:] + "."
}

func formReleaseID(w http.ResponseWriter, r *http.Request, field string) (int64, bool) {
	id, err := strconv.ParseInt(r.Form.Get(field), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid "+field, http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// promptActivationView is one time a version went live.
type promptActivationView struct {
	ReleaseID int64
	At        time.Time
	By        string
	Note      string
	// Again marks a version that was live before.
	Again bool
	// First marks the first version, which installed every prompt; Example
	// one that is the example Diderot ships.
	First   bool
	Example bool
	// Changes names the prompts whose text it changed.
	Changes string
	Live    bool
}

func newPromptActivationView(activation promptcatalog.Activation) promptActivationView {
	return promptActivationView{
		ReleaseID: activation.ReleaseID, At: activation.ActivatedAt, By: activation.ActivatedBy,
		Note: personNote(activation.CreatedBy, activation.Note), Again: activation.Again,
		First: activation.FromReleaseID == 0, Example: activation.CreatedBy == promptcatalog.SeedActor,
		Changes: promptNameList(activation.Changed, 3),
	}
}

// promptNameList names up to limit prompts, then says how many more.
func promptNameList(keys []string, limit int) string {
	names := make([]string, 0, min(len(keys), limit))
	for _, key := range keys[:min(len(keys), limit)] {
		names = append(names, promptName(key))
	}
	text := strings.Join(names, ", ")
	if more := len(keys) - len(names); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

type promptVersionsData struct {
	pageData
	Versions []promptActivationView
	Pages    pagination
}

func (s *Server) handlePromptVersions(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	before = max(before, 0)
	activations, err := s.promptStore().Activations(r.Context(), before, promptVersionPageSize+1)
	if err != nil {
		s.logger.Error("admin prompt versions query failed", "err", err)
		http.Error(w, "prompt versions query failed", http.StatusInternalServerError)
		return
	}
	data := promptVersionsData{pageData: s.promptsPage(r, "Version history"), Pages: pagination{Summary: "Newest first"}}
	if len(activations) > promptVersionPageSize {
		activations = activations[:promptVersionPageSize]
		data.Pages.ForwardURL = fmt.Sprintf("%s/versions?before=%d", promptsPath, activations[len(activations)-1].ID)
	}
	if before != 0 {
		data.Pages.BackURL = promptsPath + "/versions"
	}
	for index, activation := range activations {
		view := newPromptActivationView(activation)
		view.Live = before == 0 && index == 0
		data.Versions = append(data.Versions, view)
	}
	s.render(w, promptVersionsPage, data)
}

// promptChangeItem is one prompt a version changed, and how.
type promptChangeItem struct {
	Key  string
	Name string
	// New marks a prompt the compared version did not have, Removed one
	// this version does not have.
	New     bool
	Removed bool
	Diff    promptDiff
	Open    bool
}

type promptVersionData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	ReleaseID int64
	Live      bool
	// LiveReleaseID is the live version, which making this one live
	// replaces.
	LiveReleaseID int64
	CreatedAt     time.Time
	CreatedBy     string
	Note          string
	Example       bool
	// Source is where the version came from, when it names something an
	// operator chose, such as a commit.
	Source       string
	WentLive     []promptActivationView
	ComparedWith int64
	// ComparedLive marks a comparison with the live version, for a version
	// that never went live.
	ComparedLive bool
	// Installed is every prompt of a version that replaced none.
	Installed []promptRow
	Changes   []promptChangeItem
	// WouldChange names the prompts that making this version live changes,
	// WouldChangeCount of them.
	WouldChange      string
	WouldChangeCount int
	// DraftOutdated counts the draft's changes that making this version live
	// leaves out of date: those to prompts whose text it changes.
	DraftOutdated int
}

func (s *Server) handlePromptVersion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderNotFound(w, r, missingVersion)
		return
	}
	data, err := s.loadPromptVersion(r.Context(), id)
	if errors.Is(err, promptcatalog.ErrNotFound) {
		s.renderNotFound(w, r, missingVersion)
		return
	}
	if err != nil {
		s.logger.Error("admin prompt version query failed", "release_id", id, "err", err)
		http.Error(w, "prompt version query failed", http.StatusInternalServerError)
		return
	}
	if !data.Live {
		if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
			http.Error(w, "CSRF token failed", http.StatusInternalServerError)
			return
		}
	}
	data.pageData = s.promptsPage(r, fmt.Sprintf("Version %d", id))
	data.Notice, data.Error = cleanNotice(r.URL.Query().Get("notice")), cleanNotice(r.URL.Query().Get("error"))
	s.render(w, promptVersionPage, data)
}

func (s *Server) loadPromptVersion(ctx context.Context, id int64) (promptVersionData, error) {
	store := s.promptStore()
	release, err := store.Get(ctx, id)
	if err != nil {
		return promptVersionData{}, err
	}
	draft, err := store.Draft(ctx)
	if err != nil {
		return promptVersionData{}, err
	}
	live := &draft.Live
	activations, err := store.ReleaseActivations(ctx, id)
	if err != nil {
		return promptVersionData{}, err
	}
	data := promptVersionData{
		ReleaseID: id, Live: release.Active, LiveReleaseID: live.ID,
		CreatedAt: release.CreatedAt, CreatedBy: release.CreatedBy,
		Note: personNote(release.CreatedBy, release.Note), Example: release.Example,
		Source: promptSource(release.SourceRevision),
	}
	for _, activation := range activations {
		data.WentLive = append(data.WentLive, newPromptActivationView(activation))
	}
	if !release.Active {
		keys := changedPrompts(live.Bundle, release.Bundle)
		data.WouldChange, data.WouldChangeCount = promptNameList(keys, 4), len(keys)
		for _, entry := range draft.Entries {
			if slices.Contains(keys, entry.Key) && !draft.OutOfDate(entry) {
				data.DraftOutdated++
			}
		}
	}
	compared := live
	switch {
	case len(activations) > 0 && activations[0].FromReleaseID == 0:
		for _, prompt := range release.Bundle.Prompts {
			data.Installed = append(data.Installed, promptRow{Key: prompt.Key, Name: promptName(prompt.Key)})
		}
		return data, nil
	case len(activations) > 0:
		if compared, err = store.Get(ctx, activations[0].FromReleaseID); err != nil {
			return promptVersionData{}, err
		}
	default:
		data.ComparedLive = true
	}
	data.ComparedWith = compared.ID
	keys := changedPrompts(compared.Bundle, release.Bundle)
	for _, key := range keys {
		after, afterErr := release.Bundle.Text(key)
		before, beforeErr := compared.Bundle.Text(key)
		data.Changes = append(data.Changes, promptChangeItem{
			Key: key, Name: promptName(key), New: beforeErr != nil, Removed: afterErr != nil,
			Diff: diffText(before, after), Open: len(keys) <= promptDiffsOpen,
		})
	}
	return data, nil
}

// changedPrompts lists, in key order, the prompts whose text differs
// between two versions, counting one that only one of them has.
func changedPrompts(from, to promptcatalog.Bundle) []string {
	before := map[string]string{}
	for _, prompt := range from.Prompts {
		before[prompt.Key] = prompt.SHA256
	}
	var keys []string
	for _, prompt := range to.Prompts {
		if before[prompt.Key] != prompt.SHA256 {
			keys = append(keys, prompt.Key)
		}
		delete(before, prompt.Key)
	}
	for key := range before {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// promptSource is a version's source revision when an operator chose it:
// not one Diderot made up for a save in this UI, a push, or the example.
func promptSource(revision string) string {
	for _, generated := range []string{"ui:", "push:", "embedded:", "example:"} {
		if strings.HasPrefix(revision, generated) {
			return ""
		}
	}
	return revision
}

func (s *Server) handlePromptVersionActivate(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	expected, ok := formReleaseID(w, r, "expected_release_id")
	if !ok {
		return
	}
	result, err := adminaction.ActivatePromptRelease(r.Context(), s.audit(r), s.promptStore(), id, expected)
	if s.auditFailed(w, err) {
		return
	}
	path := fmt.Sprintf("%s/versions/%d", promptsPath, id)
	switch {
	case errors.Is(err, promptcatalog.ErrActiveReleaseChanged):
		redirectWithResult(w, r, path, "The live version changed while this page was open, so nothing changed. Check what is live now and try again.", true)
	case err != nil:
		s.logger.Warn("admin prompt activation failed", "release_id", id, "err", err)
		redirectWithResult(w, r, path, fmt.Sprintf("Version %d could not be made live: %s", id, promptProblem("", err)), true)
	case !result.Changed:
		redirectWithResult(w, r, path, fmt.Sprintf("Nothing changed: version %d has the same text as the live version.", id), false)
	default:
		redirectWithResult(w, r, path, fmt.Sprintf("Version %d is live, and new conversations use it now.", id), false)
	}
}

// askToolView is one tool Ask can call, as the model reads it.
type askToolView struct {
	Name        string
	Description string
	Params      []askToolParam
	// Connector is the title of the connector that gives Ask the tool,
	// empty for one Ask always has; ConnectorHref is its page, and
	// ConnectorState "on" or "off", or empty when unknown.
	Connector      string
	ConnectorHref  string
	ConnectorState string
}

type askToolParam struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

type promptToolsData struct {
	pageData
	Tools []askToolView
}

func (s *Server) handlePromptTools(w http.ResponseWriter, r *http.Request) {
	tools, err := s.loadAskTools(r.Context())
	if err != nil {
		s.logger.Error("admin ask tools query failed", "err", err)
		http.Error(w, "ask tools query failed", http.StatusInternalServerError)
		return
	}
	s.render(w, promptToolsPage, promptToolsData{pageData: s.promptsPage(r, "Tools Ask can use"), Tools: tools})
}

func (s *Server) loadAskTools(ctx context.Context) ([]askToolView, error) {
	var wording ask.Wording
	if s.toolWording != nil {
		wording = s.toolWording()
	}
	infos, err := ask.BuiltinToolInfos(ctx, wording, s.registries.Sources.Infos())
	if err != nil {
		return nil, err
	}
	connectors, err := s.toolConnectors(ctx)
	if err != nil {
		return nil, err
	}
	var tools []askToolView
	for _, info := range infos {
		view := askToolView{Name: info.Name, Description: info.Desc}
		if view.Params, err = askToolParams(info); err != nil {
			return nil, err
		}
		if connector, ok := connectors[info.Name]; ok {
			view.Connector, view.ConnectorHref = connector.Info().Title, "/admin/connectors/"+connector.Name()
			if s.connectors != nil {
				if state, err := s.connectors.View(ctx, connector.Name()); err == nil {
					view.ConnectorState = "off"
					if state.Enabled {
						view.ConnectorState = "on"
					}
				}
			}
		}
		tools = append(tools, view)
	}
	return tools, nil
}

// toolConnectors maps each tool a tool connector gives Ask to that
// connector.
func (s *Server) toolConnectors(ctx context.Context) (map[string]asktool.Tool, error) {
	off := func() (settings.Resolved, bool) { return settings.Resolved{}, false }
	connectors := map[string]asktool.Tool{}
	for _, connector := range s.registries.Tools.Tools() {
		for _, registration := range connector.Tools(off) {
			info, err := registration.Tool.Info(ctx)
			if err != nil {
				return nil, fmt.Errorf("ask tool of %s: %w", connector.Name(), err)
			}
			connectors[info.Name] = connector
		}
	}
	return connectors, nil
}

// askToolParams lists a tool's parameters in the order its schema gives
// them.
func askToolParams(info *schema.ToolInfo) ([]askToolParam, error) {
	input, err := info.ToJSONSchema()
	if err != nil {
		return nil, fmt.Errorf("ask tool %q schema: %w", info.Name, err)
	}
	if input == nil || input.Properties == nil {
		return nil, nil
	}
	var params []askToolParam
	for pair := input.Properties.Oldest(); pair != nil; pair = pair.Next() {
		params = append(params, askToolParam{
			Name: pair.Key, Type: schemaType(pair.Value), Description: schemaDescription(pair.Value),
			Required: slices.Contains(input.Required, pair.Key),
		})
	}
	return params, nil
}

func schemaType(property *jsonschema.Schema) string {
	if property.Type == "array" && property.Items != nil && property.Items.Type != "" {
		return "list of " + property.Items.Type
	}
	return property.Type
}

// schemaDescription is a parameter's description and, when it takes one of
// a set of values, those values.
func schemaDescription(property *jsonschema.Schema) string {
	enum := property.Enum
	if len(enum) == 0 && property.Items != nil {
		enum = property.Items.Enum
	}
	if len(enum) == 0 {
		return property.Description
	}
	values := make([]string, 0, len(enum))
	for _, value := range enum {
		values = append(values, fmt.Sprint(value))
	}
	return strings.TrimSpace(property.Description + " One of: " + strings.Join(values, ", ") + ".")
}

type legacyPromptData struct {
	pageData
	Prompt promptcatalog.LegacyAskPrompt
}

func (s *Server) handleLegacyPrompt(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderNotFound(w, r, missingLegacyPrompt)
		return
	}
	prompt, err := s.promptStore().LegacyAskPrompt(r.Context(), id)
	if errors.Is(err, promptcatalog.ErrNotFound) {
		s.renderNotFound(w, r, missingLegacyPrompt)
		return
	}
	if err != nil {
		s.logger.Error("admin legacy prompt query failed", "id", id, "err", err)
		http.Error(w, "prompt query failed", http.StatusInternalServerError)
		return
	}
	s.render(w, legacyPromptPage, legacyPromptData{pageData: s.promptsPage(r, fmt.Sprintf("Earlier Ask prompt %d", id)), Prompt: *prompt})
}
