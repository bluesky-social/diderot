package zendesk

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// conversation is what an event expands to: the indexed ticket conversation
// or article it belongs to, the event first, with the image captions and
// extracted document text of its attachments, as far as allowedScopes may
// read them. It returns events.ErrEventNotFound when the event itself is
// out of reach.
func conversation(ctx context.Context, pool *pgxpool.Pool, eventID int64, allowedScopes []string) (string, bool, error) {
	scopes := events.ReadableScopes(allowedScopes)
	rows, err := pool.Query(ctx, `WITH anchor AS (
		SELECT id, thread_id FROM events WHERE id = $1 AND source = 'zendesk'
		AND NOT deleted AND scope = ANY($2::text[])
	)
	SELECT related.id, related.occurred_at, COALESCE(related.actor, ''),
		COALESCE(related.url, ''), left(related.body, 65536), length(related.body) > 65536
	FROM events related JOIN anchor ON related.id = anchor.id
		OR (anchor.thread_id <> '' AND related.thread_id = anchor.thread_id)
	WHERE related.source = 'zendesk' AND NOT related.deleted AND related.scope = ANY($2::text[])
	ORDER BY (related.id = $1) DESC, related.occurred_at, related.id LIMIT 201`, eventID, scopes)
	if err != nil {
		return "", false, err
	}
	type entry struct {
		ID        int64
		At        time.Time
		Actor     string
		URL       string
		Body      string
		Truncated bool
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil {
		return "", false, err
	}
	if len(entries) == 0 {
		return "", false, events.ErrEventNotFound
	}
	var content strings.Builder
	truncated := len(entries) > 200
	appendBounded := func(text string, limit int) {
		remaining := max(0, limit-content.Len())
		if len(text) > remaining {
			truncated = true
			for remaining > 0 && !utf8.ValidString(text[:remaining]) {
				remaining--
			}
			text = text[:remaining]
		}
		content.WriteString(text)
	}
	var eventIDs []int64
	for _, entry := range entries[:min(200, len(entries))] {
		eventIDs = append(eventIDs, entry.ID)
		truncated = truncated || entry.Truncated
		appendBounded(fmt.Sprintf("\n[event_id=%d; %s; actor=%s]\n%s\n%s\n", entry.ID, entry.At.Format(time.RFC3339), entry.Actor, entry.URL, entry.Body), 40<<10)
	}
	rows, err = pool.Query(ctx, `SELECT chunk.event_id, chunk.kind, left(chunk.content, 65536),
		length(chunk.content) > 65536, COALESCE(doc.filename, ''),
		COALESCE(doc.page_count, 0), COALESCE(doc.pages_extracted, 0),
		CASE WHEN doc.incomplete_reason IS NOT NULL THEN 'partial' ELSE 'complete' END
		FROM event_chunks chunk JOIN events parent ON parent.id = chunk.event_id
		LEFT JOIN event_documents doc ON doc.id = chunk.event_document_id
		WHERE chunk.event_id = ANY($1::bigint[]) AND parent.source = 'zendesk' AND NOT parent.deleted
		AND parent.scope = ANY($2::text[])
		AND chunk.kind IN ('document_text', 'image_caption')
		ORDER BY (chunk.event_id = $3) DESC, parent.occurred_at, chunk.event_id,
		chunk.event_document_id, chunk.chunk_idx, chunk.id LIMIT 257`, eventIDs, scopes, eventID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var parentID int64
		var kind, body, filename, completeness string
		var sourceTruncated bool
		var pages, extracted int
		if err := rows.Scan(&parentID, &kind, &body, &sourceTruncated, &filename, &pages, &extracted, &completeness); err != nil {
			return "", false, err
		}
		count++
		if count > 256 {
			truncated = true
			break
		}
		truncated = truncated || sourceTruncated
		appendBounded(fmt.Sprintf("\n[Derived attachment content, not the event author's words; event_id=%d; kind=%s; file=%s; pages=%d; pages_extracted=%d; completeness=%s]\n%s\n", parentID, kind, filename, pages, extracted, completeness, body), (64<<10)-128)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if truncated {
		content.WriteString("\n[Truncated: additional indexed conversation or attachment content omitted.]\n")
	}
	return content.String(), truncated, nil
}
