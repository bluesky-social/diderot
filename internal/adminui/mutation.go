package adminui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bluesky-social/diderot/internal/adminaction"
)

const maxAdminFormBytes = 64 << 10
const csrfCookieName = "diderot_admin_csrf"

func issueCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	return issueCSRFTokenCookie(w, r, csrfCookieName, "/admin/", true)
}

func issueCSRFTokenCookie(w http.ResponseWriter, r *http.Request, name, path string, secure bool) (string, error) {
	if cookie, err := r.Cookie(name); err == nil && validCSRFToken(cookie.Value) {
		return cookie.Value, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate CSRF token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: token, Path: path,
		Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return token, nil
}

func (s *Server) requireMutation(w http.ResponseWriter, r *http.Request) bool {
	return requireFormMutation(w, r, s.publicOrigin, csrfCookieName)
}

func requireFormMutation(w http.ResponseWriter, r *http.Request, publicOrigin, cookieName string) bool {
	return requireFormMutationLimit(w, r, publicOrigin, cookieName, maxAdminFormBytes)
}

func requireFormMutationLimit(w http.ResponseWriter, r *http.Request, publicOrigin, cookieName string, maxBytes int64) bool {
	if !sameOriginRequest(r, publicOrigin) {
		http.Error(w, "same-origin form submission required", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || !validCSRFToken(cookie.Value) || !validCSRFToken(r.Form.Get("csrf_token")) ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.Form.Get("csrf_token"))) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

func validCSRFToken(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	return err == nil && len(raw) == 32
}

func sameOriginRequest(r *http.Request, publicOrigin string) bool {
	originValues := r.Header.Values("Origin")
	refererValues := r.Header.Values("Referer")
	fetchSiteValues := r.Header.Values("Sec-Fetch-Site")
	if len(originValues) > 1 || len(refererValues) > 1 || len(fetchSiteValues) > 1 {
		return false
	}
	raw := ""
	fromOrigin := len(originValues) == 1 && strings.TrimSpace(originValues[0]) != ""
	if fromOrigin {
		raw = strings.TrimSpace(originValues[0])
		// Opaque origins serialize as "null". Privacy-preserving browsers can
		// use that value for an ordinary same-origin form, so accept it only
		// when unforgeable browser fetch metadata independently says the
		// navigation is same-origin. A concrete mismatched Origin still fails.
		if strings.EqualFold(raw, "null") {
			return len(fetchSiteValues) == 1 && strings.EqualFold(strings.TrimSpace(fetchSiteValues[0]), "same-origin")
		}
	} else if len(refererValues) == 1 {
		raw = strings.TrimSpace(refererValues[0])
	}
	if raw == "" {
		// Some privacy-preserving clients and reverse proxies omit all three
		// request-metadata headers. Explicit cross-site fetch metadata still
		// fails closed; when no metadata survives, the 256-bit double-submit
		// token and SameSite=Strict cookie provide the CSRF boundary.
		return len(fetchSiteValues) == 0 || strings.EqualFold(strings.TrimSpace(fetchSiteValues[0]), "same-origin")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return false
	}
	requestOrigin, ok := canonicalOrigin(u)
	if !ok || requestOrigin != publicOrigin {
		return false
	}
	// Origin is origin-only. Referer legitimately identifies a page and may
	// include a path or query, but malformed Origin values fail closed.
	return !fromOrigin || (u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "")
}

func canonicalOrigin(u *url.URL) (string, bool) {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if (scheme != "http" && scheme != "https") || host == "" || strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", false
		}
		if (scheme == "http" && portNumber == 80) || (scheme == "https" && portNumber == 443) {
			port = ""
		} else {
			port = strconv.Itoa(portNumber)
		}
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, true
}

// audit records the browser caller's actions.
func (s *Server) audit(r *http.Request) adminaction.Audit {
	principal, _ := PrincipalFromContext(r.Context())
	return adminaction.Audit{Pool: s.pool, Logger: s.logger, Actor: adminaction.Actor{
		Email: principal.Email, RequestID: strings.TrimSpace(r.Header.Get("X-Request-ID")),
	}}
}

// auditFailed answers 500 when an action's audit record could not be
// started, so the action did not run.
func (s *Server) auditFailed(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, adminaction.ErrRecord) {
		return false
	}
	s.logger.Error("admin audit start failed", "err", err)
	http.Error(w, "audit log failed", http.StatusInternalServerError)
	return true
}

func cleanNotice(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 300 {
		raw = raw[:300]
	}
	return raw
}
