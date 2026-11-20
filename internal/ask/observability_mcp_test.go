package ask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// GO-2026-6911: an MCP server client must not follow a redirect and carry
// its credential or service identity to another host.
func TestMCPToolServerDoesNotFollowRedirects(t *testing.T) {
	var (
		mu     sync.Mutex
		leaked []string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Forwarded-User"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server, err := ConnectMCPToolServer(ctx, MCPToolServerOptions{
		Name:        "grafana",
		Kind:        MCPServerGrafana,
		Endpoint:    redirect.URL,
		Token:       "secret",
		ActorHeader: "X-Forwarded-User",
		Tools:       []string{"grafana_query_prometheus"},
		Timeout:     5 * time.Second,
	})
	if err == nil {
		_ = server.Close()
		t.Fatal("expected the redirect response to fail initialization")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("observability MCP client followed a redirect, target saw %q", leaked)
	}
}
