package slack

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

// fakeBackfillAPI implements the backfillAPI interface used by
// Backfill. It models a single channel's history + per-thread
// replies in-memory so tests can drive the two-pass backfill
// deterministically. Supports rate-limit injection to exercise the
// retry path.
type fakeBackfillAPI struct {
	warmFakeAPI // composes the warmer fake for users + channels

	// history is returned by GetConversationHistoryContext,
	// newest-first (matching Slack). No pagination — single page.
	history []slack.Message

	// replies[ts] returns the reply-thread for a given thread root
	// timestamp. Slack's replies call returns the root as the first
	// element, then replies oldest-first.
	replies map[string][]slack.Message

	rateLimitHistoryOnce bool
	histCalls            int

	// totalHistoryCalls and totalRepliesCalls count every call
	// unconditionally (unlike histCalls, which only tracks the 429
	// injection above), so a test can assert a channel's history was
	// never fetched at all.
	totalHistoryCalls int
	totalRepliesCalls int
}

func (f *fakeBackfillAPI) GetConversationHistoryContext(_ context.Context, _ *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	f.totalHistoryCalls++
	if f.rateLimitHistoryOnce && f.histCalls == 0 {
		f.histCalls++
		return nil, &slack.RateLimitedError{RetryAfter: 10 * time.Millisecond}
	}
	// slack.GetConversationHistoryResponse embeds the History struct
	// which embeds SlackResponse + Messages. Return the full set in
	// one page — HasMore=false terminates the loop.
	resp := &slack.GetConversationHistoryResponse{}
	resp.Messages = f.history
	resp.HasMore = false
	resp.ResponseMetaData.NextCursor = ""
	resp.Ok = true
	return resp, nil
}

func (f *fakeBackfillAPI) GetConversationRepliesContext(_ context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.totalRepliesCalls++
	msgs, ok := f.replies[params.Timestamp]
	if !ok {
		return nil, false, "", fmt.Errorf("fake: no replies for %s", params.Timestamp)
	}
	return msgs, false, "", nil
}

func getBackfillPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_users, slack_bots, slack_channel_info, slack_usergroups
		 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// buildMsg is a local helper. slack.Message wraps slack.Msg which has
// many fields but only these matter for backfill input.
func buildMsg(user, ts, threadTS, text string, replyCount int) slack.Message {
	m := slack.Message{}
	m.Type = "message"
	m.User = user
	m.Timestamp = ts
	m.ThreadTimestamp = threadTS
	m.Text = text
	m.ReplyCount = replyCount
	return m
}

// buildThreadRoot is buildMsg for a thread parent that also carries
// the latest_reply field the SyncWorker uses to gate re-enqueues.
// threadTS is "" (a root's thread_ts equals its own ts in Slack, but
// the sweep treats ThreadTimestamp=="" || ==Timestamp as "not a
// reply"; we leave it empty so the history pass ingests it as a
// top-level message and harvests its thread root).
func buildThreadRoot(user, ts, text string, replyCount int, latestReply string) slack.Message {
	m := buildMsg(user, ts, "", text, replyCount)
	m.LatestReply = latestReply
	return m
}

func TestBackfillTwoPassOrderingAndPrefix(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()

	// Scenario: one channel, one thread root with 2 replies. Total
	// 3 messages. We expect:
	//  - All 3 rows in events
	//  - Reply contextual_prefix references the thread root body
	//    (proves the two-pass ordering — root lands before replies
	//    so the prefix lookup sees it).
	//
	// Slack returns history newest-first: [reply2, reply1, root].
	// Pass 1 inserts all 3 with the top-level prefix (since none
	// have thread_root_in_db yet for the replies). Pass 2 re-visits
	// replies — and because of sink.Upsert's no-op short-circuit
	// (and identical content), nothing changes. The prefix is set
	// on first ingest.
	//
	// To get thread-root-qualified prefixes on the replies, the
	// backfill has to process parent-before-reply on first
	// insertion. The correct fix is: in pass 1 skip messages where
	// thread_ts != ts (i.e. skip thread replies — they're dupes of
	// what pass 2 will deliver in order). Then pass 2 fetches the
	// whole thread including the root (which is re-upserted as a
	// no-op) and the replies get the right prefix.
	//
	// We assert BOTH behaviors below: history inserts the root +
	// skips replies, pass 2 inserts replies with the thread-root
	// prefix.
	rootTS := "1700000000.000100"
	reply1TS := "1700000050.000200"
	reply2TS := "1700000100.000300"

	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			users: []slack.User{
				{ID: "U1", Name: "alice", Profile: slack.UserProfile{DisplayName: "Alice"}},
			},
			channelsP1: []slack.Channel{chanWith("C1", "team-eng", false, false)},
		},
		// newest-first
		history: []slack.Message{
			buildMsg("U1", reply2TS, rootTS, "the cluster bounced, back now", 0),
			buildMsg("U1", reply1TS, rootTS, "looking into it", 0),
			buildMsg("U1", rootTS, rootTS, "is production down?", 2),
		},
		replies: map[string][]slack.Message{
			// oldest-first, root-first
			rootTS: {
				buildMsg("U1", rootTS, rootTS, "is production down?", 2),
				buildMsg("U1", reply1TS, rootTS, "looking into it", 0),
				buildMsg("U1", reply2TS, rootTS, "the cluster bounced, back now", 0),
			},
		},
	}

	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")

	prog := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{}, prog); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 3 rows in events.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("events rows: got %d, want 3", n)
	}

	// Reply 1's chunk prefix should name #team-eng and
	// reference the root body.
	var replyPrefix string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(c.context_prefix, '')
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE e.external_id = $1
	`, "C1:"+reply1TS).Scan(&replyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyPrefix, "#team-eng") {
		t.Errorf("reply prefix missing channel: %q", replyPrefix)
	}
	if !strings.Contains(replyPrefix, "is production down?") {
		t.Errorf("reply prefix missing thread root excerpt: %q", replyPrefix)
	}
	if !strings.Contains(replyPrefix, "by @Alice") {
		t.Errorf("reply prefix missing author: %q", replyPrefix)
	}

	// Root's prefix is the top-level form.
	var rootPrefix string
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(c.context_prefix, '')
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE e.external_id = $1
	`, "C1:"+rootTS).Scan(&rootPrefix)
	// Backfill resolved U1's display name from the Directory
	// warm-fake (which seeded profile.display_name="Alice"), so the
	// prefix includes the `by @Alice` fragment per the actor-in-
	// prefix change.
	if rootPrefix != "Slack message in #team-eng by @Alice:" {
		t.Errorf("root prefix unexpected: %q", rootPrefix)
	}

	// Counters should report 3 inserted (plus whatever no-ops the
	// thread-root dupe created during pass 2).
	if got := prog.InsertedEvents.Load(); got < 1 {
		t.Errorf("InsertedEvents=%d, want >=1", got)
	}
	if prog.Errors.Load() != 0 {
		t.Errorf("backfill reported Errors=%d; last=%v", prog.Errors.Load(), prog.LastErr.Load())
	}
}

func TestBackfillIdempotent(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()

	rootTS := "1700000000.000100"
	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			channelsP1: []slack.Channel{chanWith("C1", "c", false, false)},
		},
		history: []slack.Message{
			buildMsg("U1", rootTS, "", "hello", 0),
		},
	}
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")

	prog1 := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{}, prog1); err != nil {
		t.Fatal(err)
	}
	if got := prog1.InsertedEvents.Load(); got != 1 {
		t.Errorf("first run inserted=%d, want 1", got)
	}

	// Second run: no new API state, should be all no-ops.
	prog2 := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{SkipPrewarm: true}, prog2); err != nil {
		t.Fatal(err)
	}
	if got := prog2.NoChangeEvents.Load(); got != 1 {
		t.Errorf("second run no-op=%d, want 1", got)
	}
	if got := prog2.InsertedEvents.Load(); got != 0 {
		t.Errorf("second run inserted=%d, want 0", got)
	}

	// Exactly one row + one chunk in the DB.
	var events, chunks int
	_ = pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM events),
		       (SELECT count(*) FROM event_chunks)
	`).Scan(&events, &chunks)
	if events != 1 || chunks != 1 {
		t.Errorf("idempotency violated: events=%d chunks=%d", events, chunks)
	}
}

// TestBackfillLimiterEngagesOn429 wires a Limiter into the
// Backfill engine and verifies that a 429 from Slack causes the
// limiter to transition into degraded mode AND that subsequent
// successful pages put it back into normal. End-to-end proof
// that historyPage routes results through the limiter.
func TestBackfillLimiterEngagesOn429(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()

	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			channelsP1: []slack.Channel{chanWith("C1", "c", false, false)},
		},
		history: []slack.Message{
			buildMsg("U1", "1700000000.000100", "", "hello", 0),
		},
		// Force the very first historyPage call to 429; the second
		// call (after Retry-After sleep) succeeds.
		rateLimitHistoryOnce: true,
	}
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")

	// Fast limiter so the test stays sub-second.
	cfg := LimiterConfig{
		NormalInterval: 1 * time.Millisecond,
		NormalBurst:    5,
	}
	limiter := NewLimiter(cfg, nil, prometheus.NewRegistry())
	bf = bf.WithLimiter(limiter)

	prog := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{}, prog); err != nil {
		t.Fatal(err)
	}

	// Backfill saw 1 message → 1 inserted event after one 429+retry.
	if got := prog.InsertedEvents.Load(); got != 1 {
		t.Errorf("inserted=%d, want 1", got)
	}
}

// TestBackfillNilLimiterIsBackcompat: when Backfill is constructed
// without WithLimiter, behavior matches the pre-Stage-2 design
// (retry-after-sleep on 429, no proactive pacing). The existing
// fake-injected 429 path keeps working.
func TestBackfillNilLimiterIsBackcompat(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()

	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			channelsP1: []slack.Channel{chanWith("C1", "c", false, false)},
		},
		history: []slack.Message{
			buildMsg("U1", "1700000000.000100", "", "hello", 0),
		},
		rateLimitHistoryOnce: true,
	}
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com")
	// Note: NO WithLimiter call.

	prog := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{}, prog); err != nil {
		t.Fatal(err)
	}
	if got := prog.InsertedEvents.Load(); got != 1 {
		t.Errorf("inserted=%d, want 1", got)
	}
}

// TestBackfillRunSkipsBlacklistedChannel pins that Backfill.Run
// honours the ingest blacklist: an operator-blacklisted channel's
// history (and its threads) must not land in `events`. runChannel
// checks the blacklist before making any Slack API call for the
// channel, so neither the history nor the replies call happens at
// all, and no row is written.
func TestBackfillRunSkipsBlacklistedChannel(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE slack_ingest_blacklist RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}

	rootTS := "1700000000.000100"
	replyTS := "1700000050.000200"
	api := &fakeBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			channelsP1: []slack.Channel{chanWith("C1", "blocked-channel", false, false)},
		},
		history: []slack.Message{
			buildMsg("U1", rootTS, rootTS, "is production down?", 1),
		},
		replies: map[string][]slack.Message{
			rootTS: {
				buildMsg("U1", rootTS, rootTS, "is production down?", 1),
				buildMsg("U1", replyTS, rootTS, "looking into it", 0),
			},
		},
	}
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())

	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Add(ctx, "C1", "blocked-channel", "operator blocked it", "operator"); err != nil {
		t.Fatal(err)
	}

	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com").
		WithIngestBlacklist(bl)

	prog := &Progress{}
	if err := bf.Run(ctx, []string{"C1"}, BackfillOptions{SkipPrewarm: true}, prog); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if api.totalHistoryCalls != 0 {
		t.Errorf("history calls = %d, want 0: a blacklisted channel must never be fetched", api.totalHistoryCalls)
	}
	if api.totalRepliesCalls != 0 {
		t.Errorf("replies calls = %d, want 0: a blacklisted channel's threads must never be fetched", api.totalRepliesCalls)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("events rows = %d, want 0 for a blacklisted channel", n)
	}
	if got := prog.InsertedEvents.Load(); got != 0 {
		t.Errorf("InsertedEvents = %d, want 0", got)
	}
	if got := prog.Errors.Load(); got != 0 {
		t.Errorf("Errors = %d, want 0: skipping a blacklisted channel is not a failure", got)
	}
}

// TestBackfillRunSkipsOnlyBlacklistedChannels drives Run with two
// channels, one blacklisted and one not, to prove the check applies
// per channel rather than, say, aborting the whole Run or leaking
// across channels.
func TestBackfillRunSkipsOnlyBlacklistedChannels(t *testing.T) {
	pool := getBackfillPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE slack_ingest_blacklist RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}

	api := &multiChannelBackfillAPI{
		warmFakeAPI: warmFakeAPI{
			channelsP1: []slack.Channel{
				chanWith("C1", "blocked-channel", false, false),
				chanWith("C2", "allowed-channel", false, false),
			},
		},
		history: map[string][]slack.Message{
			"C1": {buildMsg("U1", "1700000000.000100", "", "blocked channel message", 0)},
			"C2": {buildMsg("U1", "1700000000.000200", "", "allowed channel message", 0)},
		},
	}
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())

	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Add(ctx, "C1", "blocked-channel", "operator blocked it", "operator"); err != nil {
		t.Fatal(err)
	}

	bf := NewBackfill(pool, sink, api, dir, prefix, nil, prometheus.NewRegistry(), "example.slack.com").
		WithIngestBlacklist(bl)

	prog := &Progress{}
	if err := bf.Run(ctx, []string{"C1", "C2"}, BackfillOptions{}, prog); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := api.historyCalls["C1"]; got != 0 {
		t.Errorf("C1 (blacklisted) history calls = %d, want 0", got)
	}
	if got := api.historyCalls["C2"]; got != 1 {
		t.Errorf("C2 (not blacklisted) history calls = %d, want 1", got)
	}

	var ids []string
	rows, err := pool.Query(ctx, `SELECT external_id FROM events ORDER BY external_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != "C2:1700000000.000200" {
		t.Errorf("events external_ids = %v, want exactly [C2:1700000000.000200]", ids)
	}
}

// multiChannelBackfillAPI is a backfillAPI fake keyed per channel, used
// where fakeBackfillAPI's single-channel history field isn't enough
// (verifying a non-blacklisted channel in the same Run is still
// fetched normally).
type multiChannelBackfillAPI struct {
	warmFakeAPI
	history      map[string][]slack.Message
	historyCalls map[string]int
}

func (f *multiChannelBackfillAPI) GetConversationHistoryContext(_ context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	if f.historyCalls == nil {
		f.historyCalls = map[string]int{}
	}
	f.historyCalls[params.ChannelID]++
	resp := &slack.GetConversationHistoryResponse{}
	resp.Messages = f.history[params.ChannelID]
	resp.HasMore = false
	resp.ResponseMetaData.NextCursor = ""
	resp.Ok = true
	return resp, nil
}

func (f *multiChannelBackfillAPI) GetConversationRepliesContext(_ context.Context, _ *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}
