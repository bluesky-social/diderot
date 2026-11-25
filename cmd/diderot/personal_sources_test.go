package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/source"
)

func TestPersonalSourcesUseVerifiedOwnerScopeAcrossAPIAndMCP(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_personal_sources")
	logger := discardLogger()
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	for name, body := range map[string]string{"imap": "Private inbox parcel", "caldav": "Private journal draft"} {
		_, err := sink.Upsert(ctx, events.Event{
			Source: name, ExternalID: "test", Scope: events.ScopeOwner("alice@example.com"),
			OccurredAt: time.Now().UTC(), Body: body,
		})
		require.NoError(t, err)
	}

	registry := sourceRegistry(pool, nil, nil, nil, nil, nil, false)
	teams, err := ask.NewLinearTeamCache(sink, logger)
	require.NoError(t, err)
	retriever := retrieve.NewRetriever(pool, nil, nil, logger)
	expander := newSourceExpander(registry, source.Deps{Pool: pool, Sink: sink})
	server, err := ask.NewCorpusMCPServer(retriever, registry.Infos(), expander, sink, attachmentReader{pool: pool},
		newSourceReferenceResolver(sink, expander), teams, func() ask.CorpusAskLease { return ask.CorpusAskLease{} }, pool, ctx, "test")
	require.NoError(t, err)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	for _, tc := range []struct {
		name, email       string
		verified, allowed bool
	}{
		{"owner", "ALICE@example.com", true, true},
		{"another user", "bob@example.com", true, false},
		{"unverified owner claim", "alice@example.com", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := auth.NewFixed(auth.Principal{Subject: tc.email, Email: tc.email, EmailVerified: tc.verified})
			api := adminapi.NewServer(caller, logger)
			api.SetRetriever(retriever)
			mux := http.NewServeMux()
			require.NoError(t, api.Mount(mux))
			require.NoError(t, api.MountMCP(mux, transport, "", nil, nil))

			args := `{"sources":["imap","caldav"]}`
			apiReq := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader(args))
			mcpBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list","arguments":%s,
			 "_meta":{%q:"2026-07-28",%q:{"name":"test-client","version":"1"},%q:{}}}}`,
				args, mcp.MetaKeyProtocolVersion, mcp.MetaKeyClientInfo, mcp.MetaKeyClientCapabilities)
			mcpReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpBody))
			mcpReq.Header.Set("Accept", "application/json, text/event-stream")
			mcpReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
			mcpReq.Header.Set("MCP-Method", "tools/call")
			mcpReq.Header.Set("MCP-Name", "list")

			for _, req := range []*http.Request{apiReq, mcpReq} {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer fixture")
				req.Header.Set("X-Auth-Request-User", "alice@example.com")
				res := httptest.NewRecorder()
				mux.ServeHTTP(res, req)
				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				require.NotContains(t, res.Body.String(), `"isError":true`)
				for _, text := range []string{"Private inbox parcel", "Private journal draft"} {
					if tc.allowed {
						require.Contains(t, res.Body.String(), text, req.URL.Path)
					} else {
						require.NotContains(t, res.Body.String(), text, req.URL.Path)
					}
				}
			}
		})
	}
}
