package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func makePage(t *testing.T, props map[string]string, parentDB string) *Page {
	t.Helper()
	rawProps := make(map[string]json.RawMessage, len(props))
	for k, v := range props {
		rawProps[k] = json.RawMessage(v)
	}
	p := &Page{
		Object:     "page",
		ID:         "test-page",
		Properties: rawProps,
	}
	if parentDB != "" {
		p.Parent = Parent{Type: "database_id", DatabaseID: parentDB}
	} else {
		p.Parent = Parent{Type: "page_id", PageID: "parent"}
	}
	return p
}

func TestPropertyPreface_FullCoverage(t *testing.T) {
	page := makePage(t, map[string]string{
		"Name":   `{"type":"title","title":[{"plain_text":"My Row"}]}`,
		"Status": `{"type":"status","status":{"name":"In Review"}}`,
		"Owner": `{"type":"people","people":[
			{"object":"user","id":"u1"},
			{"object":"user","id":"u2"}]}`,
		"Tags":     `{"type":"multi_select","multi_select":[{"name":"rfc"},{"name":"net"}]}`,
		"Created":  `{"type":"date","date":{"start":"2026-04-12T15:00:00.000Z","end":""}}`,
		"Hours":    `{"type":"number","number":3.5}`,
		"Public":   `{"type":"checkbox","checkbox":true}`,
		"Email":    `{"type":"email","email":"hi@example.com"}`,
		"Phone":    `{"type":"phone_number","phone_number":"+1-555-0100"}`,
		"Link":     `{"type":"url","url":"https://example.com/x"}`,
		"Notes":    `{"type":"rich_text","rich_text":[{"plain_text":"hello"}]}`,
		"Severity": `{"type":"select","select":{"name":"P1"}}`,
		"Window":   `{"type":"date","date":{"start":"2026-04-12","end":"2026-04-13"}}`,
	}, "db-uuid")

	// User cache stub (avoid network).
	dir := &DirCache{
		dbTitles: newLRU[string, string](16),
		users:    newLRU[string, *User](16),
	}
	dir.PrewarmDatabase("db-uuid", "Project Tracker")
	dir.PrewarmUser(&User{ID: "u1", Type: "person", Name: "Alice"})
	dir.PrewarmUser(&User{ID: "u2", Type: "person", Name: "Bob"})

	got := PropertyPreface(context.Background(), page, dir)
	if got == "" {
		t.Fatal("empty preface")
	}

	// Check key fragments are present.
	wantFrags := []string{
		"Database: Project Tracker",
		"Status: In Review",
		"Owner: Alice, Bob",
		"Tags: rfc, net",
		"Created: 2026-04-12",
		"Hours: 3.5",
		"Public: true",
		"Email: hi@example.com",
		"Phone: +1-555-0100",
		"Link: https://example.com/x",
		"Notes: hello",
		"Severity: P1",
		"Window: 2026-04-12..2026-04-13",
	}
	for _, want := range wantFrags {
		if !strings.Contains(got, want) {
			t.Errorf("preface missing %q\nfull:\n%s", want, got)
		}
	}

	// "Name" (type=title) MUST be skipped — caller handles title
	// separately and including it would duplicate.
	if strings.Contains(got, "Name: ") {
		t.Errorf("preface includes the title property; should be skipped\n%s", got)
	}
}

func TestPropertyPreface_StableOrdering(t *testing.T) {
	// Same input twice should produce byte-identical output (no
	// random map iteration).
	page := makePage(t, map[string]string{
		"Z":      `{"type":"select","select":{"name":"z-val"}}`,
		"A":      `{"type":"select","select":{"name":"a-val"}}`,
		"M":      `{"type":"select","select":{"name":"m-val"}}`,
		"Status": `{"type":"status","status":{"name":"X"}}`,
	}, "")
	got1 := PropertyPreface(context.Background(), page, nil)
	got2 := PropertyPreface(context.Background(), page, nil)
	if got1 != got2 {
		t.Errorf("preface non-deterministic:\n  %q\n  %q", got1, got2)
	}
	// Sorted alphabetically: A, M, Status, Z.
	want := "A: a-val · M: m-val · Status: X · Z: z-val"
	if got1 != want {
		t.Errorf("got=%q\nwant=%q", got1, want)
	}
}

func TestPropertyPreface_DBTitleMissingOmitsPrefix(t *testing.T) {
	page := makePage(t, map[string]string{
		"Status": `{"type":"status","status":{"name":"Done"}}`,
	}, "db-uuid")
	dir := &DirCache{
		dbTitles: newLRU[string, string](16),
		users:    newLRU[string, *User](16),
	}
	dir.PrewarmDatabase("db-uuid", "") // 403/404 fallback path
	got := PropertyPreface(context.Background(), page, dir)
	if strings.Contains(got, "Database:") {
		t.Errorf("expected no Database: prefix when title is empty; got %q", got)
	}
	if !strings.Contains(got, "Status: Done") {
		t.Errorf("missing Status: %q", got)
	}
}

func TestPropertyPreface_SkipsNoiseTypes(t *testing.T) {
	page := makePage(t, map[string]string{
		"Status":   `{"type":"status","status":{"name":"P1"}}`,
		"Formula":  `{"type":"formula","formula":{"type":"number","number":42}}`,
		"Relation": `{"type":"relation","relation":[]}`,
		"Created":  `{"type":"created_time","created_time":"2026-04-12T15:00:00Z"}`,
		"Author":   `{"type":"created_by","created_by":{"object":"user","id":"u"}}`,
	}, "")
	got := PropertyPreface(context.Background(), page, nil)
	for _, banned := range []string{"Formula:", "Relation:", "Author:"} {
		if strings.Contains(got, banned) {
			t.Errorf("preface should skip %s; got %q", banned, got)
		}
	}
	// created_time is OK to render (it's a real value, not a
	// page-level dup).
	if !strings.Contains(got, "Created: 2026-04-12") {
		t.Errorf("missing Created: %q", got)
	}
}

func TestPropertyPreface_UnsupportedRendersPlaceholder(t *testing.T) {
	page := makePage(t, map[string]string{
		"Weird": `{"type":"some_new_type","some_new_type":{}}`,
	}, "")
	got := PropertyPreface(context.Background(), page, nil)
	if !strings.Contains(got, "Weird: <unsupported: some_new_type>") {
		t.Errorf("missing unsupported placeholder: %q", got)
	}
}

// TestPropertyPreface_NumberAvoidsScientific verifies that large
// numbers do NOT render as 1.234567e+06.
// Embedded prose with scientific notation reads like garbage and
// kills the embedding's lexical match for amounts.
func TestPropertyPreface_NumberAvoidsScientific(t *testing.T) {
	page := makePage(t, map[string]string{
		"Cost":  `{"type":"number","number":1234567}`,
		"Big":   `{"type":"number","number":100000000000.5}`,
		"Small": `{"type":"number","number":0.0001}`,
		"Pct":   `{"type":"number","number":3.5}`,
	}, "")
	got := PropertyPreface(context.Background(), page, nil)
	for _, want := range []string{
		"Cost: 1234567",
		"Big: 100000000000.5",
		"Small: 0.0001",
		"Pct: 3.5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\nfull: %s", want, got)
		}
	}
	if strings.Contains(got, "e+") || strings.Contains(got, "e-") {
		t.Errorf("preface contains scientific notation: %s", got)
	}
}

func TestPageTitle(t *testing.T) {
	page := makePage(t, map[string]string{
		"Name":  `{"type":"title","title":[{"plain_text":"Hello"},{"plain_text":" World"}]}`,
		"Other": `{"type":"select","select":{"name":"x"}}`,
	}, "")
	if got := PageTitle(page); got != "Hello World" {
		t.Errorf("PageTitle=%q, want 'Hello World'", got)
	}
}

func TestPageTitle_Empty(t *testing.T) {
	if got := PageTitle(nil); got != "" {
		t.Errorf("PageTitle(nil)=%q", got)
	}
	page := makePage(t, map[string]string{
		"Foo": `{"type":"select","select":{"name":"x"}}`,
	}, "")
	if got := PageTitle(page); got != "" {
		t.Errorf("PageTitle(no title prop)=%q", got)
	}
}

func TestDirCache_DatabaseTitleCachesEmptyOn404(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := NewDirCache(c)
	for i := 0; i < 5; i++ {
		title, err := dir.DatabaseTitle(context.Background(), "db-1")
		if err != nil {
			t.Fatalf("iteration %d err: %v", i, err)
		}
		if title != "" {
			t.Errorf("iteration %d title=%q, want empty", i, title)
		}
	}
	// 404 should be cached after the first hit.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d, want 1 (404 should cache)", got)
	}
}

func TestDirCache_UserCachesNilOn403(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := NewDirCache(c)
	for i := 0; i < 3; i++ {
		u, err := dir.User(context.Background(), "u-1")
		if err != nil {
			t.Fatalf("iteration %d err: %v", i, err)
		}
		if u != nil {
			t.Errorf("iteration %d user=%+v, want nil", i, u)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d, want 1", got)
	}
}

func TestDirCache_DatabaseTitlePropagatesNon404Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 401 — should propagate, not cache
	}))
	defer srv.Close()
	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := NewDirCache(c)
	_, err = dir.DatabaseTitle(context.Background(), "db-1")
	if err == nil {
		t.Errorf("DatabaseTitle: expected 401 error, got nil")
	}
	// 401 means token's broken; the caller (worker / backfill) should
	// see the error so it can fail loud rather than silently
	// degrade.
}

func TestLRU_Eviction(t *testing.T) {
	l := newLRU[string, int](3)
	l.put("a", 1)
	l.put("b", 2)
	l.put("c", 3)
	// Access "a" so it becomes most-recent.
	if v, ok := l.get("a"); !ok || v != 1 {
		t.Errorf("get a: ok=%v v=%d", ok, v)
	}
	// Add d → should evict "b" (LRU).
	l.put("d", 4)
	if _, ok := l.get("b"); ok {
		t.Errorf("expected b to be evicted")
	}
	if v, ok := l.get("a"); !ok || v != 1 {
		t.Errorf("a should still be present: ok=%v v=%d", ok, v)
	}
}

func TestLRU_PutOverwrites(t *testing.T) {
	l := newLRU[string, int](3)
	l.put("a", 1)
	l.put("a", 2)
	if v, ok := l.get("a"); !ok || v != 2 {
		t.Errorf("expected overwrite to v=2; got ok=%v v=%d", ok, v)
	}
}

func TestDirCache_Goroutine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"database","id":"db-1","title":[{"plain_text":"Books"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := NewDirCache(c)
	// 10 concurrent lookups for the same key should all see "Books".
	done := make(chan string, 10)
	for i := 0; i < 10; i++ {
		go func() {
			t, _ := dir.DatabaseTitle(context.Background(), "db-1")
			done <- t
		}()
	}
	for i := 0; i < 10; i++ {
		got := <-done
		if got != "Books" {
			t.Errorf("got %q, want Books", got)
		}
	}
}
