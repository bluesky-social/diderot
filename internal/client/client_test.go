package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://diderot.example.com":         "https://diderot.example.com/mcp",
		"https://diderot.example.com/":        "https://diderot.example.com/mcp",
		"https://gw.example.com/diderot/mcp":  "https://gw.example.com/diderot/mcp",
		"http://localhost:9091":               "http://localhost:9091/mcp",
		"http://127.0.0.1:9091/mcp?x=1#frag":  "http://127.0.0.1:9091/mcp",
		" https://diderot.example.com/mcp \n": "https://diderot.example.com/mcp",
	} {
		got, err := NormalizeURL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"diderot.example.com", "ftp://x.example.com", "http://diderot.example.com"} {
		_, err := NormalizeURL(bad)
		assert.Error(t, err, bad)
	}
}

func TestResponseForJSONAndEventStream(t *testing.T) {
	result, err := readResponse("application/json", strings.NewReader(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`), 7)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(result))

	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\n" +
		"data: \"result\":{\"ok\":1}}\n\n"
	result, err = readResponse("text/event-stream; charset=utf-8", strings.NewReader(stream), 7)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":1}`, string(result))

	_, err = readResponse("application/json", strings.NewReader(`{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"unknown task"}}`), 7)
	var rpcErr *RPCError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, "unknown task", rpcErr.Message)

	_, err = readResponse("text/event-stream", strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}\n\n"), 7)
	assert.ErrorContains(t, err, "ended without a response")
}

// A server may keep an event stream open after the response; the call
// returns at the response, not when the stream ends.
func TestCallReturnsAtTheResponseOfAnOpenStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"ask\"}]}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	m := &MCP{URL: srv.URL, HTTP: NewHTTPClient()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tools, err := m.Tools(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"ask"}, tools)
}

// A redirect to another origin would carry the credential there; it is
// refused before anything is sent.
func TestCallRefusesARedirectElsewhere(t *testing.T) {
	var leaked atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "")
	}))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	m := &MCP{URL: srv.URL + "/mcp", HTTP: NewHTTPClient(), Authorization: func(context.Context) (string, error) {
		return "Bearer secret", nil
	}}
	_, err := m.Tools(t.Context())
	assert.ErrorContains(t, err, "refusing a redirect")
	assert.False(t, leaked.Load())
}

func TestDiscoverTellsTheSignInApart(t *testing.T) {
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
	}))
	t.Cleanup(open.Close)
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="diderot"`)
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
	}))
	t.Cleanup(token.Close)
	missing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	protected := newFakeProtected(t)

	hc := NewHTTPClient()
	disc, err := Discover(t.Context(), hc, open.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, AuthNone, disc.Auth)

	disc, err = Discover(t.Context(), hc, token.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, AuthToken, disc.Auth)

	disc, err = Discover(t.Context(), hc, protected.url())
	require.NoError(t, err)
	assert.Equal(t, AuthOAuth, disc.Auth)
	assert.Equal(t, []string{protected.server.URL + "/idp"}, disc.Resource.AuthorizationServers)

	_, err = Discover(t.Context(), hc, missing.URL+"/mcp")
	assert.ErrorContains(t, err, "did not answer as an MCP server")
}

func signIn(t *testing.T, f *fakeProtected, clientID string) (*OAuth, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return SignInOAuth(ctx, SignInOptions{MCPURL: f.url(), ClientID: clientID, HTTP: NewHTTPClient(), Open: browser(t), Status: io.Discard})
}

func TestSignInOAuth(t *testing.T) {
	t.Run("the identity provider registers clients", func(t *testing.T) {
		f := newFakeProtected(t)
		f.idpRegisters = true
		oauth, account, err := signIn(t, f, "")
		require.NoError(t, err)
		assert.Equal(t, "dcr-1", oauth.ClientID)
		assert.Empty(t, oauth.HarnessClientID, "each harness registers its own client")
		assert.Equal(t, "person@example.com", account)
		assert.Equal(t, f.server.URL+"/idp/token", oauth.TokenURL)
		assert.NotEmpty(t, oauth.Token.RefreshToken)
		assert.Equal(t, []string{f.url()}, f.resources, "the authorization request names the resource")
	})

	t.Run("a gateway answers registration with its public client", func(t *testing.T) {
		f := newFakeProtected(t)
		f.gatewayClient = "gateway-public"
		f.allowClient("gateway-public")
		oauth, _, err := signIn(t, f, "")
		require.NoError(t, err)
		assert.Equal(t, "gateway-public", oauth.ClientID)
		assert.Equal(t, "gateway-public", oauth.HarnessClientID, "harnesses get the same client")
		assert.Equal(t, f.server.URL+"/idp", oauth.Issuer)
	})

	t.Run("nothing registers, and no client ID is given", func(t *testing.T) {
		f := newFakeProtected(t)
		_, _, err := signIn(t, f, "")
		assert.ErrorContains(t, err, "--client-id")
	})

	t.Run("an administrator's client ID", func(t *testing.T) {
		f := newFakeProtected(t)
		f.allowClient("admin-given")
		oauth, _, err := signIn(t, f, "admin-given")
		require.NoError(t, err)
		assert.Equal(t, "admin-given", oauth.ClientID)
		assert.Equal(t, "admin-given", oauth.HarnessClientID)
	})

	t.Run("a client the identity provider does not know", func(t *testing.T) {
		f := newFakeProtected(t)
		_, _, err := signIn(t, f, "unknown")
		assert.ErrorContains(t, err, "invalid_client")
	})
}

// A token that is about to expire is refreshed once, under the lock, and
// the rotated refresh token is saved: two processes refreshing at once
// would spend the same refresh token and sign the person out.
func TestRefreshRotatesOnceAndSaves(t *testing.T) {
	f := newFakeProtected(t)
	f.gatewayClient = "gateway-public"
	f.allowClient("gateway-public")
	oauth, _, err := signIn(t, f, "")
	require.NoError(t, err)
	oauth.Token.Expiry = time.Now().Add(10 * time.Second) // inside the refresh margin
	path := filepath.Join(t.TempDir(), "diderot", "client.json")
	require.NoError(t, SaveConfig(path, &Config{URL: f.url(), Auth: AuthOAuth, OAuth: oauth}))

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := LoadConfig(path)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = Authorization(t.Context(), path, cfg)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, f.refreshes, "one refresh for every process")
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, f.refresh, saved.OAuth.Token.RefreshToken, "the rotated refresh token is saved")

	m := &MCP{URL: f.url(), HTTP: NewHTTPClient(), Authorization: func(ctx context.Context) (string, error) {
		return Authorization(ctx, path, saved)
	}}
	tools, err := m.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"retrieve", "ask"}, tools)
}

func TestExpiredSignInWithoutRefreshIsASignInError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	cfg := &Config{URL: "https://diderot.example.com/mcp", Auth: AuthOAuth, OAuth: &OAuth{
		ClientID: "c", TokenURL: "https://idp.example.com/token",
		Token: &oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(-time.Hour)},
	}}
	require.NoError(t, SaveConfig(path, cfg))
	_, err := Authorization(t.Context(), path, cfg)
	var signIn *SignInError
	assert.True(t, errors.As(err, &signIn), "%v", err)
}

func TestAccountFromJWT(t *testing.T) {
	assert.Equal(t, "person@example.com", AccountFromJWT(fakeJWT("person@example.com", 1)))
	assert.Empty(t, AccountFromJWT("opaque-token"))
}
