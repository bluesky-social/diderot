package notion

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// EventSinkAdapter bridges Ingester (the walker's write surface) and
// NotionHider (the roots store's) to events.DBSink (the diderot-wide
// upsert path). Stamps source="notion" + Raw={} per the contract (source
// is always server-stamped). Body of event_chunks is whatever the walker
// rendered; the chunker handles the rest.
//
// Image reconcile: after Upsert returns the event_id, calls
// events.ReconcileImages with ev.Images. Two transaction
// boundaries (events tx, then images tx); failure between them
// is recoverable on the next walker tick — see design doc §
// "Failure modes / Resilience".
type EventSinkAdapter struct {
	sink *events.DBSink
	pool *pgxpool.Pool
}

// NewEventSinkAdapter writes through sink and reconciles images in pool.
func NewEventSinkAdapter(sink *events.DBSink, pool *pgxpool.Pool) *EventSinkAdapter {
	return &EventSinkAdapter{sink: sink, pool: pool}
}

func (a *EventSinkAdapter) IngestNotion(ctx context.Context, ev IngestEvent) (IngestOutcome, error) {
	raw := ev.Raw
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	res, err := a.sink.Upsert(ctx, events.Event{
		Source:         "notion",
		ExternalID:     ev.ExternalID,
		OccurredAt:     ev.OccurredAt,
		Actor:          ev.Actor,
		ActorCanonical: ev.ActorCanonical,
		ActorID:        ev.ActorID,
		ThreadID:       ev.ThreadID,
		Title:          ev.Title,
		Body:           ev.Body,
		URL:            ev.URL,
		Raw:            raw,
		ContextPrefix:  ev.ContextPrefix,
		// Notion workspace docs are broadly visible internal corpus.
		Scope: events.ScopeInternal,
	})
	if err != nil {
		return "", err
	}
	// ReconcileImages runs unconditionally — even on NoChange
	// Upsert, an earlier failed Reconcile half can be backfilled
	// here via the next walker tick re-calling IngestNotion.
	if err := events.ReconcileImages(ctx, a.pool, res.EventID,
		ImageRefsToEvents(ev.Images)); err != nil {
		return "", err
	}
	switch {
	case res.NoChange:
		return IngestUnchanged, nil
	case res.WasUpdate:
		return IngestUpdated, nil
	default:
		return IngestCreated, nil
	}
}

func (a *EventSinkAdapter) HideNotionTx(ctx context.Context, tx pgx.Tx, externalID string) (func(context.Context), error) {
	_, postCommit, err := a.sink.DeleteInTx(ctx, tx, "notion", externalID)
	return postCommit, err
}
