package sessioningest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

func newPushTestServer(t *testing.T, limits PushLimits) (*httptest.Server, *PushHandler) {
	t.Helper()
	push, _, _ := newTestPushStore(t, func(c *PushConfig) { c.MaxDocumentBytes = 64 << 10 })
	if limits.MaxInflightBytes == 0 {
		limits.MaxInflightBytes = 1 << 20
	}
	h, err := NewPushHandler(push, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	route := func(w http.ResponseWriter, r *http.Request) {
		owner := r.Header.Get("X-Test-Owner")
		if r.Method == http.MethodPut {
			h.ServePut(w, r, owner)
			return
		}
		h.ServeGet(w, r, owner)
	}
	mux.HandleFunc("PUT /v1/ingest/agent-sessions/{session_id}", route)
	mux.HandleFunc("GET /v1/ingest/agent-sessions/{session_id}", route)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, h
}

type pushCall struct {
	method, id, owner, format string
	headers                   map[string]string
	body                      []byte
}

func (c pushCall) do(t *testing.T, srv *httptest.Server) (*http.Response, map[string]any) {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodPut
	}
	if c.id == "" {
		c.id = pushSession
	}
	if c.owner == "" {
		c.owner = "alice@example.com"
	}
	if c.format == "" {
		c.format = sessiondecode.FormatClaudeCode
	}
	req, err := http.NewRequestWithContext(context.Background(), c.method, srv.URL+"/v1/ingest/agent-sessions/"+c.id, bytes.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Owner", c.owner)
	req.Header.Set(PushFormatHeader, c.format)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func TestPushHTTPContract(t *testing.T) {
	srv, _ := newPushTestServer(t, PushLimits{MaxConcurrent: 4, MaxConcurrentPerOwner: 2})
	body := ccTurns(2, 10)

	for name, c := range map[string]struct {
		call pushCall
		want int
	}{
		"no precondition":      {pushCall{body: body}, http.StatusPreconditionRequired},
		"bad session id":       {pushCall{id: "NOT-A-UUID", body: body, headers: map[string]string{"If-None-Match": "*"}}, http.StatusBadRequest},
		"unsupported format":   {pushCall{format: "bsky.session.v1", body: body, headers: map[string]string{"If-None-Match": "*"}}, http.StatusBadRequest},
		"too large":            {pushCall{body: ccTurns(100, 1000), headers: map[string]string{"If-None-Match": "*"}}, http.StatusRequestEntityTooLarge},
		"undecodable":          {pushCall{body: []byte("nope\n"), headers: map[string]string{"If-None-Match": "*"}}, http.StatusUnprocessableEntity},
		"update before create": {pushCall{body: body, headers: map[string]string{"If-Match": `"hmac-sha256:00"`}}, http.StatusPreconditionFailed},
	} {
		if resp, _ := c.call.do(t, srv); resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, c.want)
		}
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(body)
	_ = zw.Close()
	resp, out := pushCall{body: gz.Bytes(), headers: map[string]string{"If-None-Match": "*", "Content-Encoding": "gzip"}}.do(t, srv)
	revision, _ := out["revision"].(string)
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(revision, "hmac-sha256:") || resp.Header.Get("ETag") != "" {
		t.Fatalf("create: %d %v etag=%q", resp.StatusCode, out, resp.Header.Get("ETag"))
	}
	resp, out = pushCall{body: body, headers: map[string]string{"If-None-Match": "*"}}.do(t, srv)
	if resp.StatusCode != http.StatusPreconditionFailed || resp.Header.Get("ETag") != `"`+revision+`"` ||
		out["body_matches_current"] != true || out["current_revision"] != revision {
		t.Fatalf("create retry: %d %v etag=%q", resp.StatusCode, out, resp.Header.Get("ETag"))
	}

	resp, _ = pushCall{method: http.MethodGet}.do(t, srv)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != `"`+revision+`"` || resp.Header.Get(PushFormatHeader) != sessiondecode.FormatClaudeCode {
		t.Fatalf("get: %d etag=%q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	resp, _ = pushCall{method: http.MethodHead}.do(t, srv)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != `"`+revision+`"` {
		t.Fatalf("head: %d", resp.StatusCode)
	}
	resp, _ = pushCall{method: http.MethodGet, headers: map[string]string{"If-None-Match": `W/"` + revision + `"`}}.do(t, srv)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional get: %d", resp.StatusCode)
	}
	if resp, _ := (pushCall{method: http.MethodGet, headers: map[string]string{"If-Match": `"hmac-sha256:stale"`}}).do(t, srv); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match on GET: %d", resp.StatusCode)
	}
	if resp, _ := (pushCall{method: http.MethodGet, owner: "mallory@example.com"}).do(t, srv); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other owner get: %d", resp.StatusCode)
	}

	resp, out = pushCall{body: ccTurns(3, 10), headers: map[string]string{"If-Match": `"` + revision + `"`}}.do(t, srv)
	if resp.StatusCode != http.StatusOK || out["revision"] == revision || resp.Header.Get("ETag") != "" {
		t.Fatalf("update: %d %v", resp.StatusCode, out)
	}
	if resp, _ := (pushCall{owner: "mallory@example.com", body: body, headers: map[string]string{"If-Match": "*"}}).do(t, srv); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other owner put: %d", resp.StatusCode)
	}
	if resp, _ := (pushCall{format: sessiondecode.FormatCodex, body: body, headers: map[string]string{"If-Match": "*"}}).do(t, srv); resp.StatusCode != http.StatusConflict {
		t.Fatalf("format change: %d", resp.StatusCode)
	}
	if resp, _ := (pushCall{body: body, headers: map[string]string{"If-Match": `W/"` + revision + `"`}}).do(t, srv); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("weak If-Match must not match: %d", resp.StatusCode)
	}
}

func TestPushHTTPAdmission(t *testing.T) {
	srv, h := newPushTestServer(t, PushLimits{MaxConcurrent: 2, MaxConcurrentPerOwner: 1})
	call := pushCall{body: ccTurns(1, 1), headers: map[string]string{"If-None-Match": "*"}}

	release, ok := h.admit(httptest.NewRecorder(), "alice@example.com", 1)
	if !ok {
		t.Fatal("first slot refused")
	}
	resp, _ := call.do(t, srv)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("per-owner bound: %d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	releaseBob, ok := h.admit(httptest.NewRecorder(), "bob@example.com", 1)
	if !ok {
		t.Fatal("second slot refused")
	}
	carol := call
	carol.owner = "carol@example.com"
	if resp, _ := carol.do(t, srv); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("process bound: %d", resp.StatusCode)
	}
	get := pushCall{method: http.MethodGet, owner: "alice@example.com"}
	if resp, _ := get.do(t, srv); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get before create: %d", resp.StatusCode)
	}
	release()
	releaseBob()
	if resp, _ := call.do(t, srv); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	release, _ = h.admit(httptest.NewRecorder(), "alice@example.com", 1)
	if resp, out := get.do(t, srv); resp.StatusCode != http.StatusTooManyRequests || out["error"] != "too_many_concurrent_pushes" {
		t.Fatalf("a GET that reads the body is admitted like a push: %d", resp.StatusCode)
	}
	head := get
	head.method = http.MethodHead
	if resp, _ := head.do(t, srv); resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(ccTurns(1, 1))) {
		t.Fatalf("HEAD reads no body and needs no slot: %d len=%d", resp.StatusCode, resp.ContentLength)
	}
	release()
	if _, err := NewPushHandler(h.store, PushLimits{MaxConcurrent: 1, MaxConcurrentPerOwner: 1, MaxInflightBytes: 1}, nil); err == nil {
		t.Fatal("a memory bound below one document must be refused at startup")
	}
}
