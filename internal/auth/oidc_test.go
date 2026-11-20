package auth_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/auth/authtest"
)

const audience = "diderot-client"

func bearerRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/ask", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func newOIDC(t *testing.T, cfg auth.OIDCConfig) *auth.OIDC {
	t.Helper()
	a, err := auth.NewOIDC(cfg)
	require.NoError(t, err)
	return a
}

func TestOIDCVerifiesSignatureIssuerAudienceAndExpiry(t *testing.T) {
	idp := authtest.NewIdP(t)
	other := authtest.NewIdP(t)
	a := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL})

	_, err := a.Authenticate(bearerRequest(""))
	require.ErrorIs(t, err, auth.ErrNoCredentials)

	valid := authtest.Claims(idp.Issuer, audience, "user-1")
	p, err := a.Authenticate(bearerRequest(idp.Sign(t, valid)))
	require.NoError(t, err)
	assert.Equal(t, "user-1", p.Subject)

	expired := authtest.Claims(idp.Issuer, audience, "user-1")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	wrongAudience := authtest.Claims(idp.Issuer, "someone-else", "user-1")
	wrongIssuer := authtest.Claims("https://evil.example", audience, "user-1")
	for name, token := range map[string]string{
		"garbage":        "not-a-jwt",
		"expired":        idp.Sign(t, expired),
		"wrong audience": idp.Sign(t, wrongAudience),
		"wrong issuer":   idp.Sign(t, wrongIssuer),
		"foreign key":    other.Sign(t, valid),
	} {
		_, err := a.Authenticate(bearerRequest(token))
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, name)
	}
}

func TestOIDCEmailVerification(t *testing.T) {
	idp := authtest.NewIdP(t)
	token := func(verified any) string {
		claims := authtest.Claims(idp.Issuer, audience, "user-1")
		claims["email"] = "Alice@Example.com"
		claims["preferred_username"] = "alice"
		claims["groups"] = []string{"everyone"}
		if verified != nil {
			claims["email_verified"] = verified
		}
		return idp.Sign(t, claims)
	}
	required := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL, RequireEmailVerified: true})
	optional := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL})

	for _, test := range []struct {
		name         string
		verified     any
		wantRequired string
		wantVerified bool
	}{
		{name: "verified", verified: true, wantRequired: "Alice@Example.com", wantVerified: true},
		{name: "verified string", verified: "true", wantRequired: "Alice@Example.com", wantVerified: true},
		{name: "unverified", verified: false},
		{name: "absent"},
		{name: "malformed", verified: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := required.Authenticate(bearerRequest(token(test.verified)))
			require.NoError(t, err)
			// No fallback: an unverified address never reaches the principal.
			assert.Equal(t, test.wantRequired, p.Email)
			assert.Equal(t, test.wantVerified, p.EmailVerified)
			// The rest of the identity survives.
			assert.Equal(t, "alice", p.Display)
			assert.Equal(t, []string{"everyone"}, p.Groups)

			p, err = optional.Authenticate(bearerRequest(token(test.verified)))
			require.NoError(t, err)
			assert.Equal(t, "Alice@Example.com", p.Email)
			assert.Equal(t, test.wantVerified, p.EmailVerified)
		})
	}
}

func TestOIDCClaimPaths(t *testing.T) {
	idp := authtest.NewIdP(t)
	claims := authtest.Claims(idp.Issuer, audience, "user-1")
	claims["email"] = "top@example.com"
	claims["email_verified"] = true
	claims["groups"] = []string{"top-level"}
	claims["realm_access"] = map[string]any{"roles": []string{"keycloak-admin", "keycloak-user"}}
	claims["https://example.com/groups"] = []string{"auth0-admin"}
	claims["https://example.com/claims"] = map[string]any{"mail": "nested@example.com"}
	token := idp.Sign(t, claims)

	for _, test := range []struct {
		groupsClaim, emailClaim string
		wantGroups              []string
		wantEmail               string
	}{
		{groupsClaim: "", emailClaim: "", wantGroups: []string{"top-level"}, wantEmail: "top@example.com"},
		{groupsClaim: "realm_access.roles", wantGroups: []string{"keycloak-admin", "keycloak-user"}, wantEmail: "top@example.com"},
		{groupsClaim: "https://example.com/groups", wantGroups: []string{"auth0-admin"}, wantEmail: "top@example.com"},
		{emailClaim: "https://example.com/claims.mail", wantGroups: []string{"top-level"}, wantEmail: "nested@example.com"},
		{groupsClaim: "missing.path", emailClaim: "missing", wantGroups: nil, wantEmail: ""},
	} {
		a := newOIDC(t, auth.OIDCConfig{
			Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL,
			GroupsClaim: test.groupsClaim, EmailClaim: test.emailClaim, RequireEmailVerified: true,
		})
		p, err := a.Authenticate(bearerRequest(token))
		require.NoError(t, err)
		assert.Equal(t, test.wantGroups, p.Groups, test.groupsClaim)
		assert.Equal(t, test.wantEmail, p.Email, test.emailClaim)
	}
}

func TestOIDCRejectsMalformedIdentityClaims(t *testing.T) {
	idp := authtest.NewIdP(t)
	a := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL})
	for name, mutate := range map[string]func(map[string]any){
		"groups string":       func(c map[string]any) { c["groups"] = "admins" },
		"groups mixed":        func(c map[string]any) { c["groups"] = []any{"ok", 7} },
		"email number":        func(c map[string]any) { c["email"] = 7 },
		"username not string": func(c map[string]any) { c["preferred_username"] = []string{"x"} },
	} {
		claims := authtest.Claims(idp.Issuer, audience, "user-1")
		mutate(claims)
		_, err := a.Authenticate(bearerRequest(idp.Sign(t, claims)))
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, name)
	}
}

func TestOIDCDiscoversJWKSWhenUnset(t *testing.T) {
	idp := authtest.NewIdP(t)
	a := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience})
	p, err := a.Authenticate(bearerRequest(idp.Sign(t, authtest.Claims(idp.Issuer, audience, "user-1"))))
	require.NoError(t, err)
	assert.Equal(t, "user-1", p.Subject)
}

func TestOIDCDiscoveryFailureFailsClosed(t *testing.T) {
	idp := authtest.NewIdP(t)
	// Nothing listens on the discovery issuer, so every token is rejected.
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	a := newOIDC(t, auth.OIDCConfig{Issuer: unreachable.URL, Audience: audience})
	_, err := a.Authenticate(bearerRequest(idp.Sign(t, authtest.Claims(unreachable.URL, audience, "user-1"))))
	require.True(t, errors.Is(err, auth.ErrInvalidCredentials), "err = %v", err)
}

// TestOIDCStalledJWKSFetchDoesNotWedgeAuthentication: go-oidc
// shares one in-flight key fetch, so a JWKS endpoint that never finishes its
// response must time out rather than block every later request.
func TestOIDCStalledJWKSFetchDoesNotWedgeAuthentication(t *testing.T) {
	idp := authtest.NewIdP(t)
	response, err := http.Get(idp.JWKSURL)
	require.NoError(t, err)
	jwks, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	var calls atomic.Int32
	release := make(chan struct{})
	stalling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select { // headers sent, body never finishes
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(func() { close(release); stalling.Close() })

	a := newOIDC(t, auth.OIDCConfig{
		Issuer: idp.Issuer, Audience: audience, JWKSURL: stalling.URL,
		// Long enough that the healthy second fetch finishes on a slow
		// machine; the stalled first one still gives up well inside the
		// five-second bound below.
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	})
	token := idp.Sign(t, authtest.Claims(idp.Issuer, audience, "user-1"))

	started := time.Now()
	_, err = a.Authenticate(bearerRequest(token))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	require.Less(t, time.Since(started), 5*time.Second)

	// go-oidc hands the stalled fetch's error to a caller that arrives in
	// the instant between that fetch finishing and being cleared, so the
	// next request may fail once more; requests after it must succeed.
	require.Eventually(t, func() bool {
		p, err := a.Authenticate(bearerRequest(token))
		return err == nil && p.Subject == "user-1"
	}, 5*time.Second, 20*time.Millisecond)
}

func TestNewOIDCRequiresIssuerAndAudience(t *testing.T) {
	_, err := auth.NewOIDC(auth.OIDCConfig{Audience: audience})
	require.Error(t, err)
	_, err = auth.NewOIDC(auth.OIDCConfig{Issuer: "https://idp.example"})
	require.Error(t, err)
}

// TestOIDCRefusesAnotherAudience: a token the IdP issued to another client,
// an ID token for one, is not a Diderot bearer.
func TestOIDCRefusesAnotherAudience(t *testing.T) {
	const apiAudience = "https://diderot.example/api"
	idp := authtest.NewIdP(t)
	verifier := newOIDC(t, auth.OIDCConfig{Issuer: idp.Issuer, Audience: apiAudience, JWKSURL: idp.JWKSURL})
	_, err := verifier.Authenticate(bearerRequest(idp.Sign(t, authtest.Claims(idp.Issuer, apiAudience, "user-1"))))
	require.NoError(t, err)
	_, err = verifier.Authenticate(bearerRequest(idp.Sign(t, authtest.Claims(idp.Issuer, "some-client", "user-1"))))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

func TestOIDCAllowedEmailDomains(t *testing.T) {
	idp := authtest.NewIdP(t)
	token := func(email string, mutate ...func(map[string]any)) string {
		claims := authtest.Claims(idp.Issuer, audience, "user-1")
		claims["email_verified"] = true
		if email != "" {
			claims["email"] = email
		}
		for _, m := range mutate {
			m(claims)
		}
		return idp.Sign(t, claims)
	}
	unverified := func(c map[string]any) { c["email_verified"] = false }
	a := newOIDC(t, auth.OIDCConfig{
		Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL, RequireEmailVerified: true,
		AllowedEmailDomains: []string{" Example.com ", "example.org"},
	})
	for _, email := range []string{"alice@example.com", "Bob@EXAMPLE.COM", "carol@example.org"} {
		p, err := a.Authenticate(bearerRequest(token(email)))
		require.NoError(t, err, email)
		assert.Equal(t, email, p.Email)
	}
	for name, raw := range map[string]string{
		"other domain": token("guest@gmail.com"),
		"subdomain":    token("dev@eng.example.com"),
		"look-alike":   token("x@notexample.com"),
		"no email":     token(""),
		"unverified":   token("alice@example.com", unverified),
	} {
		_, err := a.Authenticate(bearerRequest(raw))
		require.ErrorIs(t, err, auth.ErrIdentityNotAllowed, name)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, name)
	}

	// An operator who vouches for the IdP's addresses matches the unverified
	// address it then accepts.
	optedOut := newOIDC(t, auth.OIDCConfig{
		Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL,
		AllowedEmailDomains: []string{"example.com"},
	})
	_, err := optedOut.Authenticate(bearerRequest(token("alice@example.com", unverified)))
	require.NoError(t, err)
	_, err = optedOut.Authenticate(bearerRequest(token("guest@gmail.com", unverified)))
	require.ErrorIs(t, err, auth.ErrIdentityNotAllowed)
}

// TestOIDCHostedDomainClaim: Google signs in consumer accounts registered
// under a work address it verified once, so only `hd` shows that the
// organization's Workspace hosts the account.
func TestOIDCHostedDomainClaim(t *testing.T) {
	idp := authtest.NewIdP(t)
	a := newOIDC(t, auth.OIDCConfig{
		Issuer: idp.Issuer, Audience: audience, JWKSURL: idp.JWKSURL, RequireEmailVerified: true,
		AllowedEmailDomains: []string{"example.com"}, HostedDomainClaim: "hd",
	})
	token := func(hd any) string {
		claims := authtest.Claims(idp.Issuer, audience, "user-1")
		claims["email"], claims["email_verified"] = "alice@example.com", true
		if hd != nil {
			claims["hd"] = hd
		}
		return idp.Sign(t, claims)
	}
	_, err := a.Authenticate(bearerRequest(token("Example.com")))
	require.NoError(t, err)
	for name, hd := range map[string]any{"absent": nil, "other": "gmail.com", "empty": ""} {
		_, err := a.Authenticate(bearerRequest(token(hd)))
		require.ErrorIs(t, err, auth.ErrIdentityNotAllowed, name)
	}
	_, err = a.Authenticate(bearerRequest(token([]string{"example.com"})))
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)

	_, err = auth.NewOIDC(auth.OIDCConfig{Issuer: idp.Issuer, Audience: audience, HostedDomainClaim: "hd"})
	require.ErrorContains(t, err, "allowed email domains")
}
