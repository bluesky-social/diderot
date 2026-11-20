package slack

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type docProgress struct {
	filename string
	settled  bool
}

// fetchDocProgress returns all documents in the barrier window, oldest first.
func fetchDocProgress(
	ctx context.Context,
	pool *pgxpool.Pool,
	channelID, threadTS string,
	since, before time.Time,
) ([]docProgress, error) {
	const q = `
		SELECT ed.filename, ed.extracted_at IS NOT NULL
		  FROM event_documents ed
		  JOIN events e ON e.id = ed.event_id
		  JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.source = 'slack'
		   AND d.channel_id = $1
		   AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		   AND e.deleted = false
		   AND e.occurred_at > $3
		   AND e.occurred_at <= $4
		 ORDER BY ed.occurred_at, ed.id
	`
	rows, err := pool.Query(ctx, q, channelID, threadTS, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []docProgress
	for rows.Next() {
		var d docProgress
		if err := rows.Scan(&d.filename, &d.settled); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// renderDocWaitNotice returns "" when every document is settled.
func renderDocWaitNotice(docs []docProgress) string {
	var unsettled int
	for _, d := range docs {
		if !d.settled {
			unsettled++
		}
	}
	if unsettled == 0 {
		return ""
	}
	return "hang on — extracting text from the attached documents before I answer."
}

// mentionDocument is one document attached to the mention message.
type mentionDocument struct {
	filename     string
	text         string
	settled      bool
	completeness string
}

// fetchMentionDocuments includes unsettled rows so they can be marked pending.
func fetchMentionDocuments(
	ctx context.Context,
	pool *pgxpool.Pool,
	externalID string,
) ([]mentionDocument, error) {
	const q = `
		SELECT ed.filename,
		       COALESCE(string_agg(c.content, E'\n\n' ORDER BY c.chunk_idx), ''),
		       ed.extracted_at IS NOT NULL,
		       CASE WHEN ed.incomplete_reason IS NOT NULL THEN 'partial'
		            WHEN ed.rejected_reason IS NOT NULL THEN 'rejected'
		            ELSE 'complete' END
		  FROM event_documents ed
		  JOIN events e ON e.id = ed.event_id
		  LEFT JOIN event_chunks c ON c.event_document_id = ed.id
		 WHERE e.source = 'slack'
		   AND e.external_id = $1
		   AND e.deleted = false
		 GROUP BY ed.id, ed.filename, ed.extracted_at, ed.incomplete_reason,
		          ed.rejected_reason, ed.occurred_at
		 ORDER BY ed.occurred_at, ed.id
	`
	rows, err := pool.Query(ctx, q, externalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mentionDocument
	for rows.Next() {
		var d mentionDocument
		if err := rows.Scan(&d.filename, &d.text, &d.settled, &d.completeness); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
