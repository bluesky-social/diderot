package zendesk

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/ratelimit"
)

func TestClientRequestBudget(t *testing.T) {
	for _, budget := range []int{150, 300} {
		client, err := NewClient("synthetic", "client", "secret", budget)
		require.NoError(t, err)
		require.Equal(t, rate.Limit(budget)/60, client.limiter.Limit())
	}
	for _, budget := range []int{0, -1} {
		_, err := NewClient("synthetic", "client", "secret", budget)
		require.ErrorContains(t, err, "requests per minute")
	}
}

func TestSharedCooldownAndQuotaExhaustion(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		headers http.Header
		delay   time.Duration
	}{
		{"retry_after", 429, http.Header{"Retry-After": {"5"}}, 5 * time.Second},
		{"missing_retry_after", 429, http.Header{}, time.Second},
		{"exhausted", 200, http.Header{"Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {"3"}}, 3 * time.Second},
		{"legacy_header", 200, http.Header{"X-Rate-Limit-Remaining": {"0"}, "Ratelimit-Reset": {"3"}}, 3 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, err := NewClient("synthetic", "client", "secret", 300)
				require.NoError(t, err)
				client.limiter = ratelimit.New(rate.Inf, 1)
				var calls atomic.Int32
				client.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						return &http.Response{StatusCode: test.status, Header: test.headers, Body: io.NopCloser(strings.NewReader("{}"))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				})
				request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, client.origin+"/api/v2/tickets/count.json", nil)
				require.NoError(t, err)
				response, err := client.do(request, client.httpClient, false)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				var workers sync.WaitGroup
				for range 4 {
					workers.Go(func() {
						response, err := client.do(request.Clone(context.Background()), client.httpClient, false)
						if err != nil {
							t.Error(err)
							return
						}
						_ = response.Body.Close()
					})
				}
				synctest.Wait()
				require.EqualValues(t, 1, calls.Load())
				time.Sleep(test.delay - time.Nanosecond)
				synctest.Wait()
				require.EqualValues(t, 1, calls.Load())
				time.Sleep(time.Nanosecond)
				workers.Wait()
				require.EqualValues(t, 5, calls.Load())
			})
		})
	}
}

func TestCooldownExtensionAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, err := NewClient("synthetic", "client", "secret", 300)
		require.NoError(t, err)
		client.observeRateLimit(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"5"}}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- client.limiter.Wait(ctx) }()
		synctest.Wait()
		time.Sleep(time.Second)
		client.observeRateLimit(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"8"}}})
		client.observeRateLimit(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"1"}}})
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case err := <-finished:
			t.Fatalf("a later, shorter cooldown released a blocked request (err=%v)", err)
		default:
		}
		cancel()
		require.ErrorIs(t, <-finished, context.Canceled)
	})
}

func TestConcurrentRequestsRemainPacedAfterOAuthAndCooldown(t *testing.T) {
	client, err := NewClient("synthetic", "client", "secret", 300)
	require.NoError(t, err)
	client.observeRateLimit(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"2"}}})
	start := time.Now()
	var lock sync.Mutex
	var times []time.Time
	client.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
		lock.Lock()
		times = append(times, time.Now())
		lock.Unlock()
		body := "{}"
		if request.URL.Path == "/oauth/tokens" {
			body = `{"access_token":"test-token","token_type":"bearer","scope":"read","expires_in":1800}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			var response any
			if err := client.get(context.Background(), "/api/v2/tickets/count.json", nil, true, &response); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	require.Len(t, times, 5)
	require.GreaterOrEqual(t, times[0].Sub(start), 1900*time.Millisecond)
	for index := 1; index < len(times); index++ {
		require.GreaterOrEqual(t, times[index].Sub(times[index-1]), 180*time.Millisecond)
	}
}

func TestCDNRateLimitsDoNotPauseTenantRequests(t *testing.T) {
	client, err := NewClient("synthetic", "client", "secret", 300)
	require.NoError(t, err)
	finalRequest, err := http.NewRequest(http.MethodGet, "https://cdn.example.test/file", nil)
	require.NoError(t, err)
	client.httpClient.Transport = fileTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader("")), Request: finalRequest}, nil
	})
	for _, location := range []string{finalRequest.URL.String(), client.origin + "/file-redirect"} {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, location, nil)
		require.NoError(t, err)
		response, err := client.do(request, client.httpClient, false)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.True(t, client.limiter.PausedUntil().IsZero())
	}
}
