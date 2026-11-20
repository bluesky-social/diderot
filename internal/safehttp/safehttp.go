// Package safehttp builds the HTTP clients that connectors and tools reach
// their services with. Each call site states its client's timeout, what it
// does with a redirect, and whether it may dial only public addresses, so
// none of that is left to net/http's defaults by accident.
package safehttp

import (
	"fmt"
	"net/http"
	"time"
)

// Options is one client's policy. The zero value is net/http's own: no
// timeout, up to ten redirects followed, the default transport.
type Options struct {
	// Timeout bounds a whole request, reading the response body included.
	// Zero is no limit.
	Timeout time.Duration
	// CheckRedirect decides each redirect, as http.Client.CheckRedirect
	// does. Nil follows up to ten; NoFollow hands the redirect response
	// back unfollowed; MaxRedirects(n) follows up to n.
	CheckRedirect func(req *http.Request, via []*http.Request) error
	// FailOnRedirect fails the request when the server answers with any
	// 3xx status, so CheckRedirect never sees one.
	FailOnRedirect bool
	// PublicOnly dials only public addresses, and never through a proxy:
	// for URLs that someone outside Diderot chose, such as an image link
	// in a page. It fails with *BlockedAddressError otherwise.
	PublicOnly bool
}

// NewClient returns a client with opts' policy.
func NewClient(opts Options) *http.Client {
	var transport http.RoundTripper
	if opts.PublicOnly {
		transport = publicOnlyTransport()
	}
	if opts.FailOnRedirect {
		next := transport
		if next == nil {
			next = http.DefaultTransport
		}
		transport = &failOnRedirect{next: next}
	}
	return &http.Client{Timeout: opts.Timeout, CheckRedirect: opts.CheckRedirect, Transport: transport}
}

// NoFollow is a CheckRedirect that hands the redirect response itself back
// to the caller, unfollowed.
func NoFollow(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// MaxRedirects returns a CheckRedirect that follows at most n redirects.
func MaxRedirects(n int) func(*http.Request, []*http.Request) error {
	return func(_ *http.Request, via []*http.Request) error {
		if len(via) >= n {
			return fmt.Errorf("stopped after %d redirects", n)
		}
		return nil
	}
}

// failOnRedirect turns any 3xx response into an error, for a service that
// should never redirect: a redirect there is misconfiguration or an attempt
// to send its credentials elsewhere.
type failOnRedirect struct {
	next http.RoundTripper
}

func (t *failOnRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("safehttp: unexpected redirect to %s (status %d); redirect refused",
			resp.Header.Get("Location"), resp.StatusCode)
	}
	return resp, nil
}
