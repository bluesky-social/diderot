package config

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authEnvironment clears every auth variable but requiredEnv's issuer and
// audience, the default mode's whole configuration, so a developer's shell
// cannot leak into the assertions.
func authEnvironment(t *testing.T) {
	t.Helper()
	requiredEnv(t)
	for _, key := range []string{
		"DIDEROT_AUTH_MODE", "DIDEROT_AUTH_STATIC_TOKENS_FILE", "DIDEROT_AUTH_NONE_EMAIL",
		"DIDEROT_OIDC_JWKS_URL",
		"DIDEROT_OIDC_GROUPS_CLAIM", "DIDEROT_OIDC_EMAIL_CLAIM", "DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED",
		"DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", "DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM",
		"DIDEROT_TRUSTED_PROXY_CIDRS", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "DIDEROT_TRUSTED_PROXY_GROUPS_HEADER",
		"DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER", "DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER",
		"DIDEROT_BROWSER_AUTH", "DIDEROT_BROWSER_OIDC_CLIENT_ID", "DIDEROT_BROWSER_OIDC_CLIENT_SECRET",
		"DIDEROT_OIDC_SCOPES", "DIDEROT_BROWSER_SESSION_KEYS", "DIDEROT_BROWSER_SESSION_KEYS_FILE",
		"DIDEROT_BROWSER_SESSION_TTL", "DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", "DIDEROT_BROWSER_SESSION_INSECURE_COOKIE",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_AuthDefaults(t *testing.T) {
	authEnvironment(t)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, AuthModeOIDC, cfg.AuthMode)
	assert.Equal(t, "https://idp.example.com", cfg.OIDCIssuer)
	assert.Empty(t, cfg.OIDCJWKSURL, "discovery from the issuer")
	assert.Equal(t, "groups", cfg.OIDCGroupsClaim)
	assert.Equal(t, "email", cfg.OIDCEmailClaim)
	assert.True(t, cfg.OIDCRequireEmailVerified)
	assert.Empty(t, cfg.OIDCAllowedEmailDomains)
	assert.Empty(t, cfg.OIDCHostedDomainClaim)
	assert.Empty(t, cfg.TrustedProxyCIDRs)
	assert.Empty(t, cfg.BrowserAuth)
	assert.Empty(t, cfg.BrowserSessionKeys)
}

// TestLoad_OIDCNeedsIssuerAndAudience: oidc, the default mode, without an
// issuer and an audience could authenticate no one, so it stops startup and
// names what to set, including the static-token alternative.
func TestLoad_OIDCNeedsIssuerAndAudience(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_ISSUER", "")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "")
	_, err := Load()
	require.ErrorContains(t, err, "DIDEROT_AUTH_MODE is oidc (the default), which needs DIDEROT_OIDC_ISSUER and DIDEROT_OIDC_AUDIENCE")
	require.ErrorContains(t, err, "DIDEROT_AUTH_MODE=static and DIDEROT_AUTH_STATIC_TOKENS_FILE")

	t.Setenv("DIDEROT_AUTH_MODE", "oidc")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "diderot")
	_, err = Load()
	require.ErrorContains(t, err, "needs DIDEROT_OIDC_ISSUER", "an audience alone")

	t.Setenv("DIDEROT_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_OIDC_AUDIENCE is required")

	// The static-token path needs no IdP at all.
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_ISSUER", "")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "")
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, AuthModeStatic, cfg.AuthMode)
}

func TestLoad_OIDCOverrides(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_ISSUER", "https://idp.example.com/realms/eng")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "diderot")
	t.Setenv("DIDEROT_OIDC_GROUPS_CLAIM", "realm_access.roles")
	t.Setenv("DIDEROT_OIDC_EMAIL_CLAIM", "https://example.com/email")
	t.Setenv("DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED", "false")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.OIDCJWKSURL)
	assert.Equal(t, "realm_access.roles", cfg.OIDCGroupsClaim)
	assert.Equal(t, "https://example.com/email", cfg.OIDCEmailClaim)
	assert.False(t, cfg.OIDCRequireEmailVerified)
}

// TestLoad_OIDCRequiresAnAudience: an audience is the only thing that makes
// a token a Diderot bearer.
func TestLoad_OIDCRequiresAnAudience(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "")
	_, err := Load()
	require.ErrorContains(t, err, "DIDEROT_OIDC_AUDIENCE is required")
}

func TestLoad_OIDCAllowedEmailDomains(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", " Example.com, example.org example.com,xn--bcher-kva.example ")
	t.Setenv("DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM", " hd ")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com", "example.org", "xn--bcher-kva.example"}, cfg.OIDCAllowedEmailDomains)
	assert.Equal(t, "hd", cfg.OIDCHostedDomainClaim)

	for _, raw := range []string{"@example.com", "*.example.com", "alice@example.com", "example..com", "https://example.com"} {
		t.Setenv("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", raw)
		_, err := Load()
		require.ErrorContains(t, err, "DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", raw)
	}

	t.Setenv("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", "")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM requires")

	// Nothing checks the list outside OIDC, so it may not be set there.
	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	t.Setenv("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", "example.com")
	_, err = Load()
	require.ErrorContains(t, err, "applies only to OIDC")
}

func TestLoad_AuthModes(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	_, err := Load()
	require.ErrorContains(t, err, "DIDEROT_AUTH_STATIC_TOKENS_FILE")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "/etc/diderot/tokens.yaml", cfg.StaticTokensFile)

	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "none")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_AUTH_NONE_EMAIL")
	t.Setenv("DIDEROT_AUTH_NONE_EMAIL", "Admin <admin@example.com>")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_AUTH_NONE_EMAIL")
	t.Setenv("DIDEROT_AUTH_NONE_EMAIL", " Admin@Example.com ")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "admin@example.com", cfg.NoneEmail)

	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "ldap")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_AUTH_MODE")

	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED", "maybe")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED")
}

func TestLoad_TrustedProxy(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_TRUSTED_PROXY_CIDRS", " 10.0.128.0/20, 10.0.150.7, fd00::/64 ,10.0.144.9/20")
	t.Setenv("DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "X-Auth-Request-Email")
	t.Setenv("DIDEROT_TRUSTED_PROXY_GROUPS_HEADER", "X-Auth-Request-Groups")
	t.Setenv("DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER", "|")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("10.0.128.0/20"),
		netip.MustParsePrefix("10.0.150.7/32"),
		netip.MustParsePrefix("fd00::/64"),
		netip.MustParsePrefix("10.0.144.0/20"),
	}, cfg.TrustedProxyCIDRs)
	assert.Equal(t, "X-Auth-Request-Email", cfg.TrustedProxyEmailHeader)
	assert.Equal(t, "X-Auth-Request-Groups", cfg.TrustedProxyGroupsHeader)
	assert.Equal(t, "|", cfg.TrustedProxyGroupsDelimiter)
	// /mcp falls back to the browser email header.
	assert.Equal(t, "X-Auth-Request-Email", cfg.TrustedProxyMCPEmailHeader)

	t.Setenv("DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER", "X-Auth-Request-User")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "X-Auth-Request-User", cfg.TrustedProxyMCPEmailHeader)
}

func TestLoad_TrustedProxyRejectsUnsafeConfig(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no email header":          {"DIDEROT_TRUSTED_PROXY_CIDRS": "10.0.0.0/8"},
		"groups without delimiter": {"DIDEROT_TRUSTED_PROXY_CIDRS": "10.0.0.0/8", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER": "X-Email", "DIDEROT_TRUSTED_PROXY_GROUPS_HEADER": "X-Groups"},
		"every IPv4 peer":          {"DIDEROT_TRUSTED_PROXY_CIDRS": "0.0.0.0/0", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER": "X-Email"},
		"every IPv6 peer":          {"DIDEROT_TRUSTED_PROXY_CIDRS": "::/0", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER": "X-Email"},
		"not an address":           {"DIDEROT_TRUSTED_PROXY_CIDRS": "proxy.example.svc", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER": "X-Email"},
		"bad CIDR":                 {"DIDEROT_TRUSTED_PROXY_CIDRS": "10.0.0.0/33", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER": "X-Email"},
	} {
		t.Run(name, func(t *testing.T) {
			authEnvironment(t)
			for key, value := range env {
				t.Setenv(key, value)
			}
			_, err := Load()
			require.Error(t, err)
		})
	}
}

// Two distinct 32-byte keys, in standard and URL-safe unpadded base64.
const (
	sessionKeyA = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	sessionKeyB = "_v7-_v7-_v7-_v7-_v7-_v7-_v7-_v7-_v7-_v7-_v4"
)

func browserOIDCEnvironment(t *testing.T) {
	t.Helper()
	authEnvironment(t)
	t.Setenv("DIDEROT_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "diderot")
	t.Setenv("DIDEROT_BROWSER_AUTH", "oidc")
	t.Setenv("DIDEROT_BROWSER_OIDC_CLIENT_ID", "diderot-web")
	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS", sessionKeyA)
}

func TestLoad_BrowserAuthOIDC(t *testing.T) {
	browserOIDCEnvironment(t)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, BrowserAuthOIDC, cfg.BrowserAuth)
	assert.Equal(t, "diderot-web", cfg.BrowserOIDCClientID)
	assert.Empty(t, cfg.BrowserOIDCClientSecret, "a public client needs no secret")
	assert.Equal(t, []string{"openid", "email", "profile"}, cfg.OIDCScopes)
	assert.Equal(t, [][]byte{bytes.Repeat([]byte{1}, 32)}, cfg.BrowserSessionKeys)
	assert.Equal(t, 12*time.Hour, cfg.BrowserSessionTTL)
	assert.Equal(t, time.Hour, cfg.BrowserSessionIdleTimeout)
	assert.False(t, cfg.BrowserSessionInsecureCookie)

	t.Setenv("DIDEROT_BROWSER_OIDC_CLIENT_SECRET", " web-secret ")
	t.Setenv("DIDEROT_OIDC_SCOPES", "openid,email profile groups")
	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS", sessionKeyB+", "+sessionKeyA)
	t.Setenv("DIDEROT_BROWSER_SESSION_TTL", "8h")
	t.Setenv("DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", "30m")
	t.Setenv("DIDEROT_BROWSER_SESSION_INSECURE_COOKIE", "true")
	// The proxy settings may stay for /mcp alone.
	t.Setenv("DIDEROT_TRUSTED_PROXY_CIDRS", "10.0.0.0/8")
	t.Setenv("DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "X-Auth-Request-Email")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "web-secret", cfg.BrowserOIDCClientSecret)
	assert.Equal(t, []string{"openid", "email", "profile", "groups"}, cfg.OIDCScopes)
	assert.Equal(t, [][]byte{bytes.Repeat([]byte{0xfe}, 32), bytes.Repeat([]byte{1}, 32)}, cfg.BrowserSessionKeys,
		"the first key seals")
	assert.Equal(t, 8*time.Hour, cfg.BrowserSessionTTL)
	assert.Equal(t, 30*time.Minute, cfg.BrowserSessionIdleTimeout)
	assert.True(t, cfg.BrowserSessionInsecureCookie)

	// Static API auth keeps the browser sign-in against the same issuer.
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, BrowserAuthOIDC, cfg.BrowserAuth)
}

// TestLoad_BrowserSessionKeysEveryMode: local sign-in needs session keys in
// static and trusted-proxy installs too, not only with the OIDC sign-in.
func TestLoad_BrowserSessionKeysEveryMode(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.BrowserSessionKeys, "keys stay optional outside the OIDC sign-in")

	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS", sessionKeyA)
	t.Setenv("DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", "30m")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.BrowserAuth)
	assert.Equal(t, [][]byte{bytes.Repeat([]byte{1}, 32)}, cfg.BrowserSessionKeys)
	assert.Equal(t, 12*time.Hour, cfg.BrowserSessionTTL)
	assert.Equal(t, 30*time.Minute, cfg.BrowserSessionIdleTimeout)

	t.Setenv("DIDEROT_AUTH_MODE", "oidc")
	t.Setenv("DIDEROT_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("DIDEROT_OIDC_AUDIENCE", "diderot")
	t.Setenv("DIDEROT_TRUSTED_PROXY_CIDRS", "10.0.0.0/8")
	t.Setenv("DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "X-Auth-Request-Email")
	t.Setenv("DIDEROT_BROWSER_AUTH", "trusted-proxy")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Len(t, cfg.BrowserSessionKeys, 1, "a trusted-proxy install may add local sign-in")

	t.Setenv("DIDEROT_PUBLIC_URL", "")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_PUBLIC_URL is required")
}

// TestLoad_BrowserSessionKeysFromFile: the keys can come from a mounted file
// instead of the variable, never from both.
func TestLoad_BrowserSessionKeysFromFile(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", "/etc/diderot/tokens.yaml")
	path := filepath.Join(t.TempDir(), "browser-session-keys")
	require.NoError(t, os.WriteFile(path, []byte(sessionKeyB+"\n"), 0o600))
	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS_FILE", path)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, [][]byte{bytes.Repeat([]byte{0xfe}, 32)}, cfg.BrowserSessionKeys)

	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS", sessionKeyA)
	_, err = Load()
	require.ErrorContains(t, err, "set DIDEROT_BROWSER_SESSION_KEYS or DIDEROT_BROWSER_SESSION_KEYS_FILE, not both")

	t.Setenv("DIDEROT_BROWSER_SESSION_KEYS", "")
	t.Setenv("DIDEROT_AUTH_MODE", "none")
	t.Setenv("DIDEROT_AUTH_NONE_EMAIL", "a@example.com")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_BROWSER_SESSION_KEYS_FILE must be unset with DIDEROT_AUTH_MODE=none")
}

func TestLoad_BrowserAuthTrustedProxy(t *testing.T) {
	authEnvironment(t)
	t.Setenv("DIDEROT_BROWSER_AUTH", "trusted-proxy")
	_, err := Load()
	require.ErrorContains(t, err, "DIDEROT_TRUSTED_PROXY_CIDRS")
	t.Setenv("DIDEROT_TRUSTED_PROXY_CIDRS", "10.0.0.0/8")
	t.Setenv("DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "X-Auth-Request-Email")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, BrowserAuthTrustedProxy, cfg.BrowserAuth)
}

// TestLoad_BrowserAuthRejectsAmbiguity: the browser has exactly one identity
// source, and a sign-in setting without the sign-in mode fails rather than
// being silently ignored.
func TestLoad_BrowserAuthRejectsAmbiguity(t *testing.T) {
	for name, test := range map[string]struct {
		env  map[string]string
		want string
	}{
		"unknown mode":       {map[string]string{"DIDEROT_BROWSER_AUTH": "saml"}, "DIDEROT_BROWSER_AUTH"},
		"none with oidc":     {map[string]string{"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "a@example.com"}, "DIDEROT_AUTH_MODE=none"},
		"none with proxy":    {map[string]string{"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "a@example.com", "DIDEROT_BROWSER_AUTH": "trusted-proxy"}, "DIDEROT_AUTH_MODE=none"},
		"stray client":       {map[string]string{"DIDEROT_BROWSER_AUTH": ""}, "DIDEROT_BROWSER_OIDC_CLIENT_ID is set"},
		"none with keys":     {map[string]string{"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "a@example.com", "DIDEROT_BROWSER_AUTH": "", "DIDEROT_BROWSER_OIDC_CLIENT_ID": ""}, "DIDEROT_BROWSER_SESSION_KEYS must be unset"},
		"lifetime, no keys":  {map[string]string{"DIDEROT_BROWSER_AUTH": "", "DIDEROT_BROWSER_OIDC_CLIENT_ID": "", "DIDEROT_BROWSER_SESSION_KEYS": "", "DIDEROT_BROWSER_SESSION_TTL": "8h"}, "DIDEROT_BROWSER_SESSION_TTL is set but DIDEROT_BROWSER_SESSION_KEYS is not"},
		"no issuer":          {map[string]string{"DIDEROT_OIDC_ISSUER": ""}, "DIDEROT_OIDC_ISSUER"},
		"no audience":        {map[string]string{"DIDEROT_OIDC_AUDIENCE": ""}, "DIDEROT_OIDC_AUDIENCE is required"},
		"no client":          {map[string]string{"DIDEROT_BROWSER_OIDC_CLIENT_ID": ""}, "DIDEROT_BROWSER_OIDC_CLIENT_ID"},
		"no keys":            {map[string]string{"DIDEROT_BROWSER_SESSION_KEYS": " , "}, "DIDEROT_BROWSER_SESSION_KEYS"},
		"short key":          {map[string]string{"DIDEROT_BROWSER_SESSION_KEYS": "c2hvcnQta2V5"}, "32 bytes"},
		"not base64":         {map[string]string{"DIDEROT_BROWSER_SESSION_KEYS": "hunter2!"}, "32 bytes"},
		"repeated key":       {map[string]string{"DIDEROT_BROWSER_SESSION_KEYS": sessionKeyA + "," + sessionKeyA}, "repeats"},
		"no openid":          {map[string]string{"DIDEROT_OIDC_SCOPES": "email profile"}, "openid"},
		"lifetime too long":  {map[string]string{"DIDEROT_BROWSER_SESSION_TTL": "200h"}, "DIDEROT_BROWSER_SESSION_TTL"},
		"bad lifetime":       {map[string]string{"DIDEROT_BROWSER_SESSION_TTL": "-1h"}, "DIDEROT_BROWSER_SESSION_TTL"},
		"idle past lifetime": {map[string]string{"DIDEROT_BROWSER_SESSION_TTL": "1h", "DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT": "2h"}, "IDLE_TIMEOUT"},
		"bad insecure flag":  {map[string]string{"DIDEROT_BROWSER_SESSION_INSECURE_COOKIE": "dev"}, "INSECURE_COOKIE"},
	} {
		t.Run(name, func(t *testing.T) {
			browserOIDCEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := Load()
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "hunter2")
			assert.NotContains(t, err.Error(), "c2hvcnQta2V5")
		})
	}
}
