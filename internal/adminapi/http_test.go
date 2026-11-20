package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testServerForHelpers(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return newTestServer("secret-token", logger)
}

// TestWriteJSON_StatusAndContentType pins that writeJSON sets both
// the status line and the JSON content type, and that a 200 caller
// gets a 200 (the old status-less helper always wrote 200; the
// converged helper must preserve that for OK callers).
func TestWriteJSON_StatusAndContentType(t *testing.T) {
	s := testServerForHelpers(t)
	rr := httptest.NewRecorder()
	s.writeJSON(rr, http.StatusCreated, map[string]string{"hello": "world"})

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not valid JSON: %v; body=%s", err, rr.Body.String())
	}
	if got["hello"] != "world" {
		t.Errorf("body = %v", got)
	}
}

// TestReadJSON_DecodesBody confirms readJSON populates the target.
func TestReadJSON_DecodesBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x",
		bytes.NewBufferString(`{"event_id":123}`))
	var got expandChunkRequest
	if err := readJSON(req, &got); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if got.EventID != 123 {
		t.Errorf("EventID = %d, want 123", got.EventID)
	}
}

// TestReadJSON_ErrorShape confirms a malformed body surfaces via
// badRequest as a 400 with the converged "bad request: <detail>"
// prefix the handlers now share.
func TestReadJSON_ErrorShape(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x",
		bytes.NewBufferString(`{not json`))
	var got expandChunkRequest
	err := readJSON(req, &got)
	if err == nil {
		t.Fatal("expected decode error on malformed body")
	}
	rr := httptest.NewRecorder()
	badRequest(rr, err)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if !strings.HasPrefix(rr.Body.String(), "bad request: ") {
		t.Errorf("body = %q, want a 'bad request: ' prefix", rr.Body.String())
	}
}

// TestRequireBackend_NilInterface confirms a nil interface backend
// trips the 503 path with the supplied message.
func TestRequireBackend_NilInterface(t *testing.T) {
	rr := httptest.NewRecorder()
	var b SyncWorker // nil interface
	ok := requireBackend(rr, b, "backend not configured")
	if ok {
		t.Fatal("requireBackend should report not-wired for a nil interface")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "backend not configured") {
		t.Errorf("body = %q, want the unavailable message", rr.Body.String())
	}
}

// TestRequireBackend_NilPointer is the typed-nil footgun guard: a nil
// *pgxpool.Pool boxed into an interface is non-nil under a plain
// ==nil check, so requireBackend must reflect to detect it.
func TestRequireBackend_NilPointer(t *testing.T) {
	rr := httptest.NewRecorder()
	var pool *pgxpool.Pool // typed nil
	ok := requireBackend(rr, pool, "pool not configured")
	if ok {
		t.Fatal("requireBackend should report not-wired for a nil *pgxpool.Pool")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestRequireBackend_WiredPasses confirms a non-nil backend passes
// without writing anything to the response.
func TestRequireBackend_WiredPasses(t *testing.T) {
	rr := httptest.NewRecorder()
	var b SyncWorker = stubSyncWorker{}
	ok := requireBackend(rr, b, "backend not configured")
	if !ok {
		t.Fatal("requireBackend should report wired for a non-nil backend")
	}
	if rr.Code != http.StatusOK {
		// httptest defaults the code to 200 until something writes;
		// requireBackend must not have written a status.
		t.Errorf("status = %d, want untouched (200 default)", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rr.Body.String())
	}
}

// stubSyncWorker is a no-op SyncWorker used only to give
// requireBackend a non-nil interface value to inspect.
type stubSyncWorker struct{}

func (stubSyncWorker) RequestBackfill(_ context.Context, _ string) (bool, error) {
	return false, errors.New("stub")
}
