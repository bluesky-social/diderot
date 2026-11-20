package adminapi

import (
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/bluesky-social/diderot/internal/auth"
)

// fakeVerifier is a test double for auth.Authenticator: it accepts a single
// known opaque bearer and maps it to a fixed principal, rejecting everything
// else. This lets the request-level admin tests keep presenting a plain
// "Authorization: Bearer <token>" without minting real signed JWTs — the
// real OIDC verifier is exercised in internal/auth's own round-trip tests.
type fakeVerifier struct {
	validToken string
	claims     *auth.Principal
}

func (f fakeVerifier) Authenticate(r *http.Request) (*auth.Principal, error) {
	switch auth.BearerToken(r) {
	case "":
		return nil, auth.ErrNoCredentials
	case f.validToken:
		return f.claims, nil
	}
	return nil, auth.ErrInvalidCredentials
}

// newTestServer builds a Server whose operator auth accepts exactly
// validToken.
func newTestServer(validToken string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	v := fakeVerifier{
		validToken: validToken,
		claims: &auth.Principal{
			Subject: "test-operator",
			Email:   "operator@example.com",
			Groups:  []string{"admins"},
		},
	}
	s := NewServer(v, logger)
	s.SetAdminGroups([]string{"admins"})
	return s
}

// noProxyIdentity is the MCP caller authenticator with no trusted proxy:
// identity headers are ignored.
func noProxyIdentity() auth.Authenticator {
	p, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{})
	if err != nil {
		panic(err)
	}
	return p
}

// proxyMCPIdentity trusts httptest's default RemoteAddr (192.0.2.1) with
// a gateway's /mcp identity header names.
func proxyMCPIdentity() auth.Authenticator {
	p, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs:       []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		EmailHeader: "X-Auth-Request-User", GroupsHeader: "X-Auth-Request-Groups", GroupsDelimiter: "|",
	})
	if err != nil {
		panic(err)
	}
	return p
}
