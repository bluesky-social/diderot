package adminui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
)

type bootstrapToggle struct{ enabled atomic.Bool }

func (b *bootstrapToggle) BootstrapAdminEnabled(context.Context) (bool, error) {
	return b.enabled.Load(), nil
}

const uiOperatorToken = "ui-operator-token-0123456789abcdef"

// newLocalSignInHarness serves the UI with local sign-in only: the bootstrap
// admin and a static token, no identity provider.
func newLocalSignInHarness(t *testing.T) (*signInHarness, *bootstrapToggle) {
	t.Helper()
	toggle := &bootstrapToggle{}
	toggle.enabled.Store(true)
	digest := sha256.Sum256([]byte(uiOperatorToken))
	static, err := auth.NewStatic([]auth.StaticToken{{
		SHA256: hex.EncodeToString(digest[:]), Email: "op@example.com", Groups: []string{"admins"},
	}})
	require.NoError(t, err)
	cfg := auth.BrowserLoginConfig{
		PublicURL: loginOrigin, SessionKeys: [][]byte{bytes.Repeat([]byte{7}, 32)},
		SessionTTL: 12 * time.Hour, IdleTimeout: time.Hour,
		Bootstrap: toggle, StaticTokens: static,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	login, err := auth.NewBrowserLogin(cfg)
	require.NoError(t, err)
	h := &signInHarness{t: t, mux: http.NewServeMux(), privacy: &fakeBrowserSessionPrivacy{}, cookies: map[string]*http.Cookie{}}
	s := &Server{
		authenticator: login, login: login,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), version: "test",
		adminGroups: map[string]struct{}{"admins": {}},
		selfGroups:  map[string]struct{}{"everyone": {}}, publicOrigin: loginOrigin,
		sessionCookieSecure: true, sessionPrivacy: h.privacy, summaryControl: fakeSummaries{EffectiveEnabled: true},
	}
	require.NoError(t, s.Mount(h.mux))
	return h, toggle
}

// localSignIn loads the sign-in page for its CSRF token, then posts form.
func (h *signInHarness) localSignIn(form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	page := h.get(auth.LoginPath + "?return_to=%2Fadmin%2Fmissing")
	require.Equal(h.t, http.StatusOK, page.Code)
	csrf := h.cookies[signInCSRFCookieName]
	require.NotNil(h.t, csrf)
	form.Set("csrf_token", csrf.Value)
	form.Set("return_to", "/admin/missing")
	r := httptest.NewRequest(http.MethodPost, loginOrigin+auth.LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", loginOrigin)
	return h.do(r)
}

func TestLocalSignInPageOffersMethods(t *testing.T) {
	h, toggle := newLocalSignInHarness(t)
	w := h.get(auth.LoginPath)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `value="bootstrap"`)
	assert.Contains(t, body, `value="token"`)
	assert.NotContains(t, body, "identity provider")
	csrf := h.cookies[signInCSRFCookieName]
	require.NotNil(t, csrf)
	assert.Equal(t, auth.LoginPath, csrf.Path)
	assert.Equal(t, http.SameSiteStrictMode, csrf.SameSite)

	toggle.enabled.Store(false)
	body = h.get(auth.LoginPath).Body.String()
	assert.NotContains(t, body, `value="bootstrap"`, "the bootstrap form disappears once it is off")
	assert.Contains(t, body, `value="token"`)
}

// TestOIDCSignInPageAddsBootstrapForm: with an identity provider, the page
// shows its button beside the bootstrap form while that is on, and sends
// browsers straight to the provider otherwise.
func TestOIDCSignInPageAddsBootstrapForm(t *testing.T) {
	idp := authtest.NewIdP(t)
	idp.RegisterClient("browser", "secret")
	toggle := &bootstrapToggle{}
	toggle.enabled.Store(true)
	h := newSignInHarness(t)
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), publicOrigin: loginOrigin, sessionCookieSecure: true}
	withBootstrap, err := auth.NewBrowserLogin(auth.BrowserLoginConfig{
		Issuer: idp.Issuer, ClientID: "browser", ClientSecret: "secret", Scopes: []string{"openid"},
		SessionGroups: []string{"admins"}, PublicURL: loginOrigin,
		SessionKeys: [][]byte{bytes.Repeat([]byte{7}, 32)}, SessionTTL: time.Hour, IdleTimeout: time.Hour,
		Bootstrap: toggle,
	})
	require.NoError(t, err)
	s.authenticator, s.login = withBootstrap, withBootstrap
	mux := http.NewServeMux()
	s.mountLogin(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, loginOrigin+auth.LoginPath+"?return_to=%2Fadmin%2F", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `href="/auth/login/oidc?return_to=%2Fadmin%2F"`)
	assert.Contains(t, w.Body.String(), `value="bootstrap"`)
	assert.NotContains(t, w.Body.String(), `value="token"`, "no static tokens outside static mode")

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, loginOrigin+"/auth/login/oidc?return_to=%2Fadmin%2F", nil))
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), idp.Issuer+"/authorize?"))

	toggle.enabled.Store(false)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, loginOrigin+auth.LoginPath, nil))
	require.Equal(t, http.StatusSeeOther, w.Code, "with only OIDC left, sign-in goes straight to the provider")

	// The plain OIDC harness never offers a form either.
	assert.Equal(t, http.StatusSeeOther, h.get(auth.LoginPath).Code)
}

func TestBootstrapAdminBrowserSession(t *testing.T) {
	h, toggle := newLocalSignInHarness(t)

	w := h.localSignIn(url.Values{"method": {"bootstrap"}, "username": {"admin"}, "password": {"wrong"}})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Wrong username or password")
	assert.NotContains(t, h.cookies, loginSessionCookie)

	w = h.localSignIn(url.Values{"method": {"bootstrap"}, "username": {"admin"}, "password": {"admin"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/missing", w.Header().Get("Location"))
	require.Contains(t, h.cookies, loginSessionCookie)
	assert.Equal(t, http.StatusNotFound, h.get("/admin/missing").Code, "the bootstrap admin passes the administrator gate")
	w = h.get("/")
	assert.Equal(t, "/admin/", w.Header().Get("Location"))
	assert.Equal(t, http.StatusForbidden, h.get("/sessions").Code, "it is in no self-service group")

	// Turning it off ends the open session on its next request.
	toggle.enabled.Store(false)
	w = h.get("/admin/missing")
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), auth.LoginPath))
	assert.NotContains(t, h.cookies, loginSessionCookie)

	toggle.enabled.Store(true)
	h.localSignIn(url.Values{"method": {"bootstrap"}, "username": {"admin"}, "password": {"admin"}})
	require.Contains(t, h.cookies, loginSessionCookie)
	toggle.enabled.Store(false)
	r := httptest.NewRequest(http.MethodPost, loginOrigin+"/admin/missing", nil)
	assert.Equal(t, http.StatusUnauthorized, h.do(r).Code)
}

func TestStaticTokenBrowserSession(t *testing.T) {
	h, _ := newLocalSignInHarness(t)
	w := h.localSignIn(url.Values{"method": {"token"}, "token": {"wrong-token"}})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "not valid")

	w = h.localSignIn(url.Values{"method": {"token"}, "token": {uiOperatorToken}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	require.Contains(t, h.cookies, loginSessionCookie)
	assert.Equal(t, http.StatusNotFound, h.get("/admin/missing").Code)
}

func TestLocalSignInChecksOriginAndCSRF(t *testing.T) {
	h, _ := newLocalSignInHarness(t)
	h.get(auth.LoginPath)
	csrf := h.cookies[signInCSRFCookieName].Value
	post := func(origin, token string) int {
		form := url.Values{"csrf_token": {token}, "method": {"bootstrap"}, "username": {"admin"}, "password": {"admin"}}
		r := httptest.NewRequest(http.MethodPost, loginOrigin+auth.LoginPath, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		return h.do(r).Code
	}
	assert.Equal(t, http.StatusForbidden, post("https://evil.example", csrf), "another site cannot sign a browser in")
	assert.Equal(t, http.StatusForbidden, post(loginOrigin, ""))
	assert.NotContains(t, h.cookies, loginSessionCookie)
	assert.Equal(t, http.StatusSeeOther, post(loginOrigin, csrf))
}

func TestUnauthorizedWithoutSignInNamesSessionKeys(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "DIDEROT_BROWSER_SESSION_KEYS")
}
