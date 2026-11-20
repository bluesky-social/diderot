package sessioningest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
)

// TestClientFeedAdaptsChangesAndCapErrors: the client adapter is the only
// place the connector's types appear; it maps them field for field and keeps
// the connector's cap error both matchable as ErrRevisionTooLarge (a
// permanent failure) and worded exactly as before.
func TestClientFeedAdaptsChangesAndCapErrors(t *testing.T) {
	started := time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/session-changes":
			_ = json.NewEncoder(w).Encode(sessionfeed.SessionChangesPage{NextCursor: 7, Changes: []sessionfeed.SessionChange{{
				ChangeSeq: 7, SessionID: "s1", Owner: "a@x", OwnerKind: "human", Client: "codex", Role: "primary",
				OriginApp: "app", Revision: "sha256:r", RewriteEpoch: 2, RecordCount: 3, UserTurnCount: 1,
				ContentBytes: 99, SourceStartedAt: started, SourceUpdatedAt: started.Add(time.Minute),
				UpdatedAt: started.Add(2 * time.Minute), Complete: true, Deleted: true,
			}}})
		default:
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		}
	}))
	defer srv.Close()
	feed := NewClientFeed(sessionfeed.NewClient(srv.URL, "token", srv.Client()))
	page, err := feed.ListSessionChanges(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := SessionChange{
		ChangeSeq: 7, SessionID: "s1", Owner: "a@x", OwnerKind: "human", Client: "codex", Role: "primary",
		OriginApp: "app", Revision: "sha256:r", RewriteEpoch: 2, RecordCount: 3, UserTurnCount: 1,
		ContentBytes: 99, SourceStartedAt: started, SourceUpdatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(2 * time.Minute), Complete: true, Deleted: true,
	}
	if page.NextCursor != 7 || len(page.Changes) != 1 || !reflect.DeepEqual(page.Changes[0], want) {
		t.Fatalf("page = %+v", page)
	}
	direct, directErr := sessionfeed.NewClient(srv.URL, "token", srv.Client()).GetSessionRevisionBytes(context.Background(), "s1", "sha256:r", 16)
	_, err = feed.GetSessionRevisionBytes(context.Background(), "s1", "sha256:r", 16)
	if direct != nil || !errors.Is(err, ErrRevisionTooLarge) || !errors.Is(err, sessionfeed.ErrRevisionTooLarge) {
		t.Fatalf("cap error = %v, want both sentinels", err)
	}
	if err.Error() != directErr.Error() {
		t.Fatalf("cap error message changed: %q, want %q", err, directErr)
	}
}

// TestDrain_TransientFeedErrorDoesNotAdvanceCursor: a feed error in
// the middle of a page drain aborts the whole tick and leaves the
// durable cursor unchanged (retry-safe — the next tick re-fetches).
func TestDrain_TransientFeedErrorDoesNotAdvanceCursor(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{{ChangeSeq: 1, SessionID: "s1"}}},
		},
		err: errors.New("transient"),
	}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: 0, Policy: DefaultPolicy()},
		Store: store, Feed: feed, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err == nil {
		t.Fatal("expected tick to fail on feed error")
	}
	c, _ := store.GetCursor(ctx)
	if c.ChangeSeq != 0 {
		t.Fatalf("cursor advanced to %d on failure, want 0", c.ChangeSeq)
	}
}
