package sessiondev

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/auth"
)

func TestIdentityTokenAndHeaderBoundary(t *testing.T) {
	identity, token, err := NewIdentity("Owner@Example.Invalid", []string{"everyone"})
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(token string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		return request
	}
	principal, err := identity.Authenticate(bearer(token))
	if err != nil {
		t.Fatal(err)
	}
	if principal.Email != "owner@example.invalid" || !principal.HasGroup("everyone") {
		t.Fatalf("principal = %+v", principal)
	}
	if _, err := identity.Authenticate(bearer(token + "x")); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("tampered token: err = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	request.Header.Set("X-Auth-Request-Email", "attacker@example.invalid")
	request.Header.Set("X-Auth-Request-Groups", "admin")
	browser, err := identity.BrowserAuthenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if browser.Email != "owner@example.invalid" || !slices.Equal(browser.Groups, []string{"everyone"}) {
		t.Fatalf("browser principal honored caller headers: %+v", browser)
	}
}

func TestListenLoopbackAndDatabaseGuard(t *testing.T) {
	listener, origin, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if origin == "" {
		t.Fatal("empty origin")
	}
	for _, address := range []string{"0.0.0.0:0", "localhost:0"} {
		if listener, _, err := ListenLoopback(address); err == nil {
			_ = listener.Close()
			t.Fatalf("ListenLoopback(%q) succeeded", address)
		}
	}
	if err := RequireLoopbackAddress("127.0.0.1:6334"); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"qdrant.example:6334", "10.0.0.1:6334", "127.0.0.1"} {
		if err := RequireLoopbackAddress(address); err == nil {
			t.Fatalf("unsafe service address %q passed guard", address)
		}
	}
	if err := RequireDevDatabase("postgres://user:pass@127.0.0.1:5432/diderot_session_dev?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
	for _, databaseURL := range []string{
		"postgres://user:pass@prod.example/diderot_session_dev",
		"postgres://user:pass@localhost/diderot_session_dev",
		"postgres://user:pass@127.0.0.1/diderot",
		"http://127.0.0.1/diderot_session_dev",
	} {
		if err := RequireDevDatabase(databaseURL); err == nil {
			t.Fatalf("unsafe database %q passed guard", databaseURL)
		}
	}
}

func TestWriteTokenPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "token")
	if err := WriteToken(path, "secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token mode = %04o, want 0600", got)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime directory mode = %04o, want 0700", got)
	}
	if err := WriteToken(path, "replacement"); err == nil {
		t.Fatal("WriteToken overwrote an existing token")
	}
	identity, _, err := NewIdentity("owner@example.invalid", []string{"everyone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.RotateToken(path); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) == "secret\n" {
		t.Fatal("RotateToken did not replace the original token")
	}
	if _, err := identity.verify(strings.TrimSpace(string(rotated))); err != nil {
		t.Fatalf("rotated token does not verify: %v", err)
	}
	if got, err := os.Stat(path); err != nil || got.Mode().Perm() != 0o600 {
		t.Fatalf("rotated token mode = %v, %v", got, err)
	}
	sharedDirectory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(sharedDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteToken(filepath.Join(sharedDirectory, "token"), "secret"); err == nil {
		t.Fatal("WriteToken accepted a non-private existing directory")
	}
	sharedInfo, err := os.Stat(sharedDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if got := sharedInfo.Mode().Perm(); got != 0o755 {
		t.Fatalf("WriteToken changed caller directory mode to %04o", got)
	}
}
