package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIngester captures everything the walker emits.
type fakeIngester struct {
	mu sync.Mutex
	in []IngestEvent
}

func (f *fakeIngester) IngestNotion(_ context.Context, ev IngestEvent) (IngestOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in = append(f.in, ev)
	return IngestCreated, nil
}

func (f *fakeIngester) snapshot() []IngestEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]IngestEvent, len(f.in))
	copy(out, f.in)
	return out
}

// notionTestServer runs a stub Notion API. Routes:
//
//	POST /search                      → search results
//	GET  /pages/{id}                  → one page
//	GET  /blocks/{id}/children        → child blocks
//	GET  /databases/{id}              → database metadata
//	GET  /views                       → database views
//	GET  /data_sources/{id}           → data source metadata
//	POST /data_sources/{id}/query     → database rows
//	GET  /users/{id}                  → user
//
// Routes are configurable per-test via the response map keyed on
// the request's "<method> <path>" form.
type notionTestServer struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	canned   map[string]string // "method path" -> JSON body
	queries  []string
	failures map[string]int // hits before responding
}

func newNotionTestServer(t *testing.T) *notionTestServer {
	t.Helper()
	s := &notionTestServer{
		t:        t,
		canned:   map[string]string{},
		failures: map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *notionTestServer) baseURL() string { return s.srv.URL }

func (s *notionTestServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	s.queries = append(s.queries, key)
	body, ok := s.canned[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"object":"error","status":404,"key":%q}`, key))
		return
	}
	if hits, ok := s.failures[key]; ok && hits > 0 {
		s.failures[key] = hits - 1
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func (s *notionTestServer) set(key, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canned[key] = body
}

func newWalkerForTest(srv *notionTestServer, ing Ingester) *Walker {
	cfg := Config{
		Token:      "ntn_test",
		BaseURL:    srv.baseURL(),
		Rate:       100,
		Burst:      100,
		MaxRetries: 1,
		HTTPClient: srv.srv.Client(),
	}
	c, err := NewClient(cfg)
	if err != nil {
		srv.t.Fatalf("NewClient: %v", err)
	}
	w := NewWalker(c, NewDirCache(c), ing,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Concurrency = 2 // small but non-trivial
	w.UseNativeMarkdown = false
	return w
}

// minimalPageBody returns a JSON string for one page response.
func minimalPageBody(id, title, lastEdited string, parentDB string) string {
	parent := `{"type":"workspace","workspace":true}`
	if parentDB != "" {
		parent = fmt.Sprintf(`{"type":"database_id","database_id":%q}`, parentDB)
	}
	return fmt.Sprintf(`{
		"object":"page","id":%q,
		"created_time":"2026-01-01T00:00:00Z",
		"last_edited_time":%q,
		"last_edited_by":{"object":"user","id":"u-test"},
		"archived":false,
		"properties": {"Name": {"id":"title","type":"title","title":[{"plain_text":%q}]}},
		"parent":%s,
		"url":"https://notion.so/page-%s"
	}`, id, lastEdited, title, parent, id)
}

func TestSearchSince_StopsBelowCutoff(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("POST /search", `{
		"object":"list",
		"results":[
			`+minimalPageBody("p1", "Recent A", "2026-05-18T12:00:00Z", "")+`,
			`+minimalPageBody("p2", "Recent B", "2026-05-18T11:00:00Z", "")+`,
			`+minimalPageBody("p3", "Old C", "2026-05-15T00:00:00Z", "")+`
		],
		"has_more":false,
		"next_cursor":null
	}`)
	// All three pages have empty blocks responses (the renderer
	// recurses into blocks).
	for _, id := range []string{"p1", "p2", "p3"} {
		srv.set("GET /blocks/"+id+"/children",
			`{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	}
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"Tester","person":{"email":"tester@example.com"}}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)

	cutoff, _ := time.Parse(time.RFC3339, "2026-05-18T00:00:00Z")
	count, err := w.SearchSince(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("SearchSince: %v", err)
	}
	// Only p1 and p2 are above cutoff. p3 is below cutoff and
	// stops pagination — even though it's in the same response
	// page, we stop scanning at it.
	if count != 2 {
		t.Errorf("count=%d, want 2", count)
	}
	got := ing.snapshot()
	wantTitles := map[string]bool{"Recent A": true, "Recent B": true}
	for _, e := range got {
		if !wantTitles[e.Title] {
			t.Errorf("unexpected title %q", e.Title)
		}
		delete(wantTitles, e.Title)
	}
	if len(wantTitles) != 0 {
		t.Errorf("missing titles: %v", wantTitles)
	}
	// Actor resolved through user cache to email.
	for _, e := range got {
		if e.ActorCanonical != "tester@example.com" {
			t.Errorf("ActorCanonical=%q, want tester@example.com", e.ActorCanonical)
		}
	}
}

func TestSearchSince_SkipsArchived(t *testing.T) {
	srv := newNotionTestServer(t)
	body := `{
		"object":"page","id":"p1",
		"created_time":"2026-01-01T00:00:00Z",
		"last_edited_time":"2026-05-18T12:00:00Z",
		"last_edited_by":{"object":"user","id":"u-test"},
		"archived":true,
		"properties": {"Name": {"id":"title","type":"title","title":[{"plain_text":"X"}]}},
		"parent":{"type":"workspace","workspace":true},
		"url":"https://notion.so/p1"
	}`
	srv.set("POST /search", `{"object":"list","results":[`+body+`],"has_more":false}`)
	srv.set("GET /blocks/p1/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)

	count, err := w.SearchSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("SearchSince: %v", err)
	}
	// Archived page → skipped (no embedding).
	if count != 0 {
		t.Errorf("count=%d, want 0 (archived)", count)
	}
	if got := ing.snapshot(); len(got) != 0 {
		t.Errorf("ingester saw %d events; want 0", len(got))
	}
}

func TestIngestOneUsesNativeMarkdownAndPersistsSourceMetadata(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/p1/markdown", `{
		"object":"page_markdown","id":"p1","markdown":"# Native body\n\nUseful text\n\n![diagram](https://prod-files-secure.s3.us-west-2.amazonaws.com/x.png?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=rotating)",
		"truncated":false,"unknown_block_ids":[]
	}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"Tester"}`)
	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	w.UseNativeMarkdown = true
	pageJSON := minimalPageBody("p1", "Native", "2026-05-18T12:00:00Z", "")
	var page Page
	if err := json.Unmarshal([]byte(pageJSON), &page); err != nil {
		t.Fatalf("page fixture: %v", err)
	}

	if _, err := w.ingestOne(context.Background(), &page, []Block{}, nil); err != nil {
		t.Fatalf("ingestOne: %v", err)
	}
	got := ing.snapshot()
	if len(got) != 1 || got[0].Body != "# Native body\n\nUseful text\n\n![diagram]" {
		t.Fatalf("events=%+v", got)
	}
	if string(got[0].Raw) == "{}" || !strings.Contains(string(got[0].Raw), `"page_markdown"`) {
		t.Fatalf("raw metadata=%s", got[0].Raw)
	}
	if strings.Contains(string(got[0].Raw), "X-Amz-") {
		t.Fatalf("raw metadata retained expiring URL: %s", got[0].Raw)
	}
	if got[0].ContextPrefix != "Notion page: Native" {
		t.Fatalf("context prefix=%q", got[0].ContextPrefix)
	}
}

func TestIngestOneNativeMarkdownRawStableAcrossSignedURLRotation(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"Tester"}`)
	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	w.UseNativeMarkdown = true
	var page Page
	if err := json.Unmarshal([]byte(minimalPageBody("p1", "Native", "2026-05-18T12:00:00Z", "")), &page); err != nil {
		t.Fatalf("page fixture: %v", err)
	}

	for _, signature := range []string{"first", "second"} {
		srv.set("GET /pages/p1/markdown", fmt.Sprintf(`{
			"object":"page_markdown","id":"p1",
			"markdown":"# Stable body\n\n![diagram](https://prod-files-secure.s3.us-west-2.amazonaws.com/x.png?X-Amz-Signature=%s)",
			"truncated":false,"unknown_block_ids":[]}`, signature))
		if _, err := w.ingestOne(context.Background(), &page, []Block{}, nil); err != nil {
			t.Fatalf("ingestOne signature %q: %v", signature, err)
		}
	}

	got := ing.snapshot()
	if len(got) != 2 {
		t.Fatalf("events=%d, want two", len(got))
	}
	if string(got[0].Raw) != string(got[1].Raw) {
		t.Fatalf("raw metadata changed with signed URL:\nfirst:  %s\nsecond: %s", got[0].Raw, got[1].Raw)
	}
}

func TestSearchSince_PaginationFollowsCursor(t *testing.T) {
	srv := newNotionTestServer(t)
	// First page: 1 hit + has_more.
	page1Hit := minimalPageBody("p1", "First", "2026-05-18T12:00:00Z", "")
	page2Hit := minimalPageBody("p2", "Second", "2026-05-18T11:00:00Z", "")

	var calls int
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Custom handler: dispatch /search by call count.
		if r.URL.Path == "/search" {
			calls++
			w.WriteHeader(http.StatusOK)
			if calls == 1 {
				_, _ = io.WriteString(w, `{"object":"list","results":[`+page1Hit+`],"has_more":true,"next_cursor":"cur-2"}`)
			} else {
				_, _ = io.WriteString(w, `{"object":"list","results":[`+page2Hit+`],"has_more":false}`)
			}
			return
		}
		// Other routes go through the canned map.
		srv.handle(w, r)
	})

	for _, id := range []string{"p1", "p2"} {
		srv.set("GET /blocks/"+id+"/children",
			`{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	}
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	count, err := w.SearchSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("SearchSince: %v", err)
	}
	if count != 2 {
		t.Errorf("count=%d, want 2", count)
	}
	if calls != 2 {
		t.Errorf("calls=%d, want 2 (cursor follow)", calls)
	}
}

// TestSearchSince_WorkerErrorAbortsPromptly protects the bounded-concurrency
// SearchPage implementation from hanging when one ingest task fails while
// more results are waiting to start.
func TestSearchSince_WorkerErrorAbortsPromptly(t *testing.T) {
	srv := newNotionTestServer(t)
	// Put BAD first and enough hits after it to exercise task scheduling
	// after the first worker failure.
	ids := []string{"BAD", "P1", "P2", "P3", "P4"}
	var hits []string
	for _, id := range ids {
		hits = append(hits, minimalPageBody(id, id, "2026-05-18T12:00:00Z", ""))
	}
	srv.set("POST /search", `{"object":"list","results":[`+strings.Join(hits, ",")+`],"has_more":false}`)
	for _, id := range ids {
		srv.set("GET /blocks/"+id+"/children",
			`{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	}
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	wantErr := errors.New("simulated transient 5xx during ingest")
	ing := &erroringIngester{inner: &fakeIngester{}, failID: "BAD", errValue: wantErr}
	w := newWalkerForTest(srv, ing)
	w.Concurrency = 1 // deterministic: the single worker dies on BAD

	cutoff, _ := time.Parse(time.RFC3339, "2026-05-18T00:00:00Z")
	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		_, gotErr = w.SearchSince(context.Background(), cutoff)
	}()

	select {
	case <-done:
		if !errors.Is(gotErr, wantErr) {
			t.Fatalf("SearchSince returned err=%v, want wrapping of %v", gotErr, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SearchSince hung after a worker error")
	}
}

func TestWalkRoot_DescendsChildPagesAndDatabaseRows(t *testing.T) {
	srv := newNotionTestServer(t)

	// Root page R has a child_page CP1 and a child_database CDB.
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"b-cp","type":"child_page","has_children":false,"child_page":{"title":"Subpage"}},
			{"object":"block","id":"db-1","type":"child_database","has_children":false,"child_database":{"title":"Logs"}}
		],
		"has_more":false
	}`)

	// Child page CP. Notion's API treats child_page block_id == page_id (same UUID).
	srv.set("GET /pages/b-cp", minimalPageBody("b-cp", "Subpage", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/b-cp/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)

	// child_database → database metadata + 1 row.
	srv.set("GET /databases/db-1", `{"object":"database","id":"db-1","title":[{"plain_text":"Logs"}],"data_sources":[{"id":"ds-1","name":"Logs"}],"parent":{"type":"workspace","workspace":true},"url":""}`)
	srv.set("GET /views", `{"object":"list","results":[],"has_more":false}`)
	srv.set("GET /data_sources/ds-1", `{"object":"data_source","id":"ds-1","title":[{"plain_text":"Logs"}],"parent":{"type":"database_id","database_id":"db-1"},"url":""}`)
	srv.set("POST /data_sources/ds-1/query", `{
		"object":"list",
		"results":[`+minimalPageBody("row-1", "Row One", "2026-05-18T00:00:00Z", "db-1")+`],
		"has_more":false
	}`)
	srv.set("GET /blocks/row-1/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T","person":{"email":"t@example.com"}}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)

	count, err := w.WalkRoot(context.Background(), "R")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	// 3 pages: R, Subpage, Row One.
	if count != 3 {
		t.Errorf("count=%d, want 3 (R + Subpage + Row One)", count)
	}
	got := ing.snapshot()
	titles := map[string]bool{}
	for _, e := range got {
		titles[e.Title] = true
	}
	for _, want := range []string{"Root", "Subpage", "Row One"} {
		if !titles[want] {
			t.Errorf("missing title %q (got %v)", want, titles)
		}
	}
	// Row One should have a populated preface from the property
	// renderer (with Database: Logs).
	for _, e := range got {
		if e.Title == "Row One" && !strings.Contains(e.Body, "Database: Logs") {
			t.Errorf("Row One body missing 'Database: Logs' preface:\n%s", e.Body)
		}
	}
}

func TestWalkRoot_EmptyRootIsNonFatal(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{"object":"list","results":[],"has_more":false}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)

	count, err := w.WalkRoot(context.Background(), "R")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if count != 0 {
		t.Errorf("count=%d, want 0 for an empty root", count)
	}
	if got := ing.snapshot(); len(got) != 0 {
		t.Errorf("ingested %d events, want none for an empty root", len(got))
	}
}

func TestWalkRootReport_IngestAccessFailurePreservesPriorPage(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{"object":"list","results":[],"has_more":false}`)

	w := newWalkerForTest(srv, &fakeIngester{})
	w.UseNativeMarkdown = true
	report, err := w.WalkRootReport(context.Background(), RootRef{Type: RootTypePage, ObjectID: "R"})
	if err != nil {
		t.Fatalf("WalkRootReport: %v", err)
	}
	if len(report.Pages) != 1 || !report.Pages[0].TransientlyInaccessible || report.Pages[0].HasContent {
		t.Fatalf("pages=%+v, want transient snapshot that preserves prior content", report.Pages)
	}
}

func TestWalkRoot_SkipsInaccessibleNestedBlockChildren(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list","results":[{
			"object":"block","id":"PRIVATE-TOGGLE","type":"toggle","has_children":true,
			"toggle":{"rich_text":[{"plain_text":"Private"}]}
		}],"has_more":false
	}`)

	report, err := newWalkerForTest(srv, &fakeIngester{}).WalkRootReport(
		context.Background(), RootRef{Type: RootTypePage, ObjectID: "R"})
	if err != nil {
		t.Fatalf("WalkRootReport: %v", err)
	}
	if len(report.Pages) != 1 || !report.Pages[0].TransientlyInaccessible {
		t.Fatalf("report=%+v, want root retained after inaccessible nested block", report)
	}
}

func TestWalkRoot_RecordsInaccessibleChildPage(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list","results":[{
			"object":"block","id":"PRIVATE-CHILD","type":"child_page",
			"child_page":{"title":"Private"}
		}],"has_more":false
	}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	report, err := newWalkerForTest(srv, &fakeIngester{}).WalkRootReport(
		context.Background(), RootRef{Type: RootTypePage, ObjectID: "R"})
	if err != nil {
		t.Fatalf("WalkRootReport: %v", err)
	}
	for _, page := range report.Pages {
		if page.PageID == "PRIVATE-CHILD" {
			if !page.Inaccessible || page.TransientlyInaccessible || page.LastError == "" {
				t.Fatalf("child snapshot=%+v", page)
			}
			return
		}
	}
	t.Fatalf("report=%+v, want inaccessible child snapshot", report)
}

func TestWalkRoot_RecordsInaccessibleDataSourceRow(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /data_sources/DS", `{
		"object":"data_source","id":"DS","title":[{"plain_text":"Docs"}],
		"parent":{"type":"database_id","database_id":"DB"}
	}`)
	srv.set("POST /data_sources/DS/query", `{
		"object":"list","results":[`+minimalPageBody("PRIVATE-ROW", "Private", "2026-05-18T00:00:00Z", "DB")+`],
		"has_more":false
	}`)

	report, err := newWalkerForTest(srv, &fakeIngester{}).WalkRootReport(
		context.Background(), RootRef{Type: RootTypeDataSource, ObjectID: "DS"})
	if err != nil {
		t.Fatalf("WalkRootReport: %v", err)
	}
	if len(report.Pages) != 1 || report.Pages[0].PageID != "PRIVATE-ROW" || !report.Pages[0].Inaccessible {
		t.Fatalf("report=%+v, want inaccessible row snapshot", report)
	}
}

func TestWalkRoot_MissingRootFails(t *testing.T) {
	srv := newNotionTestServer(t)
	w := newWalkerForTest(srv, &fakeIngester{})
	if _, err := w.WalkRoot(context.Background(), "missing"); err == nil {
		t.Fatal("missing root returned success")
	}
}

func TestWalkRoot_SkipsInaccessibleChildDatabaseButNotRootDatabase(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list","results":[{
			"object":"block","id":"PRIVATE-DB","type":"child_database",
			"child_database":{"title":"Private"}
		}],"has_more":false
	}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)
	w := newWalkerForTest(srv, &fakeIngester{})
	if count, err := w.WalkRoot(context.Background(), "R"); err != nil || count != 1 {
		t.Fatalf("page root count=%d err=%v, want parent success with child skipped", count, err)
	}
	if _, err := w.WalkRootReport(context.Background(), RootRef{Type: RootTypeDatabase, ObjectID: "PRIVATE-DB"}); err == nil {
		t.Fatal("inaccessible configured database root returned success")
	}
}

func TestWalkRoot_DatabaseWithNoAccessibleDataSourcesFails(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /databases/DB", `{
		"object":"database","id":"DB","title":[{"plain_text":"Docs"}],
		"data_sources":[{"id":"PRIVATE-DS","name":"Docs"}],
		"parent":{"type":"workspace","workspace":true}
	}`)
	srv.set("GET /views", `{"object":"list","results":[],"has_more":false}`)

	w := newWalkerForTest(srv, &fakeIngester{})
	if _, err := w.WalkRootReport(context.Background(), RootRef{
		Type: RootTypeDatabase, ObjectID: "DB",
	}); err == nil {
		t.Fatal("database root with no accessible data sources returned success")
	}
}

func TestWalkRoot_DescendsNestedBlockContainers(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list","results":[{
			"object":"block","id":"toggle-1","type":"toggle","has_children":true,
			"toggle":{"rich_text":[{"plain_text":"Nested"}]}
		}],"has_more":false
	}`)
	srv.set("GET /blocks/toggle-1/children", `{
		"object":"list","results":[{
			"object":"block","id":"CHILD","type":"child_page","has_children":false,
			"child_page":{"title":"Child"}
		}],"has_more":false
	}`)
	srv.set("GET /pages/CHILD", minimalPageBody("CHILD", "Child", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/CHILD/children", `{"object":"list","results":[{"object":"block","id":"p","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"body"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	count, err := newWalkerForTest(srv, &fakeIngester{}).WalkRoot(context.Background(), "R")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if count != 2 {
		t.Fatalf("count=%d, want root + nested child", count)
	}
	srv.mu.Lock()
	nestedFetches := 0
	for _, query := range srv.queries {
		if query == "GET /blocks/toggle-1/children" {
			nestedFetches++
		}
	}
	srv.mu.Unlock()
	if nestedFetches != 1 {
		t.Fatalf("nested block fetched %d times, want one shared fetch", nestedFetches)
	}
}

func TestWalkRootRef_ViewQueriesBackingDataSource(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /views/V", `{"object":"view","id":"V","data_source_id":"DS"}`)
	srv.set("GET /data_sources/DS", `{"object":"data_source","id":"DS","title":[{"plain_text":"Features"}],"parent":{"type":"database_id","database_id":"DB"}}`)
	srv.set("POST /data_sources/DS/query", `{"object":"list","results":[`+minimalPageBody("ROW", "Feature", "2026-05-18T00:00:00Z", "DB")+`],"has_more":false}`)
	srv.set("GET /blocks/ROW/children", `{"object":"list","results":[{"object":"block","id":"p","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"body"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	report, err := newWalkerForTest(srv, &fakeIngester{}).WalkRootReport(context.Background(), RootRef{Type: RootTypeView, ObjectID: "V"})
	if err != nil {
		t.Fatalf("WalkRootReport: %v", err)
	}
	if report.Processed != 1 || len(report.Pages) != 1 || report.Pages[0].PageID != "ROW" {
		t.Fatalf("report=%+v", report)
	}
}

func TestWalkRoot_EmptyDescendantDoesNotAbortSiblings(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"EMPTY","type":"child_page","has_children":false,"child_page":{"title":"Empty"}},
			{"object":"block","id":"CONTENT","type":"child_page","has_children":false,"child_page":{"title":"Content"}}
		],
		"has_more":false
	}`)

	srv.set("GET /pages/EMPTY", minimalPageBody("EMPTY", "Empty", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/EMPTY/children", `{"object":"list","results":[],"has_more":false}`)
	srv.set("GET /pages/CONTENT", minimalPageBody("CONTENT", "Content", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/CONTENT/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	w.Concurrency = 1

	count, err := w.WalkRoot(context.Background(), "R")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if count != 2 {
		t.Errorf("count=%d, want 2 (Root + Content; Empty skipped)", count)
	}
	got := ing.snapshot()
	titles := map[string]bool{}
	for _, e := range got {
		titles[e.Title] = true
	}
	if !titles["Root"] || !titles["Content"] {
		t.Errorf("ingested titles=%v, want Root and Content", titles)
	}
	if titles["Empty"] {
		t.Errorf("ingested titles=%v, empty page should be skipped", titles)
	}
}

func TestWalkRoot_VisitedSetBreaksCycles(t *testing.T) {
	srv := newNotionTestServer(t)
	// A → child_page B → child_page A (cycle).
	srv.set("GET /pages/A", minimalPageBody("A", "A", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/A/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"B","type":"child_page","has_children":false,"child_page":{"title":"B"}}
		],"has_more":false
	}`)
	srv.set("GET /pages/B", minimalPageBody("B", "B", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/B/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"A","type":"child_page","has_children":false,"child_page":{"title":"A"}}
		],"has_more":false
	}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	count, err := w.WalkRoot(context.Background(), "A")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if count != 2 {
		t.Errorf("count=%d, want 2 (cycle should be broken)", count)
	}
}

// erroringIngester wraps a fakeIngester but returns a plain
// (non-skip) error for one specific external_id, simulating a
// transient 5xx or other fatal ingest failure mid-walk.
type erroringIngester struct {
	inner    *fakeIngester
	failID   string
	errValue error
}

func (e *erroringIngester) IngestNotion(ctx context.Context, ev IngestEvent) (IngestOutcome, error) {
	if ev.ExternalID == e.failID {
		return "", e.errValue
	}
	return e.inner.IngestNotion(ctx, ev)
}

// TestWalkRoot_WorkerErrorAbortsPromptly is the negative control for
// the WalkRoot/walkPageID deadlock (symptom:
// notion_sync.reconcile_runs_total stuck at 0, last_walked_at NULL
// on every root, "walking root ..." logged once then permanent
// silence). Root cause: workers signal a fatal error via a
// best-effort non-blocking send on errCh and then exit, but
// walkPageID's producer loop never receives from errCh (it's passed
// in but only ctx.Done() is selected on) — once the last worker
// exits, nobody drains `jobs`, and the next `jobs <- ingestJob{...}`
// send blocks forever.
//
// Because a regression deadlocks rather than failing an
// assertion, this test MUST run WalkRoot in a goroutine and race it
// against a short timeout — a bare call would hang the test suite
// forever.
func TestWalkRoot_WorkerErrorAbortsPromptly(t *testing.T) {
	srv := newNotionTestServer(t)
	// Root -> BAD (fatal ingest error, not a 404/403 skip) -> BAD
	// has four of its own child pages (C1..C4). With Concurrency=1
	// (buffer = concurrency*2 = 2), the single worker dequeues BAD,
	// hits the fatal error, and exits. walkPageID then keeps
	// recursing into C1..C4 and keeps sending jobs to the now-
	// undrained channel: the first two sends fill the buffer, and
	// the third blocks forever with nobody left to receive. This
	// is what reproduces the deadlock deterministically rather
	// than racily (a single extra child after BAD isn't enough —
	// the buffered channel can absorb one or two sends before the
	// worker exits, depending on scheduling).
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"BAD","type":"child_page","has_children":false,"child_page":{"title":"Bad"}}
		],"has_more":false
	}`)
	srv.set("GET /pages/BAD", minimalPageBody("BAD", "Bad", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/BAD/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"C1","type":"child_page","has_children":false,"child_page":{"title":"C1"}},
			{"object":"block","id":"C2","type":"child_page","has_children":false,"child_page":{"title":"C2"}},
			{"object":"block","id":"C3","type":"child_page","has_children":false,"child_page":{"title":"C3"}},
			{"object":"block","id":"C4","type":"child_page","has_children":false,"child_page":{"title":"C4"}}
		],"has_more":false
	}`)
	for _, id := range []string{"C1", "C2", "C3", "C4"} {
		srv.set("GET /pages/"+id, minimalPageBody(id, id, "2026-05-18T00:00:00Z", ""))
		srv.set("GET /blocks/"+id+"/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	}
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	wantErr := errors.New("simulated transient 5xx during ingest")
	ing := &erroringIngester{inner: &fakeIngester{}, failID: "BAD", errValue: wantErr}
	w := newWalkerForTest(srv, ing)
	w.Concurrency = 1 // deterministic: the single worker dies on BAD

	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		_, gotErr = w.WalkRoot(context.Background(), "R")
	}()

	select {
	case <-done:
		if !errors.Is(gotErr, wantErr) {
			t.Fatalf("WalkRoot returned err=%v, want wrapping of %v", gotErr, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WalkRoot deadlocked: worker error on BAD never aborted the walk " +
			"(errCh is never drained by walkPageID's producer loop)")
	}
}

func TestWalkRoot_404OnSubpageContinues(t *testing.T) {
	srv := newNotionTestServer(t)
	// Root has child_page X (which 404s) and child_page Y (which works).
	srv.set("GET /pages/R", minimalPageBody("R", "Root", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/R/children", `{
		"object":"list",
		"results":[
			{"object":"block","id":"X","type":"child_page","has_children":false,"child_page":{"title":"Gone"}},
			{"object":"block","id":"Y","type":"child_page","has_children":false,"child_page":{"title":"Live"}}
		],"has_more":false
	}`)
	// X intentionally not registered → 404.
	srv.set("GET /pages/Y", minimalPageBody("Y", "Live", "2026-05-18T00:00:00Z", ""))
	srv.set("GET /blocks/Y/children", `{"object":"list","results":[{"object":"block","id":"para","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"body text"}]}}],"has_more":false}`)
	srv.set("GET /users/u-test", `{"object":"user","id":"u-test","type":"person","name":"T"}`)

	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)
	count, err := w.WalkRoot(context.Background(), "R")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	// R + Y, X skipped silently.
	if count != 2 {
		t.Errorf("count=%d, want 2 (R + Y; X 404)", count)
	}
}

func TestResolveActor(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /users/p1", `{"object":"user","id":"p1","type":"person","name":"Alice","person":{"email":"alice@example.com"}}`)
	srv.set("GET /users/p2", `{"object":"user","id":"p2","type":"person","name":"Bob"}`)
	srv.set("GET /users/b1", `{"object":"user","id":"b1","type":"bot","name":"DiderotBot"}`)
	srv.set("GET /users/p3", `{"object":"user","id":"p3","type":"person"}`)
	cfg := Config{Token: "x", BaseURL: srv.baseURL(), Rate: 100, Burst: 100, HTTPClient: srv.srv.Client()}
	c, _ := NewClient(cfg)
	w := &Walker{Client: c, Dir: NewDirCache(c), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	cases := []struct {
		id        string
		wantActor string
		wantCanon string
	}{
		{"p1", "Alice", "alice@example.com"},
		{"p2", "Bob", "notion:p2"},
		{"b1", "DiderotBot [bot]", "notion-bot:b1"},
		{"p3", "unknown", "notion:p3"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			actor, _, canon := w.resolveActor(context.Background(), &PartialUser{ID: tc.id})
			if actor != tc.wantActor || canon != tc.wantCanon {
				t.Errorf("got (%q, %q); want (%q, %q)",
					actor, canon, tc.wantActor, tc.wantCanon)
			}
		})
	}
}
