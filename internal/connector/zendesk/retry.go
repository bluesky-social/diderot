package zendesk

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (worker *Worker) deferRecord(ctx context.Context, kind string, sourceID int64) error {
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_retry (tenant, kind, source_id) VALUES ($1, $2, $3)
		ON CONFLICT (tenant, kind, source_id) DO NOTHING`, worker.Client.origin, kind, sourceID)
	if err == nil && worker.Logger != nil {
		worker.Logger.Warn("zendesk record queued for retry", "kind", kind, "source_id", sourceID)
	}
	return err
}

func (worker *Worker) clearRecordRetry(ctx context.Context, kind string, sourceID int64) error {
	_, err := worker.Pool.Exec(ctx, `DELETE FROM zendesk_retry WHERE tenant = $1 AND kind = $2 AND source_id = $3`, worker.Client.origin, kind, sourceID)
	return err
}

func (worker *Worker) RetryDeferred(ctx context.Context) error {
	rows, err := worker.Pool.Query(ctx, `SELECT kind, source_id FROM zendesk_retry WHERE tenant = $1 AND not_before <= now() ORDER BY not_before, kind, source_id LIMIT 10`, worker.Client.origin)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Kind string
		ID   int64
	}])
	if err != nil {
		return err
	}
	for _, item := range items {
		var readErr error
		if item.Kind == "article" {
			readErr = worker.RefreshArticle(ctx, item.ID)
		} else {
			readErr = worker.refreshTicket(ctx, item.ID)
		}
		if readErr == nil {
			continue
		}
		if !deferTicketError(readErr) {
			return readErr
		}
		_, err := worker.Pool.Exec(ctx, `UPDATE zendesk_retry
			SET attempts = attempts + 1, not_before = now() + make_interval(secs => LEAST(86400, 900 * power(2, LEAST(attempts, 7)))::int)
			WHERE tenant = $1 AND kind = $2 AND source_id = $3`, worker.Client.origin, item.Kind, item.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *Worker) refreshTicket(ctx context.Context, ticketID int64) error {
	ticket, authors, err := worker.Client.Ticket(ctx, ticketID)
	var failure *APIError
	if (errors.As(err, &failure) && (failure.Status == 404 || failure.Status == 410)) || (err == nil && ticket.Status == "deleted") {
		return worker.deleteTicket(ctx, ticketID)
	}
	if errors.As(err, &failure) && failure.Status == 403 {
		// Losing read access can mean the ticket moved into a group the
		// connector can't see. Fail closed: drop the indexed copy but keep any
		// retry record; the caller defers it, and a later readable, indexed
		// refresh restores it.
		if deleteErr := worker.tombstoneTicket(ctx, ticketID); deleteErr != nil {
			return deleteErr
		}
		return err
	}
	if err != nil {
		return err
	}
	users := make(map[int64]User)
	for _, user := range authors {
		users[user.ID] = user
	}
	return worker.importTicket(ctx, ticket, users)
}
