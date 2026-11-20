package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type NotionHider interface {
	HideNotionTx(ctx context.Context, tx pgx.Tx, externalID string) (func(context.Context), error)
}

// PageStateStore makes managed-root membership authoritative. It only sweeps
// memberships for roots whose complete walk is present in the current run;
// a failed root therefore cannot mass-hide its previously indexed subtree.
type PageStateStore struct {
	pool  *pgxpool.Pool
	hider NotionHider
}

func NewPageStateStore(pool *pgxpool.Pool, hider NotionHider) *PageStateStore {
	return &PageStateStore{pool: pool, hider: hider}
}

func (s *PageStateStore) ApplySuccessfulRoots(ctx context.Context, runID string, walked map[RootRef][]PageSnapshot) error {
	if len(walked) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("notion state: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var postCommit []func(context.Context)

	validWalked := make(map[RootRef][]PageSnapshot, len(walked))
	// removedRootCandidates holds pages that were walked under a root that
	// turned out to be concurrently removed (its notion_roots row, and so
	// its notion_page_roots links, are already gone). Whether any of these
	// pages are genuinely orphaned can't be decided here: a surviving root
	// also present in this run (validWalked) may be about to write its own
	// fresh link to the same page in the loop below, and that write hasn't
	// happened yet. Deferring the pages into removedCandidates, decided by
	// the stillRooted check below only after every validWalked root's links
	// for this run are written, gives that surviving link a chance to count
	// before a page is judged orphaned.
	removedRootCandidates := make(map[string]struct{})
	for root, pages := range walked {
		var locked int
		err := tx.QueryRow(ctx, `
			SELECT 1 FROM notion_roots
			 WHERE object_type = $1 AND object_id = $2
			 FOR KEY SHARE
		`, root.Type, root.ObjectID).Scan(&locked)
		if err == nil {
			validWalked[root] = pages
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("notion state: lock root %s:%s: %w", root.Type, root.ObjectID, err)
		}
		if s.hider == nil && len(pages) > 0 {
			return fmt.Errorf("notion state: removed root %s:%s has pages but no hider", root.Type, root.ObjectID)
		}
		for _, page := range pages {
			removedRootCandidates[page.PageID] = struct{}{}
		}
	}

	emptyPages := make(map[string]struct{})
	inaccessiblePages := make(map[string]struct{})
	readablePages := make(map[string]struct{})
	for _, pages := range validWalked {
		for _, page := range pages {
			if page.HasContent && !page.Inaccessible && !page.TransientlyInaccessible {
				readablePages[page.PageID] = struct{}{}
			}
		}
	}

	// A page this run proves readable may have had its event tombstoned a
	// moment ago by a concurrent RootsStore.Remove on a *different* root,
	// racing independently of this transaction: Remove's own orphan check
	// (roots.go) only sees persisted links, so it can tombstone a page
	// whose reclaiming walk (the one that produced readablePages above) is
	// already in flight but hasn't reached this Apply call yet. This
	// transaction has no way to recreate the tombstoned event's content,
	// so detect the mismatch directly against the event's own tombstone
	// flag, not the page's prior access_state: access_state is also
	// 'removed' in the ordinary, non-race flow of a root being removed and
	// later re-covered by another root, but in that flow the walk that
	// reported the page readable has already re-ingested it (ingestOne
	// upserts unconditionally), so the event is live again by the time
	// this runs. Checking access_state there would request a needless
	// full re-walk of every root on every such transition. PageSnapshot's
	// PageID is the same string used as the event's external_id (see
	// EventSinkAdapter), so one batched query over every readable page
	// answers this for the whole run instead of one round trip per page.
	needsReplay := false
	if len(readablePages) > 0 {
		pageIDs := make([]string, 0, len(readablePages))
		for pageID := range readablePages {
			pageIDs = append(pageIDs, pageID)
		}
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM events
				 WHERE source = 'notion' AND external_id = ANY($1) AND deleted
			)
		`, pageIDs).Scan(&needsReplay); err != nil {
			return fmt.Errorf("notion state: check tombstoned readable pages: %w", err)
		}
	}

	for root, pages := range validWalked {
		for _, page := range pages {
			metadata, err := json.Marshal(struct {
				Parent Parent `json:"parent"`
				URL    string `json:"url"`
			}{Parent: page.Parent, URL: page.URL})
			if err != nil {
				return fmt.Errorf("notion state: marshal page %s: %w", page.PageID, err)
			}
			_, readableElsewhere := readablePages[page.PageID]
			if readableElsewhere && (page.Inaccessible || page.TransientlyInaccessible) {
				// Another successful root rendered this same page during this
				// run. That stronger observation owns page state; this root still
				// refreshes its membership link below. Ensure the parent state row
				// exists even when map iteration visits this weaker observation
				// before the readable one; the readable pass will populate the
				// authoritative metadata later in the same transaction.
				_, err = tx.Exec(ctx, `
					INSERT INTO notion_page_state
					       (page_id, remote_last_edited_at, last_seen_at, last_seen_run_id,
					        access_state, last_error, source_metadata)
					VALUES ($1, $2, now(), $3, 'active', '', $4)
					ON CONFLICT (page_id) DO NOTHING
				`, page.PageID, nullableTime(page.LastEditedAt), runID, metadata)
			} else if page.Inaccessible {
				// Structural discovery proved the page's identity, but the
				// integration can no longer read it. Preserve prior metadata for
				// audit, explicitly mark access loss, and tombstone below.
				_, err = tx.Exec(ctx, `
					INSERT INTO notion_page_state
					       (page_id, remote_last_edited_at, last_seen_at, last_seen_run_id,
					        access_state, last_error, source_metadata)
					VALUES ($1, $2, now(), $3, 'inaccessible', $4, $5)
					ON CONFLICT (page_id) DO UPDATE
					   SET last_seen_at = EXCLUDED.last_seen_at,
					       last_seen_run_id = EXCLUDED.last_seen_run_id,
					       access_state = 'inaccessible',
					       last_error = EXCLUDED.last_error
				`, page.PageID, nullableTime(page.LastEditedAt), runID, page.LastError, metadata)
			} else if page.TransientlyInaccessible {
				// The structural walk proved membership, but rendering failed
				// with a transient access response. Refresh reachability while
				// preserving any prior access state and last-known-good event.
				_, err = tx.Exec(ctx, `
					INSERT INTO notion_page_state
					       (page_id, remote_last_edited_at, last_seen_at, last_seen_run_id,
					        access_state, last_error, source_metadata)
					VALUES ($1, $2, now(), $3, 'inaccessible', $4, $5)
					ON CONFLICT (page_id) DO UPDATE
					   SET remote_last_edited_at = EXCLUDED.remote_last_edited_at,
					       last_seen_at = EXCLUDED.last_seen_at,
					       last_seen_run_id = EXCLUDED.last_seen_run_id,
					       last_error = EXCLUDED.last_error,
					       source_metadata = EXCLUDED.source_metadata
				`, page.PageID, nullableTime(page.LastEditedAt), runID, page.LastError, metadata)
			} else {
				_, err = tx.Exec(ctx, `
					INSERT INTO notion_page_state
					       (page_id, remote_last_edited_at, last_seen_at, last_seen_run_id,
					        access_state, last_error, source_metadata)
					VALUES ($1, $2, now(), $3, 'active', '', $4)
					ON CONFLICT (page_id) DO UPDATE
					   SET remote_last_edited_at = EXCLUDED.remote_last_edited_at,
					       last_seen_at = EXCLUDED.last_seen_at,
					       last_seen_run_id = EXCLUDED.last_seen_run_id,
					       access_state = 'active', last_error = '',
					       source_metadata = EXCLUDED.source_metadata
				`, page.PageID, nullableTime(page.LastEditedAt), runID, metadata)
			}
			if err != nil {
				return fmt.Errorf("notion state: upsert page %s: %w", page.PageID, err)
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO notion_page_roots
				       (page_id, root_object_type, root_object_id, last_seen_run_id)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (page_id, root_object_type, root_object_id) DO UPDATE
				   SET last_seen_run_id = EXCLUDED.last_seen_run_id
			`, page.PageID, root.Type, root.ObjectID, runID)
			if err != nil {
				return fmt.Errorf("notion state: link page %s to root %s:%s: %w", page.PageID, root.Type, root.ObjectID, err)
			}
			if page.Inaccessible && !readableElsewhere {
				inaccessiblePages[page.PageID] = struct{}{}
			} else if !page.HasContent && !page.TransientlyInaccessible && !readableElsewhere {
				emptyPages[page.PageID] = struct{}{}
			}
		}
	}

	removedCandidates := make(map[string]struct{}, len(removedRootCandidates))
	for pageID := range removedRootCandidates {
		removedCandidates[pageID] = struct{}{}
	}
	for root := range validWalked {
		rows, err := tx.Query(ctx, `
			DELETE FROM notion_page_roots
			 WHERE root_object_type = $1 AND root_object_id = $2
			   AND last_seen_run_id <> $3
			 RETURNING page_id
		`, root.Type, root.ObjectID, runID)
		if err != nil {
			return fmt.Errorf("notion state: sweep root %s:%s: %w", root.Type, root.ObjectID, err)
		}
		for rows.Next() {
			var pageID string
			if err := rows.Scan(&pageID); err != nil {
				rows.Close()
				return fmt.Errorf("notion state: scan swept page: %w", err)
			}
			removedCandidates[pageID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("notion state: sweep rows: %w", err)
		}
		rows.Close()
	}

	toHide := make(map[string]string)
	for pageID := range removedCandidates {
		var stillRooted bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM notion_page_roots WHERE page_id = $1)
		`, pageID).Scan(&stillRooted); err != nil {
			return fmt.Errorf("notion state: check page roots %s: %w", pageID, err)
		}
		if !stillRooted {
			toHide[pageID] = "removed"
		}
	}
	for pageID := range emptyPages {
		toHide[pageID] = "empty"
	}
	for pageID := range inaccessiblePages {
		toHide[pageID] = "inaccessible"
	}
	if len(toHide) > 0 && s.hider == nil {
		return fmt.Errorf("notion state: %d pages require hiding but no hider is configured", len(toHide))
	}
	for pageID, state := range toHide {
		post, err := s.hider.HideNotionTx(ctx, tx, pageID)
		if err != nil {
			return fmt.Errorf("notion state: hide page %s: %w", pageID, err)
		}
		postCommit = append(postCommit, post)
		if _, err := tx.Exec(ctx, `
			UPDATE notion_page_state
			   SET access_state = $2,
			       last_error = CASE WHEN $2 = 'inaccessible' THEN last_error ELSE '' END
			 WHERE page_id = $1
		`, pageID, state); err != nil {
			return fmt.Errorf("notion state: mark page %s hidden: %w", pageID, err)
		}
	}
	if needsReplay {
		if _, err := tx.Exec(ctx, `
			UPDATE notion_sync
			   SET backfill_requested_at = clock_timestamp(),
			       backfill_cursor = NULL
			 WHERE id = 1
		`); err != nil {
			return fmt.Errorf("notion state: request replay for revived page: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("notion state: commit: %w", err)
	}
	for _, post := range postCommit {
		post(ctx)
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
