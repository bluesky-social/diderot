package adminui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/google/uuid"
)

// devCSRFTokenPattern finds the privacy form's token; the header's sign-out
// form carries another.
var devCSRFTokenPattern = regexp.MustCompile(`action="/sessions/privacy"[^>]*>\s*<input type="hidden" name="csrf_token" value="([^"]+)"`)

func TestDevSessionFixtureUsesCanonicalIDs(test *testing.T) {
	raw, err := os.ReadFile("testdata/ui-dev.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture devFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		test.Fatal(err)
	}
	var page ownerSessionsData
	if err := json.Unmarshal(fixture.Pages["sessions"], &page); err != nil {
		test.Fatal(err)
	}
	if len(page.Sessions) == 0 {
		test.Fatal("session fixture is empty")
	}
	for _, session := range page.Sessions {
		if _, err := uuid.Parse(session.SessionID); err != nil {
			test.Errorf("fixture session %q is not a canonical UUID: %v", session.SessionID, err)
		}
		selection, err := decodeSessionSelection(session.SelectionKey)
		if err != nil || selection.SessionID != session.SessionID || selection.Client != session.Client {
			test.Errorf("fixture selection does not match session %q", session.SessionID)
		}
		viewer, err := url.Parse(session.ViewerURL)
		if err != nil || viewer.Path != "/sessions/"+session.SessionID || viewer.RawQuery != "" || viewer.Fragment != "" {
			test.Errorf("fixture viewer URL for %q is not UUID-only", session.SessionID)
		}
	}
}

func TestDevHandlerRendersEveryFixtureRoute(t *testing.T) {
	handler, err := NewDevHandler("testdata/ui-dev.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, route := range devRoutes {
		t.Run(route.key, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, res.Code, res.Body.String())
			}
			body := res.Body.String()
			if !strings.Contains(body, "<title>"+route.title+" · Diderot</title>") {
				t.Fatalf("GET %s did not render %q", path, route.title)
			}
			if !strings.Contains(body, "operator@example.test") {
				t.Fatalf("GET %s lacks synthetic operator", path)
			}
			if got := res.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control = %q", path, got)
			}
		})
	}
}

func TestDevHandlerAssetsAndReadOnlyBoundary(t *testing.T) {
	handler, err := NewDevHandler("testdata/ui-dev.json", nil)
	if err != nil {
		t.Fatal(err)
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/admin/static/admin.css", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "--bg") {
		t.Fatalf("asset response = %d: %s", asset.Code, asset.Body.String())
	}
	sessionsAsset := httptest.NewRecorder()
	handler.ServeHTTP(sessionsAsset, httptest.NewRequest(http.MethodGet, "/sessions/static/admin.css", nil))
	if sessionsAsset.Code != http.StatusOK || !strings.Contains(sessionsAsset.Body.String(), "--bg") {
		t.Fatalf("sessions asset response = %d: %s", sessionsAsset.Code, sessionsAsset.Body.String())
	}

	mutation := httptest.NewRecorder()
	handler.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "/admin/operations/reembed", nil))
	if mutation.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", mutation.Code)
	}
	if got := mutation.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/admin/not-a-fixture", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing route = %d", missing.Code)
	}
}

func TestDevHandlerSessionPrivacyToggleStaysLocal(t *testing.T) {
	handler, err := NewDevHandler("testdata/ui-dev.json", nil)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8082/sessions", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("GET /sessions = %d: %s", page.Code, page.Body.String())
	}
	match := devCSRFTokenPattern.FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("GET /sessions lacks CSRF token")
	}
	if _, err := base64.RawURLEncoding.DecodeString(match[1]); err != nil {
		t.Fatalf("CSRF token is not base64url: %v", err)
	}
	cookies := page.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != selfCSRFCookieName {
		t.Fatalf("CSRF cookies = %#v", cookies)
	}
	if cookies[0].Secure {
		t.Fatal("loopback ui-dev CSRF cookie must not be Secure")
	}

	post := func(origin, fetchSite, value string) *httptest.ResponseRecorder {
		t.Helper()
		selection := encodeSessionSelection(sessioningest.SessionRef{Client: "codex", SessionID: "01990900-0000-7000-8000-000000000001"})
		form := url.Values{"csrf_token": {match[1]}, "owner_only": {value}, "session": {selection}}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8082/sessions/privacy", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		req.AddCookie(cookies[0])
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	changed := post("http://127.0.0.1:8082", "", "true")
	if changed.Code != http.StatusSeeOther {
		t.Fatalf("origin POST = %d: %s", changed.Code, changed.Body.String())
	}
	updated := httptest.NewRecorder()
	handler.ServeHTTP(updated, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8082"+changed.Header().Get("Location"), nil))
	if strings.Count(updated.Body.String(), `<td data-label="Who can find it">Only you<`) != 2 || !strings.Contains(updated.Body.String(), "Production was not changed") {
		t.Fatalf("updated sessions page missing local state or notice: %s", updated.Body.String())
	}

	reverted := post("", "same-origin", "false")
	if reverted.Code != http.StatusSeeOther {
		t.Fatalf("fetch metadata POST = %d: %s", reverted.Code, reverted.Body.String())
	}
	shared := httptest.NewRecorder()
	handler.ServeHTTP(shared, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8082/sessions", nil))
	if strings.Count(shared.Body.String(), `<td data-label="Who can find it">Only you<`) != 1 {
		t.Fatalf("fetch metadata toggle did not update local state: %s", shared.Body.String())
	}

	metadataStripped := post("", "", "true")
	if metadataStripped.Code != http.StatusSeeOther {
		t.Fatalf("metadata-stripped POST = %d: %s", metadataStripped.Code, metadataStripped.Body.String())
	}

	opaqueOrigin := post("null", "same-origin", "false")
	if opaqueOrigin.Code != http.StatusSeeOther {
		t.Fatalf("opaque-origin POST = %d: %s", opaqueOrigin.Code, opaqueOrigin.Body.String())
	}
}

func TestDevHandlerRequiresCSRFEvenWithoutRequestMetadata(t *testing.T) {
	handler, err := NewDevHandler("testdata/ui-dev.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookieToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	formRaw := make([]byte, 32)
	formRaw[0] = 1
	formToken := base64.RawURLEncoding.EncodeToString(formRaw)
	form := url.Values{"csrf_token": {formToken}, "owner_only": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8082/sessions/privacy", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: selfCSRFCookieName, Value: cookieToken})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "invalid CSRF token") {
		t.Fatalf("POST with mismatched CSRF tokens = %d: %s", res.Code, res.Body.String())
	}
}

func TestDevHandlerReloadsFixtureOnEveryRequest(t *testing.T) {
	raw, err := os.ReadFile("testdata/ui-dev.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ui-dev.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDevHandler(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	render := func() string {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/admin/", nil))
		if res.Code != http.StatusOK {
			t.Fatalf("render = %d: %s", res.Code, res.Body.String())
		}
		return res.Body.String()
	}
	if body := render(); !strings.Contains(body, ">184<") {
		t.Fatal("initial fixture value missing")
	}
	updated := strings.Replace(string(raw), `"Questions": 184`, `"Questions": 185`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if body := render(); !strings.Contains(body, ">185<") {
		t.Fatal("updated fixture value missing")
	}
}

var (
	devFixtureEmailDomain = regexp.MustCompile(`[a-z0-9._%+-]+@([a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	devFixtureURLHost     = regexp.MustCompile(`https?://([a-z0-9.-]+)`)
)

// devFixturePublicHosts are the real hosts the fixture may name: public
// services, a documentation workspace, and the quickstart's embedder
// service.
var devFixturePublicHosts = map[string]bool{
	"api.openai.com":    true,
	"mcp.sentry.dev":    true,
	"www.notion.so":     true,
	"example.slack.com": true,
	"embedder":          true,
}

// reservedHost reports whether host is a name reserved for documentation
// and testing (RFC 2606, RFC 6761), which no real system answers to.
func reservedHost(host string) bool {
	for _, reserved := range []string{"test", "invalid", "example", "localhost", "example.com", "example.net", "example.org"} {
		if host == reserved || strings.HasSuffix(host, "."+reserved) {
			return true
		}
	}
	return false
}

// TestUIDevFixtureContainsOnlySyntheticMaterial: the committed fixture holds
// no credential, and every email address and web address in it is a
// reserved placeholder or a public service, so nobody's real mailbox or
// host is in it.
func TestUIDevFixtureContainsOnlySyntheticMaterial(t *testing.T) {
	raw, err := os.ReadFile("testdata/ui-dev.json")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{
		"xoxb-", "xoxp-", "akia", "bearer eyj", "postgres://", "postgresql://",
		"begin private key", "actions_id_token_request_token",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("fixture contains forbidden secret marker %q", forbidden)
		}
	}
	emails := devFixtureEmailDomain.FindAllStringSubmatch(lower, -1)
	if len(emails) == 0 {
		t.Fatal("found no email address in the fixture; did its shape change?")
	}
	for _, match := range emails {
		if !reservedHost(match[1]) {
			t.Errorf("fixture email %q is not under a reserved placeholder domain", match[0])
		}
	}
	hosts := devFixtureURLHost.FindAllStringSubmatch(lower, -1)
	if len(hosts) == 0 {
		t.Fatal("found no web address in the fixture; did its shape change?")
	}
	for _, match := range hosts {
		host := strings.TrimRight(match[1], ".")
		if !reservedHost(host) && !devFixturePublicHosts[host] {
			t.Errorf("fixture names host %q, which is neither a reserved placeholder nor a listed public service", host)
		}
	}
	if !strings.Contains(lower, "example.test") || !strings.Contains(lower, "synthetic") {
		t.Fatal("fixture should make its synthetic identity conspicuous")
	}
}

func TestLoadDevFixtureRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(`{"pages":{"overview":{}}} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadDevFixture(path)
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevHandlerRendersSignIn(t *testing.T) {
	handler, err := NewDevHandler("testdata/ui-dev.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/auth/login":          "Sign in with your identity provider",
		"/auth/login?failed=1": "Wrong username or password.",
		"/auth/admin.css":      ".signin",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), want) {
			t.Fatalf("GET %s = %d, lacks %q", path, res.Code, want)
		}
	}
}
