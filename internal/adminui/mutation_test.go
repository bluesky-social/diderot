package adminui

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/stretchr/testify/assert"
)

func TestBrowserRejectsCrossOriginMutation(t *testing.T) {
	server := &Server{publicOrigin: "https://diderot.test"}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	r := httptest.NewRequest(http.MethodPost, "https://diderot.test/admin/reports",
		strings.NewReader(url.Values{"csrf_token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://attacker.example")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	r = r.WithContext(auth.NewContext(r.Context(), &auth.Principal{Email: "operator@example.com"}))
	w := httptest.NewRecorder()
	assert.False(t, server.requireMutation(w, r))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBrowserAcceptsPublicOriginBehindInternalProxyHost(t *testing.T) {
	server := &Server{publicOrigin: "https://diderot.test"}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	r := httptest.NewRequest(http.MethodPost,
		"http://diderot.example.svc.cluster.local/admin/reports",
		strings.NewReader(url.Values{"csrf_token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://diderot.test")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	w := httptest.NewRecorder()
	assert.True(t, server.requireMutation(w, r))
}

func TestBrowserOriginSyntaxAndRefererFallback(t *testing.T) {
	const publicOrigin = "https://diderot.test"
	request := func(header, value string) *http.Request {
		r := httptest.NewRequest(http.MethodPost,
			"http://diderot.example.svc.cluster.local/admin/reports", nil)
		r.Header.Set(header, value)
		return r
	}

	assert.False(t, sameOriginRequest(request("Origin", publicOrigin+"/not-an-origin?x=1"), publicOrigin))
	assert.True(t, sameOriginRequest(request("Referer", publicOrigin+"/admin/reports"), publicOrigin))
	duplicates := request("Origin", publicOrigin)
	duplicates.Header.Add("Origin", "https://attacker.example")
	assert.False(t, sameOriginRequest(duplicates, publicOrigin))
}

func TestSameOriginRequestAcceptsSafeMetadataFallbacks(t *testing.T) {
	const publicOrigin = "https://diderot.test"
	request := func(fetchSite string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, publicOrigin+"/sessions/privacy", nil)
		if fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		return r
	}

	assert.True(t, sameOriginRequest(request("same-origin"), publicOrigin))
	assert.False(t, sameOriginRequest(request("same-site"), publicOrigin))
	assert.False(t, sameOriginRequest(request("cross-site"), publicOrigin))
	assert.True(t, sameOriginRequest(request(""), publicOrigin))
	emptyFetchMetadata := request("")
	emptyFetchMetadata.Header["Sec-Fetch-Site"] = []string{""}
	assert.False(t, sameOriginRequest(emptyFetchMetadata, publicOrigin))

	mismatchedOrigin := request("same-origin")
	mismatchedOrigin.Header.Set("Origin", "https://attacker.example")
	assert.False(t, sameOriginRequest(mismatchedOrigin, publicOrigin))
	opaqueSameOrigin := request("same-origin")
	opaqueSameOrigin.Header.Set("Origin", "null")
	assert.True(t, sameOriginRequest(opaqueSameOrigin, publicOrigin))
	opaqueCrossSite := request("cross-site")
	opaqueCrossSite.Header.Set("Origin", "null")
	assert.False(t, sameOriginRequest(opaqueCrossSite, publicOrigin))
	opaqueWithoutMetadata := request("")
	opaqueWithoutMetadata.Header.Set("Origin", "null")
	assert.False(t, sameOriginRequest(opaqueWithoutMetadata, publicOrigin))

	duplicates := request("same-origin")
	duplicates.Header.Add("Sec-Fetch-Site", "same-origin")
	assert.False(t, sameOriginRequest(duplicates, publicOrigin))
}

func browserRequest(t *testing.T, mux *http.ServeMux, method, target string, form url.Values, csrfCookie string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Header.Set(proxyEmailHeader, "operator@example.com")
	r.Header.Set(proxyGroupsHeader, "admins")
	if csrfCookie != "" {
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://diderot.test")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
