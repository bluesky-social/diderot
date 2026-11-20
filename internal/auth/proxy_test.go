package auth_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
)

func testProxy(t *testing.T, cidrs ...string) *auth.TrustedProxy {
	t.Helper()
	var prefixes []netip.Prefix
	for _, cidr := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	p, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs: prefixes, EmailHeader: "X-Auth-Request-Email",
		GroupsHeader: "X-Auth-Request-Groups", GroupsDelimiter: "|",
	})
	require.NoError(t, err)
	return p
}

func proxiedRequest(peer string, headers map[string][]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.RemoteAddr = peer
	for name, values := range headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	return r
}

func TestTrustedProxyHonorsHeadersOnlyFromTrustedPeers(t *testing.T) {
	p := testProxy(t, "10.0.128.0/20", "fd00::/64")
	identity := map[string][]string{
		"X-Auth-Request-Email":  {" Operator@Example.com "},
		"X-Auth-Request-Groups": {"everyone| admins |"},
	}

	for _, peer := range []string{"10.0.139.128:51234", "[fd00::5]:443", "[::ffff:10.0.130.1]:80"} {
		principal, err := p.Authenticate(proxiedRequest(peer, identity))
		require.NoError(t, err, peer)
		assert.Equal(t, &auth.Principal{
			Subject: "Operator@Example.com", Email: "Operator@Example.com", EmailVerified: true,
			Display: "Operator@Example.com", Groups: []string{"everyone", "admins"},
		}, principal, peer)
	}

	// X-Forwarded-For never upgrades an untrusted hop.
	spoofed := proxiedRequest("10.0.200.1:51234", identity)
	spoofed.Header.Set("X-Forwarded-For", "10.0.139.128")
	for name, r := range map[string]*http.Request{
		"outside CIDR":      proxiedRequest("10.0.200.1:51234", identity),
		"loopback":          proxiedRequest("127.0.0.1:51234", identity),
		"forwarded-for":     spoofed,
		"unparseable peer":  proxiedRequest("not-an-address", identity),
		"trusted, no email": proxiedRequest("10.0.139.128:1", map[string][]string{"X-Auth-Request-Groups": {"admins"}}),
		"trusted, blank":    proxiedRequest("10.0.139.128:1", map[string][]string{"X-Auth-Request-Email": {"  "}}),
		"repeated email": proxiedRequest("10.0.139.128:1", map[string][]string{
			"X-Auth-Request-Email": {"a@example.com", "b@example.com"},
		}),
	} {
		_, err := p.Authenticate(r)
		require.ErrorIs(t, err, auth.ErrNoCredentials, name)
	}
}

func TestTrustedProxyRepeatedGroupsHeaderGrantsNoGroups(t *testing.T) {
	p := testProxy(t, "10.0.128.0/20")
	principal, err := p.Authenticate(proxiedRequest("10.0.139.128:1", map[string][]string{
		"X-Auth-Request-Email":  {"a@example.com"},
		"X-Auth-Request-Groups": {"everyone", "admins"},
	}))
	require.NoError(t, err)
	assert.Equal(t, "a@example.com", principal.Email)
	assert.Empty(t, principal.Groups)
}

func TestTrustedProxyWithoutCIDRsIgnoresHeaders(t *testing.T) {
	p, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{EmailHeader: "X-Auth-Request-Email"})
	require.NoError(t, err)
	assert.False(t, p.Enabled())
	_, err = p.Authenticate(proxiedRequest("127.0.0.1:1", map[string][]string{"X-Auth-Request-Email": {"a@example.com"}}))
	require.ErrorIs(t, err, auth.ErrNoCredentials)
}

func TestTrustedProxyConfigValidation(t *testing.T) {
	cidrs := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for name, cfg := range map[string]auth.TrustedProxyConfig{
		"no email header":          {CIDRs: cidrs},
		"invalid email header":     {CIDRs: cidrs, EmailHeader: "X Email"},
		"groups without delimiter": {CIDRs: cidrs, EmailHeader: "X-Email", GroupsHeader: "X-Groups"},
		"invalid prefix":           {CIDRs: []netip.Prefix{{}}, EmailHeader: "X-Email"},
	} {
		_, err := auth.NewTrustedProxy(cfg)
		require.Error(t, err, name)
	}
}

func TestStaticTokens(t *testing.T) {
	// sha256("operator-token") and sha256("reader-token").
	static, err := auth.ParseStaticTokens([]byte(`
tokens:
  - sha256: 0850123315d21ab90f4f7236408a52ef6dbd6a02a6550e5c10dc73f4d993680e
    email: " Operator@Example.com "
    groups: [admins, " everyone ", ""]
  - sha256: BA5005A40CF5212E4AC0190104CC127EDAB013294BB71279A975B27A80982D45
    email: reader@example.com
`))
	require.NoError(t, err)

	_, err = static.Authenticate(bearerRequest(""))
	require.ErrorIs(t, err, auth.ErrNoCredentials)
	_, err = static.Authenticate(bearerRequest("wrong-token"))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)

	p, err := static.Authenticate(bearerRequest("operator-token"))
	require.NoError(t, err)
	assert.Equal(t, &auth.Principal{
		Subject: "static:operator@example.com", Email: "operator@example.com", EmailVerified: true,
		Display: "operator@example.com", Groups: []string{"admins", "everyone"},
	}, p)

	// Callers cannot mutate the configured identity through a returned principal.
	p.Groups[0] = "mutated"
	again, err := static.Authenticate(bearerRequest("operator-token"))
	require.NoError(t, err)
	assert.Equal(t, "admins", again.Groups[0])

	// Uppercase hex digests are accepted; a token without groups is still
	// authenticated (use routes only).
	reader, err := static.Authenticate(bearerRequest("reader-token"))
	require.NoError(t, err)
	assert.Equal(t, "reader@example.com", reader.Email)
	assert.Empty(t, reader.Groups)

	assert.True(t, static.AnyInGroups([]string{"other", "everyone"}), "groups are compared trimmed")
	assert.False(t, static.AnyInGroups([]string{"other"}))
}

func TestStaticTokensValidation(t *testing.T) {
	const hash = "0850123315d21ab90f4f7236408a52ef6dbd6a02a6550e5c10dc73f4d993680e"
	for name, document := range map[string]string{
		"empty":         "tokens: []",
		"short hash":    "tokens: [{sha256: abc, email: a@example.com}]",
		"no email":      "tokens: [{sha256: " + hash + "}]",
		"display email": "tokens: [{sha256: " + hash + ", email: 'A <a@example.com>'}]",
		"duplicate":     "tokens: [{sha256: " + hash + ", email: a@example.com}, {sha256: " + hash + ", email: b@example.com}]",
		"unknown field": "tokens: [{sha256: " + hash + ", email: a@example.com, admin: true}]",
		"two documents": "tokens: [{sha256: " + hash + ", email: a@example.com}]\n---\ntokens: []",
	} {
		_, err := auth.ParseStaticTokens([]byte(document))
		require.Error(t, err, name)
	}
}

func TestFixedIgnoresRequestAndReturnsCopies(t *testing.T) {
	fixed := auth.NewFixed(auth.Principal{Subject: "admin@example.com", Email: "admin@example.com", Groups: []string{"admins"}})
	r := proxiedRequest("198.51.100.1:1", map[string][]string{
		"Authorization":        {"Bearer anything"},
		"X-Auth-Request-Email": {"attacker@example.com"},
	})
	p, err := fixed.Authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, "admin@example.com", p.Email)
	p.Groups[0] = "mutated"
	again, err := fixed.Authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, []string{"admins"}, again.Groups)
}
