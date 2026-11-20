package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// SignInError means the server needs a sign-in the client does not have: no
// credential, a refused one, or an OAuth token that can no longer refresh.
type SignInError struct {
	Reason string
}

func (e *SignInError) Error() string { return e.Reason }

// refreshMargin renews an access token this long before it expires, so a
// request does not carry a token that lapses in flight.
const refreshMargin = time.Minute

// Authorization returns the Authorization header value for cfg, refreshing
// and saving an OAuth token at path when it is about to expire. It is empty
// for a server that takes no credential.
func Authorization(ctx context.Context, path string, cfg *Config) (string, error) {
	switch cfg.Auth {
	case AuthNone:
		return "", nil
	case AuthToken:
		if cfg.Token == "" {
			return "", &SignInError{Reason: "no API token is saved"}
		}
		return "Bearer " + cfg.Token, nil
	case AuthOAuth:
		tok, err := oauthToken(ctx, path, cfg)
		if err != nil {
			return "", err
		}
		return "Bearer " + tok.AccessToken, nil
	default:
		return "", fmt.Errorf("unknown sign-in kind %q in %s", cfg.Auth, path)
	}
}

func usable(tok *oauth2.Token) bool {
	if tok == nil || tok.AccessToken == "" {
		return false
	}
	return tok.Expiry.IsZero() || time.Until(tok.Expiry) > refreshMargin
}

func oauthToken(ctx context.Context, path string, cfg *Config) (*oauth2.Token, error) {
	if cfg.OAuth == nil {
		return nil, &SignInError{Reason: "no OAuth sign-in is saved"}
	}
	if usable(cfg.OAuth.Token) {
		return cfg.OAuth.Token, nil
	}
	unlock, err := lockConfig(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Another process may have refreshed while this one waited for the lock;
	// its rotated refresh token is the only one still valid.
	current, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if current.OAuth == nil || current.URL != cfg.URL {
		return nil, &SignInError{Reason: "the saved sign-in changed while refreshing"}
	}
	*cfg = *current
	if usable(cfg.OAuth.Token) {
		return cfg.OAuth.Token, nil
	}
	if cfg.OAuth.Token == nil || cfg.OAuth.Token.RefreshToken == "" {
		return nil, &SignInError{Reason: "the sign-in expired"}
	}
	oc := &oauth2.Config{
		ClientID:     cfg.OAuth.ClientID,
		ClientSecret: cfg.OAuth.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: cfg.OAuth.TokenURL, AuthStyle: cfg.OAuth.AuthStyle},
	}
	// Only the refresh token: oauth2 would otherwise reuse the access token.
	// The bounded client keeps a token endpoint that stops answering from
	// holding the lock, and every other diderot process, forever.
	refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, NewHTTPClient())
	refreshed, err := oc.TokenSource(refreshCtx, &oauth2.Token{RefreshToken: cfg.OAuth.Token.RefreshToken}).Token()
	if err != nil {
		return nil, &SignInError{Reason: "the sign-in expired and could not be renewed: " + err.Error()}
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = cfg.OAuth.Token.RefreshToken
	}
	cfg.OAuth.Token = &oauth2.Token{
		AccessToken: refreshed.AccessToken, TokenType: refreshed.TokenType,
		RefreshToken: refreshed.RefreshToken, Expiry: refreshed.Expiry,
	}
	// Saved before use: the old refresh token is spent.
	if err := SaveConfig(path, cfg); err != nil {
		return nil, fmt.Errorf("save the renewed sign-in: %w", err)
	}
	return cfg.OAuth.Token, nil
}

// AccountFromJWT returns the email claim of a JWT, read without verifying it,
// to name who is signed in. It is empty for anything else.
func AccountFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}
