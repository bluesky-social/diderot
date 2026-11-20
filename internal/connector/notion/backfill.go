package notion

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Backfill is a walk of every managed root now.
func (Connector) Backfill() source.Backfill {
	return source.Backfill{
		Summary: "Read every page listed above now instead of waiting for the daily read.",
		Confirm: "Read every page again", Action: "Read now",
	}
}

// RequestBackfill resets the durable Notion search cursor. The leader
// worker consumes bounded pages; this call never holds an HTTP request
// open for the traversal.
func (Connector) RequestBackfill(ctx context.Context, deps source.Deps, _ settings.Values) (time.Time, error) {
	var requestedAt time.Time
	err := deps.Pool.QueryRow(ctx, `
		UPDATE notion_sync
		   SET backfill_requested_at = clock_timestamp(),
		       backfill_cursor = NULL
		 WHERE id = 1
		RETURNING backfill_requested_at
	`).Scan(&requestedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("request notion backfill: %w", err)
	}
	return requestedAt, nil
}
