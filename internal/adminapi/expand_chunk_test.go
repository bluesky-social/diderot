package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/ask"
)

// fakeExpander answers Expand from a canned table. Each entry can
// inject either a result, a sentinel error, or both (sentinels carry
// partial result data — e.g. ErrSourceNotExpandable returns Source
// populated so the caller can surface what kind of row was hit).
type fakeExpander struct {
	rows map[int64]fakeExpanderRow
}

type fakeExpanderRow struct {
	res ask.ExpandResult
	err error
}

func (f *fakeExpander) Expand(_ context.Context, eventID int64) (ask.ExpandResult, error) {
	r, ok := f.rows[eventID]
	if !ok {
		return ask.ExpandResult{}, ask.ErrExpandEventNotFound
	}
	return r.res, r.err
}

// expandChunkSetup wires a Server with the supplied Expander plus a
// real status pool, seeding an in-scope (public) events row for every
// event_id the fake expander knows about. The pool is required because
// handleExpandChunk runs a scope gate (eventInScope) before delegating
// to the Expander — the fake alone isn't enough anymore. The admin
// token is fixed at "secret-token".
//
// inScopeIDs is the set of event_ids that should pass the scope gate;
// pass the same ids the fakeExpander is keyed on. An id NOT seeded here
// is treated as not-found by the gate (404) before the expander is
// consulted — which is what the not-found test wants.
func expandChunkSetup(t *testing.T, exp Expander, inScopeIDs ...int64) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := newTestServer("secret-token", logger)
	s.SetExpander(exp)
	pool := servicesTestPool(t)
	s.SetStatusPool(pool)
	for _, id := range inScopeIDs {
		seedScopedEventAtID(t, pool, id, "public")
	}
	return s
}

// seedScopedEventAtID inserts an events row at an explicit id with the
// given scope. OVERRIDING SYSTEM VALUE is needed because events.id is
// GENERATED ALWAYS AS IDENTITY; the expand tests key off fixed ids
// (42, 7, ...) that must line up with the fake expander's table.
func seedScopedEventAtID(t *testing.T, pool *pgxpool.Pool, id int64, scope string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO events
			(id, source, external_id, occurred_at, body, raw, deleted, scope)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'test', $2, now(), 'x', '{}'::jsonb, false, $3)
	`, id, fmt.Sprintf("expand:%d", id), scope)
	if err != nil {
		t.Fatalf("seed event id=%d: %v", id, err)
	}
}

func postExpand(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/expand-chunk", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	rr := httptest.NewRecorder()
	s.authed(s.handleExpandChunk)(rr, req)
	return rr
}

// decodeResponse parses the JSON envelope; tests assert against it.
func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) expandChunkResponse {
	t.Helper()
	var resp expandChunkResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	return resp
}

func TestExpandChunk_AgentSessionHappyPath(t *testing.T) {
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		42: {res: ask.ExpandResult{
			Source:  "agent_session",
			Content: "user: hello\nassistant: hi back\n",
		}},
	}}
	s := expandChunkSetup(t, exp, 42)

	rr := postExpand(t, s, `{"event_id": 42}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeResponse(t, rr)
	if got.Source != "agent_session" {
		t.Errorf("Source = %q", got.Source)
	}
	if !got.Supported {
		t.Errorf("Supported should be true")
	}
	if !strings.Contains(got.Content, "hi back") {
		t.Errorf("Content = %q", got.Content)
	}
}

func TestExpandChunk_SlackHappyPath(t *testing.T) {
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		7: {res: ask.ExpandResult{
			Source:  "slack",
			Content: "Thread in #ops (3 messages)...",
		}},
	}}
	s := expandChunkSetup(t, exp, 7)

	rr := postExpand(t, s, `{"event_id": 7}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	got := decodeResponse(t, rr)
	if got.Source != "slack" {
		t.Errorf("Source = %q", got.Source)
	}
	if !got.Supported {
		t.Errorf("Supported should be true")
	}
	if !strings.Contains(got.Content, "Thread in #ops") {
		t.Errorf("Content = %q", got.Content)
	}
}

func TestExpandChunk_NotFoundReturns404(t *testing.T) {
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{}}
	s := expandChunkSetup(t, exp)
	rr := postExpand(t, s, `{"event_id": 999}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestExpandChunk_NotExpandableReturns200WithSupportedFalse(t *testing.T) {
	// Source exists but its backend is unconfigured — surface the
	// "supported=false" envelope rather than a hard error so the
	// caller can fall back gracefully.
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		1: {res: ask.ExpandResult{Source: "agent_session"},
			err: ask.ErrSourceNotExpandable},
	}}
	s := expandChunkSetup(t, exp, 1)

	rr := postExpand(t, s, `{"event_id": 1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeResponse(t, rr)
	if got.Source != "agent_session" {
		t.Errorf("Source = %q", got.Source)
	}
	if got.Supported {
		t.Errorf("Supported should be false")
	}
	if got.Note == "" {
		t.Errorf("Note should be populated")
	}
}

func TestExpandChunk_503WhenExpanderMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := newTestServer("secret-token", logger)
	rr := postExpand(t, s, `{"event_id": 1}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestExpandChunk_RejectsZeroEventID(t *testing.T) {
	exp := &fakeExpander{}
	s := expandChunkSetup(t, exp)
	rr := postExpand(t, s, `{"event_id": 0}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestExpandChunk_PropagatesRealError(t *testing.T) {
	// A genuine failure (DB error, session feed unreachable not as a
	// configured-off case) should bubble up as a 500 rather than
	// the structured "supported=false" envelope.
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		1: {res: ask.ExpandResult{Source: "agent_session"},
			err: errors.New("upstream session feed exploded")},
	}}
	s := expandChunkSetup(t, exp, 1)
	rr := postExpand(t, s, `{"event_id": 1}`)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

func TestExpandChunk_PropagatesWrappedSentinel(t *testing.T) {
	// Wrapping with %w should still resolve to ErrExpandEventNotFound
	// → 404. errors.Is composition on the sentinel is load-bearing.
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		1: {err: wrappedErr{
			inner: ask.ErrExpandEventNotFound,
			msg:   "from a deeper layer",
		}},
	}}
	// Seed id 1 in-scope so the scope gate passes and the EXPANDER is
	// actually invoked — otherwise the gate would short-circuit to 404
	// and this test wouldn't exercise sentinel resolution at all.
	s := expandChunkSetup(t, exp, 1)
	rr := postExpand(t, s, `{"event_id": 1}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("wrapped sentinel should resolve to 404; got %d", rr.Code)
	}
}

func TestExpandChunk_TruncatedFlagPasses(t *testing.T) {
	// When the expander reports truncated content, the JSON envelope
	// surfaces the flag.
	exp := &fakeExpander{rows: map[int64]fakeExpanderRow{
		3: {res: ask.ExpandResult{
			Source:    "slack",
			Content:   "abridged thread...",
			Truncated: true,
		}},
	}}
	s := expandChunkSetup(t, exp, 3)
	rr := postExpand(t, s, `{"event_id": 3}`)
	got := decodeResponse(t, rr)
	if !got.Truncated {
		t.Errorf("Truncated should be true; envelope=%+v", got)
	}
}

// wrappedErr is the same %w-shaped helper used elsewhere in this
// package's tests. Restated here because the previous shared
// implementation lived alongside the old fakeEventLookup helpers
// that this rewrite removed.
type wrappedErr struct {
	inner error
	msg   string
}

func (w wrappedErr) Error() string { return w.msg + ": " + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }
