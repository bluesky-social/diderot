package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// NewHTTPClient is the client every request goes through. Its transport sets
// its own dialer, which keeps the Go MCP SDK's discovery from refusing
// private addresses: those checks guard servers against URLs a remote party
// chose, while here the person running the command chose the URL, and an
// intranet gateway is a normal place for it. It follows a redirect only to
// the same scheme and host, so a credential never leaves for another origin
// or over plain http.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Transport: transport,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing a redirect from %s://%s to %s://%s", via[0].URL.Scheme, via[0].URL.Host, req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
}

// NormalizeURL checks a server or gateway URL and gives it the /mcp path when
// it has none. Plain http is accepted only for a loopback host, since the
// URL receives credentials.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("%q uses plain http; use https (plain http is only for a server on this machine)", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/mcp"
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Discovery is what an unauthenticated request to the MCP URL revealed about
// how to sign in.
type Discovery struct {
	Auth     string
	Resource *oauthex.ProtectedResourceMetadata // AuthOAuth only
}

// Discover sends one unauthenticated MCP request to mcpURL and works out the
// sign-in: none when it is answered, OAuth when the 401 leads to protected
// resource metadata (RFC 9728, as MCP authorization discovery specifies),
// and otherwise a bearer token the server issues.
func Discover(ctx context.Context, hc *http.Client, mcpURL string) (*Discovery, error) {
	_, resp, err := probe(ctx, hc, mcpURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return &Discovery{Auth: AuthNone}, nil
	case http.StatusUnauthorized:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s did not answer as an MCP server: %w", mcpURL, &HTTPError{Status: resp.StatusCode, Body: firstLine(body)})
	}
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if prm := resourceMetadata(ctx, hc, challenges, mcpURL); prm != nil {
		return &Discovery{Auth: AuthOAuth, Resource: prm}, nil
	}
	return &Discovery{Auth: AuthToken}, nil
}

// probe sends server/discover without credentials.
func probe(ctx context.Context, hc *http.Client, mcpURL string) (*http.Request, *http.Response, error) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"` + ProtocolVersion + `",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"diderot","version":"setup"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", "server/discover")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("reach %s: %w", mcpURL, err)
	}
	return req, resp, nil
}

// resourceMetadata looks for protected resource metadata where MCP
// authorization discovery says to: the challenge's resource_metadata, then
// the well-known path for the MCP endpoint, then the root.
func resourceMetadata(ctx context.Context, hc *http.Client, challenges []oauthex.Challenge, mcpURL string) *oauthex.ProtectedResourceMetadata {
	type candidate struct{ metadata, resource string }
	var candidates []candidate
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			candidates = append(candidates, candidate{u, mcpURL})
		}
	}
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil
	}
	m := *u
	m.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(u.Path, "/")
	candidates = append(candidates, candidate{m.String(), mcpURL})
	m.Path = "/.well-known/oauth-protected-resource"
	root := *u
	root.Path = ""
	candidates = append(candidates, candidate{m.String(), root.String()})
	for _, c := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c.metadata, c.resource, hc)
		if err == nil && prm != nil && len(prm.AuthorizationServers) > 0 {
			return prm
		}
	}
	return nil
}

// SignInOptions configures an OAuth sign-in.
type SignInOptions struct {
	MCPURL string
	// ClientID is an administrator-given public client, used instead of
	// registering one.
	ClientID string
	HTTP     *http.Client
	// Open shows the authorization page to the person; nil opens the
	// system browser.
	Open func(url string) error
	// Status receives the lines a person reads while signing in.
	Status io.Writer
}

// SignInOAuth runs the authorization code flow with PKCE against the
// authorization server the protected resource metadata names, through a
// loopback redirect. It registers a client by dynamic client registration
// where the authorization server offers it; where it does not, it registers
// with the MCP URL's own authorization server metadata (the MCP server as
// its own authorization server, as a gateway in front of an identity
// provider presents itself) and signs in with the client ID that returns.
// It returns the account the sign-in names, when the tokens say.
func SignInOAuth(ctx context.Context, opts SignInOptions) (*OAuth, string, error) {
	if opts.Open == nil {
		opts.Open = openBrowser
	}
	if opts.Status == nil {
		opts.Status = io.Discard
	}
	disc, err := Discover(ctx, opts.HTTP, opts.MCPURL)
	if err != nil {
		return nil, "", err
	}
	if disc.Auth != AuthOAuth {
		return nil, "", fmt.Errorf("%s does not use OAuth", opts.MCPURL)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen for the sign-in redirect: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	issuer := disc.Resource.AuthorizationServers[0]
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, opts.HTTP)
	if err != nil {
		return nil, "", fmt.Errorf("read the authorization server metadata of %s: %w", issuer, err)
	}
	registration := &oauthex.ClientRegistrationMetadata{
		RedirectURIs: []string{redirect}, ClientName: "Diderot CLI",
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		TokenEndpointAuthMethod: "none",
	}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: loopbackFetcher(ln, opts.Open, opts.Status),
		RequestRefreshToken:      true,
		AcceptUnadvertisedIss:    true,
		Client:                   opts.HTTP,
	}
	harnessClientID := ""
	switch {
	case opts.ClientID != "":
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: opts.ClientID}
		harnessClientID = opts.ClientID
	case asm == nil || asm.RegistrationEndpoint != "":
		// No metadata means the SDK's fallback endpoints, registration among them.
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: registration}
	default:
		own, err := auth.GetAuthServerMetadata(ctx, opts.MCPURL, opts.HTTP)
		if err != nil || own == nil || own.RegistrationEndpoint == "" {
			return nil, "", fmt.Errorf("the sign-in server %s does not let programs register, and %s offers no registration of its own; "+
				"ask your administrator for the OAuth client ID and run setup again with --client-id <id>", issuer, opts.MCPURL)
		}
		registered, err := oauthex.RegisterClient(ctx, own.RegistrationEndpoint, registration, opts.HTTP)
		if err != nil {
			return nil, "", fmt.Errorf("register with %s: %w", own.RegistrationEndpoint, err)
		}
		creds := &oauthex.ClientCredentials{ClientID: registered.ClientID}
		if registered.ClientSecret != "" {
			creds.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: registered.ClientSecret}
		}
		cfg.PreregisteredClient = creds
		harnessClientID = registered.ClientID
	}
	var signedIn *oauth2.Config
	var token *oauth2.Token
	cfg.NewTokenSource = func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		signedIn, token = oc, tok
		return oc.TokenSource(ctx, tok), nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, "", err
	}
	req, resp, err := probe(ctx, opts.HTTP, opts.MCPURL)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("%s stopped asking for a sign-in (HTTP %d)", opts.MCPURL, resp.StatusCode)
	}
	if err := handler.Authorize(ctx, req, resp); err != nil {
		return nil, "", fmt.Errorf("sign in: %w", err)
	}
	if signedIn == nil || token == nil {
		return nil, "", errors.New("sign in: no token came back")
	}
	return &OAuth{
		ClientID: signedIn.ClientID, ClientSecret: signedIn.ClientSecret, HarnessClientID: harnessClientID,
		Issuer: issuer, TokenURL: signedIn.Endpoint.TokenURL, AuthStyle: signedIn.Endpoint.AuthStyle,
		Token: &oauth2.Token{
			AccessToken: token.AccessToken, TokenType: token.TokenType,
			RefreshToken: token.RefreshToken, Expiry: token.Expiry,
		},
	}, SignedInAccount(token), nil
}

// SignedInAccount names the account a fresh token belongs to, from the access
// token or, failing that, the ID token that came with it.
func SignedInAccount(tok *oauth2.Token) string {
	if tok == nil {
		return ""
	}
	if account := AccountFromJWT(tok.AccessToken); account != "" {
		return account
	}
	if id, ok := tok.Extra("id_token").(string); ok {
		return AccountFromJWT(id)
	}
	return ""
}

// signInTimeout bounds how long sign-in waits for the browser.
const signInTimeout = 5 * time.Minute

// loopbackFetcher serves the redirect on ln and waits for the authorization
// response that carries the request's own state. A request without it, such
// as a stray local connection, is turned away and the wait goes on.
func loopbackFetcher(ln net.Listener, open func(string) error, status io.Writer) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		authURL, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		state := authURL.Query().Get("state")
		type outcome struct {
			result *auth.AuthorizationResult
			err    error
		}
		done := make(chan outcome, 1)
		deliver := func(o outcome) {
			select {
			case done <- o:
			default:
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if state == "" || q.Get("state") != state {
				http.Error(w, "This sign-in link does not match the one diderot started.", http.StatusBadRequest)
				return
			}
			if e := q.Get("error"); e != "" {
				_, _ = fmt.Fprintf(w, "Sign-in failed: %s. You can close this tab.", e)
				deliver(outcome{err: fmt.Errorf("the sign-in server answered %s: %s", e, q.Get("error_description"))})
				return
			}
			_, _ = fmt.Fprint(w, "Signed in to Diderot. You can close this tab.")
			deliver(outcome{result: &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}})
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		_, _ = fmt.Fprintf(status, "Opening your browser to sign in. If it does not open, visit:\n  %s\n", args.URL)
		_ = open(args.URL)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case o := <-done:
			return o.result, o.err
		case <-time.After(signInTimeout):
			return nil, errors.New("timed out waiting for the browser sign-in")
		}
	}
}

func openBrowser(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Start() //nolint:gosec // the authorization URL the SDK built
}
