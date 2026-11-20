package sessioningest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

// TestUpsertChangesScopedAt_SkipsWriterConflictAndCommitsRestOfPage is a
// regression test for a feed page that contains one session whose
// session_ingest_bindings row is already held by push (writer=push)
// together with an unrelated, never-before-seen session.
//
// Trigger: UpsertChangesScopedAt drains a page with two changes for the
// same owner and client: okID (new, never seen before) and collidingID
// (already push-bound). bindSessionTx correctly refuses collidingID with
// ErrSessionWriterConflict (AGENTS.md: "push may never touch a feed
// session or another owner's").
//
// Before the fix: that error aborted the page's single all-or-nothing
// transaction, which also held okID's change and the cursor advance, so
// okID's legitimate change was lost and the cursor stayed put. Replaying
// the identical page (what Worker.drain does next tick) failed the same
// way forever, for every owner, not just collidingID's.
//
// Expected: only collidingID is left out. It is reported back (for
// logging/metrics), okID's change lands, and the cursor advances past the
// whole page.
func TestUpsertChangesScopedAt_SkipsWriterConflictAndCommitsRestOfPage(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		collidingID = "ses-collide-1"
		okID        = "ses-ok-1"
		owner       = "owner@example.com"
		client      = "codex"
	)

	// A push already owns collidingID.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindSessionTx(ctx, tx, collidingID, owner, WriterPush, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		change(cursor.ChangeSeq+2, collidingID, owner, client),
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only collidingID skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != collidingID {
		t.Fatalf("skipped = %+v, want exactly one entry for collidingID %q", skipped, collidingID)
	}
	if !errors.Is(skipped[0].Err, ErrSessionWriterConflict) {
		t.Fatalf("skipped[0].Err = %v, want ErrSessionWriterConflict", skipped[0].Err)
	}

	// okID's legitimate, unrelated change must have landed.
	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	// The cursor must have advanced past the whole page, including the
	// skipped session: one colliding session must not stall the drain.
	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}
}

// TestUpsertChangesScopedAt_SkipsInvalidVisibilityScopeAndCommitsRestOfPage
// is a regression test for the sibling deterministic, permanent per-change
// error in the very same UpsertChangesScopedAt loop: a decided visibility
// scope that is neither "internal" nor the change's own owner scope. No
// Policy shipped today ever decides this (Policy.VisibilityScope only
// returns internal or owner:canonical), but the decide callback's type
// signature accepts any ChangePolicy, so any caller can trigger it, and the
// loop must handle it exactly as it handles a writer conflict: skip only
// that change, never the rest of the page.
//
// Trigger: a page with two changes for the same owner and client: okID
// (decided internal, valid) and badID (decided "owner:someone-else",
// invalid for okID/badID's own owner).
//
// Before the fix: upsertCandidateTx's invalid-scope check returned a plain
// error that aborted the whole transaction the same way a writer conflict
// did, losing okID's change and leaving the cursor unmoved.
//
// Expected: only badID is left out and reported back; okID's change lands;
// the cursor advances past the whole page.
func TestUpsertChangesScopedAt_SkipsInvalidVisibilityScopeAndCommitsRestOfPage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		badID  = "ses-bad-scope-1"
		okID   = "ses-ok-2"
		owner  = "owner2@example.com"
		client = "codex"
	)

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(ch SessionChange) ChangePolicy {
		if ch.SessionID == badID {
			return ChangePolicy{Admitted: true, VisibilityScope: "owner:someone-else@example.com"}
		}
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		change(cursor.ChangeSeq+2, badID, owner, client),
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only badID skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != badID {
		t.Fatalf("skipped = %+v, want exactly one entry for badID %q", skipped, badID)
	}
	if !errors.Is(skipped[0].Err, ErrInvalidVisibilityScope) {
		t.Fatalf("skipped[0].Err = %v, want ErrInvalidVisibilityScope", skipped[0].Err)
	}

	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}
}

// TestUpsertChangesScopedAt_SkipsMalformedSourceTimePairAndCommitsRestOfPage
// covers a malformed feed row, a class of error distinct from the three
// binding/scope refusals above. Neither sessionfeed.Client.ListSessionChanges nor
// ClientFeed.ListSessionChanges validates a change before it reaches the
// database, and validateFeedPage only checks cursors, so a feed-reported
// source_started_at later than its source_updated_at would otherwise reach
// the INSERT and violate the session_ingest_candidates_source_time_pair
// CHECK. validateSessionChange catches exactly this pair before the
// transaction ever touches the row.
//
// Before the fix: that CHECK violation aborted the whole page's single
// transaction, losing okID's unrelated change and leaving the cursor
// unmoved; replaying the identical page failed identically forever.
//
// Expected: only badID is left out, reported back as ErrMalformedFeedChange;
// okID's change lands; the cursor advances past the whole page.
func TestUpsertChangesScopedAt_SkipsMalformedSourceTimePairAndCommitsRestOfPage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		badID  = "ses-bad-time-1"
		okID   = "ses-ok-3"
		owner  = "owner3@example.com"
		client = "codex"
	)

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	bad := change(cursor.ChangeSeq+2, badID, owner, client)
	// source_started_at later than source_updated_at: violates
	// session_ingest_candidates_source_time_pair.
	bad.SourceStartedAt = bad.SourceUpdatedAt.Add(time.Hour)
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		bad,
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only badID skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != badID {
		t.Fatalf("skipped = %+v, want exactly one entry for badID %q", skipped, badID)
	}
	if !errors.Is(skipped[0].Err, ErrMalformedFeedChange) {
		t.Fatalf("skipped[0].Err = %v, want ErrMalformedFeedChange", skipped[0].Err)
	}

	// okID's legitimate, unrelated change must have landed.
	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	// The cursor must have advanced past the whole page, including the
	// skipped row: one malformed row must not stall the drain.
	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}

	// Replaying the identical page must fail the same single row again,
	// deterministically, never the whole page -- the malformed row was
	// never written, so a later legitimate revision of the same session can
	// still land once the upstream source corrects it.
	_, skipped2, err := store.UpsertChangesScopedAt(ctx, []SessionChange{bad}, afterCursor.ChangeSeq, afterCursor.ChangeSeq+1, decide)
	if err != nil {
		t.Fatalf("replay failed the page: %v", err)
	}
	if len(skipped2) != 1 || skipped2[0].SessionID != badID {
		t.Fatalf("replay skipped = %+v, want exactly one entry for badID %q", skipped2, badID)
	}
}

// TestUpsertChangesScopedAt_SkipsNULByteRowAndCommitsRestOfPage is the
// sibling regression test for the other concrete malformed-row variant the
// review reproduced: a NUL byte in a text field, a different malformation
// than the time-pair CHECK above. Postgres text columns reject a NUL byte
// outright; validateSessionChange catches it in Go first, so the row never
// reaches the database at all.
func TestUpsertChangesScopedAt_SkipsNULByteRowAndCommitsRestOfPage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		badID  = "ses-bad-nul-1"
		okID   = "ses-ok-4"
		owner  = "owner4@example.com"
		client = "codex"
	)

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	bad := change(cursor.ChangeSeq+2, badID, owner, client)
	bad.OriginApp = "codex\x00-evil"
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		bad,
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only badID skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != badID {
		t.Fatalf("skipped = %+v, want exactly one entry for badID %q", skipped, badID)
	}
	if !errors.Is(skipped[0].Err, ErrMalformedFeedChange) {
		t.Fatalf("skipped[0].Err = %v, want ErrMalformedFeedChange", skipped[0].Err)
	}

	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}
}

// TestValidateSessionChange_FieldSweep is a field-by-field sweep: for
// every field validateSessionChange checks, both a
// value that must pass and, for each check that field is subject to, a
// value that must fail. See validateSessionChange's doc comment for which
// column each field maps to and why.
func TestValidateSessionChange_FieldSweep(t *testing.T) {
	base := change(1, "ses-valid-1", "owner@example.com", "codex")
	oversized := strings.Repeat("a", maxIdentityFieldBytes+1)

	cases := []struct {
		name    string
		mutate  func(c *SessionChange)
		wantErr bool
	}{
		{"valid baseline", func(c *SessionChange) {}, false},

		// Deliberately no "required" case: an empty session_id, owner or
		// client is still a legal NOT NULL text value (Postgres does not
		// reject it), and other tests in this package rely on that for
		// behavior unrelated to validation (e.g. TestPushBindingConflicts's
		// writer-conflict page, TestFeedOwnerChangeRebinds's owner probe).
		{"session_id empty is not malformed", func(c *SessionChange) { c.SessionID = "" }, false},
		{"owner empty", func(c *SessionChange) { c.Owner = "" }, true},
		{"client empty is not malformed", func(c *SessionChange) { c.Client = "" }, false},

		{"session_id invalid utf8", func(c *SessionChange) { c.SessionID = "ses-\xff\xfe" }, true},
		{"owner invalid utf8", func(c *SessionChange) { c.Owner = "owner-\xff\xfe" }, true},
		{"client invalid utf8", func(c *SessionChange) { c.Client = "client-\xff\xfe" }, true},
		{"owner_kind invalid utf8", func(c *SessionChange) { c.OwnerKind = "\xff\xfe" }, true},
		{"role invalid utf8", func(c *SessionChange) { c.Role = "\xff\xfe" }, true},
		{"origin_app invalid utf8", func(c *SessionChange) { c.OriginApp = "\xff\xfe" }, true},
		{"revision invalid utf8", func(c *SessionChange) { c.Revision = "\xff\xfe" }, true},

		{"session_id NUL byte", func(c *SessionChange) { c.SessionID = "ses-\x00-evil" }, true},
		{"owner NUL byte", func(c *SessionChange) { c.Owner = "owner\x00@example.com" }, true},
		{"client NUL byte", func(c *SessionChange) { c.Client = "cod\x00ex" }, true},
		{"owner_kind NUL byte", func(c *SessionChange) { c.OwnerKind = "hum\x00an" }, true},
		{"role NUL byte", func(c *SessionChange) { c.Role = "prim\x00ary" }, true},
		{"origin_app NUL byte", func(c *SessionChange) { c.OriginApp = "codex\x00-evil" }, true},
		{"revision NUL byte", func(c *SessionChange) { c.Revision = "sha256:\x00bad" }, true},

		{"session_id oversized", func(c *SessionChange) { c.SessionID = oversized }, true},
		{"owner oversized", func(c *SessionChange) { c.Owner = oversized }, true},
		{"client oversized", func(c *SessionChange) { c.Client = oversized }, true},
		// Unbounded fields: no length cap, only the NUL/UTF-8 checks above.
		{"owner_kind unbounded ok", func(c *SessionChange) { c.OwnerKind = oversized }, false},
		{"role unbounded ok", func(c *SessionChange) { c.Role = oversized }, false},
		{"origin_app unbounded ok", func(c *SessionChange) { c.OriginApp = oversized }, false},
		{"revision unbounded ok", func(c *SessionChange) { c.Revision = oversized }, false},

		{"record_count over int32 max", func(c *SessionChange) { c.RecordCount = math.MaxInt32 + 1 }, true},
		{"record_count under int32 min", func(c *SessionChange) { c.RecordCount = math.MinInt32 - 1 }, true},
		{"record_count at int32 max ok", func(c *SessionChange) { c.RecordCount = math.MaxInt32 }, false},
		{"user_turn_count over int32 max", func(c *SessionChange) { c.UserTurnCount = math.MaxInt32 + 1 }, true},
		{"user_turn_count under int32 min", func(c *SessionChange) { c.UserTurnCount = math.MinInt32 - 1 }, true},

		{"source time pair: started set, updated zero", func(c *SessionChange) { c.SourceUpdatedAt = time.Time{} }, true},
		{"source time pair: updated set, started zero", func(c *SessionChange) { c.SourceStartedAt = time.Time{} }, true},
		{"source time pair: both zero ok", func(c *SessionChange) {
			c.SourceStartedAt, c.SourceUpdatedAt = time.Time{}, time.Time{}
		}, false},
		{"source_updated_at before source_started_at", func(c *SessionChange) {
			c.SourceStartedAt = c.SourceUpdatedAt.Add(time.Hour)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := base
			tc.mutate(&ch)
			err := validateSessionChange(ch)
			if tc.wantErr && !errors.Is(err, ErrMalformedFeedChange) {
				t.Fatalf("validateSessionChange(%+v) = %v, want ErrMalformedFeedChange", ch, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateSessionChange(%+v) = %v, want nil", ch, err)
			}
		})
	}
}

// TestUpsertChangesScopedAt_SkipsOversizedIdentityFieldAndCommitsRestOfPage
// is an end-to-end regression test for one of the two malformed-row
// variants the round-2 review reproduced that the previous SQLSTATE-class
// classification did not catch: an oversized session_id (there, an
// 8000-hex-character id) blows the session_ingest_bindings_pkey /
// session_ingest_candidates_pkey btree index's per-tuple size limit and
// fails with a permanent 54000 (program_limit_exceeded), a class the old
// classification (22/23 only) never recognized -- so this row would have
// failed the whole page forever. validateSessionChange's length cap catches
// it in Go, before the row ever reaches Postgres.
func TestUpsertChangesScopedAt_SkipsOversizedIdentityFieldAndCommitsRestOfPage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		okID   = "ses-ok-5"
		owner  = "owner5@example.com"
		client = "codex"
	)
	badID := strings.Repeat("a", maxIdentityFieldBytes+1)

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		change(cursor.ChangeSeq+2, badID, owner, client),
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only the oversized id skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != badID {
		t.Fatalf("skipped = %+v, want exactly one entry for the oversized session_id", skipped)
	}
	if !errors.Is(skipped[0].Err, ErrMalformedFeedChange) {
		t.Fatalf("skipped[0].Err = %v, want ErrMalformedFeedChange", skipped[0].Err)
	}

	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}
}

// TestUpsertChangesScopedAt_SkipsRecordCountOverflowAndCommitsRestOfPage is
// the end-to-end regression test for the other malformed-row variant the
// round-2 review reproduced that the old classification missed: a
// record_count above int32's range. pgx refuses to encode it as the int4
// column's argument client-side, which never even produces a *pgconn.PgError
// (so permanentDBError, had it stayed, could not have classified it
// either) -- it would have failed the whole page forever.
// validateSessionChange's range check catches it in Go first.
func TestUpsertChangesScopedAt_SkipsRecordCountOverflowAndCommitsRestOfPage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		badID  = "ses-bad-overflow-1"
		okID   = "ses-ok-6"
		owner  = "owner6@example.com"
		client = "codex"
	)

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	bad := change(cursor.ChangeSeq+2, badID, owner, client)
	bad.RecordCount = math.MaxInt32 + 1
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, okID, owner, client),
		bad,
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+2, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt failed the whole page: %v (want: only badID skipped, the rest committed)", err)
	}
	if len(skipped) != 1 || skipped[0].SessionID != badID {
		t.Fatalf("skipped = %+v, want exactly one entry for badID %q", skipped, badID)
	}
	if !errors.Is(skipped[0].Err, ErrMalformedFeedChange) {
		t.Fatalf("skipped[0].Err = %v, want ErrMalformedFeedChange", skipped[0].Err)
	}

	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}

	afterCursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCursor.ChangeSeq != cursor.ChangeSeq+2 {
		t.Fatalf("cursor = %d, want it advanced to %d", afterCursor.ChangeSeq, cursor.ChangeSeq+2)
	}
}
