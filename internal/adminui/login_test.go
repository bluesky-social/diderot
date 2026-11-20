package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
	"github.com/bluesky-social/diderot/internal/sessioningest"
)

const (
	loginOrigin        = "https://diderot.test"
	loginSessionCookie = "__Host-diderot_session"
)

type signInHarness struct {
	t       *testing.T
	idp     *authtest.IdP
	mux     *http.ServeMux
	privacy *fakeBrowserSessionPrivacy
	cookies map[string]*http.Cookie
}

func newBrowserLoginForTest(t *testing.T, idp *authtest.IdP) *auth.BrowserLogin {
	t.Helper()
	login, err := auth.NewBrowserLogin(auth.BrowserLoginConfig{
		Issuer: idp.Issuer, ClientID: "browser", ClientSecret: "secret",
		Scopes: []string{"openid", "email", "profile"}, PublicURL: loginOrigin,
		RequireEmailVerified: true,
		SessionGroups:        []string{"admins", "everyone"},
		SessionKeys:          [][]byte{bytes.Repeat([]byte{7}, 32)},
		SessionTTL:           12 * time.Hour, IdleTimeout: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return login
}

func newSignInHarness(t *testing.T) *signInHarness {
	t.Helper()
	idp := authtest.NewIdP(t)
	idp.RegisterClient("browser", "secret")
	login := newBrowserLoginForTest(t, idp)
	h := &signInHarness{t: t, idp: idp, mux: http.NewServeMux(), privacy: &fakeBrowserSessionPrivacy{}, cookies: map[string]*http.Cookie{}}
	s := &Server{
		authenticator: login, login: login,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), version: "test",
		adminGroups: map[string]struct{}{"admins": {}},
		selfGroups:  map[string]struct{}{"everyone": {}}, publicOrigin: loginOrigin,
		sessionCookieSecure: true, sessionPrivacy: h.privacy, summaryControl: fakeSummaries{EffectiveEnabled: true},
	}
	require.NoError(t, s.Mount(h.mux))
	return h
}

func (h *signInHarness) user(email string, verified bool, groups ...string) {
	h.idp.SetUser(map[string]any{"sub": "sub-" + email, "email": email, "email_verified": verified, "groups": groups})
}

// do serves one browser request with the jar's cookies and stores the
// response's cookies.
func (h *signInHarness) do(r *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	for _, c := range h.cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(h.cookies, c.Name)
		} else {
			h.cookies[c.Name] = c
		}
	}
	return w
}

func (h *signInHarness) get(target string) *httptest.ResponseRecorder {
	return h.do(httptest.NewRequest(http.MethodGet, loginOrigin+target, nil))
}

// signIn starts at target and follows redirects through the IdP back to
// Diderot, returning the callback's response.
func (h *signInHarness) signIn(target string) *httptest.ResponseRecorder {
	h.t.Helper()
	w := h.get(target)
	if strings.HasPrefix(target, auth.LoginPath) {
		require.Equal(h.t, http.StatusSeeOther, w.Code)
	} else {
		require.Equal(h.t, http.StatusSeeOther, w.Code, "an unauthenticated page load is sent to sign in")
		require.True(h.t, strings.HasPrefix(w.Header().Get("Location"), auth.LoginPath+"?return_to="))
		w = h.get(w.Header().Get("Location"))
		require.Equal(h.t, http.StatusSeeOther, w.Code)
	}
	authURL := w.Header().Get("Location")
	require.True(h.t, strings.HasPrefix(authURL, h.idp.Issuer+"/authorize?"), authURL)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	require.NoError(h.t, err)
	_ = resp.Body.Close()
	require.Equal(h.t, http.StatusFound, resp.StatusCode)
	callback := resp.Header.Get("Location")
	require.True(h.t, strings.HasPrefix(callback, loginOrigin+auth.CallbackPath+"?"), callback)
	return h.do(httptest.NewRequest(http.MethodGet, callback, nil))
}

func TestBrowserSignInRoundTrip(t *testing.T) {
	h := newSignInHarness(t)
	h.user("Op@Example.com", true, "admins", "everyone")

	w := h.get("/admin/missing?x=1")
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/auth/login?return_to=%2Fadmin%2Fmissing%3Fx%3D1", w.Header().Get("Location"))

	w = h.get(w.Header().Get("Location"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))

	w = h.signIn("/admin/missing?x=1")
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/missing?x=1", w.Header().Get("Location"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"), "the code-bearing URL is never sent as a referrer")
	require.Contains(t, h.cookies, loginSessionCookie)

	assert.Equal(t, http.StatusNotFound, h.get("/admin/missing").Code, "the administrator passes the gate")
	w = h.get("/")
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/", w.Header().Get("Location"))
	w = h.get("/sessions")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "op@example.com", h.privacy.owner)
	require.Contains(t, h.cookies, signOutCSRFCookieName)
	assert.Contains(t, w.Body.String(), `<form method="post" action="/auth/logout"><input type="hidden" name="csrf_token" value="`+
		h.cookies[signOutCSRFCookieName].Value+`"><button class="button-link" type="submit">Sign out</button></form>`,
		"the header signs out with a POST that carries the token")
}

func TestBrowserSignInKeepsGroupGates(t *testing.T) {
	for _, test := range []struct {
		name                  string
		groups                []string
		root, admin, sessions int
		rootLocation          string
	}{
		{"member", []string{"everyone", "engineering"}, http.StatusSeeOther, http.StatusForbidden, http.StatusOK, "/sessions"},
		{"operator only", []string{"admins"}, http.StatusSeeOther, http.StatusNotFound, http.StatusForbidden, "/admin/"},
		{"no allowlisted group", []string{"engineering"}, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSignInHarness(t)
			h.user("user@example.com", true, test.groups...)
			require.Equal(t, http.StatusSeeOther, h.signIn(auth.LoginPath).Code)
			w := h.get("/")
			assert.Equal(t, test.root, w.Code)
			assert.Equal(t, test.rootLocation, w.Header().Get("Location"))
			assert.Equal(t, test.admin, h.get("/admin/missing").Code)
			assert.Equal(t, test.sessions, h.get("/sessions").Code)
		})
	}
}

func TestBrowserSignInRequiresVerifiedEmail(t *testing.T) {
	h := newSignInHarness(t)
	h.user("member@example.com", false, "everyone", "admins")
	w := h.signIn("/sessions")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "verified email")
	assert.NotContains(t, h.cookies, loginSessionCookie)
	assert.Empty(t, w.Header().Get("Location"), "a failed callback never redirects back into sign-in")
}

func TestBrowserSignInRefusesOpenRedirects(t *testing.T) {
	for _, target := range []string{
		"https://evil.example/", "//evil.example/", "/\\evil.example", "/\t/evil.example",
		"javascript:alert(1)", "evil.example", "/auth/logout",
	} {
		t.Run(target, func(t *testing.T) {
			h := newSignInHarness(t)
			h.user("op@example.com", true, "admins")
			w := h.signIn(auth.LoginPath + "?return_to=" + url.QueryEscape(target))
			require.Equal(t, http.StatusSeeOther, w.Code)
			assert.Equal(t, "/", w.Header().Get("Location"))
		})
	}
}

func TestBrowserSignInCallbackFailures(t *testing.T) {
	h := newSignInHarness(t)
	h.user("op@example.com", true, "admins")

	w := h.get(auth.CallbackPath + "?code=x&state=y")
	assert.Equal(t, http.StatusBadRequest, w.Code, "no sign-in in progress")
	assert.Empty(t, w.Header().Get("Location"))

	h.idp.RewriteIDToken(func(c map[string]any) { c["nonce"] = "replayed" })
	w = h.signIn("/admin/")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
	assert.NotContains(t, h.cookies, loginSessionCookie)
	h.idp.RewriteIDToken(nil)

	h.idp.SetUser(nil)
	assert.Equal(t, http.StatusForbidden, h.signIn("/admin/").Code)
}

func TestBrowserSignOut(t *testing.T) {
	h := newSignInHarness(t)
	h.user("op@example.com", true, "admins", "everyone")
	h.signIn("/admin/missing")
	require.Contains(t, h.cookies, loginSessionCookie)

	w := h.get(auth.LogoutPath)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `action="/auth/logout"`)
	assert.Contains(t, w.Body.String(), "op@example.com")
	csrf := h.cookies[signOutCSRFCookieName]
	require.NotNil(t, csrf)
	assert.Equal(t, "/", csrf.Path, "every page's header posts the sign-out form")
	assert.True(t, csrf.Secure)
	assert.Equal(t, http.SameSiteStrictMode, csrf.SameSite)
	assert.Contains(t, w.Body.String(), `name="csrf_token" value="`+csrf.Value+`"`)

	post := func(origin, token string) *httptest.ResponseRecorder {
		form := url.Values{"csrf_token": {token}}
		r := httptest.NewRequest(http.MethodPost, loginOrigin+auth.LogoutPath, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return h.do(r)
	}
	assert.Equal(t, http.StatusForbidden, post("https://evil.example", csrf.Value).Code, "another site cannot sign a browser out")
	// A request whose browser sent no origin metadata still needs the token.
	assert.Equal(t, http.StatusForbidden, post("", "").Code)
	assert.Equal(t, http.StatusForbidden, post(loginOrigin, "").Code)
	require.Contains(t, h.cookies, loginSessionCookie)

	w = post("", csrf.Value)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, h.cookies, loginSessionCookie)
	w = h.get("/admin/missing")
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), auth.LoginPath))
}

// TestBrowserSignInKeepsMutationChecks: the Lax session cookie rides along on
// top-level navigations, so form handlers still need their own origin and
// double-submit checks, and an unauthenticated form post is refused rather
// than replayed through sign-in.
func TestBrowserSignInKeepsMutationChecks(t *testing.T) {
	h := newSignInHarness(t)
	h.user("member@example.com", true, "everyone")
	h.signIn("/sessions")
	require.Equal(t, http.StatusOK, h.get("/sessions").Code)
	csrf := h.cookies[selfCSRFCookieName].Value
	require.True(t, validCSRFToken(csrf))
	selection := encodeSessionSelection(sessioningest.SessionRef{Client: "codex", SessionID: "ses_mine"})

	post := func(origin string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, loginOrigin+"/sessions/privacy", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		return h.do(r)
	}
	good := url.Values{"csrf_token": {csrf}, "owner_only": {"true"}, "session": {selection}}
	w := post("https://evil.example", good)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "same-origin")
	w = post(loginOrigin, url.Values{"owner_only": {"true"}, "session": {selection}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "CSRF")
	assert.Nil(t, h.privacy.setSessions)

	w = post(loginOrigin, good)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "member@example.com", h.privacy.owner)

	delete(h.cookies, loginSessionCookie)
	w = post(loginOrigin, good)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
}

func TestBrowserSignInRejectsForgedSession(t *testing.T) {
	h := newSignInHarness(t)
	h.cookies[loginSessionCookie] = &http.Cookie{Name: loginSessionCookie, Value: "forged"}
	w := h.get("/admin/")
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), auth.LoginPath))
	assert.NotContains(t, h.cookies, loginSessionCookie, "the forged cookie is deleted")

	// Trusted-proxy headers are not a second way in once sign-in is active.
	r := httptest.NewRequest(http.MethodGet, loginOrigin+"/admin/missing", nil)
	r.Header.Set(proxyEmailHeader, "op@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
	assert.Equal(t, http.StatusSeeOther, h.do(r).Code)
}

func TestSignInRoutesAbsentWithoutBrowserLogin(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
		selfGroups:    map[string]struct{}{"everyone": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	for _, path := range []string{auth.LoginPath, auth.CallbackPath, auth.LogoutPath} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
}

func TestAdminLayoutSignOutOnlyWithBrowserLogin(t *testing.T) {
	render := func(signOut, token string) string {
		var out strings.Builder
		require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", overviewData{pageData: pageData{
			Title: "Test", Principal: auth.Principal{Email: "op@example.com"}, SignOutURL: signOut, SignOutToken: token,
		}}))
		return out.String()
	}
	assert.Contains(t, render(auth.LogoutPath, "tok"),
		`<form method="post" action="/auth/logout"><input type="hidden" name="csrf_token" value="tok"><button class="button-link" type="submit">Sign out</button></form>`)
	without := render("", "")
	assert.NotContains(t, without, "Sign out")
	assert.Contains(t, without, `<span class="account-name" title="op@example.com">op@example.com</span>`)
}

func TestNewDetectsBrowserLogin(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://diderot@127.0.0.1:1/unused")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	idp := authtest.NewIdP(t)
	login := newBrowserLoginForTest(t, idp)
	s, err := New(Config{
		Pool: pool, Authenticator: login, AdminGroups: []string{"admins"}, PublicURL: loginOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, BrowserLogin(login), s.login)
	assert.Equal(t, auth.LogoutPath, s.page(httptest.NewRequest(http.MethodGet, "/admin/", nil), "t", "").SignOutURL)

	s, err = New(Config{
		Pool: pool, Authenticator: testBrowserAuth(), AdminGroups: []string{"admins"}, PublicURL: loginOrigin,
	})
	require.NoError(t, err)
	assert.Nil(t, s.login)
	assert.Empty(t, s.page(httptest.NewRequest(http.MethodGet, "/admin/", nil), "t", "").SignOutURL)
}

func TestSignInPageStylesheetNeedsNoSession(t *testing.T) {
	h := newSignInHarness(t)
	w := h.get(signInStylesheetPath)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/css")
	assert.Contains(t, w.Body.String(), ".signin")
	// The other assets stay behind sign-in.
	assert.NotEqual(t, http.StatusOK, h.get("/admin/static/admin.js").Code)
}
