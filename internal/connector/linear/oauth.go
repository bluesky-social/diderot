package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	oauthTokenURL = "https://api.linear.app/oauth/token"

	// oauthScope is the one scope the connector asks for: it only reads.
	// Linear keeps an application's tokens of the same scopes valid side by
	// side and invalidates them when one is requested with other scopes, so
	// every replica and every restart must ask for the same value.
	oauthScope = "read"

	// oauthRenewMargin is how long before its expiry a token is replaced.
	// Linear issues no refresh token, and the only sign of an expired one is
	// the API's answer to a request that carried it. Replacing the token
	// first keeps the connector from depending on that answer being a 401.
	// An hour is longer than any run of requests the connector makes and
	// nothing against the token's 30 days.
	oauthRenewMargin = time.Hour

	// maxTokenAnswerBytes bounds how much of a token answer is read. A real
	// one is a few hundred bytes.
	maxTokenAnswerBytes = 64 << 10
)

// errTokenAnswer is a token endpoint answer the provider can't use. What
// wraps it names the reason and never quotes the answer, which holds a
// token.
var errTokenAnswer = errors.New("linear: unusable OAuth token answer")

// tokenStatusError is a token request Linear answered with an HTTP error.
type tokenStatusError struct{ status int }

func (e *tokenStatusError) Error() string {
	return fmt.Sprintf("linear: OAuth token request: HTTP %d", e.status)
}

// oauthTokenProvider implements TokenProvider for a Linear OAuth
// application. It signs in with the client-credentials grant, so its token
// acts as the application and depends on no member's account.
type oauthTokenProvider struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	// mu serializes callers, a token request included, so concurrent
	// callers that find no token make one request between them.
	mu    sync.Mutex
	token string
	// renewAt is when token is due to be replaced: oauthRenewMargin before
	// it expires.
	renewAt time.Time
}

// newOAuthTokenProvider returns a provider for the application with this
// client ID and secret. httpClient sends the token requests.
func newOAuthTokenProvider(clientID, clientSecret string, httpClient *http.Client) *oauthTokenProvider {
	return &oauthTokenProvider{clientID: clientID, clientSecret: clientSecret, httpClient: httpClient}
}

func (p *oauthTokenProvider) Authorization(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token == "" || !time.Now().Before(p.renewAt) {
		// The lifetime counts from before the request, so the estimate of
		// the expiry errs early.
		requested := time.Now()
		token, lifetime, err := p.mint(ctx)
		if err != nil {
			return "", err
		}
		p.token = token
		p.renewAt = requested.Add(lifetime - oauthRenewMargin)
	}
	return "Bearer " + p.token, nil
}

// mint requests a token from Linear with the client-credentials grant and
// returns it with the lifetime Linear gives it.
func (p *oauthTokenProvider) mint(ctx context.Context) (token string, lifetime time.Duration, err error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {oauthScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("linear: OAuth token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.clientID, p.clientSecret)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("linear: OAuth token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The status alone: an error answer's body is Linear's to word and
		// may repeat what the request carried.
		return "", 0, &tokenStatusError{status: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswerBytes))
	if err != nil {
		return "", 0, fmt.Errorf("linear: OAuth token request: read answer: %w", err)
	}
	var grant struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	switch err := json.Unmarshal(body, &grant); {
	case err != nil:
		// The decoder's error is left out: it quotes the answer.
		return "", 0, fmt.Errorf("%w: not JSON", errTokenAnswer)
	case grant.AccessToken == "":
		return "", 0, fmt.Errorf("%w: no access token", errTokenAnswer)
	case !strings.EqualFold(grant.TokenType, "Bearer"):
		// Any other type would need more than an Authorization value.
		return "", 0, fmt.Errorf("%w: token type is not Bearer", errTokenAnswer)
	case grant.ExpiresIn <= 0:
		// Without a lifetime every call would find the token due for
		// replacement and request another.
		return "", 0, fmt.Errorf("%w: no lifetime", errTokenAnswer)
	}
	return grant.AccessToken, time.Duration(grant.ExpiresIn) * time.Second, nil
}

// Invalidate drops the held token, which Linear answered with a 401: it
// expired or was revoked. The next Authorization requests another.
func (p *oauthTokenProvider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = ""
}
