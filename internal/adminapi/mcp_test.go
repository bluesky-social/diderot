package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
)

type recordingMCPScopeResolver struct {
	email string
}

func (r *recordingMCPScopeResolver) ScopesForEmail(_ context.Context, email string) []string {
	r.email = email
	return []string{"public", "internal", "slack-channel:C1"}
}

// mcpAuthFixture is /mcp mounted on a test server whose /v1 authenticator
// accepts the bearer "secret-token" as operator@example.com.
type mcpAuthFixture struct {
	mux      *http.ServeMux
	resolver *recordingMCPScopeResolver
	called   bool
	scopes   []string
}

func newMCPAuthFixture(t *testing.T, gatewayToken string, gatewayIdentity auth.Authenticator) *mcpAuthFixture {
	t.Helper()
	f := &mcpAuthFixture{mux: http.NewServeMux(), resolver: &recordingMCPScopeResolver{}}
	srv := testServerForHelpers(t)
	srv.SetScopeResolver(f.resolver)
	require.NoError(t, srv.MountMCP(f.mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.called = true
		f.scopes = ask.AllowedScopesFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), gatewayToken, gatewayIdentity, nil))
	return f
}

func (f *mcpAuthFixture) serve(edit func(*http.Request)) *httptest.ResponseRecorder {
	f.called, f.scopes, f.resolver.email = false, nil, ""
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	edit(req)
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestMCPBackendAuthenticationAndIdentity(t *testing.T) {
	f := newMCPAuthFixture(t, "gateway-secret", proxyMCPIdentity())

	for authorization, challenge := range map[string]string{
		"":             `Bearer realm="diderot"`,
		"Bearer wrong": `Bearer realm="diderot", error="invalid_token"`,
		// Another scheme carries no bearer at all.
		"Basic Z2F0ZXdheS1zZWNyZXQ6": `Bearer realm="diderot"`,
	} {
		rr := f.serve(func(r *http.Request) {
			if authorization != "" {
				r.Header.Set("Authorization", authorization)
			}
			// An identity header never stands in for a credential.
			r.Header.Set("X-Auth-Request-User", "alice@example.com")
		})
		require.Equal(t, http.StatusUnauthorized, rr.Code, authorization)
		require.Equal(t, challenge, rr.Header().Get("WWW-Authenticate"), authorization)
		require.False(t, f.called, authorization)
		require.Empty(t, f.resolver.email, authorization)
	}

	rr := f.serve(bearer("gateway-secret"))
	require.Equal(t, http.StatusNoContent, rr.Code)
	require.True(t, f.called)
	require.Empty(t, f.resolver.email)

	rr = f.serve(func(r *http.Request) {
		bearer("gateway-secret")(r)
		r.Header.Set("X-Auth-Request-User", " ALICE@EXAMPLE.COM ")
	})
	require.Equal(t, http.StatusNoContent, rr.Code)
	require.True(t, f.called)
	require.Equal(t, "alice@example.com", f.resolver.email)
	require.Contains(t, f.scopes, "owner:alice@example.com")
}

// TestMCPIgnoresIdentityHeadersWithoutTrustedProxy pins that the gateway
// bearer alone never lets a caller assert an identity: without a trusted
// proxy (or from a peer outside it), X-Auth-Request-User is ignored.
func TestMCPIgnoresIdentityHeadersWithoutTrustedProxy(t *testing.T) {
	for name, identity := range map[string]auth.Authenticator{
		"no trusted proxy":  noProxyIdentity(),
		"untrusted peer":    proxyMCPIdentity(),
		"trusted, no email": proxyMCPIdentity(),
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPAuthFixture(t, "gateway-secret", identity)
			rr := f.serve(func(r *http.Request) {
				bearer("gateway-secret")(r)
				if name != "trusted, no email" {
					r.Header.Set("X-Auth-Request-User", "alice@example.com")
				}
				r.Header.Set("X-Auth-Request-Groups", "everyone|support-team")
				if name == "untrusted peer" {
					r.RemoteAddr = "198.51.100.7:4000"
				}
			})
			require.Equal(t, http.StatusNoContent, rr.Code)
			require.Empty(t, f.resolver.email)
			require.Equal(t, []string{"mcp:no-verified-identity"}, f.scopes)
		})
	}
}

// TestMCPAcceptsTheAPIBearerAsItsOwnIdentity: the bearer /v1 accepts
// authenticates /mcp as that bearer's principal, with or without a gateway
// configured, and an identity header beside it changes nothing, even from
// the trusted gateway's own address.
func TestMCPAcceptsTheAPIBearerAsItsOwnIdentity(t *testing.T) {
	for name, f := range map[string]*mcpAuthFixture{
		"no gateway":                 newMCPAuthFixture(t, "", nil),
		"gateway with trusted proxy": newMCPAuthFixture(t, "gateway-secret", proxyMCPIdentity()),
	} {
		t.Run(name, func(t *testing.T) {
			rr := f.serve(bearer("secret-token"))
			require.Equal(t, http.StatusNoContent, rr.Code)
			assert.Equal(t, "operator@example.com", f.resolver.email)
			assert.Equal(t, []string{"public", "internal", "slack-channel:C1"}, f.scopes, "the test principal has an unverified email")

			// httptest's default peer is inside proxyMCPIdentity's CIDR.
			rr = f.serve(func(r *http.Request) {
				bearer("secret-token")(r)
				r.Header.Set("X-Auth-Request-User", "victim@example.com")
				r.Header.Set("X-Auth-Request-Email", "victim@example.com")
				r.Header.Set("X-Auth-Request-Groups", "admins|support-team")
			})
			require.Equal(t, http.StatusNoContent, rr.Code)
			assert.Equal(t, "operator@example.com", f.resolver.email, "a header must never replace the bearer's identity")
		})
	}
}

// TestMCPWithoutGatewayHasNoGatewayPath: with no gateway token configured,
// nothing can take the gateway path. An absent bearer in particular must not
// compare equal to the unset token.
func TestMCPWithoutGatewayHasNoGatewayPath(t *testing.T) {
	// The identity authenticator is not consulted without a token, even one
	// that would trust the peer.
	for name, identity := range map[string]auth.Authenticator{"nil": nil, "trusting": proxyMCPIdentity()} {
		f := newMCPAuthFixture(t, "", identity)
		for _, authorization := range []string{"", "Bearer", "Bearer ", "Bearer gateway-secret"} {
			rr := f.serve(func(r *http.Request) {
				if authorization != "" {
					r.Header.Set("Authorization", authorization)
				}
				r.Header.Set("X-Auth-Request-User", "alice@example.com")
			})
			require.Equal(t, http.StatusUnauthorized, rr.Code, "%s identity, Authorization %q", name, authorization)
			require.NotEmpty(t, rr.Header().Get("WWW-Authenticate"))
			require.False(t, f.called)
			require.Empty(t, f.resolver.email)
		}
	}
}

func TestMCPRefusesAnIdentityTheDeploymentDoesNotAdmit(t *testing.T) {
	srv := NewServer(refusingAuthenticator{}, nil)
	mux := http.NewServeMux()
	require.NoError(t, srv.MountMCP(mux, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a refused identity must not reach the MCP server")
	}), "", nil, nil))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	bearer("valid-but-outside-the-domain")(req)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Empty(t, rr.Header().Get("WWW-Authenticate"), "signing in again cannot help, so there is no challenge")
}

type refusingAuthenticator struct{}

func (refusingAuthenticator) Authenticate(*http.Request) (*auth.Principal, error) {
	return nil, auth.ErrIdentityNotAllowed
}

func TestMountMCPRequiresIdentityAuthenticatorWithGatewayToken(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	err := testServerForHelpers(t).MountMCP(http.NewServeMux(), handler, "secret", nil, nil)
	require.ErrorContains(t, err, "identity authenticator is required")
	require.NoError(t, testServerForHelpers(t).MountMCP(http.NewServeMux(), handler, "", nil, nil))
}

func TestMountMCPRequiresAnAPIAuthenticator(t *testing.T) {
	err := NewServer(nil, nil).MountMCP(http.NewServeMux(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "", nil, nil)
	require.ErrorContains(t, err, "authenticator is required")
}

// The metadata is served for /mcp alone; a resource elsewhere would name a
// metadata URL clients never look at.
func TestMountMCPResourceMustBeTheMCPEndpoint(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, resource := range []string{"https://diderot.example.com", "https://diderot.example.com/other", "/mcp"} {
		err := testServerForHelpers(t).MountMCP(http.NewServeMux(), handler, "", nil,
			&oauthex.ProtectedResourceMetadata{Resource: resource, AuthorizationServers: []string{"https://idp.example.com"}})
		assert.ErrorContains(t, err, "ending in /mcp", resource)
	}
	metadataURL, err := ResourceMetadataURL("https://diderot.example.com/mcp")
	require.NoError(t, err)
	assert.Equal(t, "https://diderot.example.com/.well-known/oauth-protected-resource/mcp", metadataURL)
}
