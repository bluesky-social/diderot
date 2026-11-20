package slack

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// captionBarrierDeadline bounds waits for image captions.
const captionBarrierDeadline = 20 * time.Second

const docBarrierDeadline = 45 * time.Second

const barrierCeiling = docBarrierDeadline

const barrierPoll = 500 * time.Millisecond

type pendingAttachments struct {
	images             int
	documents          int
	imageEmbeddings    int
	documentEmbeddings int
}

func (p pendingAttachments) total() int {
	return p.images + p.documents + p.imageEmbeddings + p.documentEmbeddings
}

func (p pendingAttachments) hasDocuments() bool {
	return p.documents > 0 || p.documentEmbeddings > 0
}

func (p pendingAttachments) embeddings() int {
	return p.imageEmbeddings + p.documentEmbeddings
}

func (p pendingAttachments) deadline() time.Duration {
	if p.hasDocuments() {
		return docBarrierDeadline
	}
	return captionBarrierDeadline
}

type barrierResult struct {
	initialPending pendingAttachments
	remaining      pendingAttachments
	timedOut       bool
}

// waitForAttachments blocks until every in-scope attachment has settled and
// every derived attachment chunk has been embedded, or its deadline fires.
// Poisoned rows count as settled. Keeping the embedding leg inside this
// pre-run barrier is load-bearing: the Ask/model timeout is created only after
// this function returns, so caption, extraction, and embedding latency cannot
// consume the model's answer budget.
func waitForAttachments(
	ctx context.Context,
	pool *pgxpool.Pool,
	channelID, threadTS string,
	since, before time.Time,
) barrierResult {
	start := time.Now()

	initial, err := countUnsettledInScope(ctx, pool, channelID, threadTS, since, before)
	if err != nil {
		slog.Warn("ask: attachment barrier first query failed; requiring final-state verification",
			"channel", channelID, "thread_ts", threadTS, "err", err)
		return barrierResult{timedOut: true}
	}
	res := barrierResult{initialPending: initial, remaining: initial}
	if initial.total() == 0 {
		return res
	}

	deadline := start.Add(initial.deadline())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	deadlineCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for {
		if deadlineCtx.Err() != nil {
			res.timedOut = true
			return res
		}
		sleep := barrierPoll
		if until := time.Until(deadline); until < sleep {
			sleep = until
		}
		if sleep > 0 {
			t := time.NewTimer(sleep)
			select {
			case <-deadlineCtx.Done():
				t.Stop()
				res.timedOut = true
				return res
			case <-t.C:
			}
		}

		pending, err := countUnsettledInScope(deadlineCtx, pool, channelID, threadTS, since, before)
		if err != nil {
			slog.Warn("ask: attachment barrier poll failed; ending wait",
				"channel", channelID, "thread_ts", threadTS, "err", err)
			// The caller must perform its final query with the parent
			// context. An unverifiable poll is never evidence of settlement.
			res.timedOut = true
			return res
		}
		res.remaining = pending
		if pending.total() == 0 {
			return res
		}
	}
}

// countUnsettledInScope mirrors the image, document, and embedder worker
// predicates. The embedding count is deliberately limited to derived
// attachment chunks. The mention body is passed directly to Ask and ordinary
// thread text comes from Postgres, so waiting for kind='text' would add latency
// without making this turn's context more complete.
func countUnsettledInScope(
	ctx context.Context,
	pool *pgxpool.Pool,
	channelID, threadTS string,
	since, before time.Time,
) (pendingAttachments, error) {
	const q = `
		SELECT
		  (SELECT count(*)
		     FROM event_images ei
		     JOIN events e ON e.id = ei.event_id
		     JOIN slack_message_details d ON d.event_id = e.id
		    WHERE e.source = 'slack'
		      AND d.channel_id = $1
		      AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		      AND e.deleted = false
		      AND e.occurred_at > $3
		      AND e.occurred_at <= $4
		      AND ei.captioned_at IS NULL
		      AND ei.failure_count < $5
		      AND ei.media_type LIKE 'image/%'),
		  (SELECT count(*)
		     FROM event_documents ed
		     JOIN events e ON e.id = ed.event_id
		     JOIN slack_message_details d ON d.event_id = e.id
		    WHERE e.source = 'slack'
		      AND d.channel_id = $1
		      AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		      AND e.deleted = false
		      AND e.occurred_at > $3
		      AND e.occurred_at <= $4
		      AND ed.extracted_at IS NULL
		      AND ed.failure_count < $5),
		  (SELECT count(*)
		     FROM event_chunks ec
		     JOIN events e ON e.id = ec.event_id
		     JOIN slack_message_details d ON d.event_id = e.id
		    WHERE e.source = 'slack'
		      AND d.channel_id = $1
		      AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		      AND e.deleted = false
		      AND e.occurred_at > $3
		      AND e.occurred_at <= $4
		      AND ec.kind = 'image_caption'
		      AND ec.embedded_at IS NULL
		      AND ec.failure_count < $5),
		  (SELECT count(*)
		     FROM event_chunks ec
		     JOIN events e ON e.id = ec.event_id
		     JOIN slack_message_details d ON d.event_id = e.id
		    WHERE e.source = 'slack'
		      AND d.channel_id = $1
		      AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		      AND e.deleted = false
		      AND e.occurred_at > $3
		      AND e.occurred_at <= $4
		      AND ec.kind = 'document_text'
		      AND ec.embedded_at IS NULL
		      AND ec.failure_count < $5)
	`
	var p pendingAttachments
	err := pool.QueryRow(ctx, q,
		channelID, threadTS, since, before, events.MaxFailures,
	).Scan(&p.images, &p.documents, &p.imageEmbeddings, &p.documentEmbeddings)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pendingAttachments{}, nil
		}
		return pendingAttachments{}, err
	}
	return p, nil
}

func barrierWaitNotice(p pendingAttachments, docs []docProgress) string {
	// Once source preprocessing has settled, describe the remaining stage
	// accurately. In mixed states, keep reporting the still-pending source
	// work; its completion is a prerequisite for any derived indexing work.
	if p.images == 0 && p.documents == 0 && p.embeddings() > 0 {
		if p.hasDocuments() {
			return documentEmbeddingWaitNotice
		}
		return imageEmbeddingWaitNotice
	}
	if !p.hasDocuments() {
		return captionWaitNotice
	}
	if text := renderDocWaitNotice(docs); text != "" {
		return text
	}
	return docWaitNotice
}

func barrierTimeoutNotice(p pendingAttachments) string {
	if p.images == 0 && p.documents == 0 && p.embeddings() > 0 {
		if p.hasDocuments() {
			return documentEmbeddingTimeoutNotice
		}
		return imageEmbeddingTimeoutNotice
	}
	if p.hasDocuments() {
		return docTimeoutNotice
	}
	return captionTimeoutNotice
}

// mentionImage is the direct-image manifest for one routed Slack Ask. Caption
// is the worker output available at routing time; the delivery fields are set
// by appendMentionImages to record exactly what reached the model.
type mentionImage struct {
	EventID         int64
	SourceImageID   string
	Filename        string
	MediaType       string
	Permalink       string
	Caption         string
	CaptionSnapshot string
	DeliveryStatus  string
	FailureReason   string
}

// fetchMentionImages returns every image attached directly to the mention in
// Slack source order. Starting from events.raw (rather than event_images)
// keeps an untracked or unsupported image visible as unavailable instead of
// silently omitting it from the Ask.
func fetchMentionImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	externalID string,
) ([]mentionImage, error) {
	const q = `
		WITH target AS (
			SELECT id, raw
			  FROM events
			 WHERE source = 'slack' AND external_id = $1
		), files AS (
			SELECT e.id AS event_id, f.file_obj, f.attachment_idx
			  FROM target e
			 CROSS JOIN LATERAL jsonb_array_elements(
				CASE
				  WHEN jsonb_typeof(e.raw->'files') = 'array'
				   AND jsonb_array_length(e.raw->'files') > 0
				    THEN e.raw->'files'
				  WHEN jsonb_typeof(e.raw->'message'->'files') = 'array'
				   AND jsonb_array_length(e.raw->'message'->'files') > 0
				    THEN e.raw->'message'->'files'
				  WHEN jsonb_typeof(e.raw->'previous_message'->'files') = 'array'
				    THEN e.raw->'previous_message'->'files'
				  ELSE '[]'::jsonb
				END
			  ) WITH ORDINALITY AS f(file_obj, attachment_idx)
		)
		SELECT f.event_id,
		       f.file_obj->>'id',
		       COALESCE(NULLIF(f.file_obj->>'title', ''), NULLIF(f.file_obj->>'name', ''), f.file_obj->>'id'),
		       COALESCE(NULLIF(f.file_obj->>'mimetype', ''), ei.media_type, ''),
		       COALESCE(f.file_obj->>'permalink', ''),
		       COALESCE(ec.content, ''),
		       CASE
		         WHEN ei.id IS NULL THEN 'image_not_indexed'
		         WHEN ec.content IS NOT NULL AND ec.content <> '' THEN ''
		         WHEN ei.caption_model IS NOT NULL THEN ei.caption_model
		         WHEN ei.failure_count >= $2 THEN COALESCE(NULLIF(ei.last_failure, ''), 'caption_failed')
		         ELSE 'caption_pending'
		       END
		  FROM files f
		  LEFT JOIN event_images ei
		    ON ei.event_id = f.event_id
		   AND ei.source_image_id = f.file_obj->>'id'
		  LEFT JOIN event_chunks ec
		    ON ec.id = ei.chunk_id AND ec.kind = 'image_caption'
		 WHERE f.file_obj->>'id' <> ''
		   AND (COALESCE(f.file_obj->>'mimetype', '') LIKE 'image/%' OR ei.id IS NOT NULL)
		 ORDER BY f.attachment_idx
	`
	rows, err := pool.Query(ctx, q, externalID, events.MaxFailures)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []mentionImage
	for rows.Next() {
		var image mentionImage
		if err := rows.Scan(
			&image.EventID,
			&image.SourceImageID,
			&image.Filename,
			&image.MediaType,
			&image.Permalink,
			&image.Caption,
			&image.FailureReason,
		); err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return images, nil
}

func fetchMentionEventID(ctx context.Context, pool *pgxpool.Pool, externalID string) (int64, error) {
	var eventID int64
	err := pool.QueryRow(ctx, `
		SELECT id FROM events WHERE source = 'slack' AND external_id = $1
	`, externalID).Scan(&eventID)
	return eventID, err
}

func unavailableMentionImages(eventID int64, attachments []FileAttachment, reason string) []mentionImage {
	var images []mentionImage
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment.Mimetype, "image/") {
			continue
		}
		filename := attachment.Title
		if filename == "" {
			filename = attachment.Name
		}
		images = append(images, mentionImage{
			EventID:       eventID,
			SourceImageID: attachment.FileID,
			Filename:      filename,
			MediaType:     attachment.Mimetype,
			Permalink:     attachment.Permalink,
			FailureReason: reason,
		})
	}
	return images
}

func fallbackMentionImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	externalID string,
	attachments []FileAttachment,
	reason string,
) ([]mentionImage, error) {
	images := unavailableMentionImages(0, attachments, reason)
	if len(images) == 0 {
		return nil, nil
	}
	eventID, err := fetchMentionEventID(ctx, pool, externalID)
	if err != nil {
		return images, err
	}
	for i := range images {
		images[i].EventID = eventID
	}
	return images, nil
}

// persistAskInputImages writes the exact direct-image delivery manifest for a
// routed turn. It is intentionally best-effort at the caller: audit metadata
// must not prevent an otherwise valid Ask from running.
func persistAskInputImages(
	ctx context.Context,
	pool *pgxpool.Pool,
	sessionID, sourceMessageID, sourceTurnID string,
	images []mentionImage,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const q = `
		INSERT INTO ask_input_images
		       (session_id, source, source_event_id, source_message_id,
		        source_turn_id, source_image_id, filename, media_type,
		        permalink, caption_snapshot, delivery_status, failure_reason)
		VALUES ($1, 'slack', $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''),
		        NULLIF($8, ''), NULLIF($9, ''), $10, NULLIF($11, ''))
		ON CONFLICT (session_id, source_turn_id, source_image_id) DO UPDATE SET
		       filename = EXCLUDED.filename,
		       media_type = EXCLUDED.media_type,
		       permalink = EXCLUDED.permalink,
		       caption_snapshot = EXCLUDED.caption_snapshot,
		       delivery_status = EXCLUDED.delivery_status,
		       failure_reason = EXCLUDED.failure_reason
	`
	for _, image := range images {
		if _, err := tx.Exec(ctx, q,
			sessionID,
			image.EventID,
			sourceMessageID,
			sourceTurnID,
			image.SourceImageID,
			image.Filename,
			image.MediaType,
			image.Permalink,
			image.CaptionSnapshot,
			image.DeliveryStatus,
			image.FailureReason,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
