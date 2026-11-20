package adminapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
)

// MountMCP mounts an MCP transport at /mcp. A request authenticates one of
// two ways, chosen by its bearer alone:
//
//   - As /v1 does: the bearer the configured auth mode accepts (a static API
//     token, an OIDC access token; none mode's fixed principal needs none),
//     verified by the same authenticator, names the caller. No header can
//     add to or replace that identity.
//   - As a gateway's backend, when gatewayToken is set: the gateway
//     replaces Authorization with gatewayToken, and gatewayIdentity names the
//     human behind the call (the caller email and groups the gateway
//     forwards from its authenticating edge, honored only from trusted proxy
//     addresses). This is the only path that reads identity
//     headers. The gateway credential alone may complete MCP discovery but
//     cannot read even public/internal content: tool execution needs the
//     identity.
//
// Anything else is answered 401 with a Bearer challenge.
//
// resource, when set, is the endpoint's OAuth 2.0 protected resource
// metadata (RFC 9728), as MCP authorization requires of a server that takes
// OAuth access tokens: it is served at the well-known path for /mcp, and
// the 401 challenge points to it and names its scopes, so an MCP client can
// discover where to sign in. It changes only what a refused request is
// told, never who is accepted.
func (s *Server) MountMCP(mux *http.ServeMux, handler http.Handler, gatewayToken string, gatewayIdentity auth.Authenticator, resource *oauthex.ProtectedResourceMetadata) error {
	if mux == nil || handler == nil {
		return errors.New("adminapi: MCP mux and handler are required")
	}
	if s.authenticator == nil {
		return errors.New("adminapi: an authenticator is required to mount MCP")
	}
	gatewayToken = strings.TrimSpace(gatewayToken)
	if gatewayToken != "" && gatewayIdentity == nil {
		return errors.New("adminapi: MCP gateway caller identity authenticator is required")
	}

	challenge := `Bearer realm="diderot"`
	if resource != nil {
		metadataURL, err := ResourceMetadataURL(resource.Resource)
		if err != nil {
			return err
		}
		mux.Handle(resourceMetadataPath, mcpauth.ProtectedResourceMetadataHandler(resource))
		challenge += `, resource_metadata="` + metadataURL + `"`
		if len(resource.ScopesSupported) > 0 {
			challenge += `, scope="` + strings.Join(resource.ScopesSupported, " ") + `"`
		}
	}
	mux.Handle("/mcp", s.mcpAuthed(handler, gatewayToken, gatewayIdentity, challenge))
	return nil
}

// resourceMetadataPath is where /mcp's protected resource metadata is
// served: the RFC 9728 well-known path with the endpoint's path inserted,
// the first place MCP clients look.
const resourceMetadataPath = "/.well-known/oauth-protected-resource/mcp"

// ResourceMetadataURL is the metadata URL for the MCP endpoint at resource.
func ResourceMetadataURL(resource string) (string, error) {
	u, err := url.Parse(resource)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "/mcp" {
		return "", errors.New("adminapi: the MCP resource must be an absolute URL ending in /mcp")
	}
	u.Path = resourceMetadataPath
	return u.String(), nil
}

func (s *Server) mcpAuthed(next http.Handler, gatewayToken string, gatewayIdentity auth.Authenticator, baseChallenge string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var caller *auth.Principal
		// An unset gateway token matches nothing: without the guard an
		// absent bearer would equal it.
		if presented := auth.BearerToken(r); gatewayToken != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(gatewayToken)) == 1 {
			if identified, err := gatewayIdentity.Authenticate(r); err == nil {
				caller = identified
			}
		} else {
			principal, code, msg := s.verifyOperator(r)
			if principal == nil {
				if code == http.StatusUnauthorized {
					challenge := baseChallenge
					if presented != "" {
						challenge += `, error="invalid_token"`
					}
					w.Header().Set("WWW-Authenticate", challenge)
				}
				http.Error(w, msg, code)
				return
			}
			caller = principal
		}

		var email string
		var scopes []string
		admin := false
		if caller != nil {
			email = strings.ToLower(strings.TrimSpace(caller.Email))
			if email != "" {
				scopes = s.scopesForEmail(r.Context(), email)
				decision := s.authorizationForGroups(r.Context(), caller.Groups)
				scopes = s.grantScopes(r.Context(), append(append([]string(nil), scopes...), decision.CorpusScopes()...))
				admin = s.principalIsAdmin(caller)
			}
		}
		ctx := ask.WithMCPCaller(r.Context(), email, scopes)
		// The setup gate applies to the ask tool alone; the corpus tools
		// stay open.
		if !s.askAllowed(admin) {
			ctx = ask.WithMCPAskBlocked(ctx, settings.SetupNotice)
		}
		s.logger.Info("MCP request", "email", email, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) scopesForEmail(ctx context.Context, email string) []string {
	baseline := []string{events.ScopePublic, events.ScopeInternal}
	resolver := s.getScopeResolver()
	if resolver == nil {
		return baseline
	}
	scopes := resolver.ScopesForEmail(ctx, email)
	if len(scopes) == 0 {
		return baseline
	}
	return scopes
}
