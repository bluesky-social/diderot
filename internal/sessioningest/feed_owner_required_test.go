package sessioningest

import (
	"context"
	"errors"
	"testing"

	"github.com/bluesky-social/diderot/internal/events"
)

// A feed change with no owner would become a session visible to everyone
// that no one can make owner-only and no policy principal matches, so it is
// skipped like any other malformed change. A deletion is still applied.
func TestUpsertChangesScopedAt_SkipsAChangeWithNoOwner(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	cursor, err := store.GetCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}
	changes := []SessionChange{
		change(cursor.ChangeSeq+1, "ses-ok", "owner@example.com", "codex"),
		change(cursor.ChangeSeq+2, "ses-no-owner", "", "codex"),
		change(cursor.ChangeSeq+3, "ses-blank-owner", " \t", "codex"),
	}

	_, skipped, err := store.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, cursor.ChangeSeq+3, decide)
	if err != nil {
		t.Fatalf("UpsertChangesScopedAt: %v", err)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want the two ownerless changes", skipped)
	}
	for _, skip := range skipped {
		if !errors.Is(skip.Err, ErrMalformedFeedChange) {
			t.Fatalf("skipped %s with %v, want ErrMalformedFeedChange", skip.SessionID, skip.Err)
		}
	}
	var candidates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ingest_candidates WHERE owner_canonical = ''`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if candidates != 0 {
		t.Fatalf("%d ownerless candidates were stored", candidates)
	}
	if _, err := store.LoadCandidate(ctx, Identity{Owner: "owner@example.com", Client: "codex", SessionID: "ses-ok"}); err != nil {
		t.Fatalf("the owned change was lost: %v", err)
	}

	deleted := change(1, "ses-no-owner", "", "codex")
	deleted.Deleted = true
	if err := validateSessionChange(deleted); err != nil {
		t.Fatalf("a deletion with no owner must still be applied: %v", err)
	}
}
