package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSearch_PaginationFields verifies the search wrapper round-
// trips the pagination cursor and that the response shape decodes
// the way the walker will consume it (Results as []json.RawMessage
// so we can inspect Object before deserializing into Page or
// Database).
func TestSearch_PaginationFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"object": "list",
			"results": [
				{"object": "page", "id": "abcd", "url": "https://notion.so/abcd"}
			],
			"has_more": true,
			"next_cursor": "cursor-X"
		}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	out, err := c.Search(context.Background(), SearchParams{
		FilterObject:  "page",
		SortDirection: "descending",
		StartCursor:   "prev-cursor",
		PageSize:      50,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Verify request body was shaped correctly.
	if got := gotBody["start_cursor"]; got != "prev-cursor" {
		t.Errorf("start_cursor=%v, want 'prev-cursor'", got)
	}
	if got := gotBody["page_size"]; got != float64(50) {
		t.Errorf("page_size=%v, want 50", got)
	}
	filter, ok := gotBody["filter"].(map[string]any)
	if !ok || filter["value"] != "page" {
		t.Errorf("filter=%v, want {property:object, value:page}", gotBody["filter"])
	}
	sort, ok := gotBody["sort"].(map[string]any)
	if !ok || sort["direction"] != "descending" || sort["timestamp"] != "last_edited_time" {
		t.Errorf("sort=%v, want {direction:descending, timestamp:last_edited_time}", gotBody["sort"])
	}

	// Verify response decoded.
	if !out.HasMore || out.NextCursor != "cursor-X" {
		t.Errorf("has_more=%v cursor=%q, want true 'cursor-X'", out.HasMore, out.NextCursor)
	}
	if len(out.Results) != 1 {
		t.Fatalf("len(Results)=%d, want 1", len(out.Results))
	}
}

// TestGetPage_ParsesParentAndProps verifies the basic shape of
// the page response decodes the bits the walker / property
// renderer rely on.
func TestGetPage_ParsesParentAndProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Notion API uses undashed UUIDs in path.
		if !strings.Contains(r.URL.Path, "abcd1234ef567890abcd1234567890ab") {
			t.Errorf("URL path=%q, expected undashed uuid", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"object": "page",
			"id": "abcd1234-ef56-7890-abcd-1234567890ab",
			"created_time": "2026-01-01T00:00:00.000Z",
			"last_edited_time": "2026-05-15T12:30:00.000Z",
			"last_edited_by": {"object": "user", "id": "user-1"},
			"archived": false,
			"properties": {
				"Name": {"id": "title", "type": "title", "title": [{"plain_text": "Hello"}]}
			},
			"parent": {"type": "database_id", "database_id": "db-1"},
			"url": "https://www.notion.so/Hello-abcd"
		}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	page, err := c.GetPage(context.Background(),
		"abcd1234-ef56-7890-abcd-1234567890ab")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if page.Parent.Type != "database_id" || page.Parent.DatabaseID != "db-1" {
		t.Errorf("parent=%+v, want {type:database_id, database_id:db-1}", page.Parent)
	}
	if page.LastEditedBy == nil || page.LastEditedBy.ID != "user-1" {
		t.Errorf("last_edited_by=%+v, want id=user-1", page.LastEditedBy)
	}
	if got, ok := page.Properties["Name"]; !ok || len(got) == 0 {
		t.Errorf("Properties[Name] missing or empty: %+v", page.Properties)
	}
	if page.URL != "https://www.notion.so/Hello-abcd" {
		t.Errorf("URL=%q, mismatch", page.URL)
	}
}

// TestGetUser_PersonAndBot verifies the User shape distinguishes
// person (with email) from bot (no email, no person sub-object).
func TestGetUser_PersonAndBot(t *testing.T) {
	cases := map[string]struct {
		body      string
		wantBot   bool
		wantEmail string
	}{
		"person with email": {
			body: `{"object": "user", "id": "u1", "type": "person",
				"name": "Alice",
				"person": {"email": "alice@example.com"}}`,
			wantBot:   false,
			wantEmail: "alice@example.com",
		},
		"bot has no email": {
			body: `{"object": "user", "id": "b1", "type": "bot",
				"name": "MyBot",
				"bot": {"owner": {"type": "workspace"}}}`,
			wantBot:   true,
			wantEmail: "",
		},
		"person with email withheld": {
			body: `{"object": "user", "id": "u2", "type": "person",
				"name": "Bob"}`,
			wantBot:   false,
			wantEmail: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, err := NewClient(fastClientCfg(srv))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			u, err := c.GetUser(context.Background(), "any-id")
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if u.IsBot() != tc.wantBot {
				t.Errorf("IsBot()=%v, want %v", u.IsBot(), tc.wantBot)
			}
			if u.Email() != tc.wantEmail {
				t.Errorf("Email()=%q, want %q", u.Email(), tc.wantEmail)
			}
		})
	}
}

func TestGetSelf(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"object":"user","id":"integration-123","type":"bot","name":"Diderot"}`))
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	self, err := c.GetSelf(context.Background())
	if err != nil {
		t.Fatalf("GetSelf: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/users/me" {
		t.Fatalf("request=%s %s, want GET /users/me", gotMethod, gotPath)
	}
	if self.ID != "integration-123" || !self.IsBot() {
		t.Fatalf("self=%+v, want integration bot", self)
	}
}

// TestGetBlockChildren_PaginationLoop verifies ListAllBlockChildren
// follows next_cursor through to completion.
func TestGetBlockChildren_PaginationLoop(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		w.WriteHeader(http.StatusOK)
		switch page {
		case 1:
			// First page: 2 blocks, has_more.
			_, _ = w.Write([]byte(`{
				"object": "list",
				"results": [
					{"object": "block", "id": "b1", "type": "paragraph", "has_children": false, "paragraph": {"rich_text": []}},
					{"object": "block", "id": "b2", "type": "paragraph", "has_children": false, "paragraph": {"rich_text": []}}
				],
				"has_more": true,
				"next_cursor": "page-2"
			}`))
		case 2:
			if got := r.URL.Query().Get("start_cursor"); got != "page-2" {
				t.Errorf("page 2 start_cursor=%q, want 'page-2'", got)
			}
			// Second page: 1 block, no more.
			_, _ = w.Write([]byte(`{
				"object": "list",
				"results": [
					{"object": "block", "id": "b3", "type": "heading_1", "has_children": false, "heading_1": {"rich_text": []}}
				],
				"has_more": false,
				"next_cursor": null
			}`))
		default:
			t.Fatalf("unexpected page %d", page)
		}
	}))
	defer srv.Close()

	c, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	all, err := c.ListAllBlockChildren(context.Background(), "root-block")
	if err != nil {
		t.Fatalf("ListAllBlockChildren: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len(blocks)=%d, want 3", len(all))
	}
	if all[0].Type != "paragraph" || all[2].Type != "heading_1" {
		t.Errorf("block types %s/%s, want paragraph/heading_1",
			all[0].Type, all[2].Type)
	}
	// Verify Block.UnmarshalJSON populated the catch-all Payload
	// with the per-type body.
	if _, ok := all[0].Payload["paragraph"]; !ok {
		t.Errorf("Payload[paragraph] missing on block 0: %+v", all[0].Payload)
	}
	if _, ok := all[2].Payload["heading_1"]; !ok {
		t.Errorf("Payload[heading_1] missing on block 2: %+v", all[2].Payload)
	}
}

// TestBlock_PayloadStripsKnown verifies that the catch-all Payload
// excludes the known-top-level fields (object, id, type, etc.) so
// the renderer doesn't accidentally try to render them as a block
// body.
func TestBlock_PayloadStripsKnown(t *testing.T) {
	src := `{
		"object": "block",
		"id": "b1",
		"type": "paragraph",
		"has_children": false,
		"archived": false,
		"created_time": "2026-01-01T00:00:00Z",
		"last_edited_time": "2026-01-01T00:00:00Z",
		"created_by": {"object": "user", "id": "u1"},
		"last_edited_by": {"object": "user", "id": "u1"},
		"parent": {"type": "page_id", "page_id": "p1"},
		"in_trash": false,
		"paragraph": {"rich_text": [{"plain_text": "hi"}]}
	}`
	var b Block
	if err := json.Unmarshal([]byte(src), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"object", "id", "type", "has_children",
		"archived", "created_time", "last_edited_time",
		"created_by", "last_edited_by", "parent", "in_trash"} {
		if _, present := b.Payload[k]; present {
			t.Errorf("Payload[%q] should be stripped", k)
		}
	}
	if _, ok := b.Payload["paragraph"]; !ok {
		t.Errorf("Payload[paragraph] missing")
	}
}
