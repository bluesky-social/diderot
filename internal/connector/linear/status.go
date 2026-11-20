package linear

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Status is when each of the sync worker's loops last finished, and the
// backfill waiting or in progress.
func (Connector) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	var lastSync, reconcile, visibility, backfill *time.Time
	var project string
	err := deps.Pool.QueryRow(ctx, `SELECT last_sync_at, last_reconcile_at, visibility_swept_at,
		backfill_requested_at, COALESCE(backfill_project, '') FROM linear_sync WHERE id = 1`).Scan(
		&lastSync, &reconcile, &visibility, &backfill, &project)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return settings.Status{}, fmt.Errorf("linear status: %w", err)
	}
	return settings.Status{Rows: []settings.StatusRow{
		{Label: "Last sync", Value: lastSync},
		{Label: "Last reconcile", Value: reconcile},
		{Label: "Private-team sweep", Value: visibility},
		{Label: "Backfill requested", Value: backfill, Detail: project},
	}}, nil
}
