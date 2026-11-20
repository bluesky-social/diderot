package notion

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestParseRootInput(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			"dashed UUID",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"undashed UUID",
			"abcd1234ef567890abcd1234567890ab",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"upper-case UUID becomes lower",
			"ABCD1234-EF56-7890-ABCD-1234567890AB",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"URL with workspace + slug",
			"https://www.notion.so/some-workspace/Some-Page-abcd1234ef567890abcd1234567890ab",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"URL no workspace",
			"https://notion.so/Some-Page-abcd1234ef567890abcd1234567890ab",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"URL trailing slash",
			"https://www.notion.so/some-workspace/Some-Page-abcd1234ef567890abcd1234567890ab/",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"URL with ?p= block deep-link",
			"https://www.notion.so/some-workspace/Some-Page?p=abcd1234ef567890abcd1234567890ab",
			"abcd1234-ef56-7890-abcd-1234567890ab",
			false,
		},
		{
			"empty",
			"",
			"",
			true,
		},
		{
			"garbage",
			"not a uuid",
			"",
			true,
		},
		{
			"URL no UUID",
			"https://www.notion.so/some-workspace/no-uuid-here",
			"",
			true,
		},
		{
			"too-short hex",
			"abcd1234",
			"",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRootInput(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRootInput(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got=%q, want=%q", got, tt.want)
			}
		})
	}
}

func TestParseRootReferencePreservesViewAndTypedKinds(t *testing.T) {
	viewID := "aaaaaaaa-1111-4222-8333-444444444444"
	got, err := ParseRootReference("https://app.notion.com/p/0123456789abcdef0123456789abcdef?v=" + strings.ReplaceAll(viewID, "-", ""))
	if err != nil {
		t.Fatalf("ParseRootReference(view URL): %v", err)
	}
	if got.Type != RootTypeView || got.ObjectID != viewID {
		t.Fatalf("view ref=%+v, want view:%s", got, viewID)
	}

	got, err = ParseRootReference("data_source:bbbbbbbb-1111-4222-8333-444444444444")
	if err != nil {
		t.Fatalf("ParseRootReference(data source): %v", err)
	}
	if got.Type != RootTypeDataSource {
		t.Fatalf("typed ref=%+v, want data_source", got)
	}

	if _, err := ParseRootReference("https://example.com/page-abcd1234ef567890abcd1234567890ab"); !IsValidationError(err) {
		t.Fatalf("foreign host err=%v, want ValidationError", err)
	}
}

// rootsTestDBURL skips the test if no DB is available. Mirrors
// the events package's helper.
func rootsTestDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "notion")
}

func TestRootsStore_Lifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Clean slate.
	if _, err := pool.Exec(ctx, `TRUNCATE notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store := NewRootsStore(pool)

	// Empty list.
	if rs, err := store.List(ctx); err != nil {
		t.Fatalf("List empty: %v", err)
	} else if len(rs) != 0 {
		t.Errorf("len(empty list)=%d, want 0", len(rs))
	}

	// Add via URL form.
	id, err := store.Add(ctx,
		"https://www.notion.so/team/Team-Home-abcd1234ef567890abcd1234567890ab",
		"team home", "alex@example.com")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != "abcd1234-ef56-7890-abcd-1234567890ab" {
		t.Errorf("id=%q, want canonicalized", id)
	}

	// List sees it.
	rs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].PageID != id || rs[0].Label != "team home" {
		t.Errorf("after add: %+v", rs)
	}

	// Re-add updates label, keeps added_at.
	addedAtBefore := rs[0].AddedAt
	if _, err := store.Add(ctx, id, "renamed", "alex@example.com"); err != nil {
		t.Fatalf("Add (re): %v", err)
	}
	rs, _ = store.List(ctx)
	if rs[0].Label != "renamed" {
		t.Errorf("re-add didn't update label: %q", rs[0].Label)
	}
	if !rs[0].AddedAt.Equal(addedAtBefore) {
		t.Errorf("added_at moved: was %v, now %v", addedAtBefore, rs[0].AddedAt)
	}

	// MarkWalked.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.MarkWalked(ctx, id, now); err != nil {
		t.Fatalf("MarkWalked: %v", err)
	}
	rs, _ = store.List(ctx)
	if rs[0].LastWalkedAt == nil || !rs[0].LastWalkedAt.Equal(now) {
		t.Errorf("last_walked_at=%v, want %v", rs[0].LastWalkedAt, now)
	}

	// Remove.
	removed, err := store.Remove(ctx, id)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Errorf("Remove returned false; want true")
	}
	// Re-remove returns false (no-op).
	removed, err = store.Remove(ctx, id)
	if err != nil {
		t.Fatalf("Remove (no-op): %v", err)
	}
	if removed {
		t.Errorf("Remove on missing returned true; want false")
	}
}

func TestRootsStore_RemoveImmediatelyHidesOrphanedPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_page_roots, notion_page_state, notion_sync_runs, notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store := NewRootsStore(pool)
	hider := &recordingHider{}
	store.SetHider(hider)
	rootID := "abcd1234-ef56-7890-abcd-1234567890ab"
	if _, err := store.Add(ctx, rootID, "docs", "operator@example.com"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_sync_runs (run_id, run_type, started_at, status)
		VALUES ('root-remove-run', 'reconcile', now(), 'complete')
	`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_state (page_id, last_seen_at, last_seen_run_id)
		VALUES ('orphan-page', now(), 'root-remove-run')
	`); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notion_page_roots (page_id, root_object_type, root_object_id, last_seen_run_id)
		VALUES ('orphan-page', 'auto', $1, 'root-remove-run')
	`, rootID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	removed, err := store.Remove(ctx, rootID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed || !hider.hidden("orphan-page") {
		t.Fatalf("removed=%v hidden=%v", removed, hider.hidden("orphan-page"))
	}
}

func TestRootsStore_RemoveAutoDoesNotDeleteTypedRootWithSameID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store := NewRootsStore(pool)
	id := "abcd1234-ef56-7890-abcd-1234567890ab"
	if _, err := store.Add(ctx, id, "auto", "operator@example.com"); err != nil {
		t.Fatalf("add auto: %v", err)
	}
	if _, err := store.Add(ctx, "page:"+id, "page", "operator@example.com"); err != nil {
		t.Fatalf("add page: %v", err)
	}

	removed, err := store.Remove(ctx, id)
	if err != nil {
		t.Fatalf("remove auto: %v", err)
	}
	if !removed {
		t.Fatal("remove auto returned false")
	}
	roots, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roots) != 1 || roots[0].ObjectType != RootTypePage || roots[0].ObjectID != id {
		t.Fatalf("roots=%+v, want only page:%s", roots, id)
	}
}

func TestRootsStore_MarkWalkedMissingRootIsNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_roots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store := NewRootsStore(pool)
	err = store.MarkWalkedRef(ctx, RootRef{Type: RootTypePage, ObjectID: "removed-root"}, time.Now())
	if err != nil {
		t.Fatalf("MarkWalkedRef missing root: %v", err)
	}
}
