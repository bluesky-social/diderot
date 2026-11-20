package zendesk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOAuthCachesAndRenewsAcrossConcurrentRequests(t *testing.T) {
	var exchanges atomic.Int32
	client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/oauth/tokens", request.URL.Path)
		respond(t, writer, map[string]any{
			"access_token": fmt.Sprintf("token-%d", exchanges.Add(1)), "token_type": "bearer", "scope": "read", "expires_in": 1800,
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	token, err := client.accessToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "token-1", token)
	cancel()
	token, err = client.accessToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "token-1", token)
	require.EqualValues(t, 1, exchanges.Load())

	client.tokenExpiry = time.Now().Add(20 * time.Second)
	var workers sync.WaitGroup
	failures := make(chan error, 24)
	for range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			value, err := client.accessToken(context.Background())
			if err != nil {
				failures <- err
			} else if value != "token-2" {
				failures <- errors.New("unexpected cached token")
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, exchanges.Load())
}

func TestOAuthUnauthorizedRenewsOnce(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("still-denied=%t", denied), func(t *testing.T) {
			var exchanges, reads atomic.Int32
			client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/oauth/tokens" {
					respond(t, writer, map[string]any{
						"access_token": fmt.Sprintf("token-%d", exchanges.Add(1)), "token_type": "bearer", "scope": "read", "expires_in": 1800,
					})
					return
				}
				require.Equal(t, http.MethodGet, request.Method)
				count := reads.Add(1)
				require.Equal(t, fmt.Sprintf("Bearer token-%d", count), request.Header.Get("Authorization"))
				if count == 1 || denied {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				respond(t, writer, map[string]any{})
			})
			var result any
			err := client.get(context.Background(), "/api/v2/tickets/12.json", nil, true, &result)
			if denied {
				var failure *APIError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, http.StatusUnauthorized, failure.Status)
				require.Empty(t, client.token)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 2, exchanges.Load())
			require.EqualValues(t, 2, reads.Load())
		})
	}
}

func TestOAuthRejectsUnsafeGrantsAndSanitizesFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"bad-credentials", 401, `{"error":"synthetic-secret"}`},
		{"rate-limit", 429, `{"error":"synthetic-secret"}`},
		{"server-failure", 503, `{"error":"synthetic-secret"}`},
		{"invalid-json", 200, `synthetic-secret`},
		{"oversize", 200, strings.Repeat("synthetic-secret", 6000)},
		{"write-scope", 200, `{"access_token":"sensitive-token","token_type":"bearer","scope":"read write","expires_in":1800}`},
		{"absent-scope", 200, `{"access_token":"sensitive-token","token_type":"bearer","expires_in":1800}`},
		{"absent-expiry", 200, `{"access_token":"sensitive-token","token_type":"bearer","scope":"read"}`},
		{"expired", 200, `{"access_token":"sensitive-token","token_type":"bearer","scope":"read","expires_in":0}`},
		{"overflow-expiry", 200, `{"access_token":"sensitive-token","token_type":"bearer","scope":"read","expires_in":9223372036854775807}`},
		{"absent-token", 200, `{"token_type":"bearer","scope":"read","expires_in":1800}`},
		{"header-injection", 200, `{"access_token":"sensitive-token\r\nInjected: value","token_type":"bearer","scope":"read","expires_in":1800}`},
		{"wrong-type", 200, `{"access_token":"sensitive-token","token_type":"basic","scope":"read","expires_in":1800}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exchanges atomic.Int32
			client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "/oauth/tokens", request.URL.Path)
				exchanges.Add(1)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			})
			for range 2 {
				token, err := client.accessToken(context.Background())
				require.Error(t, err)
				require.Empty(t, token)
				require.NotContains(t, err.Error(), "synthetic-secret")
				require.NotContains(t, err.Error(), "sensitive-token")
				require.False(t, deferTicketError(err))
			}
			require.Empty(t, client.token)
			require.EqualValues(t, 2, exchanges.Load())
		})
	}
}

func TestOAuthExchangeNeverFollowsRedirects(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, destination.URL, status)
		})
		_, err := client.accessToken(context.Background())
		require.ErrorContains(t, err, fmt.Sprint(status))
	}
	require.Zero(t, destinationCalls.Load())
}

func TestOAuthAnonymousReadsDoNotExchangeOrSendCredentials(t *testing.T) {
	client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/api/v2/help_center/en-us/articles/1.json", request.URL.Path)
		require.Empty(t, request.Header.Get("Authorization"))
		respond(t, writer, map[string]any{})
	})
	client.token, client.tokenExpiry = "sensitive-token", time.Now().Add(time.Hour)
	var result any
	require.NoError(t, client.get(context.Background(), "/api/v2/help_center/en-us/articles/1.json", nil, false, &result))
}

func TestOAuthRefusesOtherOriginsAndSourceWrites(t *testing.T) {
	client := testOAuthClient(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Error("invalid authenticated request reached server")
	})
	for _, target := range []struct{ method, location string }{
		{http.MethodGet, "https://other.example.test/file"},
		{http.MethodPost, client.origin + "/api/v2/tickets"},
	} {
		request, err := http.NewRequest(target.method, target.location, nil)
		require.NoError(t, err)
		_, err = client.do(request, client.httpClient, true)
		require.ErrorContains(t, err, "read-only tenant boundary")
	}
}

func TestOAuthCancelledContextDoesNotPoisonLaterRequests(t *testing.T) {
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.accessToken(ctx)
	require.ErrorIs(t, err, context.Canceled)
	token, err := client.accessToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "synthetic-token", token)
}
