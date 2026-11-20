package client

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeProtected is an MCP endpoint behind OAuth, with its own identity
// provider, built the ways the client must cope with: an identity provider
// that registers clients itself, one that does not but sits behind a
// gateway that answers registration with a fixed public client, and one
// that offers neither.
type fakeProtected struct {
	t      *testing.T
	server *httptest.Server
	// idpRegisters makes the identity provider offer dynamic registration.
	idpRegisters bool
	// gatewayClient, when set, is the client the MCP URL's own registration
	// endpoint answers with.
	gatewayClient string

	mu        sync.Mutex
	clients   map[string]bool
	codes     map[string]pendingCode
	access    string
	refresh   string
	refreshes int
	issued    int
	resources []string
}

type pendingCode struct {
	clientID, challenge, redirect string
}

func newFakeProtected(t *testing.T) *fakeProtected {
	f := &fakeProtected{t: t, clients: map[string]bool{}, codes: map[string]pendingCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", f.mcp)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"resource": f.url(), "authorization_servers": []string{f.server.URL + "/idp"},
			"scopes_supported": []string{"openid", "email"},
		})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/idp", func(w http.ResponseWriter, _ *http.Request) {
		meta := f.asMeta(f.server.URL + "/idp")
		if f.idpRegisters {
			meta["registration_endpoint"] = f.server.URL + "/idp/register"
		}
		writeJSON(w, meta)
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", func(w http.ResponseWriter, _ *http.Request) {
		if f.gatewayClient == "" {
			http.NotFound(w, nil)
			return
		}
		meta := f.asMeta(f.url())
		meta["registration_endpoint"] = f.server.URL + "/gateway/register"
		writeJSON(w, meta)
	})
	mux.HandleFunc("POST /idp/register", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		id := fmt.Sprintf("dcr-%d", len(f.clients)+1)
		f.clients[id] = true
		f.mu.Unlock()
		writeJSONStatus(w, http.StatusCreated, map[string]any{"client_id": id, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("POST /gateway/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusCreated, map[string]any{"client_id": f.gatewayClient, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("GET /idp/authorize", f.authorize)
	mux.HandleFunc("POST /idp/token", f.token)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeProtected) url() string { return f.server.URL + "/mcp" }

// allowClient registers a public client at the identity provider, as an
// administrator would.
func (f *fakeProtected) allowClient(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[id] = true
}

func (f *fakeProtected) asMeta(issuer string) map[string]any {
	return map[string]any{
		"issuer": issuer, "authorization_endpoint": f.server.URL + "/idp/authorize",
		"token_endpoint": f.server.URL + "/idp/token", "response_types_supported": []string{"code"},
		"code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"openid", "email", "offline_access"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	}
}

func (f *fakeProtected) mcp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ok := f.access != "" && r.Header.Get("Authorization") == "Bearer "+f.access
	f.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.server.URL+`/.well-known/oauth-protected-resource/mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var result any
	switch req.Method {
	case "server/discover":
		result = map[string]any{"supportedVersions": []string{ProtocolVersion}, "capabilities": map[string]any{}}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{"name": "retrieve"}, {"name": "ask"}}}
	default:
		result = map[string]any{}
	}
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// authorize stands in for the person approving in the browser: it answers
// at once with a code bound to the PKCE challenge.
func (f *fakeProtected) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	known := f.clients[q.Get("client_id")]
	f.resources = append(f.resources, q.Get("resource"))
	code := fmt.Sprintf("code-%d", len(f.codes)+1)
	f.codes[code] = pendingCode{clientID: q.Get("client_id"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || !strings.HasPrefix(redirect.Host, "127.0.0.1:") || redirect.Path != "/callback" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	if known {
		back.Set("code", code)
	} else {
		back.Set("error", "invalid_client")
	}
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *fakeProtected) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		pending, ok := f.codes[r.Form.Get("code")]
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || pending.clientID != r.Form.Get("client_id") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != pending.challenge || pending.redirect != r.Form.Get("redirect_uri") {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		delete(f.codes, r.Form.Get("code"))
	case "refresh_token":
		if f.refresh == "" || r.Form.Get("refresh_token") != f.refresh {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		f.refreshes++
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Each grant rotates both tokens, as identity providers that rotate
	// refresh tokens do: the old refresh token stops working.
	f.issued++
	f.access = fakeJWT("person@example.com", f.issued)
	f.refresh = fmt.Sprintf("refresh-%d", f.issued)
	writeJSON(w, map[string]any{"access_token": f.access, "token_type": "Bearer", "refresh_token": f.refresh, "expires_in": 3600})
}

func fakeJWT(email string, n int) string {
	payload, _ := json.Marshal(map[string]any{"email": email, "n": n})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// browser follows the authorization URL the way a browser would, to the
// loopback redirect.
func browser(t *testing.T) func(string) error {
	return func(u string) error {
		go func() {
			resp, err := http.Get(u) //nolint:gosec,noctx // the test's own server
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
		return nil
	}
}
