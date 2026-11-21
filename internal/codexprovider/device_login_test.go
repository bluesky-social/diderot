package codexprovider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeviceLoginWritesPrivateAuthAfterApproval(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	var polls atomic.Int32
	access := testJWT(t, time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, codexClientID, body["client_id"])
			_, _ = w.Write([]byte(`{"device_auth_id":"test-device","user_code":"TEST-CODE","interval":"0"}`))
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]string{"device_auth_id": "test-device", "user_code": "TEST-CODE"}, body)
			if polls.Add(1) == 1 {
				disk, err := os.ReadFile(path)
				assert.NoError(t, err)
				assert.Equal(t, "{}", string(disk))
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"authorization_code":"test-code","code_verifier":"test-pkce"}`))
		case "/oauth/token":
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, "authorization_code", r.Form.Get("grant_type"))
			assert.Equal(t, codexClientID, r.Form.Get("client_id"))
			assert.Equal(t, "test-code", r.Form.Get("code"))
			assert.Equal(t, "test-pkce", r.Form.Get("code_verifier"))
			assert.Equal(t, "http://"+r.Host+"/deviceauth/callback", r.Form.Get("redirect_uri"))
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "test-refresh"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	require.NoError(t, deviceLogin(t.Context(), server.Client(), server.URL, path, &output))
	require.EqualValues(t, 2, polls.Load())
	require.Contains(t, output.String(), "/codex/device")
	require.Contains(t, output.String(), "TEST-CODE")
	require.NotContains(t, output.String(), access)
	require.NotContains(t, output.String(), "test-refresh")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	credentials, err := (&credentialStore{path: path}).get(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, access, credentials.access)
}
