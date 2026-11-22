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

func TestOAuthTokenProvider_ReusesItsToken(t *testing.T) {
	endpoint := &fakeTokenEndpoint{t: t, expiresIn: thirtyDays}
	srv := httptest.NewServer(endpoint)
	defer srv.Close()

	provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
	for call := 1; call <= 2; call++ {
		got, err := provider.Authorization(t.Context())
		if err != nil {
			t.Fatalf("Authorization, call %d: %v", call, err)
		}
		if want := "Bearer " + fixtureAppToken(1); got != want {
			t.Errorf("Authorization, call %d = %q, want %q", call, got, want)
		}
	}
	if got := endpoint.mints.Load(); got != 1 {
		t.Errorf("token requests = %d, want 1", got)
	}
}

func TestOAuthTokenProvider_InvalidateReplacesTheToken(t *testing.T) {
	endpoint := &fakeTokenEndpoint{t: t, expiresIn: thirtyDays}
	srv := httptest.NewServer(endpoint)
	defer srv.Close()

	provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
	if _, err := provider.Authorization(t.Context()); err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	provider.Invalidate()
	got, err := provider.Authorization(t.Context())
	if err != nil {
		t.Fatalf("Authorization after Invalidate: %v", err)
	}
	if want := "Bearer " + fixtureAppToken(2); got != want {
		t.Errorf("Authorization after Invalidate = %q, want %q", got, want)
	}
}

func TestClient_ReplacesARejectedApplicationToken(t *testing.T) {
	endpoint := &fakeTokenEndpoint{t: t, expiresIn: thirtyDays}
	mux := http.NewServeMux()
	mux.Handle("/oauth/token", endpoint)
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		switch got := r.Header.Get("Authorization"); got {
		case "Bearer " + fixtureAppToken(1):
			// Linear revoked the application's first token.
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer " + fixtureAppToken(2):
			_, _ = w.Write([]byte(`{"data":{}}`))
		default:
			t.Errorf("API request Authorization = %q, want the first or second application token", got)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	httpClient := fakeLinearClient(srv)
	client := NewClient(newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, httpClient))
	client.httpClient = httpClient
	if _, _, err := client.Do(t.Context(), "query { viewer { id } }", nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := endpoint.mints.Load(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
}

func TestOAuthTokenProvider_ReplacesATokenAboutToExpire(t *testing.T) {
	// Half an hour of life is inside the hour's margin the provider keeps.
	endpoint := &fakeTokenEndpoint{t: t, expiresIn: 30 * 60}
	srv := httptest.NewServer(endpoint)
	defer srv.Close()

	provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
	for call := 1; call <= 2; call++ {
		got, err := provider.Authorization(t.Context())
		if err != nil {
			t.Fatalf("Authorization, call %d: %v", call, err)
		}
		if want := "Bearer " + fixtureAppToken(call); got != want {
			t.Errorf("Authorization, call %d = %q, want %q", call, got, want)
		}
	}
}

func TestOAuthTokenProvider_ReportsARefusedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":%q}`, fixtureErrorBody)
	}))
	defer srv.Close()

	provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
	got, err := provider.Authorization(t.Context())
	if err == nil {
		t.Fatalf("Authorization = %q, want an error", got)
	}
	// The whole message: the status, and neither the answer's body nor the
	// client secret.
	if got, want := err.Error(), "linear: OAuth token request: HTTP 401"; got != want {
		t.Errorf("Authorization error = %q, want %q", got, want)
	}
}

func TestOAuthTokenProvider_RefusesAnUnusableAnswer(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		wantErr string
	}{
		{
			name:    "not JSON",
			answer:  "<html>" + fixtureErrorBody + "</html>",
			wantErr: "linear: unusable OAuth token answer: not JSON",
		},
		{
			name:    "no access token",
			answer:  `{"expires_in":2591999,"scope":"read","token_type":"Bearer"}`,
			wantErr: "linear: unusable OAuth token answer: no access token",
		},
		{
			name:    "another token type",
			answer:  `{"access_token":"lin_oauth_fixture1","expires_in":2591999,"scope":"read","token_type":"mac"}`,
			wantErr: "linear: unusable OAuth token answer: token type is not Bearer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.answer))
			}))
			defer srv.Close()

			provider := newOAuthTokenProvider(fixtureClientID, fixtureClientSecret, fakeLinearClient(srv))
			got, err := provider.Authorization(t.Context())
			if err == nil {
				t.Fatalf("Authorization = %q, want an error", got)
			}
			// The whole message, so nothing of the answer is in it.
			if got := err.Error(); got != tt.wantErr {
				t.Errorf("Authorization error = %q, want %q", got, tt.wantErr)
			}
		})
	}
}
