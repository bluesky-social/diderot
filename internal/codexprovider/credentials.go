package codexprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	codexTokenURL = codexAuthOrigin + "/oauth/token"
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

type credentialStore struct {
	path     string
	client   *http.Client
	tokenURL string
	mu       sync.Mutex
}

type accessCredential struct {
	access  string
	account string
	expires time.Time
}

func (s *credentialStore) get(ctx context.Context, rejected string) (accessCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return accessCredential{}, err
	}
	file, err := os.Open(s.path)
	if err != nil {
		return accessCredential{}, authFailure("could not open the ChatGPT auth file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1<<20))
	file.Close()
	if err != nil {
		return accessCredential{}, authFailure("could not read the ChatGPT auth file")
	}
	var auth, tokens map[string]json.RawMessage
	if json.Unmarshal(contents, &auth) != nil || json.Unmarshal(auth["tokens"], &tokens) != nil || tokens == nil {
		return accessCredential{}, authFailure("the auth file must contain Codex ChatGPT credentials")
	}
	access := jsonString(tokens["access_token"])
	account := jsonString(tokens["account_id"])
	credential, err := credentialFromJWT(access, account)
	if err == nil && access != rejected && time.Until(credential.expires) > time.Minute {
		return credential, nil
	}
	refresh := jsonString(tokens["refresh_token"])
	if refresh == "" {
		return accessCredential{}, authFailure("the ChatGPT access token needs renewal and has no refresh token")
	}
	return s.refresh(ctx, refresh, auth, tokens, account)
}

func credentialFromJWT(access, account string) (accessCredential, error) {
	parts := strings.Split(access, ".")
	if len(parts) != 3 {
		return accessCredential{}, errors.New("invalid access token")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return accessCredential{}, errors.New("invalid access token")
	}
	var claims struct {
		Expires int64 `json:"exp"`
		Auth    struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(body, &claims) != nil || claims.Expires <= 0 {
		return accessCredential{}, errors.New("invalid access token claims")
	}
	// The backend validates the token. The claims only supply routing and a
	// refresh deadline, never authentication to Diderot itself.
	if claims.Auth.Account != "" {
		account = claims.Auth.Account
	}
	if account == "" {
		return accessCredential{}, errors.New("missing ChatGPT account")
	}
	return accessCredential{access: access, account: account, expires: time.Unix(claims.Expires, 0)}, nil
}

func (s *credentialStore) refresh(ctx context.Context, refresh string, auth, tokens map[string]json.RawMessage, account string) (accessCredential, error) {
	// Check that an atomic replacement is possible before making a request
	// that can rotate the refresh token.
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".codex-auth-")
	if err != nil {
		return accessCredential{}, authFailure("the ChatGPT auth file directory must be writable to refresh")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {codexClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return accessCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return accessCredential{}, authFailure("could not reach the ChatGPT token endpoint")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return accessCredential{}, authFailure("ChatGPT token refresh was refused")
	}
	var updated map[string]json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&updated) != nil {
		return accessCredential{}, authFailure("invalid ChatGPT token refresh response")
	}
	credential, err := credentialFromJWT(jsonString(updated["access_token"]), account)
	if err != nil {
		return accessCredential{}, authFailure("ChatGPT token refresh returned an invalid access token")
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if jsonString(updated[key]) != "" {
			tokens[key] = updated[key]
		}
	}
	tokens["account_id"], _ = json.Marshal(credential.account)
	auth["tokens"], _ = json.Marshal(tokens)
	auth["last_refresh"], _ = json.Marshal(time.Now().UTC())
	if err := saveAuthFile(temp, s.path, auth); err != nil {
		return accessCredential{}, err
	}
	return credential, nil
}

func saveAuthFile(temp *os.File, path string, auth map[string]json.RawMessage) error {
	if err := json.NewEncoder(temp).Encode(auth); err != nil {
		return authFailure("could not write the ChatGPT auth file")
	}
	if err := temp.Sync(); err != nil {
		return authFailure("could not sync the ChatGPT auth file")
	}
	if err := temp.Close(); err != nil {
		return authFailure("could not close the ChatGPT auth file")
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return authFailure("could not replace the ChatGPT auth file")
	}
	return nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func authFailure(message string) *backendError {
	return &backendError{http.StatusBadGateway, message}
}
