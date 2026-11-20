package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/retrieve"
)

var authEnvKeys = []string{
	"DIDEROT_AUTH_MODE", "DIDEROT_AUTH_STATIC_TOKENS_FILE", "DIDEROT_AUTH_NONE_EMAIL",
	"DIDEROT_OIDC_ISSUER", "DIDEROT_OIDC_JWKS_URL", "DIDEROT_OIDC_AUDIENCE",
	"DIDEROT_OIDC_GROUPS_CLAIM", "DIDEROT_OIDC_EMAIL_CLAIM", "DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED",
	"DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS", "DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM",
	"DIDEROT_TRUSTED_PROXY_CIDRS", "DIDEROT_TRUSTED_PROXY_EMAIL_HEADER", "DIDEROT_TRUSTED_PROXY_GROUPS_HEADER",
	"DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER", "DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER",
	"DIDEROT_BROWSER_AUTH", "DIDEROT_BROWSER_OIDC_CLIENT_ID", "DIDEROT_BROWSER_OIDC_CLIENT_SECRET",
	"DIDEROT_OIDC_SCOPES", "DIDEROT_BROWSER_SESSION_KEYS", "DIDEROT_BROWSER_SESSION_TTL",
	"DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", "DIDEROT_BROWSER_SESSION_INSECURE_COOKIE",
}

// serveEnv sets every variable config.Load requires, so a test starts from a
// configuration that loads and a developer's shell cannot leak into it.
func serveEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DIDEROT_DB_URL":              "postgres://diderot@db.invalid/diderot",
		"DIDEROT_PUBLIC_URL":          "https://diderot.example.com",
		"DIDEROT_AUTH_MODE":           "",
		"DIDEROT_OIDC_ISSUER":         "https://idp.example.com",
		"DIDEROT_OIDC_AUDIENCE":       "diderot",
		"DIDEROT_QDRANT_ADDR":         "qdrant.invalid:6334",
		"DIDEROT_ADMIN_GROUPS":        "admins",
		"DIDEROT_SELF_SERVICE_GROUPS": "everyone",
	} {
		t.Setenv(key, value)
	}
}

// loadAuthConfig loads config from env with every auth variable cleared
// first, so a developer's shell cannot leak into the assertions.
func loadAuthConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	serveEnv(t)
	for _, key := range authEnvKeys {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildServerAuthNoneRequiresLoopbackListeners(t *testing.T) {
	cfg := loadAuthConfig(t, map[string]string{
		"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "admin@example.com",
	})
	for _, addrs := range [][]string{
		{":9091", "127.0.0.1:9092"},
		{"127.0.0.1:9091", "0.0.0.0:9092"},
		{"127.0.0.1:9091", "localhost:9092"},
		{"[::1]:9091", "10.0.0.5:9092"},
		{"127.0.0.1:9091", "127.0.0.1:9092", ":9093"},
	} {
		_, err := buildServerAuth(cfg, addrs, nil, discardLogger())
		require.ErrorContains(t, err, "loopback", addrs)
	}

	sa, err := buildServerAuth(cfg, []string{"127.0.0.1:9091", "[::1]:9092"}, nil, discardLogger())
	require.NoError(t, err)
	assert.True(t, sa.browserIdentity)
	assert.Equal(t, "none", sa.browserMode, "none mode needs no sign-in")
	// Every surface is the one configured administrator, whatever the
	// request carries.
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.Header.Set("X-Auth-Request-Email", "attacker@example.com")
	r.Header.Set("Authorization", "Bearer forged")
	for _, a := range []auth.Authenticator{sa.api, sa.browser, sa.mcp} {
		p, err := a.Authenticate(r)
		require.NoError(t, err)
		assert.Equal(t, "admin@example.com", p.Email)
		assert.True(t, p.EmailVerified)
		assert.Equal(t, []string{"admins", "everyone"}, p.Groups)
	}
}

// TestServeListenAddrsIncludeWebhookAndMetrics: the webhook and metrics
// listeners always bind, and only to the loopback where every listener must.
func TestServeListenAddrsIncludeWebhookAndMetrics(t *testing.T) {
	t.Setenv("DIDEROT_LISTEN_ADDR", "127.0.0.1:9091")
	t.Setenv("DIDEROT_INTERNAL_LISTEN_ADDR", "127.0.0.1:9092")
	t.Setenv("DIDEROT_METRICS_LISTEN_ADDR", "")
	t.Setenv("DIDEROT_PAGERDUTY_LISTEN_ADDR", "")
	assert.Equal(t, []string{"127.0.0.1:9091", "127.0.0.1:9092", ":9093", ":9094"},
		serveListenAddrs(&config.Config{AuthMode: config.AuthModeOIDC}))
	none := &config.Config{AuthMode: config.AuthModeNone, NoneEmail: "admin@example.com"}
	addrs := serveListenAddrs(none)
	assert.Equal(t, []string{"127.0.0.1:9091", "127.0.0.1:9092", "127.0.0.1:9093", "127.0.0.1:9094"}, addrs)
	_, err := buildServerAuth(none, addrs, nil, discardLogger())
	require.NoError(t, err, "none mode still starts")
	assert.Len(t, listenerNames, len(addrs), "every listener has a name")

	t.Setenv("DIDEROT_METRICS_LISTEN_ADDR", ":9190")
	addrs = serveListenAddrs(none)
	assert.Equal(t, ":9190", addrs[3])
	_, err = buildServerAuth(none, addrs, nil, discardLogger())
	require.ErrorContains(t, err, "loopback", "none mode refuses a metrics listener on every interface")

	t.Setenv("DIDEROT_METRICS_LISTEN_ADDR", "")
	t.Setenv("DIDEROT_PAGERDUTY_LISTEN_ADDR", "127.0.0.1:9193")
	assert.Equal(t, "127.0.0.1:9193", serveListenAddrs(none)[2])
	t.Setenv("DIDEROT_PAGERDUTY_LISTEN_ADDR", ":9193")
	addrs = serveListenAddrs(&config.Config{AuthMode: config.AuthModeOIDC})
	assert.Equal(t, ":9193", addrs[2])
	_, err = buildServerAuth(none, serveListenAddrs(none), nil, discardLogger())
	require.ErrorContains(t, err, "loopback", "none mode refuses a webhook listener on every interface")
}

func TestBuildServerAuthStatic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	// sha256("operator-token")
	require.NoError(t, os.WriteFile(path, []byte(`tokens:
  - sha256: 0850123315d21ab90f4f7236408a52ef6dbd6a02a6550e5c10dc73f4d993680e
    email: operator@example.com
    groups: [admins]
`), 0o600))
	cfg := loadAuthConfig(t, map[string]string{
		"DIDEROT_AUTH_MODE": "static", "DIDEROT_AUTH_STATIC_TOKENS_FILE": path,
	})
	sa, err := buildServerAuth(cfg, []string{":9091", ":9092"}, nil, discardLogger())
	require.NoError(t, err)
	// No trusted proxy: the browser UI has no identity source.
	assert.False(t, sa.browserIdentity)

	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", nil)
	r.Header.Set("Authorization", "Bearer operator-token")
	p, err := sa.api.Authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, "operator@example.com", p.Email)
	_, err = sa.browser.Authenticate(r)
	require.ErrorIs(t, err, auth.ErrNoCredentials)

	cfg.StaticTokensFile = filepath.Join(t.TempDir(), "missing.yaml")
	_, err = buildServerAuth(cfg, []string{":9091", ":9092"}, nil, discardLogger())
	require.Error(t, err)
}

// A trusted-proxy deployment: an OIDC provider for /v1, an authenticating
// reverse proxy in front of the browser UI, and a gateway in front of /mcp.
// Every value is a placeholder.
const (
	testIssuer   = "https://auth.example.com/application/o/diderot/"
	testAudience = "diderot-public-client-id"
	proxyPeer    = "10.0.139.128:51234" // the authenticating proxy
	gatewayPeer  = "10.0.155.117:40112" // the gateway in front of /mcp
	outsidePeer  = "10.1.0.5:51234"     // any other caller that reaches the port
	gatewayKey   = "gateway-backend-token"
)

// proxyAuthEnv is that deployment's auth environment.
func proxyAuthEnv(jwksURL string) map[string]string {
	return map[string]string{
		"DIDEROT_OIDC_ISSUER":         testIssuer,
		"DIDEROT_OIDC_JWKS_URL":       jwksURL,
		"DIDEROT_OIDC_AUDIENCE":       testAudience,
		"DIDEROT_ADMIN_GROUPS":        "admins,platform,ops",
		"DIDEROT_SELF_SERVICE_GROUPS": "everyone",

		// The provider's access tokens carry email_verified=false.
		"DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED": "false",
		// The proxy's (browser) and the gateway's (/mcp) pod networks.
		"DIDEROT_TRUSTED_PROXY_CIDRS":            "10.0.128.0/20,10.0.144.0/20,10.0.160.0/20",
		"DIDEROT_TRUSTED_PROXY_EMAIL_HEADER":     "X-Auth-Request-Email",
		"DIDEROT_TRUSTED_PROXY_GROUPS_HEADER":    "X-Auth-Request-Groups",
		"DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER": "|",
		"DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER": "X-Auth-Request-User",
	}
}

// supportPolicy grants one group one source beyond the baseline scopes.
const supportPolicy = `authorization:
  grants:
    - principals: [group:support-team]
      permissions:
        - action: retrieve
          resource: corpus:zendesk
`

type recordingScopes struct {
	emails []string
}

func (r *recordingScopes) ScopesForEmail(_ context.Context, email string) []string {
	r.emails = append(r.emails, email)
	return []string{"public", "internal", "slack-channel:CPRIVATE"}
}

type recordingRetriever struct {
	scopes []string
}

func (r *recordingRetriever) Retrieve(_ context.Context, req retrieve.Request) (*retrieve.Result, error) {
	r.scopes = req.Filters.AllowedScopes
	return &retrieve.Result{}, nil
}

func (r *recordingRetriever) List(context.Context, retrieve.ListRequest) (*retrieve.Result, error) {
	return &retrieve.Result{}, nil
}

type authStack struct {
	auth      *serverAuth
	mux       *http.ServeMux
	scopes    *recordingScopes
	retriever *recordingRetriever
	mcpScopes []string
}

// newAuthStack assembles the API, browser UI, and MCP surfaces the way serve
// does, from cfg alone. No backend is wired, so a request that passes its
// authorization gate answers 503 (or the UI's 303/404) instead of 401/403.
func newAuthStack(t *testing.T, cfg *config.Config) *authStack {
	t.Helper()
	sa, err := buildServerAuth(cfg, []string{":9091", ":9092"}, nil, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, sa)
	authorizer, err := authorization.Parse([]byte(supportPolicy))
	require.NoError(t, err)

	stack := &authStack{auth: sa, mux: http.NewServeMux(), scopes: &recordingScopes{}, retriever: &recordingRetriever{}}
	admin := adminapi.NewServer(sa.api, discardLogger())
	admin.SetAuthorizer(authorizer)
	admin.SetAdminGroups(cfg.AdminGroups)
	admin.SetSelfServiceGroups(cfg.SelfServiceGroups)
	admin.SetScopeResolver(stack.scopes)
	admin.SetRetriever(stack.retriever)
	require.NoError(t, admin.Mount(stack.mux))

	// The UI routes exercised here never query; the pool connects lazily.
	pool, err := pgxpool.New(context.Background(), "postgres://diderot@127.0.0.1:1/unused")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ui, err := adminui.New(adminui.Config{
		Pool: pool, Logger: discardLogger(), Authenticator: sa.browser,
		AdminGroups: cfg.AdminGroups, SelfServiceGroups: cfg.SelfServiceGroups, PublicURL: cfg.PublicURL,
		AllowInsecureLoopbackCookies: cfg.BrowserSessionInsecureCookie,
	})
	require.NoError(t, err)
	require.NoError(t, ui.Mount(stack.mux))

	require.NoError(t, admin.MountMCP(stack.mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack.mcpScopes = ask.AllowedScopesFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), gatewayKey, sa.mcp, nil))
	return stack
}

func (s *authStack) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

// accessToken is an access token from a provider that does not verify
// addresses: email_verified is false, and groups and preferred_username are
// set.
func accessToken(t *testing.T, idp *authtest.IdP, email string, groups ...string) string {
	t.Helper()
	claims := authtest.Claims(testIssuer, testAudience, "idp-sub-"+email)
	claims["email_verified"] = false
	claims["preferred_username"] = strings.Split(email, "@")[0]
	claims["groups"] = groups
	if email != "" {
		claims["email"] = email
	}
	return idp.Sign(t, claims)
}

// TestTrustedProxyDeploymentAuthOutcomes pins every surface's decision under
// proxyAuthEnv: an OIDC bearer on /v1, the proxy's identity headers on the
// browser UI, and X-Auth-Request-User/-Groups on /mcp.
func TestTrustedProxyDeploymentAuthOutcomes(t *testing.T) {
	idp := authtest.NewIdP(t)
	cfg := loadAuthConfig(t, proxyAuthEnv(idp.JWKSURL))
	stack := newAuthStack(t, cfg)

	// No browser sign-in variable is set: the browser stays on the proxy's
	// headers and no sign-in route exists.
	assert.Equal(t, "trusted-proxy", stack.auth.browserMode)
	assert.IsType(t, &auth.TrustedProxy{}, stack.auth.browser)
	assert.Empty(t, cfg.BrowserAuth)
	// No email-domain allowlist is set: any identity the provider signs is
	// admitted, with or without an email, as below.
	assert.Empty(t, cfg.OIDCAllowedEmailDomains)
	assert.Empty(t, cfg.OIDCHostedDomainClaim)

	t.Run("api principal", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/ask", nil)
		r.Header.Set("Authorization", "Bearer "+accessToken(t, idp, "Alice@example.com", "everyone", "support-team"))
		p, err := stack.auth.api.Authenticate(r)
		require.NoError(t, err)
		assert.Equal(t, &auth.Principal{
			Subject: "idp-sub-Alice@example.com", Email: "Alice@example.com",
			Display: "Alice", Groups: []string{"everyone", "support-team"},
		}, p)
	})

	api := func(method, path, body, token string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return stack.serve(r).Code
	}
	operator := accessToken(t, idp, "op@example.com", "admins", "everyone")
	platform := accessToken(t, idp, "plat@example.com", "platform")
	member := accessToken(t, idp, "member@example.com", "everyone")
	noEmail := accessToken(t, idp, "", "everyone")
	outsider := accessToken(t, idp, "guest@example.com")
	expired := func() string {
		claims := authtest.Claims(testIssuer, testAudience, "x")
		claims["exp"] = 1
		return idp.Sign(t, claims)
	}()
	wrongAudience := idp.Sign(t, authtest.Claims(testIssuer, "another-app", "x"))

	for _, test := range []struct {
		name, method, path, body, token string
		want                            int
	}{
		// Administration has no API: an administrator's bearer finds no
		// route where the admin HTTP API was.
		{"admin: operator", "GET", "/v1/admin/reembed", "", operator, http.StatusNotFound},
		{"admin: operator connectors", "GET", "/v1/admin/connectors", "", operator, http.StatusNotFound},
		// Self-service: everyone plus an email (unverified, as this provider issues it).
		{"self-service: member", "GET", "/v1/me/agent-sessions", "", member, http.StatusServiceUnavailable},
		{"self-service: operator in everyone", "GET", "/v1/me/agent-sessions", "", operator, http.StatusServiceUnavailable},
		{"self-service: platform only", "GET", "/v1/me/agent-sessions", "", platform, http.StatusForbidden},
		{"self-service: no email", "GET", "/v1/me/agent-sessions", "", noEmail, http.StatusForbidden},
		// Use routes: any valid token.
		{"use: outsider", "POST", "/v1/retrieve", `{"question":"q"}`, outsider, http.StatusOK},
		// Authentication failures.
		{"no token", "POST", "/v1/retrieve", `{"question":"q"}`, "", http.StatusUnauthorized},
		{"garbage", "POST", "/v1/retrieve", `{"question":"q"}`, "garbage", http.StatusUnauthorized},
		{"expired", "POST", "/v1/retrieve", `{"question":"q"}`, expired, http.StatusUnauthorized},
		{"wrong audience", "POST", "/v1/retrieve", `{"question":"q"}`, wrongAudience, http.StatusUnauthorized},
	} {
		assert.Equal(t, test.want, api(test.method, test.path, test.body, test.token), test.name)
	}

	t.Run("api scopes", func(t *testing.T) {
		support := accessToken(t, idp, "Support@example.com", "everyone", "support-team")
		stack.scopes.emails = nil
		require.Equal(t, http.StatusOK, api("POST", "/v1/retrieve", `{"question":"q"}`, support))
		assert.Equal(t, []string{"Support@example.com"}, stack.scopes.emails)
		assert.Equal(t, []string{"public", "internal", "slack-channel:CPRIVATE", "corpus:zendesk"}, stack.retriever.scopes)

		require.Equal(t, http.StatusOK, api("POST", "/v1/retrieve", `{"question":"q"}`, member))
		assert.Equal(t, []string{"public", "internal", "slack-channel:CPRIVATE"}, stack.retriever.scopes)

		// A token without an email still reads the baseline.
		stack.scopes.emails = nil
		require.Equal(t, http.StatusOK, api("POST", "/v1/retrieve", `{"question":"q"}`, noEmail))
		assert.Equal(t, []string{""}, stack.scopes.emails)
	})

	browser := func(path, peer, email, groups string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = peer
		if email != "" {
			r.Header.Set("X-Auth-Request-Email", email)
		}
		if groups != "" {
			r.Header.Set("X-Auth-Request-Groups", groups)
		}
		return stack.serve(r)
	}
	for _, test := range []struct {
		name, path, peer, email, groups string
		want                            int
		location                        string
	}{
		{"root: operator", "/", proxyPeer, "op@example.com", "everyone|admins", http.StatusSeeOther, "/admin/"},
		{"root: platform", "/", proxyPeer, "plat@example.com", "platform", http.StatusSeeOther, "/admin/"},
		{"root: member", "/", proxyPeer, "member@example.com", "everyone", http.StatusSeeOther, "/sessions"},
		{"root: no groups", "/", proxyPeer, "guest@example.com", "", http.StatusForbidden, ""},
		{"root: no identity", "/", proxyPeer, "", "", http.StatusUnauthorized, ""},
		{"admin tree: operator", "/admin/missing", proxyPeer, "op@example.com", "admins", http.StatusNotFound, ""},
		{"admin tree: ops", "/admin/missing", proxyPeer, "ops@example.com", "ops", http.StatusNotFound, ""},
		{"admin tree: member", "/admin/missing", proxyPeer, "member@example.com", "everyone", http.StatusForbidden, ""},
		{"sessions: member", "/sessions", proxyPeer, "Member@Example.com", "everyone", http.StatusServiceUnavailable, ""},
		{"sessions: operator only", "/sessions", proxyPeer, "op@example.com", "admins", http.StatusForbidden, ""},
		// Identity headers from anything but the proxy are ignored.
		{"root: forged", "/", outsidePeer, "op@example.com", "admins", http.StatusUnauthorized, ""},
		{"admin tree: forged", "/admin/missing", outsidePeer, "op@example.com", "admins", http.StatusUnauthorized, ""},
		{"sessions: forged", "/sessions", outsidePeer, "member@example.com", "everyone", http.StatusUnauthorized, ""},
		{"admin tree: loopback", "/admin/missing", "127.0.0.1:1", "op@example.com", "admins", http.StatusUnauthorized, ""},
		// No built-in sign-in: the routes do not exist, and an unauthenticated
		// page load is a plain 401 rather than a redirect.
		{"sign-in: anonymous", "/auth/login", proxyPeer, "", "", http.StatusNotFound, ""},
		{"sign-in: operator", "/auth/login", proxyPeer, "op@example.com", "admins", http.StatusNotFound, ""},
		{"callback", "/auth/callback?code=c&state=s", proxyPeer, "", "", http.StatusNotFound, ""},
		{"sign-out", "/auth/logout", proxyPeer, "op@example.com", "admins", http.StatusNotFound, ""},
		{"sessions: no identity", "/sessions", proxyPeer, "", "", http.StatusUnauthorized, ""},
		{"admin tree: no identity", "/admin/missing", proxyPeer, "", "", http.StatusUnauthorized, ""},
	} {
		w := browser(test.path, test.peer, test.email, test.groups)
		assert.Equal(t, test.want, w.Code, test.name)
		assert.Equal(t, test.location, w.Header().Get("Location"), test.name)
	}

	mcp := func(peer, token, user, groups string) (int, []string) {
		stack.mcpScopes = nil
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.RemoteAddr = peer
		r.Header.Set("Authorization", "Bearer "+token)
		if user != "" {
			r.Header.Set("X-Auth-Request-User", user)
		}
		if groups != "" {
			r.Header.Set("X-Auth-Request-Groups", groups)
		}
		return stack.serve(r).Code, stack.mcpScopes
	}
	t.Run("mcp", func(t *testing.T) {
		stack.scopes.emails = nil
		code, scopes := mcp(gatewayPeer, gatewayKey, " Support@Example.com ", "everyone|support-team")
		require.Equal(t, http.StatusNoContent, code)
		assert.Equal(t, []string{"support@example.com"}, stack.scopes.emails)
		assert.Equal(t, []string{"public", "internal", "slack-channel:CPRIVATE", "corpus:zendesk"}, scopes)

		code, scopes = mcp(gatewayPeer, gatewayKey, "member@example.com", "everyone")
		require.Equal(t, http.StatusNoContent, code)
		assert.Equal(t, []string{"public", "internal", "slack-channel:CPRIVATE"}, scopes)

		// Discovery without an identity; forged identity from elsewhere.
		for name, peer := range map[string]string{"no identity": gatewayPeer, "forged": outsidePeer} {
			user := "support@example.com"
			if name == "no identity" {
				user = ""
			}
			code, scopes = mcp(peer, gatewayKey, user, "support-team")
			require.Equal(t, http.StatusNoContent, code, name)
			assert.Equal(t, []string{"mcp:no-verified-identity"}, scopes, name)
		}

		code, _ = mcp(gatewayPeer, "wrong", "support@example.com", "support-team")
		assert.Equal(t, http.StatusUnauthorized, code)
	})
}

// TestUnverifiedEmailProviderNeedsVerificationOptOut pins why a deployment
// whose provider issues email_verified=false must set
// DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED=false: under the default, a token keeps
// its groups but loses its email, so the caller drops to baseline scopes and
// loses self-service.
func TestUnverifiedEmailProviderNeedsVerificationOptOut(t *testing.T) {
	idp := authtest.NewIdP(t)
	env := proxyAuthEnv(idp.JWKSURL)
	delete(env, "DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED")
	stack := newAuthStack(t, loadAuthConfig(t, env))

	r := httptest.NewRequest(http.MethodGet, "/v1/me/agent-sessions", nil)
	r.Header.Set("Authorization", "Bearer "+accessToken(t, idp, "member@example.com", "everyone"))
	assert.Equal(t, http.StatusForbidden, stack.serve(r).Code)

	p, err := stack.auth.api.Authenticate(r)
	require.NoError(t, err)
	assert.Empty(t, p.Email)
	assert.True(t, slices.Contains(p.Groups, "everyone"))
}

// browserOIDCEnv is a proxy-less install: the built-in sign-in for the
// browser, with the trusted-proxy settings left to the gateway's /mcp.
func browserOIDCEnv(idp *authtest.IdP) map[string]string {
	env := proxyAuthEnv(idp.JWKSURL)
	env["DIDEROT_OIDC_ISSUER"] = idp.Issuer
	env["DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED"] = "true"
	env["DIDEROT_BROWSER_AUTH"] = "oidc"
	env["DIDEROT_BROWSER_OIDC_CLIENT_ID"] = "diderot-web"
	env["DIDEROT_BROWSER_OIDC_CLIENT_SECRET"] = "web-secret"
	env["DIDEROT_BROWSER_SESSION_KEYS"] = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	return env
}

// signInThrough runs the browser sign-in against mux and returns the
// session cookie.
func signInThrough(t *testing.T, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := serve(httptest.NewRequest(http.MethodGet, "https://diderot.example.com/auth/login?return_to=/admin/", nil))
	require.Equal(t, http.StatusSeeOther, w.Code)
	signin := w.Result().Cookies()
	require.Len(t, signin, 1)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(w.Header().Get("Location"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	r := httptest.NewRequest(http.MethodGet, resp.Header.Get("Location"), nil)
	r.AddCookie(signin[0])
	w = serve(r)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	require.Equal(t, "/admin/", w.Header().Get("Location"))
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-diderot_session" && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// TestBuildServerAuthBrowserOIDC: with the built-in sign-in, the browser
// has exactly one identity source, and neither credential crosses surfaces.
func TestBuildServerAuthBrowserOIDC(t *testing.T) {
	idp := authtest.NewIdP(t)
	idp.RegisterClient("diderot-web", "web-secret")
	idp.SetUser(map[string]any{
		"sub": "web-1", "email": "op@example.com", "email_verified": true,
		"groups": []string{"admins", "everyone", "support-team"},
	})
	cfg := loadAuthConfig(t, browserOIDCEnv(idp))
	stack := newAuthStack(t, cfg)
	assert.Equal(t, "oidc", stack.auth.browserMode)
	assert.True(t, stack.auth.browserIdentity)
	assert.IsType(t, &auth.BrowserLogin{}, stack.auth.browser)

	browser := func(path string, edit func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = proxyPeer
		edit(r)
		return stack.serve(r)
	}
	// The proxy's headers from a trusted peer no longer identify a browser.
	w := browser("/admin/missing", func(r *http.Request) {
		r.Header.Set("X-Auth-Request-Email", "op@example.com")
		r.Header.Set("X-Auth-Request-Groups", "admins")
	})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/auth/login?return_to=%2Fadmin%2Fmissing", w.Header().Get("Location"))
	// Nor does an API bearer.
	bearer := idp.Sign(t, authtest.Claims(idp.Issuer, testAudience, "op"))
	w = browser("/admin/missing", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+bearer) })
	assert.Equal(t, http.StatusSeeOther, w.Code)

	session := signInThrough(t, stack.mux)
	withSession := func(r *http.Request) { r.AddCookie(session) }
	assert.Equal(t, http.StatusNotFound, browser("/admin/missing", withSession).Code)
	assert.Equal(t, http.StatusServiceUnavailable, browser("/sessions", withSession).Code)

	// The session is not an API or MCP credential.
	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"question":"q"}`))
	r.AddCookie(session)
	assert.Equal(t, http.StatusUnauthorized, stack.serve(r).Code)
	mcp := func(edit func(*http.Request)) []string {
		stack.mcpScopes = nil
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.RemoteAddr = gatewayPeer
		r.Header.Set("Authorization", "Bearer "+gatewayKey)
		edit(r)
		require.Equal(t, http.StatusNoContent, stack.serve(r).Code)
		return stack.mcpScopes
	}
	assert.Equal(t, []string{"mcp:no-verified-identity"}, mcp(withSession))
	// /mcp keeps its trusted-proxy identity.
	assert.Equal(t, []string{"public", "internal", "slack-channel:CPRIVATE", "corpus:zendesk"}, mcp(func(r *http.Request) {
		r.Header.Set("X-Auth-Request-User", "support@example.com")
		r.Header.Set("X-Auth-Request-Groups", "support-team")
	}))
}

// TestAllowedEmailDomainsRefuseOutsiders: with an IdP that signs in any
// account (Google), the domain allowlist refuses outsiders on /v1 and at the
// browser sign-in, instead of admitting them to the baseline corpus without
// an email.
func TestAllowedEmailDomainsRefuseOutsiders(t *testing.T) {
	idp := authtest.NewIdP(t)
	idp.RegisterClient("diderot-web", "web-secret")
	env := browserOIDCEnv(idp)
	env["DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS"] = "example.com"
	env["DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM"] = "hd"
	stack := newAuthStack(t, loadAuthConfig(t, env))

	token := func(email, hd string) string {
		claims := authtest.Claims(idp.Issuer, testAudience, "google-"+email)
		claims["email"], claims["email_verified"] = email, true
		claims["groups"] = []string{"admins", "everyone"}
		if hd != "" {
			claims["hd"] = hd
		}
		return idp.Sign(t, claims)
	}
	call := func(method, path, body, token string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		return stack.serve(r).Code
	}
	insider := token("alice@example.com", "example.com")
	assert.Equal(t, http.StatusOK, call("POST", "/v1/retrieve", `{"question":"q"}`, insider))
	for name, outsider := range map[string]string{
		"consumer account":            token("guest@gmail.com", ""),
		"work address, not Workspace": token("bob@example.com", ""),
		"other Workspace":             token("carol@example.org", "example.org"),
	} {
		assert.Equal(t, http.StatusForbidden, call("POST", "/v1/retrieve", `{"question":"q"}`, outsider), name)
		assert.Equal(t, http.StatusForbidden, call("GET", "/v1/me/agent-sessions", "", outsider), name)
	}

	idp.SetUser(map[string]any{
		"sub": "guest", "email": "guest@gmail.com", "email_verified": true,
		"groups": []string{"admins"},
	})
	w := stack.serve(httptest.NewRequest(http.MethodGet, "https://diderot.example.com/auth/login?return_to=/admin/", nil))
	require.Equal(t, http.StatusSeeOther, w.Code)
	signin := w.Result().Cookies()
	require.Len(t, signin, 1)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(w.Header().Get("Location"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	r := httptest.NewRequest(http.MethodGet, resp.Header.Get("Location"), nil)
	r.AddCookie(signin[0])
	w = stack.serve(r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "outside the allowed domains")
	for _, c := range w.Result().Cookies() {
		assert.False(t, c.Name == "__Host-diderot_session" && c.MaxAge > 0, "session issued to an outsider")
	}
}

func TestBuildServerAuthBrowserOIDCRequiresSecureOrigin(t *testing.T) {
	cfg := loadAuthConfig(t, browserOIDCEnv(authtest.NewIdP(t)))
	cfg.PublicURL = "http://diderot.example.com"
	_, err := buildServerAuth(cfg, []string{":9091", ":9092"}, nil, discardLogger())
	require.ErrorContains(t, err, "https")

	cfg.PublicURL, cfg.BrowserSessionInsecureCookie = "https://diderot.example.com", true
	_, err = buildServerAuth(cfg, []string{":9091", ":9092"}, nil, discardLogger())
	require.ErrorContains(t, err, "loopback")
}

// TestBrowserOIDCInsecureLoopbackReachesUICookies: the development opt-in
// drops Secure on the sign-in cookies and on the UI's own CSRF cookies alike,
// or a browser that refuses Secure cookies over loopback HTTP could sign in
// but never submit a form.
func TestBrowserOIDCInsecureLoopbackReachesUICookies(t *testing.T) {
	idp := authtest.NewIdP(t)
	env := browserOIDCEnv(idp)
	env["DIDEROT_PUBLIC_URL"] = "http://127.0.0.1:9091"
	env["DIDEROT_BROWSER_SESSION_INSECURE_COOKIE"] = "true"
	stack := newAuthStack(t, loadAuthConfig(t, env))
	for _, path := range []string{"/auth/login", "/auth/logout"} {
		w := stack.serve(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9091"+path, nil))
		cookies := w.Result().Cookies()
		require.Len(t, cookies, 1, path)
		assert.False(t, cookies[0].Secure, path)
		assert.False(t, strings.HasPrefix(cookies[0].Name, "__Host-"), path)
	}
}
