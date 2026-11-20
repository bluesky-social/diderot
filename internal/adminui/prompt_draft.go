package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/setup"
)

// promptState is what the Prompts pages read: the live release, the draft,
// and the prompts this version of Diderot ships.
type promptState struct {
	promptcatalog.Draft
	Shipped promptcatalog.Bundle
}

func (s *Server) loadPromptState(ctx context.Context) (promptState, error) {
	draft, err := s.promptStore().Draft(ctx)
	if err != nil {
		return promptState{}, err
	}
	shipped, err := promptcatalog.Embedded()
	if err != nil {
		return promptState{}, err
	}
	return promptState{Draft: *draft, Shipped: shipped}, nil
}

func (st promptState) liveText(key string) (string, bool) {
	text, err := st.Live.Bundle.Text(key)
	return text, err == nil
}

func (st promptState) shippedText(key string) (string, bool) {
	text, err := st.Shipped.Text(key)
	return text, err == nil
}

// draftText is key's text with the draft applied: false when the draft
// removes it or neither the draft nor live has it.
func (st promptState) draftText(key string) (string, bool) {
	if entry, ok := st.Entry(key); ok {
		return entry.Body, !entry.Remove
	}
	return st.liveText(key)
}

// known reports whether key has a page: a prompt live, in the draft, shipped,
// or a note.
func (st promptState) known(key string) bool {
	_, live := st.liveText(key)
	_, drafted := st.Entry(key)
	_, shipped := st.shippedText(key)
	return live || drafted || shipped || promptcatalog.IsNote(key)
}

func (st promptState) summary() *promptDraftSummary {
	if len(st.Entries) == 0 {
		return nil
	}
	summary := &promptDraftSummary{Count: len(st.Entries)}
	keys := make([]string, 0, len(st.Entries))
	for _, entry := range st.Entries {
		keys = append(keys, entry.Key)
		if st.OutOfDate(entry) {
			summary.OutOfDate++
		}
		if entry.UpdatedAt.After(summary.UpdatedAt) {
			summary.UpdatedAt, summary.UpdatedBy = entry.UpdatedAt, entry.UpdatedBy
		}
	}
	summary.Names = promptNameList(keys, 3)
	return summary
}

// Shipped-text states: a prompt whose text differs from the shipped one, one
// this version ships that the prompts do not have, and one it no longer uses.
const (
	shippedNew    = "new"
	shippedUnused = "unused"
)

// shippedChange is one prompt whose text, with the draft applied, is not the
// text this version of Diderot ships.
type shippedChange struct {
	Key   string
	State string
	// Text is the shipped text, empty for a prompt this version no longer
	// uses; Current the text with the draft applied.
	Text    string
	Current string
}

// shippedChanges lists, in key order, the prompts whose text with the draft
// applied differs from the shipped text. Notes never ship, so never differ.
func (st promptState) shippedChanges() []shippedChange {
	var changes []shippedChange
	for _, prompt := range st.Shipped.Prompts {
		current, ok := st.draftText(prompt.Key)
		switch {
		case !ok:
			changes = append(changes, shippedChange{Key: prompt.Key, State: shippedNew, Text: prompt.Body})
		case current != prompt.Body:
			changes = append(changes, shippedChange{Key: prompt.Key, State: shippedDiffers, Text: prompt.Body, Current: current})
		}
	}
	keys := map[string]bool{}
	for _, prompt := range st.Live.Bundle.Prompts {
		keys[prompt.Key] = true
	}
	for _, entry := range st.Entries {
		keys[entry.Key] = true
	}
	for key := range keys {
		_, shipped := st.shippedText(key)
		current, ok := st.draftText(key)
		if !shipped && ok && !promptcatalog.IsNote(key) {
			changes = append(changes, shippedChange{Key: key, State: shippedUnused, Current: current})
		}
	}
	slices.SortFunc(changes, func(a, b shippedChange) int { return strings.Compare(a.Key, b.Key) })
	return changes
}

// promptDraftChange is one change of the draft as its page shows it.
type promptDraftChange struct {
	Key  string
	Name string
	// New marks a prompt live does not have, Removed one the draft removes.
	New     bool
	Removed bool
	Diff    promptDiff
	// Rendered is the text as the model receives it, with the variables
	// filled in as they are now.
	Rendered  string
	UpdatedAt time.Time
	UpdatedBy string
	// OutOfDate marks a change written against live text that has changed
	// since; Problem says why the text cannot go live.
	OutOfDate bool
	Problem   string
	Open      bool
}

type promptDraftData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	// LiveReleaseID is the live version, and Fingerprint identifies the
	// draft this page shows: making it live applies only while both hold.
	LiveReleaseID int64
	Fingerprint   string
	Changes       []promptDraftChange
	// OutOfDate and Problems count the changes that keep the draft from
	// going live; Problem is a problem of the whole set.
	OutOfDate int
	Problems  int
	Problem   string
	// Summaries is what going live redoes in agent-session summaries, nil
	// when the draft leaves their prompts alone.
	Summaries *summaryRedo
	// Files reports a change to the prompts that read documents and images.
	Files bool
}

// CanPublish reports whether the draft can go live as it is.
func (d promptDraftData) CanPublish() bool {
	return len(d.Changes) > 0 && d.OutOfDate == 0 && d.Problems == 0 && d.Problem == ""
}

// summaryRedo estimates the work going live starts in agent-session
// summaries, from the sessions summarized in the last summaryRedoWindow.
type summaryRedo struct {
	// MapChanged reports that every part of a session is summarized again,
	// not only the combined summary.
	MapChanged bool
	Sessions   int64
	Calls      int64
	// Tokens is what the calls read.
	Tokens int64
	Days   int
	// Cost is what the calls cost at the session summary models' published
	// prices, in words; empty when a price is not known.
	Cost string
}

const summaryRedoWindow = 7 * 24 * time.Hour

func (s *Server) handlePromptDraft(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := s.loadPromptDraft(ctx)
	if err != nil {
		s.logger.Error("admin prompt draft query failed", "err", err)
		http.Error(w, "prompt draft query failed", http.StatusInternalServerError)
		return
	}
	if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.promptsPage(r, "Draft")
	data.Notice, data.Error = cleanNotice(r.URL.Query().Get("notice")), cleanNotice(r.URL.Query().Get("error"))
	s.render(w, promptDraftPage, data)
}

func (s *Server) loadPromptDraft(ctx context.Context) (promptDraftData, error) {
	state, err := s.loadPromptState(ctx)
	if err != nil {
		return promptDraftData{}, err
	}
	bundle, err := state.Bundle()
	if err != nil {
		return promptDraftData{}, err
	}
	data := promptDraftData{LiveReleaseID: state.Live.ID, Fingerprint: bundle.SHA256}
	values := promptcatalog.Values()
	summaryKeys := map[string]bool{}
	for _, entry := range state.Entries {
		live, inLive := state.liveText(entry.Key)
		change := promptDraftChange{
			Key: entry.Key, Name: promptName(entry.Key), New: !inLive, Removed: entry.Remove,
			Diff: diffText(live, entry.Body), UpdatedAt: entry.UpdatedAt, UpdatedBy: entry.UpdatedBy,
			OutOfDate: state.OutOfDate(entry), Open: len(state.Entries) <= promptDiffsOpen,
		}
		if !entry.Remove {
			change.Rendered = promptcatalog.Render(entry.Body, values)
			if err := promptcatalog.ValidateText(entry.Key, entry.Body); err != nil {
				change.Problem = promptProblem(entry.Key, err)
			}
		}
		if change.OutOfDate {
			data.OutOfDate++
		}
		if change.Problem != "" {
			data.Problems++
		}
		switch entry.Key {
		case promptcatalog.SummarySessionMap, promptcatalog.SummarySessionReduce:
			summaryKeys[entry.Key] = true
		case promptcatalog.ConnectorDocumentExtract, promptcatalog.ConnectorImageCaption:
			data.Files = true
		}
		data.Changes = append(data.Changes, change)
	}
	if len(data.Changes) > 0 && data.Problems == 0 {
		if err := promptcatalog.ValidateWire(bundle); err != nil {
			data.Problem = promptProblem("", err)
		}
	}
	if len(summaryKeys) > 0 {
		if data.Summaries, err = s.estimateSummaryRedo(ctx, summaryKeys[promptcatalog.SummarySessionMap]); err != nil {
			return promptDraftData{}, err
		}
	}
	return data, nil
}

// estimateSummaryRedo counts the sessions summarized in the last
// summaryRedoWindow, and the model calls and input tokens summarizing them
// from the start takes: with mapChanged, every part again and then the
// combined summary from every part's summary; otherwise only the combined
// summary. The tokens are estimated from the size of what is stored, the
// way the summary repair on the Operations page does: the combined summary
// reads every part's summary, and a part's summary reads the session's
// content. No token count is recorded per part, so the counts a summary row
// carries, which are only its latest run's, are not used. It is rough: a
// prompt's own length and the sessions that change next are not known.
func (s *Server) estimateSummaryRedo(ctx context.Context, mapChanged bool) (*summaryRedo, error) {
	redo := &summaryRedo{MapChanged: mapChanged, Days: int(summaryRedoWindow / (24 * time.Hour))}
	var parts, partBytes, contentBytes, summaryBytes int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(c.parts), 0), COALESCE(sum(c.bytes), 0),
		       COALESCE(sum(cand.published_content_bytes), 0),
		       COALESCE(sum(length(s.narrative) + length(COALESCE(s.embedding_text, '')) + length(COALESCE(s.context_hint, ''))), 0)
		  FROM session_ingest_summaries s
		  LEFT JOIN session_ingest_candidates cand
		         ON cand.owner = s.owner AND cand.client = s.client AND cand.session_id = s.session_id
		  LEFT JOIN LATERAL (
		        SELECT count(*) AS parts, sum(length(summary) + length(array_to_string(evidence, ' '))) AS bytes
		          FROM session_ingest_chunk_summaries c
		         WHERE c.owner = s.owner AND c.client = s.client AND c.session_id = s.session_id) c ON true
		 WHERE s.summarized_at >= now() - make_interval(days => $1)`, redo.Days).
		Scan(&redo.Sessions, &parts, &partBytes, &contentBytes, &summaryBytes)
	if err != nil {
		return nil, fmt.Errorf("summary redo estimate: %w", err)
	}
	// The combined summary reads the parts' summaries and writes the
	// session's; a part's summary reads the session's content and writes
	// the part's.
	reduceIn, reduceOut := partBytes/summaryRepairBytesPerToken, summaryBytes/summaryRepairBytesPerToken
	mapIn, mapOut := contentBytes/summaryRepairBytesPerToken, partBytes/summaryRepairBytesPerToken
	redo.Calls, redo.Tokens = redo.Sessions, reduceIn
	if mapChanged {
		redo.Calls += parts
		redo.Tokens += mapIn
	}
	redo.Cost = s.summaryRedoCost(ctx, mapChanged, mapIn, mapOut, reduceIn, reduceOut)
	return redo, nil
}

// summaryRedoCost prices the tokens at what is published for the models the
// two session summary jobs use, or is empty when a price is not known.
func (s *Server) summaryRedoCost(ctx context.Context, mapChanged bool, mapIn, mapOut, reduceIn, reduceOut int64) string {
	if s.setup == nil {
		return ""
	}
	models, err := s.setup.Models(ctx)
	if err != nil {
		return ""
	}
	return summaryCost(models, mapChanged, mapIn, mapOut, reduceIn, reduceOut)
}

// summaryCost is what the tokens cost at the prices published for the
// models the two session summary jobs use; empty when one is not known.
func summaryCost(models setup.ModelsView, mapChanged bool, mapIn, mapOut, reduceIn, reduceOut int64) string {
	price := func(key string, in, out int64) (float64, bool) {
		role := models.Role(key)
		option, ok := role.Option(role.Model)
		if !ok || option.InputPrice == nil || option.OutputPrice == nil {
			return 0, false
		}
		return (float64(in)**option.InputPrice + float64(out)**option.OutputPrice) / 1e6, true
	}
	total, ok := price(string(modelregistry.RoleSessionSummaryReduce), reduceIn, reduceOut)
	if !ok {
		return ""
	}
	if mapChanged {
		parts, ok := price(string(modelregistry.RoleSessionSummaryMap), mapIn, mapOut)
		if !ok {
			return ""
		}
		total += parts
	}
	return dollars(total)
}

func (s *Server) handlePromptDraftPublish(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	expected, ok := formReleaseID(w, r, "expected_release_id")
	if !ok {
		return
	}
	result, err := adminaction.PublishPromptDraft(r.Context(), s.audit(r), s.promptStore(), expected,
		r.Form.Get("fingerprint"), r.Form.Get("note"))
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, promptcatalog.ErrActiveReleaseChanged):
		redirectWithResult(w, r, promptDraftPath, "The live version changed while this page was open, so nothing went live. Check the draft again.", true)
	case errors.Is(err, promptcatalog.ErrDraftChanged):
		redirectWithResult(w, r, promptDraftPath, "The draft changed while this page was open, so nothing went live. Check it again.", true)
	case errors.Is(err, promptcatalog.ErrDraftOutOfDate):
		redirectWithResult(w, r, promptDraftPath, "Nothing went live: the live text of a prompt in the draft changed after its change was written. Open it, check the change against the live text, and save it again.", true)
	case err != nil:
		s.logger.Warn("admin prompt draft publish failed", "err", err)
		redirectWithResult(w, r, promptDraftPath, "Nothing went live. "+promptProblem("", err), true)
	case !result.Changed:
		redirectWithResult(w, r, promptsPath, "Nothing changed: the draft’s text is already live. The draft is empty again.", false)
	default:
		redirectWithResult(w, r, promptsPath, fmt.Sprintf("Version %d is live, and new conversations use it now. Version %d is in the version history.", result.Release.ID, expected), false)
	}
}

func (s *Server) handlePromptDraftDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "discard" {
		redirectWithResult(w, r, promptDraftPath, "Nothing changed: tick the box to confirm discarding every change in the draft.", true)
		return
	}
	err := adminaction.DiscardPromptDraft(r.Context(), s.audit(r), s.promptStore(), r.Form.Get("fingerprint"))
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, promptcatalog.ErrDraftChanged):
		redirectWithResult(w, r, promptDraftPath, "The draft changed while this page was open, so nothing was discarded. Check it again.", true)
	case err != nil:
		s.logger.Warn("admin prompt draft discard failed", "err", err)
		redirectWithResult(w, r, promptDraftPath, "Nothing was discarded. "+promptProblem("", err), true)
	default:
		redirectWithResult(w, r, promptsPath, "Discarded the draft. The live prompts are unchanged.", false)
	}
}

// promptShippedItem is one prompt on the shipped-text page.
type promptShippedItem struct {
	Key   string
	Name  string
	State string
	// InDraft marks a prompt the draft changes; Diff is the change from its
	// text with the draft applied to the shipped text.
	InDraft bool
	Diff    promptDiff
	Open    bool
	// DraftSHA256, LiveSHA256 and ShippedSHA256 identify the texts the page
	// shows; taking the shipped text applies only while all three hold.
	DraftSHA256, LiveSHA256, ShippedSHA256 string
}

type promptShippedData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	Version   string
	Items     []promptShippedItem
	// Same counts the prompts whose text is already the shipped text.
	Same int
}

func (s *Server) handlePromptShipped(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadPromptState(r.Context())
	if err != nil {
		s.logger.Error("admin prompt shipped query failed", "err", err)
		http.Error(w, "prompt query failed", http.StatusInternalServerError)
		return
	}
	data := promptShippedData{Version: s.version}
	changes := state.shippedChanges()
	data.Same = len(state.Shipped.Prompts)
	for _, change := range changes {
		item := promptShippedItem{
			Key: change.Key, Name: promptName(change.Key), State: change.State,
			Diff: diffText(change.Current, change.Text), Open: len(changes) <= promptDiffsOpen,
		}
		if entry, ok := state.Entry(change.Key); ok {
			item.InDraft, item.DraftSHA256 = true, entry.SHA256()
		}
		if live, ok := state.liveText(change.Key); ok {
			item.LiveSHA256 = promptcatalog.BodySHA256(live)
		}
		if change.State != shippedUnused {
			item.ShippedSHA256 = promptcatalog.BodySHA256(change.Text)
			data.Same--
		}
		data.Items = append(data.Items, item)
	}
	if len(data.Items) > 0 {
		if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
			http.Error(w, "CSRF token failed", http.StatusInternalServerError)
			return
		}
	}
	data.pageData = s.promptsPage(r, "Shipped text")
	data.Notice, data.Error = cleanNotice(r.URL.Query().Get("notice")), cleanNotice(r.URL.Query().Get("error"))
	s.render(w, promptShippedPage, data)
}

// handlePromptShippedTake puts the shipped text of the prompts ticked, or of
// every prompt the page listed, in the draft.
func (s *Server) handlePromptShippedTake(w http.ResponseWriter, r *http.Request) {
	if !requireFormMutationLimit(w, r, s.publicOrigin, csrfCookieName, maxPromptFormBytes) {
		return
	}
	keys := r.Form["take"]
	if r.Form.Get("scope") == "all" {
		keys = r.Form["item"]
	}
	if len(keys) == 0 {
		redirectWithResult(w, r, promptShippedPath, "Nothing changed: tick the prompts whose shipped text you want.", true)
		return
	}
	shipped, err := promptcatalog.Embedded()
	if err != nil {
		http.Error(w, "shipped prompts failed to load", http.StatusInternalServerError)
		return
	}
	changes := make([]promptcatalog.DraftChange, 0, len(keys))
	for _, key := range keys {
		if promptcatalog.IsNote(key) {
			http.Error(w, "organization notes have no shipped text", http.StatusBadRequest)
			return
		}
		text, _ := shipped.Text(key)
		shown := r.Form.Get("shipped_sha256." + key)
		if (text == "" && shown != "") || (text != "" && shown != promptcatalog.BodySHA256(text)) {
			redirectWithResult(w, r, promptShippedPath, "The shipped text changed while this page was open, because Diderot was upgraded. Nothing changed; check the page again.", true)
			return
		}
		changes = append(changes, promptcatalog.DraftChange{
			Key: key, Body: text,
			DraftSHA256: r.Form.Get("draft_sha256." + key), LiveSHA256: r.Form.Get("live_sha256." + key),
		})
	}
	err = adminaction.TakeShippedPrompts(r.Context(), s.audit(r), s.promptStore(), changes)
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, promptcatalog.ErrDraftChanged):
		redirectWithResult(w, r, promptShippedPath, "The draft or the live prompts changed while this page was open, so nothing changed. Check the page again.", true)
	case err != nil:
		s.logger.Warn("admin prompt shipped take failed", "err", err)
		redirectWithResult(w, r, promptShippedPath, "Nothing changed. "+promptProblem("", err), true)
	default:
		redirectWithResult(w, r, promptDraftPath, "The shipped text of "+plural(len(changes), "prompt")+" is in the draft. Review it here, then make it live.", false)
	}
}

// promptDescriptionOf says what key is for, from the build when it ships
// the prompt or it is a note, and else from bundle.
func promptDescriptionOf(bundle promptcatalog.Bundle, key string) string {
	for _, note := range promptcatalog.Notes() {
		if note.Key == key {
			return note.Description
		}
	}
	for _, prompt := range bundle.Prompts {
		if prompt.Key == key {
			return promptDescription(prompt)
		}
	}
	return promptDescription(promptcatalog.Prompt{Key: key})
}
