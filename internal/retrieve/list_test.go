package retrieve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// listSetupPool truncates events and returns a clean pool. List has
// no qdrant dependency; tests don't need to spin up a collection.
func listSetupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// seedEventOnly upserts an event without driving the embedder. Use
// for List tests where we only care about the events table.
// Defaults Scope to public so fixtures predating the scope column
// don't all need the field; scope-filtering tests set it explicitly.
func seedEventOnly(t *testing.T, pool *pgxpool.Pool, ev events.Event) {
	t.Helper()
	if ev.Scope == "" {
		ev.Scope = events.ScopePublic
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	if _, err := sink.Upsert(context.Background(), ev); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
}

// newListRetriever builds a Retriever wired only against pool —
// embedder + qdrantstore left nil since List doesn't touch them.
// retrieve_test.go's setupWorld handles the full Retrieve path; this
// keeps the List-specific tests fast.
func newListRetriever(pool *pgxpool.Pool) *Retriever {
	return NewRetriever(pool, nil, nil, slog.Default())
}

func TestListNoFilterErrors(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	_, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}})
	if !errors.Is(err, ErrListNoFilter) {
		t.Errorf("err = %v, want ErrListNoFilter", err)
	}
}

func TestListOrdersByOccurredAtDesc(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Insert in non-temporal order; expect desc-by-occurred_at out.
	for i, when := range []time.Time{
		base.Add(2 * time.Hour),
		base,
		base.Add(1 * time.Hour),
	} {
		seedEventOnly(t, pool, events.Event{
			Source:     "slack",
			ExternalID: "C1:" + when.Format(time.RFC3339Nano),
			OccurredAt: when,
			Actor:      "alice",
			Body:       "msg" + string(rune('a'+i)),
		})
	}
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(res.Hits))
	}
	for i := 0; i < len(res.Hits)-1; i++ {
		if res.Hits[i].OccurredAt.Before(res.Hits[i+1].OccurredAt) {
			t.Errorf("hits not desc-ordered at %d: %v then %v",
				i, res.Hits[i].OccurredAt, res.Hits[i+1].OccurredAt)
		}
	}
}

func TestListFiltersBySource(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:1", OccurredAt: now,
		Actor: "a", Body: "slack body",
	})
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "S1:session", OccurredAt: now,
		Actor: "a@example.com", ActorCanonical: "a@example.com", Body: "claude body",
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Source != "slack" {
		t.Errorf("expected one slack hit, got %+v", res.Hits)
	}
}

func TestListFiltersByActorCanonicalOrDisplay(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	// Slack-style: display name actor, no canonical.
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:1", OccurredAt: now,
		Actor: "Alex", Body: "slack",
	})
	// Session-style: canonical = email, actor = email.
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "S1:session", OccurredAt: now,
		Actor: "alex@example.com", ActorCanonical: "alex@example.com", Body: "claude",
	})
	// Email matches the canonical column on the session row.
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Actors: []string{"alex@example.com"}})
	if err != nil {
		t.Fatalf("email filter: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Source != "agent_session" {
		t.Errorf("email-by-canonical: %+v", res.Hits)
	}
	// Display name matches the actor column on the slack row.
	res, err = r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Actors: []string{"Alex"}})
	if err != nil {
		t.Fatalf("display filter: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Source != "slack" {
		t.Errorf("display-by-actor: %+v", res.Hits)
	}
}

func TestListFiltersByTimeRange(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		when := base.Add(time.Duration(i) * 24 * time.Hour)
		seedEventOnly(t, pool, events.Event{
			Source: "slack", ExternalID: "C1:" + when.Format(time.RFC3339Nano),
			OccurredAt: when, Actor: "a", Body: "x",
		})
	}
	// Days 1-3 inclusive: after >= day1, before <= day3.
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		OccurredAfter:  base.Add(24 * time.Hour),
		OccurredBefore: base.Add(3 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 3 {
		t.Errorf("expected 3 hits in [day1, day3], got %d: %+v", len(res.Hits), res.Hits)
	}
}

func TestListThreadRootsOnly(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	// Standalone (thread_id empty).
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:1", OccurredAt: now,
		Actor: "a", Body: "standalone",
	})
	// Thread root: thread_id == external_id.
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:2", OccurredAt: now.Add(-time.Minute),
		ThreadID: "C1:2", Actor: "b", Body: "root",
	})
	// Reply: thread_id != external_id.
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:3", OccurredAt: now.Add(-2 * time.Minute),
		ThreadID: "C1:2", Actor: "c", Body: "REPLY_DROP",
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{"slack"}, ThreadRootsOnly: true, ThreadedSources: []string{"slack"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Errorf("expected 2 (standalone + root), got %d: %+v", len(res.Hits), res.Hits)
	}
	for _, h := range res.Hits {
		if strings.Contains(h.Content, "REPLY_DROP") {
			t.Errorf("reply leaked through thread_roots_only filter: %+v", h)
		}
	}
}

func TestListSessionLevelOnly(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "abc:session", OccurredAt: now,
		Actor: "a@example.com", ActorCanonical: "a@example.com", Body: "session-level",
	})
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "abc:0:50", OccurredAt: now.Add(-time.Minute),
		Actor: "a@example.com", ActorCanonical: "a@example.com", Body: "CHUNK_DROP",
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{"agent_session"}, SessionLevelOnly: true,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("expected 1 session-level hit, got %d: %+v", len(res.Hits), res.Hits)
	}
	if !strings.HasSuffix(res.Hits[0].ExternalID, ":session") {
		t.Errorf("got non-session-level hit: %+v", res.Hits[0])
	}
}

func TestListClampsTopK(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		seedEventOnly(t, pool, events.Event{
			Source: "slack", ExternalID: "C1:" + time.Duration(i).String() + "z",
			OccurredAt: now.Add(-time.Duration(i) * time.Minute),
			Actor:      "a", Body: "x",
		})
	}
	// TopK over the cap: should clamp to ListMaxTopK.
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{"slack"}, TopK: 1000,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != ListMaxTopK {
		t.Errorf("expected %d hits (clamped), got %d", ListMaxTopK, len(res.Hits))
	}
}

// TestListSurfacesSlackChannelName confirms the LEFT JOIN to
// slack_channel_info via slack_message_details lifts the channel
// name onto the Hit. Critical for grounding the model's prose:
// without this it knows only the channel ID from the permalink and
// is prone to hallucinating friendly-but-wrong names.
func TestListSurfacesSlackChannelName(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	ctx := context.Background()

	// Seed slack_channel_info entry first so the JOIN finds it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
		VALUES ('C123', 'eng-platform', false, false, now())
	`); err != nil {
		t.Fatalf("seed channel_info: %v", err)
	}

	// Seed event + slack_message_details (the JOIN bridge).
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C123:1700000000.000",
		OccurredAt: now, Actor: "Alex", Body: "msg",
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, thread_ts, is_edit, occurred_at)
		VALUES ((SELECT id FROM events WHERE external_id = 'C123:1700000000.000'),
		        'C123', NULL, false,
		        (SELECT occurred_at FROM events WHERE external_id = 'C123:1700000000.000'))
	`); err != nil {
		t.Fatalf("seed details: %v", err)
	}

	// Also seed a non-slack event to confirm the JOIN doesn't
	// poison the channel_name for it.
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "abc:session",
		OccurredAt: now.Add(-time.Minute), Actor: "a@example.com",
		ActorCanonical: "a@example.com", Body: "claude session",
	})

	res, err := r.List(ctx, ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{"slack", "agent_session"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(res.Hits))
	}
	for _, h := range res.Hits {
		switch h.Source {
		case "slack":
			if h.ChannelName != "eng-platform" {
				t.Errorf("slack hit channel = %q, want eng-platform", h.ChannelName)
			}
		case "agent_session":
			if h.ChannelName != "" {
				t.Errorf("agent_session hit channel should be empty, got %q", h.ChannelName)
			}
		}
	}
}

// TestListSlackChannelMissingDirectoryRow exercises the "channel
// not yet directory-cached" path: the JOIN doesn't find a matching
// slack_channel_info row, and the COALESCE projects ”.
func TestListSlackChannelMissingDirectoryRow(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	ctx := context.Background()

	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C999:1700000000.000",
		OccurredAt: now, Actor: "Alex", Body: "msg",
	})
	// slack_message_details row references a channel_id with NO
	// matching slack_channel_info row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, thread_ts, is_edit, occurred_at)
		VALUES ((SELECT id FROM events WHERE external_id = 'C999:1700000000.000'),
		        'C999', NULL, false,
		        (SELECT occurred_at FROM events WHERE external_id = 'C999:1700000000.000'))
	`); err != nil {
		t.Fatalf("seed details: %v", err)
	}

	res, err := r.List(ctx, ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(res.Hits))
	}
	if res.Hits[0].ChannelName != "" {
		t.Errorf("channel_name should be '' when directory row missing, got %q",
			res.Hits[0].ChannelName)
	}
}

func TestListEmptyResult(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("expected zero hits, got %d", len(res.Hits))
	}
}

func TestListTruncatesContent(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	// Body well over the per-row cap.
	huge := strings.Repeat("a", 10000)
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:huge", OccurredAt: now,
		Actor: "a", Body: huge,
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	got := res.Hits[0].Content
	if len(got) >= len(huge) {
		t.Errorf("content not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "…(truncated)") {
		t.Errorf("expected truncation marker, got %q", got[max(0, len(got)-60):])
	}
}

func TestListTruncatesRuneAware(t *testing.T) {
	// Truncation on multi-byte UTF-8 must not split a rune. Build a
	// string of 4-byte runes (🦄) longer than the budget; truncate
	// must produce valid UTF-8.
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	now := time.Now().UTC()
	body := strings.Repeat("🦄", 3000) // 12000 bytes, 3000 runes — over the 2048-rune cap
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:emoji", OccurredAt: now,
		Actor: "a", Body: body,
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := res.Hits[0].Content
	// Stripping the marker must leave valid UTF-8 unicorns only.
	idx := strings.Index(got, "…(truncated)")
	if idx <= 0 {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	prefix := got[:idx]
	for _, r := range prefix {
		if r != '🦄' && r != '\n' && r != '…' {
			t.Errorf("unexpected rune %U after truncation", r)
			break
		}
	}
}

// TestListSQLActorPathSplit confirms buildListSQL picks the
// canonical-only branch (composite-index friendly) when all actors
// are email-shaped, and the OR-with-display branch otherwise.
// Pure unit test — checks the generated SQL string, no DB.
func TestListSQLActorPathSplit(t *testing.T) {
	cases := []struct {
		name          string
		actors        []string
		wantCanonOnly bool
		wantOR        bool
	}{
		{"all emails", []string{"a@example.com", "c@example.org"}, true, false},
		{"display name only", []string{"Alex"}, false, true},
		{"mixed", []string{"a@example.com", "Alex"}, false, true},
		{"svc identity", []string{"svc:claude-action"}, false, true},
		{"@ but no dot", []string{"a@b"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _ := buildListSQL(ListRequest{AllowedScopes: []string{"public", "internal"},
				Actors: c.actors,
				TopK:   10,
			}, listChannels{}, nil, 10)
			// Hot path: actor_canonical = ANY without the OR-with-actor branch.
			// Slow path: includes "OR e.actor = ANY". Both paths now also
			// include `actor_canonical <> ''` for partial-index usage.
			canonOnly := strings.Contains(sql, "e.actor_canonical = ANY") &&
				!strings.Contains(sql, "OR e.actor = ANY")
			or := strings.Contains(sql, "OR e.actor = ANY")
			if !strings.Contains(sql, "actor_canonical <> ''") {
				t.Errorf("expected partial-index predicate `actor_canonical <> ''` in sql:\n%s", sql)
			}
			if canonOnly != c.wantCanonOnly {
				t.Errorf("canon-only branch: got %v, want %v\nsql=%s", canonOnly, c.wantCanonOnly, sql)
			}
			if or != c.wantOR {
				t.Errorf("OR branch: got %v, want %v\nsql=%s", or, c.wantOR, sql)
			}
		})
	}
}

// TestListPaginatesThroughTimestampCollision paginates via cursor
// across a corpus where more events share one timestamp than fit in
// a page. A loop that pages by occurred_before alone returns the same
// page forever once one timestamp holds more events than a page.
// Keyset cursors advance by (occurred_at, id) row comparison, so ties
// can't stall.
func TestListPaginatesThroughTimestampCollision(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)

	// 30 events sharing one timestamp + 5 older stragglers, page
	// size 25: the collided group alone overflows a page.
	collided := time.Date(2026, 5, 19, 19, 6, 30, 0, time.UTC)
	const nCollided, nOlder = 30, 5
	for i := range nCollided {
		seedEventOnly(t, pool, events.Event{
			Source:     "agent_session",
			ExternalID: fmt.Sprintf("sess-%02d:0:50", i),
			OccurredAt: collided,
			Actor:      "a@example.com", ActorCanonical: "a@example.com",
			Body: "collided",
		})
	}
	for i := range nOlder {
		seedEventOnly(t, pool, events.Event{
			Source:     "agent_session",
			ExternalID: fmt.Sprintf("older-%02d:0:50", i),
			OccurredAt: collided.Add(-time.Duration(i+1) * time.Hour),
			Actor:      "a@example.com", ActorCanonical: "a@example.com",
			Body: "older",
		})
	}

	seen := make(map[int64]bool)
	cursor := ""
	pages := 0
	// A correct pager needs ceil(35/25) = 2 pages; the loop bound
	// turns "stalls forever" into a failure instead of a hang.
	for page := range nCollided + nOlder {
		res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
			Sources: []string{"agent_session"}, TopK: 25, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		pages++
		for _, h := range res.Hits {
			if seen[h.EventID] {
				t.Errorf("page %d: event %d returned twice", page, h.EventID)
			}
			seen[h.EventID] = true
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != nCollided+nOlder {
		t.Errorf("pagination reached %d/%d events; collided-timestamp group is unreachable",
			len(seen), nCollided+nOlder)
	}
	if pages != 2 {
		t.Errorf("expected exactly 2 pages for 35 events at top_k=25, got %d", pages)
	}
}

// TestListCursorRoundTrip covers the token itself: opaque, URL-safe,
// microsecond-precision round-trip, and malformed input surfacing
// ErrListBadCursor rather than a 500-shaped error.
func TestListCursorRoundTrip(t *testing.T) {
	orig := listCursor{
		occurredAt: time.Date(2026, 5, 19, 19, 6, 30, 123456000, time.UTC),
		id:         987654,
	}
	tok := encodeListCursor(orig)
	got, err := decodeListCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.occurredAt.Equal(orig.occurredAt) || got.id != orig.id {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, orig)
	}
	for _, bad := range []string{"not-base64!", "aGVsbG8", ""} {
		if _, err := decodeListCursor(bad); !errors.Is(err, ErrListBadCursor) {
			t.Errorf("decode(%q): err = %v, want ErrListBadCursor", bad, err)
		}
	}
}

// TestListBadCursorSurfaces confirms List rejects a garbage cursor
// with ErrListBadCursor (→ 400 at the HTTP layer, recoverable
// tool-error for the model) instead of querying with a zero cursor.
func TestListBadCursorSurfaces(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	_, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{"slack"}, Cursor: "garbage!!",
	})
	if !errors.Is(err, ErrListBadCursor) {
		t.Errorf("err = %v, want ErrListBadCursor", err)
	}
}

// TestListTrimsFilterWhitespace: model-emitted tool args sometimes
// carry a stray leading space (" a@example.com"); before trimFilterValues
// that silently matched nothing through `= ANY($n)`.
func TestListTrimsFilterWhitespace(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	seedEventOnly(t, pool, events.Event{
		Source: "agent_session", ExternalID: "s1:session",
		OccurredAt: time.Now().UTC(),
		Actor:      "a@example.com", ActorCanonical: "a@example.com", Body: "x",
	})
	res, err := r.List(context.Background(), ListRequest{AllowedScopes: []string{"public", "internal"},
		Sources: []string{" agent_session"},
		Actors:  []string{" a@example.com "},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Errorf("whitespace-padded filters matched %d hits, want 1", len(res.Hits))
	}
}

func TestListRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     ListRequest
		wantErr bool
	}{
		{"empty", ListRequest{AllowedScopes: []string{"public", "internal"}}, true},
		{"sources only", ListRequest{AllowedScopes: []string{"public", "internal"}, Sources: []string{"slack"}}, false},
		{"actors only", ListRequest{AllowedScopes: []string{"public", "internal"}, Actors: []string{"a"}}, false},
		{"after only", ListRequest{AllowedScopes: []string{"public", "internal"}, OccurredAfter: time.Now()}, false},
		{"before only", ListRequest{AllowedScopes: []string{"public", "internal"}, OccurredBefore: time.Now()}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestTruncateRunesBoundary(t *testing.T) {
	// Pure unit test of the rune-aware truncate helper. No DB.
	if got := truncateRunes("hi", 10); got != "hi" {
		t.Errorf("under budget should pass through: %q", got)
	}
	if got := truncateRunes("", 10); got != "" {
		t.Errorf("empty in, empty out: %q", got)
	}
	if got := truncateRunes("hi there", 0); got != "" {
		t.Errorf("zero budget => empty: %q", got)
	}
	// 10 runes, budget 5 => should return 5 runes + marker.
	got := truncateRunes("aaaaabbbbb", 5)
	if !strings.HasPrefix(got, "aaaaa") {
		t.Errorf("expected first-5-rune prefix: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected marker: %q", got)
	}
}
