package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/auth"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// claimsWithEmail builds a verified-claims stand-in for the given
// operator email.
func claimsWithEmail(email string) *auth.Principal {
	return &auth.Principal{
		Subject: "sub-" + email,
		Email:   email,
		Display: strings.SplitN(email, "@", 2)[0],
	}
}

// poolBackedAskRunner is a minimal AskRunner for the ownership tests.
// CreateSession inserts a real ask_sessions row (so the handler's
// owner_email UPDATE + the continuation ownership SELECT operate on
// actual data); RunTurn records that it was reached and returns runErr.
type poolBackedAskRunner struct {
	pool          *pgxpool.Pool
	ranTurns      int
	runErr        error
	validationErr error
	askers        []*ask.AskerInfo
	requests      []ask.TurnRequest
}

type fakeAskerResolver struct {
	info   *ask.AskerInfo
	err    error
	emails []string
}

func (r *fakeAskerResolver) AskerForEmail(_ context.Context, email string) (*ask.AskerInfo, error) {
	r.emails = append(r.emails, email)
	return r.info, r.err
}

func (r *poolBackedAskRunner) CreateSession(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx,
		`INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (r *poolBackedAskRunner) ValidateModelOverrides(ask.ModelOverrides) error {
	return r.validationErr
}

func (r *poolBackedAskRunner) ExecuteTurn(_ context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	r.ranTurns++
	r.askers = append(r.askers, req.Asker)
	r.requests = append(r.requests, req)
	return ask.TurnResult{Reply: "ok"}, r.runErr
}

// newTestServerAs builds a Server whose verified caller has the given
// email, wired to a pool-backed ask runner + status pool.
func newTestServerAs(t *testing.T, pool *pgxpool.Pool, email string) (*Server, *poolBackedAskRunner) {
	t.Helper()
	s := newTestServer("tok", nil)
	// Override the fake verifier's email to the caller under test.
	s.authenticator = fakeVerifier{
		validToken: "tok",
		claims:     claimsWithEmail(email),
	}
	s.SetStatusPool(pool)
	runner := &poolBackedAskRunner{pool: pool}
	s.SetAskRunner(t.Context(), runner)
	return s, runner
}

func postAsk(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", stringReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.authed(s.handleAsk)(rr, req)
	return rr
}

func TestHTTPRejectsOwnedSharedSession(t *testing.T) {
	pool := askSessionPool(t)
	ctx := context.Background()
	server, runner := newTestServerAs(t, pool, "alice@example.com")
	manager := ask.NewManager(pool, nil)
	for _, surface := range []string{"", "unknown", "slack", "http"} {
		t.Run("mapped_"+surface, func(t *testing.T) {
			sessionID, err := manager.LoadOrCreateForSlackThread(ctx, "CPRIVATE", "thread-"+surface, "alice@example.com")
			if err != nil {
				t.Fatal(err)
			}
			if surface != "" {
				if _, err := pool.Exec(ctx, `UPDATE ask_sessions SET surface=$2 WHERE id=$1`, sessionID, surface); err != nil {
					t.Fatal(err)
				}
			}
			response := postAsk(t, server, `{"session_id":"`+sessionID.String()+`","question":"read my private data"}`)
			if response.Code != http.StatusNotFound {
				t.Fatalf("HTTP continuation: %d %s", response.Code, response.Body.String())
			}
			if allowed, err := server.callerOwnsSession(ctx, sessionID, "alice@example.com"); err != nil || allowed {
				t.Fatalf("artifact ownership guard: allowed=%v err=%v", allowed, err)
			}
		})
	}
	for _, surface := range []string{"slack", "scheduled"} {
		session, err := manager.CreateOwned(ctx, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE ask_sessions SET surface=$2 WHERE id=$1`, session.ID, surface); err != nil {
			t.Fatal(err)
		}
		response := postAsk(t, server, `{"session_id":"`+session.ID.String()+`","question":"continue"}`)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s continuation: %d %s", surface, response.Code, response.Body.String())
		}
	}
	if runner.ranTurns != 0 {
		t.Fatalf("shared sessions ran %d personal turns", runner.ranTurns)
	}
}

// askSessionPool truncates the ask tables so each test starts clean.
func askSessionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "adminapi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE ask_sessions, ask_messages, ask_slack_sessions RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatal(err)
	}
	return pool
}

// TestAskSessionOwnership: create-as-A, then continue-as-A works;
// continue-as-B returns the SAME shape as a bogus session id.
func TestAskSessionOwnership(t *testing.T) {
	pool := askSessionPool(t)

	// A creates a session.
	sA, runnerA := newTestServerAs(t, pool, "alice@example.com")
	rrCreate := postAsk(t, sA, `{"question": "hi"}`)
	if rrCreate.Code != http.StatusOK {
		t.Fatalf("create: status = %d; body=%s", rrCreate.Code, rrCreate.Body.String())
	}
	var created askResponse
	if err := json.Unmarshal(rrCreate.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" {
		t.Fatal("create returned empty session id")
	}
	if runnerA.ranTurns != 1 {
		t.Fatalf("create should run 1 turn, ran %d", runnerA.ranTurns)
	}
	if len(runnerA.askers) != 1 || runnerA.askers[0] == nil {
		t.Fatalf("create did not pass authenticated asker: %#v", runnerA.askers)
	}
	if got := runnerA.askers[0]; got.DisplayName != "alice" || got.Email != "alice@example.com" {
		t.Fatalf("authenticated asker = %#v", got)
	}
	if got := runnerA.requests[0]; got.Surface != ask.AskRunSurfaceHTTP ||
		len(got.AllowedScopes) == 0 {
		t.Fatalf("HTTP turn request = %#v", got)
	}

	// A continues its own session — works.
	rrContinueA := postAsk(t, sA, `{"session_id": "`+created.SessionID+`", "question": "more"}`)
	if rrContinueA.Code != http.StatusOK {
		t.Fatalf("A continuation: status = %d; body=%s", rrContinueA.Code, rrContinueA.Body.String())
	}

	// B tries to continue A's session — must be refused with the SAME
	// shape as a bogus/unknown session id.
	sB, _ := newTestServerAs(t, pool, "bob@example.com")
	rrContinueB := postAsk(t, sB, `{"session_id": "`+created.SessionID+`", "question": "steal"}`)

	bogusID := uuid.New().String()
	rrBogus := postAsk(t, sB, `{"session_id": "`+bogusID+`", "question": "whatever"}`)

	if rrContinueB.Code != rrBogus.Code {
		t.Fatalf("status differs: cross-owner=%d bogus=%d (must match — no ownership oracle)",
			rrContinueB.Code, rrBogus.Code)
	}
	if rrContinueB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-owner continuation, got %d; body=%s",
			rrContinueB.Code, rrContinueB.Body.String())
	}
	// Bodies must match modulo the session id echoed in the message.
	// Normalize by replacing each session id with a placeholder.
	normB := strings.ReplaceAll(rrContinueB.Body.String(), created.SessionID, "SID")
	normBogus := strings.ReplaceAll(rrBogus.Body.String(), bogusID, "SID")
	if normB != normBogus {
		t.Errorf("cross-owner and bogus bodies differ:\n cross-owner: %q\n bogus:       %q",
			normB, normBogus)
	}
}

func TestHandleAskForwardsModelOverrides(t *testing.T) {
	pool := askSessionPool(t)
	s, runner := newTestServerAs(t, pool, "alice@example.com")
	rr := postAsk(t, s, `{"question":"compare this","reasoning_effort":"medium","service_tier":"fast","reasoning_effort_schedule":{"after_tool_rounds":2,"effort":"high"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests = %d", len(runner.requests))
	}
	got := runner.requests[0].ModelOverrides
	if got.ReasoningEffort == nil || *got.ReasoningEffort != "medium" || got.ServiceTier == nil || *got.ServiceTier != "fast" ||
		got.ReasoningEffortSchedule == nil || got.ReasoningEffortSchedule.AfterToolRounds != 2 || got.ReasoningEffortSchedule.Effort != "high" {
		t.Fatalf("model overrides = %#v", got)
	}
}

func TestHandleAskRejectsModelOverridesBeforeCreatingSession(t *testing.T) {
	pool := askSessionPool(t)
	s, runner := newTestServerAs(t, pool, "alice@example.com")
	runner.validationErr = errors.New("model does not support service tier")
	rr := postAsk(t, s, `{"question":"compare this","service_tier":"fast"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var sessions int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ask_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || runner.ranTurns != 0 {
		t.Fatalf("invalid override created %d sessions and ran %d turns", sessions, runner.ranTurns)
	}
}

func TestHandleAskEnrichesHTTPAskerFromSharedResolver(t *testing.T) {
	pool := askSessionPool(t)
	s, runner := newTestServerAs(t, pool, "Alice@Example.com")
	resolver := &fakeAskerResolver{info: &ask.AskerInfo{
		DisplayName: "Alice Example",
		Handle:      "alice",
		Email:       "unverified-alias@example.com",
		SlackUserID: "U_ALICE",
		LocalTime:   time.Now().In(time.FixedZone("PDT", -7*60*60)),
		TZ:          "America/Los_Angeles",
		TZLabel:     "Pacific Daylight Time",
	}}
	s.SetAskerResolver(resolver)

	rr := postAsk(t, s, `{"question":"what did I do today?"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(resolver.emails) != 1 || resolver.emails[0] != "alice@example.com" {
		t.Fatalf("resolver emails = %v", resolver.emails)
	}
	if len(runner.askers) != 1 || runner.askers[0] == nil {
		t.Fatalf("runner askers = %#v", runner.askers)
	}
	got := runner.askers[0]
	if got.DisplayName != "Alice Example" || got.Handle != "alice" ||
		got.Email != "alice@example.com" || got.SlackUserID != "U_ALICE" ||
		got.TZ != "America/Los_Angeles" || got.LocalTime.IsZero() {
		t.Fatalf("enriched asker = %#v", got)
	}
}

func TestAskSessionIgnoresInvokingAgentSession(t *testing.T) {
	pool := askSessionPool(t)
	s, _ := newTestServerAs(t, pool, "alice@example.com")
	rr := postAsk(t, s, `{
		"question":"what changed?",
		"invoking_session_id":"native-session",
		"invoking_session_client":"codex",
		"invoking_feed_session_id":"not-a-session-lookup"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var created askResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := pool.QueryRow(context.Background(), `
		SELECT owner_email FROM ask_sessions WHERE id=$1`, created.SessionID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "alice@example.com" {
		t.Fatalf("session owner = %q", owner)
	}
}

// TestAskSessionNullOwnerNotContinuable: a session with NULL owner
// (e.g. a pre-migration session, or one whose asker email couldn't be
// resolved) is not continuable via the HTTP handler by anyone — same
// not-found shape.
func TestAskSessionNullOwnerNotContinuable(t *testing.T) {
	pool := askSessionPool(t)
	// Insert a session directly with NULL owner_email (models the
	// pre-migration / unresolved-asker case).
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	s, _ := newTestServerAs(t, pool, "alice@example.com")
	rr := postAsk(t, s, `{"session_id": "`+id.String()+`", "question": "continue slack sess"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("NULL-owner continuation: status = %d, want 404; body=%s",
			rr.Code, rr.Body.String())
	}
}
