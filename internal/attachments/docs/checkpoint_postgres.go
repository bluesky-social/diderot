package docs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

type postgresPageCheckpointStore struct {
	pool         *pgxpool.Pool
	documentID   int64
	eventID      int64
	sourceSHA256 string
	redactor     files.Redactor
	logger       *slog.Logger
	metrics      *workerMetrics
}

func (s *postgresPageCheckpointStore) Load(
	ctx context.Context, modelID string,
) ([]PageCheckpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT page_number, markdown, cost_usd, truncated
		  FROM event_document_extract_parts
		 WHERE event_document_id = $1
		   AND source_sha256 = decode($2, 'hex')
		   AND extract_model = $3
		 ORDER BY page_number
	`, s.documentID, s.sourceSHA256, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PageCheckpoint
	for rows.Next() {
		var page PageCheckpoint
		if err := rows.Scan(
			&page.PageNumber, &page.Markdown, &page.CostUSD, &page.Truncated,
		); err != nil {
			return nil, err
		}
		out = append(out, page)
	}
	return out, rows.Err()
}

func (s *postgresPageCheckpointStore) Save(
	ctx context.Context, modelID string, page PageCheckpoint,
) (PageCheckpoint, error) {
	if s.redactor != nil {
		redacted, findings := s.redactor.Redact(page.Markdown)
		page.Markdown = redacted
		if len(findings) > 0 {
			logger := s.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("docs: redacted secrets from extraction checkpoint",
				"document_id", s.documentID, "event_id", s.eventID,
				"rule_ids", findings, "count", len(findings))
			if s.metrics != nil {
				s.metrics.redactions.Add(float64(len(findings)))
			}
		}
	}

	// First writer wins if stale claims race on the same page.
	var saved PageCheckpoint
	err := s.pool.QueryRow(ctx, `
		WITH refreshed AS (
			UPDATE event_documents SET claimed_at = now() WHERE id = $1
			RETURNING id
		), inserted AS (
			INSERT INTO event_document_extract_parts
				(event_document_id, source_sha256, extract_model, page_number,
				 markdown, cost_usd, truncated)
			SELECT id, decode($2, 'hex'), $3, $4, $5, $6, $7
			  FROM refreshed
			ON CONFLICT (event_document_id, source_sha256, extract_model, page_number)
			DO UPDATE SET page_number = event_document_extract_parts.page_number
			RETURNING page_number, markdown, cost_usd, truncated
		)
		SELECT page_number, markdown, cost_usd, truncated
		  FROM inserted
	`, s.documentID, s.sourceSHA256, modelID, page.PageNumber, page.Markdown,
		page.CostUSD, page.Truncated,
	).Scan(
		&saved.PageNumber, &saved.Markdown, &saved.CostUSD, &saved.Truncated,
	)
	if err != nil {
		return PageCheckpoint{}, fmt.Errorf("save page checkpoint %d: %w", page.PageNumber, err)
	}
	return saved, nil
}
