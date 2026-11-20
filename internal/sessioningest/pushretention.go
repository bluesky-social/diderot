package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PruneSuperseded applies the operator retention window: for up to limit
// revisions superseded longer than retention ago, it deletes their received
// bytes and every materialization, keeps the revision row (so a citation
// reports "superseded" with the current revision), garbage-collects chunks
// no other blob of the session uses, and credits the owner's usage. A
// revision that became the head again (A -> B -> A) is never pruned.
func (p *PushStore) PruneSuperseded(ctx context.Context, retention time.Duration, limit int) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT session_id, revision FROM session_push_revisions
		 WHERE superseded_at IS NOT NULL AND pruned_at IS NULL AND superseded_at < now() - $1::interval
		 ORDER BY superseded_at LIMIT $2`, retention.String(), limit)
	if err != nil {
		return 0, fmt.Errorf("sessioningest: select prunable revisions: %w", err)
	}
	type target struct{ session, revision string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.session, &t.revision); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sessioningest: scan prunable revision: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sessioningest: select prunable revisions: %w", err)
	}
	pruned := 0
	for _, t := range targets {
		ok, err := p.pruneRevision(ctx, t.session, t.revision, retention)
		if err != nil {
			return pruned, err
		}
		if ok {
			pruned++
		}
	}
	return pruned, nil
}

func (p *PushStore) pruneRevision(ctx context.Context, sessionID, revision string, retention time.Duration) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("sessioningest: begin prune: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner string
	var head *string
	if err := tx.QueryRow(ctx, `SELECT owner_canonical, head_revision FROM session_ingest_bindings
		WHERE session_id=$1 FOR UPDATE`, sessionID).Scan(&owner, &head); err != nil {
		return false, fmt.Errorf("sessioningest: lock session for prune: %w", err)
	}
	var bodyBlob int64
	err = tx.QueryRow(ctx, `SELECT body_blob_id FROM session_push_revisions
		WHERE session_id=$1 AND revision=$2 AND pruned_at IS NULL
		  AND superseded_at IS NOT NULL AND superseded_at < now() - $3::interval
		FOR UPDATE`, sessionID, revision, retention.String()).Scan(&bodyBlob)
	if errors.Is(err, pgx.ErrNoRows) || (head != nil && *head == revision) {
		return false, nil // reinstated or pruned concurrently
	}
	if err != nil {
		return false, fmt.Errorf("sessioningest: lock revision for prune: %w", err)
	}
	blobs := []int64{bodyBlob}
	rows, err := tx.Query(ctx, `DELETE FROM session_push_materializations
		WHERE session_id=$1 AND revision=$2 RETURNING records_blob_id`, sessionID, revision)
	if err != nil {
		return false, fmt.Errorf("sessioningest: prune materializations: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, fmt.Errorf("sessioningest: scan pruned materialization: %w", err)
		}
		blobs = append(blobs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sessioningest: prune materializations: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE session_push_revisions SET body_blob_id = NULL, pruned_at = now()
		WHERE session_id=$1 AND revision=$2`, sessionID, revision); err != nil {
		return false, fmt.Errorf("sessioningest: mark revision pruned: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_push_blobs WHERE id = ANY($1)`, blobs); err != nil {
		return false, fmt.Errorf("sessioningest: prune blobs: %w", err)
	}
	var freed int64
	if err := tx.QueryRow(ctx, `
		WITH gone AS (
		    DELETE FROM session_push_chunks c
		     WHERE c.session_id = $1
		       AND NOT EXISTS (SELECT 1 FROM session_push_blob_chunks b
		                        WHERE b.session_id = c.session_id AND b.digest = c.digest)
		    RETURNING octet_length(c.data) AS n)
		SELECT COALESCE(sum(n), 0) FROM gone`, sessionID).Scan(&freed); err != nil {
		return false, fmt.Errorf("sessioningest: collect unreferenced chunks: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE session_push_owner_usage
		SET stored_bytes = stored_bytes - $2, revisions = revisions - 1, updated_at = now()
		WHERE owner_canonical = $1`, owner, freed); err != nil {
		return false, fmt.Errorf("sessioningest: credit pruned usage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("sessioningest: commit prune: %w", err)
	}
	return true, nil
}
