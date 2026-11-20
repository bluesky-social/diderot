package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// memoryMirror is object storage holding what a test puts in it.
type memoryMirror map[string][]byte

func (m memoryMirror) Put(_ context.Context, key, _ string, data []byte) error {
	m[key] = data
	return nil
}

func (m memoryMirror) Get(_ context.Context, key string) ([]byte, string, error) {
	data, ok := m[key]
	if !ok {
		return nil, "", errors.New("no such object")
	}
	return data, "image/png", nil
}

func (memoryMirror) Bucket() string { return "test" }

// TestCorpusMCPReadsSlackAttachmentsFromTheStore drives the production
// expander, event store, and attachment reader through the MCP server: a
// member of a private channel expands a message, sees its image and document,
// and fetches both; a caller outside the channel gets neither the list nor
// the files, with the answer a missing event or attachment gets.
func TestCorpusMCPReadsSlackAttachmentsFromTheStore(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_mcp_attachment"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, actor, body, raw, scope)
		VALUES ('slack', 'GLEGAL:1700000000.000100', now(), 'alice', 'contract and whiteboard attached', $1::jsonb, 'slack-channel:GLEGAL')
		RETURNING id`,
		`{"type":"message","channel":"GLEGAL","ts":"1700000000.000100","files":[
			{"id":"FIMG","name":"whiteboard.png","mimetype":"image/png","size":11,"mode":"hosted"},
			{"id":"FDOC","name":"contract.pdf","mimetype":"application/pdf","size":90000,"mode":"hosted"}]}`,
	).Scan(&eventID))
	for _, statement := range []string{
		`INSERT INTO slack_message_details (event_id, channel_id, has_files, occurred_at) VALUES ($1, 'GLEGAL', true, now())`,
		`WITH caption AS (
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			VALUES ($1, 'image_caption', 1, 'a whiteboard with the settlement terms', now()) RETURNING id)
		 INSERT INTO event_images (event_id, source_image_id, storage_key, media_type, bytes, captioned_at, chunk_id, occurred_at)
		 SELECT $1, 'FIMG', 'images/whiteboard', 'image/png', 11, now(), caption.id, now() FROM caption`,
		`WITH doc AS (
			INSERT INTO event_documents (event_id, source_file_id, occurred_at, filename, media_type, doc_format, extract_model, extracted_at, page_count, pages_extracted)
			VALUES ($1, 'FDOC', now(), 'contract.pdf', 'application/pdf', 'pdf', 'm', now(), 1, 1) RETURNING id)
		 INSERT INTO event_chunks (event_id, kind, chunk_idx, content, event_document_id, occurred_at)
		 SELECT $1, 'document_text', 2, 'The parties settle for one dollar.', doc.id, now() FROM doc`,
	} {
		_, err := pool.Exec(ctx, statement, eventID)
		require.NoError(t, err)
	}
	png := []byte("\x89PNG board")
	mirror := memoryMirror{"images/whiteboard": png}

	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	teams, err := ask.NewLinearTeamCache(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	registry := sourceRegistry(pool, nil, nil, nil, nil, nil, false)
	expander := newSourceExpander(registry, source.Deps{Pool: pool, Sink: sink})
	server, err := ask.NewCorpusMCPServer(nil, registry.Infos(), expander, sink, attachmentReader{pool: pool, mirror: mirror},
		newSourceReferenceResolver(sink, expander), teams, func() ask.CorpusAskLease { return ask.CorpusAskLease{} }, pool, ctx, "test")
	require.NoError(t, err)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	callerScopes := map[string][]string{
		"member@example.com":   {"public", "internal", "slack-channel:GLEGAL"},
		"outsider@example.com": {"public", "internal", "slack-channel:CGENERAL"},
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-Test-User")
		transport.ServeHTTP(w, r.WithContext(ask.WithMCPCaller(r.Context(), user, callerScopes[user])))
	}))
	t.Cleanup(httpServer.Close)

	call := func(user, tool string, arguments map[string]any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
			"name": tool, "arguments": arguments,
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "test-client", "version": "1"},
				mcp.MetaKeyClientCapabilities: map[string]any{},
			},
		}})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("MCP-Method", "tools/call")
		req.Header.Set("MCP-Name", tool)
		req.Header.Set("X-Test-User", user)
		resp, err := httpServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var decoded struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &decoded), string(raw))
		require.NotNil(t, decoded.Result, string(raw))
		return decoded.Result
	}

	expanded := call("member@example.com", "expand", map[string]any{"event_id": eventID})["structuredContent"].(map[string]any)
	require.Equal(t, true, expanded["supported"])
	listed := expanded["attachments"].([]any)
	require.Len(t, listed, 2)
	image, doc := listed[0].(map[string]any), listed[1].(map[string]any)
	require.Equal(t, "whiteboard.png", image["name"])
	require.Equal(t, "image/png", image["type"])
	require.Equal(t, "a whiteboard with the settlement terms", image["caption"])
	require.Equal(t, "contract.pdf", doc["name"])
	imageID, docID := image["id"].(string), doc["id"].(string)

	fetchedImage := call("member@example.com", "attachment", map[string]any{"id": imageID})
	require.NotEqual(t, true, fetchedImage["isError"])
	content := fetchedImage["content"].([]any)
	require.Len(t, content, 2)
	require.Equal(t, "image", content[1].(map[string]any)["type"])
	require.Equal(t, base64.StdEncoding.EncodeToString(png), content[1].(map[string]any)["data"])

	fetchedDoc := call("member@example.com", "attachment", map[string]any{"id": docID})["structuredContent"].(map[string]any)
	require.Equal(t, "The parties settle for one dollar.", fetchedDoc["text"])

	hidden := call("outsider@example.com", "expand", map[string]any{"event_id": eventID})["structuredContent"].(map[string]any)
	require.Equal(t, false, hidden["supported"])
	require.NotContains(t, hidden, "attachments")
	missing := call("outsider@example.com", "expand", map[string]any{"event_id": eventID + 1000})["structuredContent"].(map[string]any)
	require.Equal(t, missing["supported"], hidden["supported"])

	for _, id := range []string{imageID, docID} {
		refused := call("outsider@example.com", "attachment", map[string]any{"id": id})
		require.Equal(t, true, refused["isError"], id)
		raw, err := json.Marshal(refused)
		require.NoError(t, err)
		require.Contains(t, string(raw), "accessible from this context")
		require.NotContains(t, string(raw), "settle")
		require.NotContains(t, string(raw), base64.StdEncoding.EncodeToString(png))
	}
}
