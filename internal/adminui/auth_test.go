package adminui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth"
)

// The trusted proxy's identity header names in these tests.
const (
	proxyEmailHeader  = "X-Auth-Request-Email"
	proxyGroupsHeader = "X-Auth-Request-Groups"
)

// testBrowserAuth trusts httptest.NewRequest's default peer (192.0.2.1) and
// loopback test servers.
func testBrowserAuth() auth.Authenticator {
	p, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("127.0.0.1/32"),
		},
		EmailHeader: proxyEmailHeader, GroupsHeader: proxyGroupsHeader, GroupsDelimiter: "|",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		panic(err)
	}
	return p
}

func TestAuthedBrowser(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
		selfGroups:    map[string]struct{}{"everyone": {}},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		require.True(t, ok)
		assert.Equal(t, "operator@example.com", p.Email)
		assert.Equal(t, []string{"everyone", "admins"}, p.Groups)
		w.WriteHeader(http.StatusNoContent)
	})
	h := s.authedBrowser(next)

	t.Run("missing identity", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("ordinary user", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.Header.Set(proxyEmailHeader, "user@example.com")
		r.Header.Set(proxyGroupsHeader, "everyone")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, "<h1>This account can&#39;t open the admin pages</h1>", "the refusal is a page in the shell")
		assert.Contains(t, body, "Only members of <code>admins</code> can open the admin pages.")
		assert.Contains(t, body, "<code>DIDEROT_ADMIN_GROUPS</code>")
		assert.Contains(t, body, `<a class="button" href="/sessions">Go to My sessions</a>`, "a self-service member is pointed at My sessions")

		r = httptest.NewRequest(http.MethodPost, "/admin/reports", nil)
		r.Header.Set(proxyEmailHeader, "user@example.com")
		r.Header.Set(proxyGroupsHeader, "everyone")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, "administrator access required\n", w.Body.String(), "a refused form submission stays a plain refusal")
	})

	t.Run("user outside every group", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.Header.Set(proxyEmailHeader, "guest@example.com")
		r.Header.Set(proxyGroupsHeader, "guests")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.NotContains(t, w.Body.String(), "My sessions")
	})

	t.Run("operator", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.Header.Set(proxyEmailHeader, "operator@example.com")
		r.Header.Set(proxyGroupsHeader, "everyone| admins ")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("operator headers from untrusted peer", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.RemoteAddr = "198.51.100.7:4000"
		r.Header.Set(proxyEmailHeader, "operator@example.com")
		r.Header.Set(proxyGroupsHeader, "admins")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("repeated email header", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.Header.Add(proxyEmailHeader, "operator@example.com")
		r.Header.Add(proxyEmailHeader, "attacker@example.com")
		r.Header.Set(proxyGroupsHeader, "admins")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestBrowserSurfacesIgnoreHeadersWithoutTrustedProxy pins the default: with
// no DIDEROT_TRUSTED_PROXY_CIDRS, identity headers authenticate nobody on
// /admin, /sessions, or the root redirect.
func TestBrowserSurfacesIgnoreHeadersWithoutTrustedProxy(t *testing.T) {
	noProxy, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{})
	require.NoError(t, err)
	for name, authenticator := range map[string]auth.Authenticator{
		"no trusted proxy": noProxy,
		"untrusted peer":   testBrowserAuth(),
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{
				authenticator: authenticator,
				logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				adminGroups:   map[string]struct{}{"admins": {}},
				selfGroups:    map[string]struct{}{"everyone": {}},
			}
			mux := http.NewServeMux()
			require.NoError(t, s.mountAdminRoutes(mux))
			require.NoError(t, s.MountSessions(mux))
			for _, path := range []string{"/", "/admin/anything", "/sessions"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.RemoteAddr = "198.51.100.7:4000"
				r.Header.Set(proxyEmailHeader, "operator@example.com")
				r.Header.Set(proxyGroupsHeader, "admins|everyone")
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				assert.Equal(t, http.StatusUnauthorized, w.Code, path)
			}
		})
	}
}
