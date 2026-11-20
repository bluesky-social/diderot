package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
)

func testSupportPolicy(t *testing.T) *authorization.Authorizer {
	t.Helper()
	policy, err := authorization.New(authorization.Config{Grants: []authorization.Grant{{
		Principals: []authorization.Principal{authorization.GroupPrincipal("support-team")},
		Permissions: []authorization.Permission{
			{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestHTTPAuthorizationUsesVerifiedClaimsOnly(t *testing.T) {
	server := testServerForHelpers(t)
	server.SetAuthorizer(testSupportPolicy(t))
	server.SetScopeResolver(&recordingMCPScopeResolver{})
	for _, allowed := range []bool{false, true, false} {
		claims := claimsWithEmail("alice@example.com")
		if allowed {
			claims.Groups = []string{"support-team"}
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/ask", nil)
		request.Header.Set("X-Auth-Request-Groups", "support-team")
		request = request.WithContext(auth.NewContext(request.Context(), claims))
		scopes := server.authorizationForRequest(request)
		if slices.Contains(scopes, events.ScopeZendesk) != allowed {
			t.Fatalf("allowed=%v scopes=%v", allowed, scopes)
		}
		if !slices.Contains(scopes, "slack-channel:C1") || !slices.Contains(scopes, events.ScopePublic) || !slices.Contains(scopes, events.ScopeInternal) {
			t.Fatalf("lost source-native scopes: %v", scopes)
		}
	}
}

func TestMCPAuthorizationUsesAuthenticatedForwardedGroups(t *testing.T) {
	server := testServerForHelpers(t)
	server.SetAuthorizer(testSupportPolicy(t))
	for _, test := range []struct {
		name, token, user, peer string
		groups                  []string
		allowed                 bool
		status                  int
	}{
		{name: "support", token: "secret", user: "alice@example.com", groups: []string{"everyone|support-team"}, allowed: true, status: http.StatusNoContent},
		{name: "missing groups", token: "secret", user: "alice@example.com", status: http.StatusNoContent},
		{name: "no user", token: "secret", groups: []string{"support-team"}, status: http.StatusNoContent},
		{name: "bad bearer", token: "wrong", user: "alice@example.com", groups: []string{"support-team"}, status: http.StatusUnauthorized},
		{name: "ambiguous headers", token: "secret", user: "alice@example.com", groups: []string{"everyone", "support-team"}, status: http.StatusNoContent},
		{name: "not CSV", token: "secret", user: "alice@example.com", groups: []string{"everyone,support-team"}, status: http.StatusNoContent},
		{name: "untrusted peer", token: "secret", user: "alice@example.com", groups: []string{"everyone|support-team"}, peer: "198.51.100.7:4000", status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := server.mcpAuthed(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				called = true
				scopes := ask.AllowedScopesFromContext(request.Context())
				if slices.Contains(scopes, events.ScopeZendesk) != test.allowed {
					t.Fatalf("unexpected scopes: %v", scopes)
				}
				if test.peer != "" && !slices.Equal(scopes, []string{"mcp:no-verified-identity"}) {
					t.Fatalf("untrusted peer identity honored: %v", scopes)
				}
				writer.WriteHeader(http.StatusNoContent)
			}), "secret", proxyMCPIdentity(), `Bearer realm="diderot"`)
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if test.peer != "" {
				request.RemoteAddr = test.peer
			}
			request.Header.Set("Authorization", "Bearer "+test.token)
			request.Header.Set("X-Auth-Request-User", test.user)
			for _, value := range test.groups {
				request.Header.Add("X-Auth-Request-Groups", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || called != (test.status == http.StatusNoContent) {
				t.Fatalf("status=%d called=%v", response.Code, called)
			}
		})
	}
}

// grantingScopes grants a source's scope to callers holding internal.
type grantingScopes struct{}

func (grantingScopes) GrantScopes(_ context.Context, scopes []string) []string {
	if slices.Contains(scopes, events.ScopeInternal) {
		return append(slices.Clone(scopes), "discord-channel:1")
	}
	return scopes
}

// TestCallerScopesCarryTheSourcesGrants: the API's and MCP's callers hold
// what the sources grant the scopes they have.
func TestCallerScopesCarryTheSourcesGrants(t *testing.T) {
	server := testServerForHelpers(t)
	server.SetAuthorizer(testSupportPolicy(t))
	server.SetScopeResolver(&recordingMCPScopeResolver{})
	server.SetScopeGrants(grantingScopes{})
	request := httptest.NewRequest(http.MethodPost, "/v1/ask", nil)
	request = request.WithContext(auth.NewContext(request.Context(), claimsWithEmail("alice@example.com")))
	if scopes := server.authorizationForRequest(request); !slices.Contains(scopes, "discord-channel:1") {
		t.Fatalf("API scopes lack the grant: %v", scopes)
	}
	called := false
	handler := server.mcpAuthed(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		if scopes := ask.AllowedScopesFromContext(request.Context()); !slices.Contains(scopes, "discord-channel:1") {
			t.Fatalf("MCP scopes lack the grant: %v", scopes)
		}
		writer.WriteHeader(http.StatusNoContent)
	}), "secret", proxyMCPIdentity(), `Bearer realm="diderot"`)
	mcp := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	mcp.Header.Set("Authorization", "Bearer secret")
	mcp.Header.Set("X-Auth-Request-User", "alice@example.com")
	handler.ServeHTTP(httptest.NewRecorder(), mcp)
	if !called {
		t.Fatal("the MCP handler was not reached")
	}
}

// TestScopesForPrincipalNilMatchesEmptyPrincipal: scopesForPrincipal's own
// doc comment describes its result as "the scopes of their email, and
// those their groups are granted" -- a nil *auth.Principal has an empty
// email and no groups, so by that comment it is just a shorter way to
// express the same "no identity" case as an explicit &auth.Principal{}
// (also empty email, no groups), and the two calls should return the same
// scopes.
//
// Trigger: call scopesForPrincipal(ctx, nil) on a server with a
// ScopeGranter configured (SetScopeGrants), versus
// scopesForPrincipal(ctx, &auth.Principal{}) on the same server.
//
// On the current code this fails: the nil branch returns
// s.scopesForEmail(ctx, "") directly and never reaches s.grantScopes, so
// it comes back as just the baseline {public, internal}, missing the
// granter's "discord-channel:1" that the explicit empty principal
// receives.
func TestScopesForPrincipalNilMatchesEmptyPrincipal(t *testing.T) {
	server := testServerForHelpers(t)
	server.SetScopeGrants(grantingScopes{})
	ctx := context.Background()

	withEmpty := server.scopesForPrincipal(ctx, &auth.Principal{})
	withNil := server.scopesForPrincipal(ctx, nil)

	want := []string{events.ScopePublic, events.ScopeInternal, "discord-channel:1"}
	if !sameStringSet(withEmpty, want) {
		t.Fatalf("precondition failed: scopesForPrincipal(&auth.Principal{}) = %v, want %v", withEmpty, want)
	}
	if !sameStringSet(withNil, want) {
		t.Fatalf("scopesForPrincipal(nil) = %v, want %v (same as the empty principal): "+
			"the nil branch must take the same path as an empty principal, including the source grant", withNil, want)
	}
}

// sameStringSet reports whether a and b hold the same strings, ignoring
// order and duplicates.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac, bc := slices.Clone(a), slices.Clone(b)
	slices.Sort(ac)
	slices.Sort(bc)
	return slices.Equal(ac, bc)
}
