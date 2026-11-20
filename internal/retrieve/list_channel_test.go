package retrieve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// seedSlackChannel records a channel in the Slack directory, as the
// directory refresh does.
func seedSlackChannel(t *testing.T, pool *pgxpool.Pool, channelID, name string, private bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO slack_channel_info (channel_id, name, is_private)
		VALUES ($1, $2, $3)
		ON CONFLICT (channel_id) DO UPDATE SET name = EXCLUDED.name, is_private = EXCLUDED.is_private
	`, channelID, name, private); err != nil {
		t.Fatalf("seed channel %s: %v", channelID, err)
	}
}

// seedSlackMessage stores one Slack channel message the way ingest does:
// scoped to its channel, with its slack_message_details row. threadTS is ""
// for a standalone message, ts for a thread root, and the root's ts for a
// reply.
func seedSlackMessage(t *testing.T, pool *pgxpool.Pool, channelID, ts, threadTS, body string, occurredAt time.Time) {
	t.Helper()
	threadID := ""
	if threadTS != "" {
		threadID = channelID + ":" + threadTS
	}
	externalID := channelID + ":" + ts
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: externalID, ThreadID: threadID, OccurredAt: occurredAt,
		Actor: "alice", Body: body, Scope: events.ScopeChannel(channelID),
	})
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO slack_message_details (event_id, channel_id, thread_ts, is_edit, occurred_at)
		SELECT id, $2, NULLIF($3, ''), false, occurred_at FROM events WHERE source = 'slack' AND external_id = $1
	`, externalID, channelID, threadTS); err != nil {
		t.Fatalf("seed details %s: %v", externalID, err)
	}
}

func listBodies(res *Result) []string {
	bodies := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		bodies = append(bodies, h.Content)
	}
	return bodies
}

// TestListChannelAcceptsEveryReferenceForm reads one channel's time range by
// name, '#name', capitalized name, ID, channel URL, and message permalink, and
// gets the same messages newest-first, with no other channel's.
func TestListChannelAcceptsEveryReferenceForm(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	seedSlackChannel(t, pool, "C0EXAMPLE110", "incidents", false)
	seedSlackChannel(t, pool, "C0EXAMPLE111", "random", false)
	seedSlackMessage(t, pool, "C0EXAMPLE110", "100.1", "", "before the window", base.Add(-time.Hour))
	seedSlackMessage(t, pool, "C0EXAMPLE110", "200.1", "200.1", "root in window", base.Add(time.Minute))
	seedSlackMessage(t, pool, "C0EXAMPLE110", "300.1", "200.1", "reply in window", base.Add(2*time.Minute))
	seedSlackMessage(t, pool, "C0EXAMPLE110", "400.1", "", "after the window", base.Add(2*time.Hour))
	seedSlackMessage(t, pool, "C0EXAMPLE111", "250.1", "", "another channel", base.Add(time.Minute))

	scopes := []string{events.ScopePublic, events.ScopeInternal,
		events.ScopeChannel("C0EXAMPLE110"), events.ScopeChannel("C0EXAMPLE111")}
	for _, channel := range []string{
		"incidents", "#incidents", " Incidents ", "C0EXAMPLE110",
		"https://example.slack.com/archives/C0EXAMPLE110",
		"https://example.slack.com/archives/C0EXAMPLE110/p1700000000000100?thread_ts=1700000000.000100",
	} {
		t.Run(channel, func(t *testing.T) {
			res, err := r.List(ctx, ListRequest{
				Channels: []string{channel}, AllowedScopes: scopes,
				OccurredAfter: base, OccurredBefore: base.Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := listBodies(res)
			if want := []string{"reply in window", "root in window"}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("bodies = %v, want %v", got, want)
			}
			if reply, root := res.Hits[0], res.Hits[1]; reply.ThreadID == reply.ExternalID || root.ThreadID != root.ExternalID {
				t.Errorf("reply and root must stay distinguishable by thread_id: reply=%+v root=%+v", reply, root)
			}
		})
	}

	// A channel alone is a filter: the newest messages, no time bound needed.
	res, err := r.List(ctx, ListRequest{Channels: []string{"incidents"}, AllowedScopes: scopes, ThreadRootsOnly: true, ThreadedSources: []string{"slack"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := listBodies(res), []string{"after the window", "root in window", "before the window"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("thread roots = %v, want %v", got, want)
	}
}

// TestListChannelIsScopedToCaller pins the channel filter's visibility rule:
// a private channel the caller is not in fails exactly as a channel that does
// not exist, by name, ID, or URL, and never falls back to an unfiltered list.
func TestListChannelIsScopedToCaller(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedSlackChannel(t, pool, "C0EXAMPLE112", "general", false)
	seedSlackChannel(t, pool, "G0EXAMPLE113", "legal", true)
	seedSlackMessage(t, pool, "C0EXAMPLE112", "1.1", "", "public chatter", now)
	seedSlackMessage(t, pool, "G0EXAMPLE113", "1.1", "", "privileged advice", now)

	outsider := []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("C0EXAMPLE112")}
	member := append([]string{events.ScopeChannel("G0EXAMPLE113")}, outsider...)

	var hidden []string
	for _, channel := range []string{"legal", "#legal", "LEGAL", "G0EXAMPLE113", "https://example.slack.com/archives/G0EXAMPLE113"} {
		_, err := r.List(ctx, ListRequest{Channels: []string{channel}, AllowedScopes: outsider})
		if !errors.Is(err, ErrListChannelNotFound) {
			t.Fatalf("outsider listing %q: err = %v, want ErrListChannelNotFound", channel, err)
		}
		hidden = append(hidden, strings.ReplaceAll(err.Error(), channel, "X"))
	}
	for _, channel := range []string{"no-such-channel", "C0EXAMPLE114", "https://example.slack.com/archives/C0EXAMPLE114"} {
		_, err := r.List(ctx, ListRequest{Channels: []string{channel}, AllowedScopes: outsider})
		if !errors.Is(err, ErrListChannelNotFound) {
			t.Fatalf("listing unknown %q: err = %v, want ErrListChannelNotFound", channel, err)
		}
		if got := strings.ReplaceAll(err.Error(), channel, "X"); got != hidden[0] {
			t.Errorf("unknown channel error %q differs from hidden channel error %q", got, hidden[0])
		}
	}

	// One unreadable entry fails the call even beside a readable one, and
	// even with other filters that would match on their own.
	_, err := r.List(ctx, ListRequest{
		Channels: []string{"general", "legal"}, Sources: []string{"slack"}, AllowedScopes: outsider,
	})
	if !errors.Is(err, ErrListChannelNotFound) {
		t.Fatalf("mixed readable and hidden channels: err = %v, want ErrListChannelNotFound", err)
	}

	// No scopes at all reads nothing.
	if _, err := r.List(ctx, ListRequest{Channels: []string{"general"}}); !errors.Is(err, ErrListChannelNotFound) {
		t.Fatalf("scopeless caller: err = %v, want ErrListChannelNotFound", err)
	}

	res, err := r.List(ctx, ListRequest{Channels: []string{"general"}, AllowedScopes: outsider})
	if err != nil {
		t.Fatalf("outsider listing general: %v", err)
	}
	if got := listBodies(res); len(got) != 1 || got[0] != "public chatter" {
		t.Errorf("outsider listing general = %v, want only the public message", got)
	}

	res, err = r.List(ctx, ListRequest{Channels: []string{"legal"}, AllowedScopes: member})
	if err != nil {
		t.Fatalf("member listing legal: %v", err)
	}
	if got := listBodies(res); len(got) != 1 || got[0] != "privileged advice" {
		t.Errorf("member listing legal = %v, want the private message", got)
	}
}

// TestListChannelSharedNameReadsOnlyVisibleChannels covers two channels under
// one name: the caller gets the one they can read and nothing of the other.
func TestListChannelSharedNameReadsOnlyVisibleChannels(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedSlackChannel(t, pool, "C0EXAMPLE115", "plans", false)
	seedSlackChannel(t, pool, "G0EXAMPLE116", "plans", true)
	seedSlackMessage(t, pool, "C0EXAMPLE115", "1.1", "", "open plans", now)
	seedSlackMessage(t, pool, "G0EXAMPLE116", "1.1", "", "secret plans", now)

	res, err := r.List(ctx, ListRequest{
		Channels:      []string{"plans"},
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("C0EXAMPLE115")},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := listBodies(res); len(got) != 1 || got[0] != "open plans" {
		t.Errorf("bodies = %v, want only the readable channel's message", got)
	}
}

// TestListChannelPagesWithCursor walks a channel's range to the end with the
// cursor, across messages sharing one timestamp.
func TestListChannelPagesWithCursor(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	seedSlackChannel(t, pool, "C0EXAMPLE117", "busy", false)
	const total = 7
	for i := range total {
		// Pairs share a timestamp, so the cursor must break ties by id.
		seedSlackMessage(t, pool, "C0EXAMPLE117", fmt.Sprintf("%d.1", i), "", fmt.Sprintf("m%d", i), base.Add(time.Duration(i/2)*time.Minute))
	}
	req := ListRequest{
		Channels: []string{"busy"}, TopK: 3, OccurredAfter: base,
		AllowedScopes: []string{events.ScopePublic, events.ScopeChannel("C0EXAMPLE117")},
	}
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page > total {
			t.Fatal("pagination did not terminate")
		}
		res, err := r.List(ctx, req)
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		for _, h := range res.Hits {
			if seen[h.Content] {
				t.Fatalf("message %q returned twice", h.Content)
			}
			seen[h.Content] = true
		}
		if res.NextCursor == "" {
			break
		}
		req.Cursor = res.NextCursor
	}
	if len(seen) != total {
		t.Errorf("paged through %d messages, want %d", len(seen), total)
	}
}

// TestListChannelSQLReadsTheChannelIndex pins the query shape a channel read
// depends on: range, cursor, and order on the side table's columns.
func TestListChannelSQLReadsTheChannelIndex(t *testing.T) {
	sql, _ := buildListSQL(ListRequest{
		AllowedScopes: []string{"public"}, OccurredAfter: time.Now(), OccurredBefore: time.Now(),
	}, listChannels{slack: []string{"C1"}}, &listCursor{occurredAt: time.Now(), id: 1}, 10)
	for _, want := range []string{
		"d.channel_id = ANY(", "d.occurred_at >= ", "d.occurred_at <= ",
		"(d.occurred_at, d.event_id) < (", "ORDER BY d.occurred_at DESC, d.event_id DESC",
		"e.scope = ANY(",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("channel list SQL lacks %q:\n%s", want, sql)
		}
	}
}
