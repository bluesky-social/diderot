package slack

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
)

// Tests for the unified ingest core (ingest.go). The live socket path
// and the worker/backfill path now both flow through ingestRawMessage;
// these tests pin the two behaviors the unification has to preserve:
//
//  1. Convergence: the same Slack message ingested via the live path
//     vs the backfill path lands the identical end state (events row +
//     slack_message_details row).
//  2. The error-posture split: the backfill caller PROMOTES an
//     image-reconcile failure to a hard error (so its sweep re-walks),
//     while the live caller swallows it (best-effort, lean on the
//     sweep). Documented at length on ingestResult.
//
// New file (not appended to slack_test.go / backfill_test.go) to avoid
// colliding with the wave-2 test-infrastructure refactor.
//
// All DB-backed; skip without DIDEROT_TEST_DB_URL.

func getIngestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// Route through the package's isolated test DB (internal/testdb)
	// like every other getter in this package — NOT a raw
	// DIDEROT_TEST_DB_URL pool. The raw URL points at an unmigrated
	// database under the per-package-DB model; only the testdb clones
	// carry the schema. slackTestDBURL also handles the skip-when-unset
	// contract.
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_message_details, slack_users, slack_bots,
		 slack_channel_info, slack_usergroups RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestIngestClassicBotHydratesDisplayName(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{bots: map[string]*slack.Bot{
		"B0EXAMPLE01": {ID: "B0EXAMPLE01", Name: "Build Status bot"},
	}})
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type":"message", "subtype":"bot_message", "channel":"CAPP",
		"bot_id":"B0EXAMPLE01", "text":"build finished", "ts":"1700000000.000100"
	}`))

	var actor, canonical string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT actor, actor_canonical FROM events WHERE external_id = 'CAPP:1700000000.000100'`).Scan(&actor, &canonical))
	assert.Equal(t, "Build Status bot", actor)
	assert.Equal(t, "slack-bot:B0EXAMPLE01", canonical)
}

func TestIngestClassicBotLookupFailureKeepsActorHydratable(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{botErrs: map[string]error{
		"B_TRANSIENT": &slack.SlackErrorResponse{Err: "internal_error"},
	}})
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type":"message", "subtype":"bot_message", "channel":"CAPP",
		"bot_id":"B_TRANSIENT", "text":"build finished", "ts":"1700000001.000100"
	}`))

	var actor, canonical string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(actor, ''), actor_canonical FROM events WHERE external_id = 'CAPP:1700000001.000100'`).Scan(&actor, &canonical))
	assert.Empty(t, actor, "an unresolved display name must remain eligible for later hydration")
	assert.Equal(t, "slack-bot:B_TRANSIENT", canonical)
}

// detailsRow is the projection we compare across the two paths.
type detailsRow struct {
	channelID  string
	userID     *string
	threadTS   *string
	isEdit     bool
	hasFiles   bool
	occurredAt time.Time
}

func readEventAndDetails(t *testing.T, pool *pgxpool.Pool, externalID string) (body string, d detailsRow) {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx,
		`SELECT id, body FROM events WHERE source = 'slack' AND external_id = $1`,
		externalID).Scan(&eventID, &body); err != nil {
		t.Fatalf("read event %s: %v", externalID, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT channel_id, user_id, thread_ts, is_edit, has_files, occurred_at
		  FROM slack_message_details WHERE event_id = $1
	`, eventID).Scan(&d.channelID, &d.userID, &d.threadTS, &d.isEdit, &d.hasFiles, &d.occurredAt); err != nil {
		t.Fatalf("read details for event %d (%s): %v", eventID, externalID, err)
	}
	return body, d
}

// TestLiveAndBackfillConverge drives the SAME logical Slack message
// through the live path (handleEventsAPI) and the backfill core
// (ingestMessageCore) in two separate channels and asserts the events
// body + the slack_message_details projection match. This is the
// regression guard for the dedup: if the two paths drift again (a
// field stamped in one but not the other), the projections diverge.
func TestLiveAndBackfillConverge(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()

	// Live path into channel CLIVE.
	c := newTestConsumer(t, pool)
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "CLIVE",
		"user": "U456",
		"text": "the node pool is flapping",
		"ts": "1700000000.000100",
		"thread_ts": "1699999000.000050"
	}`))

	// Backfill path into channel CBACK with the same content shape.
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, &fakeBackfillAPI{}, dir, prefix, nil,
		prometheus.NewRegistry(), "example.slack.com")

	msg := buildMsg("U456", "1700000000.000100", "1699999000.000050",
		"the node pool is flapping", 0)
	disp, err := bf.ingestMessageCore(ctx, "CBACK", msg, ingestPathBackfill)
	if err != nil {
		t.Fatalf("backfill ingestMessageCore: %v", err)
	}
	if ingestDisposition(disp) != ingestInserted {
		t.Fatalf("backfill disposition = %q, want inserted", disp)
	}

	liveBody, liveDetails := readEventAndDetails(t, pool, "CLIVE:1700000000.000100")
	backBody, backDetails := readEventAndDetails(t, pool, "CBACK:1700000000.000100")

	if liveBody != backBody {
		t.Errorf("body diverged: live=%q back=%q", liveBody, backBody)
	}
	// channel_id differs by construction; everything else must match.
	if (liveDetails.userID == nil) != (backDetails.userID == nil) ||
		(liveDetails.userID != nil && *liveDetails.userID != *backDetails.userID) {
		t.Errorf("user_id diverged: live=%v back=%v", liveDetails.userID, backDetails.userID)
	}
	if (liveDetails.threadTS == nil) != (backDetails.threadTS == nil) ||
		(liveDetails.threadTS != nil && *liveDetails.threadTS != *backDetails.threadTS) {
		t.Errorf("thread_ts diverged: live=%v back=%v", liveDetails.threadTS, backDetails.threadTS)
	}
	if liveDetails.isEdit != backDetails.isEdit {
		t.Errorf("is_edit diverged: live=%v back=%v", liveDetails.isEdit, backDetails.isEdit)
	}
	if liveDetails.hasFiles != backDetails.hasFiles {
		t.Errorf("has_files diverged: live=%v back=%v", liveDetails.hasFiles, backDetails.hasFiles)
	}
	if !liveDetails.occurredAt.Equal(backDetails.occurredAt) {
		t.Errorf("occurred_at diverged: live=%v back=%v", liveDetails.occurredAt, backDetails.occurredAt)
	}
}

// TestLiveDetailsAreAtomic asserts the live path lands the
// slack_message_details row in lockstep with the event — never an
// event without a detail row. With details folded into the upsert tx,
// any single ingested message that has an events row must also have a
// details row.
func TestLiveDetailsAreAtomic(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()
	c := newTestConsumer(t, pool)

	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "C777",
		"user": "U999",
		"text": "atomic details check",
		"ts": "1700000123.000400"
	}`))

	var orphans int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM events e
		  LEFT JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.source = 'slack' AND d.event_id IS NULL
	`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("found %d slack events with no details row; details must land atomically", orphans)
	}
}

// TestStandaloneRootBecomesCanonicalThreadRoot covers the lifecycle where
// Slack first emits a standalone message without thread_ts, then a history
// sweep observes the same message after it has received a reply. Slack sets
// thread_ts to the root's own ts in that second shape, and the re-ingest must
// replace the formerly standalone URL with Slack's canonical thread URL.
func TestStandaloneRootBecomesCanonicalThreadRoot(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()
	const (
		channel = "CTHREAD"
		rootTS  = "1700000123.000400"
	)

	c := newTestConsumer(t, pool)
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "CTHREAD",
		"user": "U999",
		"text": "root becomes a thread",
		"ts": "1700000123.000400"
	}`))

	var gotURL string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT url FROM events WHERE source = 'slack' AND external_id = $1`,
		channel+":"+rootTS).Scan(&gotURL))
	assert.Equal(t,
		"https://example.slack.com/archives/CTHREAD/p1700000123000400",
		gotURL)

	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	prefix := NewPrefixBuilder(dir, pool)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	bf := NewBackfill(pool, sink, &fakeBackfillAPI{}, dir, prefix, nil,
		prometheus.NewRegistry(), "example.slack.com")
	rootAsThread := buildMsg("U999", rootTS, rootTS, "root becomes a thread", 1)
	disp, err := bf.ingestMessageCore(ctx, channel, rootAsThread, ingestPathSync)
	require.NoError(t, err)
	assert.Equal(t, string(ingestUpdated), disp)

	var gotThreadTS *string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT e.url, d.thread_ts
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.source = 'slack' AND e.external_id = $1
	`, channel+":"+rootTS).Scan(&gotURL, &gotThreadTS))
	assert.Equal(t,
		"https://example.slack.com/archives/CTHREAD/p1700000123000400?thread_ts=1700000123.000400&cid=CTHREAD",
		gotURL)
	require.NotNil(t, gotThreadTS)
	assert.Equal(t, rootTS, *gotThreadTS)

	// Replaying the same canonical root is a true no-op.
	disp, err = bf.ingestMessageCore(ctx, channel, rootAsThread, ingestPathSync)
	require.NoError(t, err)
	assert.Equal(t, string(ingestNoChange), disp)
}

// TestIngestHistoryTombstoneDeletesExistingEvent pins the REAL
// conversations.history / conversations.replies shape for a thread root
// that was deleted while it still had a live reply: an unwrapped
// top-level subtype="tombstone" message at that same ts (reply_count
// still reflecting the surviving reply), not a message_changed wrapper —
// that wrapped shape is what the LIVE Events API sends instead (pinned
// separately by TestParseMessageChangedTombstone). A history re-walk
// (SyncWorker's tail sweep, Backfill, ThreadWorker's replies walk) only
// ever sees the unwrapped shape, so that's what ingestRawMessage — the
// core all three share — must be fed here.
func TestIngestHistoryTombstoneDeletesExistingEvent(t *testing.T) {
	pool := getIngestPool(t)
	ctx := context.Background()
	c := newTestConsumer(t, pool)
	const externalID = "CTOMBSTONE:1700000123.000400"

	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "CTOMBSTONE",
		"user": "U999",
		"text": "this content must be removed",
		"ts": "1700000123.000400",
		"thread_ts": "1700000123.000400"
	}`))

	var chunksBefore int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE e.source = 'slack' AND e.external_id = $1
	`, externalID).Scan(&chunksBefore))
	require.Positive(t, chunksBefore)

	tombstone := []byte(`{
		"type": "message",
		"subtype": "tombstone",
		"channel": "CTOMBSTONE",
		"user": "USLACKBOT",
		"text": "This message was deleted.",
		"ts": "1700000123.000400",
		"thread_ts": "1700000123.000400",
		"reply_count": 1,
		"latest_reply": "1700000200.000100"
	}`)
	got, err := ingestRawMessage(ctx, c.ingestDeps(), tombstone)
	require.NoError(t, err)
	assert.Equal(t, ingestUpdated, got.Disposition)
	require.NotZero(t, got.EventID)

	var deleted bool
	var body string
	var chunksAfter int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT e.deleted, e.body, count(c.id)
		  FROM events e
		  LEFT JOIN event_chunks c ON c.event_id = e.id
		 WHERE e.source = 'slack' AND e.external_id = $1
		 GROUP BY e.id
	`, externalID).Scan(&deleted, &body, &chunksAfter))
	assert.True(t, deleted)
	assert.Empty(t, body)
	assert.Zero(t, chunksAfter)
}

// NOTE on the error-posture split (backfill promotes a reconcile
// failure to a hard error; live swallows it): exercising the real
// divergence needs a forced ReconcileImages failure, which requires DB
// fault injection (a fake pool that errors on the event_images
// statements) we don't have here. The attachment-free messages in the
// tests above produce a no-op reconcile, so both postures see a nil
// ImageErr and the difference doesn't manifest. The split is enforced
// in code (Backfill.ingestMessageCore returns on res.ImageErr != nil;
// Consumer.processMessageEventAsync logs + bumps attachments_err and
// continues) and documented on ingestResult. Flagged in the handoff
// as the one piece of behavior not covered by an automated test.
