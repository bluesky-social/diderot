package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/bluesky-social/diderot/internal/ask"
)

// truncateFeedbackTables resets ask_sessions (and cascades to
// ask_answer_messages + ask_feedback). Called at the top of each
// feedback test after getConsumerPool (which truncates events +
// slack_message_details).
func truncateFeedbackTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`TRUNCATE ask_sessions, ask_answer_messages, ask_feedback RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate feedback tables: %v", err)
	}
}

// seedSession inserts a minimal ask_sessions row and returns its UUID
// string. Provides FK-valid session references for feedback tests.
func seedSession(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO ask_sessions DEFAULT VALUES RETURNING id::text`,
	).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func wrapReactionAddedEvent(emoji, userID, channelID, msgTS string) slackevents.EventsAPIEvent {
	ev := &slackevents.ReactionAddedEvent{
		Type:     "reaction_added",
		User:     userID,
		Reaction: emoji,
		Item: slackevents.Item{
			Type:      "message",
			Channel:   channelID,
			Timestamp: msgTS,
		},
	}
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: ev,
		},
	}
}

func wrapReactionRemovedEvent(emoji, userID, channelID, msgTS string) slackevents.EventsAPIEvent {
	ev := &slackevents.ReactionRemovedEvent{
		Type:     "reaction_removed",
		User:     userID,
		Reaction: emoji,
		Item: slackevents.Item{
			Type:      "message",
			Channel:   channelID,
			Timestamp: msgTS,
		},
	}
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: ev,
		},
	}
}

func feedbackButtonCallback(value, userID, channelID, msgTS string) slackgo.InteractionCallback {
	return slackgo.InteractionCallback{
		Type:      slackgo.InteractionTypeBlockActions,
		User:      slackgo.User{ID: userID},
		Container: slackgo.Container{ChannelID: channelID, MessageTs: msgTS},
		ActionCallback: slackgo.ActionCallbacks{
			BlockActions: []*slackgo.BlockAction{{ActionID: feedbackActionID, Value: value}},
		},
	}
}

func TestPostAskAnswerUsesNativeFeedbackButtons(t *testing.T) {
	forms := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		forms <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":"1700000000.000100"}`))
	}))
	t.Cleanup(srv.Close)

	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}
	const answer = "A useful answer."
	if err := c.postAskAnswer(context.Background(), "C_TEST", "1699999999.000100", "", 0, answer, ""); err != nil {
		t.Fatalf("postAskAnswer: %v", err)
	}
	form := <-forms
	if got := form.Get("text"); got != answer {
		t.Errorf("fallback text = %q, want %q", got, answer)
	}
	if got := form.Get("mrkdwn"); got != "false" {
		t.Errorf("fallback mrkdwn = %q, want false", got)
	}

	var blocks []map[string]any
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	if len(blocks) != 2 || blocks[0]["type"] != "markdown" || blocks[1]["type"] != "context_actions" {
		t.Fatalf("blocks = %#v", blocks)
	}
	elements, ok := blocks[1]["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("feedback elements = %#v", blocks[1]["elements"])
	}
	feedback, ok := elements[0].(map[string]any)
	if !ok || feedback["type"] != "feedback_buttons" || feedback["action_id"] != feedbackActionID {
		t.Fatalf("feedback element = %#v", elements[0])
	}
	positive := feedback["positive_button"].(map[string]any)
	positiveText := positive["text"].(map[string]any)
	negative := feedback["negative_button"].(map[string]any)
	negativeText := negative["text"].(map[string]any)
	if positiveText["text"] != "Mark this answer as helpful" || negativeText["text"] != "Mark this answer as not helpful" {
		t.Errorf("feedback button labels = positive %#v, negative %#v", positiveText, negativeText)
	}
}

func TestPostAskAnswerAddsPreciseIncompleteNoticeBeforeChunking(t *testing.T) {
	forms := make(chan url.Values, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		forms <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":"1700000000.000100"}`))
	}))
	t.Cleanup(srv.Close)

	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}
	answer := strings.Repeat("a", slackPostChunkBytes-10)
	if err := c.postAskAnswer(context.Background(), "C_TEST", "1699999999.000100", "", 0, answer, ask.IncompleteMaxTokens); err != nil {
		t.Fatalf("postAskAnswer: %v", err)
	}

	first, second := <-forms, <-forms
	for i, form := range []url.Values{first, second} {
		if got := len(form.Get("text")); got > slackPostChunkBytes {
			t.Errorf("chunk %d length = %d, exceeds %d", i, got, slackPostChunkBytes)
		}
		if got := form.Get("mrkdwn"); got != "false" {
			t.Errorf("chunk %d fallback mrkdwn = %q, want false", i, got)
		}
		var blocks []map[string]any
		if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
			t.Fatalf("decode chunk %d blocks: %v", i, err)
		}
		if len(blocks) == 0 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != form.Get("text") {
			t.Errorf("chunk %d does not use its Markdown block as the authoritative body: %#v", i, blocks)
		}
	}
	if strings.Contains(first.Get("text"), "output limit") {
		t.Error("incomplete notice landed before the final chunk")
	}
	wantNotice := "_(" + ask.IncompleteNotice(ask.IncompleteMaxTokens) + ")_"
	if !strings.HasSuffix(second.Get("text"), wantNotice) {
		t.Errorf("final chunk = %q, want suffix %q", second.Get("text"), wantNotice)
	}
	if strings.Contains(second.Get("text"), "response was cut off") {
		t.Error("legacy generic cut-off marker still present")
	}
}

func TestPostAskAnswerPreservesApproximationTildesInMarkdownBlock(t *testing.T) {
	forms := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		forms <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":"1700000000.000100"}`))
	}))
	t.Cleanup(srv.Close)

	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}
	const answer = "Estimated saving: **~$1k/month**; the fleet represented ~50% of spend."
	if err := c.postAskAnswer(context.Background(), "C_TEST", "1699999999.000100", "", 0, answer, ""); err != nil {
		t.Fatalf("postAskAnswer: %v", err)
	}

	form := <-forms
	if got := form.Get("text"); got != answer {
		t.Fatalf("fallback text = %q, want approximation markers preserved", got)
	}
	if got := form.Get("mrkdwn"); got != "false" {
		t.Fatalf("fallback mrkdwn = %q, want false", got)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	if len(blocks) != 2 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != answer {
		t.Fatalf("blocks = %#v", blocks)
	}
}

// --- verdict mapping ---

func TestVerdictForEmoji(t *testing.T) {
	cases := []struct {
		emoji string
		want  string
	}{
		{"thumbsup", "up"},
		{"+1", "up"},
		// A skin-tone variant is the same thumb, its tone a suffix.
		{"+1::skin-tone-3", "up"},
		{"thumbsup::skin-tone-6", "up"},
		{"-1::skin-tone-2", "down"},
		{"thumbsdown::skin-tone-5", "down"},
		// A custom emoji named like a thumb is not one.
		{"thumbsup_bug", "other"},
		{"thumbsup_bug::skin-tone-2", "other"},
		{"+1::other", "other"},
		{"thumbsdown", "down"},
		{"-1", "down"},
		{"thumbsdown_later", "other"}, // custom emoji, not a vote
		{"tada", "other"},
		{"white_check_mark", "other"},
		{"eyes", "other"},
		{"", "other"},
	}
	for _, tc := range cases {
		got := verdictForEmoji(tc.emoji)
		if got != tc.want {
			t.Errorf("verdictForEmoji(%q) = %q, want %q", tc.emoji, got, tc.want)
		}
	}
}

// --- insertAnswerMessage + lookupAnswerMessage ---

func TestInsertAnswerMessage_PersistedCorrectly(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)

	const channelID = "C_TEST"
	const msgTS = "1700000010.000100"
	const turn = 3
	const chunkIndex = 1

	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, turn, chunkIndex); err != nil {
		t.Fatalf("insertAnswerMessage: %v", err)
	}

	gotSession, gotTurn, found, err := lookupAnswerMessage(ctx, pool, channelID, msgTS)
	if err != nil {
		t.Fatalf("lookupAnswerMessage: %v", err)
	}
	if !found {
		t.Fatal("lookupAnswerMessage: row not found after insert")
	}
	if gotSession != sessionID {
		t.Errorf("session_id = %q, want %q", gotSession, sessionID)
	}
	if gotTurn != turn {
		t.Errorf("turn = %d, want %d", gotTurn, turn)
	}

	var gotChunk int
	if err := pool.QueryRow(ctx,
		`SELECT chunk_index FROM ask_answer_messages WHERE channel_id = $1 AND message_ts = $2`,
		channelID, msgTS,
	).Scan(&gotChunk); err != nil {
		t.Fatalf("query chunk_index: %v", err)
	}
	if gotChunk != chunkIndex {
		t.Errorf("chunk_index = %d, want %d", gotChunk, chunkIndex)
	}
}

// --- reaction event handler tests ---

// TestReactionAdded_RegisteredMessage_UpsertsFeedback verifies that a
// thumbsup on a known answer message inserts an ask_feedback row.
func TestReactionAdded_RegisteredMessage_UpsertsFeedback(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000001.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 2, 0); err != nil {
		t.Fatalf("insertAnswerMessage: %v", err)
	}

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")

	c.handleEventsAPI(ctx, wrapReactionAddedEvent("thumbsup", "U_HUMAN", channelID, msgTS))

	var verdict string
	var removedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT verdict, removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 2 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = 'thumbsup'`,
		sessionID, msgTS,
	).Scan(&verdict, &removedAt); err != nil {
		t.Fatalf("query ask_feedback: %v", err)
	}
	if verdict != "up" {
		t.Errorf("verdict = %q, want 'up'", verdict)
	}
	if removedAt != nil {
		t.Errorf("removed_at should be NULL, got %v", removedAt)
	}
}

// TestReactionAdded_UnknownMessage_NoRow verifies that a reaction on a
// non-answer message acks and inserts no feedback row.
func TestReactionAdded_UnknownMessage_NoRow(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	c := newTestConsumer(t, pool)

	c.handleEventsAPI(ctx, wrapReactionAddedEvent("thumbsup", "U_HUMAN", "C_TEST", "1700000099.000000"))

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_feedback`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ask_feedback rows = %d, want 0 (unknown message)", n)
	}
}

// TestReactionAdded_BotSelf_Ignored verifies that a reaction from the
// bot itself is never recorded.
func TestReactionAdded_BotSelf_Ignored(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	if err := insertAnswerMessage(ctx, pool, "C_TEST", "1700000001.000100", sessionID, 1, 0); err != nil {
		t.Fatal(err)
	}

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")

	c.handleEventsAPI(ctx, wrapReactionAddedEvent("white_check_mark", "U_DIDEROT", "C_TEST", "1700000001.000100"))

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_feedback`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ask_feedback rows = %d, want 0 (bot self-reaction ignored)", n)
	}
}

// TestReactionRemoved_SetsRemovedAt verifies that reaction_removed
// stamps removed_at on the matching ask_feedback row.
func TestReactionRemoved_SetsRemovedAt(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000002.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 1, 0); err != nil {
		t.Fatal(err)
	}

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")

	c.handleEventsAPI(ctx, wrapReactionAddedEvent("thumbsup", "U_HUMAN", channelID, msgTS))

	c.handleEventsAPI(ctx, wrapReactionRemovedEvent("thumbsup", "U_HUMAN", channelID, msgTS))

	var removedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 1 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = 'thumbsup'`,
		sessionID, msgTS,
	).Scan(&removedAt); err != nil {
		t.Fatalf("query ask_feedback removed_at: %v", err)
	}
	if removedAt == nil {
		t.Error("removed_at should be set after reaction_removed, got NULL")
	}
}

func TestFeedbackButtonClickPersistsAndUpdatesVote(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000002.000200"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 1, 0); err != nil {
		t.Fatal(err)
	}

	c := newTestConsumer(t, pool)
	c.handleInteractive(ctx, feedbackButtonCallback(feedbackPositiveValue, "U_HUMAN", channelID, msgTS))
	c.handleInteractive(ctx, feedbackButtonCallback(feedbackNegativeValue, "U_HUMAN", channelID, msgTS))

	var verdict, rowKey string
	var removedAt *time.Time
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT min(verdict), min(emoji), min(removed_at), count(*)
		  FROM ask_feedback
		 WHERE session_id = $1::uuid AND turn = 1 AND message_ts = $2
		   AND reactor = 'U_HUMAN'
	`, sessionID, msgTS).Scan(&verdict, &rowKey, &removedAt, &count); err != nil {
		t.Fatalf("query button feedback: %v", err)
	}
	if count != 1 || verdict != "down" || rowKey != feedbackButtonsRowKey || removedAt != nil {
		t.Errorf("button feedback = count %d, verdict %q, key %q, removed_at %v", count, verdict, rowKey, removedAt)
	}
}

// TestReactionAdded_ReAdd_ClearsRemovedAt verifies that re-adding a
// previously retracted reaction clears removed_at.
func TestReactionAdded_ReAdd_ClearsRemovedAt(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000003.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")

	// add → remove → re-add
	c.handleEventsAPI(ctx, wrapReactionAddedEvent("+1", "U_HUMAN", channelID, msgTS))
	c.handleEventsAPI(ctx, wrapReactionRemovedEvent("+1", "U_HUMAN", channelID, msgTS))
	c.handleEventsAPI(ctx, wrapReactionAddedEvent("+1", "U_HUMAN", channelID, msgTS))

	var removedAt *time.Time
	var verdict string
	if err := pool.QueryRow(ctx,
		`SELECT verdict, removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = '+1'`,
		sessionID, msgTS,
	).Scan(&verdict, &removedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if verdict != "up" {
		t.Errorf("verdict = %q, want 'up'", verdict)
	}
	if removedAt != nil {
		t.Errorf("removed_at should be NULL after re-add, got %v", removedAt)
	}
}

// --- fakeReactionsAPI for sweep tests ---

// fakeReactionsAPI implements reactionsAPI with canned per-message
// reaction data.
type fakeReactionsAPI struct {
	// reactions maps "channelID:messageTS" → []ItemReaction.
	// A missing key returns an empty list (no reactions).
	reactions map[string][]slackgo.ItemReaction
}

func (f *fakeReactionsAPI) GetReactionsContext(_ context.Context, item slackgo.ItemRef, _ slackgo.GetReactionsParameters) (slackgo.ReactedItem, error) {
	key := item.Channel + ":" + item.Timestamp
	return slackgo.ReactedItem{Reactions: f.reactions[key]}, nil
}

// newFakeReactions builds a fakeReactionsAPI keyed by channel:ts.
func newFakeReactions(byMsg map[string][]slackgo.ItemReaction) *fakeReactionsAPI {
	return &fakeReactionsAPI{reactions: byMsg}
}

// --- sweep reconcile tests ---

// TestSweepFeedback_PresentNotInDB_Inserted verifies that a reaction
// that exists on a live Slack message but has no ask_feedback row
// gets inserted by the sweep.
func TestSweepFeedback_PresentNotInDB_Inserted(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000020.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Live Slack has a thumbsup from U_HUMAN on this message.
	fake := newFakeReactions(map[string][]slackgo.ItemReaction{
		channelID + ":" + msgTS: {
			{Name: "thumbsup", Users: []string{"U_HUMAN"}},
		},
	})

	c := newTestConsumer(t, pool)
	c.reactions = fake

	if err := c.SweepFeedback(ctx); err != nil {
		t.Fatalf("SweepFeedback: %v", err)
	}

	var verdict string
	var removedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT verdict, removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = 'thumbsup'`,
		sessionID, msgTS,
	).Scan(&verdict, &removedAt); err != nil {
		t.Fatalf("query after sweep: %v", err)
	}
	if verdict != "up" {
		t.Errorf("verdict = %q, want 'up'", verdict)
	}
	if removedAt != nil {
		t.Errorf("removed_at should be NULL for live reaction, got %v", removedAt)
	}
}

// TestSweepFeedback_DBRowGoneFromLive_SetsRemovedAt verifies that a
// reaction that was in the DB but is no longer live on the Slack
// message gets its removed_at stamped by the sweep.
func TestSweepFeedback_DBRowGoneFromLive_SetsRemovedAt(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000021.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Seed a feedback row (simulating a reaction that was captured
	// via the live event path and later retracted while disconnected).
	if err := upsertFeedback(ctx, pool, sessionID, 0, channelID, msgTS, "U_HUMAN", "thumbsup", "up"); err != nil {
		t.Fatal(err)
	}

	// Live Slack has NO reactions on this message.
	fake := newFakeReactions(map[string][]slackgo.ItemReaction{})

	c := newTestConsumer(t, pool)
	c.reactions = fake

	if err := c.SweepFeedback(ctx); err != nil {
		t.Fatalf("SweepFeedback: %v", err)
	}

	var removedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = 'thumbsup'`,
		sessionID, msgTS,
	).Scan(&removedAt); err != nil {
		t.Fatalf("query after sweep: %v", err)
	}
	if removedAt == nil {
		t.Error("removed_at should be stamped by sweep, got NULL")
	}
}

func TestSweepFeedback_DoesNotRemoveButtonVote(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000021.000200"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := upsertFeedback(ctx, pool, sessionID, 0, channelID, msgTS, "U_HUMAN", feedbackButtonsRowKey, "up"); err != nil {
		t.Fatal(err)
	}

	c := newTestConsumer(t, pool)
	c.reactions = newFakeReactions(map[string][]slackgo.ItemReaction{})
	if err := c.SweepFeedback(ctx); err != nil {
		t.Fatalf("SweepFeedback: %v", err)
	}

	var removedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT removed_at FROM ask_feedback
		 WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2
		   AND reactor = 'U_HUMAN' AND emoji = $3
	`, sessionID, msgTS, feedbackButtonsRowKey).Scan(&removedAt); err != nil {
		t.Fatalf("query button feedback after sweep: %v", err)
	}
	if removedAt != nil {
		t.Errorf("button feedback was removed by reaction sweep: %v", removedAt)
	}
}

// TestSweepFeedback_ChunkInversion_NotStamped is the regression test
// for the inversion bug: a multi-chunk answer where U_HUMAN reacted
// on chunk 1 (ts=ts1) only. Sweeping chunk 2 (ts=ts2) which has no
// reactions must NOT stamp chunk 1's feedback row removed.
func TestSweepFeedback_ChunkInversion_NotStamped(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const ts1 = "1700000030.000100" // chunk 0
	const ts2 = "1700000030.000200" // chunk 1

	// Register both chunks as answer messages for the same session+turn.
	if err := insertAnswerMessage(ctx, pool, channelID, ts1, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := insertAnswerMessage(ctx, pool, channelID, ts2, sessionID, 0, 1); err != nil {
		t.Fatal(err)
	}

	// Live Slack: chunk 1 (ts1) has a thumbsup; chunk 2 (ts2) has none.
	fake := newFakeReactions(map[string][]slackgo.ItemReaction{
		channelID + ":" + ts1: {
			{Name: "thumbsup", Users: []string{"U_HUMAN"}},
		},
		// ts2 key absent → empty list returned by fakeReactionsAPI
	})

	c := newTestConsumer(t, pool)
	c.reactions = fake

	if err := c.SweepFeedback(ctx); err != nil {
		t.Fatalf("SweepFeedback: %v", err)
	}

	// chunk 1's feedback row must exist and NOT have removed_at set.
	var removedAt *time.Time
	var verdict string
	if err := pool.QueryRow(ctx,
		`SELECT verdict, removed_at FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2
		    AND reactor = 'U_HUMAN' AND emoji = 'thumbsup'`,
		sessionID, ts1,
	).Scan(&verdict, &removedAt); err != nil {
		t.Fatalf("query chunk 1 feedback after sweep: %v", err)
	}
	if verdict != "up" {
		t.Errorf("verdict = %q, want 'up'", verdict)
	}
	if removedAt != nil {
		t.Errorf("chunk 1 feedback incorrectly stamped removed_at by chunk 2 sweep: %v", removedAt)
	}

	// chunk 2 must have no feedback rows at all (no reactions there).
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ask_feedback
		  WHERE session_id = $1::uuid AND turn = 0 AND message_ts = $2`,
		sessionID, ts2,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("chunk 2 feedback rows = %d, want 0 (no reactions on chunk 2)", n)
	}
}

// TestSweepFeedback_KeepsALiveReactionsTime verifies that the hourly sweep,
// which sees every live reaction again for a week, leaves the time the
// reaction was given alone, while a re-add after a retract and a changed
// button vote are new ratings with new times.
func TestSweepFeedback_KeepsALiveReactionsTime(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000040.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.handleEventsAPI(ctx, wrapReactionAddedEvent("+1", "U_HUMAN", channelID, msgTS))
	c.handleFeedbackButtonClick(ctx, feedbackButtonCallback(feedbackPositiveValue, "U_HUMAN", channelID, msgTS), feedbackPositiveValue)
	// Both were given a day ago.
	if _, err := pool.Exec(ctx, `UPDATE ask_feedback SET added_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	addedAt := func(emoji string) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT added_at FROM ask_feedback WHERE message_ts = $1 AND reactor = 'U_HUMAN' AND emoji = $2`,
			msgTS, emoji).Scan(&at); err != nil {
			t.Fatalf("added_at %s: %v", emoji, err)
		}
		return at
	}
	given := addedAt("+1")

	c.reactions = newFakeReactions(map[string][]slackgo.ItemReaction{
		channelID + ":" + msgTS: {{Name: "+1", Users: []string{"U_HUMAN"}}},
	})
	for range 2 {
		if err := c.SweepFeedback(ctx); err != nil {
			t.Fatalf("SweepFeedback: %v", err)
		}
	}
	// A redelivered reaction_added is the same reaction, too.
	c.handleEventsAPI(ctx, wrapReactionAddedEvent("+1", "U_HUMAN", channelID, msgTS))
	if got := addedAt("+1"); !got.Equal(given) {
		t.Errorf("live reaction's added_at moved from %v to %v", given, got)
	}

	// Taken back and given again: a new rating.
	c.handleEventsAPI(ctx, wrapReactionRemovedEvent("+1", "U_HUMAN", channelID, msgTS))
	c.handleEventsAPI(ctx, wrapReactionAddedEvent("+1", "U_HUMAN", channelID, msgTS))
	if got := addedAt("+1"); time.Since(got) > time.Hour {
		t.Errorf("re-added reaction kept its old time %v", got)
	}

	// The same button vote again keeps its time; a changed one is new.
	clicked := addedAt(feedbackButtonsRowKey)
	c.handleFeedbackButtonClick(ctx, feedbackButtonCallback(feedbackPositiveValue, "U_HUMAN", channelID, msgTS), feedbackPositiveValue)
	if got := addedAt(feedbackButtonsRowKey); !got.Equal(clicked) {
		t.Errorf("repeated button vote moved added_at from %v to %v", clicked, got)
	}
	c.handleFeedbackButtonClick(ctx, feedbackButtonCallback(feedbackNegativeValue, "U_HUMAN", channelID, msgTS), feedbackNegativeValue)
	if got := addedAt(feedbackButtonsRowKey); time.Since(got) > time.Hour {
		t.Errorf("changed button vote kept its old time %v", got)
	}
}

// TestSweepFeedback_CorrectsASkinToneThumb verifies that a skin-tone thumb
// stored as "other" before verdictForEmoji knew the suffix is counted as a
// rating the next time the sweep sees it, keeping its time.
func TestSweepFeedback_CorrectsASkinToneThumb(t *testing.T) {
	pool := getConsumerPool(t)
	truncateFeedbackTables(t, pool)
	ctx := context.Background()

	sessionID := seedSession(t, pool)
	const channelID = "C_TEST"
	const msgTS = "1700000041.000100"
	if err := insertAnswerMessage(ctx, pool, channelID, msgTS, sessionID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ask_feedback (session_id, turn, channel_id, message_ts, reactor, emoji, verdict, added_at)
		VALUES ($1::uuid, 0, $2, $3, 'U_HUMAN', '+1::skin-tone-3', 'other', now() - interval '2 days'),
		       ($1::uuid, 0, $2, $3, 'U_OTHER', '-1::skin-tone-2', 'other', now() - interval '2 days')`,
		sessionID, channelID, msgTS); err != nil {
		t.Fatal(err)
	}
	c := newTestConsumer(t, pool)
	c.reactions = newFakeReactions(map[string][]slackgo.ItemReaction{
		channelID + ":" + msgTS: {
			{Name: "+1::skin-tone-3", Users: []string{"U_HUMAN"}},
			{Name: "-1::skin-tone-2", Users: []string{"U_OTHER"}},
		},
	})
	if err := c.SweepFeedback(ctx); err != nil {
		t.Fatalf("SweepFeedback: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT reactor, verdict, added_at < now() - interval '1 day' FROM ask_feedback ORDER BY reactor`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]string{"U_HUMAN": "up", "U_OTHER": "down"}
	seen := 0
	for rows.Next() {
		var reactor, verdict string
		var old bool
		if err := rows.Scan(&reactor, &verdict, &old); err != nil {
			t.Fatal(err)
		}
		seen++
		if verdict != want[reactor] {
			t.Errorf("%s verdict = %q, want %q", reactor, verdict, want[reactor])
		}
		if !old {
			t.Errorf("%s: a corrected verdict is not a new rating; added_at should not move", reactor)
		}
	}
	if seen != 2 {
		t.Errorf("rows = %d, want 2", seen)
	}
}
