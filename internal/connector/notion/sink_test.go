package notion

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// TestEventSinkAdapter: pages land as source=notion, internal-scope events
// with their images reconciled, report created/unchanged/updated, and hide
// in the caller's transaction.
func TestEventSinkAdapter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	const externalID = "sink-adapter-page"
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'notion' AND external_id = $1`, externalID); err != nil {
		t.Fatalf("clean: %v", err)
	}

	adapter := NewEventSinkAdapter(events.NewDBSink(pool, prometheus.NewRegistry()), pool)
	page := IngestEvent{
		ExternalID: externalID, OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Actor: "Example Author", URL: "https://www.notion.so/sink-adapter-page", Title: "Sink adapter",
		Body: "first body", Images: []ImageRef{{SourceImageID: "block-1:key-1", Mimetype: "image/png"}},
	}
	for _, step := range []struct {
		body string
		want IngestOutcome
	}{
		{"first body", IngestCreated},
		{"first body", IngestUnchanged},
		{"second body", IngestUpdated},
	} {
		page.Body = step.body
		got, err := adapter.IngestNotion(ctx, page)
		if err != nil {
			t.Fatalf("IngestNotion(%q): %v", step.body, err)
		}
		if got != step.want {
			t.Fatalf("IngestNotion(%q) = %q, want %q", step.body, got, step.want)
		}
	}

	var (
		eventID int64
		scope   string
		raw     string
		images  int
	)
	if err := pool.QueryRow(ctx, `
		SELECT e.id, e.scope, e.raw::text, (SELECT count(*) FROM event_images i WHERE i.event_id = e.id)
		FROM events e WHERE e.source = 'notion' AND e.external_id = $1`, externalID).
		Scan(&eventID, &scope, &raw, &images); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if scope != events.ScopeInternal || raw != "{}" || images != 1 {
		t.Fatalf("event scope=%q raw=%s images=%d, want internal, {}, 1", scope, raw, images)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	postCommit, err := adapter.HideNotionTx(ctx, tx, externalID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("HideNotionTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	var deleted bool
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE id = $1`, eventID).Scan(&deleted); err != nil {
		t.Fatalf("read deleted: %v", err)
	}
	if !deleted {
		t.Fatal("the hidden page's event is not deleted")
	}
}
