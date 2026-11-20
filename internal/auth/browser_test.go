package auth_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
)

const (
	browserClient  = "diderot-browser"
	browserSecret  = "browser-secret"
	browserOrigin  = "https://diderot.example.com"
	sessionCookie  = "__Host-diderot_session"
	signInCookie   = "__Host-diderot_signin"
	adminGroup     = "admins"
	selfGroup      = "everyone"
	unrelatedGroup = "engineering"
)

var (
	currentKey = bytes.Repeat([]byte{1}, 32)
	retiredKey = bytes.Repeat([]byte{2}, 32)
)

func browserLoginConfig(idp *authtest.IdP) auth.BrowserLoginConfig {
	return auth.BrowserLoginConfig{
		Issuer: idp.Issuer, ClientID: browserClient, ClientSecret: browserSecret,
		Scopes: []string{"openid", "email", "profile"}, PublicURL: browserOrigin,
		RequireEmailVerified: true,
		SessionGroups:        []string{adminGroup, selfGroup},
		SessionKeys:          [][]byte{currentKey},
		SessionTTL:           12 * time.Hour, IdleTimeout: time.Hour,
	}
}

func newBrowserLogin(t *testing.T, cfg auth.BrowserLoginConfig) *auth.BrowserLogin {
	t.Helper()
	login, err := auth.NewBrowserLogin(cfg)
	require.NoError(t, err)
	return login
}

func newBrowserIdP(t *testing.T) *authtest.IdP {
	t.Helper()
	idp := authtest.NewIdP(t)
	idp.RegisterClient(browserClient, browserSecret)
	idp.SetUser(map[string]any{
		"sub": "user-1", "email": "Op@example.com", "email_verified": true,
		"preferred_username": "op", "groups": []string{unrelatedGroup, adminGroup, selfGroup},
	})
	return idp
}

// jar is a minimal cookie store keyed by name: enough for one origin, and it
// keeps Secure cookies on the plain-HTTP test requests.
type jar map[string]*http.Cookie

func (j jar) store(w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(j, c.Name)
		} else {
			j[c.Name] = c
		}
	}
}

func (j jar) request(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range j {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return r
}

func setCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// authorize follows the authorization URL to the test IdP, which signs the
// configured user in, and returns the callback URL it redirects to.
func authorize(t *testing.T, authURL string) *url.URL {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	callback, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	return callback
}

// startSignIn begins a sign-in and returns the browser's jar and the IdP's
// redirect back to the callback.
func startSignIn(t *testing.T, login *auth.BrowserLogin, returnTo string) (jar, *url.URL) {
	t.Helper()
	cookies := jar{}
	w := httptest.NewRecorder()
	authURL, err := login.StartLogin(w, returnTo)
	require.NoError(t, err)
	cookies.store(w)
	return cookies, authorize(t, authURL)
}

func finish(login *auth.BrowserLogin, cookies jar, callback *url.URL) (*httptest.ResponseRecorder, string, *auth.Principal, error) {
	w := httptest.NewRecorder()
	returnTo, p, err := login.FinishLogin(w, cookies.request(http.MethodGet, callback.String()))
	cookies.store(w)
	return w, returnTo, p, err
}

// signIn runs the whole flow and returns the signed-in browser's jar.
func signIn(t *testing.T, login *auth.BrowserLogin) jar {
	t.Helper()
	cookies, callback := startSignIn(t, login, "/admin/")
	_, _, _, err := finish(login, cookies, callback)
	require.NoError(t, err)
	require.Contains(t, cookies, sessionCookie)
	return cookies
}

func TestBrowserLoginRoundTrip(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))

	w := httptest.NewRecorder()
	authURL, err := login.StartLogin(w, "/admin/ask/sessions?before=42")
	require.NoError(t, err)
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, idp.Issuer+"/authorize", u.Scheme+"://"+u.Host+u.Path)
	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, browserClient, q.Get("client_id"))
	assert.Equal(t, browserOrigin+auth.CallbackPath, q.Get("redirect_uri"))
	assert.Equal(t, "openid email profile", q.Get("scope"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))
	assert.NotEmpty(t, q.Get("state"))
	assert.NotEmpty(t, q.Get("nonce"))
	assert.NotEqual(t, q.Get("state"), q.Get("nonce"))
	assert.NotContains(t, authURL, "admin", "the return path stays in the sealed cookie")

	started := setCookie(w, signInCookie)
	require.NotNil(t, started)
	assert.True(t, started.HttpOnly)
	assert.True(t, started.Secure)
	assert.Equal(t, http.SameSiteLaxMode, started.SameSite)
	assert.Equal(t, "/", started.Path)
	assert.Equal(t, 600, started.MaxAge)
	assert.NotContains(t, started.Value, q.Get("state"))

	cookies := jar{}
	cookies.store(w)
	callback := authorize(t, authURL)
	assert.Equal(t, browserOrigin+auth.CallbackPath, callback.Scheme+"://"+callback.Host+callback.Path)
	w, returnTo, p, err := finish(login, cookies, callback)
	require.NoError(t, err)
	assert.Equal(t, "/admin/ask/sessions?before=42", returnTo)
	assert.Equal(t, &auth.Principal{
		Subject: "user-1", Email: "Op@example.com", EmailVerified: true, Display: "op",
		Groups: []string{adminGroup, selfGroup},
	}, p, "only allowlist groups are kept")

	session := setCookie(w, sessionCookie)
	require.NotNil(t, session)
	assert.True(t, session.HttpOnly)
	assert.True(t, session.Secure)
	assert.Equal(t, http.SameSiteLaxMode, session.SameSite)
	assert.Equal(t, "/", session.Path)
	assert.Empty(t, session.Domain)
	assert.Equal(t, 3600, session.MaxAge, "the idle deadline comes first")
	assert.NotContains(t, cookies, signInCookie, "the sign-in cookie is consumed")

	got, err := login.Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	assert.Equal(t, p, got)

	// The callback cannot be replayed: its sign-in cookie is gone.
	_, _, _, err = finish(login, cookies, callback)
	require.ErrorIs(t, err, auth.ErrLoginState)
}

func TestBrowserLoginRequiresMatchingState(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))

	t.Run("mismatched state", func(t *testing.T) {
		cookies, callback := startSignIn(t, login, "/")
		q := callback.Query()
		q.Set("state", "attacker-state")
		callback.RawQuery = q.Encode()
		w, _, _, err := finish(login, cookies, callback)
		require.ErrorIs(t, err, auth.ErrLoginState)
		assert.Nil(t, setCookie(w, sessionCookie))
	})
	t.Run("no sign-in cookie", func(t *testing.T) {
		_, callback := startSignIn(t, login, "/")
		_, _, _, err := finish(login, jar{}, callback)
		require.ErrorIs(t, err, auth.ErrLoginState)
	})
	t.Run("another browser's sign-in", func(t *testing.T) {
		// Login CSRF: the attacker's callback in the victim's browser.
		_, attackerCallback := startSignIn(t, login, "/")
		victim, _ := startSignIn(t, login, "/")
		_, _, _, err := finish(login, victim, attackerCallback)
		require.ErrorIs(t, err, auth.ErrLoginState)
	})
	t.Run("expired sign-in", func(t *testing.T) {
		cookies, callback := startSignIn(t, login, "/")
		auth.SetBrowserLoginClock(login, func() time.Time { return time.Now().Add(11 * time.Minute) })
		defer auth.SetBrowserLoginClock(login, time.Now)
		_, _, _, err := finish(login, cookies, callback)
		require.ErrorIs(t, err, auth.ErrLoginState)
	})
	t.Run("forged sign-in cookie", func(t *testing.T) {
		cookies, callback := startSignIn(t, login, "/")
		cookies[signInCookie].Value = tamper(cookies[signInCookie].Value)
		_, _, _, err := finish(login, cookies, callback)
		require.ErrorIs(t, err, auth.ErrLoginState)
	})
	t.Run("no code", func(t *testing.T) {
		cookies, callback := startSignIn(t, login, "/")
		q := callback.Query()
		q.Del("code")
		callback.RawQuery = q.Encode()
		_, _, _, err := finish(login, cookies, callback)
		require.ErrorIs(t, err, auth.ErrLoginState)
	})
}

// TestBrowserLoginRequiresPKCEVerifier: a code redeemed with another
// sign-in's verifier (an injected code) is refused by the IdP.
func TestBrowserLoginRequiresPKCEVerifier(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	cookies, callback := startSignIn(t, login, "/")
	_, injected := startSignIn(t, login, "/")
	q := callback.Query()
	q.Set("code", injected.Query().Get("code"))
	callback.RawQuery = q.Encode()
	w, _, _, err := finish(login, cookies, callback)
	require.ErrorIs(t, err, auth.ErrLoginProvider)
	assert.Contains(t, err.Error(), "invalid_grant")
	assert.Nil(t, setCookie(w, sessionCookie))
}

func TestBrowserLoginVerifiesIDToken(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	for name, rewrite := range map[string]func(map[string]any){
		"nonce mismatch":        func(c map[string]any) { c["nonce"] = "replayed" },
		"nonce missing":         func(c map[string]any) { delete(c, "nonce") },
		"other audience":        func(c map[string]any) { c["aud"] = "another-client" },
		"other issuer":          func(c map[string]any) { c["iss"] = "https://idp.example.net" },
		"expired":               func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"other authorized part": func(c map[string]any) { c["aud"] = []string{browserClient, "x"}; c["azp"] = "x" },
		"no subject":            func(c map[string]any) { delete(c, "sub") },
		"malformed groups":      func(c map[string]any) { c["groups"] = "admins" },
	} {
		t.Run(name, func(t *testing.T) {
			idp.RewriteIDToken(rewrite)
			defer idp.RewriteIDToken(nil)
			cookies, callback := startSignIn(t, login, "/")
			w, _, _, err := finish(login, cookies, callback)
			require.ErrorIs(t, err, auth.ErrInvalidCredentials)
			assert.Nil(t, setCookie(w, sessionCookie))
		})
	}
}

func TestBrowserLoginWithoutVerifiedEmailIssuesNoSession(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	for name, user := range map[string]map[string]any{
		"unverified": {"sub": "u", "email": "u@example.com", "email_verified": false, "groups": []string{selfGroup}},
		"no claim":   {"sub": "u", "email": "u@example.com", "groups": []string{selfGroup}},
		"no email":   {"sub": "u", "email_verified": true, "groups": []string{adminGroup}},
	} {
		t.Run(name, func(t *testing.T) {
			idp.SetUser(user)
			cookies, callback := startSignIn(t, login, "/")
			w, _, _, err := finish(login, cookies, callback)
			require.ErrorIs(t, err, auth.ErrLoginNoEmail)
			assert.Nil(t, setCookie(w, sessionCookie))
		})
	}

	// An operator who vouches for the IdP's addresses opts out, as for bearers.
	cfg := browserLoginConfig(idp)
	cfg.RequireEmailVerified = false
	optedOut := newBrowserLogin(t, cfg)
	idp.SetUser(map[string]any{"sub": "u", "email": "u@example.com", "email_verified": false, "groups": []string{selfGroup}})
	cookies, callback := startSignIn(t, optedOut, "/")
	_, _, p, err := finish(optedOut, cookies, callback)
	require.NoError(t, err)
	assert.Equal(t, "u@example.com", p.Email)
	assert.False(t, p.EmailVerified)
}

func TestBrowserLoginAllowedEmailDomains(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.AllowedEmailDomains, cfg.HostedDomainClaim = []string{"example.com"}, "hd"
	login := newBrowserLogin(t, cfg)
	for name, user := range map[string]map[string]any{
		"other domain":    {"sub": "u", "email": "u@gmail.com", "email_verified": true, "hd": "example.com", "groups": []string{adminGroup}},
		"no hosted claim": {"sub": "u", "email": "u@example.com", "email_verified": true, "groups": []string{adminGroup}},
		"unverified":      {"sub": "u", "email": "u@example.com", "email_verified": false, "hd": "example.com", "groups": []string{adminGroup}},
	} {
		t.Run(name, func(t *testing.T) {
			idp.SetUser(user)
			cookies, callback := startSignIn(t, login, "/")
			w, _, _, err := finish(login, cookies, callback)
			require.ErrorIs(t, err, auth.ErrIdentityNotAllowed)
			assert.Nil(t, setCookie(w, sessionCookie))
		})
	}

	idp.SetUser(map[string]any{"sub": "u", "email": "u@example.com", "email_verified": true, "hd": "example.com", "groups": []string{adminGroup}})
	cookies, callback := startSignIn(t, login, "/")
	_, _, p, err := finish(login, cookies, callback)
	require.NoError(t, err)
	assert.Equal(t, "u@example.com", p.Email)
	assert.Contains(t, cookies, sessionCookie)

	cfg.AllowedEmailDomains = nil
	_, err = auth.NewBrowserLogin(cfg)
	require.ErrorContains(t, err, "allowed email domains")
}

func TestBrowserLoginHonorsClaimPaths(t *testing.T) {
	idp := newBrowserIdP(t)
	idp.SetUser(map[string]any{
		"sub": "kc-1", "email_verified": true,
		"https://example.com/email": "kc@example.com",
		"realm_access":              map[string]any{"roles": []string{selfGroup}},
	})
	cfg := browserLoginConfig(idp)
	cfg.GroupsClaim, cfg.EmailClaim = "realm_access.roles", "https://example.com/email"
	login := newBrowserLogin(t, cfg)
	cookies, callback := startSignIn(t, login, "/")
	_, _, p, err := finish(login, cookies, callback)
	require.NoError(t, err)
	assert.Equal(t, "kc@example.com", p.Email)
	assert.Equal(t, []string{selfGroup}, p.Groups)
}

func TestBrowserLoginIdPDenial(t *testing.T) {
	idp := newBrowserIdP(t)
	idp.SetUser(nil)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	cookies, callback := startSignIn(t, login, "/")
	_, _, _, err := finish(login, cookies, callback)
	require.ErrorIs(t, err, auth.ErrLoginDenied)
	assert.Contains(t, err.Error(), "access_denied")
}

func TestBrowserLoginWrongClientSecret(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.ClientSecret = "wrong"
	login := newBrowserLogin(t, cfg)
	cookies, callback := startSignIn(t, login, "/")
	_, _, _, err := finish(login, cookies, callback)
	require.ErrorIs(t, err, auth.ErrLoginProvider)
	assert.NotContains(t, err.Error(), callback.Query().Get("code"))
}

func TestBrowserLoginPublicClient(t *testing.T) {
	idp := newBrowserIdP(t)
	idp.RegisterClient("public-browser", "")
	cfg := browserLoginConfig(idp)
	cfg.ClientID, cfg.ClientSecret = "public-browser", ""
	login := newBrowserLogin(t, cfg)
	signIn(t, login)
}

func TestBrowserLoginJWKSOverride(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.JWKSURL = idp.JWKSURL
	signIn(t, newBrowserLogin(t, cfg))
}

func TestBrowserLoginDiscoveryFailure(t *testing.T) {
	cfg := browserLoginConfig(newBrowserIdP(t))
	cfg.Issuer = "http://127.0.0.1:1"
	login := newBrowserLogin(t, cfg)
	_, err := login.StartLogin(httptest.NewRecorder(), "/")
	require.ErrorIs(t, err, auth.ErrLoginProvider)
}

// tamper flips one bit in the middle of a sealed value.
func tamper(value string) string {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	raw[len(raw)/2] ^= 1
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestBrowserSessionRejectsForgedCookies(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	signedIn := signIn(t, login)
	session := signedIn[sessionCookie].Value

	cfg := browserLoginConfig(idp)
	cfg.SessionKeys = [][]byte{bytes.Repeat([]byte{9}, 32)}
	stranger := signIn(t, newBrowserLogin(t, cfg))

	pending, _ := startSignIn(t, login, "/")
	for name, value := range map[string]string{
		"garbage":            "not-a-session",
		"empty payload":      base64.RawURLEncoding.EncodeToString([]byte{1}),
		"flipped bit":        tamper(session),
		"truncated":          session[:len(session)-4],
		"other key":          stranger[sessionCookie].Value,
		"sign-in as session": pending[signInCookie].Value,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
			_, err := login.Authenticate(r)
			require.ErrorIs(t, err, auth.ErrInvalidCredentials)

			w := httptest.NewRecorder()
			_, err = login.AuthenticateBrowser(w, r)
			require.ErrorIs(t, err, auth.ErrInvalidCredentials)
			deleted := setCookie(w, sessionCookie)
			require.NotNil(t, deleted, "a cookie that no longer verifies is deleted")
			assert.Negative(t, deleted.MaxAge)
		})
	}

	// No cookie at all is not an invalid credential and sets nothing.
	w := httptest.NewRecorder()
	_, err := login.AuthenticateBrowser(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	require.ErrorIs(t, err, auth.ErrNoCredentials)
	assert.Empty(t, w.Result().Cookies())
}

func TestBrowserSessionKeyRotation(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.SessionKeys = [][]byte{retiredKey}
	old := signIn(t, newBrowserLogin(t, cfg))

	cfg.SessionKeys = [][]byte{currentKey, retiredKey}
	rotated := newBrowserLogin(t, cfg)
	_, err := rotated.Authenticate(old.request(http.MethodGet, "/admin/"))
	require.NoError(t, err, "the retired key still opens sessions")

	// Activity re-seals the session under the current key.
	w := httptest.NewRecorder()
	_, err = rotated.AuthenticateBrowser(w, old.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	resealed := jar{}
	resealed.store(w)
	require.Contains(t, resealed, sessionCookie)

	cfg.SessionKeys = [][]byte{currentKey}
	retired := newBrowserLogin(t, cfg)
	_, err = retired.Authenticate(resealed.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	_, err = retired.Authenticate(old.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "dropping the key ends its sessions")
}

func TestBrowserSessionLifetime(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.SessionTTL, cfg.IdleTimeout = 3*time.Hour, time.Hour
	login := newBrowserLogin(t, cfg)
	start := time.Now()
	clock := start
	auth.SetBrowserLoginClock(login, func() time.Time { return clock })
	cookies := signIn(t, login)

	visit := func(at time.Duration) error {
		clock = start.Add(at)
		w := httptest.NewRecorder()
		_, err := login.AuthenticateBrowser(w, cookies.request(http.MethodGet, "/admin/"))
		cookies.store(w)
		return err
	}
	// Activity inside the idle window slides it.
	require.NoError(t, visit(50*time.Minute))
	require.NoError(t, visit(100*time.Minute))
	require.NoError(t, visit(150*time.Minute))
	// The absolute lifetime does not slide.
	require.ErrorIs(t, visit(3*time.Hour), auth.ErrInvalidCredentials)
	assert.NotContains(t, cookies, sessionCookie)

	cookies = signIn(t, login)
	start = clock
	require.ErrorIs(t, visit(61*time.Minute), auth.ErrInvalidCredentials, "idle too long")
}

func TestBrowserSessionRefreshCookie(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	start := time.Now()
	clock := start
	auth.SetBrowserLoginClock(login, func() time.Time { return clock })
	cookies := signIn(t, login)

	w := httptest.NewRecorder()
	_, err := login.AuthenticateBrowser(w, cookies.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	assert.Nil(t, setCookie(w, sessionCookie), "no refresh within the refresh interval")

	clock = start.Add(2 * time.Minute)
	w = httptest.NewRecorder()
	_, err = login.AuthenticateBrowser(w, cookies.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	refreshed := setCookie(w, sessionCookie)
	require.NotNil(t, refreshed)
	assert.Equal(t, 3600, refreshed.MaxAge)
	assert.True(t, refreshed.Secure)
	assert.True(t, refreshed.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, refreshed.SameSite)
}

func TestBrowserLogoutDeletesCookies(t *testing.T) {
	idp := newBrowserIdP(t)
	login := newBrowserLogin(t, browserLoginConfig(idp))
	cookies := signIn(t, login)
	w := httptest.NewRecorder()
	login.Logout(w)
	for _, name := range []string{sessionCookie, signInCookie} {
		c := setCookie(w, name)
		require.NotNil(t, c, name)
		assert.Negative(t, c.MaxAge, name)
		// A __Host- cookie is replaced only with the same attributes.
		assert.True(t, c.Secure, name)
		assert.Equal(t, "/", c.Path, name)
	}
	cookies.store(w)
	_, err := login.Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrNoCredentials)
}

func TestBrowserLoginInsecureLoopbackCookies(t *testing.T) {
	idp := newBrowserIdP(t)
	cfg := browserLoginConfig(idp)
	cfg.PublicURL, cfg.InsecureCookies = "http://127.0.0.1:9091", true
	login := newBrowserLogin(t, cfg)
	w := httptest.NewRecorder()
	authURL, err := login.StartLogin(w, "/")
	require.NoError(t, err)
	assert.Contains(t, authURL, url.QueryEscape("http://127.0.0.1:9091/auth/callback"))
	c := setCookie(w, "diderot_signin")
	require.NotNil(t, c, "no __Host- prefix without Secure")
	assert.False(t, c.Secure)
	assert.True(t, c.HttpOnly)

	// Loopback HTTP without the opt-in keeps Secure cookies (browsers accept
	// them on loopback).
	cfg.InsecureCookies = false
	w = httptest.NewRecorder()
	_, err = newBrowserLogin(t, cfg).StartLogin(w, "/")
	require.NoError(t, err)
	require.NotNil(t, setCookie(w, signInCookie))
}

func TestNewBrowserLoginValidates(t *testing.T) {
	idp := newBrowserIdP(t)
	for name, mutate := range map[string]func(*auth.BrowserLoginConfig){
		"no client":            func(c *auth.BrowserLoginConfig) { c.ClientID = " " },
		"no openid scope":      func(c *auth.BrowserLoginConfig) { c.Scopes = []string{"email"} },
		"no public URL":        func(c *auth.BrowserLoginConfig) { c.PublicURL = "" },
		"plain HTTP":           func(c *auth.BrowserLoginConfig) { c.PublicURL = "http://diderot.example.com" },
		"insecure over https":  func(c *auth.BrowserLoginConfig) { c.InsecureCookies = true },
		"insecure off-host":    func(c *auth.BrowserLoginConfig) { c.PublicURL, c.InsecureCookies = "http://10.0.0.5", true },
		"insecure by name":     func(c *auth.BrowserLoginConfig) { c.PublicURL, c.InsecureCookies = "http://localhost:9091", true },
		"no keys":              func(c *auth.BrowserLoginConfig) { c.SessionKeys = nil },
		"short key":            func(c *auth.BrowserLoginConfig) { c.SessionKeys = [][]byte{currentKey[:16]} },
		"no session groups":    func(c *auth.BrowserLoginConfig) { c.SessionGroups = []string{" "} },
		"idle beyond lifetime": func(c *auth.BrowserLoginConfig) { c.IdleTimeout = 13 * time.Hour },
		"no lifetime":          func(c *auth.BrowserLoginConfig) { c.SessionTTL = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := browserLoginConfig(idp)
			mutate(&cfg)
			_, err := auth.NewBrowserLogin(cfg)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), string(currentKey))
		})
	}
	for _, origin := range []string{"http://localhost:9091", "http://[::1]:9091", "https://diderot.example.com/"} {
		cfg := browserLoginConfig(idp)
		cfg.PublicURL = origin
		_, err := auth.NewBrowserLogin(cfg)
		require.NoError(t, err, origin)
	}
}

func TestSafeReturnPath(t *testing.T) {
	for _, path := range []string{
		"/", "/admin/", "/admin/ask/sessions?before=42", "/sessions", "/sessions?notice=a%20b",
		"/admin/sources/slack/C123", "/%2F%2Fevil.example", "/@evil.example",
	} {
		assert.Equal(t, path, auth.SafeReturnPath(path), path)
	}
	for _, path := range []string{
		"", "admin", "https://evil.example/", "http:evil.example", "//evil.example", "///evil.example",
		"/\\evil.example", "\\\\evil.example", "/\t/evil.example", "/\n/evil.example", "/admin\r\nX: y",
		"javascript:alert(1)", "/ evil", "/café", "/auth/login", "/auth/callback?code=x",
		"/./auth/logout", "/admin/../auth/login", "/auth", strings.Repeat("/a", 1025),
	} {
		assert.Equal(t, "/", auth.SafeReturnPath(path), "%q", path)
	}
}
