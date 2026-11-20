package ask

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// testPool returns a pgxpool against the integration DB or skips the
// test if it's not configured. Mirrors the pattern from other
// internal/* packages.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "ask"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE ask_sessions, ask_messages, ask_slack_sessions, mcp_tasks, prompt_release_activations, prompt_active_release, prompt_release_entries, prompt_releases RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestCreate(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	s, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == uuid.Nil {
		t.Fatal("zero UUID")
	}
	if s.FinishedAt != nil {
		t.Fatalf("new session should be active, got finished_at=%v", s.FinishedAt)
	}
	if time.Since(s.CreatedAt) > time.Minute {
		t.Fatalf("created_at far from now: %v", s.CreatedAt)
	}
}

// TestManagerOwnsSession pins who may continue a session over MCP or HTTP:
// its owner, by email whatever the case, and nobody for a session with no
// owner, a Slack thread's session, or one that does not exist.
func TestManagerOwnsSession(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	owned, err := mgr.CreateOwned(ctx, "Alex@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	ownerless, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slackSession, err := mgr.LoadOrCreateForSlackThread(ctx, "C1", "1700000000.000100", "alex@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		session uuid.UUID
		email   string
		want    bool
	}{
		{"owner", owned.ID, "alex@example.com", true},
		{"owner, other case", owned.ID, " ALEX@example.com ", true},
		{"another caller", owned.ID, "mallory@example.com", false},
		{"no email", owned.ID, "", false},
		{"ownerless session", ownerless.ID, "alex@example.com", false},
		{"slack thread started by the caller", slackSession, "alex@example.com", false},
		{"unknown session", uuid.New(), "alex@example.com", false},
	} {
		got, err := mgr.OwnsSession(ctx, c.session, c.email)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: OwnsSession = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRecordSessionProvenanceIsWriteOnce(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()
	session, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release, err := promptcatalog.NewStore(pool).EnsureSeed(ctx, promptcatalog.MustEmbedded(), "test-session-provenance")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfaceHTTP, "model-a", release.ID, testScopes); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfaceHTTP, "model-b", 0, testScopes); err != nil {
		t.Fatal(err)
	}
	var surface, model string
	var storedPromptID int64
	if err := pool.QueryRow(ctx, `SELECT surface, initial_model_id, prompt_release_id FROM ask_sessions WHERE id=$1`, session.ID).
		Scan(&surface, &model, &storedPromptID); err != nil {
		t.Fatal(err)
	}
	if surface != "http" || model != "model-a" || storedPromptID != release.ID {
		t.Fatalf("provenance rewritten: surface=%q model=%q prompt=%d", surface, model, storedPromptID)
	}

	withoutOptionalValues, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, withoutOptionalValues.ID, AskRunSurfaceHTTP, "", 0, testScopes); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, withoutOptionalValues.ID, AskRunSurfaceHTTP, "model-b", release.ID, testScopes); err != nil {
		t.Fatal(err)
	}
	var optionalModel *string
	var optionalPrompt *int64
	if err := pool.QueryRow(ctx, `SELECT surface, initial_model_id, prompt_release_id FROM ask_sessions WHERE id=$1`, withoutOptionalValues.ID).
		Scan(&surface, &optionalModel, &optionalPrompt); err != nil {
		t.Fatal(err)
	}
	if surface != "http" || optionalModel != nil || optionalPrompt != nil {
		t.Fatalf("intentional NULL provenance rewritten: surface=%q model=%v prompt=%v", surface, optionalModel, optionalPrompt)
	}
}

func TestSessionProvenanceRejectsCrossSurfaceContinuation(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()
	for _, original := range []AskRunSurface{AskRunSurfaceHTTP, AskRunSurfaceSlack, AskRunSurfaceScheduled} {
		session, err := mgr.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.RecordSessionProvenance(ctx, session.ID, original, "", 0, testScopes); err != nil {
			t.Fatal(err)
		}
		for _, next := range []AskRunSurface{AskRunSurfaceHTTP, AskRunSurfaceSlack, AskRunSurfaceScheduled} {
			err := mgr.RecordSessionProvenance(ctx, session.ID, next, "", 0, testScopes)
			if (err == nil) != (original == next) {
				t.Fatalf("surface %s -> %s: %v", original, next, err)
			}
		}
	}
	threadID, err := mgr.LoadOrCreateForSlackThread(ctx, "CPRIVATE", "1.1", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, threadID, AskRunSurfaceHTTP, "", 0, testScopes); err == nil {
		t.Fatal("unstarted Slack session accepted an HTTP turn")
	}
	if err := mgr.RecordSessionProvenance(ctx, threadID, AskRunSurfaceSlack, "", 0, testScopes); err != nil {
		t.Fatal(err)
	}
}

var testScopes = []string{"public", "internal"}

// A PagerDuty investigation attaches its alert thread to its session so Slack
// follow-ups continue it. The follow-up may only do so when its audience can
// already read everything the investigation ran with.
func TestSessionProvenanceLetsSlackContinuePagerDutyWithinItsAudience(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()
	investigationScopes := []string{"public", "internal", "slack-channel:CALERTS"}

	newInvestigation := func(t *testing.T) uuid.UUID {
		t.Helper()
		session, err := mgr.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfacePagerDuty, "model-a", 0, investigationScopes); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id) VALUES ('CALERTS', $1, $2)`,
			session.ID.String(), session.ID); err != nil {
			t.Fatal(err)
		}
		return session.ID
	}

	t.Run("covering audience continues", func(t *testing.T) {
		id := newInvestigation(t)
		followUp := []string{"internal", "public", "slack-channel:CALERTS", "slack-channel:CPUBLIC"}
		if err := mgr.RecordSessionProvenance(ctx, id, AskRunSurfaceSlack, "model-b", 0, followUp); err != nil {
			t.Fatalf("Slack follow-up refused: %v", err)
		}
		var surface, model string
		var scopes []string
		if err := pool.QueryRow(ctx, `SELECT surface, initial_model_id, initial_scopes FROM ask_sessions WHERE id=$1`, id).
			Scan(&surface, &model, &scopes); err != nil {
			t.Fatal(err)
		}
		if surface != "pagerduty" || model != "model-a" || len(scopes) != len(investigationScopes) {
			t.Fatalf("continuation rewrote provenance: surface=%q model=%q scopes=%v", surface, model, scopes)
		}
	})

	t.Run("narrower audience is refused", func(t *testing.T) {
		id := newInvestigation(t)
		if err := mgr.RecordSessionProvenance(ctx, id, AskRunSurfaceSlack, "", 0, []string{"public", "internal"}); err == nil {
			t.Fatal("Slack follow-up without the alert channel's scope continued the investigation")
		}
	})

	t.Run("other surfaces stay refused", func(t *testing.T) {
		id := newInvestigation(t)
		for _, next := range []AskRunSurface{AskRunSurfaceHTTP, AskRunSurfaceScheduled} {
			if err := mgr.RecordSessionProvenance(ctx, id, next, "", 0, investigationScopes); err == nil {
				t.Fatalf("%s turn continued a PagerDuty investigation", next)
			}
		}
	})

	t.Run("sessions without recorded scopes are refused", func(t *testing.T) {
		id := newInvestigation(t)
		if _, err := pool.Exec(ctx, `UPDATE ask_sessions SET initial_scopes = NULL WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := mgr.RecordSessionProvenance(ctx, id, AskRunSurfaceSlack, "", 0, investigationScopes); err == nil {
			t.Fatal("historical PagerDuty session without recorded scopes continued from Slack")
		}
	})

	t.Run("Slack sessions are not continued by PagerDuty", func(t *testing.T) {
		session, err := mgr.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfaceSlack, "", 0, investigationScopes); err != nil {
			t.Fatal(err)
		}
		if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfacePagerDuty, "", 0, investigationScopes); err == nil {
			t.Fatal("PagerDuty turn continued a Slack session")
		}
	})
}

func TestRecordSessionProvenanceRequiresScopes(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()
	session, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordSessionProvenance(ctx, session.ID, AskRunSurfaceHTTP, "", 0, nil); err == nil {
		t.Fatal("provenance recorded without scopes")
	}
}

func TestLoadOrCreateForSlackThread_FirstCall(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	id, err := mgr.LoadOrCreateForSlackThread(ctx, "C1", "1700000000.000100", "")
	if err != nil {
		t.Fatal(err)
	}
	if id == uuid.Nil {
		t.Fatal("zero UUID")
	}

	// Mapping row should exist.
	var stored uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT session_id FROM ask_slack_sessions WHERE channel_id='C1' AND thread_ts='1700000000.000100'`,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != id {
		t.Errorf("mapping points to %s, expected %s", stored, id)
	}

	// Session row should exist.
	var sessionID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM ask_sessions WHERE id = $1`, id,
	).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOrCreateForSlackThread_CacheHit(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	id1, err := mgr.LoadOrCreateForSlackThread(ctx, "C1", "T1", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := mgr.LoadOrCreateForSlackThread(ctx, "C1", "T1", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("got different ids on second call: %s vs %s", id1, id2)
	}

	// Only one session row should exist.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 session row, got %d", n)
	}
}

// TestLoadOrCreateForSlackThread_ConcurrentDoesNotOrphan exercises
// the race where two goroutines hit the slow path simultaneously.
// Both insert into ask_sessions, but the unique constraint on the
// mapping row forces one tx to rollback. The rollback discards its
// session row — no orphans.
func TestLoadOrCreateForSlackThread_ConcurrentDoesNotOrphan(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	// Run many concurrent creators on the same (channel, thread).
	const N = 20
	ids := make([]uuid.UUID, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := mgr.LoadOrCreateForSlackThread(ctx, "C1", "T1", "")
			ids[i] = id
			errs[i] = err
		}(i)
	}
	wg.Wait()

	// All calls succeeded with the same ID.
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("goroutine %d returned different id %s vs %s", i, ids[i], ids[0])
		}
	}

	// Exactly one session row.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 session row after race, got %d", n)
	}
}

// ownerEmailForSession reads back ask_sessions.owner_email, returning
// "" for SQL NULL so callers can compare against the empty string.
func ownerEmailForSession(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var owner *string
	if err := pool.QueryRow(context.Background(),
		`SELECT owner_email FROM ask_sessions WHERE id = $1`, id,
	).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return ""
	}
	return *owner
}

// TestLoadOrCreateForSlackThread_StampsOwnerOnCreate verifies the
// creating call stamps owner_email with the lowercased, trimmed asker
// email.
func TestLoadOrCreateForSlackThread_StampsOwnerOnCreate(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	id, err := mgr.LoadOrCreateForSlackThread(ctx, "C_OWNER", "T1", "  Alice@Example.com  ")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := ownerEmailForSession(t, pool, id), "alice@example.com"; got != want {
		t.Errorf("owner_email = %q, want %q", got, want)
	}
}

// TestLoadOrCreateForSlackThread_FollowUpDoesNotOverwriteOwner verifies
// the "original asker" guarantee: a follow-up call for the same
// (channel, thread) from a DIFFERENT asker must not overwrite the
// owner_email stamped on create.
func TestLoadOrCreateForSlackThread_FollowUpDoesNotOverwriteOwner(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	id1, err := mgr.LoadOrCreateForSlackThread(ctx, "C_OWNER2", "T1", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	id2, err := mgr.LoadOrCreateForSlackThread(ctx, "C_OWNER2", "T1", "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if id1 != id2 {
		t.Fatalf("follow-up returned different session id: %s vs %s", id1, id2)
	}
	if got, want := ownerEmailForSession(t, pool, id1), "alice@example.com"; got != want {
		t.Errorf("owner_email = %q, want %q (original asker must win)", got, want)
	}
}

// TestLoadOrCreateForSlackThread_EmptyEmailStoresNull verifies that
// creating with an empty askerEmail leaves owner_email NULL rather
// than storing an empty string.
func TestLoadOrCreateForSlackThread_EmptyEmailStoresNull(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	id, err := mgr.LoadOrCreateForSlackThread(ctx, "C_OWNER3", "T1", "")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := ownerEmailForSession(t, pool, id), ""; got != want {
		t.Errorf("owner_email = %q, want empty (NULL)", got)
	}
}

func TestWithSessionLock_Serializes(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	s, err := mgr.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	const N = 10

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := mgr.WithSessionLock(ctx, s.ID, func(ctx context.Context) error {
				cur := concurrent.Add(1)
				// Track peak concurrency.
				for {
					m := maxConcurrent.Load()
					if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				concurrent.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("WithSessionLock: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent = %d, want 1 (serialization broken)", got)
	}

	// Map should be empty after all locks released.
	mgr.mu.Lock()
	if got := len(mgr.locks); got != 0 {
		t.Errorf("locks map size = %d, want 0 after refcount drop", got)
	}
	mgr.mu.Unlock()
}

func TestWithSessionLock_DifferentSessionsDoNotBlock(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	s1, _ := mgr.Create(ctx)
	s2, _ := mgr.Create(ctx)

	// Each holds its session until the other holds its own too, which only
	// happens if neither blocks the other.
	var inside sync.WaitGroup
	inside.Add(2)
	both := make(chan struct{})
	go func() { inside.Wait(); close(both) }()
	var together atomic.Int32
	var wg sync.WaitGroup
	for _, id := range []uuid.UUID{s1.ID, s2.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mgr.WithSessionLock(ctx, id, func(ctx context.Context) error {
				inside.Done()
				select {
				case <-both:
					together.Add(1)
				case <-time.After(10 * time.Second):
				}
				return nil
			})
		}()
	}
	wg.Wait()

	if got := together.Load(); got != 2 {
		t.Errorf("%d of 2 sessions held their lock at the same time (different sessions should not block)", got)
	}
}

// TestWithSessionLock_CtxCancelUnblocksWaiter — a goroutine waiting
// on a held lock must unblock when its ctx is cancelled, not leak.
func TestWithSessionLock_CtxCancelUnblocksWaiter(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)

	s, _ := mgr.Create(context.Background())

	holderStarted := make(chan struct{})
	holderRelease := make(chan struct{})

	// Goroutine 1: holds the lock until we say so.
	go func() {
		_ = mgr.WithSessionLock(context.Background(), s.ID, func(ctx context.Context) error {
			close(holderStarted)
			<-holderRelease
			return nil
		})
	}()

	<-holderStarted

	// Goroutine 2: tries to acquire the same lock with a ctx we'll
	// cancel immediately. Should return ctx.Canceled, not block forever.
	waiterCtx, waiterCancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- mgr.WithSessionLock(waiterCtx, s.ID, func(ctx context.Context) error {
			t.Error("waiter should not have entered fn")
			return nil
		})
	}()

	// Give the waiter a moment to be parked on the channel.
	time.Sleep(20 * time.Millisecond)
	waiterCancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not unblock on ctx cancel")
	}

	close(holderRelease)
}

// TestSlackSessionState_NoRow: returns (false, zero, nil) when no
// ask_slack_sessions row exists for the thread.
func TestSlackSessionState_NoRow(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	known, ct, err := mgr.SlackSessionState(ctx, "C1", "1700000000.000100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if known {
		t.Error("expected known=false for non-existent session")
	}
	if !ct.IsZero() {
		t.Errorf("expected zero context_through, got %v", ct)
	}
}

// TestSlackSessionState_NullMark: a session row with a NULL
// context_through returns (true, zero, nil).
func TestSlackSessionState_NullMark(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	// Create session + mapping without setting context_through (NULL).
	_, err := mgr.LoadOrCreateForSlackThread(ctx, "C2", "1700000001.000100", "")
	if err != nil {
		t.Fatal(err)
	}

	known, ct, err := mgr.SlackSessionState(ctx, "C2", "1700000001.000100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !known {
		t.Error("expected known=true for existing session")
	}
	if !ct.IsZero() {
		t.Errorf("expected zero context_through for NULL column, got %v", ct)
	}
}

// TestAdvanceSlackContextThrough: advancing the mark sets it;
// subsequent advances to an earlier time do not move it backward
// (GREATEST invariant).
func TestAdvanceSlackContextThrough(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	ctx := context.Background()

	sessionID, err := mgr.LoadOrCreateForSlackThread(ctx, "C3", "1700000002.000100", "")
	if err != nil {
		t.Fatal(err)
	}

	t1 := time.Unix(1700000010, 0).UTC()
	t2 := time.Unix(1700000020, 0).UTC()
	t0 := time.Unix(1700000005, 0).UTC() // earlier than t1

	// Advance to t1.
	if err := mgr.AdvanceSlackContextThrough(ctx, sessionID, t1); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	_, ct, _ := mgr.SlackSessionState(ctx, "C3", "1700000002.000100")
	if !ct.Equal(t1) {
		t.Errorf("after first advance: got %v, want %v", ct, t1)
	}

	// Advance to t2 (forward — must update).
	if err := mgr.AdvanceSlackContextThrough(ctx, sessionID, t2); err != nil {
		t.Fatalf("second advance: %v", err)
	}
	_, ct, _ = mgr.SlackSessionState(ctx, "C3", "1700000002.000100")
	if !ct.Equal(t2) {
		t.Errorf("after second advance: got %v, want %v", ct, t2)
	}

	// Attempt to advance to t0 (earlier than current — must not regress).
	if err := mgr.AdvanceSlackContextThrough(ctx, sessionID, t0); err != nil {
		t.Fatalf("third advance (regression attempt): %v", err)
	}
	_, ct, _ = mgr.SlackSessionState(ctx, "C3", "1700000002.000100")
	if !ct.Equal(t2) {
		t.Errorf("mark must not regress: got %v, want %v (should still be t2)", ct, t2)
	}
}
