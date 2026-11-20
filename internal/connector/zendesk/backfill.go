package zendesk

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// The State keys of a backfill: when one was last requested, which only
// RequestBackfill writes, and the request the sync worker last started,
// which only the worker writes.
const (
	backfillRequestedKey = "backfill_requested_at"
	backfillStartedKey   = "backfill_started_at"
)

// Backfill is a re-read of every ticket and Help Center article.
func (Connector) Backfill() source.Backfill {
	return source.Backfill{
		Summary: "Re-read every ticket and Help Center article from the start.",
		Confirm: "Re-read tickets and articles", Action: "Re-read",
	}
}

// RequestBackfill records a backfill, which the sync worker starts at its
// next pass by reading tickets and articles from the start again.
func (Connector) RequestBackfill(ctx context.Context, deps source.Deps, _ settings.Values) (time.Time, error) {
	requestedAt := time.Now().UTC()
	if err := deps.State.Set(ctx, backfillRequestedKey, requestedAt); err != nil {
		return time.Time{}, fmt.Errorf("request zendesk backfill: %w", err)
	}
	return requestedAt, nil
}

// pendingBackfill returns when the backfill waiting to start was requested,
// or nil when none is waiting.
func pendingBackfill(ctx context.Context, state source.State) (*time.Time, error) {
	var requested, started time.Time
	if _, err := state.Get(ctx, backfillRequestedKey, &requested); err != nil {
		return nil, err
	}
	if _, err := state.Get(ctx, backfillStartedKey, &started); err != nil {
		return nil, err
	}
	if !requested.After(started) {
		return nil, nil
	}
	return &requested, nil
}

// startBackfill starts the backfill waiting, if one is, by forgetting where
// the ticket and article streams had read to, and reports whether it did.
func (worker *Worker) startBackfill(ctx context.Context) (bool, error) {
	requested, err := pendingBackfill(ctx, worker.State)
	if err != nil || requested == nil {
		return false, err
	}
	if _, err := worker.Pool.Exec(ctx, `DELETE FROM zendesk_sync
		WHERE tenant = $1 AND stream IN ('tickets', 'tickets-recent', 'articles')`, worker.Client.origin); err != nil {
		return false, fmt.Errorf("zendesk: start backfill: %w", err)
	}
	if err := worker.State.Set(ctx, backfillStartedKey, *requested); err != nil {
		return false, fmt.Errorf("zendesk: start backfill: %w", err)
	}
	return true, nil
}
