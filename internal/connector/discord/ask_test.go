package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// fakeAskRouter is Ask with real sessions and a turn the test decides.
type fakeAskRouter struct {
	*ask.Manager
	mu        sync.Mutex
	turn      func(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
	requests  []ask.TurnRequest
	artifacts map[uuid.UUID][]byte
	delivered []string
}

func (r *fakeAskRouter) LookupOrCreateSlackSession(ctx context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error) {
	return r.LoadOrCreateForSlackThread(ctx, channelID, threadTS, askerEmail)
}

func (r *fakeAskRouter) ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	turn := r.turn
	r.mu.Unlock()
	return turn(ctx, req)
}

func (r *fakeAskRouter) seen() []ask.TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ask.TurnRequest(nil), r.requests...)
}

func (r *fakeAskRouter) ReadArtifact(_ context.Context, sessionID, artifactID uuid.UUID) (ask.ArtifactRecord, []byte, error) {
	return ask.ArtifactRecord{ID: artifactID, SessionID: sessionID, ContentHash: "hash"}, r.artifacts[artifactID], nil
}

func (r *fakeAskRouter) ArtifactDeliveryExists(context.Context, uuid.UUID, int, uuid.UUID, string, string) (bool, error) {
	return false, nil
}

func (r *fakeAskRouter) RecordArtifactDelivery(_ context.Context, _ uuid.UUID, _ int, _ uuid.UUID, surface, target, externalID, status, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivered = append(r.delivered, surface+" "+target+" "+status+" "+externalID)
	return nil
}

// askHarness is a running Discord connector with Ask, on the fake.
type askHarness struct {
	f      *fakeDiscord
	deps   source.Deps
	router *fakeAskRouter
	conn   *fakeConn
	seeded seeded
}

func startAsk(t *testing.T, summarizer *askchat.Summarizer) *askHarness {
	t.Helper()
	shortIntervals(t)
	renew := askchat.WorkingRenew
	askchat.WorkingRenew = 100 * time.Millisecond
	t.Cleanup(func() { askchat.WorkingRenew = renew })
	f := newFakeDiscord(t)
	h := &askHarness{f: f, seeded: seedServer(f), deps: testDeps(t)}
	h.deps.Files = &lentFiles{}
	_, err := h.deps.Pool.Exec(t.Context(), `TRUNCATE ask_sessions, ask_turn_claims, slack_working_threads, ask_answer_messages, ask_feedback CASCADE`)
	require.NoError(t, err)
	h.router = &fakeAskRouter{Manager: ask.NewManager(h.deps.Pool, h.deps.Logger), artifacts: map[uuid.UUID][]byte{},
		turn: func(context.Context, ask.TurnRequest) (ask.TurnResult, error) {
			return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
		}}
	c := Connector{APIBase: f.apiBase()}
	resolved := settings.NewResolved(ConnectorName, settings.Values{FieldHistoryDays: int64(0)}, map[string]string{FieldBotToken: "test-token"})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- c.RunWithAsk(ctx, h.deps, resolved, &Ask{Router: h.router, Summarizer: summarizer})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Error("Run did not return")
		}
	})
	h.conn = f.nextConn(t)
	channels := ChannelsFor(h.deps)
	eventually(t, "the directory", func() bool {
		o, err := channels.Overview(t.Context())
		return err == nil && len(o.Directory.Guilds) == 1
	})
	_, err = channels.Add(t.Context(), fakeText, VisibilityEveryone, "", false, "admin@example.com")
	require.NoError(t, err)
	_, err = channels.Add(t.Context(), fakePrivate, VisibilityPolicy, "staff", true, "admin@example.com")
	require.NoError(t, err)
	require.NoError(t, channels.SetAskInGuild(t.Context(), fakeGuild, true))
	return h
}

// mentions numbers the mentions, so two of one millisecond differ.
var mentions atomic.Int32

// mention is a message in channelID by author that mentions the bot.
func (h *askHarness) mention(channelID string, author user, text string) message {
	now := time.Now().UTC()
	m := msg(fakeID(now, int(mentions.Add(1))%1000), author, now, "<@"+fakeBot+"> "+text)
	m.GuildID, m.ChannelID = fakeGuild, channelID
	m.Mentions = []user{{ID: fakeBot, Username: "diderot-test", Bot: true}}
	h.f.addMessage(channelID, m)
	h.f.dispatch("MESSAGE_CREATE", m)
	return m
}

func (h *askHarness) waitTurns(t *testing.T, n int) []ask.TurnRequest {
	t.Helper()
	eventually(t, fmt.Sprintf("%d turns", n), func() bool { return len(h.router.seen()) >= n })
	return h.router.seen()
}

// TestAskAnswersAMentionInAThreadWithWhatItsAudienceCanRead: a mention in a
// channel starts a thread from the question and answers there, in chunks
// that fit Discord, without pings or previews, with only what everyone in
// the channel can read: never the private channel, whoever asks.
func TestAskAnswersAMentionInAThreadWithWhatItsAudienceCanRead(t *testing.T) {
	h := startAsk(t, nil)
	long := strings.Repeat("A line of the answer that goes on for a while, @everyone.\n", 60)
	h.router.turn = func(context.Context, ask.TurnRequest) (ask.TurnResult, error) {
		// Long enough for the placeholder to show.
		time.Sleep(300 * time.Millisecond)
		return ask.TurnResult{Reply: long, AssistantTurn: 1}, nil
	}
	q := h.mention(fakeText, alice, "what did we decide about the build cache?")
	req := h.waitTurns(t, 1)[0]
	assert.Equal(t, ask.AskRunSurfaceDiscord, req.Surface)
	assert.Equal(t, "what did we decide about the build cache?", req.UserInput)
	assert.ElementsMatch(t, []string{events.ScopePublic, ChannelScope(fakeText)}, req.AllowedScopes,
		"the private channel's members are not the channel's audience")
	assert.Equal(t, "Alice Example", req.Asker.DisplayName)

	// The thread is started from the question.
	threads := h.f.writes(http.MethodPost, "/channels/"+fakeText+"/messages/"+q.ID+"/threads")
	require.Len(t, threads, 1)
	assert.Equal(t, "what did we decide about the build cache?", threads[0].Message.Content)

	eventually(t, "the answer", func() bool { return len(h.f.writes(http.MethodPost, "/channels/"+q.ID+"/messages")) >= 3 })
	eventually(t, "the placeholder down", func() bool { return len(h.f.writes(http.MethodDelete, "/channels/"+q.ID+"/messages/")) == 1 })
	posts := h.f.writes(http.MethodPost, "/channels/"+q.ID+"/messages")
	assert.Equal(t, workingText, posts[0].Message.Content)
	require.NotNil(t, posts[0].Message.Components)
	assert.True(t, hasButton(message{Components: *posts[0].Message.Components}, stopPrefix+channelKey(req.AllowedScopes)))
	var answer strings.Builder
	for _, p := range posts[1:] {
		assert.LessOrEqual(t, len(p.Message.Content), 2000)
		assert.Equal(t, flagSuppressEmbeds, p.Message.Flags)
		require.NotNil(t, p.Message.AllowedMentions)
		assert.Empty(t, p.Message.AllowedMentions.Parse)
		answer.WriteString(p.Message.Content + "\n")
	}
	assert.NotContains(t, answer.String(), "@everyone")
	assert.Equal(t, 60, strings.Count(answer.String(), "A line of the answer"))
	// Only the answer is left in the thread.
	for _, m := range h.f.channelMessages(q.ID) {
		assert.NotEqual(t, workingText, m.Content)
	}

	// The question is marked seen, then answered, in its channel.
	eventually(t, "the answered mark", func() bool {
		return len(h.f.writes(http.MethodPut, "/channels/"+fakeText+"/messages/"+q.ID+"/reactions/✅/@me")) == 1
	})
	assert.Len(t, h.f.writes(http.MethodPut, "/channels/"+fakeText+"/messages/"+q.ID+"/reactions/👀/@me"), 1)
	assert.Len(t, h.f.writes(http.MethodDelete, "/channels/"+fakeText+"/messages/"+q.ID+"/reactions/👀/@me"), 1)

	// A follow-up in the thread continues the session with what was said
	// in the thread since.
	now := time.Now().UTC()
	aside := msg(fakeID(now, int(mentions.Add(1))%1000), bob, now, "also: the prefetch job is flaky")
	aside.GuildID, aside.ChannelID = fakeGuild, q.ID
	h.f.addMessage(q.ID, aside)
	h.mention(q.ID, bob, "and the prefetch job?")
	follow := h.waitTurns(t, 2)[1]
	assert.Equal(t, req.SessionID, follow.SessionID)
	assert.Contains(t, follow.ThreadContext, "bob: also: the prefetch job is flaky")
	assert.NotContains(t, follow.ThreadContext, "A line of the answer", "the bot's own posts are not context")

	// The answers are not indexed.
	time.Sleep(200 * time.Millisecond)
	for _, e := range indexedEvents(t, h.deps) {
		assert.NotContains(t, e.Body, "A line of the answer")
	}
}

// channelKey is the session key of a channel whose viewers all read its
// history.
func channelKey(scopes []string) string {
	return sessionKey(append(slices.Clone(scopes), "thread-history"))
}

// TestAskGivesNoThreadContextWhereViewersCantReadHistory: a thread's own
// messages are read with the bot's access, so where some of the channel's
// viewers can't read its history they are not the turn's context.
func TestAskGivesNoThreadContextWhereViewersCantReadHistory(t *testing.T) {
	h := startAsk(t, nil)
	const noHistory, thread = "200000000000000006", "230000000000000001"
	h.f.mu.Lock()
	h.f.channels = append(h.f.channels, channel{ID: noHistory, Type: channelText, Name: "announcements-only", PermissionOverwrites: []overwrite{
		{ID: fakeEveryone, Type: 0, Deny: strconv.FormatInt(permReadMessageHistory, 10), Allow: "0"},
	}})
	h.f.threads = append(h.f.threads, channel{ID: thread, Type: channelPublicThread, GuildID: fakeGuild, ParentID: noHistory, Name: "old talk"})
	h.f.mu.Unlock()
	earlier := time.Now().Add(-time.Hour).UTC()
	old := msg(fakeID(earlier, 1), bob, earlier, "something said before you could scroll back")
	h.f.addMessage(thread, old)
	h.mention(thread, alice, "what was said here?")
	req := h.waitTurns(t, 1)[0]
	assert.Empty(t, req.ThreadContext)
	assert.Equal(t, []string{events.ScopePublic, ChannelScope(fakeText)}, req.AllowedScopes)
}

// TestAskTakesAPlacesQuestionsInTurn: a second question in a thread waits
// for the first's turn, so each turn's placeholder is its own and comes
// down.
func TestAskTakesAPlacesQuestionsInTurn(t *testing.T) {
	h := startAsk(t, nil)
	var mu sync.Mutex
	running, overlapped := 0, false
	h.router.turn = func(context.Context, ask.TurnRequest) (ask.TurnResult, error) {
		mu.Lock()
		running++
		overlapped = overlapped || running > 1
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}
	q := h.mention(fakeText, alice, "first")
	h.waitTurns(t, 1)
	h.mention(q.ID, bob, "second")
	h.mention(q.ID, alice, "third")
	h.waitTurns(t, 3)
	eventually(t, "every placeholder down", func() bool {
		answers, placeholders := 0, 0
		for _, m := range h.f.channelMessages(q.ID) {
			switch m.Content {
			case "the answer":
				answers++
			case workingText:
				placeholders++
			}
		}
		return answers == 3 && placeholders == 0
	})
	mu.Lock()
	defer mu.Unlock()
	assert.False(t, overlapped, "turns in one place never overlap")
}

// TestAskInAPrivateChannelUsesIt: in a chosen private channel whose viewers
// all read its history, an answer may use it, as well as the open channels.
func TestAskInAPrivateChannelUsesIt(t *testing.T) {
	h := startAsk(t, nil)
	h.mention(fakePrivate, alice, "what is staff saying?")
	req := h.waitTurns(t, 1)[0]
	assert.ElementsMatch(t, []string{events.ScopePublic, ChannelScope(fakeText), ChannelScope(fakePrivate)}, req.AllowedScopes)
}

// TestAskWhereItIsOff: a mention where Ask is off gets a one-line reply and
// no turn.
func TestAskWhereItIsOff(t *testing.T) {
	h := startAsk(t, nil)
	require.NoError(t, ChannelsFor(h.deps).SetAskInChannel(t.Context(), fakeText, false))
	q := h.mention(fakeText, alice, "anyone?")
	eventually(t, "the reply", func() bool { return len(h.f.writes(http.MethodPost, "/channels/"+fakeText+"/messages")) == 1 })
	reply := h.f.writes(http.MethodPost, "/channels/"+fakeText+"/messages")[0]
	assert.Equal(t, offInChannel, reply.Message.Content)
	assert.Equal(t, q.ID, reply.Message.MessageReference.MessageID)
	assert.Empty(t, h.router.seen())
}

// TestAskStop: Stop cancels the turn, posts nothing, and leaves the
// placeholder saying it stopped.
func TestAskStop(t *testing.T) {
	h := startAsk(t, nil)
	running := make(chan struct{})
	h.router.turn = func(ctx context.Context, _ ask.TurnRequest) (ask.TurnResult, error) {
		close(running)
		<-ctx.Done()
		return ask.TurnResult{}, context.Cause(ctx)
	}
	q := h.mention(fakeText, alice, "a long one")
	<-running
	var placeholder message
	eventually(t, "the placeholder", func() bool {
		for _, m := range h.f.channelMessages(q.ID) {
			if m.Content == workingText {
				placeholder = m
				return true
			}
		}
		return false
	})
	req := h.router.seen()[0]
	h.f.dispatch("INTERACTION_CREATE", map[string]any{
		"id": "900000000000000001", "application_id": "500000000000000001", "type": interactionComponent, "token": "stop-token",
		"guild_id": fakeGuild, "channel_id": q.ID, "member": map[string]any{"user": bob, "roles": []string{}},
		"message": placeholder, "data": map[string]any{"custom_id": stopPrefix + channelKey(req.AllowedScopes), "component_type": componentButton},
	})
	eventually(t, "the stopped placeholder", func() bool {
		for _, m := range h.f.channelMessages(q.ID) {
			if m.ID == placeholder.ID {
				return m.Content == stoppedText && len(m.Components) == 0
			}
		}
		return false
	})
	callbacks := h.f.writes(http.MethodPost, "/interactions/900000000000000001/")
	require.Len(t, callbacks, 1)
	assert.Equal(t, responseDeferredUpdate, callbacks[0].Response.Type)
	eventually(t, "the seen mark taken off", func() bool {
		return len(h.f.writes(http.MethodDelete, "/channels/"+fakeText+"/messages/"+q.ID+"/reactions/👀/@me")) == 1
	})
	assert.Len(t, h.f.writes(http.MethodPost, "/channels/"+q.ID+"/messages"), 1, "only the placeholder was posted")
}

// TestAskDirectMessage: a DM is answered with what its asker can read, and
// only when Answer direct messages is on and the asker is a member.
func TestAskDirectMessage(t *testing.T) {
	h := startAsk(t, nil)
	const dm = "210000000000000001"
	send := func(author user, text string) message {
		now := time.Now().UTC()
		m := msg(fakeID(now, 9), author, now, text)
		m.ChannelID = dm
		h.f.addMessage(dm, m)
		h.f.dispatch("MESSAGE_CREATE", m)
		return m
	}
	send(alice, "hello?")
	eventually(t, "the off reply", func() bool { return len(h.f.writes(http.MethodPost, "/channels/"+dm+"/messages")) == 1 })
	assert.Equal(t, offInDMs, h.f.writes(http.MethodPost, "/channels/"+dm+"/messages")[0].Message.Content)

	require.NoError(t, ChannelsFor(h.deps).SetAskDirectMessages(t.Context(), true))
	send(alice, "what is staff saying?")
	req := h.waitTurns(t, 1)[0]
	// Alice's roles don't let her view the private channel.
	assert.ElementsMatch(t, []string{events.ScopePublic, ChannelScope(fakeText)}, req.AllowedScopes)
	eventually(t, "the answer", func() bool {
		for _, w := range h.f.writes(http.MethodPost, "/channels/"+dm+"/messages") {
			if w.Message.Content == "the answer" {
				return true
			}
		}
		return false
	})

	carol := user{ID: "300000000000000003", Username: "carol"}
	h.f.fail("/guilds/"+fakeGuild+"/members/"+carol.ID, 404)
	send(carol, "let me in")
	eventually(t, "the not-a-member reply", func() bool {
		for _, w := range h.f.writes(http.MethodPost, "/channels/"+dm+"/messages") {
			if w.Message.Content == notAMember {
				return true
			}
		}
		return false
	})
	assert.Len(t, h.router.seen(), 1)
}

// TestAskPrivateCommand: /ask private:True answers through the
// interaction, which only its user sees, with what they can read.
func TestAskPrivateCommand(t *testing.T) {
	h := startAsk(t, nil)
	h.router.turn = func(context.Context, ask.TurnRequest) (ask.TurnResult, error) {
		time.Sleep(300 * time.Millisecond)
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}
	h.f.dispatch("INTERACTION_CREATE", map[string]any{
		"id": "900000000000000002", "application_id": "500000000000000001", "type": interactionCommand, "token": "ask-token",
		"guild_id": fakeGuild, "channel_id": fakeText, "channel": map[string]any{"id": fakeText, "type": channelText},
		"member": map[string]any{"user": alice, "roles": []string{}},
		"data": map[string]any{"name": commandAsk, "type": commandChatInput, "options": []map[string]any{
			{"name": "question", "type": 3, "value": "what's private?"}, {"name": "private", "type": 5, "value": true},
		}},
	})
	req := h.waitTurns(t, 1)[0]
	assert.ElementsMatch(t, []string{events.ScopePublic, ChannelScope(fakeText)}, req.AllowedScopes)
	eventually(t, "the answer", func() bool {
		edits := h.f.writes(http.MethodPatch, "/webhooks/500000000000000001/ask-token/messages/@original")
		return len(edits) >= 2 && edits[len(edits)-1].Message.Content == "the answer"
	})
	callbacks := h.f.writes(http.MethodPost, "/interactions/900000000000000002/")
	require.Len(t, callbacks, 1)
	assert.Equal(t, responseDeferredMessage, callbacks[0].Response.Type)
	assert.Equal(t, flagEphemeral, callbacks[0].Response.Data.Flags)
	edits := h.f.writes(http.MethodPatch, "/webhooks/500000000000000001/ask-token/messages/@original")
	assert.Equal(t, workingText, edits[0].Message.Content, "the placeholder, with its Stop button")
	require.NotNil(t, edits[len(edits)-1].Message.Components)
	assert.Empty(t, *edits[len(edits)-1].Message.Components, "the answer takes the button off")
	assert.Empty(t, h.f.writes(http.MethodPost, "/channels/"), "nothing is posted in the channel")
}

// TestAskPublicCommandAnswersInAThread: /ask shows the question, starts a
// thread from it, and answers there with the channel's audience's scopes.
func TestAskPublicCommandAnswersInAThread(t *testing.T) {
	h := startAsk(t, nil)
	h.f.dispatch("INTERACTION_CREATE", map[string]any{
		"id": "900000000000000003", "application_id": "500000000000000001", "type": interactionCommand, "token": "pub-token",
		"guild_id": fakeGuild, "channel_id": fakeText, "channel": map[string]any{"id": fakeText, "type": channelText},
		"member": map[string]any{"user": bob, "roles": []string{}},
		"data": map[string]any{"name": commandAsk, "type": commandChatInput, "options": []map[string]any{
			{"name": "question", "type": 3, "value": "status of the deploy?"},
		}},
	})
	req := h.waitTurns(t, 1)[0]
	assert.ElementsMatch(t, []string{events.ScopePublic, ChannelScope(fakeText)}, req.AllowedScopes)
	h.f.mu.Lock()
	original := h.f.ask.originals["pub-token"]
	h.f.mu.Unlock()
	require.NotNil(t, original)
	assert.Equal(t, "**bob** asked: status of the deploy?", original.Content)
	eventually(t, "the answer in the thread", func() bool {
		for _, w := range h.f.writes(http.MethodPost, "/channels/"+original.ID+"/messages") {
			if w.Message.Content == "the answer" {
				return true
			}
		}
		return false
	})
}

// TestAskAttachesPanelsAndRecordsRatings: a turn's panels are attached as
// files, and a 👍 on an answer is its rating.
func TestAskAttachesPanelsAndRecordsRatings(t *testing.T) {
	h := startAsk(t, nil)
	panel := uuid.New()
	h.router.artifacts[panel] = []byte("\x89PNG panel")
	h.router.turn = func(context.Context, ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{Reply: "see the chart", AssistantTurn: 1,
			Artifacts: []ask.ArtifactDescriptor{{ID: panel, Title: "Error rate", AltText: "errors per minute, flat"}}}, nil
	}
	q := h.mention(fakeText, alice, "chart the error rate")
	var answerID string
	eventually(t, "the panel", func() bool {
		for _, w := range h.f.writes(http.MethodPost, "/channels/"+q.ID+"/messages") {
			if len(w.Files) == 1 {
				assert.Equal(t, "grafana-panel-"+panel.String()+".png", w.Files[0])
				assert.Equal(t, "Error rate", w.Message.Content)
				return true
			}
		}
		return false
	})
	for _, m := range h.f.channelMessages(q.ID) {
		if m.Content == "see the chart" {
			answerID = m.ID
		}
	}
	require.NotEmpty(t, answerID)
	h.f.dispatch("MESSAGE_REACTION_ADD", map[string]any{"user_id": bob.ID, "channel_id": q.ID, "message_id": answerID,
		"guild_id": fakeGuild, "emoji": map[string]any{"name": "👍🏽"}})
	eventually(t, "the rating", func() bool {
		var verdict string
		err := h.deps.Pool.QueryRow(t.Context(), `SELECT verdict FROM ask_feedback WHERE message_ts = $1 AND reactor = $2`, answerID, bob.ID).Scan(&verdict)
		return err == nil && verdict == "up"
	})
}

// TestAskSummarizesAThread: the message command summarizes the thread for
// the channel.
func TestAskSummarizesAThread(t *testing.T) {
	var transcript string
	var mu sync.Mutex
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		transcript = body.Messages[len(body.Messages)-1].Content
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"**Summary** They pinned the toolchain."}}]}`))
	}))
	t.Cleanup(model.Close)
	summarizer, err := askchat.NewSummarizer(askchat.SummarizerConfig{SystemPrompt: SummaryPrompt, BaseURL: model.URL, APIKey: "test-key", ModelID: "summary-model"})
	require.NoError(t, err)
	h := startAsk(t, summarizer)
	starter := h.f.channelMessages(fakeText)[3]
	require.NotNil(t, starter.Thread)
	h.f.dispatch("INTERACTION_CREATE", map[string]any{
		"id": "900000000000000004", "application_id": "500000000000000001", "type": interactionCommand, "token": "sum-token",
		"guild_id": fakeGuild, "channel_id": fakeText, "channel": map[string]any{"id": fakeText, "type": channelText},
		"member": map[string]any{"user": alice, "roles": []string{}},
		"data": map[string]any{"name": commandSummarize, "type": commandMessage, "target_id": starter.ID,
			"resolved": map[string]any{"messages": map[string]any{starter.ID: starter}}},
	})
	eventually(t, "the summary", func() bool {
		edits := h.f.writes(http.MethodPatch, "/webhooks/500000000000000001/sum-token/messages/@original")
		return len(edits) == 1 && edits[0].Message.Content == "**Summary** They pinned the toolchain."
	})
	callbacks := h.f.writes(http.MethodPost, "/interactions/900000000000000004/")
	require.Len(t, callbacks, 1)
	assert.Zero(t, callbacks[0].Response.Data.Flags, "a summary for the channel is not ephemeral")
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, transcript, "should we pin the toolchain?")
	assert.Contains(t, transcript, "yes, pin it to 1.27")
}

// TestAskSweepClearsWhatADeadTurnLeft: a placeholder whose turn died with
// its process is taken down by the next sweep, which says the answer was
// interrupted.
func TestAskSweepClearsWhatADeadTurnLeft(t *testing.T) {
	deps := testDeps(t)
	_, err := deps.Pool.Exec(t.Context(), `TRUNCATE slack_working_threads`)
	require.NoError(t, err)
	f := newFakeDiscord(t)
	const thread = "220000000000000001"
	conv := askchat.Conversation{Place: thread, Thread: "s-0123456789abcdef"}
	stop := buttonRow("Stop", stopPrefix+conv.Thread)
	f.addMessage(thread, message{ID: fakeID(time.Now(), 1), Author: user{ID: fakeBot, Bot: true}, Content: workingText, Components: stop})
	f.addMessage(thread, message{ID: fakeID(time.Now(), 2), Author: alice, Content: "a question"})
	store := askchat.PGWorkingThreads{Pool: deps.Pool, Surface: ask.AskRunSurfaceDiscord}
	require.NoError(t, store.Begin(t.Context(), conv.Place, conv.Thread, uuid.New()))
	// A Slack record is not Discord's to sweep.
	slack := askchat.PGWorkingThreads{Pool: deps.Pool, Surface: ask.AskRunSurfaceSlack}
	require.NoError(t, slack.Begin(t.Context(), "C0123", "1700000000.000100", uuid.New()))
	_, err = deps.Pool.Exec(t.Context(), `UPDATE slack_working_threads SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)

	s := newSurface(NewClient(f.apiBase(), "test-token"), "500000000000000001", fakeBot, deps.Logger)
	engine := &askchat.Engine{Surface: s, Working: store, Turns: &askchat.Turns{}, Logger: deps.Logger}
	swept, err := engine.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, swept)
	msgs := f.channelMessages(thread)
	var contents []string
	for _, m := range msgs {
		contents = append(contents, m.Content)
	}
	assert.NotContains(t, contents, workingText)
	assert.Contains(t, contents, askchat.InterruptedNotice)
	var slackLeft int
	require.NoError(t, deps.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_working_threads WHERE surface = 'slack'`).Scan(&slackLeft))
	assert.Equal(t, 1, slackLeft)
}

// TestGuildAccess pins who may read what: an open channel is one every
// member reads; a channel's own viewers must all read its history.
func TestGuildAccess(t *testing.T) {
	perms := func(p int64) string { return strconv.FormatInt(p, 10) }
	g := guildAccess{guildID: fakeGuild, rolePerms: map[string]int64{
		fakeGuild: permViewChannel | permReadMessageHistory, fakeRole: 0, "400000000000000002": permViewChannel,
	}, channels: map[string]channel{
		"open":   {ID: "open"},
		"denied": {ID: "denied", PermissionOverwrites: []overwrite{{ID: fakeRole, Type: 0, Deny: perms(permReadMessageHistory), Allow: "0"}}},
		"staff": {ID: "staff", PermissionOverwrites: []overwrite{
			{ID: fakeGuild, Type: 0, Deny: perms(permViewChannel), Allow: "0"},
			{ID: fakeRole, Type: 0, Allow: perms(permViewChannel | permReadMessageHistory), Deny: "0"},
		}},
		"member-view": {ID: "member-view", PermissionOverwrites: []overwrite{
			{ID: fakeGuild, Type: 0, Deny: perms(permViewChannel | permReadMessageHistory), Allow: "0"},
			{ID: fakeAlice, Type: 1, Allow: perms(permViewChannel), Deny: "0"},
		}},
	}}
	assert.True(t, g.open("open"))
	assert.False(t, g.open("denied"), "a deny to any role closes it")
	assert.False(t, g.open("staff"))
	assert.True(t, g.viewersRead("open"))
	assert.True(t, g.viewersRead("staff"))
	assert.False(t, g.viewersRead("denied"))
	assert.False(t, g.viewersRead("member-view"))
	assert.False(t, g.open("missing"))
	assert.False(t, g.viewersRead("missing"))

	assert.True(t, g.memberReads(member{Roles: []string{fakeRole}}, fakeBob, "staff"))
	assert.False(t, g.memberReads(member{}, fakeBob, "staff"))

	// A server whose members view channels without reading their history:
	// an answer in its channel can't use the channel.
	viewOnly := guildAccess{guildID: fakeGuild, rolePerms: map[string]int64{fakeGuild: permViewChannel, fakeRole: permReadMessageHistory},
		channels: map[string]channel{"general": {ID: "general"}}}
	assert.False(t, viewOnly.open("general"))
	assert.False(t, viewOnly.viewersRead("general"))
	assert.False(t, viewOnly.memberReads(member{}, fakeBob, "general"))
	assert.True(t, viewOnly.memberReads(member{Roles: []string{fakeRole}}, fakeBob, "general"))
}

// trust marks the fake's server trusted, or not.
func (h *askHarness) trust(t *testing.T, trusted bool) {
	t.Helper()
	require.NoError(t, ChannelsFor(h.deps).SetTrustedGuild(t.Context(), fakeGuild, trusted))
}

// TestAskInATrustedServerReadsInternal: in a trusted server an answer in a
// channel also reads internal, since every viewer is a member, but never a
// policy scope; not in an announcement channel, whose posts reach the
// servers that follow it, nor in a server people can read without joining.
func TestAskInATrustedServerReadsInternal(t *testing.T) {
	h := startAsk(t, nil)
	const news = "200000000000000008"
	h.f.mu.Lock()
	h.f.channels = append(h.f.channels, channel{ID: news, Type: channelAnnouncement, Name: "news"})
	h.f.mu.Unlock()
	h.mention(fakeText, alice, "before")
	assert.NotContains(t, h.waitTurns(t, 1)[0].AllowedScopes, events.ScopeInternal, "an untrusted server reads no internal content")

	h.trust(t, true)
	h.mention(fakeText, alice, "trusted")
	req := h.waitTurns(t, 2)[1]
	assert.ElementsMatch(t, []string{events.ScopePublic, events.ScopeInternal, ChannelScope(fakeText)}, req.AllowedScopes)
	for _, scope := range req.AllowedScopes {
		assert.NotContains(t, scope, "corpus:", "never a policy scope")
	}

	h.mention(news, alice, "in the announcements")
	assert.NotContains(t, h.waitTurns(t, 3)[2].AllowedScopes, events.ScopeInternal, "an announcement channel's posts can leave the server")

	h.f.mu.Lock()
	h.f.ask.guildFeatures = []string{"COMMUNITY", "DISCOVERABLE"}
	h.f.mu.Unlock()
	h.mention(fakeText, alice, "discoverable")
	assert.NotContains(t, h.waitTurns(t, 4)[3].AllowedScopes, events.ScopeInternal, "a server people can read without joining")
}

// TestAskTrustedServerAndDirectMessages: a DM reads internal only when its
// asker is a full member of a trusted server where Ask is on: not a guest,
// not one still in Membership Screening, and not a member of an untrusted
// server only.
func TestAskTrustedServerAndDirectMessages(t *testing.T) {
	h := startAsk(t, nil)
	const dm, other = "210000000000000002", "100000000000000002"
	channels := ChannelsFor(h.deps)
	require.NoError(t, channels.SetAskDirectMessages(t.Context(), true))
	// Ask is on in a second server, trusted, that the askers are not in.
	var s AskSettings
	require.NoError(t, h.deps.State.Update(t.Context(), askKey, &s, func(bool) error {
		s.Guilds[other], s.Trusted = true, map[string]bool{other: true}
		return nil
	}))
	h.f.fail("/guilds/"+other+"/members/", 404, 404, 404, 404, 404, 404, 404, 404)
	n := 0
	ask := func(author user) []string {
		n++
		now := time.Now().UTC()
		m := msg(fakeID(now, int(mentions.Add(1))%1000), author, now, "what is internal?")
		m.ChannelID = dm
		h.f.addMessage(dm, m)
		h.f.dispatch("MESSAGE_CREATE", m)
		return h.waitTurns(t, n)[n-1].AllowedScopes
	}
	assert.NotContains(t, ask(alice), events.ScopeInternal, "a trusted server the asker is not in counts for nothing")

	h.trust(t, true)
	assert.Contains(t, ask(alice), events.ScopeInternal)
	h.f.mu.Lock()
	h.f.ask.members = map[string]member{
		fakeBob:              {Roles: []string{}, Flags: memberIsGuest},
		"300000000000000003": {Roles: []string{}, Pending: true},
	}
	h.f.mu.Unlock()
	assert.NotContains(t, ask(bob), events.ScopeInternal, "a guest")
	assert.NotContains(t, ask(user{ID: "300000000000000003", Username: "carol"}), events.ScopeInternal, "a member still in screening")
}

// TestAskTrustingAServerStartsNewSessions: a thread's conversation runs with
// one set of scopes, so trusting its server, or no longer, starts a new
// session there.
func TestAskTrustingAServerStartsNewSessions(t *testing.T) {
	h := startAsk(t, nil)
	q := h.mention(fakeText, alice, "first")
	first := h.waitTurns(t, 1)[0]
	eventually(t, "the first answer", func() bool {
		return len(h.f.writes(http.MethodPut, "/channels/"+fakeText+"/messages/"+q.ID+"/reactions/✅/@me")) == 1
	})
	h.trust(t, true)
	h.mention(q.ID, bob, "now trusted")
	trusted := h.waitTurns(t, 2)[1]
	assert.Contains(t, trusted.AllowedScopes, events.ScopeInternal)
	assert.NotEqual(t, first.SessionID, trusted.SessionID)
	h.mention(q.ID, bob, "still trusted")
	assert.Equal(t, trusted.SessionID, h.waitTurns(t, 3)[2].SessionID)
	h.trust(t, false)
	h.mention(q.ID, bob, "no longer")
	untrusted := h.waitTurns(t, 4)[3]
	assert.NotContains(t, untrusted.AllowedScopes, events.ScopeInternal)
	assert.NotEqual(t, trusted.SessionID, untrusted.SessionID)
	assert.Equal(t, first.SessionID, untrusted.SessionID, "back to the scopes it started with")
}

// TestAskReadsAccessWhenTheTurnRuns: a question that waits for the turn
// before it in its place runs with the access of when it runs, so one asked
// before its server stopped being trusted gets no internal content.
func TestAskReadsAccessWhenTheTurnRuns(t *testing.T) {
	h := startAsk(t, nil)
	h.trust(t, true)
	release := make(chan struct{})
	h.router.turn = func(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
		if req.UserInput == "slow" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}
	q := h.mention(fakeText, alice, "slow")
	assert.Contains(t, h.waitTurns(t, 1)[0].AllowedScopes, events.ScopeInternal)
	h.mention(q.ID, bob, "queued")
	// The queued question is marked seen while it waits.
	eventually(t, "the queued question seen", func() bool {
		return len(h.f.writes(http.MethodPut, "/channels/"+q.ID+"/messages/")) >= 1
	})
	h.trust(t, false)
	close(release)
	queued := h.waitTurns(t, 2)[1]
	assert.Equal(t, "queued", queued.UserInput)
	assert.NotContains(t, queued.AllowedScopes, events.ScopeInternal)
}

func TestSessionKeyFollowsTheScopes(t *testing.T) {
	a := sessionKey([]string{"public", ChannelScope("1")})
	assert.Equal(t, a, sessionKey([]string{ChannelScope("1"), "public", "public"}))
	assert.NotEqual(t, a, sessionKey([]string{"public"}))
	assert.NotEqual(t, a, sessionKey([]string{"public", ChannelScope("1"), ChannelScope("2")}))
	assert.LessOrEqual(t, len(stopPrefix+a), 100, "a button's custom ID holds it")
}

func TestVerdictFor(t *testing.T) {
	assert.Equal(t, "up", verdictFor("👍"))
	assert.Equal(t, "up", verdictFor("👍🏿"))
	assert.Equal(t, "down", verdictFor("👎"))
	assert.Empty(t, verdictFor("🔥"))
}
