// Package reembed regenerates embeddings for the existing events
// corpus by re-running each row through the current ingest code
// paths. Useful when the chunker changes, the prefix builder
// changes, the embedder model changes, or we just want to
// retroactively apply a new resolver behavior.
//
// The heavy lifting happens via events.DBSink.Upsert with
// ForceUpdate=true: the sink's UPDATE branch deletes existing
// chunks, re-chunks the body, and inserts new chunk rows with
// embedded_at=NULL. The embedder worker picks them up on its
// next poll.
//
// Operationally: one Run at a time, on the leader, which claims
// the job any replica requested (job.go); progress is saved to
// admin_jobs. The sink's own transaction boundaries provide
// safety; an interruption means some events got reembedded and
// some didn't — re-running the job is idempotent (already-new
// rows produce the same chunks; the UPDATE path wastes a bit
// of embedder time re-generating them but correctness holds).
//
// The job runs alongside live ingest, so a row's batch SELECT and its
// later Upsert can straddle a concurrent edit or delete of that same
// row. processRow ties its write to the snapshot it read
// (events.Event.CheckUnchangedSince/UnchangedSince): a write that would
// land on a row changed since the read is skipped rather than applied,
// so the job can never resurrect a deleted event or revert an edit it
// raced.
package reembed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// ErrSourceOff is what a SourceRebuilder returns while the source whose
// events it rebuilds isn't running. The driver stops re-embedding that
// source, with this as its error.
var ErrSourceOff = errors.New("the source's connector is off")

// SourceRebuilder encapsulates the per-source knowledge about how
// to re-derive a row's presentation-layer fields (actor display
// name, context prefix, resolved body mentions) from the stored
// raw payload. One implementation per source that supports it.
//
// The reembed driver calls Rebuild for each events row it's
// processing; the impl returns a populated events.Event with
// ForceUpdate=true that drives the sink.
type SourceRebuilder interface {
	// Source returns the events.source value this rebuilder
	// handles. The driver filters events by this.
	Source() string

	// Rebuild reconstructs the Event from the raw payload +
	// stored row ids. Returns (*events.Event, nil) on success;
	// (nil, nil) to skip this row (e.g. subtype that no longer
	// maps to an event); error on genuine failure.
	Rebuild(ctx context.Context, row StoredRow) (*events.Event, error)
}

// StoredRow is the subset of an events row the driver passes into
// the Rebuilder. Mirrors the Postgres columns we SELECT.
type StoredRow struct {
	ID         int64
	ExternalID string
	OccurredAt time.Time
	ActorID    string
	ThreadID   string
	URL        string
	Raw        json.RawMessage
	// UpdatedAt is the row's updated_at at SELECT time, nil if the row
	// had never been updated (events.updated_at is nullable; it's set
	// only by an UPDATE, never the initial INSERT). processRow feeds
	// it back as the rebuilt Event's UnchangedSince so the later
	// Upsert — which may run seconds after this row was read — detects
	// a delete or edit that commits in between and skips the row
	// instead of overwriting that newer commit with this stale
	// snapshot.
	UpdatedAt *time.Time
}

// Progress is the snapshot-safe counters + state the driver
// updates as it works. The admin endpoint polls these.
type Progress struct {
	Source    atomic.Value // string: current source being processed
	Total     atomic.Int64 // rows the driver plans to process for this source
	Processed atomic.Int64
	Rewritten atomic.Int64 // sink returned WasUpdate (real re-embed queued)
	NoChange  atomic.Int64 // byte-identical — shouldn't happen with ForceUpdate, but count in case
	Skipped   atomic.Int64 // rebuilder returned (nil, nil), or the row changed since it was read (Stale)
	Errors    atomic.Int64
	StartedAt time.Time
	Finished  atomic.Bool
	LastErr   atomic.Value // string
}

// Driver iterates the events table by source and drives the
// rebuilders. One Driver per Run; not meant to outlive the job.
type Driver struct {
	pool       *pgxpool.Pool
	sink       *events.DBSink
	rebuilders map[string]SourceRebuilder
	logger     *slog.Logger
}

// NewDriver constructs a driver with the given rebuilders. Each
// rebuilder is registered against its Source() value.
func NewDriver(pool *pgxpool.Pool, sink *events.DBSink, logger *slog.Logger, rbs ...SourceRebuilder) *Driver {
	if logger == nil {
		logger = slog.Default()
	}
	m := make(map[string]SourceRebuilder, len(rbs))
	for _, rb := range rbs {
		m[rb.Source()] = rb
	}
	return &Driver{pool: pool, sink: sink, rebuilders: m, logger: logger}
}

// Sources returns the source names the driver knows how to
// reembed. The admin endpoint surfaces this so an operator can
// pick a specific source to reembed (say, just slack without
// touching the others).
func (d *Driver) Sources() []string {
	out := make([]string, 0, len(d.rebuilders))
	for s := range d.rebuilders {
		out = append(out, s)
	}
	return out
}

// Run regenerates embeddings for every row whose source matches
// a registered rebuilder. If sources is non-empty, only those are
// processed.
//
// Iterates by id ascending (so the caller can infer a stable
// progress ordering). Paginates with a cursor rather than
// SELECT * to keep memory bounded.
func (d *Driver) Run(ctx context.Context, sources []string, progress *Progress) error {
	if progress == nil {
		progress = &Progress{}
	}
	progress.StartedAt = time.Now()

	targets := sources
	if len(targets) == 0 {
		targets = d.Sources()
	}

	for _, src := range targets {
		rb, ok := d.rebuilders[src]
		if !ok {
			d.logger.Warn("reembed: no rebuilder registered; skipping", "source", src)
			continue
		}
		progress.Source.Store(src)
		if err := d.runSource(ctx, src, rb, progress); err != nil {
			if errors.Is(err, context.Canceled) {
				progress.Finished.Store(true)
				return err
			}
			d.logger.Error("reembed: source failed", "source", src, "err", err)
			progress.LastErr.Store(err.Error())
			progress.Errors.Add(1)
			// Continue to the next source.
		}
	}
	progress.Finished.Store(true)
	return nil
}

func (d *Driver) runSource(ctx context.Context, src string, rb SourceRebuilder, progress *Progress) error {
	// Per-source count goes into Total via Add, not Store — when a
	// run spans multiple sources (all-sources mode), the processed
	// counter is cumulative and Total must be too, or the percent
	// goes sideways on the second source.
	var total int64
	if err := d.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source = $1 AND deleted = false`,
		src).Scan(&total); err != nil {
		return fmt.Errorf("count: %w", err)
	}
	progress.Total.Add(total)
	d.logger.Info("reembed: starting source", "source", src, "rows", total)

	const batchSize = 200
	var cursorID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := d.pool.Query(ctx, `
			SELECT id, external_id, occurred_at,
			       COALESCE(actor_id, ''),
			       COALESCE(thread_id, ''),
			       COALESCE(url, ''),
			       raw, updated_at
			  FROM events
			 WHERE source = $1
			   AND id > $2
			   AND deleted = false
			 ORDER BY id
			 LIMIT $3
		`, src, cursorID, batchSize)
		if err != nil {
			return fmt.Errorf("select: %w", err)
		}
		var batch []StoredRow
		for rows.Next() {
			var r StoredRow
			if err := rows.Scan(
				&r.ID, &r.ExternalID, &r.OccurredAt,
				&r.ActorID, &r.ThreadID, &r.URL, &r.Raw, &r.UpdatedAt,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan: %w", err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}

		for _, row := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := d.processRow(ctx, rb, row, progress); err != nil {
				if errors.Is(err, ErrSourceOff) {
					return fmt.Errorf("%s: %w", src, err)
				}
				progress.Errors.Add(1)
				progress.LastErr.Store(err.Error())
				d.logger.Warn("reembed: row failed; continuing",
					"source", src, "event_id", row.ID, "err", err)
			}
			progress.Processed.Add(1)
			cursorID = row.ID
		}
	}
}

func (d *Driver) processRow(ctx context.Context, rb SourceRebuilder, row StoredRow, progress *Progress) error {
	ev, err := rb.Rebuild(ctx, row)
	if err != nil {
		return err
	}
	if ev == nil {
		progress.Skipped.Add(1)
		return nil
	}
	ev.ForceUpdate = true
	// The rebuild is driven entirely by the row snapshot the batch
	// SELECT took, which can be seconds old by the time we get here
	// (Directory lookups, a prefix query, the Upsert transaction all
	// run per row). Tie the write to that snapshot: if a connector's
	// own edit or delete committed on this row since the SELECT, its
	// updated_at has moved and the sink declines to write rather than
	// clobber the newer commit with this stale rebuild.
	ev.CheckUnchangedSince = true
	ev.UnchangedSince = row.UpdatedAt
	res, err := d.sink.Upsert(ctx, *ev)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	switch {
	case res.Stale:
		// Someone else wrote this row after we read it. Don't retry
		// here: either a later reembed pass picks it up fresh, or the
		// concurrent write that made us stale was itself a live
		// Upsert that already re-chunked and queued embedding for the
		// new content, so there's nothing left for this job to do.
		progress.Skipped.Add(1)
	case res.WasUpdate:
		progress.Rewritten.Add(1)
	case res.NoChange:
		progress.NoChange.Add(1)
	default:
		// Inserted? Shouldn't happen because the row already exists
		// — but don't crash if it does (race with a concurrent
		// delete + re-insert, for instance).
		progress.Rewritten.Add(1)
	}
	return nil
}
