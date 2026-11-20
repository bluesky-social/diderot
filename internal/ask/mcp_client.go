package ask

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectMCP opens a streamable-HTTP session to one of Ask's
// operator-configured MCP servers with the official SDK.
//
// headers carry credentials and are set on every request. The endpoints are
// operator configuration and never redirect, so redirects are refused: Go's client keeps custom
// headers (and Authorization, for a same-host target) when it follows one,
// which would hand them to wherever the redirect points (the class of bug in
// GO-2026-6911).
//
// The session sends one request per call and opens no standing SSE stream:
// Ask uses no server-initiated messages, and the previous client didn't
// either.
func connectMCP(ctx context.Context, endpoint string, timeout time.Duration, headers map[string]string) (*mcp.ClientSession, error) {
	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: &mcpHeaderTransport{base: http.DefaultTransport, headers: headers},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "diderot-ask", Version: "1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
}

type mcpHeaderTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *mcpHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	return t.base.RoundTrip(req)
}
