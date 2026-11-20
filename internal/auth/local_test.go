package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
)

// bootstrapSwitch is a BootstrapAdmin whose answer the test flips.
type bootstrapSwitch struct {
	enabled atomic.Bool
	err     atomic.Pointer[error]
	calls   atomic.Int32
}

func (s *bootstrapSwitch) BootstrapAdminEnabled(context.Context) (bool, error) {
	s.calls.Add(1)
	if err := s.err.Load(); err != nil {
		return false, *err
	}
	return s.enabled.Load(), nil
}

const operatorToken = "operator-token-0123456789abcdef"

func staticTokens(t *testing.T, tokens ...string) *auth.Static {
	t.Helper()
	var entries []auth.StaticToken
	for i, token := range tokens {
		digest := sha256.Sum256([]byte(token))
		entries = append(entries, auth.StaticToken{
			SHA256: hex.EncodeToString(digest[:]), Email: []string{"op@example.com", "second@example.com"}[i],
			Groups: []string{adminGroup},
		})
	}
	static, err := auth.NewStatic(entries)
	require.NoError(t, err)
	return static
}

// localLogin is a browser sign-in with no identity provider.
func localLogin(t *testing.T, bootstrap auth.BootstrapAdmin, static *auth.Static, proxy *auth.TrustedProxy) *auth.BrowserLogin {
	t.Helper()
	login, err := auth.NewBrowserLogin(auth.BrowserLoginConfig{
		PublicURL: browserOrigin, SessionKeys: [][]byte{currentKey},
		SessionTTL: 12 * time.Hour, IdleTimeout: time.Hour,
		Bootstrap: bootstrap, StaticTokens: static, Proxy: proxy,
	})
	require.NoError(t, err)
	return login
}

func bootstrapSignIn(t *testing.T, login *auth.BrowserLogin, username, password string) (jar, *auth.Principal, error) {
	t.Helper()
	cookies := jar{}
	w := httptest.NewRecorder()
	p, err := login.SignInBootstrap(w, httptest.NewRequest(http.MethodPost, "/auth/login/bootstrap", nil), username, password)
	cookies.store(w)
	return cookies, p, err
}

func TestBootstrapSignInWhileEnabled(t *testing.T) {
	enabled := &bootstrapSwitch{}
	enabled.enabled.Store(true)
	login := localLogin(t, enabled, nil, nil)
	assert.False(t, login.OIDCEnabled())
	assert.False(t, login.StaticTokensEnabled())
	_, err := login.StartLogin(httptest.NewRecorder(), "/")
	require.ErrorIs(t, err, auth.ErrLoginProvider, "no identity provider is configured")

	for _, creds := range [][2]string{{"admin", "wrong"}, {"root", "admin"}, {"", ""}, {"admin ", "admin"}} {
		cookies, _, err := bootstrapSignIn(t, login, creds[0], creds[1])
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, creds)
		assert.NotContains(t, cookies, sessionCookie)
	}

	cookies, principal, err := bootstrapSignIn(t, login, "admin", "admin")
	require.NoError(t, err)
	require.Contains(t, cookies, sessionCookie)
	for _, got := range []*auth.Principal{principal, mustAuthenticate(t, login, cookies)} {
		assert.True(t, got.Bootstrap)
		assert.Equal(t, auth.BootstrapSubject, got.Subject)
		assert.Equal(t, auth.BootstrapEmail, got.Email)
		assert.Empty(t, got.Groups, "the bootstrap admin is not faked into any group")
		assert.False(t, got.EmailVerified)
	}
}

func mustAuthenticate(t *testing.T, login *auth.BrowserLogin, cookies jar) *auth.Principal {
	t.Helper()
	p, err := login.Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.NoError(t, err)
	return p
}

// TestBootstrapSessionEndsWhenDisabled: the account is checked against the
// source of truth on every request, so turning it off ends an open session
// at once, and it can no longer sign in.
func TestBootstrapSessionEndsWhenDisabled(t *testing.T) {
	state := &bootstrapSwitch{}
	state.enabled.Store(true)
	login := localLogin(t, state, nil, nil)
	cookies, _, err := bootstrapSignIn(t, login, "admin", "admin")
	require.NoError(t, err)
	before := state.calls.Load()
	mustAuthenticate(t, login, cookies)
	mustAuthenticate(t, login, cookies)
	assert.Equal(t, before+2, state.calls.Load(), "every request rechecks the account")

	state.enabled.Store(false)
	w := httptest.NewRecorder()
	_, err = login.AuthenticateBrowser(w, cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrBootstrapDisabled)
	deleted := setCookie(w, sessionCookie)
	require.NotNil(t, deleted)
	assert.Negative(t, deleted.MaxAge)

	_, _, err = bootstrapSignIn(t, login, "admin", "admin")
	require.ErrorIs(t, err, auth.ErrBootstrapDisabled)

	// A failure to read the switch is not proof the session is bad: it is
	// refused but kept.
	state.enabled.Store(true)
	cookies, _, err = bootstrapSignIn(t, login, "admin", "admin")
	require.NoError(t, err)
	backendDown := errors.New("database unavailable")
	state.err.Store(&backendDown)
	w = httptest.NewRecorder()
	_, err = login.AuthenticateBrowser(w, cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, backendDown)
	assert.NotErrorIs(t, err, auth.ErrInvalidCredentials)
	assert.Nil(t, setCookie(w, sessionCookie))
}

func TestBootstrapSignInUnavailableWithoutSwitch(t *testing.T) {
	login := localLogin(t, nil, nil, nil)
	enabled, err := login.BootstrapEnabled(context.Background())
	require.NoError(t, err)
	assert.False(t, enabled)
	_, _, err = bootstrapSignIn(t, login, "admin", "admin")
	require.ErrorIs(t, err, auth.ErrLocalSignInUnavailable)
}

func TestStaticTokenSignIn(t *testing.T) {
	login := localLogin(t, nil, staticTokens(t, operatorToken), nil)
	require.True(t, login.StaticTokensEnabled())

	w := httptest.NewRecorder()
	_, err := login.SignInToken(w, "not-a-configured-token")
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	assert.Nil(t, setCookie(w, sessionCookie))

	cookies := jar{}
	w = httptest.NewRecorder()
	principal, err := login.SignInToken(w, "  "+operatorToken+"\n")
	require.NoError(t, err)
	cookies.store(w)
	require.Contains(t, cookies, sessionCookie)
	assert.NotContains(t, cookies[sessionCookie].Value, operatorToken)
	assert.Equal(t, "op@example.com", principal.Email)
	got := mustAuthenticate(t, login, cookies)
	assert.Equal(t, "op@example.com", got.Email)
	assert.Equal(t, []string{adminGroup}, got.Groups)
	assert.False(t, got.Bootstrap)

	// A restart that loads a token set without that token ends the session.
	reloaded := localLogin(t, nil, staticTokens(t, "a-different-token-0123456789abcd"), nil)
	_, err = reloaded.Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	// And one without static mode at all.
	_, err = localLogin(t, nil, nil, nil).Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

// TestLocalSessionBeforeProxyIdentity: behind a trusted proxy, a valid local
// session wins; without one, or once it stops verifying, the proxy's
// headers identify the browser.
func TestLocalSessionBeforeProxyIdentity(t *testing.T) {
	proxy, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, EmailHeader: "X-Proxy-Email",
	})
	require.NoError(t, err)
	state := &bootstrapSwitch{}
	state.enabled.Store(true)
	login := localLogin(t, state, nil, proxy)
	viaProxy := func(cookies jar) *http.Request {
		r := cookies.request(http.MethodGet, "/admin/")
		r.Header.Set("X-Proxy-Email", "proxied@example.com")
		return r
	}

	got, err := login.Authenticate(viaProxy(jar{}))
	require.NoError(t, err)
	assert.Equal(t, "proxied@example.com", got.Email)

	cookies, _, err := bootstrapSignIn(t, login, "admin", "admin")
	require.NoError(t, err)
	got, err = login.Authenticate(viaProxy(cookies))
	require.NoError(t, err)
	assert.True(t, got.Bootstrap, "the local session wins over proxy headers")

	state.enabled.Store(false)
	w := httptest.NewRecorder()
	got, err = login.AuthenticateBrowser(w, viaProxy(cookies))
	require.NoError(t, err)
	assert.Equal(t, "proxied@example.com", got.Email)
	require.NotNil(t, setCookie(w, sessionCookie), "the dead session is still deleted")

	_, err = login.Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "without proxy headers the session's own error stands")
}

// TestOIDCSessionNeedsOIDC: a session from a sign-in method the deployment no
// longer configures does not verify, even under the same keys.
func TestOIDCSessionNeedsOIDC(t *testing.T) {
	idp := newBrowserIdP(t)
	cookies := signIn(t, newBrowserLogin(t, browserLoginConfig(idp)))
	_, err := localLogin(t, nil, nil, nil).Authenticate(cookies.request(http.MethodGet, "/admin/"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	require.True(t, strings.Contains(err.Error(), "OIDC"), err.Error())
}
