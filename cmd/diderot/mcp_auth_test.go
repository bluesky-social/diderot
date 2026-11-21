package main

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
)

// mcpStack is /mcp mounted the way mountCorpusMCP mounts it: the /v1
// authenticator and, when DIDEROT_GATEWAY_API_KEY is set, the gateway path,
// both from cfg alone.
type mcpStack struct {
	mux    *http.ServeMux
	scopes *recordingScopes
	// reached and callerScopes record what the MCP server behind the
	// authentication saw.
	reached      bool
	callerScopes []string
	// challenge is the 401 challenge without its error: the cases' expected
	// `Bearer realm="diderot"` stands for it, so one case list serves the
	// modes that publish resource metadata and those that do not.
	challenge string
}

func newMCPStack(t *testing.T, cfg *config.Config, listenAddrs []string) *mcpStack {
	t.Helper()
	sa, err := buildServerAuth(cfg, listenAddrs, nil, discardLogger())
	require.NoError(t, err)
	authorizer, err := authorization.Parse([]byte(supportPolicy))
	require.NoError(t, err)
	stack := &mcpStack{mux: http.NewServeMux(), scopes: &recordingScopes{}, challenge: missingBearer}
	resource := mcpResourceMetadata(cfg)
	if resource != nil {
		stack.challenge = oidcChallenge
	}
	admin := adminapi.NewServer(sa.api, discardLogger())
	admin.SetAuthorizer(authorizer)
	admin.SetAdminGroups(cfg.AdminGroups)
	admin.SetScopeResolver(stack.scopes)
	require.NoError(t, admin.MountMCP(stack.mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack.reached = true
		stack.callerScopes = ask.AllowedScopesFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), cfg.GatewayAPIKey, sa.mcp, resource))
	return stack
}

// mcpAuthCase is one request to /mcp and the decision it must get.
type mcpAuthCase struct {
	name string
	// peer is the connecting address; empty is outsidePeer.
	peer string
	// authorization is the whole Authorization header; empty sends none.
	authorization string
	// user and groups are the gateway's identity headers, X-Auth-Request-User and
	// X-Auth-Request-Groups, sent when non-empty. X-Auth-Request-Email, the browser's
	// header, is sent with the same value.
	user, groups string

	wantCode int
	// wantEmail is the identity tools run as; empty with a 204 means the
	// request may only discover.
	wantEmail string
	// unverifiedEmail marks a caller admitted by an OIDC verification opt-out.
	unverifiedEmail bool
	// wantZendesk says the caller's groups grant the policy's corpus scope.
	wantZendesk bool
	// wantChallenge is the WWW-Authenticate header of a 401.
	wantChallenge string
}

const (
	missingBearer = `Bearer realm="diderot"`
	invalidBearer = `Bearer realm="diderot", error="invalid_token"`
	// oidcChallenge is the OIDC-mode challenge for serveEnv's public URL and
	// the default scopes: where the resource metadata is, and what to ask
	// the issuer for.
	oidcChallenge = `Bearer realm="diderot", resource_metadata="https://diderot.example.com/.well-known/oauth-protected-resource/mcp", scope="openid email profile"`
)

func runMCPAuthCases(t *testing.T, stack *mcpStack, cases []mcpAuthCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stack.reached, stack.callerScopes, stack.scopes.emails = false, nil, nil
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.RemoteAddr = outsidePeer
			if c.peer != "" {
				r.RemoteAddr = c.peer
			}
			if c.authorization != "" {
				r.Header.Set("Authorization", c.authorization)
			}
			if c.user != "" {
				r.Header.Set("X-Auth-Request-User", c.user)
				r.Header.Set("X-Auth-Request-Email", c.user)
			}
			if c.groups != "" {
				r.Header.Set("X-Auth-Request-Groups", c.groups)
			}
			w := httptest.NewRecorder()
			stack.mux.ServeHTTP(w, r)

			require.Equal(t, c.wantCode, w.Code)
			assert.Equal(t, strings.Replace(c.wantChallenge, missingBearer, stack.challenge, 1), w.Header().Get("WWW-Authenticate"))
			if c.wantCode != http.StatusNoContent {
				assert.False(t, stack.reached, "a refused request must not reach the MCP server")
				assert.Empty(t, stack.scopes.emails, "a refused request resolves no identity")
				return
			}
			require.True(t, stack.reached)
			if c.wantEmail == "" {
				assert.Empty(t, stack.scopes.emails)
				assert.Equal(t, []string{"mcp:no-verified-identity"}, stack.callerScopes)
				return
			}
			assert.Equal(t, []string{c.wantEmail}, stack.scopes.emails)
			want := []string{"public", "internal", "slack-channel:CPRIVATE"}
			if !c.unverifiedEmail {
				want = append(want, "owner:"+c.wantEmail)
			}
			if c.wantZendesk {
				want = append(want, "corpus:zendesk")
			}
			assert.Equal(t, want, stack.callerScopes)
		})
	}
}

// gatewayHeaderEnv is the trusted-proxy configuration of a deployment with a
// gateway in front of /mcp.
var gatewayHeaderEnv = map[string]string{
	"DIDEROT_TRUSTED_PROXY_CIDRS":            "10.0.128.0/20,10.0.144.0/20,10.0.160.0/20",
	"DIDEROT_TRUSTED_PROXY_EMAIL_HEADER":     "X-Auth-Request-Email",
	"DIDEROT_TRUSTED_PROXY_GROUPS_HEADER":    "X-Auth-Request-Groups",
	"DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER": "|",
	"DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER": "X-Auth-Request-User",
}

func staticTokensFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `tokens:
  - sha256: %x
    email: operator@example.com
    groups: [admins]
  - sha256: %x
    email: support@example.com
    groups: [support-team]
`, sha256.Sum256([]byte("operator-token")), sha256.Sum256([]byte("support-token"))), 0o600))
	return path
}

// gatewayPathCases are the gateway path's decisions, the same in every mode
// that configures a gateway key and trusted proxy.
func gatewayPathCases() []mcpAuthCase {
	key := "Bearer " + gatewayKey
	return []mcpAuthCase{
		{name: "gateway key, identity from the gateway", peer: gatewayPeer, authorization: key,
			user: " Support@Example.com ", groups: "everyone|support-team",
			wantCode: http.StatusNoContent, wantEmail: "support@example.com", wantZendesk: true},
		{name: "gateway key, no identity: discovery only", peer: gatewayPeer, authorization: key,
			wantCode: http.StatusNoContent},
		{name: "gateway key, identity forged from an untrusted peer", peer: outsidePeer, authorization: key,
			user: "support@example.com", groups: "support-team", wantCode: http.StatusNoContent},
		{name: "identity headers from the gateway without its key", peer: gatewayPeer,
			user: "support@example.com", groups: "support-team",
			wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "gateway key in another scheme", peer: gatewayPeer, authorization: "Basic " + gatewayKey,
			user: "support@example.com", wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
	}
}

// TestMCPAuthStaticMode: a static-mode install with a gateway in front. Its
// API tokens authenticate /mcp as their mapped identities, and the gateway
// path keeps working beside them.
func TestMCPAuthStaticMode(t *testing.T) {
	env := maps.Clone(gatewayHeaderEnv)
	env["DIDEROT_AUTH_MODE"], env["DIDEROT_AUTH_STATIC_TOKENS_FILE"] = "static", staticTokensFile(t)
	t.Setenv("DIDEROT_GATEWAY_API_KEY", gatewayKey)
	stack := newMCPStack(t, loadAuthConfig(t, env), []string{":9091", ":9092"})

	runMCPAuthCases(t, stack, append(gatewayPathCases(), []mcpAuthCase{
		{name: "no credential", wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "unknown token", authorization: "Bearer nope", wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "token digest instead of token", authorization: fmt.Sprintf("Bearer %x", sha256.Sum256([]byte("operator-token"))),
			wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "static token", authorization: "Bearer operator-token",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com"},
		{name: "static token with groups", authorization: "Bearer support-token",
			wantCode: http.StatusNoContent, wantEmail: "support@example.com", wantZendesk: true},
		{name: "static token, header naming another user", authorization: "Bearer operator-token",
			user: "support@example.com", groups: "support-team",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com"},
		{name: "static token, header from the trusted gateway peer", peer: gatewayPeer, authorization: "Bearer operator-token",
			user: "support@example.com", groups: "support-team",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com"},
		{name: "static token in another scheme", authorization: "Basic operator-token",
			wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
	}...))
}

// TestMCPAuthStaticModeWithoutGateway is the Compose quickstart: static
// tokens, no gateway key, no trusted proxy. There is no gateway path at all.
func TestMCPAuthStaticModeWithoutGateway(t *testing.T) {
	t.Setenv("DIDEROT_GATEWAY_API_KEY", "")
	cfg := loadAuthConfig(t, map[string]string{
		"DIDEROT_AUTH_MODE": "static", "DIDEROT_AUTH_STATIC_TOKENS_FILE": staticTokensFile(t),
	})
	require.Empty(t, cfg.GatewayAPIKey)
	stack := newMCPStack(t, cfg, []string{":9091", ":9092"})

	runMCPAuthCases(t, stack, []mcpAuthCase{
		{name: "no credential", wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "empty bearer", authorization: "Bearer ", wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "no credential, identity headers", peer: gatewayPeer, user: "operator@example.com", groups: "admins",
			wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "another deployment's gateway key", peer: gatewayPeer, authorization: "Bearer " + gatewayKey,
			user: "operator@example.com", wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "static token", authorization: "Bearer operator-token",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com"},
		{name: "static token, header naming another user", authorization: "Bearer operator-token",
			user: "support@example.com", groups: "support-team",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com"},
	})
}

// TestMCPAuthOIDCMode is a common shape: OIDC access tokens on /v1, a gateway
// with its key in front of /mcp. A verified access token authenticates /mcp
// as its own principal; the gateway path is unchanged.
func TestMCPAuthOIDCMode(t *testing.T) {
	idp := authtest.NewIdP(t)
	t.Setenv("DIDEROT_GATEWAY_API_KEY", gatewayKey)
	stack := newMCPStack(t, loadAuthConfig(t, proxyAuthEnv(idp.JWKSURL)), []string{":9091", ":9092"})

	expired := authtest.Claims(testIssuer, testAudience, "idp-sub-old")
	expired["email"], expired["exp"] = "operator@example.com", time.Now().Add(-time.Hour).Unix()
	otherAudience := authtest.Claims(testIssuer, "another-client", "idp-sub-x")
	otherAudience["email"] = "operator@example.com"
	forged := authtest.NewIdP(t).Sign(t, authtest.Claims(testIssuer, testAudience, "idp-sub-forged"))

	runMCPAuthCases(t, stack, append(gatewayPathCases(), []mcpAuthCase{
		{name: "no credential", wantCode: http.StatusUnauthorized, wantChallenge: missingBearer},
		{name: "not a token", authorization: "Bearer nope", wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "expired token", authorization: "Bearer " + idp.Sign(t, expired),
			wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "token for another audience", authorization: "Bearer " + idp.Sign(t, otherAudience),
			wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "token signed by another issuer's key", authorization: "Bearer " + forged,
			user: "operator@example.com", wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
		{name: "access token", authorization: "Bearer " + accessToken(t, idp, "operator@example.com", "admins"),
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com", unverifiedEmail: true},
		{name: "access token with groups", authorization: "Bearer " + accessToken(t, idp, "Support@Example.com", "support-team"),
			wantCode: http.StatusNoContent, wantEmail: "support@example.com", wantZendesk: true, unverifiedEmail: true},
		{name: "access token, header naming another user", authorization: "Bearer " + accessToken(t, idp, "operator@example.com"),
			user: "support@example.com", groups: "support-team",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com", unverifiedEmail: true},
		{name: "access token, header from the trusted gateway peer", peer: gatewayPeer,
			authorization: "Bearer " + accessToken(t, idp, "operator@example.com"),
			user:          "support@example.com", groups: "support-team",
			wantCode: http.StatusNoContent, wantEmail: "operator@example.com", unverifiedEmail: true},
		// A token without an email authenticates but names nobody: it can
		// discover, and every tool refuses it.
		{name: "access token without an email", authorization: "Bearer " + accessToken(t, idp, "", "support-team"),
			user: "support@example.com", wantCode: http.StatusNoContent},
		{name: "a static-mode token", authorization: "Bearer operator-token",
			wantCode: http.StatusUnauthorized, wantChallenge: invalidBearer},
	}...))
}

// TestMCPAuthOIDCModeRefusesOutsideDomains: an identity the deployment does
// not admit is 403 on /mcp as on /v1, with no challenge, since signing in
// again cannot help.
func TestMCPAuthOIDCModeRefusesOutsideDomains(t *testing.T) {
	idp := authtest.NewIdP(t)
	env := proxyAuthEnv(idp.JWKSURL)
	env["DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED"] = "true"
	env["DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS"] = "example.com"
	t.Setenv("DIDEROT_GATEWAY_API_KEY", gatewayKey)
	stack := newMCPStack(t, loadAuthConfig(t, env), []string{":9091", ":9092"})

	token := func(email string) string {
		claims := authtest.Claims(testIssuer, testAudience, "sub-"+email)
		claims["email"], claims["email_verified"] = email, true
		return "Bearer " + idp.Sign(t, claims)
	}
	runMCPAuthCases(t, stack, []mcpAuthCase{
		{name: "allowed domain", authorization: token("alice@example.com"),
			wantCode: http.StatusNoContent, wantEmail: "alice@example.com"},
		{name: "outside domain", authorization: token("guest@example.org"), wantCode: http.StatusForbidden},
		{name: "outside domain, header naming an insider", peer: gatewayPeer, authorization: token("guest@example.org"),
			user: "alice@example.com", wantCode: http.StatusForbidden},
	})
}

// TestMCPAuthNoneMode: a none-mode install is one administrator on loopback.
// /mcp answers as that principal whatever the request carries, as /v1 does,
// with or without a gateway key configured.
func TestMCPAuthNoneMode(t *testing.T) {
	for name, key := range map[string]string{"no gateway key": "", "gateway key": gatewayKey} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DIDEROT_GATEWAY_API_KEY", key)
			cfg := loadAuthConfig(t, map[string]string{
				"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "admin@example.com",
			})
			stack := newMCPStack(t, cfg, []string{"127.0.0.1:9091", "127.0.0.1:9092"})

			runMCPAuthCases(t, stack, []mcpAuthCase{
				{name: "no credential", peer: "127.0.0.1:50000",
					wantCode: http.StatusNoContent, wantEmail: "admin@example.com"},
				{name: "any bearer", peer: "127.0.0.1:50000", authorization: "Bearer anything",
					wantCode: http.StatusNoContent, wantEmail: "admin@example.com"},
				{name: "the gateway key", peer: "127.0.0.1:50000", authorization: "Bearer " + gatewayKey,
					wantCode: http.StatusNoContent, wantEmail: "admin@example.com"},
				{name: "header naming another user", peer: "127.0.0.1:50000", authorization: "Bearer " + gatewayKey,
					user: "support@example.com", groups: "support-team",
					wantCode: http.StatusNoContent, wantEmail: "admin@example.com"},
			})
		})
	}
}

// TestMCPResourceMetadata: in oidc mode /mcp publishes its protected
// resource metadata (RFC 9728) at the well-known path the challenge names,
// pointing MCP clients at the issuer and the scopes that carry the identity
// claims. Static and none modes take no OAuth token and publish nothing.
func TestMCPResourceMetadata(t *testing.T) {
	get := func(stack *mcpStack) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
		w := httptest.NewRecorder()
		stack.mux.ServeHTTP(w, r)
		return w
	}

	t.Run("oidc", func(t *testing.T) {
		idp := authtest.NewIdP(t)
		env := proxyAuthEnv(idp.JWKSURL)
		env["DIDEROT_OIDC_SCOPES"] = "openid,email profile groups"
		stack := newMCPStack(t, loadAuthConfig(t, env), []string{":9091", ":9092"})
		w := get(stack)
		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `{
			"resource": "https://diderot.example.com/mcp",
			"authorization_servers": ["`+testIssuer+`"],
			"scopes_supported": ["openid", "email", "profile", "groups"],
			"bearer_methods_supported": ["header"],
			"resource_name": "Diderot"
		}`, w.Body.String())
		const metadata = `resource_metadata="https://diderot.example.com/.well-known/oauth-protected-resource/mcp"`
		assert.Equal(t, `Bearer realm="diderot", `+metadata+`, scope="openid email profile groups"`, stack.challengeFor(t, ""))
		assert.Equal(t, `Bearer realm="diderot", `+metadata+`, scope="openid email profile groups", error="invalid_token"`,
			stack.challengeFor(t, "Bearer nope"))
	})

	t.Run("static", func(t *testing.T) {
		cfg := loadAuthConfig(t, map[string]string{
			"DIDEROT_AUTH_MODE": "static", "DIDEROT_AUTH_STATIC_TOKENS_FILE": staticTokensFile(t),
		})
		assert.Nil(t, mcpResourceMetadata(cfg))
		stack := newMCPStack(t, cfg, []string{":9091", ":9092"})
		assert.Equal(t, http.StatusNotFound, get(stack).Code)
		assert.Equal(t, missingBearer, stack.challengeFor(t, ""))
	})

	t.Run("none", func(t *testing.T) {
		cfg := loadAuthConfig(t, map[string]string{"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "admin@example.com"})
		assert.Nil(t, mcpResourceMetadata(cfg))
	})
}

// challengeFor is the WWW-Authenticate of a refused /mcp request carrying
// authorization (none when empty).
func (s *mcpStack) challengeFor(t *testing.T, authorization string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = outsidePeer
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	return w.Header().Get("WWW-Authenticate")
}
