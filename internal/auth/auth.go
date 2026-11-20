// Package auth is the server-side authentication seam shared by the JSON API
// (/v1), the browser UI (/admin, /sessions), and the corpus MCP endpoint.
//
// An Authenticator turns one request into a Principal. It answers only "who
// is this?"; group allowlists and corpus policy stay with the callers.
//
// Email is the identity join key: the same lowercased address names a Slack
// profile (private-channel scopes), an agent-session owner, and a self-service
// member. An Authenticator therefore leaves Principal.Email empty unless the
// address is trustworthy: verified by the IdP, asserted by a trusted proxy, or
// supplied by operator configuration.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Principal is one authenticated caller.
type Principal struct {
	// Subject is the stable identifier for audit logs: the OIDC `sub`, or the
	// email for identities that have no separate subject.
	Subject string
	// Email is empty when the source could not vouch for it. Consumers use it
	// directly; they never consult EmailVerified to decide whether to trust it.
	Email string
	// EmailVerified records whether the source vouched for Email. It is
	// informational: with DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED=false an OIDC
	// principal carries an unverified Email by operator choice.
	EmailVerified bool
	// Display is best-effort presentation text (OIDC preferred_username).
	Display string
	Groups  []string
	// Bootstrap marks the built-in bootstrap admin (BootstrapPrincipal). It
	// has no groups; browser surfaces admit it as an administrator, and its
	// authenticator issues it only while the account is enabled. No other
	// surface ever sees it.
	Bootstrap bool
}

// HasGroup reports whether p is a member of group.
func (p *Principal) HasGroup(group string) bool {
	return p != nil && slices.Contains(p.Groups, group)
}

// HasAnyGroup reports whether p is a member of at least one group in allowed.
// An empty allowlist matches nobody.
func (p *Principal) HasAnyGroup(allowed map[string]struct{}) bool {
	if p == nil {
		return false
	}
	for _, group := range p.Groups {
		if _, ok := allowed[group]; ok {
			return true
		}
	}
	return false
}

func (p *Principal) clone() *Principal {
	c := *p
	c.Groups = slices.Clone(p.Groups)
	return &c
}

// Authenticator identifies the caller of one request. It returns
// ErrNoCredentials when the request carries no credential this authenticator
// accepts, and an error wrapping ErrInvalidCredentials when it carries one
// that fails verification.
type Authenticator interface {
	Authenticate(*http.Request) (*Principal, error)
}

var (
	ErrNoCredentials      = errors.New("auth: no credentials")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrIdentityNotAllowed means a credential verified but names an
	// identity outside the deployment's allowed email domains. It wraps
	// ErrInvalidCredentials, so a caller that only tells missing from bad
	// credentials refuses it as bad.
	ErrIdentityNotAllowed = fmt.Errorf("%w: identity not allowed", ErrInvalidCredentials)
)

// BearerToken returns the token from an `Authorization: Bearer` header, or ""
// when the header is absent or uses another scheme.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

type principalContextKey struct{}

// NewContext returns ctx carrying p.
func NewContext(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// FromContext returns the principal installed by NewContext, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalContextKey{}).(*Principal)
	return p
}

// Fixed authenticates every request as one principal. It backs
// DIDEROT_AUTH_MODE=none (loopback-only, single administrator) and the
// browser side of session-dev, and ignores any credential or header the
// request carries.
type Fixed struct {
	principal Principal
}

func NewFixed(p Principal) *Fixed {
	return &Fixed{principal: *p.clone()}
}

func (f *Fixed) Authenticate(*http.Request) (*Principal, error) {
	return f.principal.clone(), nil
}
