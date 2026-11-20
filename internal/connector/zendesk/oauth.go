package zendesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// errInvalidGrant is a token exchange whose answer is not a read-only bearer
// grant.
var errInvalidGrant = errors.New("zendesk: invalid read-only OAuth grant")

// oauthStatusError is a token exchange Zendesk answered with an HTTP error.
type oauthStatusError struct{ status int }

func (err *oauthStatusError) Error() string {
	return fmt.Sprintf("zendesk: OAuth token exchange HTTP %d", err.status)
}

func (client *Client) accessToken(ctx context.Context) (string, error) {
	client.tokenMu.Lock()
	defer client.tokenMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if client.token != "" && time.Now().Add(30*time.Second).Before(client.tokenExpiry) {
		return client.token, nil
	}
	payload, err := json.Marshal(struct {
		GrantType    string `json:"grant_type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
	}{"client_credentials", client.clientID, client.clientSecret, "read", 1800})
	if err != nil {
		return "", errors.New("zendesk: invalid OAuth request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.origin+"/oauth/tokens", bytes.NewReader(payload))
	if err != nil {
		return "", errors.New("zendesk: invalid OAuth endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	started := time.Now()
	response, err := client.send(request, client.httpClient)
	if err != nil {
		return "", errors.New("zendesk: OAuth transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", &oauthStatusError{status: response.StatusCode}
	}
	const maximumBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil || len(body) > maximumBytes {
		return "", errors.New("zendesk: invalid OAuth response")
	}
	var grant struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &grant) != nil || grant.AccessToken == "" || strings.ContainsAny(grant.AccessToken, " \t\r\n") ||
		!strings.EqualFold(grant.TokenType, "bearer") || grant.Scope != "read" || grant.ExpiresIn <= 30 || grant.ExpiresIn > 172800 {
		return "", errInvalidGrant
	}
	client.token = grant.AccessToken
	client.tokenExpiry = started.Add(time.Duration(min(grant.ExpiresIn, 1800)) * time.Second)
	return client.token, nil
}

func (client *Client) do(request *http.Request, transport *http.Client, authenticated bool) (*http.Response, error) {
	if !authenticated {
		return client.send(request, transport)
	}
	if request.Method != http.MethodGet || request.URL.Scheme+"://"+request.URL.Host != client.origin {
		return nil, errors.New("zendesk: authenticated request outside read-only tenant boundary")
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := client.accessToken(request.Context())
		if err != nil {
			return nil, err
		}
		outbound := request.Clone(request.Context())
		outbound.Header.Set("Authorization", "Bearer "+token)
		response, err := client.send(outbound, transport)
		if err != nil || response.StatusCode != http.StatusUnauthorized || response.Request == nil || response.Request.Header.Get("Authorization") != "Bearer "+token {
			return response, err
		}
		client.tokenMu.Lock()
		if client.token == token {
			client.token = ""
			client.tokenExpiry = time.Time{}
		}
		client.tokenMu.Unlock()
		if attempt == 1 {
			return response, nil
		}
		_ = response.Body.Close()
	}
	return nil, errors.New("zendesk: OAuth retry limit reached")
}
