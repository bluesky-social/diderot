package zendesk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/ratelimit"
)

type fileTransport func(*http.Request) (*http.Response, error)

func (transport fileTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func seedFile(t *testing.T, worker *Worker, scope, location string) string {
	t.Helper()
	ctx := context.Background()
	result, err := worker.Sink.Upsert(ctx, events.Event{
		Source: "zendesk", ExternalID: "file-parent", Scope: scope, Body: "file parent",
		OccurredAt: fixtureTicket().CreatedAt,
	})
	require.NoError(t, err)
	_, err = worker.Pool.Exec(ctx, `INSERT INTO zendesk_files (source_file_id, event_id, content_url, media_type)
		VALUES ('file', $1, $2, 'image/png')`, result.EventID, location)
	require.NoError(t, err)
	return "file"
}

func TestFileCredentialsStayOnProtectedTenantOrigin(t *testing.T) {
	for _, test := range []struct {
		name, scope, location string
		authenticated         bool
	}{
		{"protected-tenant", events.ScopeZendesk, "https://synthetic.zendesk.com/file", true},
		{"public-tenant", events.ScopePublic, "https://synthetic.zendesk.com/file", false},
		{"protected-cdn", events.ScopeZendesk, "https://cdn.example.test/file", false},
		{"lookalike", events.ScopeZendesk, "https://synthetic.zendesk.com.example.test/file", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
			require.NoError(t, err)
			client.token, client.tokenExpiry = "synthetic-token", time.Now().Add(time.Hour)
			client.limiter = ratelimit.New(rate.Inf, 1)
			worker := testWorker(t, client)
			fileID := seedFile(t, worker, test.scope, test.location)
			fetcher := NewFileFetcher(worker.Pool, client)
			calls := 0
			fetcher.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if test.authenticated && calls == 1 {
					require.Equal(t, "Bearer synthetic-token", request.Header.Get("Authorization"))
				} else {
					require.Empty(t, request.Header.Get("Authorization"))
				}
				require.Empty(t, request.Header.Get("Cookie"))
				if calls == 1 {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://synthetic.zendesk.com/redirected"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("synthetic image")), Request: request}, nil
			})
			body, mediaType, err := fetcher.Fetch(context.Background(), fileID)
			require.NoError(t, err)
			require.Equal(t, "synthetic image", string(body))
			require.Equal(t, "image/png", mediaType)
			require.Equal(t, 2, calls)
		})
	}
}

func TestFileFetcherRejectsUnsafeAndDeletedFiles(t *testing.T) {
	for _, location := range []string{
		"http://synthetic.zendesk.com/file", "https://user:password@synthetic.zendesk.com/file",
		"https://synthetic.zendesk.com:8443/file", "file:///etc/passwd",
		"https://127.0.0.1/file", "https://169.254.169.254/latest/meta-data/",
	} {
		t.Run(location, func(t *testing.T) {
			client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
			require.NoError(t, err)
			worker := testWorker(t, client)
			fileID := seedFile(t, worker, events.ScopeZendesk, location)
			_, _, err = NewFileFetcher(worker.Pool, client).Fetch(context.Background(), fileID)
			var permanent *files.PermanentError
			require.ErrorAs(t, err, &permanent)
			require.NotContains(t, err.Error(), "password")
			require.NotContains(t, err.Error(), "synthetic-secret")
		})
	}
	t.Run("deleted-parent", func(t *testing.T) {
		client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
		require.NoError(t, err)
		worker := testWorker(t, client)
		fileID := seedFile(t, worker, events.ScopePublic, "https://synthetic.zendesk.com/file")
		_, err = worker.Sink.Delete(context.Background(), "zendesk", "file-parent")
		require.NoError(t, err)
		fetcher := NewFileFetcher(worker.Pool, client)
		fetcher.httpClient.Transport = fileTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("deleted parent must not download")
			return nil, nil
		})
		_, _, err = fetcher.Fetch(context.Background(), fileID)
		var permanent *files.PermanentError
		require.ErrorAs(t, err, &permanent)
	})
}

func TestFileFetcherBoundsDownload(t *testing.T) {
	client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
	require.NoError(t, err)
	worker := testWorker(t, client)
	fileID := seedFile(t, worker, events.ScopePublic, "https://synthetic.zendesk.com/file")
	fetcher := NewFileFetcher(worker.Pool, client)
	fetcher.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (25<<20)+1))), Request: request}, nil
	})
	_, _, err = fetcher.Fetch(context.Background(), fileID)
	var permanent *files.PermanentError
	require.ErrorAs(t, err, &permanent)
}

func TestFileFetcherOAuthRenewalAndTransientFailures(t *testing.T) {
	for _, test := range []struct {
		name           string
		tokenStatus    int
		fileStatus     int
		wantTokenCalls int
		wantFileCalls  int
		wantTransient  bool
	}{
		{"renew-after-401", 200, 200, 2, 2, false},
		{"persistent-401", 200, 401, 2, 2, true},
		{"exchange-failure", 401, 200, 1, 0, true},
		{"exchange-rate-limit", 429, 200, 1, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
			require.NoError(t, err)
			client.limiter = ratelimit.New(rate.Inf, 1)
			tokenCalls, fileCalls := 0, 0
			client.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
				require.Equal(t, "/oauth/tokens", request.URL.Path)
				require.Equal(t, http.MethodPost, request.Method)
				tokenCalls++
				body := fmt.Sprintf(`{"access_token":"token-%d","token_type":"bearer","scope":"read","expires_in":1800}`, tokenCalls)
				return &http.Response{StatusCode: test.tokenStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			worker := testWorker(t, client)
			fileID := seedFile(t, worker, events.ScopeZendesk, "https://synthetic.zendesk.com/file")
			fetcher := NewFileFetcher(worker.Pool, client)
			fetcher.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
				fileCalls++
				require.Equal(t, fmt.Sprintf("Bearer token-%d", fileCalls), request.Header.Get("Authorization"))
				status := test.fileStatus
				if fileCalls == 1 {
					status = http.StatusUnauthorized
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("file")), Request: request}, nil
			})
			body, _, err := fetcher.Fetch(context.Background(), fileID)
			if test.wantTransient {
				var transient *files.TransientError
				require.ErrorAs(t, err, &transient)
			} else {
				require.NoError(t, err)
				require.Equal(t, "file", string(body))
			}
			require.Equal(t, test.wantTokenCalls, tokenCalls)
			require.Equal(t, test.wantFileCalls, fileCalls)
		})
	}
}

func TestFileFetcherUnauthenticatedDownloadsDoNotExchangeTokens(t *testing.T) {
	for _, test := range []struct{ scope, location string }{
		{events.ScopePublic, "https://synthetic.zendesk.com/file"},
		{events.ScopeZendesk, "https://cdn.example.test/file"},
	} {
		client, err := NewClient("synthetic", "synthetic-client", "synthetic-secret", 300)
		require.NoError(t, err)
		client.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
			t.Error("anonymous attachment attempted token exchange")
			return nil, fmt.Errorf("unexpected OAuth exchange")
		})
		worker := testWorker(t, client)
		fileID := seedFile(t, worker, test.scope, test.location)
		fetcher := NewFileFetcher(worker.Pool, client)
		fetcher.httpClient.Transport = fileTransport(func(request *http.Request) (*http.Response, error) {
			require.Empty(t, request.Header.Get("Authorization"))
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("file")), Request: request}, nil
		})
		_, _, err = fetcher.Fetch(context.Background(), fileID)
		require.NoError(t, err)
	}
}
