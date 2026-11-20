package adminui

import (
	"context"
	"net/http"
	"strings"

	"github.com/bluesky-social/diderot/internal/auth"
)

// PrincipalFromContext returns the browser principal installed by
// authedBrowser or authedSelfServiceBrowser.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	p := auth.FromContext(ctx)
	if p == nil {
		return auth.Principal{}, false
	}
	return *p, true
}

// authedBrowser admits only administrators. Identity comes from the
// configured browser authenticator: for example, identity headers from a
// trusted authenticating proxy, which strips caller-supplied copies.
func (s *Server) authedBrowser(next http.Handler) http.Handler {
	return s.observeAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setBrowserSecurityHeaders(w)
		principal, code, message := s.browserPrincipal(w, r)
		if code != http.StatusOK {
			s.denyBrowser(w, r, code, message)
			return
		}
		if !s.isAdmin(principal) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				s.denyAdmin(w, r, principal)
				return
			}
			http.Error(w, "administrator access required", http.StatusForbidden)
			return
		}
		s.logger.Info("admin ui request", "email", principal.Email, "method", r.Method, "path", r.URL.Path)
		r = s.withSignOutToken(w, r)
		next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), principal)))
	}))
}

func (s *Server) authedSelfServiceBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setBrowserSecurityHeaders(w)
		principal, code, message := s.browserPrincipal(w, r)
		if code != http.StatusOK {
			s.denyBrowser(w, r, code, message)
			return
		}
		if !principal.HasAnyGroup(s.selfGroups) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				s.denySelfService(w, r, principal)
				return
			}
			http.Error(w, "team member access required", http.StatusForbidden)
			return
		}
		principal.Email = strings.ToLower(principal.Email)
		s.logger.Info("session privacy ui request", "email", principal.Email, "method", r.Method, "path", r.URL.Path)
		r = s.withSignOutToken(w, r)
		next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), principal)))
	})
}

// signedInBrowser admits any signed-in browser. It serves only the
// self-service pages' stylesheet and script, which the page that refuses an
// account outside the self-service groups needs too.
func (s *Server) signedInBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setBrowserSecurityHeaders(w)
		if _, code, message := s.browserPrincipal(w, r); code != http.StatusOK {
			s.denyBrowser(w, r, code, message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAdmin admits the administrator groups and, while its authenticator still
// issues it, the bootstrap admin.
func (s *Server) isAdmin(principal *auth.Principal) bool {
	return principal.Bootstrap || principal.HasAnyGroup(s.adminGroups)
}

// browserPrincipal requires an identity with an email: every browser action
// is attributed to, and owner-scoped by, that address. With the built-in
// sign-in, the session cookie may be refreshed on w.
func (s *Server) browserPrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, int, string) {
	var principal *auth.Principal
	var err error
	if s.login != nil {
		principal, err = s.login.AuthenticateBrowser(w, r)
	} else {
		principal, err = s.authenticator.Authenticate(r)
	}
	if err != nil || principal == nil {
		return nil, http.StatusUnauthorized, "authentication required"
	}
	principal.Email = strings.TrimSpace(principal.Email)
	if principal.Email == "" {
		return nil, http.StatusUnauthorized, "authentication required"
	}
	return principal, http.StatusOK, ""
}

// denyBrowser answers a request browserPrincipal refused. With the built-in
// sign-in, an unauthenticated page load is sent to sign in and brought back;
// a form submission is not replayed and gets the plain 401. Without it, the
// 401 says how to turn local sign-in on.
func (s *Server) denyBrowser(w http.ResponseWriter, r *http.Request, code int, message string) {
	if code == http.StatusUnauthorized && s.login != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) {
		http.Redirect(w, r, signInURL(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if code == http.StatusUnauthorized && s.login == nil {
		message += ". " + sessionKeysHint
	}
	http.Error(w, message, code)
}

func setBrowserSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
