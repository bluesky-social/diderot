package notion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Status is when search and the walk of the managed roots last ran, the
// walk waiting to run, and how many pages search found outside every root.
func (Connector) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	var discovery, reconcile, backfill *time.Time
	err := deps.Pool.QueryRow(ctx, `SELECT last_discovery_at, last_reconcile_at, backfill_requested_at
		FROM notion_sync WHERE id = 1`).Scan(&discovery, &reconcile, &backfill)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return settings.Status{}, fmt.Errorf("notion status: %w", err)
	}
	discoveries, err := NewRootsStore(deps.Pool).ListDiscoveries(ctx)
	if err != nil {
		return settings.Status{}, fmt.Errorf("notion discoveries: %w", err)
	}
	var uncovered int64
	for _, item := range discoveries {
		if !item.Covered {
			uncovered++
		}
	}
	return settings.Status{Rows: []settings.StatusRow{
		{Label: "Last search for shared pages", Value: discovery},
		{Label: "Pages last read", Value: reconcile},
		{Label: "Last re-read requested", Value: backfill},
		{Label: "Shared pages not indexed", Value: uncovered},
	}}, nil
}
