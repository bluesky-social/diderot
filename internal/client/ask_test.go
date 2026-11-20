package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAskServer answers ask as Diderot does: a task, polled with tasks/get.
type fakeAskServer struct {
	mu      sync.Mutex
	methods []string
	headers []http.Header
	polls   int
	// finishAfter is how many polls answer working; 0 never finishes.
	finishAfter int
	result      map[string]any
}

func (f *fakeAskServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.methods = append(f.methods, req.Method)
	f.headers = append(f.headers, r.Header.Clone())
	var result any
	switch req.Method {
	case "tools/call":
		result = map[string]any{"resultType": "task", "taskId": "task-1", "status": "working", "pollIntervalMs": 10}
	case "tasks/get":
		f.polls++
		if f.finishAfter == 0 || f.polls < f.finishAfter {
			result = map[string]any{"resultType": "complete", "taskId": "task-1", "status": "working", "pollIntervalMs": 10}
		} else {
			result = map[string]any{"resultType": "complete", "taskId": "task-1", "status": "completed", "result": f.result}
		}
	case "resources/read":
		result = map[string]any{"contents": []map[string]any{{
			"uri": req.Params["uri"], "mimeType": "image/png", "blob": base64.StdEncoding.EncodeToString([]byte("png bytes")),
		}}}
	default:
		result = map[string]any{"resultType": "complete"}
	}
	f.mu.Unlock()
	// Answer as an event stream, as a streamable HTTP server may.
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	_, _ = w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
}

const (
	testSession  = "11111111-1111-4111-8111-111111111111"
	testArtifact = "22222222-2222-4222-8222-222222222222"
)

func TestAskPollsTheTaskToItsAnswer(t *testing.T) {
	fake := &fakeAskServer{finishAfter: 3, result: map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "{}"},
			{"type": "resource_link", "uri": "diderot://ask/" + testSession + "/artifacts/" + testArtifact, "name": testArtifact},
		},
		"structuredContent": map[string]any{
			"session_id": testSession, "reply": "The answer, citing [a thread](https://example.com/t).",
			"artifacts": []map[string]any{{"uri": "diderot://ask/" + testSession + "/artifacts/" + testArtifact, "media_type": "image/png", "byte_size": 9, "title": "Chart"}},
		},
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	m := &MCP{URL: srv.URL + "/mcp", HTTP: NewHTTPClient(), Version: "test"}

	answer, err := m.Ask(t.Context(), "What happened?", testSession)
	require.NoError(t, err)
	assert.Equal(t, testSession, answer.SessionID)
	assert.Contains(t, answer.Reply, "citing")
	require.Len(t, answer.Artifacts, 1)
	assert.Equal(t, []string{"tools/call", "tasks/get", "tasks/get", "tasks/get"}, fake.methods)
	assert.Equal(t, "ask", fake.headers[0].Get("Mcp-Name"))
	assert.Equal(t, "task-1", fake.headers[1].Get("Mcp-Name"))
	assert.Equal(t, ProtocolVersion, fake.headers[0].Get("Mcp-Protocol-Version"))

	dir := t.TempDir()
	path, err := m.SaveArtifact(t.Context(), answer.Artifacts[0], dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, testArtifact+".png"), path)
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	require.NoError(t, err)
	assert.Equal(t, "png bytes", string(b))
	_, err = m.SaveArtifact(t.Context(), answer.Artifacts[0], dir)
	assert.ErrorIs(t, err, os.ErrExist, "an existing file is never replaced")

	_, err = m.SaveArtifact(t.Context(), Artifact{URI: "diderot://ask/" + testSession + "/artifacts/../../etc/passwd"}, dir)
	assert.ErrorContains(t, err, "not an ask artifact")
}

func TestAskCancelledCancelsTheTask(t *testing.T) {
	fake := &fakeAskServer{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	m := &MCP{URL: srv.URL + "/mcp", HTTP: NewHTTPClient(), Version: "test"}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := m.Ask(ctx, "A long question", "")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, "tasks/cancel", fake.methods[len(fake.methods)-1])
}

func TestAskRefusedBeforeATask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"isError": true, "content": []map[string]any{{"type": "text", "text": "Ask is not open yet."}},
		}})
	}))
	t.Cleanup(srv.Close)
	m := &MCP{URL: srv.URL + "/mcp", HTTP: NewHTTPClient()}
	_, err := m.Ask(t.Context(), "Hello?", "")
	var toolErr *ToolError
	require.ErrorAs(t, err, &toolErr)
	assert.Equal(t, "Ask is not open yet.", toolErr.Message)
}
