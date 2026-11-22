package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	oauthTokenURL = "https://api.linear.app/oauth/token"

	// oauthScope is the one scope the connector asks for: it only reads.
	// Linear keeps an application's tokens of the same scopes valid side by
	// side and invalidates them when one is requested with other scopes, so
	// every replica and every restart must ask for the same value.
	oauthScope = "read"
)

// oauthTokenProvider implements TokenProvider for a Linear OAuth
// application. It signs in with the client-credentials grant, so its token
// acts as the application and depends on no member's account.
type oauthTokenProvider struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client
}

// newOAuthTokenProvider returns a provider for the application with this
// client ID and secret. httpClient sends the token requests; it must refuse
// redirects, which would carry the secret to another host.
func newOAuthTokenProvider(clientID, clientSecret string, httpClient *http.Client) *oauthTokenProvider {
	return &oauthTokenProvider{clientID: clientID, clientSecret: clientSecret, httpClient: httpClient}
}

func (p *oauthTokenProvider) Authorization(ctx context.Context) (string, error) {
	token, err := p.mint(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

// mint requests a token from Linear with the client-credentials grant.
func (p *oauthTokenProvider) mint(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {oauthScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("linear: OAuth token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.clientID, p.clientSecret)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("linear: OAuth token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var grant struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		return "", fmt.Errorf("linear: OAuth token answer: %w", err)
	}
	return grant.AccessToken, nil
}
