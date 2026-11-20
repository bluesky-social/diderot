package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The bootstrap admin is a fixed local account that lets the first operator
// of a fresh install reach the admin UI before any identity provider or
// token is set up. It is an administrator only while BootstrapAdmin reports
// it enabled.
const (
	BootstrapUsername = "admin"
	bootstrapPassword = "admin"
	// BootstrapSubject and BootstrapEmail stand in for an identity the
	// account does not have. The .invalid domain can never route mail or
	// match a Slack profile.
	BootstrapSubject = "bootstrap:admin"
	BootstrapEmail   = "admin@bootstrap.invalid"
)

var (
	// ErrBootstrapDisabled means the bootstrap admin is turned off.
	ErrBootstrapDisabled = fmt.Errorf("%w: the bootstrap admin is turned off", ErrInvalidCredentials)
	// ErrLocalSignInUnavailable means the requested local method is not
	// configured.
	ErrLocalSignInUnavailable = errors.New("auth: local sign-in method not configured")
)

// BootstrapPrincipal is the bootstrap admin. It carries no groups and no
// verified email; browser surfaces admit it as an administrator because of
// its Bootstrap mark.
func BootstrapPrincipal() *Principal {
	return &Principal{
		Subject: BootstrapSubject, Email: BootstrapEmail,
		Display: "admin (bootstrap)", Bootstrap: true,
	}
}

func (b *BrowserLogin) bootstrapEnabled(ctx context.Context) (bool, error) {
	if b.bootstrap == nil {
		return false, nil
	}
	enabled, err := b.bootstrap.BootstrapAdminEnabled(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: bootstrap admin state: %w", err)
	}
	return enabled, nil
}

// BootstrapEnabled reports whether the bootstrap admin may sign in now.
func (b *BrowserLogin) BootstrapEnabled(ctx context.Context) (bool, error) {
	return b.bootstrapEnabled(ctx)
}

// StaticTokensEnabled reports whether a static bearer token can be pasted to
// sign in.
func (b *BrowserLogin) StaticTokensEnabled() bool { return b.static != nil }

// SignInBootstrap checks the bootstrap admin's credentials and, while the
// account is enabled, sets a bootstrap session.
func (b *BrowserLogin) SignInBootstrap(w http.ResponseWriter, r *http.Request, username, password string) (*Principal, error) {
	if b.bootstrap == nil {
		return nil, ErrLocalSignInUnavailable
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(BootstrapUsername))
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(bootstrapPassword))
	if userOK&passwordOK != 1 {
		return nil, fmt.Errorf("%w: wrong username or password", ErrInvalidCredentials)
	}
	enabled, err := b.bootstrapEnabled(r.Context())
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrBootstrapDisabled
	}
	if err := b.startSession(w, &browserSession{Method: methodBootstrap}); err != nil {
		return nil, err
	}
	return BootstrapPrincipal(), nil
}

// SignInToken sets a session for a static bearer token. The session holds
// the token's SHA-256, never the token.
func (b *BrowserLogin) SignInToken(w http.ResponseWriter, token string) (*Principal, error) {
	if b.static == nil {
		return nil, ErrLocalSignInUnavailable
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	principal, ok := b.static.lookup(digest)
	if !ok {
		return nil, fmt.Errorf("%w: unknown static token", ErrInvalidCredentials)
	}
	if err := b.startSession(w, &browserSession{Method: methodStatic, TokenSHA256: hex.EncodeToString(digest[:])}); err != nil {
		return nil, err
	}
	return principal, nil
}

// startSession stamps session's issue and last-seen times, sets its cookie,
// and drops any OIDC sign-in in progress.
func (b *BrowserLogin) startSession(w http.ResponseWriter, session *browserSession) error {
	now := b.now().Unix()
	session.IssuedAt, session.SeenAt = now, now
	if err := b.setSession(w, session); err != nil {
		return err
	}
	http.SetCookie(w, b.cookie(b.loginCookie, "", -1))
	return nil
}
