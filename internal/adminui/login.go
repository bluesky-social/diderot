package adminui

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/bluesky-social/diderot/internal/auth"
)

// BrowserLogin is the browser's own sign-in (auth.BrowserLogin): OIDC, the
// bootstrap admin, or a pasted static token, each ending in a sealed session
// cookie. A Config.Authenticator that implements it gets the /auth/ routes,
// sign-in redirects for unauthenticated page loads, and a sliding session.
type BrowserLogin interface {
	auth.Authenticator
	AuthenticateBrowser(http.ResponseWriter, *http.Request) (*auth.Principal, error)
	OIDCEnabled() bool
	StaticTokensEnabled() bool
	BootstrapEnabled(context.Context) (bool, error)
	StartLogin(w http.ResponseWriter, returnTo string) (string, error)
	FinishLogin(http.ResponseWriter, *http.Request) (string, *auth.Principal, error)
	SignInBootstrap(w http.ResponseWriter, r *http.Request, username, password string) (*auth.Principal, error)
	SignInToken(w http.ResponseWriter, token string) (*auth.Principal, error)
	Logout(http.ResponseWriter)
}

const (
	// signOutCSRFCookieName holds the double-submit token of the sign-out
	// form, which every page's header carries, so its cookie has Path=/.
	// The name differs from the one the cookie had while it was scoped to
	// the sign-out page: a browser still holding that one would send it
	// first and fail the comparison. signInCSRFCookieName is the local
	// sign-in forms' token.
	signOutCSRFCookieName = "diderot_signout_token"
	signInCSRFCookieName  = "diderot_signin_csrf"

	// oidcLoginPath starts the OIDC sign-in from the sign-in page.
	oidcLoginPath = auth.LoginPath + "/oidc"
)

// sessionKeysHint tells an operator how to turn on local sign-in.
const sessionKeysHint = "To sign in without an identity provider, set DIDEROT_BROWSER_SESSION_KEYS (generate a key with: openssl rand -base64 32) and restart Diderot."

var signInPage = template.Must(template.New("signin.html").Funcs(templateFuncs).ParseFS(assets, "templates/signin.html"))

type signInData struct {
	Title   string
	Message string
	// Error is why a sign-in was refused.
	Error     string
	SignIn    string
	SignOut   string
	CSRFToken string
	Principal string
	// The sign-in page's methods.
	ReturnTo    string
	OIDC        string
	Bootstrap   bool
	StaticToken bool
}

// mountLogin registers the sign-in routes. They answer only on the browser
// listener: the internal listener 404s /auth/ with the rest of the browser
// surface.
func (s *Server) mountLogin(mux Mux) {
	if s.login == nil {
		return
	}
	mux.HandleFunc("GET "+auth.LoginPath, s.handleSignIn)
	mux.HandleFunc("GET "+signInStylesheetPath, handleSignInStylesheet)
	mux.HandleFunc("POST "+auth.LoginPath, s.handleLocalSignIn)
	mux.HandleFunc("GET "+oidcLoginPath, s.handleOIDCSignIn)
	mux.HandleFunc("GET "+auth.CallbackPath, s.handleSignInCallback)
	mux.HandleFunc("GET "+auth.LogoutPath, s.handleSignOutPage)
	mux.HandleFunc("POST "+auth.LogoutPath, s.handleSignOut)
}

// signInStylesheetPath serves the admin stylesheet to the sign-in pages,
// which render before there is a session to reach /admin/static/.
const signInStylesheetPath = "/auth/admin.css"

func handleSignInStylesheet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFileFS(w, r, assets, "static/admin.css")
}

func signInURL(returnTo string) string {
	return auth.LoginPath + "?return_to=" + url.QueryEscape(auth.SafeReturnPath(returnTo))
}

func (s *Server) signOutURL() string {
	if s.login == nil {
		return ""
	}
	return auth.LogoutPath
}

// handleSignIn offers every configured method. With the OIDC sign-in alone
// it goes straight to the identity provider.
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	returnTo := auth.SafeReturnPath(r.URL.Query().Get("return_to"))
	data, err := s.signInForms(w, r, returnTo)
	if err != nil {
		s.logger.Error("sign-in page could not start", "err", err)
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	if data.OIDC != "" && !data.Bootstrap && !data.StaticToken {
		s.startOIDC(w, r, returnTo)
		return
	}
	s.renderSignIn(w, http.StatusOK, data)
}

// signInForms describes the methods available right now, with a fresh CSRF
// token for the local forms.
func (s *Server) signInForms(w http.ResponseWriter, r *http.Request, returnTo string) (signInData, error) {
	data := signInData{Title: "Sign in", ReturnTo: returnTo, StaticToken: s.login.StaticTokensEnabled()}
	if s.login.OIDCEnabled() {
		data.OIDC = oidcLoginPath + "?return_to=" + url.QueryEscape(returnTo)
	}
	bootstrap, err := s.login.BootstrapEnabled(r.Context())
	if err != nil {
		// The page still offers the other methods.
		s.logger.Warn("bootstrap admin state unavailable", "err", err)
	}
	data.Bootstrap = bootstrap
	if data.Bootstrap || data.StaticToken {
		token, err := issueCSRFTokenCookie(w, r, signInCSRFCookieName, auth.LoginPath, s.sessionCookieSecure)
		if err != nil {
			return signInData{}, err
		}
		data.CSRFToken = token
	}
	if data.OIDC == "" && !data.Bootstrap && !data.StaticToken {
		data.Message = "No sign-in method is available. Sign in through your identity-aware proxy, or ask an administrator."
	}
	return data, nil
}

func (s *Server) handleOIDCSignIn(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	if !s.login.OIDCEnabled() {
		http.NotFound(w, r)
		return
	}
	s.startOIDC(w, r, r.URL.Query().Get("return_to"))
}

func (s *Server) startOIDC(w http.ResponseWriter, r *http.Request, returnTo string) {
	target, err := s.login.StartLogin(w, returnTo)
	if err != nil {
		s.logger.Error("browser sign-in could not start", "err", err)
		s.renderSignIn(w, http.StatusBadGateway, signInData{
			Title: "Sign-in unavailable", Error: "Diderot could not reach the identity provider. Try again shortly.",
			SignIn: auth.LoginPath,
		})
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleLocalSignIn signs in as the bootstrap admin or with a static token.
// The origin and double-submit checks keep another site from signing a
// browser into an account it chose.
func (s *Server) handleLocalSignIn(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	if !requireFormMutation(w, r, s.publicOrigin, signInCSRFCookieName) {
		return
	}
	returnTo := auth.SafeReturnPath(r.Form.Get("return_to"))
	var principal *auth.Principal
	var err error
	method := r.Form.Get("method")
	switch method {
	case "bootstrap":
		principal, err = s.login.SignInBootstrap(w, r, r.Form.Get("username"), r.Form.Get("password"))
	case "token":
		principal, err = s.login.SignInToken(w, r.Form.Get("token"))
	default:
		http.Error(w, "unknown sign-in method", http.StatusBadRequest)
		return
	}
	if err == nil {
		s.logger.Info("browser sign-in", "method", method, "email", principal.Email, "subject", principal.Subject)
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
		return
	}
	status, message := http.StatusUnauthorized, "That token is not valid here."
	switch {
	case errors.Is(err, auth.ErrBootstrapDisabled):
		message = "The bootstrap admin is turned off. Sign in another way."
	case errors.Is(err, auth.ErrLocalSignInUnavailable):
		status, message = http.StatusNotFound, "That sign-in method is not available."
	case !errors.Is(err, auth.ErrInvalidCredentials):
		s.logger.Error("local sign-in failed", "method", method, "err", err)
		status, message = http.StatusInternalServerError, "Sign-in failed. Try again shortly."
	case method == "bootstrap":
		message = "Wrong username or password."
	}
	s.logger.Warn("local sign-in refused", "method", method, "status", status, "err", err)
	data, formErr := s.signInForms(w, r, returnTo)
	if formErr != nil {
		http.Error(w, message, status)
		return
	}
	data.Error = message
	s.renderSignIn(w, status, data)
}

// handleSignInCallback never redirects back to sign-in on failure: a
// persistent failure would otherwise loop through the IdP.
func (s *Server) handleSignInCallback(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	if !s.login.OIDCEnabled() {
		http.NotFound(w, r)
		return
	}
	returnTo, principal, err := s.login.FinishLogin(w, r)
	if err == nil {
		s.logger.Info("browser sign-in", "method", "oidc", "email", principal.Email, "subject", principal.Subject)
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
		return
	}
	data := signInData{Title: "Sign-in failed", SignIn: auth.LoginPath}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, auth.ErrLoginState):
		status, data.Error = http.StatusBadRequest,
			"This sign-in expired, was already used, or was started in another browser or tab."
	case errors.Is(err, auth.ErrLoginDenied):
		status, data.Error = http.StatusForbidden, "The identity provider did not approve this sign-in."
	case errors.Is(err, auth.ErrLoginNoEmail):
		status, data.Error = http.StatusForbidden,
			"Your identity provider did not supply a verified email address, which Diderot requires."
	case errors.Is(err, auth.ErrLoginProvider):
		status, data.Error = http.StatusBadGateway, "Diderot could not complete sign-in with the identity provider."
	case errors.Is(err, auth.ErrIdentityNotAllowed):
		status, data.Error = http.StatusForbidden,
			"This account may not sign in to Diderot: its email address is missing, unverified, or outside the allowed domains."
	case errors.Is(err, auth.ErrInvalidCredentials):
		status, data.Error = http.StatusUnauthorized, "Diderot could not verify the identity provider's response."
	default:
		data.Error = "Sign-in failed."
	}
	s.logger.Warn("browser sign-in failed", "status", status, "err", err)
	s.renderSignIn(w, status, data)
}

func (s *Server) handleSignOutPage(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	token, err := issueCSRFTokenCookie(w, r, signOutCSRFCookieName, "/", s.sessionCookieSecure)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data := signInData{
		Title: "Sign out", Message: "Sign out of Diderot in this browser?",
		SignOut: auth.LogoutPath, CSRFToken: token,
	}
	if principal, err := s.login.Authenticate(r); err == nil {
		data.Principal = principal.Email
	}
	s.renderSignIn(w, http.StatusOK, data)
}

// handleSignOut deletes the session. The origin and double-submit checks
// keep another site from signing a browser out; the IdP's own session is
// left alone.
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w)
	if !requireFormMutation(w, r, s.publicOrigin, signOutCSRFCookieName) {
		return
	}
	if principal, err := s.login.Authenticate(r); err == nil {
		s.logger.Info("browser sign-out", "email", principal.Email, "subject", principal.Subject)
	}
	s.login.Logout(w)
	s.renderSignIn(w, http.StatusOK, signInData{
		Title: "Signed out", Message: "You have signed out of Diderot in this browser.", SignIn: auth.LoginPath,
	})
}

func (s *Server) renderSignIn(w http.ResponseWriter, status int, data signInData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := signInPage.ExecuteTemplate(w, "signin", data); err != nil {
		s.logger.Error("sign-in page render failed", "err", err)
	}
}
