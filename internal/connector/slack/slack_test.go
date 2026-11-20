package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
)

func getConsumerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_message_details, slack_bots, slack_channel_config,
		 ask_sessions, ask_answer_messages,
		 ask_feedback, ask_turn_claims RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// newTestConsumer wires a Consumer suitable for handleEventsAPI
// tests. No appToken/botToken are needed: Slack presentation uses the
// fake progress transport, and the remaining nil-api paths degrade safely.
func newTestConsumer(t *testing.T, pool *pgxpool.Pool) *Consumer {
	t.Helper()
	policy, err := authorization.New(authorization.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	return &Consumer{
		channelAuthorizer: &ChannelAuthorizer{Policy: policy},
		pool:              pool,
		sink:              sink,
		workspaceHost:     "example.slack.com",
		logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		metrics:           newConsumerMetrics(prometheus.NewRegistry()),
		progress:          &fakeProgressTransport{},
		// dir is nil — resolver-less parse is what tests want anyway.
	}
}

// wrapMessageEvent wraps a raw message-event payload in the
// slackevents.EventsAPIEvent shell our handler expects.
func wrapMessageEvent(t *testing.T, raw string) slackevents.EventsAPIEvent {
	t.Helper()
	var inner slackevents.MessageEvent
	if err := json.Unmarshal([]byte(raw), &inner); err != nil {
		t.Fatalf("unmarshal test MessageEvent: %v", err)
	}
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &inner,
		},
	}
}

func TestConsumerIngestsPlainMessage(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	raw := `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "hello from the consumer test",
		"ts": "1700000000.000100"
	}`
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	// One row in events, one in slack_message_details.
	var numEvents, numDetails int
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM events),
		       (SELECT count(*) FROM slack_message_details)
	`).Scan(&numEvents, &numDetails)
	if err != nil {
		t.Fatal(err)
	}
	if numEvents != 1 || numDetails != 1 {
		t.Fatalf("got events=%d, details=%d; want 1/1", numEvents, numDetails)
	}

	var channelID string
	var isEdit bool
	err = pool.QueryRow(ctx, `
		SELECT channel_id, is_edit FROM slack_message_details
	`).Scan(&channelID, &isEdit)
	if err != nil {
		t.Fatal(err)
	}
	if channelID != "C123" || isEdit {
		t.Errorf("details row: channel=%q is_edit=%v", channelID, isEdit)
	}
}

// TestConsumerSkipsSelfAuthoredMessages verifies the live-path
// gate that drops messages diderot itself posted (placeholder,
// status edits, answer chunks). The test fires a normal user
// message + a self-authored message and asserts only the user
// message lands in events. Filter is by actor_id (the bot's
// slack user_id) so human follow-ups in diderot-active threads
// still ingest normally — covered by the user message case.
func TestConsumerSkipsSelfAuthoredMessages(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	ctx := context.Background()

	// Self-authored: placeholder reply diderot posted to a thread.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "C123",
		"user": "U_DIDEROT",
		"text": ":hourglass_flowing_sand: thinking...",
		"ts": "1700000000.000100"
	}`))

	// Human follow-up in the same channel: actor differs from
	// selfUserID, ingests normally.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "C123",
		"user": "U_HUMAN",
		"text": "wait, did you find the BGP thread?",
		"ts": "1700000010.000200"
	}`))

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1 (self-authored should be filtered, human kept)", n)
	}

	// Verify it's the human's message that landed.
	var actorID, body string
	if err := pool.QueryRow(ctx,
		`SELECT actor_id, body FROM events`).Scan(&actorID, &body); err != nil {
		t.Fatal(err)
	}
	if actorID != "U_HUMAN" {
		t.Errorf("actor_id = %q, want U_HUMAN (self-authored row leaked through)", actorID)
	}
	if !strings.Contains(body, "BGP thread") {
		t.Errorf("body = %q, want the human follow-up", body)
	}
}

func TestConsumerHandlesEdit(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	// Plain insert first.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "v1",
		"ts": "1700000000.000100"
	}`))

	// message_changed for the same ts: should update in place.
	// slackevents.MessageEvent's json tags handle nested Message,
	// but if it doesn't populate our embedded payload we'd need to
	// fall back to the raw path. Let's build the inner shape manually.
	inner := map[string]any{
		"type":    "message",
		"subtype": "message_changed",
		"channel": "C123",
		"ts":      "1700000010.000000",
		"message": map[string]any{
			"type": "message",
			"user": "U456",
			"text": "v2 edited",
			"ts":   "1700000000.000100",
		},
	}
	innerJSON, _ := json.Marshal(inner)
	var msgEvent slackevents.MessageEvent
	_ = json.Unmarshal(innerJSON, &msgEvent)
	c.handleEventsAPI(ctx, slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &msgEvent,
		},
	})

	// Still one events row, body is v2 edited, is_edit on side table.
	var numEvents int
	var body string
	var isEdit bool
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM events),
		       (SELECT body FROM events),
		       (SELECT is_edit FROM slack_message_details)
	`).Scan(&numEvents, &body, &isEdit)
	if err != nil {
		t.Fatal(err)
	}
	if numEvents != 1 {
		t.Errorf("events rows: got %d, want 1", numEvents)
	}
	if body != "v2 edited" {
		t.Errorf("body: got %q", body)
	}
	if !isEdit {
		t.Errorf("is_edit: want true after edit")
	}
}

func TestConsumerHandlesDelete(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	// Insert first.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "doomed",
		"ts": "1700000000.000100"
	}`))

	inner := map[string]any{
		"type":       "message",
		"subtype":    "message_deleted",
		"channel":    "C123",
		"deleted_ts": "1700000000.000100",
		"ts":         "1700000020.000000",
		"previous_message": map[string]any{
			"type": "message",
			"user": "U456",
			"text": "doomed",
			"ts":   "1700000000.000100",
		},
	}
	innerJSON, _ := json.Marshal(inner)
	var msgEvent slackevents.MessageEvent
	_ = json.Unmarshal(innerJSON, &msgEvent)
	c.handleEventsAPI(ctx, slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &msgEvent,
		},
	})

	// Event should now be deleted=true with empty body + no chunks.
	var deleted bool
	var body string
	var numChunks int
	err := pool.QueryRow(ctx, `
		SELECT deleted, body,
		       (SELECT count(*) FROM event_chunks WHERE event_id = events.id)
		  FROM events
	`).Scan(&deleted, &body, &numChunks)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("row should be deleted=true after message_deleted")
	}
	if body != "" {
		t.Errorf("body should be empty after tombstone, got %q", body)
	}
	if numChunks != 0 {
		t.Errorf("chunks after tombstone: got %d, want 0", numChunks)
	}
}

func TestConsumerSkipsChannelJoinSubtype(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	inner := map[string]any{
		"type":    "message",
		"subtype": "channel_join",
		"channel": "C123",
		"user":    "U456",
		"text":    "<@U456> has joined",
		"ts":      "1700000000.000100",
	}
	innerJSON, _ := json.Marshal(inner)
	var msgEvent slackevents.MessageEvent
	_ = json.Unmarshal(innerJSON, &msgEvent)
	c.handleEventsAPI(ctx, slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &msgEvent,
		},
	})

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("channel_join should be skipped; events count = %d", n)
	}
}

func TestConsumerResolvesActorDisplayName(t *testing.T) {
	// The consumer must stamp
	// Directory-resolved display name into events.actor so the
	// `@alice` (not `@U12345`) shows up in retrieval results.
	pool := getConsumerPool(t)
	ctx := context.Background()

	// Pre-populate the Directory cache with U456's display_name so
	// LookupUser doesn't try to call Slack.
	_, err := pool.Exec(ctx, `
		INSERT INTO slack_users (user_id, name, display_name, real_name, email, is_bot, fetched_at)
		VALUES ('U456', 'legacy-name', 'Alice Example', 'Alice', 'alice@example.com', false, now())
	`)
	if err != nil {
		t.Fatal(err)
	}
	// Build a consumer wired with a real Directory.
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	dir := newDirectoryWithAPI(pool, &fakeAPI{})
	c := &Consumer{
		pool:          pool,
		sink:          sink,
		workspaceHost: "example.slack.com",
		logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		metrics:       newConsumerMetrics(prometheus.NewRegistry()),
		dir:           dir,
	}

	raw := `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "hello",
		"ts": "1700000000.000100"
	}`
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	var actor, actorID string
	err = pool.QueryRow(ctx,
		`SELECT COALESCE(actor, ''), COALESCE(actor_id, '') FROM events`,
	).Scan(&actor, &actorID)
	if err != nil {
		t.Fatal(err)
	}
	if actorID != "U456" {
		t.Errorf("actor_id = %q, want U456", actorID)
	}
	if actor != "Alice Example" {
		t.Errorf("actor = %q, want 'Alice Example' (resolved from Directory)", actor)
	}
}

func TestConsumerIngestsOnSuccess(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	raw := `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "hi",
		"ts": "1700000000.000100"
	}`
	// Direct handler calls run ingest synchronously, so the row is present on
	// return. Run dispatches the same handler through its tracked goroutines.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("events count = %d, want 1", n)
	}
}

func TestConsumerToleratesSinkError(t *testing.T) {
	// Ack-first refactor: the event loop acks the envelope BEFORE
	// dispatching the handler, so a sink error can't be signalled
	// back as a no-ack; the SyncWorker tail-sweep is the durable
	// backstop for messages that failed to upsert inline. The
	// handler's contract is simply "don't panic, don't block" on a
	// sink failure. Returning false to trigger Slack redelivery is
	// not an option: socket-mode redelivery causes duplicate-event
	// cascades.
	pool := getConsumerPool(t)
	pool.Close()
	c := newTestConsumer(t, pool)

	raw := `{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "sink is down; handler must not panic",
		"ts": "1700000000.000100"
	}`
	// Must return without panicking despite the closed pool.
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))
}

func TestConsumerDropsOnParseError(t *testing.T) {
	// Parse errors are content problems — the handler logs + metrics
	// and drops. No row lands.
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	// message_changed without the required `message` sub-object
	// — ParseMessage returns an error for this shape.
	inner := map[string]any{
		"type":    "message",
		"subtype": "message_changed",
		"channel": "C123",
		"ts":      "1700000000.000100",
		// no message or previous_message object
	}
	innerJSON, _ := json.Marshal(inner)
	var msgEvent slackevents.MessageEvent
	_ = json.Unmarshal(innerJSON, &msgEvent)
	c.handleEventsAPI(ctx, slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &msgEvent,
		},
	})

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("events count = %d, want 0 (parse error drops)", n)
	}
}

func TestConsumerDropsOnSkippedSubtype(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	ctx := context.Background()

	inner := map[string]any{
		"type":    "message",
		"subtype": "channel_join",
		"channel": "C123",
		"user":    "U456",
		"ts":      "1700000000.000100",
	}
	innerJSON, _ := json.Marshal(inner)
	var msgEvent slackevents.MessageEvent
	_ = json.Unmarshal(innerJSON, &msgEvent)
	c.handleEventsAPI(ctx, slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &msgEvent,
		},
	})

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("events count = %d, want 0 (skipped subtype drops)", n)
	}
}

// TestConsumerRunValidatesTokens ensures Run() fails fast without
// touching the network when tokens are empty — the leader shouldn't
// silently swallow a misconfigured deployment.
func TestConsumerRunValidatesTokens(t *testing.T) {
	c := NewConsumer(nil, nil, "", "", "host", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Run(ctx)
	if err == nil {
		t.Fatal("expected error on empty tokens, got nil")
	}
}

// =====================================================================
// Ask routing
// =====================================================================

// fakeAskRouter records the routing decisions handleEventsAPI makes
// so the assertion can check whether the consumer would have asked
// the router to do anything. Sessions known/unknown are configurable
// per-test; RunSlackTurn isn't actually invoked in these tests because
// the goroutine runs against a nil api and would panic at PostMessage —
// the test asserts the routing decision via lookup-or-create calls,
// not the post itself.
//
// To keep the goroutine off the api code path entirely, fakeAskRouter
// returns an error from LookupOrCreateSlackSession, which short-
// circuits runAskTurn before it ever reaches PostMessageContext.
// When succeedCreate is true, LookupOrCreateSlackSession returns a
// stable fake UUID so runAskTurn can reach ExecuteTurn.
type fakeAskRouter struct {
	known          map[string]bool
	contextThrough map[string]time.Time // per "channel:thread" mark, nil = zero
	succeedCreate  bool                 // if true, LookupOrCreate returns a UUID (not an error)

	mu              sync.Mutex
	knownCalls      []string // "channel:thread"
	createCalls     []string
	createEmails    []string
	turnCalls       []string
	questions       []string
	threadContexts  []string    // ThreadContext per ExecuteTurn call
	contextThroughs []time.Time // ContextThrough per ExecuteTurn call
	requests        []ask.TurnRequest
}

type fakeDMScopeResolver struct {
	scopes []string
	emails []string
}

func (f *fakeDMScopeResolver) ScopesForEmail(_ context.Context, email string) []string {
	f.emails = append(f.emails, email)
	return f.scopes
}

func (f *fakeAskRouter) SlackSessionState(_ context.Context, channelID, threadTS string) (bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := channelID + ":" + threadTS
	f.knownCalls = append(f.knownCalls, key)
	known := f.known[key]
	var ct time.Time
	if f.contextThrough != nil {
		ct = f.contextThrough[key]
	}
	return known, ct, nil
}

func (f *fakeAskRouter) LookupOrCreateSlackSession(_ context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, channelID+":"+threadTS)
	f.createEmails = append(f.createEmails, askerEmail)
	if f.succeedCreate {
		// Return a stable fake UUID so runAskTurn proceeds past the
		// session-lookup step.
		return uuid.MustParse("00000000-0000-0000-0000-000000000001"), nil
	}
	// Default: error short-circuits the goroutine before it touches
	// the (nil in tests) Slack api client. We assert on createCalls /
	// turnCalls rather than on the post.
	return uuid.Nil, errStubAsk
}

func (f *fakeAskRouter) ExecuteTurn(_ context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turnCalls = append(f.turnCalls, req.SessionID.String())
	f.questions = append(f.questions, req.UserInput)
	f.threadContexts = append(f.threadContexts, req.ThreadContext)
	f.contextThroughs = append(f.contextThroughs, req.ContextThrough)
	f.requests = append(f.requests, req)
	return ask.TurnResult{}, nil
}

var errStubAsk = errors.New("stub ask")

// waitForLen polls f.<field> length until it reaches `want` or the
// deadline fires. Routing decisions happen inline in handleEventsAPI;
// the goroutine that follows does so async so we need to give it a
// chance to land its mutations before asserting.
func waitForLen(t *testing.T, get func() int, want int, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: timed out waiting for len >= %d (got %d)", msg, want, get())
}

func TestAskRouting_NewThreadWithMention_RoutesToAsk(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> can you tell me about the BGP decision?",
		"ts": "1700000001.000100"
	}`
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"new thread with mention should reach LookupOrCreateSlackSession")
	router.mu.Lock()
	defer router.mu.Unlock()
	if got := router.knownCalls[0]; got != "C_OPS:1700000001.000100" {
		t.Errorf("known check key = %q", got)
	}
	if got := router.createCalls[0]; got != "C_OPS:1700000001.000100" {
		t.Errorf("create key = %q", got)
	}
}

func TestAskRouting_NewThreadNoMention_IngestsOnly(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "just a regular comment, no mention",
		"ts": "1700000002.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	// Routing decision is made synchronously; we ack immediately
	// after, so the assertion is reachable without a wait. Under
	// the mention-required-always rule the no-mention path returns
	// BEFORE the known-session lookup is consulted (the lookup only
	// drives downstream UX choices, not engagement), so both
	// knownCalls and createCalls stay at zero.
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 {
		t.Errorf("knownCalls = %d, want 0 (no-mention path should not consult known-session lookup)", len(router.knownCalls))
	}
	if len(router.createCalls) != 0 {
		t.Errorf("createCalls = %d, want 0 (no mention + no session = no ask)", len(router.createCalls))
	}
}

func TestAskRouting_KnownSessionNoMention_DoesNotRoute(t *testing.T) {
	// Engagement rule: every routed turn requires an explicit
	// @-mention, regardless of session state. Two humans
	// discussing the bot's prior reply should NOT trigger another
	// ask turn — that's the false-positive class this rule
	// eliminates. The session existence lookup still runs (it
	// drives downstream UX like the eyes-reaction skip), but it
	// doesn't gate routing.
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{
		"C_OPS:1700000003.000100": true,
	}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "follow-up without re-mention",
		"thread_ts": "1700000003.000100",
		"ts": "1700000003.000200"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	// Give the (non-existent) goroutine a chance to misfire if
	// it's going to. The mention check runs synchronously in
	// maybeRouteAsk, so by the time handleEventsAPI returns we
	// either bailed or spawned a goroutine — but that goroutine
	// would call createCalls. A short wait surfaces a regression.
	time.Sleep(50 * time.Millisecond)
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.createCalls) != 0 {
		t.Errorf("createCalls = %d, want 0 (no mention = no routing, even on known session)",
			len(router.createCalls))
	}
}

func TestAskRouting_KnownSessionWithMention_RoutesToAsk(t *testing.T) {
	// The complementary case: explicit @-mention in a known-session
	// thread continues the existing session (LookupOrCreateSlack-
	// Session reuses the session_id; createCalls fires once).
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{
		"C_OPS:1700000003.000100": true,
	}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> can you elaborate on the second point?",
		"thread_ts": "1700000003.000100",
		"ts": "1700000003.000200"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"explicit @-mention in known-session thread should still route as a follow-up")
}

func TestAskRouting_SelfMessage_DoesNotRoute(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{
		"C_OPS:1700000004.000100": true,
	}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_DIDEROT",
		"text": "this is the bot's own reply",
		"thread_ts": "1700000004.000100",
		"ts": "1700000004.000200"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 {
		t.Errorf("knownCalls = %d, want 0 (self-loop guard fires before lookup)", len(router.knownCalls))
	}
}

func TestAskRouting_DMRoutesWithoutMentionWithAudienceScopes(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	user := &slack.User{ID: "U_AVERY", Name: "avery"}
	user.Profile.Email = "Avery@Example.com"
	user.Profile.RealName = "Alex Rivera"
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{users: map[string]*slack.User{
		"U_AVERY": user,
	}})
	scopeResolver := &fakeDMScopeResolver{scopes: []string{
		events.ScopePublic,
		events.ScopeInternal,
		events.ScopeChannel("G_PRIVATE"),
	}}
	c.dmScopes = scopeResolver
	router := &fakeAskRouter{known: map[string]bool{}, succeedCreate: true}
	c.ask = router

	first := `{
		"type": "message",
		"channel": "D_AVERY",
		"user": "U_AVERY",
		"text": "what did i work on yesterday?",
		"ts": "1700000005.000100"
	}`
	second := `{
		"type": "message",
		"channel": "D_AVERY",
		"user": "U_AVERY",
		"text": "what about the deploy?",
		"ts": "1700000006.000100"
	}`
	followUp := `{
		"type": "message",
		"channel": "D_AVERY",
		"user": "U_AVERY",
		"text": "and what happened after that?",
		"thread_ts": "1700000005.000100",
		"ts": "1700000007.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, first))
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, second))
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, followUp))

	waitForLen(t, func() int {
		router.mu.Lock()
		defer router.mu.Unlock()
		return len(router.requests)
	}, 3, "two DM threads and a follow-up should execute three turns")

	router.mu.Lock()
	defer router.mu.Unlock()
	wantKeys := []string{
		"D_AVERY:1700000005.000100",
		"D_AVERY:1700000006.000100",
		"D_AVERY:1700000005.000100",
	}
	if !slices.Equal(router.knownCalls, wantKeys) {
		t.Errorf("known check keys = %v, want %v", router.knownCalls, wantKeys)
	}
	createKeys := slices.Clone(router.createCalls)
	slices.Sort(createKeys)
	slices.Sort(wantKeys)
	if !slices.Equal(createKeys, wantKeys) {
		t.Errorf("create keys = %v, want %v", createKeys, wantKeys)
	}
	for _, got := range router.createEmails {
		if got != "Avery@Example.com" {
			t.Errorf("session owner email = %q", got)
		}
	}
	wantScopes := []string{
		events.ScopePublic,
		events.ScopeInternal,
		events.ScopeChannel("G_PRIVATE"),
		events.ScopeOwner("avery@example.com"),
	}
	for _, req := range router.requests {
		if !slices.Equal(req.AllowedScopes, wantScopes) {
			t.Errorf("DM scopes = %v, want %v", req.AllowedScopes, wantScopes)
		}
		if req.Asker == nil || req.Asker.SlackUserID != "U_AVERY" {
			t.Errorf("DM asker = %#v", req.Asker)
		}
		if req.ThreadContext != "" {
			t.Errorf("DM thread context = %q, want persisted-session history only", req.ThreadContext)
		}
	}
	if !slices.Equal(scopeResolver.emails, []string{
		"avery@example.com", "avery@example.com", "avery@example.com",
	}) {
		t.Errorf("scope resolver emails = %v", scopeResolver.emails)
	}
	var ownerScoped int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE scope = $1`,
		events.ScopeOwner("avery@example.com"),
	).Scan(&ownerScoped); err != nil {
		t.Fatal(err)
	}
	if ownerScoped != 3 {
		t.Fatalf("owner-scoped DM events = %d, want 3", ownerScoped)
	}
}

func TestAskRouting_DMExternalUserFailsClosed(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{users: map[string]*slack.User{
		"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
	}})
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, `{
		"type": "message",
		"channel": "D_VENDOR",
		"user": "U_VENDOR",
		"text": "show me internal context",
		"ts": "1700000007.000100"
	}`))

	time.Sleep(50 * time.Millisecond)
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 || len(router.createCalls) != 0 {
		t.Fatalf("external DM routed: known=%v create=%v", router.knownCalls, router.createCalls)
	}
}

func TestAskRouting_DMUnresolvedIdentityFailsClosed(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, `{
		"type": "message",
		"channel": "D_UNKNOWN",
		"user": "U_UNKNOWN",
		"text": "show me internal context",
		"ts": "1700000008.000100"
	}`))

	time.Sleep(50 * time.Millisecond)
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 || len(router.createCalls) != 0 {
		t.Fatalf("unresolved DM routed: known=%v create=%v", router.knownCalls, router.createCalls)
	}
}

func TestAskRouting_EditInKnownSession_NoMention_DoesNotRoute(t *testing.T) {
	// An edit WITHOUT an @-mention must not route. The original ts is stale
	// (2023) so the staleness guard also fires — either condition alone is
	// sufficient to suppress routing. An edit that introduces a mention
	// does route; this case stays suppressed because there is no mention.
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{
		"C_OPS:1700000007.000100": true,
	}}
	c.ask = router

	raw := `{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C_OPS",
		"message": {
			"type": "message",
			"user": "U_AVERY",
			"text": "edited follow-up without mention",
			"thread_ts": "1700000007.000100",
			"ts": "1700000007.000200"
		},
		"previous_message": {
			"type": "message",
			"user": "U_AVERY",
			"text": "original follow-up",
			"thread_ts": "1700000007.000100",
			"ts": "1700000007.000200"
		},
		"ts": "1700000007.000300"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.createCalls) != 0 {
		t.Errorf("edit without mention must not trigger ask, got %d create calls", len(router.createCalls))
	}
}

// freshSlackTS returns a Slack-format ts string for a time offset from now.
// The offset is typically a small negative value (e.g. -5*time.Minute) to
// produce a "recent" message that passes the editRouteWindow staleness guard.
func freshSlackTS(offset time.Duration) string {
	t := time.Now().Add(offset)
	secs := t.Unix()
	micros := int64(t.Nanosecond()) / 1000
	return fmt.Sprintf("%d.%06d", secs, micros)
}

// editPayload builds a message_changed JSON payload with the given fields.
// origTS is the original message ts; editedTS is the edited.ts from Slack's
// edit sub-object; editText is the new body. channelID and threadTS control
// channel and thread anchoring. prevText is the before-edit body.
func editPayload(channelID, origTS, editedTS, editText, threadTS, prevText string) string {
	threadField := ""
	if threadTS != "" {
		threadField = fmt.Sprintf(`, "thread_ts": %q`, threadTS)
	}
	editedField := ""
	if editedTS != "" {
		editedField = fmt.Sprintf(`, "edited": {"user": "U_AVERY", "ts": %q}`, editedTS)
	}
	return fmt.Sprintf(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": %q,
		"message": {
			"type": "message",
			"user": "U_AVERY",
			"text": %q,
			"ts": %q%s%s
		},
		"previous_message": {
			"type": "message",
			"user": "U_AVERY",
			"text": %q,
			"ts": %q%s
		},
		"ts": %q
	}`, channelID, editText, origTS, threadField, editedField, prevText, origTS, threadField, editedTS)
}

// TestAskRouting_EditIntroducesMention_Routes verifies edit routing:
// a fresh message_changed edit that introduces an @-mention for the first time
// is routed through the ask flow with a composite claim key.
func TestAskRouting_EditIntroducesMention_Routes(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	origTS := freshSlackTS(-5 * time.Minute)    // fresh original message
	editedTS := freshSlackTS(-30 * time.Second) // the edit happened 30s ago

	raw := editPayload("C_EDIT", origTS, editedTS,
		"<@U_DIDEROT> what's the auth error about?",
		"", // no thread_ts → top-level message
		"what's the auth error about?")

	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	waitForLen(t,
		func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) },
		1, "edit introducing mention should route to ask")

	// Verify the composite claim key is in the DB.
	wantClaimKey := origTS + ":" + editedTS
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ask_turn_claims WHERE channel_id=$1 AND message_ts=$2`,
		"C_EDIT", wantClaimKey,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("composite claim key %q not found in ask_turn_claims (n=%d)", wantClaimKey, n)
	}
}

// TestAskRouting_EditOnAlreadyAnswered_Routes verifies that an edit introducing
// a mention on a message whose original ts was already answered (has a claim
// row for origTS) still routes: the composite claim key is novel.
func TestAskRouting_EditOnAlreadyAnswered_Routes(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	origTS := freshSlackTS(-10 * time.Minute)   // fresh original message
	editedTS := freshSlackTS(-60 * time.Second) // edit was 60s ago

	// Pre-insert a claim for the ORIGINAL ts (simulates the bot having
	// already answered the original question).
	if _, err := pool.Exec(ctx,
		`INSERT INTO ask_turn_claims (channel_id, message_ts) VALUES ($1, $2)`,
		"C_EDIT2", origTS,
	); err != nil {
		t.Fatalf("pre-insert original claim: %v", err)
	}

	raw := editPayload("C_EDIT2", origTS, editedTS,
		"<@U_DIDEROT> actually, can you look at this specific error?",
		"", "", // no thread, empty prevText
	)

	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))

	waitForLen(t,
		func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) },
		1, "edit on already-answered message should still route with composite key")

	// Composite key should exist; original plain key also still there.
	compositeKey := origTS + ":" + editedTS
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ask_turn_claims WHERE channel_id=$1 AND message_ts=$2`,
		"C_EDIT2", compositeKey,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("composite claim key %q not found in ask_turn_claims", compositeKey)
	}
}

// TestAskRouting_EditRedelivery_Suppressed verifies that two deliveries of the
// SAME edit (same origTS + editedTS) deduplicate via the composite claim key.
func TestAskRouting_EditRedelivery_Suppressed(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	origTS := freshSlackTS(-3 * time.Minute)
	editedTS := freshSlackTS(-20 * time.Second)

	raw := editPayload("C_EDIT3", origTS, editedTS,
		"<@U_DIDEROT> same edit delivered twice",
		"", "original without mention")

	// First delivery: claim succeeds, turn goroutine spawns.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	waitForLen(t,
		func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) },
		1, "first edit delivery should reach LookupOrCreateSlackSession")

	// Second delivery of the SAME edit: composite claim conflicts → suppressed.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	time.Sleep(100 * time.Millisecond)

	router.mu.Lock()
	defer router.mu.Unlock()
	if got := len(router.createCalls); got != 1 {
		t.Errorf("createCalls=%d, want 1 — same-edit redelivery must be suppressed", got)
	}
}

// TestAskRouting_EditTooOld_DoesNotRoute verifies the staleness guard: an edit
// whose original message ts is older than editRouteWindow is silently dropped.
func TestAskRouting_EditTooOld_DoesNotRoute(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	// origTS is 2h ago — past the 1h editRouteWindow.
	origTS := freshSlackTS(-2 * time.Hour)
	editedTS := freshSlackTS(-10 * time.Second)

	raw := editPayload("C_EDIT4", origTS, editedTS,
		"<@U_DIDEROT> editing an old message with a mention",
		"", "old message text")

	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	time.Sleep(50 * time.Millisecond)

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.createCalls) != 0 {
		t.Errorf("createCalls=%d, want 0 — stale edit must not route", len(router.createCalls))
	}
}

func TestAskRouting_NoAsk_NoOp(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	// c.ask = nil — the WithAsk-not-called case.

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> hi",
		"ts": "1700000006.000100"
	}`
	// Should not panic, not block, just ingest.
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))
}

// TestAskRouting_ExternalAsker_Blocked verifies the per-asker gate:
// a Slack Connect "stranger" gets silently ignored even if they
// @-mention the bot. No router calls, no panic.
func TestAskRouting_ExternalAsker_Blocked(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_VENDOR",
		"text": "<@U_DIDEROT> what's the BGP status?",
		"ts": "1700000010.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 {
		t.Errorf("external asker should be blocked before session lookup, got %d known calls",
			len(router.knownCalls))
	}
	if len(router.createCalls) != 0 {
		t.Errorf("external asker should NOT reach LookupOrCreateSlackSession, got %d", len(router.createCalls))
	}
}

// TestAskRouting_ExternalChannel_Blocked verifies the per-channel
// gate: an internal asker in a channel containing externals,
// without allow_external_ask, gets blocked. The asker-facing
// ephemeral is fired as a goroutine; we don't assert on it here —
// c.api is nil so postGateEphemeral short-circuits anyway.
func TestAskRouting_ExternalChannel_Blocked(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY":  {ID: "U_AVERY", Name: "avery"},
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED": {"U_AVERY", "U_VENDOR"},
		}}, nil)
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_MIXED",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> hello",
		"ts": "1700000011.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.createCalls) != 0 {
		t.Errorf("blocked channel should not reach LookupOrCreateSlackSession, got %d", len(router.createCalls))
	}
}

// TestAskRouting_ExternalChannel_AllowedRoutes verifies that
// flipping allow_external_ask=true lets internal askers through
// even in a channel with external members.
func TestAskRouting_ExternalChannel_AllowedRoutes(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY":  {ID: "U_AVERY", Name: "avery"},
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	store := NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED_OPTED": {"U_AVERY", "U_VENDOR"},
		}}, nil)
	if err := store.SetAllowExternalAsk(context.Background(), "C_MIXED_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	// AllowExternalAsk allows external-membered routing; AskEnabled
	// is the orthogonal gate that says "this channel is opted into
	// engagement at all." Both required for the bot to answer.
	if err := store.SetAskEnabled(context.Background(), "C_MIXED_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	c.channelConfig = store
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_MIXED_OPTED",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> hello",
		"ts": "1700000012.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"opted-in channel should route normally even with external members")
}

// TestConsumerDedupe_RedeliveredEventDropped verifies that when
// the consumer receives the same (channel, ts) twice, the second
// delivery is dropped without re-running the upsert/ask path.
// Without the dedupe, Slack ack-timeout retries produce duplicate ask
// answers.
func TestConsumerDedupe_RedeliveredEventDropped(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.seen = newSeenStore(time.Minute)
	ctx := context.Background()

	raw := `{
		"type": "message",
		"channel": "C_DEDUPE",
		"user": "U_AVERY",
		"text": "first delivery",
		"ts": "1700000600.000100"
	}`
	// First delivery → fresh, ingests normally. Redeliveries (retry
	// attempts 1, 2) hit the dedupe gate and are dropped. The gate is
	// now a pure duplicate-work guard — the ack already fired in the
	// event loop before the handler ran — so the observable
	// contract is simply "one row, not three".
	c.handleEventsAPIWithRetry(ctx, wrapMessageEvent(t, raw), 0, "")
	c.handleEventsAPIWithRetry(ctx, wrapMessageEvent(t, raw), 1, "timeout")
	c.handleEventsAPIWithRetry(ctx, wrapMessageEvent(t, raw), 2, "timeout")

	// Should be exactly ONE row in events despite three deliveries.
	var numEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source='slack' AND raw->>'channel'='C_DEDUPE'`).Scan(&numEvents); err != nil {
		t.Fatal(err)
	}
	if numEvents != 1 {
		t.Errorf("events count=%d, want 1 (dedupe should suppress redeliveries)", numEvents)
	}
}

// TestConsumerDedupe_DifferentTSStillIngests covers the
// negative case: two distinct messages in the same channel
// (different ts values) both ingest, even when the second
// one is delivered with retry_attempt > 0 (a tail-of-the-
// queue redelivery from Slack's "Delayed Events" replay).
func TestConsumerDedupe_DifferentTSStillIngests(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.seen = newSeenStore(time.Minute)
	ctx := context.Background()

	raw1 := `{
		"type": "message",
		"channel": "C_DEDUPE2",
		"user": "U_AVERY",
		"text": "first message",
		"ts": "1700000600.000001"
	}`
	raw2 := `{
		"type": "message",
		"channel": "C_DEDUPE2",
		"user": "U_AVERY",
		"text": "second message",
		"ts": "1700000610.000002"
	}`
	c.handleEventsAPIWithRetry(ctx, wrapMessageEvent(t, raw1), 0, "")
	// Second message arrives with retry_attempt=1 (a redelivery
	// of a *different* message — they're independent).
	c.handleEventsAPIWithRetry(ctx, wrapMessageEvent(t, raw2), 1, "timeout")

	var numEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source='slack' AND raw->>'channel'='C_DEDUPE2'`).Scan(&numEvents); err != nil {
		t.Fatal(err)
	}
	if numEvents != 2 {
		t.Errorf("events count=%d, want 2 (different ts → both should ingest)", numEvents)
	}
}

// =====================================================================
// Ask-turn claim (durable dedupe)
// =====================================================================

// TestAskTurnClaim_ClaimOnce verifies the raw DB contract: claiming
// (channel, ts) for the first time returns RowsAffected==1; claiming
// the same (channel, ts) a second time returns 0 (ON CONFLICT DO
// NOTHING).
func TestAskTurnClaim_ClaimOnce(t *testing.T) {
	pool := getConsumerPool(t)
	ctx := context.Background()

	tag, err := pool.Exec(ctx,
		`INSERT INTO ask_turn_claims (channel_id, message_ts) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		"C_CLAIM", "1700000001.000100",
	)
	if err != nil {
		t.Fatalf("first claim insert: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("first claim: RowsAffected=%d, want 1", tag.RowsAffected())
	}

	tag, err = pool.Exec(ctx,
		`INSERT INTO ask_turn_claims (channel_id, message_ts) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		"C_CLAIM", "1700000001.000100",
	)
	if err != nil {
		t.Fatalf("second claim insert: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("second claim: RowsAffected=%d, want 0 (duplicate suppressed)", tag.RowsAffected())
	}
}

// TestAskRouting_DuplicateClaim_DoesNotRouteSecondDelivery verifies
// the end-to-end route-level behaviour: when the same (channel,
// message_ts) is delivered twice, the second delivery does not spawn
// a second ask turn. The seenStore is left nil so the claim is the
// sole dedupe gate for this test — this is the scenario that matters
// for Delayed Events redelivery, which arrives after the seenStore
// TTL has expired (or on a pod that never saw the original event).
//
// The test uses fakeAskRouter's LookupOrCreateSlackSession error to
// short-circuit runAskTurn before any Slack API calls, and asserts
// that createCalls lands exactly once despite two deliveries.
func TestAskRouting_DuplicateClaim_DoesNotRouteSecondDelivery(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	ctx := context.Background()

	raw := `{
		"type": "message",
		"channel": "C_CLAIM_ROUTE",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> tell me about the BGP decision",
		"ts": "1700000020.000100"
	}`

	// First delivery: claim succeeds, turn goroutine spawns.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"first delivery should reach LookupOrCreateSlackSession")

	// Second delivery of the same (channel, ts): claim insert returns
	// 0 rows (ON CONFLICT DO NOTHING); maybeRouteAsk returns without
	// spawning a goroutine. We wait briefly then assert no extra call.
	c.handleEventsAPI(ctx, wrapMessageEvent(t, raw))
	time.Sleep(100 * time.Millisecond)

	router.mu.Lock()
	defer router.mu.Unlock()
	if got := len(router.createCalls); got != 1 {
		t.Errorf("createCalls=%d, want 1 — duplicate delivery must not spawn a second turn", got)
	}
}

// TestAskRouting_AppRelayedHumanMention covers the two messages that are
// identical at the bot_id field and must be treated oppositely: a person
// @-mentioning the bot through an app's user token, and a bot @-mentioning it
// under its own token. Slack stamps a bot_id on both, so routing has to decide
// from who `user` actually is.
func TestAskRouting_AppRelayedHumanMention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		user      string
		wantRoute bool
	}{
		{name: "human via app user token", user: "U_AVERY", wantRoute: true},
		{name: "bot under its own token", user: "U_OTHERBOT", wantRoute: false},
		{name: "diderot's own reply", user: "U_DIDEROT", wantRoute: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := getConsumerPool(t)
			c := newTestConsumer(t, pool)
			c.selfUserID.Store("U_DIDEROT")
			c.dir = newDirectoryWithAPI(pool, &fakeAPI{
				users: map[string]*slack.User{
					"U_AVERY":    {ID: "U_AVERY", Name: "avery"},
					"U_OTHERBOT": {ID: "U_OTHERBOT", Name: "pagerduty", IsBot: true},
					"U_DIDEROT":  {ID: "U_DIDEROT", Name: "diderot", IsBot: true},
				},
			})
			router := &fakeAskRouter{known: map[string]bool{}}
			c.ask = router

			// bot_id + bot_profile is exactly what Slack attaches to a
			// chat.postMessage made with a user token.
			raw := `{
				"type": "message",
				"channel": "C_OPS",
				"user": "` + tc.user + `",
				"bot_id": "B_RELAY",
				"app_id": "A_RELAY",
				"bot_profile": {"id": "B_RELAY", "name": "relay"},
				"text": "<@U_DIDEROT> what broke in the uploader?",
				"ts": "1700000020.000100"
			}`
			c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

			router.mu.Lock()
			defer router.mu.Unlock()
			routed := len(router.knownCalls) > 0
			if routed != tc.wantRoute {
				t.Errorf("routed = %v, want %v (known calls: %d)", routed, tc.wantRoute, len(router.knownCalls))
			}
		})
	}
}

// The DB's human-only user_id contract is unchanged by the routing fix: an
// app-relayed message still records no human author in the side table.
func TestDetailsFromStillTreatsBotIDAsBotAuthored(t *testing.T) {
	d, err := DetailsFrom([]byte(`{
		"type":"message","channel":"C_OPS","user":"U_AVERY","bot_id":"B_RELAY",
		"text":"hi","ts":"1700000030.000100"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.UserID != "" {
		t.Errorf("DetailsFrom UserID = %q, want empty (bot_id keeps the side table human-only)", d.UserID)
	}
	if got := rawMessageUser([]byte(`{"user":"U_AVERY"}`)); got != "U_AVERY" {
		t.Errorf("rawMessageUser = %q, want U_AVERY", got)
	}
	if got := rawMessageUser([]byte(`{"message":{"user":"U_EDIT"}}`)); got != "U_EDIT" {
		t.Errorf("rawMessageUser (edit) = %q, want U_EDIT", got)
	}
	if got := rawMessageUser([]byte(`not json`)); got != "" {
		t.Errorf("rawMessageUser (garbage) = %q, want empty", got)
	}
}
