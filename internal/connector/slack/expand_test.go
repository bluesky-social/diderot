package slack

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

// getExpandPool returns a pool with the events + slack tables
// truncated. Mirrors getSyncPool but excludes the sync-only tables
// since these tests don't drive the worker.
func getExpandPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_channel_info RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// insertSlackMessage seeds an events row + slack_message_details row
// representing one slack message. Returns the new event_id.
//
// occurredAt is the message's wall-clock time. threadTS encodes the
// message's thread membership: "" for standalones, the root's ts for
// replies, and the message's own ts for thread roots that have at
// least one reply.
//
// channelID is the slack channel id; channelName is set on
// slack_channel_info via an upsert so the renderer can pick it up.
func insertSlackMessage(t *testing.T, pool *pgxpool.Pool, channelID, channelName, ts, threadTS, actor, body string, occurredAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	if channelName != "" {
		_, err := pool.Exec(ctx, `
			INSERT INTO slack_channel_info (channel_id, name)
			VALUES ($1, $2)
			ON CONFLICT (channel_id) DO UPDATE SET name = EXCLUDED.name
		`, channelID, channelName)
		if err != nil {
			t.Fatalf("upsert channel_info: %v", err)
		}
	}
	extID := channelID + ":" + ts
	threadID := ""
	if threadTS != "" {
		threadID = channelID + ":" + threadTS
	}
	var eventID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO events
		    (source, external_id, occurred_at, actor, actor_id, thread_id,
		     title, body, url, raw, deleted, scope)
		VALUES
		    ('slack', $1, $2, $3, '', $4, '', $5, '', '{}'::jsonb, false, 'public')
		RETURNING id
	`, extID, occurredAt.UTC(), actor, threadID, body).Scan(&eventID)
	if err != nil {
		t.Fatalf("insert event %s: %v", extID, err)
	}
	var threadTSPtr *string
	if threadTS != "" {
		threadTSPtr = &threadTS
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, thread_ts, is_edit, occurred_at)
		VALUES ($1, $2, $3, false, $4)
	`, eventID, channelID, threadTSPtr, occurredAt.UTC())
	if err != nil {
		t.Fatalf("insert details: %v", err)
	}
	return eventID
}

// at builds a deterministic UTC time for tests. minute is the minute
// offset from a fixed base; keeps row ordering predictable without
// littering tests with full RFC3339 strings.
func at(minute int) time.Time {
	base := time.Date(2026, 5, 17, 14, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(minute) * time.Minute)
}

// tsFor builds a slack-shaped ts string for a given test minute,
// just so external_id values look real.
func tsFor(minute int) string {
	t := at(minute)
	return fmt.Sprintf("%d.000000", t.Unix())
}

func TestSlackExpandFiltersEveryFetchedMessage(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		pool := getExpandPool(t)
		thread := ""
		if threaded {
			thread = tsFor(0)
		}
		rootID := insertSlackMessage(t, pool, "CSCOPED", "scoped", tsFor(0), thread, "alice", "visible root", at(0))
		for offset, scope := range []string{"unstamped", "corpus:zendesk", "owner:bob@example.com"} {
			id := insertSlackMessage(t, pool, "CSCOPED", "", tsFor(offset+1), thread, "bob", "hidden content", at(offset+1))
			if _, err := pool.Exec(context.Background(), `UPDATE events SET scope=$2 WHERE id=$1`, id, scope); err != nil {
				t.Fatal(err)
			}
		}
		expander := NewExpander(pool)
		result, err := expander.Expand(context.Background(), rootID, []string{"public", "unstamped", ""})
		if err != nil || strings.Contains(result.Content, "hidden content") || !strings.Contains(result.Content, "visible root") {
			t.Fatalf("threaded=%v result=%+v err=%v", threaded, result, err)
		}
		if _, err := expander.Expand(context.Background(), rootID, nil); !errors.Is(err, events.ErrEventNotFound) {
			t.Fatalf("missing snapshot: %v", err)
		}
	}
}

func TestSlackExpand_Thread_RootAndReplies(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	// Root + two replies. Root's threadTS == its own ts (slack
	// populates after first reply).
	rootTS := tsFor(0)
	rootID := insertSlackMessage(t, pool, "C123", "incident-room",
		rootTS, rootTS, "alice", "is the api flaking again?", at(0))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(2), rootTS, "bob", "yeah, I just saw 503s", at(2))
	replyID := insertSlackMessage(t, pool, "C123", "",
		tsFor(5), rootTS, "carol", "rolling the control plane", at(5))

	// Expanding the reply should pull the whole thread (root +
	// 2 replies, oldest first).
	res, err := exp.Expand(context.Background(), replyID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand(reply): %v", err)
	}
	wantSubs := []string{
		"Thread in #incident-room",
		"@alice",
		"is the api flaking again?",
		"@bob",
		"yeah, I just saw 503s",
		"@carol",
		"rolling the control plane",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(res.Content, sub) {
			t.Errorf("missing %q in:\n%s", sub, res.Content)
		}
	}
	// Order: root before replies.
	idxRoot := strings.Index(res.Content, "is the api flaking")
	idxBob := strings.Index(res.Content, "503s")
	idxCarol := strings.Index(res.Content, "control plane")
	if idxRoot >= idxBob || idxBob >= idxCarol {
		t.Errorf("expected root → bob → carol order; got root=%d bob=%d carol=%d", idxRoot, idxBob, idxCarol)
	}

	// Expanding the root should return the same thread (just from
	// a different anchor).
	res2, err := exp.Expand(context.Background(), rootID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand(root): %v", err)
	}
	if !strings.Contains(res2.Content, "control plane") {
		t.Errorf("root expand should include replies, got:\n%s", res2.Content)
	}
}

func TestSlackExpand_Thread_RootWithoutCorpus(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	// Root not in corpus, only the reply. Slack populates thread_ts
	// on the reply pointing at the root's ts. Simulates the bot
	// joining mid-thread.
	rootTS := tsFor(0)
	replyID := insertSlackMessage(t, pool, "C123", "ops",
		tsFor(5), rootTS, "carol", "the rollout fixed it", at(5))

	res, err := exp.Expand(context.Background(), replyID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Reply alone — should still render with thread header (size=1)
	// and not crash.
	if !strings.Contains(res.Content, "Thread in #ops") {
		t.Errorf("expected thread header, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "the rollout fixed it") {
		t.Errorf("expected reply content, got:\n%s", res.Content)
	}
}

func TestSlackExpand_NeighborWindow(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool).WithWindow(20, 30*time.Minute)

	// Standalones in same channel; matched is in the middle.
	insertSlackMessage(t, pool, "C123", "general",
		tsFor(0), "", "alice", "morning", at(0))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(2), "", "bob", "anyone seen the deploy?", at(2))
	matchedID := insertSlackMessage(t, pool, "C123", "",
		tsFor(5), "", "carol", "yeah, it landed at 14:00", at(5))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(8), "", "dave", "ok cool", at(8))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(60), "", "eve", "much later message", at(60)) // outside ±30m

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(res.Content, "Channel context for #general") {
		t.Errorf("expected neighbor header, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "[matched]") {
		t.Errorf("expected matched marker on the hit line, got:\n%s", res.Content)
	}
	wantIn := []string{"morning", "deploy", "it landed at 14:00", "ok cool"}
	for _, sub := range wantIn {
		if !strings.Contains(res.Content, sub) {
			t.Errorf("missing %q in:\n%s", sub, res.Content)
		}
	}
	if strings.Contains(res.Content, "much later message") {
		t.Errorf("eve message should be outside ±30m window, got:\n%s", res.Content)
	}
}

func TestSlackExpand_NeighborWindow_ExcludesParallelThreadReplies(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool).WithWindow(20, 30*time.Minute)

	// A standalone matched message, plus a parallel thread happening
	// concurrently in the same channel. The thread root should appear
	// (it's a top-level channel post) but the replies should not.
	matchedID := insertSlackMessage(t, pool, "C123", "general",
		tsFor(5), "", "carol", "matched line", at(5))

	parallelRootTS := tsFor(2)
	insertSlackMessage(t, pool, "C123", "",
		parallelRootTS, parallelRootTS, "alice", "parallel thread root", at(2))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(3), parallelRootTS, "bob", "PARALLEL_REPLY_DO_NOT_INCLUDE_1", at(3))
	insertSlackMessage(t, pool, "C123", "",
		tsFor(4), parallelRootTS, "dave", "PARALLEL_REPLY_DO_NOT_INCLUDE_2", at(4))

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(res.Content, "parallel thread root") {
		t.Errorf("thread root should appear in neighbor window, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "replies in thread") {
		t.Errorf("thread root should be annotated with reply count, got:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "PARALLEL_REPLY_DO_NOT_INCLUDE_1") ||
		strings.Contains(res.Content, "PARALLEL_REPLY_DO_NOT_INCLUDE_2") {
		t.Errorf("parallel thread replies should be excluded, got:\n%s", res.Content)
	}
}

func TestSlackExpand_NeighborWindow_OtherChannelsExcluded(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	matchedID := insertSlackMessage(t, pool, "C123", "general",
		tsFor(5), "", "carol", "matched", at(5))
	insertSlackMessage(t, pool, "C999", "other",
		tsFor(5), "", "alice", "OTHER_CHANNEL_LINE", at(5))

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if strings.Contains(res.Content, "OTHER_CHANNEL_LINE") {
		t.Errorf("other channel content should be excluded, got:\n%s", res.Content)
	}
}

func TestSlackExpand_NeighborWindow_DeletedExcluded(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	matchedID := insertSlackMessage(t, pool, "C123", "general",
		tsFor(5), "", "carol", "matched", at(5))
	deletedID := insertSlackMessage(t, pool, "C123", "",
		tsFor(3), "", "bob", "DELETED_LINE", at(3))
	if _, err := pool.Exec(context.Background(),
		`UPDATE events SET deleted = true WHERE id = $1`, deletedID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if strings.Contains(res.Content, "DELETED_LINE") {
		t.Errorf("deleted message should be excluded, got:\n%s", res.Content)
	}
}

func TestSlackExpand_NeighborWindow_DeadChannel(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	matchedID := insertSlackMessage(t, pool, "C123", "general",
		tsFor(5), "", "carol", "lonely message", at(5))

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(res.Content, "lonely message") {
		t.Errorf("matched message should appear, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "[matched]") {
		t.Errorf("matched marker expected, got:\n%s", res.Content)
	}
}

func TestSlackExpand_EventNotFound(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool)

	_, err := exp.Expand(context.Background(), 999999, []string{"public"})
	if !errors.Is(err, events.ErrEventNotFound) {
		t.Errorf("err = %v, want ErrExpandEventNotFound", err)
	}
}

func TestSlackExpand_NeighborWindow_CountCap(t *testing.T) {
	pool := getExpandPool(t)
	exp := NewExpander(pool).WithWindow(2, 30*time.Minute)

	// 5 messages before, matched, 5 messages after. Count cap of 2
	// should yield: 2 before + matched + 2 after = 5 total.
	for i := -5; i <= -1; i++ {
		insertSlackMessage(t, pool, "C123", "general",
			tsFor(10+i), "", fmt.Sprintf("user%d", i), fmt.Sprintf("before%d", i), at(10+i))
	}
	matchedID := insertSlackMessage(t, pool, "C123", "",
		tsFor(10), "", "carol", "matched", at(10))
	for i := 1; i <= 5; i++ {
		insertSlackMessage(t, pool, "C123", "",
			tsFor(10+i), "", fmt.Sprintf("user%d", i), fmt.Sprintf("after%d", i), at(10+i))
	}

	res, err := exp.Expand(context.Background(), matchedID, []string{"public"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Count the rendered "[HH:MM:SS]" prefixes — one per row.
	got := strings.Count(res.Content, "[14:")
	if got != 5 {
		t.Errorf("expected 5 message rows under count cap, got %d:\n%s", got, res.Content)
	}
	// Tightest neighbors should be present, farthest excluded.
	if !strings.Contains(res.Content, "before-1") || !strings.Contains(res.Content, "before-2") {
		t.Errorf("nearest before-rows should be present, got:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "before-5") {
		t.Errorf("farthest before-row should be excluded by count cap, got:\n%s", res.Content)
	}
}
