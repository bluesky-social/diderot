package sessioningest

import (
	"context"
	"fmt"

	"github.com/bluesky-social/diderot/internal/events"
)

// Tests seed candidates as feed pages. Each helper saves its page from
// wherever the cursor stands, as a drain that had just loaded it would.

// upsertScopedAt fails on any skip: every test that seeds through these
// helpers expects its rows to land, so a silent skip here (a row a test
// didn't realize was malformed, or a stale binding another test left
// behind) must surface as a test failure, not pass unnoticed. A test that
// wants to exercise a skip calls UpsertChangesScopedAt directly instead.
func (s *Store) upsertScopedAt(ctx context.Context, changes []SessionChange, nextCursor int64, decide func(SessionChange) ChangePolicy) ([]Identity, error) {
	cursor, err := s.GetCursor(ctx)
	if err != nil {
		return nil, err
	}
	excluded, skipped, err := s.UpsertChangesScopedAt(ctx, changes, cursor.ChangeSeq, nextCursor, decide)
	if err == nil && len(skipped) > 0 {
		err = fmt.Errorf("sessioningest: test helper got %d unexpected skip(s), first %+v", len(skipped), skipped[0])
	}
	return excluded, err
}

// UpsertChangesAt saves a page with every admitted session visible to all.
func (s *Store) UpsertChangesAt(ctx context.Context, changes []SessionChange, nextCursor int64, admit func(SessionChange) bool) ([]Identity, error) {
	return s.upsertScopedAt(ctx, changes, nextCursor, func(ch SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: !ch.Deleted && admit(ch), VisibilityScope: events.ScopeInternal}
	})
}

// UpsertChanges is UpsertChangesAt advancing to the page's greatest change.
func (s *Store) UpsertChanges(ctx context.Context, changes []SessionChange, admit func(SessionChange) bool) ([]Identity, error) {
	var nextCursor int64
	for _, change := range changes {
		if change.ChangeSeq > nextCursor {
			nextCursor = change.ChangeSeq
		}
	}
	return s.UpsertChangesAt(ctx, changes, nextCursor, admit)
}
