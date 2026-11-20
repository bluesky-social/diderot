package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

const documentBackfillBatchSize = 500

type DocumentBackfillOptions struct {
	ChannelID string
	SinceID   int64
	MaxEvents int
	Reset     bool
}

type DocumentBackfillResult struct {
	Scanned     int
	Enqueued    int
	Documents   int
	NoDocuments int
	NextCursor  int64
	Done        bool
	Errors      []DocumentBackfillError
}

type DocumentBackfillError struct {
	EventID int64
	Err     string
}

func BackfillDocuments(
	ctx context.Context, pool *pgxpool.Pool, opts DocumentBackfillOptions,
) (DocumentBackfillResult, error) {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = documentBackfillBatchSize
	}
	result := DocumentBackfillResult{NextCursor: opts.SinceID}

	if opts.Reset && opts.SinceID == 0 {
		if _, err := pool.Exec(ctx, `
			DELETE FROM event_documents ed
			 USING slack_message_details d, slack_channel_config c
			 WHERE d.event_id = ed.event_id
			   AND c.channel_id = d.channel_id
			   AND c.doc_ingest_enabled
			   AND ($1 = '' OR d.channel_id = $1)
		`, opts.ChannelID); err != nil {
			return result, fmt.Errorf("reset document extraction: %w", err)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT e.id, e.raw
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		  JOIN slack_channel_config c ON c.channel_id = d.channel_id
		 WHERE e.source = 'slack'
		   AND NOT e.deleted
		   AND d.has_files
		   AND c.doc_ingest_enabled
		   AND e.id > $1
		   AND ($2 = '' OR d.channel_id = $2)
		 ORDER BY e.id
		 LIMIT $3
	`, opts.SinceID, opts.ChannelID, opts.MaxEvents)
	if err != nil {
		return result, fmt.Errorf("query document backfill: %w", err)
	}
	type rawEvent struct {
		id  int64
		raw []byte
	}
	var batch []rawEvent
	for rows.Next() {
		var event rawEvent
		if err := rows.Scan(&event.id, &event.raw); err != nil {
			rows.Close()
			return result, err
		}
		batch = append(batch, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	for _, event := range batch {
		result.Scanned++
		result.NextCursor = event.id
		details, err := DetailsFrom(event.raw)
		if err != nil {
			result.Errors = append(result.Errors, DocumentBackfillError{event.id, "parse: " + err.Error()})
			continue
		}
		refs := DocumentRefsFromAttachments(details.Attachments)
		if len(refs) == 0 {
			result.NoDocuments++
			continue
		}
		if err := events.ReconcileDocuments(ctx, pool, event.id, refs); err != nil {
			result.Errors = append(result.Errors, DocumentBackfillError{event.id, "reconcile: " + err.Error()})
			continue
		}
		result.Enqueued++
		result.Documents += len(refs)
	}
	result.Done = len(batch) < opts.MaxEvents
	return result, nil
}

type DocumentBackfillWorker struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func NewDocumentBackfillWorker(pool *pgxpool.Pool, logger *slog.Logger) *DocumentBackfillWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DocumentBackfillWorker{pool: pool, logger: logger}
}

func (w *DocumentBackfillWorker) Sweep(ctx context.Context) error {
	var channelID string
	var cursor int64
	err := w.pool.QueryRow(ctx, `
		SELECT channel_id, doc_backfill_cursor
		  FROM slack_channel_config
		 WHERE doc_ingest_enabled AND doc_backfill_cursor IS NOT NULL
		 ORDER BY updated_at, channel_id
		 LIMIT 1
	`).Scan(&channelID, &cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	result, err := BackfillDocuments(ctx, w.pool, DocumentBackfillOptions{
		ChannelID: channelID,
		SinceID:   cursor,
		MaxEvents: documentBackfillBatchSize,
	})
	if err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("document backfill event %d: %s", result.Errors[0].EventID, result.Errors[0].Err)
	}

	next := result.NextCursor
	_, err = w.pool.Exec(ctx, `
		UPDATE slack_channel_config
		   SET doc_backfill_cursor = CASE WHEN $3 THEN NULL ELSE $2::bigint END
		 WHERE channel_id = $1 AND doc_ingest_enabled AND doc_backfill_cursor = $4
	`, channelID, next, result.Done, cursor)
	if err == nil {
		w.logger.Info("docs: history enqueue batch",
			"channel", channelID, "events", result.Scanned, "cursor", next, "done", result.Done)
	}
	return err
}
