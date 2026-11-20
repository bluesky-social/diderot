package notion

import (
	"context"
	"testing"
)

func TestCatalogResolver_LinkedViewResolvesBackingDataSource(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /databases/container", `{
		"object":"database","id":"container","in_trash":false,
		"title":[{"plain_text":"Linked"}],"data_sources":[]
	}`)
	srv.set("GET /views", `{
		"object":"list","type":"view",
		"results":[{"object":"view","id":"view-1"}],"has_more":false
	}`)
	srv.set("GET /views/view-1", `{
		"object":"view","id":"view-1","type":"table","name":"Roadmap",
		"parent":{"type":"database_id","database_id":"container"},
		"data_source_id":"source-1"
	}`)
	srv.set("GET /data_sources/source-1", `{
		"object":"data_source","id":"source-1","in_trash":false,
		"title":[{"plain_text":"Features"}],
		"parent":{"type":"database_id","database_id":"owner-db"}
	}`)

	w := newWalkerForTest(srv, &fakeIngester{})
	got, err := w.Catalog.ResolveDatabase(context.Background(), "container")
	if err != nil {
		t.Fatalf("ResolveDatabase: %v", err)
	}
	if len(got) != 1 || got[0].ID != "source-1" {
		t.Fatalf("resolved=%+v, want source-1", got)
	}
}

func TestCatalogResolver_DeduplicatesDirectAndViewedDataSources(t *testing.T) {
	srv := newNotionTestServer(t)
	srv.set("GET /databases/container", `{
		"object":"database","id":"container","in_trash":false,
		"data_sources":[{"id":"source-1","name":"Features"}]
	}`)
	srv.set("GET /views", `{
		"object":"list","results":[{"object":"view","id":"view-1"}],"has_more":false
	}`)
	srv.set("GET /views/view-1", `{
		"object":"view","id":"view-1","data_source_id":"source-1"
	}`)
	srv.set("GET /data_sources/source-1", `{
		"object":"data_source","id":"source-1","in_trash":false
	}`)

	w := newWalkerForTest(srv, &fakeIngester{})
	got, err := w.Catalog.ResolveDatabase(context.Background(), "container")
	if err != nil {
		t.Fatalf("ResolveDatabase: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(resolved)=%d, want 1", len(got))
	}
}
