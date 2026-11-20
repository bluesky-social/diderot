package adminui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMountGatesEntireAdminTree(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
		selfGroups:    map[string]struct{}{"everyone": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))

	for _, path := range []string{"/admin/static/admin.css", "/admin/static/favicon.svg", "/admin/not-a-route"} {
		t.Run(path+" missing identity", func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})

		t.Run(path+" ordinary user", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set(proxyEmailHeader, "user@example.com")
			r.Header.Set(proxyGroupsHeader, "everyone")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}

	r := httptest.NewRequest(http.MethodGet, "/admin/static/admin.css", nil)
	r.Header.Set(proxyEmailHeader, "operator@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
}

func TestEveryShellShowsTheMark(t *testing.T) {
	icons := []string{
		`<link rel="icon" href="/auth/favicon.ico" sizes="32x32">`,
		`<link rel="icon" type="image/svg+xml" href="data:image/svg&#43;xml;base64,`,
		`<link rel="apple-touch-icon" href="/auth/apple-touch-icon.png">`,
		`<svg class="brand-mark" viewBox="-12 -12 24 24" aria-hidden="true"`,
	}
	var out strings.Builder
	require.NoError(t, overviewPage.ExecuteTemplate(&out, "layout", overviewData{pageData: pageData{Title: "Test"}}))
	admin := out.String()
	out.Reset()
	require.NoError(t, ownerSessionsPage.ExecuteTemplate(&out, "self_layout", ownerSessionsData{}))
	self := out.String()
	out.Reset()
	require.NoError(t, signInPage.ExecuteTemplate(&out, "signin", signInData{Title: "Sign in"}))
	signIn := out.String()
	for _, want := range icons {
		assert.Contains(t, admin, want)
		assert.Contains(t, self, want)
		assert.Contains(t, signIn, want)
	}
	assert.Equal(t, 6, strings.Count(string(markSVG), "<path "), "six petals")
}

// TestIconsNeedNoSignIn: a signed-out browser fetches the icons the sign-in
// page names.
func TestIconsNeedNoSignIn(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
		selfGroups:    map[string]struct{}{"everyone": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	for path, contentType := range map[string]string{
		"/auth/favicon.ico":          "image/x-icon",
		"/auth/apple-touch-icon.png": "image/png",
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.Equal(t, contentType, w.Header().Get("Content-Type"), path)
		assert.NotEmpty(t, w.Body.Bytes(), path)
	}
}

func TestRootRedirectHasBrowserSecurityHeaders(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups:   map[string]struct{}{"admins": {}},
		selfGroups:    map[string]struct{}{"everyone": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(proxyEmailHeader, "user@example.com")
	r.Header.Set(proxyGroupsHeader, "everyone")
	mux.ServeHTTP(w, r)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/sessions", w.Header().Get("Location"))
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
}
