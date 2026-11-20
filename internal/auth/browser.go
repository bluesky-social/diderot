package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Routes of the built-in browser sign-in. The IdP client's registered
// redirect URI is the public origin + CallbackPath.
const (
	LoginPath    = "/auth/login"
	CallbackPath = "/auth/callback"
	LogoutPath   = "/auth/logout"
)

var (
	// ErrLoginState means the callback does not belong to a sign-in this
	// browser started: the sign-in cookie is missing, expired, or already
	// used, or its state does not match.
	ErrLoginState = errors.New("auth: sign-in state missing, expired, or mismatched")
	// ErrLoginDenied means the IdP answered the callback with an error.
	ErrLoginDenied = errors.New("auth: identity provider denied sign-in")
	// ErrLoginProvider means discovery or the code exchange failed.
	ErrLoginProvider = errors.New("auth: identity provider unavailable")
	// ErrLoginNoEmail means the ID token carried no email the configured
	// claim rules accept (see OIDCConfig.RequireEmailVerified). Every browser
	// surface is attributed to and owner-scoped by email, so no session is
	// issued.
	ErrLoginNoEmail = errors.New("auth: identity has no verified email")
)

const (
	// loginStateTTL bounds how long a started sign-in may take at the IdP.
	loginStateTTL = 10 * time.Minute
	// maxCookieValue keeps a sealed session inside every browser's 4 KiB
	// per-cookie limit, attributes included.
	maxCookieValue = 3800
	// sessionClockSkew tolerates replicas whose clocks disagree slightly
	// about when a session was issued.
	sessionClockSkew = time.Minute

	purposeSession = "diderot/browser-session/v1"
	purposeLogin   = "diderot/browser-signin/v1"
)

// BrowserLoginConfig configures the browser's own sign-in: a sealed session
// cookie issued by any configured method. The methods are the OIDC
// authorization-code flow (when Issuer is set), the bootstrap admin (when
// Bootstrap is set), and static bearer tokens pasted into a form (when
// StaticTokens is set).
type BrowserLoginConfig struct {
	// Issuer is the OIDC issuer; its discovery document supplies the
	// authorization and token endpoints. Empty turns the OIDC sign-in off,
	// and the OIDC fields below are then ignored.
	Issuer string
	// JWKSURL optionally overrides the discovered signing-key endpoint, as
	// OIDCConfig.JWKSURL does for bearer tokens.
	JWKSURL string
	// ClientID is the browser client registered at the IdP, and the
	// audience its ID tokens must carry. ClientSecret is empty for a public
	// client; PKCE protects the code either way.
	ClientID     string
	ClientSecret string
	// Scopes must include "openid".
	Scopes []string
	// GroupsClaim, EmailClaim, and RequireEmailVerified project ID-token
	// claims, and AllowedEmailDomains and HostedDomainClaim admit them,
	// exactly as OIDCConfig does for bearer tokens. A refused identity gets
	// no session.
	GroupsClaim          string
	EmailClaim           string
	RequireEmailVerified bool
	AllowedEmailDomains  []string
	HostedDomainClaim    string
	// SessionGroups are the only groups an OIDC session records: the browser
	// gates only on its administrator and self-service allowlists, and an
	// IdP's full group list may not fit in a cookie. Required with Issuer.
	SessionGroups []string

	// Bootstrap admits the built-in bootstrap admin while it reports the
	// account enabled. Nil offers no bootstrap admin.
	Bootstrap BootstrapAdmin
	// StaticTokens admits a pasted static-mode bearer token. Nil offers no
	// token sign-in.
	StaticTokens *Static
	// Proxy identifies a browser that has no valid session, from trusted
	// proxy headers. Nil or disabled reads no headers.
	Proxy *TrustedProxy

	// PublicURL is the browser-facing origin. The redirect URI is
	// PublicURL + CallbackPath. It must be https, or http on a loopback host.
	PublicURL string
	// SessionKeys are 32-byte AES-256-GCM keys. The first seals new cookies
	// and every key opens them. A rotation across replicas first appends the
	// new key everywhere, then moves it first, then drops the old key once
	// sessions sealed with it have expired.
	SessionKeys [][]byte
	// SessionTTL bounds a session from sign-in; IdleTimeout ends it after
	// that long without a browser request. 0 < IdleTimeout <= SessionTTL.
	SessionTTL  time.Duration
	IdleTimeout time.Duration
	// InsecureCookies drops the Secure attribute (and with it the __Host-
	// cookie prefix) for a plain-HTTP literal loopback IP PublicURL during
	// development.
	InsecureCookies bool
	// HTTPClient reaches the IdP. Nil uses a client bounded by
	// keyFetchTimeout.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// BootstrapAdmin reports whether the bootstrap admin is enabled. It is asked
// on every request a bootstrap session makes, so it must read the source of
// truth rather than a cache: turning the account off ends its sessions at
// once.
type BootstrapAdmin interface {
	BootstrapAdminEnabled(ctx context.Context) (bool, error)
}

// BrowserLogin is the browser's own sign-in. Every method ends in the same
// sealed session cookie; the OIDC method is an authorization-code flow with
// PKCE (S256), state, and nonce.
//
// The session is stateless: an AES-256-GCM sealed cookie holding the sign-in
// method and its issue and last-seen times, so every replica can verify it
// without shared storage. An OIDC session also holds the principal. A
// bootstrap or static-token session is re-resolved on every request: the
// bootstrap admin against BootstrapAdmin, a token by its SHA-256 against the
// loaded token set. Signing out deletes the cookie; a copy taken before then
// stays valid until its idle or absolute deadline, which bounds the exposure
// instead of a server-side revocation list.
type BrowserLogin struct {
	clientID     string
	clientSecret string
	scopes       []string
	redirectURL  string
	discovery    *discovery
	// verifier is set when JWKSURL overrides the discovered keys.
	verifier   *oidc.IDTokenVerifier
	client     *http.Client
	claims     *OIDC
	keepGroups map[string]struct{}
	sealer     *sealer

	bootstrap BootstrapAdmin
	static    *Static
	proxy     *TrustedProxy

	ttl     time.Duration
	idle    time.Duration
	refresh time.Duration
	secure  bool
	// sessionCookie and loginCookie carry the __Host- prefix when secure, so
	// a sibling subdomain can neither set nor shadow them.
	sessionCookie string
	loginCookie   string
	now           func() time.Time
}

func NewBrowserLogin(cfg BrowserLoginConfig) (*BrowserLogin, error) {
	secure, err := browserCookieSecurity(cfg.PublicURL, cfg.InsecureCookies)
	if err != nil {
		return nil, err
	}
	if cfg.SessionTTL <= 0 || cfg.IdleTimeout <= 0 || cfg.IdleTimeout > cfg.SessionTTL {
		return nil, errors.New("auth: browser session needs 0 < idle timeout <= lifetime")
	}
	sealer, err := newSealer(cfg.SessionKeys)
	if err != nil {
		return nil, err
	}
	b := &BrowserLogin{
		sealer:        sealer,
		bootstrap:     cfg.Bootstrap,
		static:        cfg.StaticTokens,
		proxy:         cfg.Proxy,
		ttl:           cfg.SessionTTL,
		idle:          cfg.IdleTimeout,
		refresh:       min(time.Minute, cfg.IdleTimeout/4),
		secure:        secure,
		sessionCookie: "diderot_session",
		loginCookie:   "diderot_signin",
		now:           time.Now,
	}
	if secure {
		b.sessionCookie = "__Host-" + b.sessionCookie
		b.loginCookie = "__Host-" + b.loginCookie
	}
	if strings.TrimSpace(cfg.Issuer) != "" {
		if err := b.configureOIDC(cfg); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *BrowserLogin) configureOIDC(cfg BrowserLoginConfig) error {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return errors.New("auth: browser sign-in requires an OIDC client ID")
	}
	if !slices.Contains(cfg.Scopes, oidc.ScopeOpenID) {
		return errors.New(`auth: browser sign-in scopes must include "openid"`)
	}
	keep := map[string]struct{}{}
	for _, group := range cfg.SessionGroups {
		if group = strings.TrimSpace(group); group != "" {
			keep[group] = struct{}{}
		}
	}
	if len(keep) == 0 {
		return errors.New("auth: browser sign-in requires at least one session group")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: keyFetchTimeout}
	}
	claims, err := newOIDC(OIDCConfig{
		GroupsClaim: cfg.GroupsClaim, EmailClaim: cfg.EmailClaim,
		RequireEmailVerified: cfg.RequireEmailVerified,
		AllowedEmailDomains:  cfg.AllowedEmailDomains, HostedDomainClaim: cfg.HostedDomainClaim,
	})
	if err != nil {
		return err
	}
	fetchCtx := oidc.ClientContext(context.Background(), client)
	verifierConfig := &oidc.Config{ClientID: strings.TrimSpace(cfg.ClientID)}
	b.clientID = strings.TrimSpace(cfg.ClientID)
	b.clientSecret = cfg.ClientSecret
	b.scopes = slices.Clone(cfg.Scopes)
	b.redirectURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/") + CallbackPath
	b.discovery = &discovery{issuer: cfg.Issuer, config: verifierConfig, fetchCtx: fetchCtx, logger: logger}
	b.client = client
	b.claims = claims
	b.keepGroups = keep
	if cfg.JWKSURL != "" {
		b.verifier = oidc.NewVerifier(cfg.Issuer, oidc.NewRemoteKeySet(fetchCtx, cfg.JWKSURL), verifierConfig)
	}
	return nil
}

// OIDCEnabled reports whether the OIDC sign-in is configured.
func (b *BrowserLogin) OIDCEnabled() bool { return b.discovery != nil }

// browserCookieSecurity reports whether cookies are Secure. A plain-HTTP
// origin is accepted only on loopback, where browsers treat it as a secure
// context; anywhere else the session would cross the network in clear text.
// Dropping Secure additionally needs a literal loopback IP, as the browser
// UI's own CSRF cookies do (adminui.Config.AllowInsecureLoopbackCookies).
func browserCookieSecurity(publicURL string, insecure bool) (bool, error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || u.Host == "" {
		return false, errors.New("auth: browser sign-in requires the public URL")
	}
	ip := net.ParseIP(u.Hostname())
	literalLoopback := ip != nil && ip.IsLoopback()
	switch {
	case u.Scheme == "https" && !insecure:
		return true, nil
	case u.Scheme == "http" && insecure && literalLoopback:
		return false, nil
	case u.Scheme == "http" && !insecure && (literalLoopback || u.Hostname() == "localhost"):
		return true, nil
	case insecure:
		return false, errors.New("auth: insecure browser session cookies require a plain-HTTP literal loopback IP public URL")
	default:
		return false, errors.New("auth: browser sign-in requires an https public URL (plain HTTP only on loopback)")
	}
}

// Session methods: how a session was signed in, and so how each request
// resolves its principal.
const (
	methodOIDC      = "oidc"
	methodBootstrap = "bootstrap"
	methodStatic    = "static"
)

// browserSession is the sealed session cookie's payload.
type browserSession struct {
	Method string `json:"method"`
	// An OIDC session records the identity the IdP vouched for.
	Subject       string   `json:"sub,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
	Display       string   `json:"name,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	// A static-token session records the token's SHA-256 (hex), the value
	// the token file already holds, never the token.
	TokenSHA256 string `json:"token_sha256,omitempty"`
	// IssuedAt and SeenAt are Unix seconds. Deadlines are computed from the
	// current configuration, so shortening a lifetime applies at once.
	IssuedAt int64 `json:"iat"`
	SeenAt   int64 `json:"seen"`
}

// loginState is the sealed sign-in cookie's payload: everything the callback
// must match, kept out of the authorization URL.
type loginState struct {
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	ReturnTo  string `json:"return_to"`
	CreatedAt int64  `json:"created"`
}

// Authenticate identifies the browser from its session cookie, then from
// trusted proxy headers when it has no valid session. Only a bootstrap
// session touches the network (one BootstrapAdmin check).
func (b *BrowserLogin) Authenticate(r *http.Request) (*Principal, error) {
	return b.authenticate(nil, r)
}

// AuthenticateBrowser is Authenticate for a browser surface that may answer
// with a refreshed cookie: it slides the idle deadline, re-seals a session
// opened by a retired key under the current one, and deletes a cookie that
// no longer verifies.
func (b *BrowserLogin) AuthenticateBrowser(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	return b.authenticate(w, r)
}

// authenticate prefers a valid session over proxy headers, so a browser
// behind a trusted proxy can still sign in locally (as the bootstrap admin,
// say) and sign out again to fall back to its proxy identity.
func (b *BrowserLogin) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	principal, err := b.sessionPrincipal(w, r)
	if err == nil || b.proxy == nil {
		return principal, err
	}
	if proxied, proxyErr := b.proxy.Authenticate(r); proxyErr == nil {
		return proxied, nil
	}
	return nil, err
}

func (b *BrowserLogin) sessionPrincipal(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	session, keyIndex, err := b.openSession(r)
	var principal *Principal
	if err == nil {
		principal, err = b.resolve(r.Context(), session)
	}
	if err != nil {
		if w != nil && errors.Is(err, ErrInvalidCredentials) {
			http.SetCookie(w, b.cookie(b.sessionCookie, "", -1))
		}
		return nil, err
	}
	if w != nil {
		now := b.now()
		if keyIndex != 0 || now.Sub(time.Unix(session.SeenAt, 0)) >= b.refresh {
			session.SeenAt = now.Unix()
			// A failed refresh leaves the current cookie, which stays valid.
			_ = b.setSession(w, session)
		}
	}
	return principal, nil
}

func (b *BrowserLogin) openSession(r *http.Request) (*browserSession, int, error) {
	c, err := r.Cookie(b.sessionCookie)
	if err != nil || c.Value == "" {
		return nil, 0, ErrNoCredentials
	}
	var session browserSession
	keyIndex, err := b.sealer.open(purposeSession, c.Value, &session)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: session cookie: %v", ErrInvalidCredentials, err)
	}
	now := b.now()
	issued, seen := time.Unix(session.IssuedAt, 0), time.Unix(session.SeenAt, 0)
	switch {
	case issued.After(now.Add(sessionClockSkew)):
		return nil, 0, fmt.Errorf("%w: session issued in the future", ErrInvalidCredentials)
	case !now.Before(issued.Add(b.ttl)):
		return nil, 0, fmt.Errorf("%w: session expired", ErrInvalidCredentials)
	case !now.Before(seen.Add(b.idle)):
		return nil, 0, fmt.Errorf("%w: session idle too long", ErrInvalidCredentials)
	}
	return &session, keyIndex, nil
}

// resolve returns the principal a session stands for under the current
// configuration. A method that is no longer configured, a bootstrap admin
// that has been turned off, and a token removed from the token set all end
// the session.
func (b *BrowserLogin) resolve(ctx context.Context, session *browserSession) (*Principal, error) {
	switch session.Method {
	case methodOIDC:
		if !b.OIDCEnabled() {
			return nil, fmt.Errorf("%w: OIDC sign-in is not configured", ErrInvalidCredentials)
		}
		if session.Subject == "" || session.Email == "" {
			return nil, fmt.Errorf("%w: session has no identity", ErrInvalidCredentials)
		}
		return &Principal{
			Subject: session.Subject, Email: session.Email, EmailVerified: session.EmailVerified,
			Display: session.Display, Groups: slices.Clone(session.Groups),
		}, nil
	case methodBootstrap:
		enabled, err := b.bootstrapEnabled(ctx)
		if err != nil {
			return nil, err
		}
		if !enabled {
			return nil, ErrBootstrapDisabled
		}
		return BootstrapPrincipal(), nil
	case methodStatic:
		principal, ok := b.static.lookupHex(session.TokenSHA256)
		if !ok {
			return nil, fmt.Errorf("%w: static token is no longer configured", ErrInvalidCredentials)
		}
		return principal, nil
	default:
		return nil, fmt.Errorf("%w: unknown session method", ErrInvalidCredentials)
	}
}

// setSession seals session into the session cookie. The cookie expires at
// the earlier of its idle and absolute deadlines.
func (b *BrowserLogin) setSession(w http.ResponseWriter, session *browserSession) error {
	value, err := b.sealer.seal(purposeSession, session)
	if err != nil {
		return err
	}
	if len(value) > maxCookieValue {
		return errors.New("auth: session too large for a cookie")
	}
	deadline := min(session.IssuedAt+int64(b.ttl.Seconds()), session.SeenAt+int64(b.idle.Seconds()))
	http.SetCookie(w, b.cookie(b.sessionCookie, value, int(deadline-b.now().Unix())))
	return nil
}

func (b *BrowserLogin) cookie(name, value string, maxAge int) *http.Cookie {
	if maxAge == 0 {
		// http.Cookie reads MaxAge 0 as "no Max-Age", a browser-session
		// cookie; a deadline under a second away is already past.
		maxAge = -1
	}
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: b.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

// StartLogin begins a sign-in that will return to returnTo (see
// SafeReturnPath). It records state, nonce, and the PKCE verifier in a
// short-lived sealed cookie and returns the IdP authorization URL.
func (b *BrowserLogin) StartLogin(w http.ResponseWriter, returnTo string) (string, error) {
	if !b.OIDCEnabled() {
		return "", fmt.Errorf("%w: OIDC sign-in is not configured", ErrLoginProvider)
	}
	provider, _, err := b.endpoints()
	if err != nil {
		return "", err
	}
	state := loginState{
		State: rand.Text(), Nonce: rand.Text(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: SafeReturnPath(returnTo), CreatedAt: b.now().Unix(),
	}
	value, err := b.sealer.seal(purposeLogin, state)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, b.cookie(b.loginCookie, value, int(loginStateTTL.Seconds())))
	return b.oauthConfig(provider).AuthCodeURL(state.State,
		oidc.Nonce(state.Nonce), oauth2.S256ChallengeOption(state.Verifier)), nil
}

// FinishLogin completes the sign-in at the callback: it checks the state
// against the sign-in cookie, redeems the code with the PKCE verifier,
// verifies the ID token (signature, issuer, audience, expiry, nonce), and
// sets the session cookie. The sign-in cookie is consumed whatever the
// outcome. It returns the validated path to continue to.
func (b *BrowserLogin) FinishLogin(w http.ResponseWriter, r *http.Request) (string, *Principal, error) {
	if !b.OIDCEnabled() {
		return "", nil, fmt.Errorf("%w: OIDC sign-in is not configured", ErrLoginProvider)
	}
	c, err := r.Cookie(b.loginCookie)
	http.SetCookie(w, b.cookie(b.loginCookie, "", -1))
	if err != nil || c.Value == "" {
		return "", nil, fmt.Errorf("%w: no sign-in in progress", ErrLoginState)
	}
	var state loginState
	if _, err := b.sealer.open(purposeLogin, c.Value, &state); err != nil {
		return "", nil, fmt.Errorf("%w: sign-in cookie: %v", ErrLoginState, err)
	}
	if b.now().Sub(time.Unix(state.CreatedAt, 0)) > loginStateTTL {
		return "", nil, fmt.Errorf("%w: sign-in expired", ErrLoginState)
	}
	query := r.URL.Query()
	if state.State == "" || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state.State)) != 1 {
		return "", nil, fmt.Errorf("%w: state mismatch", ErrLoginState)
	}
	if code := query.Get("error"); code != "" {
		return "", nil, fmt.Errorf("%w: %s", ErrLoginDenied, boundedErrorCode(code))
	}
	code := query.Get("code")
	if code == "" {
		return "", nil, fmt.Errorf("%w: callback carries no code", ErrLoginState)
	}

	provider, verifier, err := b.endpoints()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), b.client), keyFetchTimeout)
	defer cancel()
	token, err := b.oauthConfig(provider).Exchange(ctx, code, oauth2.VerifierOption(state.Verifier))
	if err != nil {
		return "", nil, fmt.Errorf("%w: code exchange: %s", ErrLoginProvider, exchangeError(err))
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return "", nil, fmt.Errorf("%w: token response has no id_token", ErrLoginProvider)
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", nil, fmt.Errorf("%w: ID token: %v", ErrInvalidCredentials, err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(state.Nonce)) != 1 {
		return "", nil, fmt.Errorf("%w: ID token nonce mismatch", ErrInvalidCredentials)
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return "", nil, fmt.Errorf("%w: decode ID token claims: %v", ErrInvalidCredentials, err)
	}
	// The token came straight from the token endpoint, but an authorized
	// party other than this client still means it was minted for another.
	if azp, ok := claims["azp"].(string); ok && azp != b.clientID {
		return "", nil, fmt.Errorf("%w: ID token authorized party %q", ErrInvalidCredentials, azp)
	}
	if idToken.Subject == "" {
		return "", nil, fmt.Errorf("%w: ID token has no subject", ErrInvalidCredentials)
	}
	principal, err := b.claims.principal(idToken.Subject, claims)
	if err != nil {
		return "", nil, err
	}
	if principal.Email == "" {
		return "", nil, ErrLoginNoEmail
	}
	var groups []string
	for _, group := range principal.Groups {
		if _, ok := b.keepGroups[group]; ok && !slices.Contains(groups, group) {
			groups = append(groups, group)
		}
	}
	principal.Groups = groups
	now := b.now().Unix()
	session := &browserSession{
		Method:  methodOIDC,
		Subject: principal.Subject, Email: principal.Email, EmailVerified: principal.EmailVerified,
		Display: principal.Display, Groups: groups, IssuedAt: now, SeenAt: now,
	}
	if err := b.setSession(w, session); err != nil {
		return "", nil, err
	}
	return SafeReturnPath(state.ReturnTo), principal, nil
}

// Logout deletes the session (and any sign-in in progress). The IdP's own
// session is untouched, so the next sign-in may complete without a prompt.
func (b *BrowserLogin) Logout(w http.ResponseWriter) {
	http.SetCookie(w, b.cookie(b.sessionCookie, "", -1))
	http.SetCookie(w, b.cookie(b.loginCookie, "", -1))
}

func (b *BrowserLogin) endpoints() (*oidc.Provider, *oidc.IDTokenVerifier, error) {
	provider, verifier, err := b.discovery.discover()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrLoginProvider, err)
	}
	if b.verifier != nil {
		verifier = b.verifier
	}
	return provider, verifier, nil
}

func (b *BrowserLogin) oauthConfig(provider *oidc.Provider) *oauth2.Config {
	endpoint := provider.Endpoint()
	if b.clientSecret == "" {
		// A public client authenticates with client_id alone. Probing
		// HTTP Basic first could spend the one-time code on a rejected try.
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{
		ClientID: b.clientID, ClientSecret: b.clientSecret, Endpoint: endpoint,
		RedirectURL: b.redirectURL, Scopes: b.scopes,
	}
}

// exchangeError describes a failed code exchange by its OAuth error code
// alone: the raw response body is IdP-controlled and unbounded.
func exchangeError(err error) string {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if retrieve.ErrorCode != "" {
			return boundedErrorCode(retrieve.ErrorCode)
		}
		if retrieve.Response != nil {
			return retrieve.Response.Status
		}
		return "token endpoint error"
	}
	return err.Error()
}

// boundedErrorCode keeps an IdP-supplied OAuth error code loggable: RFC 6749
// limits it to printable ASCII, and anything else is replaced.
func boundedErrorCode(code string) string {
	if len(code) > 64 {
		code = code[:64]
	}
	for _, r := range code {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return "invalid_error_code"
		}
	}
	return code
}

// SafeReturnPath returns raw when it is a same-origin, root-relative path
// that a sign-in may return to, and "/" otherwise. It refuses anything a
// browser could resolve to another origin (scheme-relative "//host",
// backslashes, embedded control characters a browser would strip) and the
// sign-in routes themselves, which would loop.
func SafeReturnPath(raw string) string {
	if raw == "" || len(raw) > 2048 || raw[0] != '/' || strings.HasPrefix(raw, "//") {
		return "/"
	}
	for _, r := range raw {
		if r <= 0x20 || r >= 0x7f || r == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	if clean := path.Clean(u.Path); clean == "/auth" || strings.HasPrefix(clean, "/auth/") {
		return "/"
	}
	return raw
}

// sealer encrypts and authenticates cookie payloads with AES-256-GCM. The
// purpose is bound as additional data, so a sign-in cookie can never be
// presented as a session.
type sealer struct {
	aeads []cipher.AEAD
}

const sealVersion = 1

func newSealer(keys [][]byte) (*sealer, error) {
	if len(keys) == 0 {
		return nil, errors.New("auth: browser sessions require at least one key")
	}
	s := &sealer{}
	for i, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("auth: browser session key %d must be 32 bytes", i)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("auth: browser session key %d: %w", i, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("auth: browser session key %d: %w", i, err)
		}
		s.aeads = append(s.aeads, aead)
	}
	return s, nil
}

func (s *sealer) seal(purpose string, v any) (string, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	aead := s.aeads[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(append([]byte{sealVersion}, nonce...), nonce, plaintext, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open decrypts value into v and returns the index of the key that opened
// it.
func (s *sealer) open(purpose, value string, v any) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, errors.New("malformed")
	}
	for i, aead := range s.aeads {
		header := 1 + aead.NonceSize()
		if len(raw) < header+aead.Overhead() || raw[0] != sealVersion {
			return 0, errors.New("malformed")
		}
		plaintext, err := aead.Open(nil, raw[1:header], raw[header:], []byte(purpose))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(plaintext, v); err != nil {
			return 0, errors.New("malformed payload")
		}
		return i, nil
	}
	return 0, errors.New("not sealed by a configured key")
}
