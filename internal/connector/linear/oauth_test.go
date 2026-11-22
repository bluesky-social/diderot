package linear

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Synthetic OAuth application credentials, not real ones.
const (
	fixtureClientID     = "fixture0client0id000000000000000"
	fixtureClientSecret = "fixture-client-secret"
)

// fakeTokenEndpoint is Linear's OAuth token endpoint for the fixture
// application. Each valid client-credentials request gets a new token, so a
// test can tell a held token from a replacement.
type fakeTokenEndpoint struct {
	t *testing.T
	// expiresIn is the lifetime, in seconds, the endpoint gives each token.
	expiresIn int
	mints     atomic.Int32
}

// thirtyDays is the lifetime Linear gives a client-credentials token.
const thirtyDays = 2591999

func (f *fakeTokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
		f.t.Errorf("token request = %s %s, want POST /oauth/token", r.Method, r.URL.Path)
	}
	if got, want := r.Header.Get("Content-Type"), "application/x-www-form-urlencoded"; got != want {
		f.t.Errorf("token request Content-Type = %q, want %q", got, want)
	}
	id, secret, ok := r.BasicAuth()
	if !ok || id != fixtureClientID || secret != fixtureClientSecret {
		f.t.Errorf("token request Basic auth = (%q, %q, %t), want the fixture client's ID and secret", id, secret, ok)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("token request form: %v", err)
	}
	if got, want := r.PostForm.Encode(), "grant_type=client_credentials&scope=read"; got != want {
		f.t.Errorf("token request form = %q, want %q", got, want)
	}
	_, _ = fmt.Fprintf(w, `{"access_token":%q,"expires_in":%d,"scope":"read","token_type":"Bearer"}`,
		fixtureAppToken(int(f.mints.Add(1))), f.expiresIn)
}

// fixtureAppToken is the nth token the fake endpoint mints.
func fixtureAppToken(n int) string {
	return fmt.Sprintf("lin_oauth_fixture%d", n)
}

// fakeLinearClient is an HTTP client whose requests all go to srv, whatever
// host they name.
func fakeLinearClient(srv *httptest.Server) *http.Client {
	client := srv.Client()
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}
	return client
}

func TestOAuthTokenProvider_SignsInAsTheApplication(t *testing.T) {
	endpoint := &fakeTokenEndpoint{t: t, expiresIn: thirtyDays}
	srv := httptest.NewServer(endpoint)
	defer srv.Close()

	provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
	got, err := provider.Authorization(t.Context())
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if want := "Bearer " + fixtureAppToken(1); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
