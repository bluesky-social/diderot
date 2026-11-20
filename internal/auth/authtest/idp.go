// Package authtest runs a minimal OIDC identity provider for tests: a
// discovery document, a JWKS endpoint, an RS256 signer for arbitrary claims,
// and an authorization-code + PKCE flow for registered clients.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const keyID = "authtest"

type IdP struct {
	// Issuer is the server's own origin, as served in its discovery document.
	Issuer  string
	JWKSURL string
	signer  jose.Signer

	mu      sync.Mutex
	clients map[string]string
	codes   map[string]grant
	user    map[string]any
	rewrite func(map[string]any)
	// accessAudience, when set, makes /token issue JWT access tokens for
	// that audience instead of opaque ones.
	accessAudience string
}

// grant is one issued authorization code and what /token must match.
type grant struct {
	clientID    string
	redirectURI string
	challenge   string
	nonce       string
	claims      map[string]any
}

// NewIdP starts an IdP that lives for the duration of t.
func NewIdP(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	idp := &IdP{signer: signer, clients: map[string]string{}, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.Issuer,
			"jwks_uri":                              idp.JWKSURL,
			"authorization_endpoint":                idp.Issuer + "/authorize",
			"token_endpoint":                        idp.Issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	mux.HandleFunc("GET /authorize", idp.authorize)
	mux.HandleFunc("POST /token", idp.token)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	idp.Issuer = server.URL
	idp.JWKSURL = server.URL + "/jwks"
	return idp
}

// Claims returns a valid claim set for issuer and audience, expiring in an
// hour. Callers add or override identity claims.
func Claims(issuer, audience, subject string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": issuer, "aud": audience, "sub": subject,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// Sign returns claims as a compact RS256 JWT signed by the IdP's key.
func (i *IdP) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	token, err := i.sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (i *IdP) sign(claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signed, err := i.signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return signed.CompactSerialize()
}

// RegisterClient admits clientID to the authorization-code flow. An empty
// secret registers a public client.
func (i *IdP) RegisterClient(clientID, secret string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clients[clientID] = secret
}

// SetUser sets the identity /authorize signs in: ID-token claims such as
// sub, email, email_verified, and groups. iss, aud, iat, exp, and nonce are
// added at /token. Nil makes /authorize deny the request (access_denied).
func (i *IdP) SetUser(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.user = maps.Clone(claims)
}

// IssueAccessTokens makes /token issue JWT access tokens whose aud is
// audience and whose claims are the signed-in user's, as an IdP does for an
// API it protects. Without it access tokens are opaque.
func (i *IdP) IssueAccessTokens(audience string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.accessAudience = audience
}

// RewriteIDToken installs fn to edit every later ID token's claims just
// before signing, for tests of tampered or mismatched tokens. Nil removes it.
func (i *IdP) RewriteIDToken(fn func(claims map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rewrite = fn
}

// authorize auto-approves the configured user and redirects to the client
// with a one-time code, as a browser would be after signing in.
func (i *IdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.mu.Lock()
	_, known := i.clients[q.Get("client_id")]
	user := maps.Clone(i.user)
	i.mu.Unlock()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	switch {
	case !known:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case err != nil || !redirect.IsAbs():
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	case q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		http.Error(w, "unsupported authorization request", http.StatusBadRequest)
		return
	}
	params := redirect.Query()
	params.Set("state", q.Get("state"))
	if user == nil {
		params.Set("error", "access_denied")
	} else {
		code := randomString()
		i.mu.Lock()
		i.codes[code] = grant{
			clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
			challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), claims: user,
		}
		i.mu.Unlock()
		params.Set("code", code)
	}
	redirect.RawQuery = params.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token redeems a code once, checking the client, redirect URI, and PKCE
// verifier the way a conforming IdP does.
func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	clientID, secret, basic := r.BasicAuth()
	if basic {
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	} else {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	i.mu.Lock()
	wantSecret, known := i.clients[clientID]
	code := r.PostForm.Get("code")
	g, issued := i.codes[code]
	delete(i.codes, code)
	rewrite := i.rewrite
	accessAudience := i.accessAudience
	i.mu.Unlock()
	if !known || subtle.ConstantTimeCompare([]byte(secret), []byte(wantSecret)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	challenge := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("grant_type") != "authorization_code" || !issued || g.clientID != clientID ||
		g.redirectURI != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(challenge[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	claims := Claims(i.Issuer, clientID, "")
	delete(claims, "sub")
	for k, v := range g.claims {
		claims[k] = v
	}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	if rewrite != nil {
		rewrite(claims)
	}
	idToken, err := i.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	accessToken := randomString()
	if accessAudience != "" {
		access := Claims(i.Issuer, accessAudience, "")
		delete(access, "sub")
		for k, v := range g.claims {
			access[k] = v
		}
		if accessToken, err = i.sign(access); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": accessToken, "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func randomString() string {
	return rand.Text()
}
