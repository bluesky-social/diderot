package notion

import (
	"context"
	"testing"
	"time"
)

func TestDiscoverDataSourcesDoesNotIngest(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("POST /search", `{
		"object":"list","results":[{
			"object":"data_source","id":"bbbbbbbb-1111-4222-8333-444444444444",
			"last_edited_time":"2026-08-19T12:00:00Z",
			"title":[{"plain_text":"Features"}],
			"parent":{"type":"database_id","database_id":"owner-db"},
			"url":"https://app.notion.com/p/owner-db"
		}],"has_more":false
	}`)
	ing := &fakeIngester{}
	w := newWalkerForTest(srv, ing)

	batch, err := w.DiscoverDataSourcesPage(context.Background(), time.Time{}, "")
	if err != nil {
		t.Fatalf("DiscoverDataSourcesPage: %v", err)
	}
	if !batch.Done || len(batch.Resources) != 1 {
		t.Fatalf("batch=%+v", batch)
	}
	if batch.Resources[0].Title != "Features" {
		t.Fatalf("resource=%+v", batch.Resources[0])
	}
	if got := ing.snapshot(); len(got) != 0 {
		t.Fatalf("discovery ingested %d events; want zero", len(got))
	}
}

func TestDiscoverDataSourcesRejectsBrokenPagination(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("POST /search", `{"object":"list","results":[],"has_more":true,"next_cursor":null}`)
	w := newWalkerForTest(srv, &fakeIngester{})
	if _, err := w.DiscoverDataSourcesPage(context.Background(), time.Time{}, ""); err == nil {
		t.Fatal("expected has_more/next_cursor invariant error")
	}
}
