package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig configures bearer verification of IdP-issued JWT access tokens.
type OIDCConfig struct {
	// Issuer is the `iss` required on tokens.
	Issuer string
	// Audience is required in the token's `aud`.
	Audience string
	// JWKSURL is where signing keys are fetched. It may name a different host
	// than Issuer (an in-cluster IdP service, for example); signing keys are
	// host-independent. Empty discovers the JWKS from
	// Issuer/.well-known/openid-configuration on first use.
	JWKSURL string
	// GroupsClaim and EmailClaim are claim paths (see lookupClaim). Empty
	// means "groups" and "email".
	GroupsClaim string
	EmailClaim  string
	// RequireEmailVerified leaves Principal.Email empty unless the token
	// carries `email_verified: true`. There is no fallback to the unverified
	// address.
	RequireEmailVerified bool
	// AllowedEmailDomains, when non-empty, rejects every token whose email
	// (as RequireEmailVerified leaves it) is missing or not at one of these
	// domains. Matching is exact and case-insensitive: a subdomain must be
	// listed itself. Empty admits any domain.
	AllowedEmailDomains []string
	// HostedDomainClaim additionally requires this claim (Google's "hd") to
	// name one of AllowedEmailDomains, which shows the IdP hosts the account
	// rather than only having verified its address once. Requires
	// AllowedEmailDomains.
	HostedDomainClaim string
	// HTTPClient fetches discovery and JWKS documents. Nil uses a client
	// bounded by keyFetchTimeout.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// keyFetchTimeout bounds each discovery or JWKS fetch. go-oidc shares one
// in-flight key fetch among all callers, so an unbounded fetch to a stalled
// IdP would block every later cache miss for the life of the process.
const keyFetchTimeout = 10 * time.Second

// OIDC authenticates `Authorization: Bearer` JWTs offline against the IdP's
// signing keys. Construction never touches the network: keys (and, without
// JWKSURL, the discovery document) are fetched on first use and cached.
type OIDC struct {
	verifier             func(context.Context) (*oidc.IDTokenVerifier, error)
	groupsClaim          string
	emailClaim           string
	requireEmailVerified bool
	// allowedDomains is nil when every email domain is admitted.
	allowedDomains    map[string]struct{}
	hostedDomainClaim string
}

func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if strings.TrimSpace(cfg.Issuer) == "" || strings.TrimSpace(cfg.Audience) == "" {
		return nil, errors.New("auth: OIDC issuer and audience are required")
	}
	a, err := newOIDC(cfg)
	if err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: keyFetchTimeout}
	}
	// go-oidc reads its HTTP client from the context it is constructed with.
	fetchCtx := oidc.ClientContext(context.Background(), client)
	verifierConfig := &oidc.Config{ClientID: cfg.Audience}
	if cfg.JWKSURL != "" {
		keys := oidc.NewRemoteKeySet(fetchCtx, cfg.JWKSURL)
		v := oidc.NewVerifier(cfg.Issuer, keys, verifierConfig)
		a.verifier = func(context.Context) (*oidc.IDTokenVerifier, error) { return v, nil }
		return a, nil
	}
	d := &discovery{issuer: cfg.Issuer, config: verifierConfig, fetchCtx: fetchCtx, logger: logger}
	a.verifier = d.get
	return a, nil
}

func newOIDC(cfg OIDCConfig) (*OIDC, error) {
	a := &OIDC{
		groupsClaim:          strings.TrimSpace(cfg.GroupsClaim),
		emailClaim:           strings.TrimSpace(cfg.EmailClaim),
		requireEmailVerified: cfg.RequireEmailVerified,
		hostedDomainClaim:    strings.TrimSpace(cfg.HostedDomainClaim),
	}
	for _, domain := range cfg.AllowedEmailDomains {
		if domain = strings.ToLower(strings.TrimSpace(domain)); domain != "" {
			if a.allowedDomains == nil {
				a.allowedDomains = map[string]struct{}{}
			}
			a.allowedDomains[domain] = struct{}{}
		}
	}
	if a.hostedDomainClaim != "" && a.allowedDomains == nil {
		return nil, errors.New("auth: a hosted-domain claim requires allowed email domains")
	}
	if a.groupsClaim == "" {
		a.groupsClaim = "groups"
	}
	if a.emailClaim == "" {
		a.emailClaim = "email"
	}
	return a, nil
}

func (a *OIDC) Authenticate(r *http.Request) (*Principal, error) {
	raw := BearerToken(r)
	if raw == "" {
		return nil, ErrNoCredentials
	}
	return a.verify(r.Context(), raw)
}

func (a *OIDC) verify(ctx context.Context, raw string) (*Principal, error) {
	verifier, err := a.verifier(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	token, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decode claims: %v", ErrInvalidCredentials, err)
	}
	return a.principal(token.Subject, claims)
}

// principal projects verified claims and applies the allowed-domain policy.
// A configured claim that is present with the wrong shape rejects the token
// rather than silently dropping the identity it was meant to carry.
func (a *OIDC) principal(subject string, claims map[string]any) (*Principal, error) {
	email, err := stringClaim(claims, a.emailClaim)
	if err != nil {
		return nil, err
	}
	display, err := stringClaim(claims, "preferred_username")
	if err != nil {
		return nil, err
	}
	groups, err := stringsClaim(claims, a.groupsClaim)
	if err != nil {
		return nil, err
	}
	verified := emailVerified(claims["email_verified"])
	p := &Principal{
		Subject:       subject,
		EmailVerified: verified,
		Display:       strings.TrimSpace(display),
		Groups:        groups,
	}
	if verified || !a.requireEmailVerified {
		p.Email = strings.TrimSpace(email)
	}
	if err := a.admit(p, claims); err != nil {
		return nil, err
	}
	return p, nil
}

// admit enforces AllowedEmailDomains and HostedDomainClaim. A disallowed
// identity is refused outright, not kept without an email: the use routes
// admit any authenticated caller to the baseline corpus, so an emailless
// principal from any account the IdP signs in would still read it.
func (a *OIDC) admit(p *Principal, claims map[string]any) error {
	if a.allowedDomains == nil {
		return nil
	}
	at := strings.LastIndexByte(p.Email, '@')
	if at < 0 {
		return fmt.Errorf("%w: no accepted email to match against the allowed domains", ErrIdentityNotAllowed)
	}
	domain := strings.ToLower(p.Email[at+1:])
	if _, ok := a.allowedDomains[domain]; !ok {
		return fmt.Errorf("%w: email domain %q is not allowed", ErrIdentityNotAllowed, domain)
	}
	if a.hostedDomainClaim == "" {
		return nil
	}
	hosted, err := stringClaim(claims, a.hostedDomainClaim)
	if err != nil {
		return err
	}
	hosted = strings.ToLower(strings.TrimSpace(hosted))
	if _, ok := a.allowedDomains[hosted]; !ok {
		return fmt.Errorf("%w: hosted domain claim %q is %q", ErrIdentityNotAllowed, a.hostedDomainClaim, hosted)
	}
	return nil
}

// emailVerified accepts the boolean the OIDC spec defines and the string
// "true" some IdPs emit instead. Anything else is unverified.
func emailVerified(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

func stringClaim(claims map[string]any, path string) (string, error) {
	v, ok := lookupClaim(claims, path)
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: claim %q is not a string", ErrInvalidCredentials, path)
	}
	return s, nil
}

func stringsClaim(claims map[string]any, path string) ([]string, error) {
	v, ok := lookupClaim(claims, path)
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: claim %q is not an array", ErrInvalidCredentials, path)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%w: claim %q has a non-string element", ErrInvalidCredentials, path)
		}
		out = append(out, s)
	}
	return out, nil
}

// lookupClaim resolves a claim path. The whole path is tried first as a
// literal key, so namespaced URL claims such as Auth0's
// "https://example.com/groups" work despite their dots. Otherwise the path is
// split at a dot into an object key and the remaining path, longest key
// first: "realm_access.roles" reads Keycloak's nested realm roles.
func lookupClaim(claims map[string]any, path string) (any, bool) {
	if v, ok := claims[path]; ok {
		return v, true
	}
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '.' {
			continue
		}
		child, ok := claims[path[:i]].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := lookupClaim(child, path[i+1:]); ok {
			return v, true
		}
	}
	return nil, false
}

// discoveryRetryInterval bounds how often a failing discovery fetch is
// retried, so a burst of requests during an IdP outage cannot fan out into a
// burst of discovery requests.
const discoveryRetryInterval = 5 * time.Second

type discovery struct {
	issuer string
	config *oidc.Config
	// fetchCtx carries the bounded HTTP client; the provider keeps it for its
	// own JWKS fetches.
	fetchCtx context.Context
	logger   *slog.Logger

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	failedAt time.Time
	err      error
}

func (d *discovery) get(context.Context) (*oidc.IDTokenVerifier, error) {
	_, verifier, err := d.discover()
	return verifier, err
}

// discover returns the discovered provider and a verifier built from its
// keys and d.config.
func (d *discovery) discover() (*oidc.Provider, *oidc.IDTokenVerifier, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.provider != nil {
		return d.provider, d.verifier, nil
	}
	if d.err != nil && time.Since(d.failedAt) < discoveryRetryInterval {
		return nil, nil, d.err
	}
	// Discovery runs on the process context rather than the caller's, so one
	// caller's disconnect cannot fail it for everyone queued behind the lock;
	// the bounded client caps how long they wait.
	provider, err := oidc.NewProvider(d.fetchCtx, d.issuer)
	if err != nil {
		d.err = fmt.Errorf("OIDC discovery: %w", err)
		d.failedAt = time.Now()
		d.logger.Warn("OIDC discovery failed", "issuer", d.issuer, "err", err)
		return nil, nil, d.err
	}
	// The provider's key set fetches lazily with its own background context.
	d.provider = provider
	d.verifier = provider.Verifier(d.config)
	d.err = nil
	return d.provider, d.verifier, nil
}
