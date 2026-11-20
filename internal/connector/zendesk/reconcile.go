package zendesk

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func (worker *Worker) Reconcile(ctx context.Context) (bool, error) {
	var cursor string
	var watermark int64
	err := worker.Pool.QueryRow(ctx, `SELECT cursor, watermark FROM zendesk_sync WHERE tenant = $1 AND stream = 'reconcile'`, worker.Client.origin).Scan(&cursor, &watermark)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if time.Now().Unix() < watermark {
		return true, nil
	}
	var afterID int64
	if cursor != "" {
		afterID, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return false, errors.New("zendesk: invalid reconciliation checkpoint")
		}
	}
	rows, err := worker.Pool.Query(ctx, `SELECT min(id), split_part(external_id, ':', 1), split_part(external_id, ':', 2)::bigint
		FROM events WHERE source = 'zendesk' AND NOT deleted
		AND external_id ~ '^(ticket|article):[0-9]+(:|$)'
		AND (external_id = thread_id OR starts_with(external_id, 'article:'))
		GROUP BY split_part(external_id, ':', 1), split_part(external_id, ':', 2)
		HAVING min(id) > $1 ORDER BY min(id) LIMIT 10`, afterID)
	if err != nil {
		return false, err
	}
	records, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		EventID int64
		Kind    string
		ID      int64
	}])
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.Kind == "ticket" {
			err = worker.refreshTicket(ctx, record.ID)
		} else {
			err = worker.RefreshArticle(ctx, record.ID)
		}
		if err != nil {
			if !deferTicketError(err) {
				return false, err
			}
			if err := worker.deferRecord(ctx, record.Kind, record.ID); err != nil {
				return false, err
			}
		}
		afterID = record.EventID
	}
	caughtUp := len(records) < 10
	watermark = 0
	if caughtUp {
		afterID = 0
		watermark = time.Now().Add(24 * time.Hour).Unix()
	}
	_, err = worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, cursor, watermark)
		VALUES ($1, 'reconcile', $2, $3) ON CONFLICT (tenant, stream)
		DO UPDATE SET cursor = EXCLUDED.cursor, watermark = EXCLUDED.watermark, updated_at = now()`, worker.Client.origin, strconv.FormatInt(afterID, 10), watermark)
	return caughtUp, err
}
