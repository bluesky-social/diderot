package codexprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
)

const (
	codexAuthOrigin = "https://auth.openai.com"
	deviceTimeout   = 15 * time.Minute
	maxLoginReply   = 1 << 20
)

type deviceChallenge struct {
	ID       string          `json:"device_auth_id"`
	UserCode string          `json:"user_code"`
	Interval json.RawMessage `json:"interval"`
}

type deviceGrant struct {
	Code     string `json:"authorization_code"`
	Verifier string `json:"code_verifier"`
}

// DeviceLogin authorizes this adapter with ChatGPT on a headless machine.
// Only the verification URL and short-lived device code go to output.
func DeviceLogin(ctx context.Context, path string, output io.Writer) error {
	client := safehttp.NewClient(safehttp.Options{Timeout: 25 * time.Second, FailOnRedirect: true})
	return deviceLogin(ctx, client, codexAuthOrigin, path, output)
}

func deviceLogin(ctx context.Context, client *http.Client, issuer, path string, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()

	// Reserve a private, writable file before asking the user to sign in.
	temp, err := os.CreateTemp(filepath.Dir(path), ".codex-auth-")
	if err != nil {
		return errors.New("the Codex credentials directory must be writable")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	const devicePath = "/api/accounts/deviceauth/"
	payload, _ := json.Marshal(map[string]string{"client_id": codexClientID})
	var challenge deviceChallenge
	status, _, err := loginPost(ctx, client, issuer+devicePath+"usercode", "application/json", payload, &challenge)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ChatGPT refused the device code request (HTTP %d)", status)
	}
	seconds, err := strconv.Atoi(strings.Trim(string(challenge.Interval), `" `))
	if err != nil || seconds < 0 || seconds > int(deviceTimeout.Seconds()) ||
		challenge.ID == "" || challenge.UserCode == "" || len(strings.Fields(challenge.UserCode)) != 1 ||
		strings.ContainsAny(challenge.UserCode, "\r\n\t\x1b") {
		return errors.New("ChatGPT returned an invalid device code response")
	}
	if _, err := fmt.Fprintf(output, "Open %s/codex/device and enter code %s.\n", issuer, challenge.UserCode); err != nil {
		return err
	}
	payload, _ = json.Marshal(map[string]string{"device_auth_id": challenge.ID, "user_code": challenge.UserCode})
	grant, err := waitForDeviceGrant(ctx, client, issuer+devicePath+"token", payload, time.Duration(max(1, seconds))*time.Second)
	if err != nil {
		return err
	}

	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {codexClientID},
		"code": {grant.Code}, "code_verifier": {grant.Verifier}, "redirect_uri": {issuer + "/deviceauth/callback"},
	}
	var tokens map[string]json.RawMessage
	status, _, err = loginPost(ctx, client, issuer+"/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()), &tokens)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ChatGPT refused the authorization exchange (HTTP %d)", status)
	}
	credential, err := credentialFromJWT(jsonString(tokens["access_token"]), "")
	if err != nil || jsonString(tokens["refresh_token"]) == "" {
		return errors.New("ChatGPT returned incomplete Codex credentials")
	}
	tokens["account_id"], _ = json.Marshal(credential.account)
	auth := map[string]json.RawMessage{}
	auth["auth_mode"] = json.RawMessage(`"chatgpt"`)
	auth["tokens"], _ = json.Marshal(tokens)
	auth["last_refresh"], _ = json.Marshal(time.Now().UTC())
	return saveAuthFile(temp, path, auth)
}

func waitForDeviceGrant(ctx context.Context, client *http.Client, endpoint string, payload []byte, interval time.Duration) (deviceGrant, error) {
	for {
		if err := sleepContext(ctx, interval); err != nil {
			return deviceGrant{}, err
		}
		var grant deviceGrant
		status, reason, err := loginPost(ctx, client, endpoint, "application/json", payload, &grant)
		if err != nil {
			return deviceGrant{}, err
		}
		switch {
		case status == http.StatusOK:
			if grant.Code == "" || grant.Verifier == "" {
				return deviceGrant{}, errors.New("ChatGPT returned an incomplete device authorization")
			}
			return grant, nil
		case status == http.StatusForbidden || status == http.StatusNotFound || reason == "deviceauth_authorization_pending":
			// ChatGPT uses these responses while waiting for approval.
		case status == http.StatusTooManyRequests || reason == "slow_down":
			interval += 5 * time.Second
		default:
			return deviceGrant{}, fmt.Errorf("ChatGPT device authorization failed (HTTP %d)", status)
		}
	}
}

func sleepContext(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func loginPost(ctx context.Context, client *http.Client, endpoint, contentType string, payload []byte, reply any) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", contentType)
	response, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("contact ChatGPT: %w", err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxLoginReply))
	if response.StatusCode == http.StatusOK {
		if err := decoder.Decode(reply); err != nil {
			return 0, "", errors.New("ChatGPT returned an invalid login response")
		}
		return response.StatusCode, "", nil
	}
	var failure struct {
		Error json.RawMessage `json:"error"`
	}
	_ = decoder.Decode(&failure)
	reason := jsonString(failure.Error)
	if reason == "" {
		var detail struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(failure.Error, &detail)
		reason = detail.Code
	}
	return response.StatusCode, reason, nil
}
