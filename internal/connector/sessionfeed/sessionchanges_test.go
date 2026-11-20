package sessionfeed

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestListSessionChanges_BuildsQueryAndParses(t *testing.T) {
	var gotAfter, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/session-changes" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer auth")
		}
		gotAfter = r.URL.Query().Get("after")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changes":[{"change_seq":7,"session_id":"ses_1","owner":"alex@example.com","owner_kind":"human","client":"codex","role":"primary","origin_app":"codex","revision":"sha256:abc","rewrite_epoch":2,"record_count":5,"user_turn_count":2,"content_bytes":100,"source_started_at":"2026-07-24T18:00:00Z","source_updated_at":"2026-07-24T18:41:00Z","updated_at":"2026-07-24T18:42:00Z","complete":true,"deleted":false}],"next_cursor":7}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", nil)
	page, err := c.ListSessionChanges(context.Background(), 6, 50)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfter != "6" {
		t.Errorf("after = %q, want 6", gotAfter)
	}
	if gotLimit != "50" {
		t.Errorf("limit = %q, want 50", gotLimit)
	}
	if len(page.Changes) != 1 {
		t.Fatalf("changes len = %d, want 1", len(page.Changes))
	}
	ch := page.Changes[0]
	if ch.ChangeSeq != 7 || ch.SessionID != "ses_1" || ch.OwnerKind != "human" ||
		ch.Revision != "sha256:abc" || ch.RewriteEpoch != 2 {
		t.Errorf("parsed change = %+v", ch)
	}
	if !ch.Complete || ch.Deleted {
		t.Errorf("complete/deleted parse wrong: %+v", ch)
	}
	if ch.SourceStartedAt.IsZero() || ch.SourceUpdatedAt.IsZero() {
		t.Errorf("source timestamps missing: %+v", ch)
	}
	if page.NextCursor != 7 {
		t.Errorf("next_cursor = %d, want 7", page.NextCursor)
	}
}

func TestListSessionChanges_OmitsZeroAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "" {
			t.Errorf("after should be omitted for fresh cursor, got %q", r.URL.Query().Get("after"))
		}
		_, _ = w.Write([]byte(`{"changes":[],"next_cursor":0}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	if _, err := c.ListSessionChanges(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
}

func TestListSessionChanges_RejectsRegressingCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Requested after=10 but server echoes next_cursor=5: regression.
		_, _ = w.Write([]byte(`{"changes":[{"change_seq":7,"session_id":"s","owner":"o","owner_kind":"human","client":"c","role":"primary"}],"next_cursor":5}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	if _, err := c.ListSessionChanges(context.Background(), 10, 50); err == nil {
		t.Fatal("regressing next_cursor should be rejected")
	}
}

func TestListSessionChanges_RejectsCursorTrailingRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// page serves change_seq 7 but next_cursor claims 5: would
		// re-deliver the row on the next page.
		_, _ = w.Write([]byte(`{"changes":[{"change_seq":7,"session_id":"s","owner":"o","owner_kind":"human","client":"c","role":"primary"}],"next_cursor":5}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	if _, err := c.ListSessionChanges(context.Background(), 0, 50); err == nil {
		t.Fatal("next_cursor trailing served rows should be rejected")
	}
}

func TestGetSessionRevision_FetchesRevisionBySessionID(t *testing.T) {
	var gotSession, gotRevision, gotOwner, gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.URL.Path
		gotRevision = r.URL.Query().Get("revision")
		gotOwner = r.URL.Query().Get("owner")
		gotClient = r.URL.Query().Get("client")
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer auth")
		}
		_, _ = w.Write([]byte(`{"messages":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	body, err := c.GetSessionRevision(context.Background(), "ses_1", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotSession, "/sessions/ses_1/records/redacted") {
		t.Errorf("session path = %q", gotSession)
	}
	if gotRevision != "sha256:abc" {
		t.Errorf("revision = %q", gotRevision)
	}
	if gotOwner != "" || gotClient != "" {
		t.Errorf("identity query parameters should be absent: owner=%q client=%q", gotOwner, gotClient)
	}
	if string(body) != `{"messages":[]}` {
		t.Errorf("body = %q", body)
	}
}

func TestGetSessionRevision_Gunzips(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"messages":[]}`))
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	body, err := c.GetSessionRevision(context.Background(), "ses_1", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"messages":[]}` {
		t.Errorf("gunzipped body = %q", body)
	}
}

func TestGetSessionRecordRange_PassesRevisionAndRangeToFeed(t *testing.T) {
	var gotPath string
	var q url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		q = r.URL.Query()
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer auth")
		}
		_, _ = w.Write([]byte("record body"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	body, err := c.GetSessionRecordRange(context.Background(), "ses_1", "sha256:abc", 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/sessions/ses_1/records/redacted" {
		t.Errorf("path = %q", gotPath)
	}
	if q.Has("owner") || q.Has("client") {
		t.Errorf("identity query parameters should be absent: %v", q)
	}
	if q.Get("revision") != "sha256:abc" {
		t.Errorf("revision = %q", q.Get("revision"))
	}
	if q.Get("start") != "3" || q.Get("end") != "7" {
		t.Errorf("range = [%s,%s), want [3,7)", q.Get("start"), q.Get("end"))
	}
	if string(body) != "record body" {
		t.Errorf("body = %q", body)
	}
}

func TestListSessionChanges_AcceptsCursorGreaterThanServedRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The feed may legally return an explicit next_cursor (10) that is
		// GREATER than the largest served change_seq (3). The connector
		// must ACCEPT and surface it verbatim — clamping it to the max
		// row would re-deliver the leading rows on the next drain.
		_, _ = w.Write([]byte(`{"changes":[{"change_seq":3,"session_id":"s","owner":"o","owner_kind":"human","client":"c","role":"primary"}],"next_cursor":10}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	page, err := c.ListSessionChanges(context.Background(), 0, 50)
	if err != nil {
		t.Fatal("explicit next_cursor greater than served rows must be accepted")
	}
	if page.NextCursor != 10 {
		t.Fatalf("next_cursor = %d, want 10", page.NextCursor)
	}
}

func TestGetSessionRevisionBytes_RejectsDeclaredOverCapBeforeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("short")) // never read: the declared length is rejected first
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	_, err := c.GetSessionRevisionBytes(context.Background(), "ses_1", "sha256:abc", 64)
	if !errors.Is(err, ErrRevisionTooLarge) {
		t.Fatalf("err = %v, want ErrRevisionTooLarge", err)
	}
	if !strings.Contains(err.Error(), "declares 1000000 bytes") {
		t.Fatalf("err = %v, want declared-length rejection", err)
	}
}

func TestGetSessionRevisionBytes_OverCapChunkedBodyIsTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flushing before the body forces chunked encoding, so there is no
		// Content-Length and the cap is enforced while reading.
		w.(http.Flusher).Flush()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	_, err := c.GetSessionRevisionBytes(context.Background(), "ses_1", "sha256:abc", 64)
	if !errors.Is(err, ErrRevisionTooLarge) {
		t.Fatalf("err = %v, want ErrRevisionTooLarge", err)
	}
}

func TestGetSessionRevisionBytes_DecompressedOverCapIsTooLarge(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(bytes.Repeat([]byte("x"), 4096))
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	_, err := c.GetSessionRevisionBytes(context.Background(), "ses_1", "sha256:abc", 1024)
	if !errors.Is(err, ErrRevisionTooLarge) {
		t.Fatalf("err = %v, want ErrRevisionTooLarge", err)
	}
}
