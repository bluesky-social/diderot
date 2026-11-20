package notion

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestNulByteDiscoveryTitleDoesNotStallDiscoveryWatermark pins that
// storeDiscoveries sanitizes titles itself: it writes Notion Search result
// titles into notion_discovered_resources directly (tx.Exec), bypassing
// events.DBSink entirely, so the sink's sanitizing has no effect on it.
//
// Trigger: a Notion Search result's title contains a literal NUL byte.
// Postgres text columns reject that byte (SQLSTATE 22021).
//
// Unsanitized, that error propagates out of storeDiscoveries to its caller
// (the discovery tick), which never advances last_discovery_at — so every
// subsequent discovery tick re-fetches the same Search result and fails on
// the same NUL byte, forever (discovery is lower-stakes than event ingest —
// it's coverage-audit metadata only, per the table's own comment — but the
// stall mechanism is identical).
//
// The fix sanitizes Title (and ParentID/URL, the other free-text-shaped
// columns) with events.SanitizeText before the insert, the same rule the
// sink applies, reusing the same helper rather than inventing a second
// sanitizing rule for this one call site.
func TestNulByteDiscoveryTitleDoesNotStallDiscoveryWatermark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE notion_discovered_resources, notion_sync_runs CASCADE`); err != nil {
		t.Fatalf("clean: %v", err)
	}

	w := &SyncWorker{pool: pool}
	runID, err := w.startRun(ctx, "discovery", time.Now())
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}

	resources := []DiscoveredResource{
		{ObjectType: RootTypeDataSource, ObjectID: "11111111-1111-1111-1111-111111111111", Title: "poisoned\u0000title", URL: "https://notion.so/poisoned"},
	}
	if err := w.storeDiscoveries(ctx, runID, time.Now(), resources); err != nil {
		t.Fatalf("storeDiscoveries() = %v, want nil (a NUL byte in a discovered title must be sanitized, not fail the whole discovery tick)", err)
	}

	var title string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM notion_discovered_resources WHERE object_id = $1`,
		"11111111-1111-1111-1111-111111111111",
	).Scan(&title); err != nil {
		t.Fatalf("read discovered resource: %v", err)
	}
	const want = "poisonedtitle"
	if title != want {
		t.Fatalf("title = %q, want %q (NUL byte stripped)", title, want)
	}
}
