package zendesk

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Status is when tickets and articles were last read, when the
// reconciliation of indexed tickets last moved, and the backfill waiting to
// start.
func (Connector) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	var tickets, articles, reconcile *time.Time
	err := deps.Pool.QueryRow(ctx, `SELECT
		max(updated_at) FILTER (WHERE stream IN ('tickets', 'tickets-recent')),
		max(updated_at) FILTER (WHERE stream = 'articles'),
		max(updated_at) FILTER (WHERE stream = 'reconcile')
		FROM zendesk_sync`).Scan(&tickets, &articles, &reconcile)
	if err != nil {
		return settings.Status{}, fmt.Errorf("zendesk status: %w", err)
	}
	backfill, err := pendingBackfill(ctx, deps.State)
	if err != nil {
		return settings.Status{}, fmt.Errorf("zendesk status: %w", err)
	}
	return settings.Status{Rows: []settings.StatusRow{
		{Label: "Last ticket sync", Value: tickets},
		{Label: "Last article sync", Value: articles},
		{Label: "Last reconcile", Value: reconcile},
		{Label: "Backfill requested", Value: backfill},
	}}, nil
}
