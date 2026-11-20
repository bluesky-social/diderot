package notion

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// TestNulBytePageDoesNotAbortRootReconcile pins that a page Postgres cannot
// store as given does not abort a Notion root's walk. It is driven through
// the real write path (EventSinkAdapter -> events.DBSink) against the test
// Postgres, not a fake Ingester.
//
// Trigger: a page's rendered block text contains a literal NUL byte
// (U+0000). Postgres text columns reject that byte (SQLSTATE 22021).
//
// Contract: this package's own TestWalkRoot_EmptyDescendantDoesNotAbortSiblings
// establishes that one child page the walk can't use (there, an empty
// page) must not stop its siblings from being walked and ingested.
// A page Postgres couldn't store would not get this treatment from the
// walker: IngestNotion's error is neither errSkipped nor isSkipError
// (404/403 only), so the worker goroutine in WalkRootReport returns the
// error, which aborts the whole root's walk — discarding the healthy
// sibling's already-committed progress along with it (no event row
// commits transactionally with "this root finished walking").
//
// The guarantee is at the sink (internal/events), not in the walker's
// error classification: DBSink.Upsert sanitizes NUL bytes and invalid
// UTF-8 out of every text field before writing, so the poisoned page is
// stored successfully instead of erroring at all, and WalkRoot completes
// normally.
func TestNulBytePageDoesNotAbortRootReconcile(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"HEALTHY","type":"child_page","has_children":false,"child_page":{"title":"Healthy"}},
			{"object":"block","id":"POISONED","type":"child_page","has_children":false,"child_page":{"title":"Poisoned"}}
		],
		"has_more":false
	}`)
	srv.set("GET /pages/HEALTHY", minimalPageBody("HEALTHY", "Healthy", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/HEALTHY/children", `{"object":"list","results":[{"object":"block","id":"para-h","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"healthy body text"}]}}],"has_more":false}`)
	srv.set("GET /pages/POISONED", minimalPageBody("POISONED", "Poisoned", "2026-05-18T00:00:00Z", ""))
	// A literal NUL inside the rendered block text. Postgres text columns
	// cannot store it (SQLSTATE 22021); nothing in the walker or
	// EventSinkAdapter stripped or rejected it before the write.
	srv.set("GET /blocks/POISONED/children", `{"object":"list","results":[{"object":"block","id":"para-p","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"poisoned\u0000text"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, rootsTestDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'notion'`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	adapter := NewEventSinkAdapter(events.NewDBSink(pool, prometheus.NewRegistry()), pool)

	w := newWalkerForTest(srv, adapter)
	w.Concurrency = 1 // deterministic processing order: Root, Healthy, Poisoned

	// The actual contract: one page Postgres would have refused must not
	// keep its perfectly healthy sibling's root-level reconcile from
	// completing. Before the fix it did: WalkRoot("R") returned the
	// sink's 22021 error.
	processed, err := w.WalkRoot(ctx, "R")
	if err != nil {
		t.Fatalf("WalkRoot(\"R\") = %v; a page with a NUL byte must be sanitized and stored, not fail the whole root's reconcile (compare TestWalkRoot_EmptyDescendantDoesNotAbortSiblings)", err)
	}
	if processed != 3 {
		t.Fatalf("WalkRoot(\"R\") processed %d pages, want 3 (Root, Healthy, Poisoned)", processed)
	}

	// The poisoned page is stored, with the NUL byte stripped from the
	// body the walker rendered (a single paragraph block renders as
	// "<text>\n\n").
	var body string
	if err := pool.QueryRow(ctx,
		`SELECT body FROM events WHERE source = 'notion' AND external_id = 'POISONED' AND NOT deleted`,
	).Scan(&body); err != nil {
		t.Fatalf("read poisoned page body: %v", err)
	}
	const wantBody = "poisonedtext\n\n"
	if body != wantBody {
		t.Fatalf("poisoned page body = %q, want %q (NUL byte stripped)", body, wantBody)
	}

	// The healthy sibling is unaffected.
	var healthyBody string
	if err := pool.QueryRow(ctx,
		`SELECT body FROM events WHERE source = 'notion' AND external_id = 'HEALTHY' AND NOT deleted`,
	).Scan(&healthyBody); err != nil {
		t.Fatalf("read healthy page body: %v", err)
	}
	const wantHealthyBody = "healthy body text\n\n"
	if healthyBody != wantHealthyBody {
		t.Fatalf("healthy page body = %q, want %q", healthyBody, wantHealthyBody)
	}
}

// TestIngestOneSkipsNulOnlyNativeMarkdownBody pins that a page whose body
// is nothing but NUL bytes is skipped, driven through the
// default path (UseNativeMarkdown=true, where Notion's Markdown
// endpoint response passes through as Body with no local padding — unlike
// the legacy block renderer, every block type for which appends a
// trailing "\n" or "\n\n", which would mask this case by leaving
// whitespace behind for TrimSpace to catch even without sanitizing).
//
// Trigger: Notion's page-Markdown response is exactly a NUL byte.
// ingestOne's old `strings.TrimSpace(body) == ""` check does not
// consider "\x00" empty (NUL isn't whitespace), so it fell through to
// call Sink.Upsert with Body == "\x00". Upsert now sanitizes Body to ""
// and fails validate()'s "Body is required" check — a plain ingest
// error, neither errSkipped nor 404/403 (isSkipError), which
// WalkRootReport's worker loop treats as fatal and aborts the whole
// root's walk on — discarding a healthy sibling's already-committed
// progress exactly as an unsanitized sink would, just reached through a
// different field.
//
// The fix makes ingestOne check
// strings.TrimSpace(events.SanitizeText(body)) instead, so this page is
// recognized as having nothing to embed and returns errSkipped — same as
// a page with no rendered content at all — instead of ever reaching
// Sink.Upsert.
func TestIngestOneSkipsNulOnlyNativeMarkdownBody(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/p1/markdown", `{
		"object":"page_markdown","id":"p1","markdown":"\u0000",
		"truncated":false,"unknown_block_ids":[]
	}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"Tester"}`)
	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	w.UseNativeMarkdown = true
	var page Page
	if err := json.Unmarshal([]byte(minimalPageBody("p1", "NulOnly", "2026-05-18T12:00:00Z", "")), &page); err != nil {
		t.Fatalf("page fixture: %v", err)
	}

	_, err := w.ingestOne(context.Background(), &page, []Block{}, nil)
	if !errors.Is(err, errSkipped) {
		t.Fatalf("ingestOne(NUL-only native markdown body) = %v, want errSkipped\n"+
			"a different (or nil) error means the page reached Sink.Upsert instead of being "+
			"recognized as having nothing to embed", err)
	}
	if got := ing.snapshot(); len(got) != 0 {
		t.Fatalf("ingester saw %d events for a skipped page; want 0: %+v", len(got), got)
	}
}
