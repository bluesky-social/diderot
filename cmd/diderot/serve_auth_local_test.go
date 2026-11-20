package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
)

type fixedBootstrap bool

func (b fixedBootstrap) BootstrapAdminEnabled(context.Context) (bool, error) { return bool(b), nil }

// testSessionKey is base64 of 32 bytes of 0x01.
const testSessionKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

// TestBuildServerAuthLocalSignIn: session keys give a static-mode install a
// browser sign-in (the bootstrap admin and pasted tokens), and give a
// trusted-proxy install the bootstrap admin beside its proxy identity.
func TestBuildServerAuthLocalSignIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	// sha256("operator-token")
	require.NoError(t, os.WriteFile(path, []byte(`tokens:
  - sha256: 0850123315d21ab90f4f7236408a52ef6dbd6a02a6550e5c10dc73f4d993680e
    email: operator@example.com
    groups: [admins]
`), 0o600))
	cfg := loadAuthConfig(t, map[string]string{
		"DIDEROT_AUTH_MODE": "static", "DIDEROT_AUTH_STATIC_TOKENS_FILE": path,
		"DIDEROT_BROWSER_SESSION_KEYS": testSessionKey,
	})
	sa, err := buildServerAuth(cfg, []string{":9091", ":9092"}, fixedBootstrap(true), discardLogger())
	require.NoError(t, err)
	assert.True(t, sa.localSignIn)
	assert.True(t, sa.browserIdentity)
	login, ok := sa.browser.(*auth.BrowserLogin)
	require.True(t, ok)
	assert.False(t, login.OIDCEnabled())
	assert.True(t, login.StaticTokensEnabled())
	enabled, err := login.BootstrapEnabled(context.Background())
	require.NoError(t, err)
	assert.True(t, enabled)

	w := httptest.NewRecorder()
	_, err = login.SignInToken(w, "operator-token")
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	p, err := sa.browser.Authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, "operator@example.com", p.Email)
	_, err = sa.api.Authenticate(r)
	require.ErrorIs(t, err, auth.ErrNoCredentials, "a browser session is never an API credential")

	idp := authtest.NewIdP(t)
	env := proxyAuthEnv(idp.JWKSURL)
	env["DIDEROT_BROWSER_SESSION_KEYS"] = testSessionKey
	sa, err = buildServerAuth(loadAuthConfig(t, env), []string{":9091", ":9092"}, fixedBootstrap(true), discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "trusted-proxy", sa.browserMode)
	login, ok = sa.browser.(*auth.BrowserLogin)
	require.True(t, ok)
	assert.False(t, login.StaticTokensEnabled(), "no static tokens outside static mode")
	r = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.RemoteAddr = proxyPeer
	r.Header.Set("X-Auth-Request-Email", "op@example.com")
	p, err = sa.browser.Authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, "op@example.com", p.Email, "proxy identity still works without a session")
}
