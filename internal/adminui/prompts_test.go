package adminui

import (
	"bytes"
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryPromptHasANameAndGroup(t *testing.T) {
	for _, prompt := range append(promptcatalog.MustEmbedded().Prompts, promptcatalog.Notes()...) {
		assert.NotEqual(t, prompt.Key, promptName(prompt.Key), "%s has no name", prompt.Key)
		assert.NotEqual(t, otherPrompts.ID, promptGroupOf(prompt.Key).ID, "%s has no group", prompt.Key)
	}
	for _, name := range promptcatalog.VariableNames() {
		assert.NotEmpty(t, promptVariableMeanings[name], "{{%s}} has no meaning", name)
	}
	// A version saved by an older build still shows this build's description.
	shipped := promptcatalog.MustEmbedded().Prompts[0]
	assert.Equal(t, shipped.Description, promptDescription(promptcatalog.Prompt{Key: shipped.Key, Description: "Stored with an old version."}))
	assert.Equal(t, "Ours.", promptDescription(promptcatalog.Prompt{Key: "ask.unknown", Description: "Ours."}))
	assert.Equal(t, "ask.unknown", promptName("ask.unknown"))
	assert.Equal(t, otherPrompts.ID, promptGroupOf("ask.unknown").ID)
}

func TestPromptTemplatesEscapeBodies(t *testing.T) {
	hostile := `</textarea><script>alert("x")</script>`
	var out bytes.Buffer
	require.NoError(t, promptToolsPage.ExecuteTemplate(&out, "layout", promptToolsData{
		pageData: pageData{Title: "Tools Ask can use"},
		Tools:    []askToolView{{Name: "hostile", Description: hostile, Params: []askToolParam{{Name: "q", Description: hostile}}}},
	}))
	assert.NotContains(t, out.String(), `<script>alert("x")</script>`)
	assert.Contains(t, out.String(), "&lt;script&gt;")

	out.Reset()
	diff := diffText("safe", hostile)
	require.NoError(t, promptPage.ExecuteTemplate(&out, "layout", promptPageData{
		pageData: pageData{Title: "Role and research"}, Prompt: promptRow{Key: "ask.base", Name: "Role and research"},
		Body: hostile, Draft: &promptDraftView{Diff: diff},
		History: []promptVersionView{{ReleaseID: 1, Note: hostile}},
		Viewing: &promptChangeView{promptVersionView: promptVersionView{ReleaseID: 1}, Body: hostile, Diff: diff},
	}))
	assert.NotContains(t, out.String(), `<script>alert("x")</script>`)
	assert.Equal(t, 1, strings.Count(out.String(), "</textarea>"), "the body cannot close the editor")

	out.Reset()
	require.NoError(t, promptDraftPage.ExecuteTemplate(&out, "layout", promptDraftData{
		pageData: pageData{Title: "Draft"},
		Changes:  []promptDraftChange{{Key: "ask.base", Name: "Role and research", Diff: diff, Rendered: hostile, Problem: hostile}},
	}))
	assert.NotContains(t, out.String(), `<script>alert("x")</script>`)
}

type promptBrowser struct {
	t    *testing.T
	mux  *http.ServeMux
	pool *pgxpool.Pool
	csrf string
}

func newPromptBrowser(t *testing.T) *promptBrowser {
	return newPromptBrowserWith(t, promptcatalog.MustEmbedded())
}

// newPromptBrowserWith is an administrator's browser on an install seeded
// with bundle.
func newPromptBrowserWith(t *testing.T, bundle promptcatalog.Bundle) *promptBrowser {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_prompts"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		TRUNCATE admin_audit_log, legacy_ask_session_prompts, legacy_ask_prompts, ask_sessions,
		         prompt_release_activations, prompt_active_release,
		         prompt_release_entries, prompt_releases, prompt_draft_entries,
		         session_ingest_summaries, session_ingest_chunk_summaries
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = promptcatalog.NewStore(pool).EnsureSeed(ctx, bundle, "embedded:test")
	require.NoError(t, err)
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	b := &promptBrowser{t: t, mux: mux, pool: pool}
	page := browserRequest(t, mux, http.MethodGet, "https://diderot.test/admin/prompts/ask.base", nil, "")
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			b.csrf = cookie.Value
		}
	}
	require.True(t, validCSRFToken(b.csrf))
	return b
}

func (b *promptBrowser) get(path string) string {
	b.t.Helper()
	res := browserRequest(b.t, b.mux, http.MethodGet, "https://diderot.test"+path, nil, b.csrf)
	require.Equal(b.t, http.StatusOK, res.Code, res.Body.String())
	return res.Body.String()
}

func (b *promptBrowser) status(path string) int {
	b.t.Helper()
	return browserRequest(b.t, b.mux, http.MethodGet, "https://diderot.test"+path, nil, b.csrf).Code
}

func (b *promptBrowser) post(path string, form url.Values) (int, string, string) {
	b.t.Helper()
	form.Set("csrf_token", b.csrf)
	res := browserRequest(b.t, b.mux, http.MethodPost, "https://diderot.test"+path, form, b.csrf)
	location, _ := url.QueryUnescape(res.Header().Get("Location"))
	return res.Code, location, res.Body.String()
}

func (b *promptBrowser) active() *promptcatalog.Release {
	b.t.Helper()
	release, err := promptcatalog.NewStore(b.pool).Current(context.Background())
	require.NoError(b.t, err)
	return release
}

func (b *promptBrowser) draft() promptcatalog.Draft {
	b.t.Helper()
	draft, err := promptcatalog.NewStore(b.pool).Draft(context.Background())
	require.NoError(b.t, err)
	return *draft
}

// formValue is the value of the hidden input name in page.
func formValue(t *testing.T, page, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `name="`+name+`" value="`)
	require.True(t, ok, "no %s in the page", name)
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

// save puts body in the draft from key's page, as the browser does.
func (b *promptBrowser) save(key, body string) (int, string, string) {
	b.t.Helper()
	page := b.get("/admin/prompts/" + key)
	return b.post("/admin/prompts/"+key, url.Values{
		"body": {body}, "draft_sha256": {formValue(b.t, page, "draft_sha256")}, "live_sha256": {formValue(b.t, page, "live_sha256")},
	})
}

// publish makes the draft live from the draft page, as the browser does.
func (b *promptBrowser) publish(note string) (int, string) {
	b.t.Helper()
	page := b.get("/admin/prompts/draft")
	code, location, _ := b.post("/admin/prompts/draft/publish", url.Values{
		"expected_release_id": {formValue(b.t, page, "expected_release_id")},
		"fingerprint":         {formValue(b.t, page, "fingerprint")}, "note": {note},
	})
	return code, location
}

func (b *promptBrowser) lastAudit() (action, target, status string) {
	b.t.Helper()
	require.NoError(b.t, b.pool.QueryRow(context.Background(), `
		SELECT action, target, status FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &target, &status))
	return action, target, status
}

func TestPromptEditorBrowserWorkflow(t *testing.T) {
	b := newPromptBrowser(t)
	seed := b.active()
	require.True(t, seed.Example)
	key := promptcatalog.AskAnswerDiscipline
	original, err := seed.Bundle.Text(key)
	require.NoError(t, err)
	seedID := strconv.FormatInt(seed.ID, 10)
	path := "/admin/prompts/" + key

	list := b.get("/admin/prompts")
	assert.Contains(t, list, `<a class="row-link" href="/admin/prompts/ask.answer_discipline">Answer rules</a> <span class="id">ask.answer_discipline</span>`)
	assert.Contains(t, list, "Ask: how Diderot answers")
	assert.Contains(t, list, "the example prompts Diderot ships")
	assert.Contains(t, list, `<span class="badge badge-neutral">shipped</span>`)
	assert.NotContains(t, list, `<span class="badge badge-neutral">example</span>`, "while all of it is the example, no row needs saying so")
	assert.NotContains(t, list, "Use the example as-is", "only setup offers keeping the example")
	assert.NotContains(t, list, "Review the draft")
	assert.NotContains(t, list, "Compare with the shipped text")

	page := b.get(path)
	assert.Contains(t, page, `name="live_sha256" value="`+promptcatalog.BodySHA256(original)+`"`)
	assert.Contains(t, page, `name="draft_sha256" value=""`)
	firstLine, _, _ := strings.Cut(original, "\n")
	assert.Contains(t, page, html.EscapeString(firstLine))
	assert.Contains(t, page, "<code>{{ORG_NAME}}</code>")
	assert.Contains(t, page, "<code>{{SOURCES}}</code>")
	assert.Contains(t, page, "The example text Diderot ships")
	assert.Contains(t, page, "Save to draft")

	// Saving puts the text in the draft and changes nothing that runs.
	code, location, _ := b.save(key, "Be brief.\r\nCite sources.\r\n")
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "Saved to the draft.")
	assert.Equal(t, seed.ID, b.active().ID)
	action, target, status := b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.save", key, "succeeded"}, []string{action, target, status})
	drafted := b.get(path)
	assert.Contains(t, drafted, "In the draft")
	assert.Contains(t, drafted, `<ins class="diff-line diff-add"><span class="diff-mark" aria-hidden="true">+</span>Be brief.</ins>`)
	assert.Contains(t, drafted, `<del class="diff-line diff-del"><span class="diff-mark" aria-hidden="true">−</span>`+html.EscapeString(firstLine)+`</del>`)
	assert.Contains(t, drafted, "Be brief.\nCite sources.</textarea>", "CRLF from the browser is normalized")
	assert.Contains(t, drafted, `name="draft_sha256" value="`+promptcatalog.BodySHA256("Be brief.\nCite sources.")+`"`)

	// A page opened before that save must not overwrite it.
	code, _, conflict := b.post(path, url.Values{"body": {"Stale text."}, "live_sha256": {promptcatalog.BodySHA256(original)}})
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, conflict, "changed while you were editing, so nothing was saved.")
	assert.Contains(t, conflict, `href="/admin/prompts/ask.answer_discipline">Open the current text</a>`)
	assert.Contains(t, conflict, "Stale text.")
	assert.NotContains(t, conflict, "Save to draft")
	_, _, status = b.lastAudit()
	assert.Equal(t, "failed", status)

	// Text Diderot refuses is kept for fixing, with the page's guards.
	draftSHA := formValue(t, drafted, "draft_sha256")
	code, _, rejected := b.post(path, url.Values{"body": {"Write for {{COMPANY}}."}, "draft_sha256": {draftSHA}, "live_sha256": {promptcatalog.BodySHA256(original)}})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, rejected, "Not saved. The text uses unknown variable {{COMPANY}}")
	assert.Contains(t, rejected, "Write for {{COMPANY}}.")
	assert.Contains(t, rejected, `name="draft_sha256" value="`+draftSHA+`"`)
	code, _, empty := b.post(path, url.Values{"body": {"  "}, "draft_sha256": {draftSHA}, "live_sha256": {promptcatalog.BodySHA256(original)}})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, empty, "Not saved. The text is empty.")

	// The list says what the draft holds.
	list = b.get("/admin/prompts")
	assert.Contains(t, list, "The draft changes one prompt</strong>: Answer rules.")
	assert.Contains(t, list, `<span class="badge badge-attention">draft</span>`)

	// The draft page shows the change and makes it live once.
	draftPage := b.get("/admin/prompts/draft")
	assert.Contains(t, draftPage, "One prompt differs from the live")
	assert.Contains(t, draftPage, `<ins class="diff-line diff-add"><span class="diff-mark" aria-hidden="true">+</span>Be brief.</ins>`)
	assert.Contains(t, draftPage, "Show it as the model receives it")
	assert.Contains(t, draftPage, `name="expected_release_id" value="`+seedID+`"`)
	assert.Contains(t, draftPage, "Make the draft live")
	assert.NotContains(t, draftPage, "agent sessions are summarized")
	code, location, _ = b.post("/admin/prompts/draft/publish", url.Values{"expected_release_id": {seedID}, "fingerprint": {"stale"}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "error=The draft changed while this page was open")
	assert.Equal(t, seed.ID, b.active().ID)

	code, location = b.publish("tighter")
	require.Equal(t, http.StatusSeeOther, code)
	saved := b.active()
	savedID := strconv.FormatInt(saved.ID, 10)
	assert.Contains(t, location, "Version "+savedID+" is live")
	assert.Equal(t, "operator@example.com", saved.CreatedBy)
	assert.Equal(t, "tighter", saved.Note)
	assert.Equal(t, "Be brief.\nCite sources.", saved.Bundle.MustText(key))
	for i, prompt := range saved.Bundle.Prompts {
		if prompt.Key != key {
			assert.Equal(t, seed.Bundle.Prompts[i], prompt)
		}
	}
	action, target, status = b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.publish", "draft", "succeeded"}, []string{action, target, status})
	assert.Empty(t, b.draft().Entries)
	assert.Contains(t, b.get("/admin/prompts/draft"), "The draft is empty")

	// The list and the prompt name who changed it last, and that it is no
	// longer the shipped text.
	list = b.get("/admin/prompts")
	assert.Contains(t, list, "operator@example.com")
	assert.Contains(t, list, "“tighter”")
	assert.Contains(t, list, `<span class="badge badge-neutral">edited</span>`)
	assert.Contains(t, list, "One prompt differs from the text this version of Diderot ships.")
	assert.Contains(t, b.get(path), `href="/admin/prompts/shipped#shipped-ask.answer_discipline">compare them</a>`)

	// Viewing a version shows what it changed; one whose text is not live
	// offers to put it in the draft.
	viewed := b.get(path + "?version=" + savedID)
	assert.Contains(t, viewed, "Version "+savedID+" <span class=\"badge badge-good\">live</span>")
	assert.Contains(t, viewed, "This is the live text.")
	// The removed count is the example prompt's length, which is not this
	// test's business.
	assert.Regexp(t, `2 lines added, \d+ removed`, viewed)
	assert.NotContains(t, viewed, "restore_release_id")
	history := b.get(path + "?version=" + seedID)
	assert.Contains(t, history, "The first version of this prompt")
	assert.Contains(t, history, `name="restore_release_id" value="`+seedID+`"`)
	assert.Contains(t, b.get(path+"?version=999"), "Version 999 did not change this prompt.")

	code, location, _ = b.post(path+"/restore", url.Values{
		"restore_release_id": {seedID}, "draft_sha256": {""}, "live_sha256": {formValue(t, history, "live_sha256")},
	})
	require.Equal(t, http.StatusSeeOther, code, location)
	assert.Contains(t, location, "Version "+seedID+"’s text is in the draft.")
	assert.Equal(t, saved.ID, b.active().ID)
	entry, ok := b.draft().Entry(key)
	require.True(t, ok)
	assert.Equal(t, original, entry.Body)
	action, target, status = b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.restore", key, "succeeded"}, []string{action, target, status})
	assert.Contains(t, b.get(path+"?version="+seedID), "This is the draft’s text.")

	// Discarding that one change.
	restored := b.get(path)
	code, location, _ = b.post(path+"/discard", url.Values{
		"draft_sha256": {formValue(t, restored, "draft_sha256")}, "live_sha256": {formValue(t, restored, "live_sha256")},
	})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "Discarded.")
	assert.Empty(t, b.draft().Entries)
	action, target, status = b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.discard", key, "succeeded"}, []string{action, target, status})

	// The version history and one version.
	versions := b.get("/admin/prompts/versions")
	assert.Contains(t, versions, `<a class="row-link" href="/admin/prompts/versions/`+savedID+`">`)
	assert.Contains(t, versions, "Answer rules")
	assert.Contains(t, versions, "The example prompts Diderot ships")
	version := b.get("/admin/prompts/versions/" + savedID)
	assert.Contains(t, version, "Compared with <a href=\"/admin/prompts/versions/"+seedID+"\">version "+seedID+"</a>")
	first := b.get("/admin/prompts/versions/" + seedID)
	assert.Contains(t, first, "This was the first version")
	assert.Contains(t, first, `href="/admin/prompts/ask.base?version=`+seedID+`#version"`)
	assert.Contains(t, first, "Make version "+seedID+" live")
	assert.Contains(t, first, "changes one prompt: Answer rules.")
	assert.Equal(t, http.StatusNotFound, b.status("/admin/prompts/versions/999"))

	// Making a whole version live again stays one click, guarded the same
	// way, and leaves a draft written against the old live text out of date.
	_, _, _ = b.save(key, "Drafted against the edit.")
	_, _, _ = b.save(promptcatalog.AskOrganization, "A note the older version leaves alone.")
	assert.Contains(t, b.get("/admin/prompts/versions/"+seedID), "One drafted change will be marked out of date",
		"only the change to a prompt the older version has other text for")
	assert.NotContains(t, b.get("/admin/prompts/versions/"+savedID), "drafted change", "the live version is not offered")
	code, location, _ = b.post("/admin/prompts/versions/"+seedID+"/activate", url.Values{"expected_release_id": {seedID}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "error=The live version changed while this page was open")
	code, location, _ = b.post("/admin/prompts/versions/"+seedID+"/activate", url.Values{"expected_release_id": {savedID}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "Version "+seedID+" is live")
	assert.Equal(t, seed.ID, b.active().ID)
	action, target, status = b.lastAudit()
	assert.Equal(t, []string{"prompts.activate", seedID, "succeeded"}, []string{action, target, status})
	assert.Contains(t, b.get("/admin/prompts/versions"), "Made live again")
	outdated := b.get("/admin/prompts/draft")
	assert.Contains(t, outdated, `<span class="badge badge-attention">outdated</span>`)
	assert.Contains(t, outdated, "The draft cannot go live yet.")
	assert.NotContains(t, outdated, "Make the draft live")
	assert.Contains(t, b.get(path), "The live text changed after this change was written.")
	assert.Contains(t, b.get("/admin/prompts"), "One change was written against live text that has changed since.")

	// Saving it again from the page rebases it.
	code, _, _ = b.save(key, "Drafted against the edit.")
	require.Equal(t, http.StatusSeeOther, code)
	assert.NotContains(t, b.get("/admin/prompts/draft"), `badge-attention">outdated`)

	// Discarding the whole draft needs the tick and the draft it was shown.
	draftPage = b.get("/admin/prompts/draft")
	fingerprint := formValue(t, draftPage, "fingerprint")
	code, location, _ = b.post("/admin/prompts/draft/discard", url.Values{"fingerprint": {fingerprint}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "tick the box")
	code, location, _ = b.post("/admin/prompts/draft/discard", url.Values{"fingerprint": {"stale"}, "confirm": {"discard"}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "The draft changed while this page was open, so nothing was discarded.")
	code, location, _ = b.post("/admin/prompts/draft/discard", url.Values{"fingerprint": {fingerprint}, "confirm": {"discard"}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "Discarded the draft.")
	assert.Empty(t, b.draft().Entries)
	action, target, status = b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.discard_all", "draft", "succeeded"}, []string{action, target, status})

	assert.Equal(t, http.StatusNotFound, b.status("/admin/prompts/ask.nope"))
}

// TestPromptDraftGoesLiveAsOneVersion: changes to several prompts, an
// organization note among them, go live together as one version.
func TestPromptDraftGoesLiveAsOneVersion(t *testing.T) {
	b := newPromptBrowser(t)
	seed := b.active()

	note := b.get("/admin/prompts/ask.organization")
	assert.Contains(t, note, "About your organization")
	assert.Contains(t, note, "Not written yet: nothing is added in its place.")
	assert.Contains(t, note, "Save it empty to have no note.")
	list := b.get("/admin/prompts")
	assert.Contains(t, list, `href="/admin/prompts/ask.organization">About your organization</a>`)

	for key, body := range map[string]string{
		promptcatalog.AskOrganization:     "Runbooks live in the ops repository.",
		promptcatalog.AskSurfaceHTTP:      "Plain Markdown.",
		promptcatalog.AskAnswerDiscipline: "Be brief.",
	} {
		code, _, page := b.save(key, body)
		require.Equal(t, http.StatusSeeOther, code, page)
	}
	draftPage := b.get("/admin/prompts/draft")
	assert.Contains(t, draftPage, "3 prompts differ from the live")
	assert.Contains(t, draftPage, "About your organization</strong> <span class=\"muted\">new, ")
	assert.Equal(t, seed.ID, b.active().ID)

	code, _ := b.publish("")
	require.Equal(t, http.StatusSeeOther, code)
	live := b.active()
	assert.Equal(t, "Runbooks live in the ops repository.", live.Bundle.NoteText(promptcatalog.AskOrganization))
	assert.Equal(t, "Plain Markdown.", live.Bundle.MustText(promptcatalog.AskSurfaceHTTP))
	assert.Equal(t, "Be brief.", live.Bundle.MustText(promptcatalog.AskAnswerDiscipline))
	activations, err := promptcatalog.NewStore(b.pool).Activations(context.Background(), 0, 10)
	require.NoError(t, err)
	assert.Len(t, activations, 2, "the seed, then one version for the whole draft")

	// Saving the note empty removes it.
	code, _, page := b.save(promptcatalog.AskOrganization, "")
	require.Equal(t, http.StatusSeeOther, code, page)
	assert.Contains(t, b.get("/admin/prompts/ask.organization"), "The draft removes this note, so nothing is added in its place.")
	assert.Contains(t, b.get("/admin/prompts/draft"), `<del class="diff-line diff-del"><span class="diff-mark" aria-hidden="true">−</span>Runbooks live in the ops repository.</del>`,
		"a removal shows the text it removes")
	code, _ = b.publish("")
	require.Equal(t, http.StatusSeeOther, code)
	assert.Empty(t, b.active().Bundle.NoteText(promptcatalog.AskOrganization))
}

// TestPromptDraftSaysWhatGoingLiveRedoes: a change to a summary prompt says
// that sessions are summarized again, and estimates the work from the
// sessions summarized lately.
func TestPromptDraftSaysWhatGoingLiveRedoes(t *testing.T) {
	b := newPromptBrowser(t)
	ctx := context.Background()
	for i, at := range []string{"1 day", "2 days", "30 days"} {
		session := "s" + strconv.Itoa(i)
		_, err := b.pool.Exec(ctx, `
			INSERT INTO session_ingest_summaries (owner, client, session_id, revision, narrative, summarized_at)
			VALUES ('ada@example.com', 'codex', $1, 'r1', 'Summary.', now() - $2::interval)`, session, at)
		require.NoError(t, err)
		// The session's content, which summarizing every part again reads.
		_, err = b.pool.Exec(ctx, `
			INSERT INTO session_ingest_candidates (owner, client, session_id, owner_canonical, published_content_bytes)
			VALUES ('ada@example.com', 'codex', $1, 'ada@example.com', 9000)`, session)
		require.NoError(t, err)
		// Its parts' summaries, as the summarizer stores them: no token
		// count is kept with a part, so the estimate goes by their size.
		for part := range 3 {
			_, err := b.pool.Exec(ctx, `
				INSERT INTO session_ingest_chunk_summaries (owner, client, session_id, record_start, record_end, summary)
				VALUES ('ada@example.com', 'codex', $1, $2, $2 + 10, repeat('x', 300))`, session, part*10)
			require.NoError(t, err)
		}
	}

	code, _, page := b.save(promptcatalog.SummarySessionReduce, "Combine the parts.")
	require.Equal(t, http.StatusSeeOther, code, page)
	reduce := b.get("/admin/prompts/draft")
	assert.Contains(t, reduce, "This changes how agent sessions are summarized.")
	assert.Contains(t, reduce, "each session’s summary is written again from the summaries of all its parts")
	assert.Contains(t, reduce, "In the last 7 days, 2 sessions were summarized. Summarizing them again that way takes about 2 model calls reading about 600 tokens")

	code, _, page = b.save(promptcatalog.SummarySessionMap, "Summarize the part.")
	require.Equal(t, http.StatusSeeOther, code, page)
	both := b.get("/admin/prompts/draft")
	assert.Contains(t, both, "each session is summarized again from the start, every part of it")
	assert.Contains(t, both, "about 8 model calls reading about 6.6K tokens")
	assert.NotContains(t, both, "about 0 tokens")

	code, _, page = b.save(promptcatalog.ConnectorImageCaption, "Describe the image.")
	require.Equal(t, http.StatusSeeOther, code, page)
	assert.Contains(t, b.get("/admin/prompts/draft"), "Images and scanned pages already read keep their descriptions")
}

// TestPromptShippedText: after an upgrade the shipped-text page lists each
// prompt whose text differs from what this version ships, new prompts, and
// prompts it no longer uses, and taking them puts them in the draft.
func TestPromptShippedText(t *testing.T) {
	shipped := promptcatalog.MustEmbedded()
	older, err := shipped.WithBody(promptcatalog.AskSurfaceHTTP, "Old reply format.")
	require.NoError(t, err)
	var prompts []promptcatalog.Prompt
	for _, prompt := range older.Prompts {
		if prompt.Key != promptcatalog.AskTitle {
			prompts = append(prompts, prompt)
		}
	}
	retired := promptcatalog.Prompt{Key: "zz.retired", Path: "zz/retired.md", Body: "Retired.", SHA256: promptcatalog.BodySHA256("Retired.")}
	older.Prompts = append(prompts, retired)
	older, err = older.WithBody("zz.retired", "Retired.")
	require.NoError(t, err)
	b := newPromptBrowserWith(t, older)

	list := b.get("/admin/prompts")
	assert.Contains(t, list, "3 prompts differ from the text this version of Diderot ships.")
	assert.Contains(t, list, `href="/admin/prompts/zz.retired">zz.retired</a>`)

	page := b.get("/admin/prompts/shipped")
	assert.Contains(t, page, `id="shipped-ask.surface.http"`)
	assert.Contains(t, page, `<del class="diff-line diff-del"><span class="diff-mark" aria-hidden="true">−</span>Old reply format.</del>`)
	assert.Contains(t, page, "New in this version: your prompts do not have it.")
	assert.Contains(t, page, "This version no longer uses it. Taking the shipped text removes it.")
	assert.NotContains(t, page, "ask.organization", "notes never ship")
	assert.Contains(t, page, "Take all 3")

	form := url.Values{"take": {promptcatalog.AskSurfaceHTTP}}
	for _, key := range []string{promptcatalog.AskSurfaceHTTP, promptcatalog.AskTitle, "zz.retired"} {
		form.Add("item", key)
		for _, field := range []string{"draft_sha256.", "live_sha256.", "shipped_sha256."} {
			form.Set(field+key, formValue(t, page, field+key))
		}
	}

	// A page made by another version of Diderot is refused.
	tampered := url.Values{}
	for name, values := range form {
		tampered[name] = values
	}
	tampered.Set("shipped_sha256."+promptcatalog.AskSurfaceHTTP, promptcatalog.BodySHA256("Another version's text."))
	code, location, _ := b.post("/admin/prompts/shipped", tampered)
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "The shipped text changed while this page was open")
	assert.Empty(t, b.draft().Entries)

	code, location, _ = b.post("/admin/prompts/shipped", form)
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "/admin/prompts/draft?notice=The shipped text of 1 prompt is in the draft.")
	entry, ok := b.draft().Entry(promptcatalog.AskSurfaceHTTP)
	require.True(t, ok)
	assert.Equal(t, shipped.MustText(promptcatalog.AskSurfaceHTTP), entry.Body)
	action, target, status := b.lastAudit()
	assert.Equal(t, []string{"prompts.draft.take_shipped", "draft", "succeeded"}, []string{action, target, status})

	// The same page again is stale for the prompt now in the draft.
	form.Set("scope", "all")
	code, location, _ = b.post("/admin/prompts/shipped", form)
	require.Equal(t, http.StatusSeeOther, code)
	assert.Contains(t, location, "The draft or the live prompts changed while this page was open")

	page = b.get("/admin/prompts/shipped")
	assert.Contains(t, page, "2 prompts differ from the shipped text")
	form = url.Values{"scope": {"all"}}
	for _, key := range []string{promptcatalog.AskTitle, "zz.retired"} {
		form.Add("item", key)
		for _, field := range []string{"draft_sha256.", "live_sha256.", "shipped_sha256."} {
			form.Set(field+key, formValue(t, page, field+key))
		}
	}
	code, location, _ = b.post("/admin/prompts/shipped", form)
	require.Equal(t, http.StatusSeeOther, code, location)
	assert.Contains(t, b.get("/admin/prompts/shipped"), "Every prompt has the shipped text")

	code, _ = b.publish("Take the shipped text")
	require.Equal(t, http.StatusSeeOther, code)
	assert.Equal(t, shipped.EffectiveSHA256(), b.active().Bundle.EffectiveSHA256())
}

func TestPromptHistoryPages(t *testing.T) {
	b := newPromptBrowser(t)
	key := promptcatalog.AskSurfaceHTTP
	for i := range promptHistoryPageSize + 2 {
		code, _, body := b.save(key, "Reply "+strconv.Itoa(i))
		require.Equal(t, http.StatusSeeOther, code, body)
		code, location := b.publish("")
		require.Equal(t, http.StatusSeeOther, code, location)
	}
	page := b.get("/admin/prompts/" + key)
	assert.Equal(t, promptHistoryPageSize+1, strings.Count(page, "?version="), "a page of versions, and the text section's link to the live one")
	// A browser leaves the link's #history behind; a test request would not.
	older, _, _ := strings.Cut(strings.SplitN(page, `href="/admin/prompts/ask.surface.http?before=`, 2)[1], "#")
	rest := b.get("/admin/prompts/" + key + "?before=" + older)
	assert.Contains(t, rest, ">Newer</a>")
	assert.NotContains(t, rest, ">Older</a>")
	assert.Contains(t, rest, `<span class="badge badge-neutral">example</span>`, "the oldest page ends with the example")
}

func TestPromptToolsAndLegacyPages(t *testing.T) {
	b := newPromptBrowser(t)
	tools := b.get("/admin/prompts/tools")
	assert.Contains(t, tools, "<code>retrieve</code>")
	assert.Contains(t, tools, "Always on")
	assert.Contains(t, tools, "Parameters (")

	_, err := b.pool.Exec(context.Background(), `
		INSERT INTO legacy_ask_prompts (id, body, created_by, note, created_at, recovery_source)
		VALUES (18, 'legacy Ask body', 'operator@example.com', 'pre-catalog prompt', $1, 'test restore')`,
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	legacy := b.get("/admin/prompts/legacy/18")
	assert.Contains(t, legacy, "Earlier Ask prompt 18")
	assert.Contains(t, legacy, "legacy Ask body")
	assert.Contains(t, legacy, "pre-catalog prompt")
	assert.Equal(t, http.StatusNotFound, b.status("/admin/prompts/legacy/19"))
}

// TestPromptsDuringSetup: while the example is live and the install is in
// setup, the Prompts page offers the way on and keeping the example.
func TestPromptsDuringSetup(t *testing.T) {
	h := newSetupHarness(t)
	_, err := promptcatalog.NewStore(h.pool).EnsureSeed(context.Background(), promptcatalog.MustEmbedded(), "embedded:test")
	require.NoError(t, err)
	list := h.get("/admin/prompts", false)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	body := list.Body.String()
	assert.Contains(t, body, "This step of setup is not done yet.")
	assert.Contains(t, body, `action="/admin/settings/actions/accept-example-prompts"`)
	assert.Contains(t, body, `<input type="hidden" name="version" value="1">`)
	assert.Contains(t, h.get("/admin/prompts/ask.base", false).Body.String(), "This step of setup is not done yet.")

	kept := h.post("/admin/settings/actions/accept-example-prompts", url.Values{"version": {"1"}}, false)
	require.Equal(t, http.StatusSeeOther, kept.Code, kept.Body.String())
	assert.Contains(t, kept.Header().Get("Location"), "/admin/settings?notice=")
	body = h.get("/admin/prompts", false).Body.String()
	assert.Contains(t, body, "This step of setup is done.")
	assert.NotContains(t, body, "Use the example as-is")
}
