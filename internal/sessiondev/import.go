package sessiondev

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/sessionfixture"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ImportDiderot resets and seeds only the session/corpus tables in a guarded
// session-dev database. The caller must run migrations first.
func ImportDiderot(ctx context.Context, pool *pgxpool.Pool, bundle *sessionfixture.Bundle, viewerBaseURL string) error {
	if pool == nil || bundle == nil {
		return fmt.Errorf("session-dev import requires a pool and bundle")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		TRUNCATE session_privacy_audit, session_ingest_chunk_summaries,
		         session_ingest_summaries, session_ingest_published,
		         session_ingest_candidates, event_chunks, events
		RESTART IDENTITY CASCADE;
		UPDATE session_ingest_cursor
		   SET change_seq = 0, policy_hash = '', updated_at = now()
	`); err != nil {
		return fmt.Errorf("reset Diderot session-dev state: %w", err)
	}
	for _, session := range bundle.Manifest.Sessions {
		started, _ := time.Parse(time.RFC3339, session.StartedAt)
		updated, _ := time.Parse(time.RFC3339, session.UpdatedAt)
		scope := "internal"
		ownerOnly := session.Privacy == sessionfixture.PrivacyOwnerOnly
		if ownerOnly {
			scope = "owner:" + session.Owner
		}
		publishedRevision := ""
		var publishedRewriteEpoch int64
		publishedRecordCount := 0
		publishedUserTurnCount := 0
		publishedContentBytes := int64(0)
		if session.Complete {
			publishedRevision = session.Revision
			publishedRewriteEpoch = session.RewriteEpoch
			publishedRecordCount = session.RecordCount
			publishedUserTurnCount = session.UserTurnCount
			publishedContentBytes = session.ContentBytes
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_ingest_candidates
			       (owner, client, session_id, owner_kind, role, origin_app,
			        revision, rewrite_epoch, change_seq, record_count,
			        user_turn_count, content_bytes, updated_at, complete, deleted,
			        published_revision, published_rewrite_epoch,
			        published_record_count, published_user_turn_count,
			        published_content_bytes, published_at, source_started_at,
			        source_updated_at, published_source_started_at,
			        published_source_updated_at, visibility_scope,
			        owner_canonical, privacy_owner_only)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,false,
			        $15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
		`, session.Owner, session.Client, session.SessionID, session.OwnerKind,
			session.SessionRole, session.OriginApp, session.Revision,
			session.RewriteEpoch, session.ChangeSeq, session.RecordCount,
			session.UserTurnCount, session.ContentBytes, updated, session.Complete,
			publishedRevision, publishedRewriteEpoch, publishedRecordCount,
			publishedUserTurnCount, publishedContentBytes, nullableTime(session.Complete, updated),
			started, updated, nullableTime(session.Complete, started),
			nullableTime(session.Complete, updated), scope, session.Owner, ownerOnly); err != nil {
			return fmt.Errorf("insert Diderot fixture candidate %s: %w", session.SessionID, err)
		}
		inputDigest := "sha256:" + session.Document.UncompressedSHA256
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_ingest_summaries
			       (owner, client, session_id, revision, rewrite_epoch, narrative,
			        embedding_text, context_hint, tags, model_id, record_count,
			        input_digest, title, title_model, title_input_digest,
			        title_generated_at, title_attempted_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6,$7,'{session-dev}',
			        'session-dev', $8,$9,$10,'session-dev',$9,now(),now())
		`, session.Owner, session.Client, session.SessionID, session.Revision,
			session.RewriteEpoch, "Synthetic summary: "+session.Title,
			"Local fixture for "+session.Harness, session.RecordCount,
			inputDigest, session.Title); err != nil {
			return fmt.Errorf("insert Diderot fixture summary %s: %w", session.SessionID, err)
		}
		if !session.Complete {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_ingest_published
			       (owner, client, session_id, revision, rewrite_epoch,
			        content_digest, published_at, source_started_at, source_updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$7)
		`, session.Owner, session.Client, session.SessionID, session.Revision,
			session.RewriteEpoch, inputDigest, updated, started); err != nil {
			return fmt.Errorf("insert Diderot fixture publication %s: %w", session.SessionID, err)
		}
		raw, _ := json.Marshal(map[string]any{
			"event_kind": "native_session", "owner": session.Owner,
			"client": session.Client, "revision": session.Revision,
			"rewrite_epoch": session.RewriteEpoch,
		})
		externalID := session.SessionID + ":session"
		viewerURL := strings.TrimRight(viewerBaseURL, "/") + "/sessions/" + session.SessionID
		var eventID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO events
			       (source, external_id, occurred_at, actor, actor_id,
			        actor_canonical, title, body, embedding_text, url, raw,
			        session_id, record_start, record_end, scope)
			VALUES ('agent_session',$1,$2,$3,$3,$3,$4,$5,$5,$6,$7,$8,0,$9,$10)
			RETURNING id
		`, externalID, updated, session.Owner, session.Title,
			"Synthetic summary: "+session.Title, viewerURL, raw,
			session.SessionID, session.RecordCount, scope).Scan(&eventID); err != nil {
			return fmt.Errorf("insert Diderot fixture event %s: %w", session.SessionID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_chunks
			       (event_id, chunk_idx, kind, content, context_prefix,
			        occurred_at, embedded_at, embedder_id)
			VALUES ($1,0,'text',$2,$3,$4,now(),'session-dev')
		`, eventID, "Synthetic summary: "+session.Title,
			"Local fixture for "+session.Harness, updated); err != nil {
			return fmt.Errorf("insert Diderot fixture chunk %s: %w", session.SessionID, err)
		}
	}
	return tx.Commit(ctx)
}

func nullableTime(valid bool, value time.Time) any {
	if !valid {
		return nil
	}
	return value
}
