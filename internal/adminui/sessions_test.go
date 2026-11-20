package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBrowserSessionPrivacy struct {
	owner       string
	setValue    bool
	setSessions []sessioningest.SessionRef
	setErr      error
	// sessions replaces the one indexed session ListSessions returns.
	sessions []sessioningest.OwnerSession
}

// fakeSummaries is a fixed agent-session summary state.
type fakeSummaries sessioningest.SummaryControlState

func (f fakeSummaries) State(context.Context) (sessioningest.SummaryControlState, error) {
	return sessioningest.SummaryControlState(f), nil
}

func (fakeSummaries) SetEnabled(context.Context, bool) error { return nil }

func (f *fakeBrowserSessionPrivacy) SetSessions(_ context.Context, owner string, sessions []sessioningest.SessionRef, value bool) (sessioningest.SessionPrivacyUpdate, error) {
	f.owner, f.setSessions, f.setValue = owner, sessions, value
	if f.setErr != nil {
		return sessioningest.SessionPrivacyUpdate{}, f.setErr
	}
	return sessioningest.SessionPrivacyUpdate{OwnerOnly: value, SessionUpdates: len(sessions)}, nil
}

func (f *fakeBrowserSessionPrivacy) ListSessions(_ context.Context, owner string, _ int, _ int64) (sessioningest.OwnerSessionPage, error) {
	f.owner = owner
	if f.sessions != nil {
		return sessioningest.OwnerSessionPage{Sessions: f.sessions}, nil
	}
	return sessioningest.OwnerSessionPage{Sessions: []sessioningest.OwnerSession{{
		Client: "codex", SessionID: "ses_mine", Title: "Repair session privacy", OriginApp: "codex", ChangeSeq: 9,
		RecordCount: 12, UserTurnCount: 4, Indexed: true, OwnerOnly: true, PrivacyExplicit: true,
	}}}, nil
}

func newSessionPrivacyBrowser(t *testing.T, backend SessionPrivacyService) *http.ServeMux {
	t.Helper()
	return newSessionPrivacyBrowserWith(t, backend, fakeSummaries{EffectiveEnabled: true})
}

func newSessionPrivacyBrowserWith(t *testing.T, backend SessionPrivacyService, summaries SessionSummaryControl) *http.ServeMux {
	t.Helper()
	s := &Server{
		summaryControl: summaries,
		authenticator:  testBrowserAuth(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)), version: "test",
		adminGroups: map[string]struct{}{"admins": {}},
		selfGroups:  map[string]struct{}{"everyone": {}}, publicOrigin: "https://diderot.test",
		viewerBaseURL: func() string { return "https://sessions.test" }, sessionPrivacy: backend,
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	return mux
}

func sessionBrowserRequest(method, target string, form url.Values, csrf string) *http.Request {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Header.Set(proxyEmailHeader, " Me@Example.com ")
	r.Header.Set(proxyGroupsHeader, "everyone")
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://diderot.test")
	}
	if csrf != "" {
		r.AddCookie(&http.Cookie{Name: selfCSRFCookieName, Value: csrf})
	}
	return r
}

func TestSessionPrivacyBrowserRendersOnlyVerifiedOwnerAndMutatesSelf(t *testing.T) {
	backend := &fakeBrowserSessionPrivacy{}
	mux := newSessionPrivacyBrowser(t, backend)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, sessionBrowserRequest(http.MethodGet, "https://diderot.test/sessions", nil, ""))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "ses_mine")
	assert.Contains(t, w.Body.String(), "Repair session privacy")
	assert.Contains(t, w.Body.String(), `href="https://sessions.test/sessions/ses_mine"`)
	assert.Contains(t, w.Body.String(), "Only you")
	assert.Contains(t, w.Body.String(), "your choice")
	assert.Contains(t, w.Body.String(), `<span class="badge badge-good">indexed</span>`)
	assert.NotContains(t, w.Body.String(), "other@example.com")
	assert.Equal(t, "me@example.com", backend.owner)
	var csrf string
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == selfCSRFCookieName {
			csrf = cookie.Value
			assert.Equal(t, "/sessions", cookie.Path)
		}
	}
	require.True(t, validCSRFToken(csrf))

	selection := encodeSessionSelection(sessioningest.SessionRef{Client: "codex", SessionID: "ses_mine"})
	form := url.Values{"csrf_token": {csrf}, "owner_only": {"false"}, "session": {selection}, "owner": {"other@example.com"}}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, sessionBrowserRequest(http.MethodPost, "https://diderot.test/sessions/privacy", form, csrf))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/sessions?notice=1+session+can+now+be+found+by+everyone+who+can+search+Diderot.", w.Header().Get("Location"))
	assert.Equal(t, "me@example.com", backend.owner)
	assert.False(t, backend.setValue)
	assert.Equal(t, []sessioningest.SessionRef{{Client: "codex", SessionID: "ses_mine"}}, backend.setSessions)
}

// TestSessionsPageSaysWhySessionsArentIndexed: while summaries can't run,
// the page says why in a sentence, and a session that isn't indexed reads
// "not indexed" rather than "queued", as if work were under way.
func TestSessionsPageSaysWhySessionsArentIndexed(t *testing.T) {
	waiting := []sessioningest.OwnerSession{{Client: "claude-code", SessionID: "ses_waiting", RecordCount: 3, UserTurnCount: 1}}
	notSetUp := "Sessions that aren&#39;t indexed are stored, but not searchable: this server isn&#39;t set up to summarize them. An administrator can set it up."
	for _, tc := range []struct {
		state  fakeSummaries
		status string
		says   string
	}{
		{fakeSummaries{EffectiveEnabled: true}, ">queued<", ""},
		{fakeSummaries{Blocked: sessioningest.SummaryPaused}, ">not indexed<",
			"Sessions that aren&#39;t indexed are stored, but not searchable until an administrator resumes summaries."},
		{fakeSummaries{Blocked: sessioningest.SummaryNoModel}, ">not indexed<", notSetUp},
	} {
		mux := newSessionPrivacyBrowserWith(t, &fakeBrowserSessionPrivacy{sessions: waiting}, tc.state)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, sessionBrowserRequest(http.MethodGet, "https://diderot.test/sessions", nil, ""))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), tc.status, tc.state.Blocked)
		if tc.says == "" {
			assert.NotContains(t, w.Body.String(), "not searchable")
		} else {
			assert.Contains(t, w.Body.String(), tc.says)
		}
	}

	// A session whose summary gave up is not queued: nothing is trying it.
	parked := []sessioningest.OwnerSession{{Client: "claude-code", SessionID: "ses_parked", RecordCount: 3, UserTurnCount: 1, SummaryFailed: true}}
	failed := newSessionPrivacyBrowserWith(t, &fakeBrowserSessionPrivacy{sessions: parked}, fakeSummaries{EffectiveEnabled: true})
	recorder := httptest.NewRecorder()
	failed.ServeHTTP(recorder, sessionBrowserRequest(http.MethodGet, "https://diderot.test/sessions", nil, ""))
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), ">could not be summarized<")
	assert.NotContains(t, recorder.Body.String(), ">queued<")

	// With every listed session indexed, there is nothing to explain.
	mux := newSessionPrivacyBrowserWith(t, &fakeBrowserSessionPrivacy{}, fakeSummaries{Blocked: sessioningest.SummaryPaused})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, sessionBrowserRequest(http.MethodGet, "https://diderot.test/sessions", nil, ""))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "not searchable")
}

func TestSessionPrivacyBrowserRequiresTeamGroupAndCSRF(t *testing.T) {
	mux := newSessionPrivacyBrowser(t, &fakeBrowserSessionPrivacy{})
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	r.Header.Set(proxyEmailHeader, "user@example.com")
	r.Header.Set(proxyGroupsHeader, "other")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "<h1>This account can&#39;t open My sessions</h1>", "the refusal is a page in the shell")
	assert.Contains(t, body, "signed in as <strong>user@example.com</strong>")
	assert.Contains(t, body, "Only members of <code>everyone</code> can open My sessions.")
	assert.NotContains(t, body, `<a href="/sessions"`, "the header does not offer the page it refuses")
	assert.NotContains(t, body, "Go to the admin pages", "only an administrator is pointed at the admin pages")

	r = httptest.NewRequest(http.MethodGet, "/sessions/static/admin.css", nil)
	r.Header.Set(proxyEmailHeader, "user@example.com")
	r.Header.Set(proxyGroupsHeader, "other")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code, "the page's stylesheet reaches the account it refuses")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/static/admin.css", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "and nobody who is not signed in")

	r = httptest.NewRequest(http.MethodGet, "/sessions", nil)
	r.Header.Set(proxyEmailHeader, "op@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `<a class="button" href="/admin/">Go to the admin pages</a>`)

	form := url.Values{"csrf_token": {"bad"}, "owner_only": {"true"}}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, sessionBrowserRequest(http.MethodPost, "https://diderot.test/sessions/privacy", form, "bad"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestSessionPrivacyBrowserExplainsUnusableSelections: a form sent with no
// session selected, or one that has since gone, comes back to the page it
// was sent from with what to do, not a bare error.
func TestSessionPrivacyBrowserExplainsUnusableSelections(t *testing.T) {
	csrf := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	selection := encodeSessionSelection(sessioningest.SessionRef{Client: "codex", SessionID: "ses_mine"})
	for _, tc := range []struct {
		name     string
		backend  *fakeBrowserSessionPrivacy
		sessions []string
		location string
	}{
		{"gone", &fakeBrowserSessionPrivacy{setErr: sessioningest.ErrInvalidSessionSelection}, []string{selection},
			"/sessions?before=7&error=A+selected+session+is+no+longer+listed%2C+so+nothing+changed.+Choose+again+from+the+list+below."},
		{"none", &fakeBrowserSessionPrivacy{}, nil, "/sessions?before=7&error=Select+at+least+one+session+first."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := newSessionPrivacyBrowser(t, tc.backend)
			form := url.Values{"csrf_token": {csrf}, "owner_only": {"true"}, "session": tc.sessions, "before": {"7"}}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, sessionBrowserRequest(http.MethodPost, "https://diderot.test/sessions/privacy", form, csrf))
			assert.Equal(t, http.StatusSeeOther, w.Code)
			assert.Equal(t, tc.location, w.Header().Get("Location"))
			if tc.sessions == nil {
				assert.Empty(t, tc.backend.owner, "nothing selected never reaches the store")
			}
		})
	}

	w := httptest.NewRecorder()
	mux := newSessionPrivacyBrowser(t, &fakeBrowserSessionPrivacy{})
	mux.ServeHTTP(w, sessionBrowserRequest(http.MethodGet, "https://diderot.test/sessions?error=Select+at+least+one+session+first.", nil, ""))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `<div class="notice notice-bad" role="alert"><p>Select at least one session first.</p></div>`)
}
